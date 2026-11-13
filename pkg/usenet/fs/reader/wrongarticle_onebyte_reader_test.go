package reader

// Audit tests: how a segment table that promises one byte more than the
// article delivers turns into a silent hole in the stream.

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
)

func newAuditReader(t *testing.T, retention Retention, server *nntpd.Server, segments []SegmentMeta) *StreamingReader {
	t.Helper()
	host, port := server.Addr()
	client, err := nntp.NewClient(&config.Config{Usenet: config.Usenet{Providers: []config.UsenetProvider{
		{Host: host, Port: port, Backbone: "audit", Priority: 1, MaxConnections: 2},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	opts := []Option{WithRetention(retention), WithMaxConnections(2), WithPrefetchAhead(0),
		WithBodyPipelineDepth(2), WithDownloadTimeout(5 * time.Second)}
	if retention == RetentionRewind {
		opts = append(opts, WithDiskPath(t.TempDir()))
	}
	sr, err := NewStreamingReader(t.Context(), client, segments, opts...)
	if err != nil {
		_ = client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = errors.Join(sr.Close(), client.Close()) })
	return sr
}

// Segment 0 is declared N bytes; the provider's article is a valid,
// CRC-correct yEnc part of N-1 bytes. Segment 1 is a correct N-byte part.
func TestAuditShortArticleBecomesSilentHole(t *testing.T) {
	const n = 64 << 10
	for _, retention := range []Retention{RetentionDelivery, RetentionRewind} {
		t.Run(fmt.Sprint(retention), func(t *testing.T) {
			server, err := nntpd.New(nntpd.Config{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(server.Close)
			file := nntpd.Pattern(0, 2*n)
			segments := []SegmentMeta{
				{MessageID: "<s0@audit>", Number: 1, Bytes: n, StartOffset: 0, EndOffset: n - 1},
				{MessageID: "<s1@audit>", Number: 2, Bytes: n, StartOffset: n, EndOffset: 2*n - 1},
			}
			server.AddArticle("<s0@audit>", nntpd.Encode(file[:n-1], "a.bin", 1, 2*n, 0))
			server.AddArticle("<s1@audit>", nntpd.Encode(file[n:], "a.bin", 2, 2*n, n))
			sr := newAuditReader(t, retention, server, segments)

			// 1. Fetch the short article (today: accepted and published).
			fetchErr := sr.fetcher.Fetch(t.Context(), 0)
			stored := sr.cache.SegmentDataSize(0)
			declared := sr.cache.SegmentOffset(1) - sr.cache.SegmentOffset(0)
			t.Logf("fetch err=%v state=%s declared=%d stored=%d", fetchErr, sr.cache.GetState(0), declared, stored)

			// 2. A read that starts on the missing byte.
			one := make([]byte, 1)
			got, err := sr.ReadAt(one, n-1)
			t.Logf("ReadAt(1 byte @ %d) = %d, %v", n-1, got, err)

			// 3. A read spanning the missing byte.
			span := bytes.Repeat([]byte{0xEE}, 32)
			got2, err2 := sr.ReadAt(span, n-16)
			t.Logf("ReadAt(32 @ %d) = %d, %v; byte@hole=%#x want %#x", n-16, got2, err2, span[15], file[n-1])

			var failures []string
			if fetchErr == nil && stored < declared {
				failures = append(failures, fmt.Sprintf("short article published as %d of %d declared bytes without error", stored, declared))
			}
			if err == nil {
				failures = append(failures, fmt.Sprintf("read at the missing byte returned (%d, nil)", got))
			}
			if err2 == nil && (got2 != len(span) || !bytes.Equal(span, file[n-16:n+16])) {
				failures = append(failures, fmt.Sprintf("read across the hole returned (%d, nil) with corrupt content", got2))
			}
			if err2 != nil && !bytes.Equal(span[:got2], file[n-16:n-16+got2]) {
				failures = append(failures, "error read returned corrupt prefix")
			}
			if len(failures) == 0 {
				return
			}
			t.Errorf("1-byte hole served silently: %v", failures)
		})
	}
}

// A table whose next segment starts two bytes after the previous inclusive
// end is accepted by computeOffsets, and the gap byte reads as (0, nil)
// even though no article is short.
func TestAuditOneByteTableGapIsAccepted(t *testing.T) {
	const n = 64 << 10
	server, err := nntpd.New(nntpd.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	file := nntpd.Pattern(0, 2*n)
	segments := []SegmentMeta{
		{MessageID: "<g0@audit>", Number: 1, Bytes: n, StartOffset: 0, EndOffset: n - 1},
		{MessageID: "<g1@audit>", Number: 2, Bytes: n, StartOffset: n + 1, EndOffset: 2 * n},
	}
	server.AddArticle("<g0@audit>", nntpd.Encode(file[:n], "g.bin", 1, 2*n, 0))
	server.AddArticle("<g1@audit>", nntpd.Encode(file[n:], "g.bin", 2, 2*n, n))
	offs := computeOffsets(segments)
	sr := newAuditReader(t, RetentionDelivery, server, segments)
	one := make([]byte, 1)
	got, err := sr.ReadAt(one, n)
	t.Logf("offsets=%v ReadAt(1 @ %d) = %d, %v", offs, n, got, err)
	if offs[1] == n+1 && got == 0 && err == nil {
		t.Errorf("1-byte gap in the table accepted; read at the gap returned (0, nil)")
	}
}
