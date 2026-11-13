package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Recording is local footage of a published stream, written by MediaMTX into
// the directory chosen on the command line (-recordings-dir). One subdirectory
// per stream path holds that stream's segments. This file serves the list of
// recorded segments and lets the web page download them.
//
// There is no cloud or SD-card access here: recording captures what BombeCam
// already streams, which is what the rest of this project can do with the
// vendor cameras.

var (
	recordingsMu   sync.Mutex
	recordingsRoot string // "" means recording is off (the default)
)

// setRecordingsRoot installs the recording directory chosen at startup.
func setRecordingsRoot(dir string) {
	recordingsMu.Lock()
	defer recordingsMu.Unlock()
	recordingsRoot = dir
}

// recordingsDisabled reports whether recording was turned on.
func recordingsDisabled() bool {
	recordingsMu.Lock()
	defer recordingsMu.Unlock()
	return recordingsRoot == ""
}

// retention days: a recorded segment whose file is older than this many days
// is deleted by the periodic cleanup. 0 (the default) keeps everything.
var (
	retentionMu   sync.Mutex
	retentionDays int
)

// setRecordingsRetention installs the retention window in days. Call it once
// at startup, before the cleanup loop starts.
func setRecordingsRetention(days int) {
	retentionMu.Lock()
	defer retentionMu.Unlock()
	retentionDays = days
	if retentionDays < 0 {
		retentionDays = 0
	}
}

// recordingsRetention returns the current retention in days (0 = keep all).
func recordingsRetention() int {
	retentionMu.Lock()
	defer retentionMu.Unlock()
	return retentionDays
}

// cleanRecordings deletes every recorded segment older than the retention
// window, then removes the now-empty per-stream directories it leaves behind.
// It is a best effort and never fails: a missing root or an unwritable file is
// ignored. MediaMTX writes the segment it is currently filling with a fresh
// mtime, so nothing open reaches the cutoff.
func cleanRecordings() {
	recordingsMu.Lock()
	root := recordingsRoot
	recordingsMu.Unlock()
	if root == "" {
		return
	}
	days := recordingsRetention()
	if days <= 0 {
		return
	}
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)

	// Collect the per-stream directories while walking so we can drop the empty
	// ones afterwards. All calls under one WalkDir keep the list stable.
	var streamDirs []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root {
				streamDirs = append(streamDirs, path)
			}
			return nil
		}
		if !isRecordingFile(d.Name()) {
			return nil
		}
		if info, err := d.Info(); err == nil && info.ModTime().Before(cutoff) {
			_ = os.Remove(path)
		}
		return nil
	})

	// Remove stream directories deepest-first; os.Remove only succeeds on an
	// empty directory, so non-empty ones are left alone.
	sort.Slice(streamDirs, func(i, j int) bool { return len(streamDirs[i]) > len(streamDirs[j]) })
	for _, dir := range streamDirs {
		_ = os.Remove(dir)
	}
}

// StartRecordingCleanup runs a background loop that prunes old segments every
// interval. It returns a function that stops the loop. Call it only after the
// recording root and retention have been set.
func StartRecordingCleanup(interval time.Duration) func() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if interval <= 0 {
			interval = time.Hour
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cleanRecordings()
			}
		}
	}()
	return cancel
}

type recordingFile struct {
	Stream string `json:"stream"` // stream path (directory name)
	Name   string `json:"name"`   // file name (a segment)
	Size   int64  `json:"size"`
	Path   string `json:"path"` // forward-slash path relative to the root
}

// listRecordings walks the recording directory and returns every segment,
// newest first. A benign error (the directory missing, e.g. nothing recorded
// yet) returns an empty list, not a failure.
func listRecordings() []recordingFile {
	recordingsMu.Lock()
	root := recordingsRoot
	recordingsMu.Unlock()
	if root == "" {
		return nil
	}
	var out []recordingFile
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !isRecordingFile(d.Name()) {
			return nil
		}
		stream := filepath.Base(filepath.Dir(path))
		rel, _ := filepath.Rel(root, path)
		out = append(out, recordingFile{
			Stream: stream,
			Name:   d.Name(),
			Size:   sizeOf(path),
			Path:   filepath.ToSlash(rel),
		})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path > out[j].Path })
	return out
}

// isRecordingFile reports whether name looks like a MediaMTX recording segment.
func isRecordingFile(name string) bool {
	return strings.HasSuffix(name, ".mp4") || strings.HasSuffix(name, ".mkv") || strings.HasSuffix(name, ".ts")
}

func sizeOf(path string) int64 {
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return 0
}

// handleRecordings serves GET /api/v1/recordings: the list of recorded
// segments, or an explanation when recording is off.
func handleRecordings(w http.ResponseWriter, r *http.Request) {
	if !validateOperatorRequest(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	if recordingsDisabled() {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled":     false,
			"recordings":  []recordingFile{},
			"instruction": "Recording is off. Start BombeCam with -recordings-dir <folder> (or BOMBECAM_RECORDINGS_DIR) to record streams to that folder.",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":        true,
		"retention_days": recordingsRetention(),
		"recordings":     listRecordings(),
	})
}

// handleRecordingDownload serves GET /api/v1/recordings/download?path=... for
// one recorded segment. The path is validated to stay inside the recording
// directory, so the page can only read files BombeCam actually recorded.
func handleRecordingDownload(w http.ResponseWriter, r *http.Request) {
	if !validateOperatorRequest(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	recordingsMu.Lock()
	root := recordingsRoot
	recordingsMu.Unlock()
	if root == "" {
		http.Error(w, "recording is off", http.StatusNotFound)
		return
	}
	rel := filepath.FromSlash(strings.TrimPrefix(r.URL.Query().Get("path"), "/"))
	if rel == "" || !isRecordingFile(rel) {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	full := filepath.Join(root, rel)
	if !inside(root, full) {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	if strings.ContainsAny(rel, "\x00") {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	http.ServeFile(w, r, full)
}

// inside reports whether candidate is root itself or a descendant of root,
// using cleaned absolute paths so ".." cannot escape.
func inside(root, candidate string) bool {
	r := filepath.Clean(filepath.FromSlash(root))
	c := filepath.Clean(filepath.FromSlash(candidate))
	if c == r {
		return false // a directory, not a file
	}
	rel, err := filepath.Rel(r, c)
	if err != nil || rel == "." {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// latestRecording returns the newest recorded segment for one stream path
// (a camera), as a relative forward-slash path, or "" when recording is off
// or that stream has no segments yet. listRecordings returns newest first.
func latestRecording(stream string) string {
	for _, r := range listRecordings() {
		if r.Stream == stream {
			return r.Path
		}
	}
	return ""
}