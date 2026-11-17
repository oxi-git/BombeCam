package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/Fever-r/BombeCam/internal/mediamtx"
	"github.com/Fever-r/BombeCam/pkg/bridge"
)

var (
	mediaRuntimeMu  sync.RWMutex
	mediaSupervisor *mediamtx.Supervisor
	mediaAPIBase    = fmt.Sprintf("http://127.0.0.1:%d", mediamtx.DefaultAPIPort)
)

// setMediaRuntime records the MediaMTX supervisor (nil when unmanaged) and API base.
func setMediaRuntime(sup *mediamtx.Supervisor, apiBase string) {
	mediaRuntimeMu.Lock()
	defer mediaRuntimeMu.Unlock()
	mediaSupervisor = sup
	if apiBase != "" {
		mediaAPIBase = strings.TrimRight(apiBase, "/")
	}
}

func currentMediaRuntime() (*mediamtx.Supervisor, string) {
	mediaRuntimeMu.RLock()
	defer mediaRuntimeMu.RUnlock()
	return mediaSupervisor, mediaAPIBase
}

// mediaServerHealth reports whether MediaMTX is usable and, if not, why.
func mediaServerHealth() (ok bool, detail string) {
	sup, apiBase := currentMediaRuntime()
	if sup != nil {
		st := sup.Status()
		if st.LastError != "" && !st.Listening {
			return false, st.LastError
		}
		if !st.Listening {
			return false, "the video server (MediaMTX) is not running"
		}
		return true, ""
	}
	if _, _, err := mediamtx.GetPath(apiBase, "bombecam-health"); err != nil {
		return false, fmt.Sprintf("the video server (MediaMTX) API at %s is unreachable", apiBase)
	}
	return true, ""
}

func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// resolveFFmpeg finds an FFmpeg executable, or returns "" when none exists.
// requested is the -ffmpeg flag / FFMPEG_PATH value ("ffmpeg" means "search").
func resolveFFmpeg(requested string) string {
	requested = strings.TrimSpace(requested)
	if requested != "" && requested != "ffmpeg" && requested != "ffmpeg.exe" {
		if isFile(requested) {
			if abs, err := filepath.Abs(requested); err == nil {
				return abs
			}
			return requested
		}
		if p, err := exec.LookPath(requested); err == nil {
			return p
		}
		fmt.Printf("[media] FFmpeg not found at %q\n", requested)
		return ""
	}
	name := exeName("ffmpeg")
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), name))
	}
	candidates = append(candidates, filepath.Join(mediamtx.DefaultCacheDir(), name))
	for _, c := range candidates {
		if isFile(c) {
			return c
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	if runtime.GOOS == "windows" {
		local := os.Getenv("LOCALAPPDATA")
		home := os.Getenv("USERPROFILE")
		pf := os.Getenv("ProgramFiles")
		pd := os.Getenv("ProgramData")
		win := []string{
			filepath.Join(local, "Microsoft", "WinGet", "Links", name),
			filepath.Join(`C:\ffmpeg`, "bin", name),
			filepath.Join(pf, "ffmpeg", "bin", name),
			filepath.Join(home, "scoop", "shims", name),
			filepath.Join(pd, "chocolatey", "bin", name),
		}
		if matches, _ := filepath.Glob(filepath.Join(local, "Microsoft", "WinGet", "Packages", "Gyan.FFmpeg*", "*", "bin", name)); len(matches) > 0 {
			win = append(win, matches...)
		}
		for _, c := range win {
			if c != "" && isFile(c) {
				return c
			}
		}
	}
	return ""
}

// choosePublisher resolves -publisher auto|native|ffmpeg. The built-in
// publisher is the default: it timestamps audio and video on one clock (the
// FFmpeg pipeline opened its two inputs one after the other and left audio
// seconds out of sync). FFmpeg, when installed, is still used, but only to
// add a G.711 copy of the audio for browsers and to encode talkback.
func choosePublisher(mode, ffmpegPath string) string {
	if strings.ToLower(strings.TrimSpace(mode)) == "ffmpeg" {
		if ffmpegPath == "" {
			fmt.Printf("[media] -publisher ffmpeg requested but FFmpeg was not found; using the built-in publisher\n")
			return bridge.PublisherNative
		}
		return bridge.PublisherFFmpeg
	}
	return bridge.PublisherNative
}

// setupGatewayLog mirrors everything the gateway (and its FFmpeg children)
// prints into %LOCALAPPDATA%\bombecam\logs\gateway.log (previous run kept
// as gateway.prev.log), so problems can be diagnosed after the console closes.
// gatewayLogDir is the folder of gateway.log and mediamtx.log.
func gatewayLogDir() string {
	return filepath.Join(filepath.Dir(mediamtx.DefaultCacheDir()), "logs")
}

func setupGatewayLog() string {
	dir := gatewayLogDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	p := filepath.Join(dir, "gateway.log")
	if _, err := os.Stat(p); err == nil {
		_ = os.Rename(p, filepath.Join(dir, "gateway.prev.log"))
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return ""
	}
	r, w, err := os.Pipe()
	if err != nil {
		_ = f.Close()
		return ""
	}
	console := os.Stdout
	os.Stdout, os.Stderr = w, w
	// A crash (Go panic) is written straight to the process's error handle,
	// not through os.Stderr: send it to the log file too, so it is not lost
	// when the console window closes.
	_ = debug.SetCrashOutput(f, debug.CrashOptions{})
	disableConsoleQuickEdit()
	// The file comes first and the console never holds BombeCam up: a
	// console window can pause its output (for example while text is
	// selected in it), and a blocked write here would, once the pipe is
	// full, stop every part of BombeCam that prints. Console lines that
	// cannot be shown at once are dropped; the file keeps them all.
	toConsole := make(chan string, 256)
	go func() {
		for line := range toConsole {
			_, _ = console.WriteString(line)
		}
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		br := bufio.NewReaderSize(r, 64*1024)
		for {
			line, err := br.ReadString('\n')
			if len(line) > 0 {
				_, _ = f.WriteString(time.Now().Format("2006-01-02 15:04:05.000 ") + line)
				select {
				case toConsole <- line:
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()
	// On exit, let the last lines ("bye.") reach the file before the
	// process ends.
	flushGatewayLog = func() {
		_ = w.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		_ = f.Sync()
	}
	return p
}

// flushGatewayLog writes out what is still in the log pipe; set by
// setupGatewayLog, a no-op otherwise.
var flushGatewayLog = func() {}
