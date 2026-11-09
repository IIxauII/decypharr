package config

import (
	"strings"
	"testing"

	json "github.com/bytedance/sonic"
)

func TestResolveDownloadAction(t *testing.T) {
	tests := []struct {
		name                           string
		requested, arrAction, fallback DownloadAction
		want                           DownloadAction
	}{
		{"request wins over arr and default", DownloadActionDownload, DownloadActionSymlink, DownloadActionSymlink, DownloadActionDownload},
		{"arr wins over default", "", DownloadActionDownload, DownloadActionSymlink, DownloadActionDownload},
		{"default when nothing else is set", "", "", DownloadActionSymlink, DownloadActionSymlink},
		{"none is a request choice", DownloadActionNone, DownloadActionDownload, DownloadActionSymlink, DownloadActionNone},
		{"none is an arr choice", "", DownloadActionNone, DownloadActionSymlink, DownloadActionNone},
		{"unknown request falls through to arr", "strm", DownloadActionDownload, DownloadActionSymlink, DownloadActionDownload},
		{"unknown request falls through to default", "bogus", "", DownloadActionSymlink, DownloadActionSymlink},
		{"unknown arr falls through to default", "", "strm", DownloadActionDownload, DownloadActionDownload},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveDownloadAction(tt.requested, tt.arrAction, tt.fallback); got != tt.want {
				t.Fatalf("ResolveDownloadAction(%q, %q, %q) = %q, want %q", tt.requested, tt.arrAction, tt.fallback, got, tt.want)
			}
		})
	}
}

func TestArrDownloadActionJSON(t *testing.T) {
	t.Run("decodes from config.json", func(t *testing.T) {
		var got Arr
		if err := json.Unmarshal([]byte(`{"name":"lidarr","download_action":"download"}`), &got); err != nil {
			t.Fatal(err)
		}
		if got.DownloadAction != DownloadActionDownload {
			t.Fatalf("download_action = %q, want %q", got.DownloadAction, DownloadActionDownload)
		}
	})
	t.Run("round trips", func(t *testing.T) {
		want := Arr{Name: "lidarr", Host: "http://lidarr:8686", Token: "key", DownloadAction: DownloadActionDownload}
		data, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		var got Arr
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("round trip = %#v, want %#v", got, want)
		}
	})
	t.Run("inherit is omitted", func(t *testing.T) {
		data, err := json.Marshal(Arr{Name: "sonarr"})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "download_action") {
			t.Fatalf("empty download_action was written: %s", data)
		}
	})
}
