package qbit

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	json "github.com/bytedance/sonic"
	"github.com/rs/zerolog"
	"github.com/sirrobot01/decypharr/internal/config"
	"github.com/sirrobot01/decypharr/internal/customerror"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/debrid/common"
	debridTypes "github.com/sirrobot01/decypharr/pkg/debrid/types"
	"github.com/sirrobot01/decypharr/pkg/manager"
)

func TestHandleLoginAlwaysReturnsSID(t *testing.T) {
	config.Reset()
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)
	config.Get().UseAuth = false

	mgr := manager.New()
	t.Cleanup(func() {
		if err := mgr.Stop(); err != nil {
			t.Error(err)
		}
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v2/auth/login", strings.NewReader("username=homarr-user&password=homarr-password"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	(&QBit{manager: mgr}).handleLogin(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("login status = %d, want %d", recorder.Code, http.StatusOK)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "SID" || cookies[0].Value == "" {
		t.Fatalf("login cookies = %#v, want one SID cookie", cookies)
	}
	username, password, err := extractFromSID(cookies[0].Value)
	if err != nil {
		t.Fatalf("decode SID: %v", err)
	}
	if username != "homarr-user" || password != "homarr-password" {
		t.Fatalf("SID credentials = %q/%q", username, password)
	}
}

func TestWriteTorrentAddErrorPreservesSemantics(t *testing.T) {
	tests := map[string]struct {
		err       error
		status    int
		code      string
		retryable bool
		permanent bool
	}{
		"blocked": {
			err:       fmt.Errorf("submit: %w", customerror.TorrentBlockedError),
			status:    http.StatusUnavailableForLegalReasons,
			code:      "torrent_blocked",
			permanent: true,
		},
		"not cached": {
			err:       fmt.Errorf("status: %w", customerror.TorrentNotCachedError),
			status:    http.StatusNotFound,
			code:      "torrent_not_cached",
			retryable: true,
		},
		"invalid input": {
			err:       customerror.NewError(errors.New("bad magnet"), http.StatusBadRequest, "invalid_magnet", false, false).Permanent(),
			status:    http.StatusBadRequest,
			code:      "invalid_magnet",
			permanent: true,
		},
		"provider failure": {
			err:       errors.New("connection reset"),
			status:    http.StatusBadGateway,
			code:      "provider_error",
			retryable: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeTorrentAddError(recorder, tt.err)

			if recorder.Code != tt.status {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.status)
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("content type = %q, want application/json", got)
			}

			var response torrentAddErrorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if response.Code != tt.code || response.Retryable != tt.retryable || response.Permanent != tt.permanent {
				t.Fatalf("response = %#v, want code=%q retryable=%t permanent=%t", response, tt.code, tt.retryable, tt.permanent)
			}
		})
	}
}

// slotlessDebrid rejects every submit, so the manager queues the torrent,
// action included, and calls nothing else.
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
	q.manager.Arr().AddOrUpdate(arr.Arr{Name: "whisparr", Host: "http://127.0.0.1:1", Token: "key", DownloadAction: config.DownloadActionSymlink})
	q.manager.Clients().Store("slotless", slotlessDebrid{})
	routes := q.Routes()

	for i, tc := range []struct {
		name, category, sequential string
		want                       config.DownloadAction
	}{
		{"arr setting", "lidarr", "", config.DownloadActionDownload},
		{"registered arr setting", "whisparr", "", config.DownloadActionSymlink},
		{"sequential download wins over arr", "sonarr", "true", config.DownloadActionDownload},
		{"sequential false keeps arr", "sonarr", "false", config.DownloadActionSymlink},
		{"default for a category with no Arr", "radarr", "", config.DownloadActionNone},
		{"unknown arr value falls through to default", "readarr", "", config.DownloadActionNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hash := fmt.Sprintf("%040x", i+1)
			form := url.Values{"urls": {"magnet:?xt=urn:btih:" + hash + "&dn=Release"}, "category": {tc.category}, "sequentialDownload": {tc.sequential}}
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
