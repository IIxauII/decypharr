package reader

// Audit tests for the 398336-byte short commit seen on segment idx 40/81.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
)

const auditSeg = 3840000
const auditShort = 398336

// misroutingServer answers BODY <want> with the stored body of another
// message-id, the way a pooled connection that is one response behind would.
func misroutingServer(t *testing.T, bodies map[string][]byte, route map[string]string) (string, int) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r, w := bufio.NewReader(c), bufio.NewWriter(c)
				w.WriteString("200 ok\r\n")
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
						id := f[1]
						if other, ok := route[id]; ok {
							id = other
						}
						fmt.Fprintf(w, "222 0 %s body\r\n", id)
						w.Write(bodies[id])
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
	return "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
}

// A complete, valid response for a different (398336-byte) article is
// published as segment 1 with length 398336, and the stream then serves a
// silent hole at in-segment offset 398336, exactly the live symptom.
func TestAuditMisroutedResponseCommitsShortSegment(t *testing.T) {
	file := nntpd.Pattern(0, 2*auditSeg)
	foreign := nntpd.Pattern(7, auditShort)
	bodies := map[string][]byte{
		"<s0@audit>":      nntpd.Encode(file[:auditSeg], "a.mkv", 1, 2*auditSeg, 0),
		"<s1@audit>":      nntpd.Encode(file[auditSeg:], "a.mkv", 2, 2*auditSeg, auditSeg),
		"<foreign@other>": nntpd.Encode(foreign, "other.bin", 9, 50*auditShort, 8*auditShort),
	}
	segs := []SegmentMeta{
		{MessageID: "<s0@audit>", Number: 1, Bytes: auditSeg, StartOffset: 0, EndOffset: auditSeg - 1},
		{MessageID: "<s1@audit>", Number: 2, Bytes: auditSeg, StartOffset: auditSeg, EndOffset: 2*auditSeg - 1},
	}
	for _, retention := range []Retention{RetentionRewind, RetentionDelivery} {
		for _, path := range []string{"demand", "prefetch-batch"} {
			t.Run(fmt.Sprintf("retention-%d/%s", retention, path), func(t *testing.T) {
				host, port := misroutingServer(t, bodies, map[string]string{"<s1@audit>": "<foreign@other>"})
				client, err := nntp.NewClient(&config.Config{Usenet: config.Usenet{Providers: []config.UsenetProvider{
					{Host: host, Port: port, Backbone: "mis", Priority: 1, MaxConnections: 2}}}})
				if err != nil {
					t.Fatal(err)
				}
				opts := []Option{WithRetention(retention), WithMaxConnections(2), WithPrefetchAhead(0), WithBodyPipelineDepth(2), WithDownloadTimeout(10 * time.Second)}
				if retention == RetentionRewind {
					opts = append(opts, WithDiskPath(t.TempDir()))
				}
				sr, err := NewStreamingReader(t.Context(), client, segs, opts...)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = sr.Close(); _ = client.Close() }()
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancel()
				var fetchErr error
				if path == "demand" {
					fetchErr = sr.fetcher.Fetch(ctx, 1)
				} else {
					fetchErr = sr.fetcher.fetchPrefetchBatch(ctx, []int{0, 1})
				}
				one := make([]byte, 1)
				n, readErr := sr.ReadAt(one, auditSeg+auditShort)
				t.Logf("fetch err=%v state=%s stored=%d; ReadAt(1 @ seg1+%d) = %d, %v", fetchErr, sr.cache.GetState(1), sr.cache.segLengths[1].Load(), auditShort, n, readErr)
				head := make([]byte, 64)
				hn, _ := sr.ReadAt(head, auditSeg)
				if sr.cache.segLengths[1].Load() == auditShort && n == 0 && readErr == nil {
					t.Errorf("misrouted %d-byte response published as segment 1 (content is foreign: %t); read at in-segment %d returned (0, nil)",
						auditShort, !bytes.Equal(head[:hn], file[auditSeg:auditSeg+hn]), auditShort)
				}
			})
		}
	}
}

// Forensics suspect (1): a destination write that fails part-way in the
// body pipeline. Uses a 6 MiB tmpfs at /small as the rewind disk so the
// second 3.84 MB segment hits ENOSPC mid-write.
func TestAuditPipelineWriteErrorAfterPartialWrite(t *testing.T) {
	if _, err := os.Stat("/small"); err != nil {
		t.Skip("needs a small tmpfs at /small")
	}
	server, err := nntpd.New(nntpd.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	file := nntpd.Pattern(0, 2*auditSeg)
	segs := []SegmentMeta{
		{MessageID: "<w0@audit>", Number: 1, Bytes: auditSeg, StartOffset: 0, EndOffset: auditSeg - 1},
		{MessageID: "<w1@audit>", Number: 2, Bytes: auditSeg, StartOffset: auditSeg, EndOffset: 2*auditSeg - 1},
	}
	server.AddArticle("<w0@audit>", nntpd.Encode(file[:auditSeg], "w.mkv", 1, 2*auditSeg, 0))
	server.AddArticle("<w1@audit>", nntpd.Encode(file[auditSeg:], "w.mkv", 2, 2*auditSeg, auditSeg))
	host, port := server.Addr()
	client, err := nntp.NewClient(&config.Config{Usenet: config.Usenet{Providers: []config.UsenetProvider{
		{Host: host, Port: port, Backbone: "w", Priority: 1, MaxConnections: 2}}}})
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := os.MkdirTemp("/small", "rw")
	sr, err := NewStreamingReader(t.Context(), client, segs, WithRetention(RetentionRewind), WithDiskPath(dir),
		WithMaxConnections(2), WithPrefetchAhead(0), WithBodyPipelineDepth(2), WithDownloadTimeout(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sr.Close(); _ = client.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	err = sr.fetcher.fetchPrefetchBatch(ctx, []int{0, 1})
	t.Logf("batch err=%v", err)
	for i := range segs {
		t.Logf("seg %d: state=%s stored=%d present=%v", i, sr.cache.GetState(i), sr.cache.segLengths[i].Load(), sr.cache.buf.Present(sr.cache.segOffsets[i], auditSeg))
	}
	if err == nil && sr.cache.GetState(1) != StateOnDisk {
		t.Errorf("write error swallowed: batch returned nil, segment 1 left in state %s (stored %d)", sr.cache.GetState(1), sr.cache.segLengths[1].Load())
	}
	if sr.cache.GetState(1) == StateOnDisk && sr.cache.segLengths[1].Load() < auditSeg {
		t.Errorf("partial write published: %d bytes", sr.cache.segLengths[1].Load())
	}
	rctx, rcancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer rcancel()
	one := make([]byte, 1)
	n, rerr := sr.ReadAtContext(rctx, one, auditSeg+10)
	t.Logf("ReadAt seg1 = %d, %v", n, rerr)
}
