package netstack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Fever-r/BombeCam/pkg/policy"
)

// Persistent state for bombecam-net on a Linux router: the "Block cloud video"
// setting and the last ruleset, written atomically (write-temp-then-rename).
// A missing or corrupt setting restores as Block = Yes (fail closed): the
// router exists to protect the cameras, and Yes still lets controls work.

// DefaultStateDir is the standard persistent directory for BombeCam state.
const DefaultStateDir = "/var/lib/bombecam"

// State status constants.
const (
	StatusDegradedFailClosed = "DEGRADED_FAIL_CLOSED"
	StatusUnknown            = "UNKNOWN"
	// StatusRulesLoaded means the compiled ruleset is present in the kernel.
	// It is not a claim that the router is on the camera's path; only a
	// verification run (bombecam-verify) or the checklist can show that.
	StatusRulesLoaded = "RULES_LOADED_ENFORCEMENT_UNVERIFIED"
	StatusRemoved     = "RULES_REMOVED"
)

// PersistedState is stored in state.json.
type PersistedState struct {
	BlockCloudVideo  bool      `json:"block_cloud_video"`
	Region           string    `json:"region,omitempty"`
	Status           string    `json:"status"`
	RulesetHash      string    `json:"ruleset_hash"`
	LastTransition   time.Time `json:"last_transition"`
	UpdatedAt        time.Time `json:"updated_at"`
	EnforcementPoint string    `json:"enforcement_point"`
	Notes            string    `json:"notes,omitempty"`
}

// StateManager handles atomic persistence and fail-closed restore.
type StateManager struct {
	mu            sync.RWMutex
	StateDir      string
	SettingFile   string
	StateJSONFile string
	RulesFile     string
}

// NewStateManager creates a StateManager for the directory.
func NewStateManager(stateDir string) *StateManager {
	if stateDir == "" {
		stateDir = DefaultStateDir
	}
	return &StateManager{
		StateDir:      stateDir,
		SettingFile:   filepath.Join(stateDir, "block_cloud_video"),
		StateJSONFile: filepath.Join(stateDir, "state.json"),
		RulesFile:     filepath.Join(stateDir, "rules.nft"),
	}
}

// Persist atomically writes the setting, ruleset and metadata.
func (sm *StateManager) Persist(block bool, region policy.Region, ruleset string, status string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if err := os.MkdirAll(sm.StateDir, 0o750); err != nil {
		return fmt.Errorf("create state dir %q: %w", sm.StateDir, err)
	}
	now := time.Now().UTC()
	hash := ""
	if ruleset != "" {
		hash = ComputeHash(ruleset)
	}
	if status == "" {
		status = StatusRulesLoaded
	}
	val := "no\n"
	if block {
		val = "yes\n"
	}
	if err := atomicWriteFile(sm.SettingFile, []byte(val), 0o640); err != nil {
		return fmt.Errorf("write setting: %w", err)
	}
	data, err := json.MarshalIndent(PersistedState{
		BlockCloudVideo:  block,
		Region:           string(region.OrDefault()),
		Status:           status,
		RulesetHash:      hash,
		LastTransition:   now,
		UpdatedAt:        now,
		EnforcementPoint: "nftables",
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWriteFile(sm.StateJSONFile, data, 0o640); err != nil {
		return fmt.Errorf("write state.json: %w", err)
	}
	if ruleset != "" {
		if err := atomicWriteFile(sm.RulesFile, []byte(ruleset), 0o640); err != nil {
			return fmt.Errorf("write rules.nft: %w", err)
		}
	}
	return nil
}

// RestoreOnBoot returns the persisted setting. Missing, unreadable or corrupt
// state restores Block = Yes and says so in the returned state.
func (sm *StateManager) RestoreOnBoot() (bool, PersistedState, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	failClosed := func(note string) (bool, PersistedState, error) {
		now := time.Now().UTC()
		return true, PersistedState{
			BlockCloudVideo: true, Status: StatusDegradedFailClosed,
			LastTransition: now, UpdatedAt: now, EnforcementPoint: "nftables", Notes: note,
		}, nil
	}
	data, err := os.ReadFile(sm.SettingFile)
	if os.IsNotExist(err) {
		return failClosed("no saved setting: defaulting to Block cloud video = Yes")
	}
	if err != nil {
		return failClosed(fmt.Sprintf("could not read the saved setting (%v): defaulting to Yes", err))
	}
	var block bool
	switch strings.TrimSpace(strings.ToLower(string(data))) {
	case "yes":
		block = true
	case "no":
		block = false
	default:
		return failClosed(fmt.Sprintf("saved setting is corrupt (%q): defaulting to Yes", strings.TrimSpace(string(data))))
	}
	var st PersistedState
	if raw, err := os.ReadFile(sm.StateJSONFile); err == nil {
		if uerr := json.Unmarshal(raw, &st); uerr != nil {
			return true, PersistedState{BlockCloudVideo: true, Status: StatusDegradedFailClosed},
				fmt.Errorf("state file is unparseable (%w): defaulting to Yes", uerr)
		}
	}
	st.BlockCloudVideo = block
	if st.Status == "" {
		st.Status = StatusUnknown
	}
	return block, st, nil
}

// GetState reads the current PersistedState.
func (sm *StateManager) GetState() (PersistedState, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	data, err := os.ReadFile(sm.StateJSONFile)
	if err != nil {
		return PersistedState{}, err
	}
	var st PersistedState
	if err := json.Unmarshal(data, &st); err != nil {
		return PersistedState{}, err
	}
	return st, nil
}

// atomicWriteFile writes to a temp file in the same directory, syncs, and
// renames it over the target.
func atomicWriteFile(targetFile string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(targetFile)
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.tmp.%d", filepath.Base(targetFile), time.Now().UnixNano()))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, targetFile); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
