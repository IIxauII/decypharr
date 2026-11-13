package reader

// Audit test: the live Pokemon S21E40 reproducer. Eweka answers BODY for the
// NZB's idx0 message-id with a different post's part 247 (398336 bytes,
// CRC-valid). The segment expects SegmentDataStart 90 + 767910 bytes.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/nntp"
	"github.com/sirrobot01/decypharr/internal/testutil/nntpd"
)

func TestAuditPokemonForeignArticle(t *testing.T) {
	raw, err := os.ReadFile("testdata/wrongarticle/idx0.raw")
	if err != nil {
		t.Skip(err)
	}
	foreign := bytes.TrimSuffix(raw, []byte(".\r\n"))
	const id = "<C0v3or0MtVLkAKpRhsxrJNQez6@UATE6Zjr.ctY>"
	const article, dataStart, bytesWanted = 768000, 90, 767910
	right := nntpd.Pattern(0, article)
	seg := []SegmentMeta{{MessageID: id, Number: 1, Bytes: bytesWanted, StartOffset: 0, EndOffset: bytesWanted - 1, SegmentDataStart: dataStart}}
	for _, withBackup := range []bool{false, true} {
		t.Run(fmt.Sprintf("backup=%t", withBackup), func(t *testing.T) {
			primary, err := nntpd.New(nntpd.Config{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(primary.Close)
			primary.AddArticle(id, foreign)
			ph, pp := primary.Addr()
			providers := []config.UsenetProvider{{Host: ph, Port: pp, Backbone: "eweka", Priority: 1, MaxConnections: 2}}
			if withBackup {
				backup, err := nntpd.New(nntpd.Config{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(backup.Close)
				backup.AddArticle(id, nntpd.Encode(right, "5b8156ca.part01.rar", 1, 100*article, 0))
				bh, bp := backup.Addr()
				providers = append(providers, config.UsenetProvider{Host: bh, Port: bp, Backbone: "other", Priority: 2, Backup: true, MaxConnections: 2})
			}
			client, err := nntp.NewClient(&config.Config{Usenet: config.Usenet{Providers: providers}})
			if err != nil {
				t.Fatal(err)
			}
			sr, err := NewStreamingReader(t.Context(), client, seg, WithRetention(RetentionRewind), WithDiskPath(t.TempDir()),
				WithMaxConnections(2), WithPrefetchAhead(0), WithDownloadTimeout(10*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sr.Close(); _ = client.Close() }()
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			var problems []string
			for _, off := range []int64{398245, 398246, 398247, 500000} {
				one := []byte{0xEE}
				n, err := sr.ReadAtContext(ctx, one, off)
				t.Logf("ReadAt(1 @ %d) = %d, %v (state=%s stored=%d)", off, n, err, sr.cache.GetState(0), sr.cache.segLengths[0].Load())
				switch {
				case n == 0 && err == nil:
					problems = append(problems, fmt.Sprintf("@%d: (0, nil)", off))
				case err == nil && one[0] != right[dataStart+off] && withBackup:
					problems = append(problems, fmt.Sprintf("@%d: foreign byte served", off))
				case err != nil && withBackup:
					problems = append(problems, fmt.Sprintf("@%d: error although the backup has the right article: %v", off, err))
				case err != nil && strings.Contains(err.Error(), "still missing after re-fetch"):
					problems = append(problems, fmt.Sprintf("@%d: re-fetch committed the foreign article again", off))
				}
			}
			if sr.cache.GetState(0) == StateOnDisk && sr.cache.segLengths[0].Load() != bytesWanted {
				problems = append(problems, fmt.Sprintf("published %d of %d bytes", sr.cache.segLengths[0].Load(), bytesWanted))
			}
			if len(problems) > 0 {
				t.Errorf("foreign 398336-byte article accepted for %s: %v", id, problems)
			}
		})
	}
}
