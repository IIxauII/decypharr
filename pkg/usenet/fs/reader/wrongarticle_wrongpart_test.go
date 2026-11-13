package reader

// Audit test: a full-size, CRC-valid article that is the wrong yEnc part
// (provider serving another post under the requested message-id) must not
// be published; failover must store the backup's correct copy.

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
)

func TestAuditWrongPartFullSizeFailsOver(t *testing.T) {
	const size = 256 << 10
	file := nntpd.Pattern(0, 2*size)
	wrong := nntpd.Pattern(99, size)
	segs := []SegmentMeta{
		{MessageID: "<p1@audit>", Number: 1, Bytes: size, StartOffset: 0, EndOffset: size - 1},
		{MessageID: "<p2@audit>", Number: 2, Bytes: size, StartOffset: size, EndOffset: 2*size - 1},
	}
	for _, retention := range []Retention{RetentionRewind, RetentionDelivery} {
		for _, path := range []string{"demand", "prefetch-batch"} {
			t.Run(fmt.Sprintf("retention-%d/%s", retention, path), func(t *testing.T) {
				primary, _ := nntpd.New(nntpd.Config{})
				backup, _ := nntpd.New(nntpd.Config{})
				t.Cleanup(primary.Close)
				t.Cleanup(backup.Close)
				primary.AddArticle("<p1@audit>", nntpd.Encode(file[:size], "a.mkv", 1, 2*size, 0))
				primary.AddArticle("<p2@audit>", nntpd.Encode(wrong, "other.bin", 247, 400*size, 246*size))
				backup.AddArticle("<p1@audit>", nntpd.Encode(file[:size], "a.mkv", 1, 2*size, 0))
				backup.AddArticle("<p2@audit>", nntpd.Encode(file[size:], "a.mkv", 2, 2*size, size))
				ph, pp := primary.Addr()
				bh, bp := backup.Addr()
				client, err := nntp.NewClient(&config.Config{Usenet: config.Usenet{Providers: []config.UsenetProvider{
					{Host: ph, Port: pp, Backbone: "eweka", Priority: 1, MaxConnections: 2},
					{Host: bh, Port: bp, Backbone: "other", Priority: 2, Backup: true, MaxConnections: 2},
				}}})
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
				var ferr error
				if path == "demand" {
					ferr = sr.fetcher.Fetch(ctx, 1)
				} else {
					ferr = sr.fetcher.fetchPrefetchBatch(ctx, []int{0, 1})
				}
				got := make([]byte, size)
				n, rerr := sr.ReadAt(got, size)
				t.Logf("fetch err=%v; ReadAt seg1 = %d, %v", ferr, n, rerr)
				if bytes.Equal(got[:min(n, 64)], wrong[:min(n, 64)]) && n > 0 {
					t.Errorf("wrong-part article published as segment 1")
				} else if rerr != nil || n != size || !bytes.Equal(got, file[size:]) {
					t.Errorf("backup copy not served: n=%d err=%v exact=%t", n, rerr, bytes.Equal(got[:n], file[size:size+n]))
				}
			})
		}
	}
}
