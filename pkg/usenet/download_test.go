package usenet

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
	"github.com/sirrobot01/decypharr/pkg/storage"
)

// The RAR5 key for password "password", an all-zero salt and KDF count 1.
// It is the known answer from parser.TestRAR5ExtraRecordBoundaries, so the
// fixture is encrypted independently of the parser's key derivation.
const (
	downloadTestPassword = "password"
	downloadTestKDF      = 1
	downloadTestKeyHex   = "9e41dea5935137cf748669cbf242b3ea049d20e0add3db231dabc9840bc58a6a"
)

func TestDownloadWritesArchiveMemberPlaintext(t *testing.T) {
	// 600009 bytes: not a multiple of the AES block, so the encrypted member
	// carries 7 bytes of padding, and longer than one copy buffer.
	plain := make([]byte, 600009)
	copy(plain, "fLaC")
	for i := 4; i < len(plain); i++ {
		plain[i] = byte(i*7 + i/251)
	}

	for _, tc := range []struct {
		name      string
		encrypted bool
		// cuts splits the member's packed bytes across volumes. A cut that
		// isn't a multiple of 16 leaves one AES block straddling two volumes.
		cuts []int
	}{
		{name: "encrypted single volume", encrypted: true},
		{name: "encrypted multi volume", encrypted: true, cuts: []int{300007}},
		{name: "plain multi volume", cuts: []int{300007}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const member = "01 - Track.flac"
			server, u := newDownloadTestUsenet(t)
			source := postRAR5Member(t, server, member, plain, tc.encrypted, tc.cuts)

			nzb, groups, err := u.Parse(t.Context(), "album.nzb", []byte(source), "lidarr")
			if err != nil {
				t.Fatal(err)
			}
			nzb, err = u.Process(t.Context(), nzb, groups)
			if err != nil {
				t.Fatal(err)
			}
			// A single-member archive is renamed after the release, so find the
			// member by its path inside the archive.
			var file *storage.NZBFile
			for i := range nzb.Files {
				if nzb.Files[i].InternalPath == member {
					file = &nzb.Files[i]
				}
			}
			if file == nil {
				t.Fatalf("parsed NZB has no member %q", member)
			}
			if file.IsEncrypted != tc.encrypted || file.Size != int64(len(plain)) {
				t.Fatalf("parsed member encrypted=%v size=%d, want encrypted=%v size=%d", file.IsEncrypted, file.Size, tc.encrypted, len(plain))
			}
			spanned := map[string]bool{}
			for _, seg := range file.Segments {
				spanned[seg.MessageID[:strings.LastIndexByte(seg.MessageID, '.')]] = true
			}
			if len(spanned) != len(tc.cuts)+1 {
				t.Fatalf("member data spans %d volumes, want %d", len(spanned), len(tc.cuts)+1)
			}

			var out bytes.Buffer
			var reported int64
			if err := u.Download(t.Context(), nzb.ID, file.Name, &out, func(downloaded, _ int64) { reported = downloaded }); err != nil {
				t.Fatal(err)
			}
			if got := out.Bytes(); !bytes.Equal(got, plain) {
				at := 0
				for at < min(len(got), len(plain)) && got[at] == plain[at] {
					at++
				}
				t.Fatalf("downloaded %d bytes starting %x, want the %d-byte plaintext starting %x (first difference at offset %d)",
					len(got), got[:min(len(got), 4)], len(plain), plain[:4], at)
			}
			if reported != int64(len(plain)) {
				t.Fatalf("last progress report = %d bytes, want %d", reported, len(plain))
			}
		})
	}
}

func TestDownloadRejectsEncryptedMemberWithoutPassword(t *testing.T) {
	server, u := newDownloadTestUsenet(t)
	source := postRAR5Member(t, server, "01 - Track.flac", []byte("fLaC and the rest of the track"), true, nil)
	// Without the password the parser still marks the member encrypted, but
	// derives no key.
	source = strings.Replace(source, `<meta type="password">`+downloadTestPassword+`</meta>`, "", 1)

	nzb, groups, err := u.Parse(t.Context(), "album.nzb", []byte(source), "lidarr")
	if err != nil {
		t.Fatal(err)
	}
	if nzb, err = u.Process(t.Context(), nzb, groups); err != nil {
		t.Fatal(err)
	}
	if len(nzb.Files) != 1 || !nzb.Files[0].IsEncrypted {
		t.Fatalf("parsed NZB files = %+v, want one encrypted member", nzb.Files)
	}

	var out bytes.Buffer
	err = u.Download(t.Context(), nzb.ID, nzb.Files[0].Name, &out, nil)
	if err == nil || !strings.Contains(err.Error(), "password") {
		t.Fatalf("Download error = %v, want one naming the missing password", err)
	}
	if !customerror.IsPermanentError(err) || customerror.IsSilentError(err) {
		t.Fatalf("Download error %v: permanent=%v silent=%v, want permanent and not silent",
			err, customerror.IsPermanentError(err), customerror.IsSilentError(err))
	}
	if out.Len() != 0 {
		t.Fatalf("Download wrote %d bytes, want none", out.Len())
	}
}

func TestCBCDecrypterRejectsIncompleteCiphertext(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, 32)
	iv := bytes.Repeat([]byte{0x22}, aes.BlockSize)
	plain := []byte("forty bytes of plaintext, then padding.!")
	packed := make([]byte, 48)
	copy(packed, plain)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(packed, packed)

	for _, tc := range []struct {
		name      string
		size      int64
		stream    int    // ciphertext bytes fed in
		writes    []int  // sizes of the writes before the last, which takes the rest
		wantErr   string // empty: the plaintext comes out whole
		permanent bool
	}{
		{name: "one write", size: 40, stream: 48},
		{name: "7-byte writes", size: 40, stream: 48, writes: []int{7, 7, 7, 7, 7, 7}},
		// The 3-byte write leaves the carried block still short of 16 bytes.
		{name: "write shorter than the carry needs", size: 40, stream: 48, writes: []int{20, 3}},
		{name: "ends mid-block", size: 40, stream: 45, writes: []int{7, 7, 7, 7, 7, 7}, wantErr: "13 bytes into an AES block"},
		{name: "ends short of the size", size: 40, stream: 32, writes: []int{20}, wantErr: "8 bytes short"},
		{name: "no size", size: 0, stream: 48, wantErr: "no size", permanent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			file := &storage.NZBFile{Name: "track.flac", Size: tc.size, IsEncrypted: true, EncryptionKey: key, EncryptionIV: iv}
			err := func() error {
				d, err := newCBCDecrypter(&out, file)
				if err != nil {
					return err
				}
				// write decrypts in place, so it gets a copy of the stream.
				rest := bytes.Clone(packed[:tc.stream])
				for _, n := range tc.writes {
					if _, err := d.write(rest[:n]); err != nil {
						return err
					}
					rest = rest[n:]
				}
				if _, err := d.write(rest); err != nil {
					return err
				}
				return d.finish()
			}()
			if tc.wantErr == "" {
				if err != nil || !bytes.Equal(out.Bytes(), plain) {
					t.Fatalf("got %q, err %v; want %q", out.Bytes(), err, plain)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
			if customerror.IsPermanentError(err) != tc.permanent || customerror.IsSilentError(err) {
				t.Fatalf("err %v: permanent=%v silent=%v, want permanent=%v and not silent",
					err, customerror.IsPermanentError(err), customerror.IsSilentError(err), tc.permanent)
			}
		})
	}
}

// newDownloadTestUsenet starts a fake NNTP server and opens a Usenet client
// against it.
func newDownloadTestUsenet(t *testing.T) (*nntpd.Server, *Usenet) {
	t.Helper()
	server, err := nntpd.New(nntpd.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)

	config.Reset()
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)
	host, port := server.Addr()
	if _, err := config.Update(func(cfg *config.Config) error {
		cfg.Usenet.Providers = []config.UsenetProvider{{Host: host, Port: port, MaxConnections: 4}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	u, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = u.Close() })
	return server, u
}

// postRAR5Member stores one RAR5 member as a stored (uncompressed) archive,
// split into volumes at cuts, posts every volume as several articles and
// returns the NZB. An encrypted member is one AES-256-CBC chain over the whole
// member, keyed and IV'd from the first volume, as RAR5 writes it, and its NZB
// carries the password.
func postRAR5Member(t *testing.T, server *nntpd.Server, member string, plain []byte, encrypted bool, cuts []int) string {
	t.Helper()
	packed := plain
	var extra []byte
	head := "<head></head>"
	if encrypted {
		head = `<head><meta type="password">` + downloadTestPassword + `</meta></head>`
		key, err := hex.DecodeString(downloadTestKeyHex)
		if err != nil {
			t.Fatal(err)
		}
		iv := bytes.Repeat([]byte{0x5c}, aes.BlockSize)
		packed = make([]byte, (len(plain)+aes.BlockSize-1)/aes.BlockSize*aes.BlockSize)
		copy(packed, plain)
		block, err := aes.NewCipher(key)
		if err != nil {
			t.Fatal(err)
		}
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(packed, packed)

		// Encryption record: type, version 0, flags 0, KDF count, salt, IV.
		record := []byte{0x01, 0x00, 0x00, downloadTestKDF}
		record = append(record, make([]byte, 16)...)
		record = append(record, iv...)
		extra = append(binary.AppendUvarint(nil, uint64(len(record))), record...)
	}

	bounds := append(append([]int{0}, cuts...), len(packed))
	volumes := len(bounds) - 1
	var files strings.Builder
	for v := range volumes {
		name := "album.rar"
		if volumes > 1 {
			name = fmt.Sprintf("album.part%d.rar", v+1)
		}
		volume := rar5Volume(member, int64(len(plain)), extra, packed[bounds[v]:bounds[v+1]], v, volumes)
		files.WriteString(postVolume(server, name, volume))
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
` + head + "\n" + files.String() + `</nzb>`
}

// rar5Volume lays out one RAR5 volume: signature, main header, the member's
// file header and its data part, then the end-of-archive header.
func rar5Volume(member string, unpackedSize int64, extra, data []byte, index, volumes int) []byte {
	const (
		headerMain, headerFile, headerEnd = 1, 2, 5
		flagExtra, flagData               = 0x0001, 0x0002
		flagSplitBefore, flagSplitAfter   = 0x0008, 0x0010
		archiveVolume, archiveVolumeNum   = 0x0001, 0x0002
	)
	out := []byte("Rar!\x1a\x07\x01\x00")

	var mainBody []byte
	switch {
	case volumes == 1:
		mainBody = binary.AppendUvarint(nil, 0)
	case index == 0:
		mainBody = binary.AppendUvarint(nil, archiveVolume)
	default:
		mainBody = binary.AppendUvarint(nil, archiveVolume|archiveVolumeNum)
		mainBody = binary.AppendUvarint(mainBody, uint64(index))
	}
	out = append(out, rar5Header(headerMain, 0, mainBody, nil, -1)...)

	fileFlags := uint64(flagData)
	if len(extra) > 0 {
		fileFlags |= flagExtra
	}
	if index > 0 {
		fileFlags |= flagSplitBefore
	}
	if index < volumes-1 {
		fileFlags |= flagSplitAfter
	}
	// File flags 0, unpacked size, attributes, compression info 0 (stored),
	// host OS Unix, name.
	var fileBody []byte
	for _, value := range []uint64{0, uint64(unpackedSize), 0x1a4, 0, 1, uint64(len(member))} {
		fileBody = binary.AppendUvarint(fileBody, value)
	}
	fileBody = append(fileBody, member...)
	out = append(out, rar5Header(headerFile, fileFlags, fileBody, extra, len(data))...)
	out = append(out, data...)

	endFlags := uint64(0)
	if index < volumes-1 {
		endFlags = 1 // more volumes follow
	}
	return append(out, rar5Header(headerEnd, 0, binary.AppendUvarint(nil, endFlags), nil, -1)...)
}

// rar5Header encodes a RAR5 block header. dataSize < 0 means no data area.
func rar5Header(headerType, flags uint64, body, extra []byte, dataSize int) []byte {
	content := binary.AppendUvarint(nil, headerType)
	content = binary.AppendUvarint(content, flags)
	if len(extra) > 0 {
		content = binary.AppendUvarint(content, uint64(len(extra)))
	}
	if dataSize >= 0 {
		content = binary.AppendUvarint(content, uint64(dataSize))
	}
	content = append(content, body...)
	content = append(content, extra...)
	sized := binary.AppendUvarint(nil, uint64(len(content)))
	sized = append(sized, content...)
	return append(binary.LittleEndian.AppendUint32(nil, crc32.ChecksumIEEE(sized)), sized...)
}

// postVolume posts a volume as 128 KiB articles and returns its NZB <file>.
func postVolume(server *nntpd.Server, name string, volume []byte) string {
	const articleSize = 128 << 10
	parts := (len(volume) + articleSize - 1) / articleSize
	var segments strings.Builder
	for part := range parts {
		start := part * articleSize
		end := min(start+articleSize, len(volume))
		messageID := fmt.Sprintf("%s.%d@download-test", name, part+1)
		body := nntpd.Encode(volume[start:end], name, part+1, int64(len(volume)), int64(start))
		server.AddArticle("<"+messageID+">", body)
		fmt.Fprintf(&segments, "<segment bytes=\"%d\" number=\"%d\">%s</segment>\n", len(body), part+1, messageID)
	}
	return fmt.Sprintf(`<file poster="poster" date="1" subject="&quot;%s&quot; yEnc (1/%d)">
<groups><group>alt.test</group></groups>
<segments>
%s</segments>
</file>
`, name, parts, segments.String())
}
