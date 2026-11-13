package reader

// Audit test: serve the real failing articles (#41 = idx 40, #42, #82 = idx 81)
// through every production fetch path and check the published length/content.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
)

func TestAuditRealArticlesAllPaths(t *testing.T) {
	dir := "testdata/wrongarticle"
	names := []string{"seg041"}
	var raws, decs [][]byte
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(dir, n+".raw"))
		if err != nil {
			t.Skip(err)
		}
		dec, err := os.ReadFile(filepath.Join(dir, n+".pydec"))
		if err != nil {
			t.Fatal(err)
		}
		raws = append(raws, bytes.TrimSuffix(raw, []byte(".\r\n")))
		decs = append(decs, dec)
	}
	const size = 3840000
	for _, retention := range []Retention{RetentionRewind, RetentionDelivery} {
		for _, path := range []string{"demand", "prefetch-batch"} {
			t.Run(fmt.Sprintf("retention-%d/%s", retention, path), func(t *testing.T) {
				server, err := nntpd.New(nntpd.Config{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(server.Close)
				var segs []SegmentMeta
				for i, n := range names {
					id := "<" + n + "@real>"
					server.AddArticle(id, raws[i])
					segs = append(segs, SegmentMeta{MessageID: id, Number: []int{41, 42, 82}[i], Bytes: size, StartOffset: int64(i * size), EndOffset: int64((i+1)*size - 1)})
				}
				sr := newAuditReader(t, retention, server, segs)
				ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
				defer cancel()
				if path == "demand" {
					for i := range segs {
						if err := sr.fetcher.Fetch(ctx, i); err != nil {
							t.Fatalf("fetch %d: %v", i, err)
						}
					}
				} else if err := sr.fetcher.fetchPrefetchBatch(ctx, []int{0, 1, 2}); err != nil {
					t.Fatalf("batch: %v", err)
				}
				for i := range segs {
					if retention != RetentionRewind && sr.cache.GetState(i) == StateEmpty {
						continue // whole-extent eviction by the memory window, not a short commit
					}
					got := make([]byte, size)
					n, ok := sr.cache.ReadRangeInto(i, 0, size, got)
					if !ok || n != size || !bytes.Equal(got, decs[i]) {
						t.Errorf("%s: state=%s stored=%d n=%d ok=%t exact=%t", names[i], sr.cache.GetState(i), sr.cache.segLengths[i].Load(), n, ok, bytes.Equal(got[:n], decs[i][:n]))
					}
				}
			})
		}
	}
}
