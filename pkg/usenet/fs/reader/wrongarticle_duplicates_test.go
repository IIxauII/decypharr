package reader

// Audit tests for provider-side duplicate articles (Eweka keeps several
// posts under one message-id and returns either copy per connection).
// Fixtures are the real raw bodies forensics captured; see
// .audit/articles/fixtures.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
)

const fixtureDir = "testdata/wrongarticle"

func loadRaw(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Skip(err)
	}
	return bytes.TrimSuffix(b, []byte(".\r\n"))
}

func loadDec(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureDir, name))
	if err != nil {
		t.Skip(err)
	}
	return b
}

// dupServer serves, on its k-th accepted connection, copies[id][min(k, len-1)].
type dupServer struct {
	ln     net.Listener
	copies map[string][][]byte
	conns  atomic.Int64
	bodies atomic.Int64
}

func newDupServer(t *testing.T, copies map[string][][]byte) *dupServer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &dupServer{ln: ln, copies: copies}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			k := int(s.conns.Add(1) - 1)
			go func(c net.Conn) {
				defer c.Close()
				r, w := bufio.NewReader(c), bufio.NewWriter(c)
				w.WriteString("200 dup\r\n")
				w.Flush()
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					f := strings.Fields(line)
					if len(f) == 0 {
						continue
					}
					switch strings.ToUpper(f[0]) {
					case "DATE":
						w.WriteString("111 20260101000000\r\n")
					case "BODY":
						list := s.copies[f[1]]
						if len(list) == 0 {
							w.WriteString("430 no such article\r\n")
							break
						}
						s.bodies.Add(1)
						// Like Eweka: the requested id is echoed even for a foreign body.
						fmt.Fprintf(w, "222 0 %s body\r\n", f[1])
						w.Write(list[min(k, len(list)-1)])
						w.WriteString(".\r\n")
					default:
						w.WriteString("500 unknown\r\n")
					}
					if r.Buffered() == 0 {
						w.Flush()
					}
				}
			}(c)
		}
	}()
	return s
}

func (s *dupServer) addr() (string, int) { return "127.0.0.1", s.ln.Addr().(*net.TCPAddr).Port }

// softWarTable is the real Soft War layout: 749 x 3840000 + 511937, plain post.
func softWarTable() []SegmentMeta {
	const n, size, last = 750, 3840000, 511937
	segs := make([]SegmentMeta, n)
	var off int64
	for i := range segs {
		b := int64(size)
		if i == n-1 {
			b = last
		}
		segs[i] = SegmentMeta{MessageID: fmt.Sprintf("<sw%d@fixture>", i), Number: i + 1, Bytes: b, StartOffset: off, EndOffset: off + b - 1}
		off += b
	}
	segs[40].MessageID = "<NYMYqMz7e-0-8@e-3-R9KwTeDAp>"
	segs[81].MessageID = "<LKtApgoBS-I-w@R-r-WshjkjU8K>"
	return segs
}

// pokemonTable is the first three segments of the Pokemon RAR member.
func pokemonTable() []SegmentMeta {
	return []SegmentMeta{
		{MessageID: "<C0v3or0MtVLkAKpRhsxrJNQez6@UATE6Zjr.ctY>", Number: 1, Bytes: 767910, StartOffset: 0, EndOffset: 767909, SegmentDataStart: 90},
		{MessageID: "<JS4GUPhBuSgC0BrDG2192vu4J@Ax9bFgR.VfS>", Number: 2, Bytes: 768000, StartOffset: 767910, EndOffset: 1535909},
		{MessageID: "<IEy2o-gIFBvHrsNABLEJ-HjVnZ@3yetrBEm.won>", Number: 3, Bytes: 768000, StartOffset: 1535910, EndOffset: 2303909},
	}
}

func dupReader(t *testing.T, retention Retention, s *dupServer, segs []SegmentMeta) *StreamingReader {
	h, p := s.addr()
	client, err := nntp.NewClient(&config.Config{Usenet: config.Usenet{Providers: []config.UsenetProvider{
		{Host: h, Port: p, Backbone: "eweka", Priority: 1, MaxConnections: 2}}}})
	if err != nil {
		t.Fatal(err)
	}
	opts := []Option{WithRetention(retention), WithMaxConnections(2), WithPrefetchAhead(0), WithBodyPipelineDepth(2), WithDownloadTimeout(20 * time.Second)}
	if retention == RetentionRewind {
		opts = append(opts, WithDiskPath(t.TempDir()))
	}
	sr, err := NewStreamingReader(t.Context(), client, segs, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sr.Close(); _ = client.Close() })
	return sr
}

func fetchVia(t *testing.T, sr *StreamingReader, path string, idx int) error {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	if path == "demand" {
		return sr.fetcher.Fetch(ctx, idx)
	}
	return sr.fetcher.fetchPrefetchBatch(ctx, []int{idx})
}

func TestAuditDuplicateFixtures(t *testing.T) {
	type tc struct {
		name    string
		table   func() []SegmentMeta
		idx     int
		raw     string
		want    string // decoded fixture for genuine copies; "" = must be rejected
		dataOff int
	}
	cases := []tc{
		{"softwar-idx40-foreign", softWarTable, 40, "softwar-idx40-foreign.raw", "", 0},
		{"softwar-idx81-foreign", softWarTable, 81, "softwar-idx81-foreign.raw", "", 0},
		{"pokemon-idx0-foreign", pokemonTable, 0, "idx0.raw", "", 90},
		{"softwar-idx40-genuine", softWarTable, 40, "seg041.raw", "seg041.pydec", 0},
		{"pokemon-idx1-genuine", pokemonTable, 1, "idx1.raw", "idx1.pydec", 0},
	}
	for _, c := range cases {
		for _, retention := range []Retention{RetentionRewind, RetentionDelivery} {
			for _, path := range []string{"demand", "prefetch-batch"} {
				t.Run(fmt.Sprintf("%s/retention-%d/%s", c.name, retention, path), func(t *testing.T) {
					segs := c.table()
					s := newDupServer(t, map[string][][]byte{segs[c.idx].MessageID: {loadRaw(t, c.raw)}})
					sr := dupReader(t, retention, s, segs)
					err := fetchVia(t, sr, path, c.idx)
					state, stored := sr.cache.GetState(c.idx), sr.cache.SegmentDataSize(c.idx)
					if c.want == "" {
						if err == nil || state == StateOnDisk || !nntp.IsWrongArticleError(err) {
							t.Fatalf("foreign body not rejected: err=%v state=%s stored=%d", err, state, stored)
						}
						if got := s.bodies.Load(); got != 3 {
							t.Errorf("BODY attempts = %d, want 3 (one per fresh connection)", got)
						}
						t.Logf("rejected after %d BODYs: %v", s.bodies.Load(), err)
						return
					}
					if err != nil || state != StateOnDisk {
						t.Fatalf("genuine body rejected: err=%v state=%s", err, state)
					}
					want := loadDec(t, c.want)[c.dataOff:][:segs[c.idx].Bytes]
					got := make([]byte, len(want))
					n, ok := sr.cache.ReadRangeInto(c.idx, 0, int64(len(want)), got)
					if !ok || n != len(want) || !bytes.Equal(got, want) {
						t.Fatalf("genuine body stored wrong: n=%d ok=%t", n, ok)
					}
				})
			}
		}
	}
}

// Eweka-like: the same message-id returns the foreign copy on the first two
// connections and the genuine one on the third. The fetch must end with the
// genuine bytes and never publish the foreign ones.
func TestAuditDuplicateRetriesOnFreshConnection(t *testing.T) {
	segs := softWarTable()
	id := segs[40].MessageID
	foreign, genuine := loadRaw(t, "softwar-idx40-foreign.raw"), loadRaw(t, "seg041.raw")
	want := loadDec(t, "seg041.pydec")
	for _, retention := range []Retention{RetentionRewind, RetentionDelivery} {
		for _, path := range []string{"demand", "prefetch-batch"} {
			t.Run(fmt.Sprintf("retention-%d/%s", retention, path), func(t *testing.T) {
				s := newDupServer(t, map[string][][]byte{id: {foreign, foreign, genuine}})
				sr := dupReader(t, retention, s, segs)
				if err := fetchVia(t, sr, path, 40); err != nil {
					t.Fatalf("fetch: %v", err)
				}
				got := make([]byte, len(want))
				n, ok := sr.cache.ReadRangeInto(40, 0, int64(len(want)), got)
				if !ok || n != len(want) || !bytes.Equal(got, want) {
					t.Fatalf("stored bytes are not the genuine part: n=%d ok=%t", n, ok)
				}
				t.Logf("genuine copy stored after %d BODYs on %d connections", s.bodies.Load(), s.conns.Load())
			})
		}
	}
}

// Read-side hardening: a hole reads as an error, never (0, nil) or (n-1, nil).
func TestAuditShortSegmentNeverReadsAsSuccess(t *testing.T) {
	segs := pokemonTable()
	s := newDupServer(t, map[string][][]byte{segs[0].MessageID: {loadRaw(t, "idx0.raw")}})
	sr := dupReader(t, RetentionRewind, s, segs)
	for _, off := range []int64{398245, 398246, 398247} {
		one := make([]byte, 1)
		n, err := sr.ReadAt(one, off)
		if err == nil || !nntp.IsWrongArticleError(err) && !errors.Is(err, ErrShortSegment) {
			t.Errorf("ReadAt(1 @ %d) = %d, %v; want a wrong-article error", off, n, err)
		}
	}
}
