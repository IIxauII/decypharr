package usenet

import (
	"context"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/sirrobot01/decypharr/internal/crypto"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sourcegraph/conc/pool"
)

// segmentResult holds a fetched segment and its index for ordered writing
type segmentResult struct {
	index int
	data  []byte
	err   error
}

// ProgressCallback is called periodically during download with progress info
// downloaded: total bytes written so far, speed: bytes per second (estimated)
type ProgressCallback func(downloaded int64, speed int64)

// Download downloads a file by fetching segments in parallel and streaming to writer in order.
// Bytes flow to the writer progressively as in-order segments complete - no waiting for all segments.
// If progressCallback is provided, it will be called after each segment write with current progress.
func (u *Usenet) Download(ctx context.Context, nzoID, filename string, writer io.Writer, progressCallback ProgressCallback) error {
	// get file metadata
	file, err := u.getFile(nzoID, filename)
	if err != nil {
		return fmt.Errorf("failed to get file: %w", err)
	}

	if len(file.Segments) == 0 {
		return fmt.Errorf("file has no segments: %s", file.Name)
	}

	// The segments of an encrypted archive member carry AES ciphertext, so
	// decrypt it on the way to the writer. Decryption overwrites each
	// segment's data, which was fetched for this download alone and is not
	// read again after the write.
	write := writer.Write
	var decrypter *cbcDecrypter
	if file.IsEncrypted {
		if len(file.EncryptionKey) == 0 {
			return customerror.NewPermanentError(fmt.Errorf("%s is encrypted and the NZB has no password", file.Name))
		}
		// The stored size, key and IV won't change on a retry, so a bad one is
		// permanent.
		if decrypter, err = newCBCDecrypter(writer, file.EncryptionKey, file.EncryptionIV, file.Size); err != nil {
			return customerror.NewPermanentError(fmt.Errorf("decrypt %s: %w", file.Name, err))
		}
		write = decrypter.write
	}

	// Track progress
	var completedSegments atomic.Int64
	var downloadedBytes atomic.Int64

	// Channel for segment results - buffered to allow parallel fetching ahead
	resultChan := make(chan segmentResult, max(u.processingMaxConnections, 1)*2)

	// Map to hold out-of-order segments waiting to be written
	pendingSegments := make(map[int][]byte)
	var pendingMu sync.Mutex
	nextToWrite := 0

	// Error tracking
	var writeErr error
	var writeErrMu sync.Mutex

	// Writer goroutine - writes segments in order as they arrive
	var writerWg sync.WaitGroup
	writerWg.Go(func() {
		for result := range resultChan {
			if result.err != nil {
				writeErrMu.Lock()
				if writeErr == nil {
					writeErr = result.err
				}
				writeErrMu.Unlock()
				continue
			}

			pendingMu.Lock()
			pendingSegments[result.index] = result.data

			// Write all consecutive segments starting from nextToWrite
			for {
				data, exists := pendingSegments[nextToWrite]
				if !exists {
					break
				}
				delete(pendingSegments, nextToWrite)
				pendingMu.Unlock()

				// Write to output
				n, err := write(data)
				if err != nil {
					writeErrMu.Lock()
					if writeErr == nil {
						writeErr = fmt.Errorf("write failed at segment %d: %w", nextToWrite, err)
					}
					writeErrMu.Unlock()
					pendingMu.Lock()
					break
				}

				completedSegments.Add(1)
				downloaded := downloadedBytes.Add(int64(n))
				nextToWrite++

				// Call progress callback if provided
				if progressCallback != nil {
					// Estimate speed (rough: assume ~1s per segment batch)
					completed := completedSegments.Load()
					speed := downloaded / max(1, completed) * int64(max(u.processingMaxConnections, 1))
					progressCallback(downloaded, speed)
				}

				pendingMu.Lock()
			}
			pendingMu.Unlock()
		}
	})

	// Fetch segments in parallel
	p := pool.New().WithContext(ctx).WithMaxGoroutines(max(u.processingMaxConnections, 1))

	for idx, segment := range file.Segments {
		segIdx := idx
		seg := segment

		p.Go(func(ctx context.Context) error {
			// Check for write errors
			writeErrMu.Lock()
			if writeErr != nil {
				writeErrMu.Unlock()
				return writeErr
			}
			writeErrMu.Unlock()

			// Check context
			if ctx.Err() != nil {
				return ctx.Err()
			}

			// Fetch segment using manager with failover
			var data []byte
			err := u.nntp.ExecuteWithFailover(ctx, nntp.WorkloadDownload, func(conn *nntp.Connection) error {
				d, e := conn.GetDecodedBody(seg.MessageID)
				data = d
				return e
			})
			if err != nil {
				resultChan <- segmentResult{index: segIdx, err: fmt.Errorf("segment %d: %w", segIdx, err)}
				return nil // Don't stop other workers
			}

			// Handle SegmentDataStart for sliced segments
			if seg.SegmentDataStart > 0 {
				if seg.SegmentDataStart >= int64(len(data)) {
					resultChan <- segmentResult{index: segIdx, err: fmt.Errorf("segment %d: offset exceeds data", segIdx)}
					return nil
				}
				data = data[seg.SegmentDataStart:]
			}

			// Trim to expected size
			if int64(len(data)) > seg.Bytes {
				data = data[:seg.Bytes]
			}

			resultChan <- segmentResult{index: segIdx, data: data}
			return nil
		})
	}

	// Wait for all fetches to complete, then close result channel
	fetchErr := p.Wait()
	close(resultChan)

	// Wait for writer to finish
	writerWg.Wait()

	// Check for errors
	if writeErr != nil {
		return writeErr
	}
	if fetchErr != nil {
		return fetchErr
	}
	if decrypter != nil {
		if err := decrypter.finish(); err != nil {
			return fmt.Errorf("decrypt %s: %w", file.Name, err)
		}
	}

	event := u.logger.Info().
		Str("file", filename).
		Int64("bytes", downloadedBytes.Load())
	if decrypter != nil {
		event = event.Bool("decrypted", true)
	}
	event.Msg("Download complete")

	return nil
}

// cbcDecrypter decrypts an encrypted RAR5 member as its segments arrive. The
// ciphertext is one AES-256-CBC chain across all volumes, padded to a whole
// block past the plaintext, and segments need not end on a block.
type cbcDecrypter struct {
	w     io.Writer
	mode  cipher.BlockMode
	carry [crypto.BlockSize]byte // start of a block the last write cut off
	held  int                    // bytes in carry
	left  int64                  // plaintext bytes still to write
}

// newCBCDecrypter returns a decrypter that writes the first size bytes of
// plaintext to w.
func newCBCDecrypter(w io.Writer, key, iv []byte, size int64) (*cbcDecrypter, error) {
	if size <= 0 {
		return nil, errors.New("the file has no size")
	}
	mode, err := crypto.NewDecrypter(key, iv)
	if err != nil {
		return nil, err
	}
	return &cbcDecrypter{w: w, mode: mode, left: size}, nil
}

// write decrypts p in place, after any partial block carried from the last
// call, and writes the plaintext to w, dropping the padding past the
// plaintext size. p must be the caller's to overwrite. It returns the number
// of plaintext bytes written.
func (d *cbcDecrypter) write(p []byte) (int, error) {
	var written int
	if d.held > 0 {
		k := copy(d.carry[d.held:], p)
		d.held += k
		p = p[k:]
		if d.held < crypto.BlockSize {
			return 0, nil
		}
		d.held = 0
		d.mode.CryptBlocks(d.carry[:], d.carry[:])
		n, err := d.emit(d.carry[:])
		written += n
		if err != nil {
			return written, err
		}
	}

	whole := len(p) - len(p)%crypto.BlockSize
	d.mode.CryptBlocks(p[:whole], p[:whole])
	d.held = copy(d.carry[:], p[whole:])
	n, err := d.emit(p[:whole])
	return written + n, err
}

// emit writes plaintext to w up to the plaintext size.
func (d *cbcDecrypter) emit(plain []byte) (int, error) {
	plain = plain[:min(int64(len(plain)), d.left)]
	if len(plain) == 0 {
		return 0, nil
	}
	n, err := d.w.Write(plain)
	d.left -= int64(n)
	return n, err
}

// finish reports ciphertext that ended mid-block or short of the plaintext.
func (d *cbcDecrypter) finish() error {
	if d.held > 0 {
		return fmt.Errorf("encrypted data ends %d bytes into an AES block", d.held)
	}
	if d.left > 0 {
		return fmt.Errorf("encrypted data ends %d bytes short of the file size", d.left)
	}
	return nil
}
