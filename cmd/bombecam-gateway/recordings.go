package main

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
		"enabled":    true,
		"recordings": listRecordings(),
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