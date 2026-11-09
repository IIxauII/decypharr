package arr

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
)

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

	actionOf := func(configured []config.Arr, name string) config.DownloadAction {
		t.Helper()
		for _, c := range configured {
			if c.Name == name {
				return c.DownloadAction
			}
		}
		t.Fatalf("%s missing from %#v", name, configured)
		return ""
	}

	t.Run("loaded from config", func(t *testing.T) {
		if got, _ := arrs.Get("lidarr"); got.DownloadAction != config.DownloadActionDownload {
			t.Fatalf("download_action = %q, want %q", got.DownloadAction, config.DownloadActionDownload)
		}
	})
	t.Run("synced from config", func(t *testing.T) {
		arrs.SyncFromConfig([]config.Arr{{Name: "lidarr", Host: "http://lidarr:8686", Token: "key", DownloadAction: config.DownloadActionNone}})
		if got, _ := arrs.Get("lidarr"); got.DownloadAction != config.DownloadActionNone {
			t.Fatalf("download_action = %q, want %q", got.DownloadAction, config.DownloadActionNone)
		}
	})
	t.Run("synced to config for a configured arr", func(t *testing.T) {
		if got := actionOf(arrs.SyncToConfig(), "lidarr"); got != config.DownloadActionNone {
			t.Fatalf("download_action = %q, want the registry's %q", got, config.DownloadActionNone)
		}
	})
	t.Run("synced to config for a registry-only arr", func(t *testing.T) {
		arrs.AddOrUpdate(Arr{Name: "radarr", Host: "http://radarr:7878", Token: "key", DownloadAction: config.DownloadActionSymlink})
		if got := actionOf(arrs.SyncToConfig(), "radarr"); got != config.DownloadActionSymlink {
			t.Fatalf("download_action = %q, want %q", got, config.DownloadActionSymlink)
		}
	})
}
