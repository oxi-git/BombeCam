package main

import (
	"flag"
	"net/http"

	"github.com/Fever-r/BombeCam/pkg/osaiovalue"
	"github.com/Fever-r/BombeCam/pkg/serverkey"
)

// The Osaio server key signs every cloud request. It is read, in order, from
// -server-key-file, BOMBECAM_SERVER_KEY, BOMBECAM_SERVER_KEY_FILE, server.key
// in the data folder (which Settings > Server key writes), and finally the key
// built into this binary (serverkey.LatestKnown, set at build time; empty in a
// build without one). A key changed on the web page applies at once. The app
// ID works the same way (app_id.go, osaio_values.go). See
// docs/technical/CONFIGURATION.md.

var serverKeyFileFlag = flag.String("server-key-file", "", "file holding the Osaio server key (overrides BOMBECAM_SERVER_KEY, BOMBECAM_SERVER_KEY_FILE, the saved key and the built-in key)")

// serverKeyValue is the server key. Requests get its store through the
// session manager (keys), not through install.
var serverKeyValue = &osaioValue{
	kind:    serverkey.Kind,
	flag:    serverKeyFileFlag,
	builtIn: func() string { return serverkey.LatestKnown },
	log:     "server-key",
	field:   "key",
	code:    "server_key",
	missing: SessionStatusServerKeyMissing,
	message: serverKeyMissingMessage,
}

// loadServerKeys resolves the key sources and installs the result.
func loadServerKeys(dataDir string) *serverkey.Store { return serverKeyValue.load(dataDir) }

func setServerKeys(s *serverkey.Store) { serverKeyValue.set(s) }

// serverKeyStatus is the key's status for the page; it never includes the key.
func serverKeyStatus() osaiovalue.Status { return serverKeyValue.status() }

// resumeAfterKeyChange reconnects once the key changes (see resumeAfterChange).
func resumeAfterKeyChange(sm *SessionManager, stm *StreamManager) {
	resumeAfterChange(serverKeyValue, sm, stm)
}

// handleServerKey serves /api/v1/server-key (see osaioValue.handle): GET the
// status, POST {"key": "..."} to use a key, DELETE to go back to the built-in
// one.
func handleServerKey(w http.ResponseWriter, r *http.Request, sm *SessionManager, stm *StreamManager) {
	serverKeyValue.handle(w, r, sm, stm)
}
