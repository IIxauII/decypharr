package sabnzbd

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/pkg/manager"
)

// newDownloadActionTestSABnzbd configures Usenet, so an added NZB is queued
// with its action before any network work, and a lidarr entry that is in the
// config but not in the Arr registry.
func newDownloadActionTestSABnzbd(t *testing.T) (*SABnzbd, *manager.Manager) {
	t.Helper()
	config.Reset()
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)
	if _, err := config.Update(func(c *config.Config) error {
		c.UseAuth = false
		c.DownloadFolder = t.TempDir()
		c.DefaultDownloadAction = config.DownloadActionSymlink
		c.Usenet = config.Usenet{
			Providers:                []config.UsenetProvider{{Host: "127.0.0.1", Port: 1, MaxConnections: 1}},
			MaxConnections:           1,
			ProcessingMaxConnections: 1,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mgr := manager.New()
	t.Cleanup(func() {
		if err := mgr.Stop(); err != nil {
			t.Error(err)
		}
	})
	if _, err := config.Update(func(c *config.Config) error {
		c.Arrs = []config.Arr{{Name: "lidarr", DownloadAction: config.DownloadActionDownload}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return New(mgr), mgr
}

func TestAuthenticateInheritsConfiguredDownloadAction(t *testing.T) {
	sab, _ := newDownloadActionTestSABnzbd(t)

	got, err := sab.authenticate(t.Context(), "lidarr", "", "")
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.DownloadAction != config.DownloadActionDownload {
		t.Fatalf("download_action = %q, want the configured %q", got.DownloadAction, config.DownloadActionDownload)
	}
}

func TestAddFileResolvesDownloadAction(t *testing.T) {
	sab, mgr := newDownloadActionTestSABnzbd(t)
	router := sab.Routes()

	for _, tc := range []struct {
		name, category, action string
		want                   config.DownloadAction
	}{
		{"arr setting", "lidarr", "", config.DownloadActionDownload},
		{"request wins over arr", "lidarr", "none", config.DownloadActionNone},
		{"unknown request falls through to arr", "lidarr", "strm", config.DownloadActionDownload},
		{"default for an arr without one", "sonarr", "", config.DownloadActionSymlink},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body bytes.Buffer
			form := multipart.NewWriter(&body)
			fields := map[string]string{"mode": "addfile", "cat": tc.category}
			if tc.action != "" {
				fields["action"] = tc.action
			}
			for key, value := range fields {
				if err := form.WriteField(key, value); err != nil {
					t.Fatal(err)
				}
			}
			file, err := form.CreateFormFile("name", tc.name+".nzb")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write([]byte("queued before parsing")); err != nil {
				t.Fatal(err)
			}
			if err := form.Close(); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/", &body)
			req.Header.Set("Content-Type", form.FormDataContentType())
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}

			var added AddNZBResponse
			if err := json.Unmarshal(response.Body.Bytes(), &added); err != nil {
				t.Fatal(err)
			}
			if len(added.NzoIds) != 1 {
				t.Fatalf("response = %#v", added)
			}
			entry, err := mgr.Queue().GetTorrent(added.NzoIds[0])
			if err != nil {
				t.Fatal(err)
			}
			if entry.Action != tc.want {
				t.Fatalf("action = %q, want %q", entry.Action, tc.want)
			}
		})
	}
}
