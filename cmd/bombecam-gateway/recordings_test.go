package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The recordings handlers guard with validateOperatorRequest, which lets a
// request through when no operator manager is installed (the test default),
// like the other /api/v1 handlers' tests in this package.

func TestRecordingsListAndDownload(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "front_porch")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(sub, "2026-11-13_10-00-00.mp4")
	if err := os.WriteFile(p, []byte("fake-mp4"), 0o644); err != nil {
		t.Fatal(err)
	}

	setRecordingsRoot(root)
	defer setRecordingsRoot("")

	t.Run("list", func(t *testing.T) {
		rr := httptest.NewRecorder()
		handleRecordings(rr, httptest.NewRequest(http.MethodGet, "/api/v1/recordings", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
		}
		var out struct {
			Enabled    bool            `json:"enabled"`
			Recordings []recordingFile `json:"recordings"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if !out.Enabled {
			t.Fatalf("expected enabled, got %s", rr.Body.String())
		}
		if len(out.Recordings) != 1 {
			t.Fatalf("expected 1 recording, got %+v", out.Recordings)
		}
		got := out.Recordings[0]
		if got.Stream != "front_porch" || got.Name != "2026-11-13_10-00-00.mp4" || got.Size != 8 {
			t.Fatalf("unexpected recording %+v", got)
		}
	})

	t.Run("download", func(t *testing.T) {
		rr := httptest.NewRecorder()
		u := "/api/v1/recordings/download?path=front_porch/2026-11-13_10-00-00.mp4"
		handleRecordingDownload(rr, httptest.NewRequest(http.MethodGet, u, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
		}
		if rr.Body.String() != "fake-mp4" {
			t.Fatalf("download body=%q", rr.Body.String())
		}
	})

	t.Run("download refuses path escape", func(t *testing.T) {
		rr := httptest.NewRecorder()
		u := "/api/v1/recordings/download?path=../secret.txt"
		handleRecordingDownload(rr, httptest.NewRequest(http.MethodGet, u, nil))
		if rr.Code == http.StatusOK {
			t.Fatalf("path escape must be refused, got %d body=%s", rr.Code, rr.Body.String())
		}
	})
}

func TestRecordingsDisabled(t *testing.T) {
	setRecordingsRoot("")
	rr := httptest.NewRecorder()
	handleRecordings(rr, httptest.NewRequest(http.MethodGet, "/api/v1/recordings", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d", rr.Code)
	}
	var out struct {
		Enabled bool `json:"enabled"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.Enabled {
		t.Fatal("recording must be off by default")
	}
}