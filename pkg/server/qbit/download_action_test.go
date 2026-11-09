package qbit

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/debrid/common"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
)

// slotlessDebrid answers every submit with "too many active downloads", so
// the manager queues the torrent for a retry, action included, without
// calling the provider for anything else.
type slotlessDebrid struct{ common.Client }

func (slotlessDebrid) SubmitMagnet(*debridTypes.Torrent) (*debridTypes.Torrent, error) {
	return nil, customerror.TooManyActiveDownloadsError
}

func (slotlessDebrid) Config() config.Debrid {
	return config.Debrid{Name: "slotless", Provider: "slotless"}
}

func (slotlessDebrid) Logger() zerolog.Logger { return zerolog.Nop() }

func TestTorrentsAddResolvesDownloadAction(t *testing.T) {
	q := newAuthenticationTestQBit(t)
	if _, err := config.Update(func(c *config.Config) error {
		c.DefaultDownloadAction = config.DownloadActionNone
		c.Arrs = []config.Arr{
			{Name: "lidarr", DownloadAction: config.DownloadActionDownload},
			{Name: "sonarr", DownloadAction: config.DownloadActionSymlink},
			{Name: "readarr", DownloadAction: "strm"},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	q.manager.Clients().Store("slotless", slotlessDebrid{})
	routes := q.Routes()

	for i, tc := range []struct {
		name, category, sequential string
		want                       config.DownloadAction
	}{
		{"arr setting", "lidarr", "", config.DownloadActionDownload},
		{"sequential download wins over arr", "sonarr", "true", config.DownloadActionDownload},
		{"sequential false keeps arr", "sonarr", "false", config.DownloadActionSymlink},
		{"default for an arr without one", "radarr", "", config.DownloadActionNone},
		{"unknown arr value falls through to default", "readarr", "", config.DownloadActionNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hash := fmt.Sprintf("%040x", i+1)
			form := url.Values{"urls": {"magnet:?xt=urn:btih:" + hash + "&dn=Release"}, "category": {tc.category}}
			if tc.sequential != "" {
				form.Set("sequentialDownload", tc.sequential)
			}
			req := httptest.NewRequest(http.MethodPost, "/torrents/add", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response := httptest.NewRecorder()
			routes.ServeHTTP(response, req)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}

			entry, err := q.manager.Queue().GetTorrent(hash)
			if err != nil {
				t.Fatal(err)
			}
			if entry.Action != tc.want {
				t.Fatalf("action = %q, want %q", entry.Action, tc.want)
			}
		})
	}
}

func TestAuthenticateInheritsConfiguredDownloadAction(t *testing.T) {
	q := newAuthenticationTestQBit(t)
	if _, err := config.Update(func(c *config.Config) error {
		c.Arrs = []config.Arr{{Name: "lidarr", DownloadAction: config.DownloadActionDownload}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got, err := q.authenticate(t.Context(), "lidarr", "", "")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.DownloadAction != config.DownloadActionDownload {
		t.Fatalf("download_action = %q, want the configured %q", got.DownloadAction, config.DownloadActionDownload)
	}
}
