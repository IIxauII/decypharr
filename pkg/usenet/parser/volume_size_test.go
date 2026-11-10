package parser

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
	"github.com/sirrobot01/decypharr/pkg/storage"
	"github.com/sirrobot01/decypharr/pkg/usenet/manifest"
)

const (
	volumeTestArticle = 16 << 10
	// Full volumes end on an article boundary, like 15,360,000-byte volumes
	// posted as 768,000-byte articles.
	volumeTestVolume = 4 * volumeTestArticle
	// shortLastVolume is member data for a two-article last volume.
	shortLastVolume = volumeTestArticle + 4000
)

func TestRARVolumeSizesIgnoreSubjectFileOrder(t *testing.T) {
	for _, tc := range []struct {
		name string
		// volumes is the number of volumes; nameFormat names volume v+1.
		volumes    int
		nameFormat string
		// numbers are the [N/M] subject indices of the volumes. nil posts
		// subjects without one, which all parse as file 1.
		numbers []int
		// document lists the volumes in NZB order; nil is volume order.
		document []int
		// lastData is the number of member bytes in the last volume.
		lastData int
		// encoded writes encoded article sizes into the NZB instead of
		// decoded ones.
		encoded bool
		// strays also posts album.m3u, which joins the group as media,
		// and album.log, which content detection drops.
		strays bool
	}{
		{name: "upload order", volumes: 4, nameFormat: "album.part%d.rar", numbers: []int{1, 2, 3, 4}, lastData: shortLastVolume},
		// The two-article last volume neither sorts last by file number nor
		// has the most articles.
		{name: "short last volume out of order", volumes: 4, nameFormat: "album.part%d.rar", numbers: []int{3, 4, 1, 2}, lastData: shortLastVolume},
		{name: "short last volume out of order, encoded sizes", volumes: 4, nameFormat: "album.part%d.rar", numbers: []int{3, 4, 1, 2}, lastData: shortLastVolume, encoded: true},
		// The last volume has as many articles as the others and sorts
		// first by file number.
		{name: "last volume as reference", volumes: 4, nameFormat: "album.part%d.rar", numbers: []int{2, 4, 3, 1}, lastData: 3*volumeTestArticle + 4000},
		// Without file numbers the parser numbers files in NZB order.
		{
			name: "13 volumes without file numbers, shuffled", volumes: 13, nameFormat: "album.part%02d.rar",
			document: []int{12, 4, 0, 9, 2, 11, 6, 1, 10, 3, 8, 5, 7}, lastData: shortLastVolume,
		},
		{name: "stray playlist after the volumes", volumes: 4, nameFormat: "album.part%d.rar", numbers: []int{1, 2, 3, 4}, lastData: shortLastVolume, strays: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config.Reset()
			config.SetConfigPath(t.TempDir())
			t.Cleanup(config.Reset)
			_ = config.Get()

			server, err := nntpd.New(nntpd.Config{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(server.Close)

			const member = "01 - Track.flac"
			plain, volumes := storedRAR5Set(t, member, tc.volumes, tc.lastData)
			files := len(volumes)
			if tc.strays {
				files += 2
			}
			document := tc.document
			if document == nil {
				for v := range volumes {
					document = append(document, v)
				}
			}
			articles := make(map[string][]byte)
			postedSizes := make(map[string]int64)
			names := make([]string, len(volumes))
			var entries strings.Builder
			for _, v := range document {
				names[v] = fmt.Sprintf(tc.nameFormat, v+1)
				postedSizes[names[v]] = int64(len(volumes[v]))
				number := 0
				if tc.numbers != nil {
					number = tc.numbers[v]
				}
				entries.WriteString(postFile(server, articles, names[v], volumes[v], number, files, tc.encoded))
			}
			if tc.strays {
				entries.WriteString(postFile(server, articles, "album.m3u", []byte("#EXTM3U\n01 - Track.flac\n"), files-1, files, tc.encoded))
				entries.WriteString(postFile(server, articles, "album.log", []byte("ripped with care\n"), files, files, tc.encoded))
			}
			source := `<?xml version="1.0" encoding="UTF-8"?>
<nzb xmlns="http://www.newzbin.com/DTD/2003/nzb">
` + entries.String() + `</nzb>`

			host, port := server.Addr()
			client, err := nntp.NewClient(&config.Config{Usenet: config.Usenet{
				Providers: []config.UsenetProvider{{Host: host, Port: port, MaxConnections: 4}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			p := NewParser(client, 4, zerolog.Nop())

			nzb, groups, err := p.Parse(t.Context(), "album.nzb", []byte(source))
			if err != nil {
				t.Fatal(err)
			}
			group := groups["album"]
			if len(groups) != 1 || group == nil {
				t.Fatalf("file groups = %v, want only album", groups)
			}
			if want := len(volumes) + min(files-len(volumes), 1); len(group.Files) != want {
				t.Fatalf("album group has %d files, want %d", len(group.Files), want)
			}

			// Every volume must be sized exactly as posted, whatever the
			// archive inside makes of a wrong size, from the headers of the
			// first and last volumes alone.
			before := server.Bodies.Load()
			if err := p.enrichGroupWithFileInfo(t.Context(), group); err != nil {
				t.Fatal(err)
			}
			if got := server.Bodies.Load() - before; got > 2 {
				t.Errorf("sizing the volumes read %d articles, want at most 2", got)
			}
			for v, name := range names {
				if got := group.Files[v].Filename; got != name {
					t.Errorf("file %d of the group is %s, want %s", v+1, got, name)
				}
			}
			described, err := buildArchiveVolumeDescriptors(group)
			if err != nil {
				t.Fatal(err)
			}
			for _, volume := range described {
				if want, ok := postedSizes[volume.Name]; ok && volume.Size != want {
					t.Errorf("volume %s sized %d bytes, want %d", volume.Name, volume.Size, want)
				}
			}

			nzb, err = p.Process(t.Context(), nzb, groups)
			if err != nil {
				t.Fatal(err)
			}
			var file *storage.NZBFile
			for i := range nzb.Files {
				if nzb.Files[i].InternalPath == member {
					file = &nzb.Files[i]
				}
			}
			if file == nil {
				t.Fatalf("parsed NZB has no member %q", member)
			}
			if file.Size != int64(len(plain)) {
				t.Fatalf("member size = %d, want %d", file.Size, len(plain))
			}
			// The member's segments must select exactly its bytes from the
			// posted articles.
			var got []byte
			for _, segment := range file.Segments {
				article := articles[segment.MessageID]
				start, end := segment.SegmentDataStart, segment.SegmentDataStart+segment.Bytes
				if start < 0 || end > int64(len(article)) {
					t.Fatalf("segment %s selects bytes [%d, %d) of a %d-byte article", segment.MessageID, start, end, len(article))
				}
				got = append(got, article[start:end]...)
			}
			if !bytes.Equal(got, plain) {
				t.Fatalf("member segments select %d bytes that differ from the %d-byte member", len(got), len(plain))
			}
		})
	}
}

func TestSplitArchiveVolumeSizesIgnoreSubjectFileOrder(t *testing.T) {
	// Two full four-article volumes and a short last one, whose subject
	// numbers it first. NZB byte counts are decoded sizes. A stray playlist
	// named after the release joins the group: it sorts after 7z volumes and
	// before the .zip of a ZIP set.
	sizes := [3]int64{400, 400, 150}
	numbers := [3]int{2, 3, 1}
	for _, tc := range []struct {
		fileType storage.NZBFileType
		names    [3]string // in volume order
	}{
		{fileType: storage.NZBFileTypeZip, names: [3]string{"a.z01", "a.z02", "a.zip"}},
		{fileType: storage.NZBFileTypeSevenZip, names: [3]string{"a.7z.001", "a.7z.002", "a.7z.003"}},
	} {
		t.Run(string(tc.fileType), func(t *testing.T) {
			headers := make(headerSource)
			want := make(map[string]int64)
			group := &FileGroup{BaseName: "a", Type: tc.fileType, fileMeta: make(map[string]filePartMeta)}
			for v, name := range tc.names {
				file := manifest.File{Filename: name, Number: numbers[v], Order: numbers[v] - 1}
				for offset := int64(0); offset < sizes[v]; offset += 100 {
					number := len(file.Segments) + 1
					file.Segments = append(file.Segments, manifest.Segment{
						Number: number, MessageID: fmt.Sprintf("%s.%d", name, number), Bytes: min(100, sizes[v]-offset),
					})
				}
				headers[file.Segments[0].MessageID] = &nntp.YencMetadata{Name: name, Size: sizes[v], PartSize: 100}
				want[name] = sizes[v]
				group.Files = append(group.Files, file)
			}
			stray := manifest.File{Filename: "a.m3u", Number: 4, Order: 3, Segments: []manifest.Segment{{Number: 1, MessageID: "a.m3u.1", Bytes: 30}}}
			headers["a.m3u.1"] = &nntp.YencMetadata{Name: stray.Filename, Size: 30, PartSize: 30}
			group.Files = append(group.Files, stray)

			p := NewParserWithSource(headers, 2, zerolog.Nop())
			if err := p.enrichGroupWithFileInfo(t.Context(), group); err != nil {
				t.Fatal(err)
			}
			volumes, err := buildArchiveVolumeDescriptors(group)
			if err != nil {
				t.Fatal(err)
			}
			for _, volume := range volumes {
				if size, ok := want[volume.Name]; ok && volume.Size != size {
					t.Errorf("volume %s sized %d bytes, want %d", volume.Name, volume.Size, size)
				}
			}
		})
	}
}

func TestSortGroupFilesBreaksTiesByNZBOrder(t *testing.T) {
	// Obfuscated names carry no volume key and their subjects parse as one
	// file number, so only NZB order tells them apart. The names sort in
	// reverse of it.
	obfuscated := [3]string{"zQ8vB2kL9xRt", "mN4pW7cJ1aYe", "aH6sD3fG5uIo"}
	for _, tc := range []struct {
		fileType storage.NZBFileType
		names    [3]string // by NZB order
	}{
		{fileType: storage.NZBFileTypeRar, names: obfuscated},
		{fileType: storage.NZBFileTypeZip, names: obfuscated},
		{fileType: storage.NZBFileTypeMedia, names: obfuscated},
		// 7z sorts by name, so only equal names tie.
		{fileType: storage.NZBFileTypeSevenZip, names: [3]string{"x.7z.001", "x.7z.001", "x.7z.001"}},
	} {
		t.Run(string(tc.fileType), func(t *testing.T) {
			group := &FileGroup{Type: tc.fileType}
			for _, order := range []int{2, 0, 1} {
				group.Files = append(group.Files, manifest.File{Filename: tc.names[order], Number: 1, Order: order})
			}
			sortGroupFiles(group)
			for i, file := range group.Files {
				if file.Order != i {
					t.Fatalf("position %d holds NZB file %d (%s), want NZB file %d", i, file.Order, file.Filename, i)
				}
			}
		})
	}
}

// headerSource serves the yEnc headers of the articles it holds. It stands in
// for the article broker because fakeArticleBackend gives every article the
// same header.
type headerSource map[string]*nntp.YencMetadata

func (s headerSource) Header(_ context.Context, messageID string, _ int) (*nntp.YencMetadata, error) {
	if metadata, ok := s[messageID]; ok {
		return metadata, nil
	}
	return nil, errors.New("no header for " + messageID)
}

func (headerSource) Body(context.Context, string) ([]byte, error) {
	return nil, errors.New("no bodies")
}
func (headerSource) Stat(context.Context, string) error { return nil }
func (headerSource) IsAvailable(string) bool            { return false }
func (headerSource) Metrics() ArticleMetrics            { return ArticleMetrics{} }

// storedRAR5Set stores member uncompressed in count RAR5 volumes. All but the
// last are exactly volumeTestVolume bytes, as RAR cuts them; the last holds
// the remaining lastData member bytes.
func storedRAR5Set(t *testing.T, member string, count, lastData int) ([]byte, [][]byte) {
	t.Helper()
	sizes := make([]int, count)
	for v := range count - 1 {
		// The first volume's main header is one byte shorter than the rest.
		overhead := len(rar5Volume(member, int64(count*volumeTestVolume), make([]byte, volumeTestVolume), v, count)) - volumeTestVolume
		sizes[v] = volumeTestVolume - overhead
	}
	sizes[count-1] = lastData
	total := 0
	for _, size := range sizes {
		total += size
	}
	plain := nntpd.Pattern(0, total)
	volumes := make([][]byte, count)
	offset := 0
	for v, size := range sizes {
		volumes[v] = rar5Volume(member, int64(total), plain[offset:offset+size], v, count)
		offset += size
		if v < count-1 && len(volumes[v]) != volumeTestVolume {
			t.Fatalf("volume %d is %d bytes, want %d", v+1, len(volumes[v]), volumeTestVolume)
		}
	}
	return plain, volumes
}

// rar5Volume lays out one volume of a multi-volume RAR5 set: signature, main
// header, the member's file header and its data part, then the
// end-of-archive header.
func rar5Volume(member string, unpackedSize int64, data []byte, index, volumes int) []byte {
	const flagSplitBefore, flagSplitAfter = 0x0008, 0x0010
	out := []byte(RAR5Signature)

	mainBody := binary.AppendUvarint(nil, RAR5MainFlagVolume)
	if index > 0 {
		mainBody = binary.AppendUvarint(nil, RAR5MainFlagVolume|RAR5MainFlagVolumeNumber)
		mainBody = binary.AppendUvarint(mainBody, uint64(index))
	}
	out = append(out, rar5Header(RAR5HeaderTypeMain, 0, mainBody, -1)...)

	fileFlags := uint64(RAR5HeaderFlagDataArea)
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
	out = append(out, rar5Header(RAR5HeaderTypeFile, fileFlags, fileBody, len(data))...)
	out = append(out, data...)

	endFlags := uint64(0)
	if index < volumes-1 {
		endFlags = 1 // more volumes follow
	}
	return append(out, rar5Header(RAR5HeaderTypeEndOfArc, 0, binary.AppendUvarint(nil, endFlags), -1)...)
}

// rar5Header encodes a RAR5 block header. dataSize < 0 means no data area.
func rar5Header(headerType, flags uint64, body []byte, dataSize int) []byte {
	content := binary.AppendUvarint(nil, headerType)
	content = binary.AppendUvarint(content, flags)
	if dataSize >= 0 {
		content = binary.AppendUvarint(content, uint64(dataSize))
	}
	content = append(content, body...)
	sized := binary.AppendUvarint(nil, uint64(len(content)))
	sized = append(sized, content...)
	return append(binary.LittleEndian.AppendUint32(nil, crc32.ChecksumIEEE(sized)), sized...)
}

// postFile posts data as volumeTestArticle-byte articles, records each
// article's decoded payload by message ID and returns the file's NZB <file>.
// A number above 0 starts the subject with "[number/files]". The NZB byte
// counts are the decoded article sizes unless encoded is set.
func postFile(server *nntpd.Server, articles map[string][]byte, name string, data []byte, number, files int, encoded bool) string {
	parts := (len(data) + volumeTestArticle - 1) / volumeTestArticle
	var segments strings.Builder
	for part := range parts {
		start := part * volumeTestArticle
		end := min(start+volumeTestArticle, len(data))
		messageID := fmt.Sprintf("%s.%d@volume-test", name, part+1)
		body := nntpd.Encode(data[start:end], name, part+1, int64(len(data)), int64(start))
		server.AddArticle("<"+messageID+">", body)
		articles[messageID] = data[start:end]
		size := end - start
		if encoded {
			size = len(body)
		}
		fmt.Fprintf(&segments, "<segment bytes=\"%d\" number=\"%d\">%s</segment>\n", size, part+1, messageID)
	}
	subject := fmt.Sprintf("&quot;%s&quot; yEnc (1/%d)", name, parts)
	if number > 0 {
		subject = fmt.Sprintf("[%d/%d] - %s", number, files, subject)
	}
	return fmt.Sprintf(`<file poster="poster" date="1" subject="%s">
<groups><group>alt.test</group></groups>
<segments>
%s</segments>
</file>
`, subject, segments.String())
}
