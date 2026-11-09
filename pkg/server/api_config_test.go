package server

import (
	"encoding/json"
	"github.com/sirrobot01/decypharr/pkg/arr"
	"github.com/sirrobot01/decypharr/pkg/manager"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sirrobot01/decypharr/internal/config"
)

func TestMergeConfigUpdatePreservesOmittedFields(t *testing.T) {
	current := config.Config{
		Port:     "9000",
		LogLevel: "info",
		Debrids: []config.Debrid{{
			Name:   "realdebrid",
			APIKey: "secret",
		}},
		Mount: config.Mount{
			Type:      config.MountTypeDFS,
			MountPath: "/mnt/decypharr",
		},
		Notifications: config.Notifications{
			Enabled:    true,
			WebhookURL: "https://example.com/webhook",
		},
	}

	merged, err := mergeConfigUpdate(&current, strings.NewReader(`{"log_level":"debug"}`))
	if err != nil {
		t.Fatalf("merge config update: %v", err)
	}

	if merged.LogLevel != "debug" {
		t.Fatalf("expected updated log level, got %q", merged.LogLevel)
	}
	if merged.Port != current.Port {
		t.Fatalf("expected port %q to be preserved, got %q", current.Port, merged.Port)
	}
	if !reflect.DeepEqual(merged.Debrids, current.Debrids) {
		t.Fatalf("expected debrid config to be preserved, got %#v", merged.Debrids)
	}
	if !reflect.DeepEqual(merged.Mount, current.Mount) {
		t.Fatalf("expected mount config to be preserved, got %#v", merged.Mount)
	}
	if !reflect.DeepEqual(merged.Notifications, current.Notifications) {
		t.Fatalf("expected notification config to be preserved, got %#v", merged.Notifications)
	}
}

func TestMergeConfigUpdateMergesNestedObjects(t *testing.T) {
	current := config.Config{
		Mount: config.Mount{
			Type:      config.MountTypeRclone,
			MountPath: "/mnt/decypharr",
		},
	}

	merged, err := mergeConfigUpdate(&current, strings.NewReader(`{"mount":{"type":"dfs"}}`))
	if err != nil {
		t.Fatalf("merge config update: %v", err)
	}

	if merged.Mount.Type != config.MountTypeDFS {
		t.Fatalf("expected mount type %q, got %q", config.MountTypeDFS, merged.Mount.Type)
	}
	if merged.Mount.MountPath != current.Mount.MountPath {
		t.Fatalf("expected mount path %q to be preserved, got %q", current.Mount.MountPath, merged.Mount.MountPath)
	}
}

func TestMergeConfigUpdateAllowsExplicitClear(t *testing.T) {
	current := config.Config{Debrids: []config.Debrid{{Name: "realdebrid", APIKey: "secret"}}}

	merged, err := mergeConfigUpdate(&current, strings.NewReader(`{"debrids":[]}`))
	if err != nil {
		t.Fatalf("merge config update: %v", err)
	}

	if len(merged.Debrids) != 0 {
		t.Fatalf("expected debrid config to be cleared, got %#v", merged.Debrids)
	}
}

func TestMergeConfigUpdateKeepsDownloadActionPerArr(t *testing.T) {
	current := config.Config{Arrs: []config.Arr{
		{Name: "lidarr", Host: "http://lidarr:8686", Token: "lidarr-key", DownloadAction: config.DownloadActionDownload},
		{Name: "sonarr", Host: "http://sonarr:8989", Token: "sonarr-key"},
	}}
	// GET lists arrs in map order, so the UI can post them in a different
	// order than stored. It always sends download_action, "" for inherit.
	update := `{"arrs":[
		{"name":"sonarr","host":"http://sonarr:8989","token":"sonarr-key","download_action":""},
		{"name":"lidarr","host":"http://lidarr:8686","token":"lidarr-key","download_action":"download"}
	]}`

	merged, err := mergeConfigUpdate(&current, strings.NewReader(update))
	if err != nil {
		t.Fatalf("merge config update: %v", err)
	}

	want := map[string]config.DownloadAction{"sonarr": "", "lidarr": config.DownloadActionDownload}
	if len(merged.Arrs) != len(want) {
		t.Fatalf("arrs = %#v", merged.Arrs)
	}
	for _, a := range merged.Arrs {
		if a.DownloadAction != want[a.Name] {
			t.Errorf("%s download_action = %q, want %q", a.Name, a.DownloadAction, want[a.Name])
		}
	}
}

func TestConfigHandlersUseSnapshots(t *testing.T) {
	config.Reset()
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)
	before := config.Get()
	mgr := manager.New()
	t.Cleanup(func() { _ = mgr.Stop() })
	mgr.Arr().AddOrUpdate(arr.Arr{Name: "manual", Host: "http://example.test", Token: "token", Source: arr.SourceManual})
	server := &Server{manager: mgr}
	response := httptest.NewRecorder()
	server.handleGetConfig(response, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET status=%d", response.Code)
	}
	if len(before.Arrs) != 0 {
		t.Fatal("GET changed the current snapshot")
	}
	response = httptest.NewRecorder()
	server.handleUpdateConfig(response, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(`{"app_url":"https://new.example.test"}`)))
	if response.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", response.Code, response.Body.String())
	}
	if before.AppURL == "https://new.example.test" {
		t.Fatal("POST changed the previous snapshot")
	}
	if config.Get().AppURL != "https://new.example.test" {
		t.Fatal("POST did not publish the update")
	}
	var result struct {
		Restarted bool `json:"restarted"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Restarted {
		t.Fatal("live URL update restarted services")
	}
}

func TestUpdateConfigRejectsInvalidHearsayBlocklist(t *testing.T) {
	config.Reset()
	config.SetConfigPath(t.TempDir())
	t.Cleanup(config.Reset)
	mgr := manager.New()
	t.Cleanup(func() { _ = mgr.Stop() })
	server := &Server{manager: mgr}

	response := httptest.NewRecorder()
	server.handleUpdateConfig(response, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(`{"hearsay":{"blocklist":["10.0.0.1"]}}`)))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("POST status=%d body=%s, want 400", response.Code, response.Body.String())
	}
	if len(config.Get().Hearsay.Blocklist) != 0 {
		t.Fatal("invalid blocklist was saved")
	}
}
