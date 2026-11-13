package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
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

// Retention: segments older than the window are deleted (and their empty
// stream directory is removed), younger ones survive, and retention 0 changes
// nothing.
func TestRecordingsRetentionCleanup(t *testing.T) {
	root := t.TempDir()
	old := filepath.Join(root, "old_cam", "2026-01-01_00-00-00.mp4")
	newer := filepath.Join(root, "new_cam", "2026-11-13_10-00-00.mp4")
	for _, p := range []string{old, newer} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Make the first segment look old.
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	// Guard: without retention, nothing is deleted.
	setRecordingsRoot(root)
	setRecordingsRetention(0)
	defer func() {
		setRecordingsRoot("")
		setRecordingsRetention(0)
	}()
	cleanRecordings()
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("retention 0 must keep everything: %v", err)
	}

	// With retention 1 day, the old segment goes away and its empty directory
	// is removed too; the fresh one stays.
	setRecordingsRetention(1)
	cleanRecordings()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old segment must be deleted, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Dir(old)); !os.IsNotExist(err) {
		t.Errorf("empty stream directory must be removed, stat err=%v", err)
	}
	if _, err := os.Stat(newer); err != nil {
		t.Errorf("recent segment must be kept: %v", err)
	}
}