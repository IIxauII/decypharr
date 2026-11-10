package arr

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
)

func TestSyncFromConfigAppliesValidHost(t *testing.T) {
	config.Reset()
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)

	arrs := New()
	arrs.AddOrUpdate(Arr{Name: "whisparr", Host: "http://old.example", Token: "old-token", Source: SourceAuto})
	arrs.SyncFromConfig([]config.Arr{{
		Name:   "whisparr",
		Host:   "http://new.example",
		Token:  "new-token",
		Source: string(SourceAuto),
	}})

	got, ok := arrs.Get("whisparr")
	if !ok || got.Host != "http://new.example" || got.Token != "new-token" {
		t.Fatalf("synced Arr = %#v", got)
	}
}

func TestSyncFromConfigPreservesResolvedHostForInvalidUpdate(t *testing.T) {
	config.Reset()
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)

	arrs := New()
	arrs.AddOrUpdate(Arr{Name: "whisparr", Host: "http://resolved.example", Token: "old-token", Source: SourceAuto})
	arrs.SyncFromConfig([]config.Arr{{
		Name:   "whisparr",
		Host:   "not-a-url",
		Token:  "new-token",
		Source: string(SourceAuto),
	}})

	got, ok := arrs.Get("whisparr")
	if !ok || got.Host != "http://resolved.example" || got.Token != "new-token" {
		t.Fatalf("synced Arr = %#v", got)
	}
}

func TestArrInstanceFingerprintCanonicalizesHost(t *testing.T) {
	first := Arr{Type: Sonarr, Host: "HTTP://Example.COM:80/sonarr/", Token: "first"}.Fingerprint()
	second := Arr{Type: Sonarr, Host: "http://example.com/sonarr", Token: "second"}.Fingerprint()
	if first == "" || first != second {
		t.Fatalf("equivalent Arr hosts produced fingerprints %q and %q", first, second)
	}

	differentPath := Arr{Type: Sonarr, Host: "http://example.com/other"}.Fingerprint()
	differentType := Arr{Type: Radarr, Host: "http://example.com/sonarr"}.Fingerprint()
	if first == differentPath || first == differentType {
		t.Fatal("different Arr instances produced the same fingerprint")
	}
}

func TestInferTypeIgnoresCase(t *testing.T) {
	tests := []struct {
		host, name string
		want       Type
	}{
		{"http://localhost:7878", "Radarr", Radarr},
		{"http://localhost:8989", "Sonarr", Sonarr},
		{"http://RADARR:7878", "movies", Radarr},
		{"http://localhost:8686", "LiDaRr", Lidarr},
		{"http://localhost:9000", "Movies", Others},
	}
	for _, tt := range tests {
		if got := inferType(tt.host, tt.name); got != tt.want {
			t.Errorf("inferType(%q, %q) = %v, want %v", tt.host, tt.name, got, tt.want)
		}
	}
}

func TestSyncFromConfigWarnsOnlyForUnknownDownloadAction(t *testing.T) {
	config.Reset()
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)
	arrs := New()
	var logs bytes.Buffer
	arrs.logger = zerolog.New(&logs)

	arrs.SyncFromConfig([]config.Arr{
		{Name: "sonarr", Host: "http://sonarr:8989", Token: "key", DownloadAction: "strm"},
		{Name: "radarr", Host: "http://radarr:7878", Token: "key", DownloadAction: config.DownloadActionSymlink},
		{Name: "lidarr", Host: "http://lidarr:8686", Token: "key"},
	})

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("want one warning, got %d lines:\n%s", len(lines), logs.String())
	}
	for _, want := range []string{`"level":"warn"`, `"arr":"sonarr"`, `"download_action":"strm"`} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("warning %s lacks %s", lines[0], want)
		}
	}
}

func TestDownloadActionSurvivesConfigSync(t *testing.T) {
	config.Reset()
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)
	if _, err := config.Update(func(c *config.Config) error {
		c.Arrs = []config.Arr{{Name: "lidarr", Host: "http://lidarr:8686", Token: "key", DownloadAction: config.DownloadActionDownload}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	arrs := New()
	if got, _ := arrs.Get("lidarr"); got.DownloadAction != config.DownloadActionDownload {
		t.Fatalf("loaded download_action = %q, want %q", got.DownloadAction, config.DownloadActionDownload)
	}

	arrs.SyncFromConfig([]config.Arr{{Name: "lidarr", Host: "http://lidarr:8686", Token: "key", DownloadAction: config.DownloadActionNone}})
	arrs.AddOrUpdate(Arr{Name: "radarr", Host: "http://radarr:7878", Token: "key", DownloadAction: config.DownloadActionSymlink})
	got := map[string]config.DownloadAction{}
	for _, c := range arrs.SyncToConfig() {
		got[c.Name] = c.DownloadAction
	}
	if got["lidarr"] != config.DownloadActionNone || got["radarr"] != config.DownloadActionSymlink {
		t.Fatalf("synced download_action = %v, want lidarr none, radarr symlink", got)
	}
}
