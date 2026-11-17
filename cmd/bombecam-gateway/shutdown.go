package main

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// The Shut down button in Settings ends BombeCam the same way Ctrl+C or closing
// the window does: main stops every stream and the video server (MediaMTX),
// then exits. The router's Firewall rules are not touched; they live on the
// router and keep working without BombeCam.
var (
	shutdownOnce      sync.Once
	shutdownRequested = make(chan struct{})
)

// requestShutdown asks main to shut down. Safe to call more than once.
func requestShutdown() {
	shutdownOnce.Do(func() { close(shutdownRequested) })
}

// shutdownContext ends when the gateway is asked to shut down, so background
// jobs started at startup stop with it instead of running until the process
// ends.
func shutdownContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-shutdownRequested:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx
}

// shutdownDelay lets the reply reach the browser before the server stops.
var shutdownDelay = 300 * time.Millisecond

// handleGatewayShutdown handles POST /api/v1/gateway/shutdown. Like the
// Firewall routes it needs operator authentication unless the request comes
// from this PC, and a browser session must send its CSRF token.
func handleGatewayShutdown(w http.ResponseWriter, r *http.Request, streamMgr *StreamManager) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	if om := GatewayOperatorManager(); om != nil {
		if !om.ValidateSecurityBoundary(w, r, signInRequired(r, currentStreamManager())) {
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"status":           "shutting_down",
		"router_unchanged": true,
		"message":          "BombeCam is shutting down. Streams stop; Firewall rules stay on the router.",
	})
	go func() {
		time.Sleep(shutdownDelay)
		requestShutdown()
	}()
}
