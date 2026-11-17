package shadowshim

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"sync"
	"time"
)

// ThingShadow encapsulates the live shadow state for a single IoT Thing.
type ThingShadow struct {
	mu               sync.RWMutex
	ThingName        string
	Desired          map[string]any
	Reported         map[string]any
	MetadataDesired  map[string]int64
	MetadataReported map[string]int64
	ReportedReceipt  map[string]time.Time
	DesiredSeq       map[string]int64
	ReportedSeq      map[string]int64
	Version          int64
	LastUpdated      time.Time
}

func newThingShadow(name string) *ThingShadow {
	return &ThingShadow{
		ThingName:        name,
		Desired:          make(map[string]any),
		Reported:         make(map[string]any),
		MetadataDesired:  make(map[string]int64),
		MetadataReported: make(map[string]int64),
		ReportedReceipt:  make(map[string]time.Time),
		DesiredSeq:       make(map[string]int64),
		ReportedSeq:      make(map[string]int64),
		Version:          0,
		LastUpdated:      time.Now(),
	}
}

type readbackWaiter struct {
	dispatchTime time.Time
	expected     map[string]any
	ch           chan *ConfirmationResult
	completed    bool
}

// StateMachine manages shadows across all things and orchestrates Invariant I4 readback.
type StateMachine struct {
	thingsMu  sync.RWMutex
	things    map[string]*ThingShadow
	waitersMu sync.Mutex
	waiters   map[string][]*readbackWaiter
}

// NewStateMachine instantiates an empty StateMachine.
func NewStateMachine() *StateMachine {
	return &StateMachine{
		things:  make(map[string]*ThingShadow),
		waiters: make(map[string][]*readbackWaiter),
	}
}

func (sm *StateMachine) getOrCreateThing(thingName string) *ThingShadow {
	sm.thingsMu.Lock()
	defer sm.thingsMu.Unlock()
	t, ok := sm.things[thingName]
	if !ok {
		t = newThingShadow(thingName)
		sm.things[thingName] = t
	}
	return t
}

// GetShadow returns a snapshot of the current shadow document for a thing.
func (sm *StateMachine) GetShadow(thingName string) (*ShadowDocument, error) {
	sm.thingsMu.RLock()
	t, ok := sm.things[thingName]
	sm.thingsMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("thing '%s' not found", thingName)
	}

	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.buildDocumentLocked(""), nil
}

// FreshReportedShadow filters observations by their local receipt time. Camera
// clocks and new desired commands must not make an old observation look fresh.
func (sm *StateMachine) FreshReportedShadow(thingName string, maxAge time.Duration) (*ShadowDocument, error) {
	sm.thingsMu.RLock()
	t := sm.things[thingName]
	sm.thingsMu.RUnlock()
	if t == nil {
		return nil, fmt.Errorf("thing '%s' not found", thingName)
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	doc := t.buildDocumentLocked("")
	doc.State.Desired, doc.State.Delta, doc.Metadata.Desired = nil, nil, nil
	for key := range doc.State.Reported {
		receipt, ok := t.ReportedReceipt[key]
		meta := doc.Metadata.Reported[key]
		if !ok || time.Since(receipt) > maxAge || time.Since(time.Unix(meta.Timestamp, 0)) > maxAge {
			delete(doc.State.Reported, key)
			delete(doc.Metadata.Reported, key)
		}
	}
	return doc, nil
}

// getLastReportedState retrieves the last reported state for diagnostics with zero nested locks.
func (sm *StateMachine) getLastReportedState(thingName string) map[string]any {
	sm.thingsMu.RLock()
	t, ok := sm.things[thingName]
	sm.thingsMu.RUnlock()

	if !ok {
		return nil
	}

	t.mu.RLock()
	defer t.mu.RUnlock()
	lastReported := make(map[string]any, len(t.Reported))
	for k, v := range t.Reported {
		lastReported[k] = v
	}
	return lastReported
}

// ApplyUpdate processes an incoming shadow update request, updating desired/reported states,
// calculating deltas, incrementing versions, and triggering Invariant I4 confirmation checks.
func (sm *StateMachine) ApplyUpdate(thingName string, req *ShadowUpdateRequest) (*ShadowDocument, *ShadowDeltaMessage, error) {
	if thingName == "" {
		return nil, nil, fmt.Errorf("thingName cannot be empty")
	}

	t := sm.getOrCreateThing(thingName)

	// Critical section 1: Mutate ThingShadow state
	t.mu.Lock()

	now := time.Now()
	nowUnix := now.Unix()
	if req.Timestamp > 0 {
		nowUnix = req.Timestamp
	} else if nowUnix < t.LastUpdated.Unix() {
		// Clock monotonicity guard: ensure nowUnix never regresses below t.LastUpdated.Unix()
		nowUnix = t.LastUpdated.Unix()
	}

	// 1. Process Desired State
	hasDesiredUpdates := false
	if req.State.Desired != nil {
		for k, v := range req.State.Desired {
			existingTs, exists := t.MetadataDesired[k]
			if exists && req.Timestamp > 0 && req.Timestamp < existingTs {
				// Reject stale out-of-order desired update with explicit past timestamp
				continue
			}
			hasDesiredUpdates = true
			t.DesiredSeq[k]++
			if v == nil {
				delete(t.Desired, k)
				delete(t.MetadataDesired, k)
			} else {
				t.Desired[k] = v
				attrTs := nowUnix
				if exists && attrTs < existingTs {
					attrTs = existingTs // Ensure monotonic non-decreasing timestamp in metadata
				}
				t.MetadataDesired[k] = attrTs
			}
		}
	}

	// 2. Process Reported State
	hasReportedUpdates := false
	if req.State.Reported != nil {
		for k, v := range req.State.Reported {
			existingTs, exists := t.MetadataReported[k]
			if exists && req.Timestamp > 0 && req.Timestamp < existingTs {
				// Reject stale out-of-order reported update to prevent state regression
				continue
			}
			hasReportedUpdates = true
			if v == nil {
				delete(t.Reported, k)
				delete(t.MetadataReported, k)
				delete(t.ReportedReceipt, k)
			} else {
				t.Reported[k] = v
				attrTs := nowUnix
				if exists && attrTs < existingTs {
					attrTs = existingTs // Ensure monotonic non-decreasing timestamp in metadata
				}
				t.MetadataReported[k] = attrTs
				t.ReportedReceipt[k] = now
				t.ReportedSeq[k] = t.DesiredSeq[k] // Catch up sequence on reported match
			}
		}
	}

	// 3. Increment Shadow Version if changes were made or empty update
	if hasDesiredUpdates || hasReportedUpdates || (req.State.Desired == nil && req.State.Reported == nil) {
		t.Version++
		t.LastUpdated = now
	}

	// 4. Calculate Delta (Desired vs Reported, including unacknowledged idempotent commands)
	deltaState := make(map[string]any)
	deltaMetadata := make(map[string]FieldMetadata)

	for k, desVal := range t.Desired {
		repVal, hasRep := t.Reported[k]
		desSeq := t.DesiredSeq[k]
		repSeq := t.ReportedSeq[k]

		if !hasRep || !valuesEqual(desVal, repVal) || desSeq > repSeq {
			deltaState[k] = desVal
			deltaMetadata[k] = FieldMetadata{Timestamp: t.MetadataDesired[k]}
		}
	}

	var deltaMsg *ShadowDeltaMessage
	if len(deltaState) > 0 {
		deltaMsg = &ShadowDeltaMessage{
			State:     deltaState,
			Metadata:  deltaMetadata,
			Timestamp: nowUnix,
			Version:   t.Version,
		}
	}

	doc := t.buildDocumentLocked(req.ClientToken)

	// Prepare safe snapshots for waiter notification under lock
	var reportedSnapshot map[string]any
	var metaSnapshot map[string]int64
	var receiptSnapshot map[string]time.Time
	currentVersion := t.Version

	if hasReportedUpdates {
		reportedSnapshot = make(map[string]any, len(t.Reported))
		for k, v := range t.Reported {
			reportedSnapshot[k] = v
		}
		metaSnapshot = make(map[string]int64, len(t.MetadataReported))
		for k, ts := range t.MetadataReported {
			metaSnapshot[k] = ts
		}
		receiptSnapshot = make(map[string]time.Time, len(t.ReportedReceipt))
		for k, rt := range t.ReportedReceipt {
			receiptSnapshot[k] = rt
		}
	}

	// RELEASE t.mu BEFORE calling sm.notifyWaiters to eliminate AB-BA deadlock!
	t.mu.Unlock()

	// 5. Notify Invariant I4 waiters outside of t.mu
	if hasReportedUpdates {
		sm.notifyWaiters(thingName, reportedSnapshot, metaSnapshot, receiptSnapshot, now, nowUnix, currentVersion)
	}

	return doc, deltaMsg, nil
}

func (t *ThingShadow) buildDocumentLocked(clientToken string) *ShadowDocument {
	desiredCopy := make(map[string]any, len(t.Desired))
	for k, v := range t.Desired {
		desiredCopy[k] = v
	}

	reportedCopy := make(map[string]any, len(t.Reported))
	for k, v := range t.Reported {
		reportedCopy[k] = v
	}

	deltaCopy := make(map[string]any)
	for k, desVal := range t.Desired {
		repVal, hasRep := t.Reported[k]
		desSeq := t.DesiredSeq[k]
		repSeq := t.ReportedSeq[k]
		if !hasRep || !valuesEqual(desVal, repVal) || desSeq > repSeq {
			deltaCopy[k] = desVal
		}
	}

	metaDesired := make(map[string]FieldMetadata, len(t.MetadataDesired))
	for k, ts := range t.MetadataDesired {
		metaDesired[k] = FieldMetadata{Timestamp: ts}
	}

	metaReported := make(map[string]FieldMetadata, len(t.MetadataReported))
	for k, ts := range t.MetadataReported {
		metaReported[k] = FieldMetadata{Timestamp: ts}
	}

	return &ShadowDocument{
		State: ShadowState{
			Desired:  desiredCopy,
			Reported: reportedCopy,
			Delta:    deltaCopy,
		},
		Metadata: ShadowMetadata{
			Desired:  metaDesired,
			Reported: metaReported,
		},
		Timestamp:   t.LastUpdated.Unix(),
		Version:     t.Version,
		ClientToken: clientToken,
	}
}

// Invariant I4 Monotonic Readback Implementation
func (sm *StateMachine) notifyWaiters(thingName string, reported map[string]any, metadataReported map[string]int64, reportedReceipt map[string]time.Time, reportedTime time.Time, reportedSec int64, version int64) {
	sm.waitersMu.Lock()
	waiterList, ok := sm.waiters[thingName]
	if !ok || len(waiterList) == 0 {
		sm.waitersMu.Unlock()
		return
	}

	var remaining []*readbackWaiter
	for _, w := range waiterList {
		if w.completed {
			continue
		}

		// Invariant I4 Monotonicity Check 1:
		// Gateway receive time must satisfy reportedTime >= w.dispatchTime (no negative jitter)
		if reportedTime.Before(w.dispatchTime) {
			remaining = append(remaining, w)
			continue
		}

		// Invariant I4 Monotonicity Check 2:
		// Payload timestamp must satisfy reportedSec >= w.dispatchTime.Unix() when reportedSec > 0
		if reportedSec > 0 && reportedSec < w.dispatchTime.Unix() {
			remaining = append(remaining, w)
			continue
		}

		// Invariant I4 Monotonicity Check 3:
		// Per-attribute freshness: for every k in expected, require:
		// - reported value matches expected value
		// - attrTs >= w.dispatchTime.Unix()
		// - receiptTime >= w.dispatchTime
		matched := true
		for k, expectedVal := range w.expected {
			reportedVal, exists := reported[k]
			if !exists || !valuesEqual(expectedVal, reportedVal) {
				matched = false
				break
			}

			attrTs, hasMeta := metadataReported[k]
			if !hasMeta || attrTs < w.dispatchTime.Unix() {
				matched = false
				break
			}

			receiptTime, hasReceipt := reportedReceipt[k]
			if !hasReceipt || receiptTime.Before(w.dispatchTime) {
				matched = false
				break
			}
		}

		if matched {
			w.completed = true
			confirmedState := make(map[string]any, len(w.expected))
			for k := range w.expected {
				confirmedState[k] = reported[k]
			}

			res := &ConfirmationResult{
				ThingName:            thingName,
				DispatchTime:         w.dispatchTime,
				ConfirmationTime:     reportedTime,
				Duration:             reportedTime.Sub(w.dispatchTime),
				ConfirmedState:       confirmedState,
				ReportedTimestampSec: reportedSec,
				Version:              version,
			}
			select {
			case w.ch <- res:
			default:
			}
			close(w.ch)
		} else {
			remaining = append(remaining, w)
		}
	}

	if len(remaining) > 0 {
		sm.waiters[thingName] = remaining
	} else {
		delete(sm.waiters, thingName)
	}
	sm.waitersMu.Unlock()
}

// RegisterWaiter registers a waiter for thingName synchronously.
// It returns the waiter and a cleanup function to unregister the waiter if canceled or timed out.
func (sm *StateMachine) RegisterWaiter(thingName string, expected map[string]any, dispatchTime time.Time) (*readbackWaiter, func(), error) {
	if thingName == "" {
		return nil, nil, fmt.Errorf("thingName cannot be empty")
	}
	if len(expected) == 0 {
		return nil, nil, fmt.Errorf("expected state map cannot be empty")
	}

	w := &readbackWaiter{
		dispatchTime: dispatchTime,
		expected:     expected,
		ch:           make(chan *ConfirmationResult, 1),
	}

	sm.waitersMu.Lock()
	sm.waiters[thingName] = append(sm.waiters[thingName], w)
	sm.waitersMu.Unlock()

	cleanup := func() {
		sm.waitersMu.Lock()
		defer sm.waitersMu.Unlock()
		list := sm.waiters[thingName]
		var filtered []*readbackWaiter
		for _, item := range list {
			if item != w {
				filtered = append(filtered, item)
			}
		}
		if len(filtered) > 0 {
			sm.waiters[thingName] = filtered
		} else {
			delete(sm.waiters, thingName)
		}
	}

	return w, cleanup, nil
}

// ConfirmReadback waits for monotonic readback confirmation per Invariant I4.
func (sm *StateMachine) ConfirmReadback(ctx context.Context, thingName string, expected map[string]any, dispatchTime time.Time, timeout time.Duration) (*ConfirmationResult, error) {
	w, cleanup, err := sm.RegisterWaiter(thingName, expected, dispatchTime)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case res := <-w.ch:
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		lastReported := sm.getLastReportedState(thingName)
		return nil, &ErrReadbackTimeout{
			ThingName:    thingName,
			DispatchTime: dispatchTime,
			Timeout:      timeout,
			Expected:     expected,
			LastReported: lastReported,
		}
	}
}

// valuesEqual provides robust comparison across JSON numbers, ints, floats, booleans, and strings.
func valuesEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}

	// Try numeric equality
	af, aOk := toFloat(a)
	bf, bOk := toFloat(b)
	if aOk && bOk {
		return math.Abs(af-bf) < 1e-9
	}

	// Try boolean equality
	ab, aBool := toBool(a)
	bb, bBool := toBool(b)
	if aBool && bBool {
		return ab == bb
	}

	// Direct reflection fallback
	return reflect.DeepEqual(a, b)
}

func toFloat(v any) (float64, bool) {
	switch val := v.(type) {
	case float64:
		return val, true
	case float32:
		return float64(val), true
	case int:
		return float64(val), true
	case int64:
		return float64(val), true
	case int32:
		return float64(val), true
	case uint:
		return float64(val), true
	case uint64:
		return float64(val), true
	case uint32:
		return float64(val), true
	}
	return 0, false
}

func toBool(v any) (bool, bool) {
	switch val := v.(type) {
	case bool:
		return val, true
	}
	return false, false
}
