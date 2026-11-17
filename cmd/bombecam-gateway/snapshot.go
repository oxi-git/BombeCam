package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Fever-r/BombeCam/pkg/profile"
)

// Snapshots: GET /api/v1/cameras/{id}/snapshot.jpg returns one JPEG frame,
// taken by FFmpeg from the camera's local RTSP stream and cached for a few
// seconds (Home Assistant's still image, notifications). The web UI's port
// is only on this PC, so when "Snapshots for other devices" is on, a second,
// small listener serves the same route on the network (port 8655). That
// listener has no other routes at all: control and settings stay on this PC.

const (
	snapshotCacheFor = 4 * time.Second
	snapshotStaleFor = 60 * time.Second
)

var errNoFFmpeg = errors.New("snapshots need FFmpeg on this PC (winget install Gyan.FFmpeg), then restart BombeCam")

type snapEntry struct {
	jpeg []byte    // result of the last attempt (nil when it failed)
	err  error     // error of the last attempt
	at   time.Time // time of the last attempt

	good   []byte // last picture that worked
	goodAt time.Time
}

type snapshotService struct {
	mu       sync.Mutex
	cache    map[string]*snapEntry
	inflight map[string]chan struct{}

	srv    *http.Server
	port   int
	status string

	// grab is FFmpeg by default; tests replace it.
	grab func(ctx context.Context, ffmpeg, rtspURL string) ([]byte, error)
}

var snapshots = &snapshotService{grab: grabJPEG}

// grabJPEG asks FFmpeg for one decoded frame of an RTSP stream as a JPEG.
func grabJPEG(ctx context.Context, ffmpeg, rtspURL string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error",
		"-rtsp_transport", "tcp", "-i", rtspURL,
		"-an", "-frames:v", "1", "-q:v", "4", "-f", "image2", "-c:v", "mjpeg", "pipe:1")
	hideConsole(cmd)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("no picture from the camera within the time limit")
		}
		msg := strings.TrimSpace(errOut.String())
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, fmt.Errorf("ffmpeg: %v %s", err, msg)
	}
	if out.Len() < 100 || out.Bytes()[0] != 0xFF || out.Bytes()[1] != 0xD8 {
		return nil, fmt.Errorf("ffmpeg returned no JPEG")
	}
	return out.Bytes(), nil
}

// Get returns a JPEG for the camera. A picture taken in the last few seconds
// is returned as is. An older one (up to snapshotStaleFor) is also returned
// at once while a fresh one is taken in the background, so Home Assistant
// does not wait for FFmpeg to find a keyframe on every refresh. One FFmpeg
// runs per camera at a time; concurrent callers share its result.
func (s *snapshotService) Get(ctx context.Context, sm *StreamManager, cameraID string) ([]byte, error) {
	ffmpeg := sm.FFmpegPath()
	if ffmpeg == "" {
		return nil, errNoFFmpeg
	}
	src := fmt.Sprintf("%s/%s", strings.TrimRight(sm.PublishRTSPBase(), "/"), cameraID)
	for {
		s.mu.Lock()
		if s.cache == nil {
			s.cache, s.inflight = map[string]*snapEntry{}, map[string]chan struct{}{}
		}
		e := s.cache[cameraID]
		if e != nil && time.Since(e.at) < snapshotCacheFor {
			jpeg, err := e.jpeg, e.err
			if jpeg == nil && e.good != nil && time.Since(e.goodAt) < snapshotStaleFor {
				jpeg, err = e.good, nil // the last try failed; the previous picture is still recent
			}
			s.mu.Unlock()
			return jpeg, err
		}
		ch, busy := s.inflight[cameraID]
		if !busy {
			ch = make(chan struct{})
			s.inflight[cameraID] = ch
			go s.refresh(cameraID, ffmpeg, src, ch)
		}
		if e != nil && e.good != nil && time.Since(e.goodAt) < snapshotStaleFor {
			good := e.good
			s.mu.Unlock()
			return good, nil
		}
		s.mu.Unlock()
		select {
		case <-ch:
			// loop: the new cache entry is fresh now
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// refresh takes a new picture and stores it (or the error) in the cache.
func (s *snapshotService) refresh(cameraID, ffmpeg, src string, done chan struct{}) {
	s.mu.Lock()
	grab := s.grab
	s.mu.Unlock()
	gctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	jpeg, err := grab(gctx, ffmpeg, src)
	cancel()

	s.mu.Lock()
	e := s.cache[cameraID]
	if e == nil {
		e = &snapEntry{}
		s.cache[cameraID] = e
	}
	e.jpeg, e.err, e.at = jpeg, err, time.Now()
	if err == nil {
		e.good, e.goodAt = jpeg, e.at
	}
	delete(s.inflight, cameraID)
	close(done)
	s.mu.Unlock()
}

// resolveSnapshotCamera accepts a camera ID or its stream name.
func resolveSnapshotCamera(sm *StreamManager, id string) (string, bool) {
	id = strings.TrimSuffix(id, ".jpg")
	mc, ok := sm.ResolveCamera(id)
	if !ok {
		return "", false
	}
	return mc.UUID, true
}

// handleSnapshot serves GET /api/v1/cameras/{id}/snapshot.jpg.
func handleSnapshot(w http.ResponseWriter, r *http.Request, sm *StreamManager, id string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	camID, ok := resolveSnapshotCamera(sm, id)
	if !ok {
		http.Error(w, "camera not found", http.StatusNotFound)
		return
	}
	jpeg, err := snapshots.Get(r.Context(), sm, camID)
	if err != nil {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "No snapshot: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(jpeg)
}

// snapshotURLFor is the snapshot URL other devices use, or "" when the
// network listener is off.
func snapshotURLFor(sm *StreamManager, r *http.Request, cameraID string) string {
	s := currentIntegrationSettings()
	if !s.Snapshots {
		return ""
	}
	host, _, _ := sm.AdvertisedHost(r)
	u := url.URL{Scheme: "http", Host: fmt.Sprintf("%s:%d", hostForURL(host), snapshotPort(s)),
		Path: "/api/v1/cameras/" + sm.streamPath(cameraID) + "/snapshot.jpg"}
	if user, pass := sm.StreamCredentials(); user != "" {
		u.User = url.UserPassword(user, pass)
	}
	return u.String()
}

// Status describes the network listener for the settings page.
func (s *snapshotService) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == "" {
		return "off"
	}
	return s.status
}

// Configure starts, moves or stops the network listener to match settings.
func (s *snapshotService) Configure(sm *StreamManager, settings profile.IntegrationSettings) {
	want := 0
	if settings.Snapshots {
		want = snapshotPort(settings)
	}
	s.mu.Lock()
	if s.srv != nil && s.port == want && strings.HasPrefix(s.status, "listening") {
		s.mu.Unlock()
		return
	}
	old := s.srv
	s.srv, s.port, s.status = nil, 0, ""
	s.mu.Unlock()
	if old != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = old.Shutdown(ctx)
		cancel()
	}
	if want == 0 {
		return
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", want))
	if err != nil {
		s.mu.Lock()
		s.status = fmt.Sprintf("error: port %d is in use by another program", want)
		s.mu.Unlock()
		fmt.Printf("[snapshots] cannot listen on port %d: %v\n", want, err)
		return
	}
	srv := &http.Server{Handler: snapshotLANHandler(sm), ReadHeaderTimeout: 5 * time.Second}
	s.mu.Lock()
	s.srv, s.port, s.status = srv, want, fmt.Sprintf("listening on port %d", want)
	s.mu.Unlock()
	fmt.Printf("[snapshots] serving snapshots to the network on port %d (read-only)\n", want)
	go func() { _ = srv.Serve(ln) }()
}

// snapshotLANHandler is everything the network listener serves: snapshots,
// read-only, behind the stream password when that is on. Host names other
// than IP addresses are refused (DNS rebinding).
func snapshotLANHandler(sm *StreamManager) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		if net.ParseIP(host) == nil && !strings.EqualFold(host, "localhost") {
			http.Error(w, "use this PC's IP address", http.StatusForbidden)
			return
		}
		if user, pass := sm.StreamCredentials(); user != "" {
			u, p, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 || subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="BombeCam"`)
				http.Error(w, "stream user name and password required", http.StatusUnauthorized)
				return
			}
		}
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/cameras/")
		parts := strings.Split(rest, "/")
		if rest == r.URL.Path || len(parts) != 2 || parts[1] != "snapshot.jpg" || parts[0] == "" {
			http.NotFound(w, r)
			return
		}
		handleSnapshot(w, r, sm, parts[0])
	})
}
