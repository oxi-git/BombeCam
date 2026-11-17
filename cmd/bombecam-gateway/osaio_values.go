package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/Fever-r/BombeCam/pkg/osaiovalue"
)

// An osaioValue is a value from the Osaio app that BombeCam needs before it
// can reach Osaio: the server key (server_key.go) and the app ID (app_id.go).
// Both work the same way. Each is read, in order, from its command-line file,
// its environment variable, the file named in the environment, its file in
// the data folder (which its Settings dialog writes), and finally the value
// built into this binary. A change on the web page applies at once. See
// docs/technical/CONFIGURATION.md.
type osaioValue struct {
	kind    osaiovalue.Kind
	flag    *string                 // the command-line file
	builtIn func() string           // the value built into this binary
	install func(*osaiovalue.Store) // where requests read it from; nil if they get the store otherwise
	log     string                  // log prefix: "server-key"
	field   string                  // the JSON field a POST sends: "key"
	code    string                  // error codes: invalid_<code>, <code>_not_editable, ...
	missing SessionStatus           // the status while it is missing
	message string                  // what that status tells the user

	mu    sync.RWMutex
	store *osaiovalue.Store
}

// osaioValues lists them in the order startup checks them.
func osaioValues() []*osaioValue { return []*osaioValue{serverKeyValue, appIDValue} }

// load resolves the sources and installs the result.
func (v *osaioValue) load(dataDir string) *osaiovalue.Store {
	s := osaiovalue.Load(v.kind, osaiovalue.Options{
		FlagFile:    *v.flag,
		DefaultFile: filepath.Join(dataDir, v.kind.FileName),
		BuiltIn:     v.builtIn(),
	})
	st := s.Status()
	switch {
	case st.Configured:
		fmt.Printf("[%s] using the %s from %s\n", v.log, v.kind.Noun, v.sourceName(st.Source))
	case st.Problem != "":
		fmt.Printf("[%s] %s is set but unusable: %s\n", v.log, v.sourceName(st.Source), st.Problem)
	default:
		fmt.Printf("[%s] no %s configured; enter it on the web page (cloud sign-in waits until then)\n", v.log, v.kind.Name)
	}
	v.set(s)
	return s
}

func (v *osaioValue) set(s *osaiovalue.Store) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.store = s
	if v.install != nil {
		v.install(s)
	}
}

// get returns the store (nil before startup).
func (v *osaioValue) get() *osaiovalue.Store {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.store
}

// status is the value's status for the page; it never includes the value.
func (v *osaioValue) status() osaiovalue.Status {
	if s := v.get(); s != nil {
		return s.Status()
	}
	return osaiovalue.Status{}
}

func (v *osaioValue) sourceName(src osaiovalue.Source) string {
	switch src {
	case osaiovalue.SourceFlag:
		return v.kind.Flag
	case osaiovalue.SourceEnv:
		return v.kind.EnvValue
	case osaiovalue.SourceEnvFile:
		return v.kind.EnvFile
	case osaiovalue.SourceFile:
		return "the saved " + v.kind.Noun + " file (" + v.kind.FileName + ")"
	case osaiovalue.SourceBuiltIn:
		return "the " + v.kind.Noun + " built into this version"
	}
	return "nowhere"
}

// Startup that stopped for lack of a value, or because Osaio refused the
// start-up login, runs again once a value is saved. Checking for a value and
// parking the startup happen under pendingStartupMu, as does taking it after
// a save, so a save can't slip in between and leave the startup waiting.
// osaioChanges counts the saves, for a startup that fails while one is made.
var (
	pendingStartupMu sync.Mutex
	pendingStartup   func()
	osaioChanges     atomic.Uint64
)

func setPendingStartup(f func()) {
	pendingStartupMu.Lock()
	pendingStartup = f
	pendingStartupMu.Unlock()
}

func takePendingStartup() func() {
	pendingStartupMu.Lock()
	defer pendingStartupMu.Unlock()
	f := pendingStartup
	pendingStartup = nil
	return f
}

// retryStartupAfterChange runs f after the next change of the server key or
// app ID, or now if one changed since gen (osaioChanges before the attempt).
func retryStartupAfterChange(gen uint64, f func()) {
	pendingStartupMu.Lock()
	if osaioChanges.Load() == gen {
		pendingStartup, f = f, nil
	}
	pendingStartupMu.Unlock()
	if f != nil {
		f()
	}
}

// waitForOsaioValues reports whether startup must wait for the server key or
// the app ID before signing in. If so, the session shows server_key_missing
// or app_id_missing, and resume runs once the value is saved on the web page.
func waitForOsaioValues(sm *SessionManager, resume func()) bool {
	pendingStartupMu.Lock()
	defer pendingStartupMu.Unlock()
	for _, v := range osaioValues() {
		store := v.get()
		if store == nil {
			continue // tests that never load a store sign with their own values
		}
		if _, err := store.Key(); err == nil {
			continue
		}
		fmt.Printf("[session] waiting for the Osaio %s before signing in (enter it on the web page)\n", v.kind.Name)
		sm.SetStatus(v.missing, v.message)
		pendingStartup = resume
		return true
	}
	return false
}

// resumeAfterChange reconnects once v changes: it finishes a startup that
// waited for a value, and signs in again, with its saved password, every
// login whose last attempt failed.
func resumeAfterChange(v *osaioValue, sm *SessionManager, stm *StreamManager) {
	forgetRenewAttempts()
	pendingStartupMu.Lock()
	f := pendingStartup
	pendingStartup = nil
	if st := sm.GetStatus(); st.Status == SessionStatusServerKeyMissing || st.Status == SessionStatusAppIDMissing {
		sm.SetStatus(SessionStatusCredentialsMissing, "")
	}
	pendingStartupMu.Unlock()
	if f != nil {
		fmt.Printf("[%s] %s changed; continuing startup\n", v.log, v.kind.Noun)
		f()
		return
	}
	for _, session := range stm.Registry().Sessions() {
		switch session.GetStatus().Status {
		case SessionStatusServerKeyMissing, SessionStatusAppIDMissing, SessionStatusInvalidCredentials:
		default:
			continue
		}
		email := session.Email()
		password := savedPassword(email, stm)
		if email == "" || password == "" {
			continue
		}
		fmt.Printf("[%s] %s changed; signing in %s again\n", v.log, v.kind.Noun, email)
		if err := session.Login(email, password); err != nil {
			fmt.Printf("[session] signing in %s after the %s change failed: %v\n", email, v.kind.Name, err)
			continue
		}
		saveLoginToken(email, session.Cloud(), stm)
	}
}

// handle serves the value's endpoint (/api/v1/server-key, /api/v1/app-id).
//
//	GET    -> {"configured", "source", "editable", "has_built_in", "problem"};
//	          never the value itself, which the page does not show
//	POST   {"<field>": "..."} uses that value from now on (saved in the data folder)
//	DELETE goes back to the value built into this version
func (v *osaioValue) handle(w http.ResponseWriter, r *http.Request, sm *SessionManager, stm *StreamManager) {
	store := v.get()
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, v.status())
		return
	case http.MethodPost, http.MethodDelete:
	default:
		http.Error(w, "GET, POST or DELETE required", http.StatusMethodNotAllowed)
		return
	}
	if !validateOperatorRequest(w, r, stm) {
		return
	}
	if store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": v.code + "_unavailable", "message": "BombeCam is still starting; try again in a moment."})
		return
	}
	if r.Method == http.MethodDelete {
		if err := store.Reset(); err != nil {
			v.changeFailed(w, store, err)
			return
		}
		fmt.Printf("[%s] back to the %s built into this version\n", v.log, v.kind.Noun)
		osaioChanges.Add(1)
		go guardStartup(sm, func() { resumeAfterChange(v, sm, stm) })
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, v.code: store.Status()})
		return
	}
	var req map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request", "message": fmt.Sprintf(`Send {"%s": "..."}.`, v.field)})
		return
	}
	value, _ := req[v.field].(string)
	if err := store.Save(value); err != nil {
		v.changeFailed(w, store, err)
		return
	}
	fmt.Printf("[%s] %s changed from the web page; now using %s\n", v.log, v.kind.Noun, v.sourceName(store.Status().Source))
	osaioChanges.Add(1)
	go guardStartup(sm, func() { resumeAfterChange(v, sm, stm) })
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, v.code: store.Status()})
}

func (v *osaioValue) changeFailed(w http.ResponseWriter, store *osaiovalue.Store, err error) {
	code, errCode := http.StatusBadRequest, "invalid_"+v.code
	switch {
	case errors.Is(err, v.kind.ErrNotEditable):
		code, errCode = http.StatusConflict, v.code+"_not_editable"
	case !errors.Is(err, v.kind.ErrNone):
		code, errCode = http.StatusInternalServerError, v.code+"_not_saved"
	}
	writeJSON(w, code, map[string]any{"error": errCode, "message": err.Error(), v.code: store.Status()})
}
