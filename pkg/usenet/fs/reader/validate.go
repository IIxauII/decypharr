package reader

import (
	"errors"
	"fmt"

	"github.com/sirrobot01/decypharr/internal/nntp"
	nntpyenc "github.com/sirrobot01/decypharr/internal/nntp/yenc"
)

// ErrShortSegment marks a decoded article that holds fewer bytes than the
// segment table maps onto it.
var ErrShortSegment = errors.New("article shorter than its segment table entry")

// ErrWrongArticle marks a CRC-valid article that is not the requested part.
// Providers can hold several posts under one message-id and return either
// copy per connection, so the caller retries on another connection.
var ErrWrongArticle = errors.New("article is not the requested segment")

// articleExpectation is what the NZB says the article behind one segment is.
// Zero fields are unknown and not checked.
type articleExpectation struct {
	part     int64 // =ybegin part
	altPart  int64 // also accepted (posters that number NZB segments from 0)
	begin    int64 // =ypart begin, 1-based
	partSize int64 // =ypart end - begin + 1
	total    int64 // =ybegin total
	fileSize int64 // =ybegin size
}

// segmentValidator checks every decoded body against the segment it was
// requested for before the cache may publish it.
type segmentValidator struct {
	segments []SegmentMeta
	expect   []articleExpectation
}

// newSegmentValidator derives per-segment expectations from the table.
//
// Every segment gets a part-number check. When the whole file is a plain
// post (no SegmentDataStart, contiguous offsets, segment numbers forming
// runs that start at 0 or 1, one run per posted file), StartOffset maps
// 1:1 onto the posted file, so =ypart begin/size and =ybegin total/size are
// known exactly too. Archive members (RAR/7z/zip slices) carry a
// SegmentDataStart and keep the part-number and size checks only.
func newSegmentValidator(segments []SegmentMeta) *segmentValidator {
	v := &segmentValidator{segments: segments, expect: make([]articleExpectation, len(segments))}
	plain := len(segments) > 0
	for i, seg := range segments {
		if seg.SegmentDataStart != 0 || seg.Bytes <= 0 || seg.Number < 0 ||
			(i > 0 && seg.StartOffset != segments[i-1].EndOffset+1) {
			plain = false
		}
		if seg.Number > 0 {
			v.expect[i].part = int64(seg.Number)
			v.expect[i].altPart = int64(seg.Number) + 1
		} else if seg.Number == 0 {
			v.expect[i].part = 1
		}
	}
	if !plain {
		return v
	}
	for start := 0; start < len(segments); {
		end := start + 1
		for end < len(segments) && segments[end].Number == segments[end-1].Number+1 {
			end++
		}
		first := segments[start].Number
		if first == 0 || first == 1 {
			base := segments[start].StartOffset
			var fileSize int64
			for _, seg := range segments[start:end] {
				fileSize += seg.Bytes
			}
			for i := start; i < end; i++ {
				e := &v.expect[i]
				e.part = int64(segments[i].Number) + int64(1-first)
				e.altPart = 0
				e.begin = segments[i].StartOffset - base + 1
				e.partSize = segments[i].Bytes
				e.total = int64(end - start)
				e.fileSize = fileSize
			}
		}
		start = end
	}
	return v
}

// check validates one decoded body. stored is what the cache writer kept
// (decoded length minus SegmentDataStart, capped at seg.Bytes).
func (v *segmentValidator) check(segIdx int, stored int64, meta nntpyenc.DecoderMeta) error {
	if v == nil || segIdx < 0 || segIdx >= len(v.segments) {
		return nil
	}
	seg := v.segments[segIdx]
	if seg.Bytes > 0 && stored < seg.Bytes {
		return nntp.NewWrongArticleError(fmt.Errorf("segment %d %s: article holds %d of the %d bytes the segment table expects (yEnc part %d/%d of %q): %w",
			seg.Number, seg.MessageID, stored, seg.Bytes, meta.PartNumber, meta.TotalParts, meta.FileName, ErrShortSegment))
	}
	e := v.expect[segIdx]
	var mismatch string
	switch {
	case e.part > 0 && meta.PartNumber > 0 && meta.PartNumber != e.part && meta.PartNumber != e.altPart:
		mismatch = fmt.Sprintf("part %d, want %d", meta.PartNumber, e.part)
	case e.begin > 0 && meta.PartSize > 0 && meta.Begin() != e.begin:
		mismatch = fmt.Sprintf("=ypart begin %d, want %d", meta.Begin(), e.begin)
	case e.partSize > 0 && meta.PartSize > 0 && meta.PartSize != e.partSize:
		mismatch = fmt.Sprintf("part size %d, want %d", meta.PartSize, e.partSize)
	case e.total > 0 && meta.TotalParts > 0 && meta.TotalParts != e.total:
		mismatch = fmt.Sprintf("total %d, want %d", meta.TotalParts, e.total)
	case e.fileSize > 0 && meta.FileSize > 0 && meta.FileSize != e.fileSize:
		mismatch = fmt.Sprintf("file size %d, want %d", meta.FileSize, e.fileSize)
	}
	if mismatch == "" {
		return nil
	}
	return nntp.NewWrongArticleError(fmt.Errorf("segment %d %s: provider returned %q part %d/%d (%s): %w",
		seg.Number, seg.MessageID, meta.FileName, meta.PartNumber, meta.TotalParts, mismatch, ErrWrongArticle))
}
