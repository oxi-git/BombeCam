package verifier

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Fever-r/BombeCam/pkg/policy"
)

// NetworkProbe defines a target endpoint and port tested during verification.
// Every probe must succeed with cloud-video blocking off (the control leg);
// ExpectedWhenBlocking is the compiled prediction for the test leg.
type NetworkProbe struct {
	Name                 string `json:"name"`
	TargetHost           string `json:"target_host"`
	Port                 int    `json:"port"`
	Protocol             string `json:"protocol"` // "tcp" or "udp"
	ExpectedWhenBlocking bool   `json:"expected_when_blocking"`
}

// ProbeMatrixForRegion is the fallback matrix for a region, used only when no
// matrix was generated from the policy.
func ProbeMatrixForRegion(region policy.Region) []NetworkProbe {
	eps, err := region.Endpoints()
	if err != nil {
		eps, _ = policy.DefaultRegion.Endpoints()
	}
	control := policy.ControlHost
	if len(eps.ControlHosts) > 0 {
		control = eps.ControlHosts[0]
	}
	return []NetworkProbe{
		{Name: "control-mqtts-8883", TargetHost: control, Port: eps.ControlPort, Protocol: "tcp", ExpectedWhenBlocking: true},
		{Name: "stream-setup-443", TargetHost: eps.StreamSetupHost, Port: eps.StreamSetupPort, Protocol: "tcp", ExpectedWhenBlocking: true},
		{Name: "public-dns-53", TargetHost: "8.8.8.8", Port: 53, Protocol: "udp", ExpectedWhenBlocking: true},
		{Name: "other-https-443", TargetHost: "1.1.1.1", Port: 443, Protocol: "tcp", ExpectedWhenBlocking: false},
		{Name: "relay-stun-3478", TargetHost: "142.250.180.127", Port: 3478, Protocol: "udp", ExpectedWhenBlocking: false},
		{Name: "arbitrary-port-9999", TargetHost: "1.1.1.1", Port: 9999, Protocol: "tcp", ExpectedWhenBlocking: false},
	}
}

// DefaultProbeMatrix is the fallback matrix for the default region.
var DefaultProbeMatrix = ProbeMatrixForRegion(policy.DefaultRegion)

// Config configures the runtime verification subsystem
type Config struct {
	CameraID         string
	CameraMAC        string
	CameraIP         string
	GatewayIP        string
	EnforcementPoint string        // "nftables", "openwrt", "consumer_router"
	Region           policy.Region // Osaio region for the fallback probe matrix
	ControlPath      ControlPathType
	VerdictTTL       time.Duration // Default: 5 minutes
	LearnedTTL       int           // Learned camera IP initial TTL (default 64)
}

// DefaultConfig returns base verification configuration
func DefaultConfig() Config {
	return Config{
		EnforcementPoint: "nftables",
		ControlPath:      ControlPathNone,
		VerdictTTL:       5 * time.Minute,
		LearnedTTL:       64,
	}
}

// Verifier implements the differential canary probing loop.
type Verifier struct {
	cfg           Config
	mu            sync.RWMutex
	blocking      bool
	controlPath   ControlPathType
	latestVerdict *Verdict
	prober        ProbeExecutor
	metrics       *Metrics
	matrix        []NetworkProbe
}

// ProbeExecutor abstraction allows swapping mock or real netns socket dialers
type ProbeExecutor interface {
	CheckPreconditions(ctx context.Context, cameraIP, cameraMAC string) (bool, error)
	ExecuteProbe(ctx context.Context, probe NetworkProbe, fromCanary bool) bool
	CheckCounterIncrement(ctx context.Context) (bool, error)

	// AttributeL2 must return true only when a frame was actually observed
	// with the camera's source MAC, this host's destination MAC, an
	// off-subnet destination IP, and an IP TTL equal to learnedTTL.
	// Returning a hardcoded true here fabricates the evidence that separates
	// ENFORCED_PROVEN from ENFORCED_OBSERVED.
	AttributeL2(ctx context.Context, cameraMAC string, learnedTTL int) (bool, error)
	// ApplyMode puts "Block cloud video" on (true) or off (false).
	ApplyMode(ctx context.Context, block bool) error
}

// TemporaryControlExecutor must use a kernel-expiring bypass for the canary
// only. It must leave the real camera subjects blocked even if this process dies.
type TemporaryControlExecutor interface {
	BeginControlLeg(context.Context, time.Duration) error
}

const controlLease = 30 * time.Second
const controlBudget = 20 * time.Second
const restorationBudget = 10 * time.Second

// SetProbeMatrix replaces the probe matrix. The matrix should
// be generated from the compiled policy so it is correct by construction for
// every backend; see MatrixFromPolicy.
func (v *Verifier) SetProbeMatrix(m []NetworkProbe) error {
	if len(m) == 0 {
		return fmt.Errorf("refusing an empty probe matrix: a verifier with nothing to probe " +
			"would report success having measured nothing")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.matrix = m
	return nil
}

// probeMatrixLocked returns the matrix. Callers must already hold v.mu —
// RunVerification and VerifyConsumerRouterBlock both do, and sync.RWMutex is
// not reentrant, so re-locking here deadlocks the verifier.
func (v *Verifier) probeMatrixLocked() []NetworkProbe {
	if len(v.matrix) == 0 {
		return ProbeMatrixForRegion(v.cfg.Region)
	}
	return v.matrix
}

// ProbeMatrix returns a copy of the active matrix for callers not holding the lock.
func (v *Verifier) ProbeMatrix() []NetworkProbe {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make([]NetworkProbe, len(v.matrix))
	copy(out, v.matrix)
	return out
}

// SetControlPath records how camera commands travel (informational).
func (v *Verifier) SetControlPath(cp ControlPathType) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.controlPath = cp
}

// ControlPath returns the currently configured control path
func (v *Verifier) ControlPath() ControlPathType {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.controlPath == "" {
		return ControlPathNone
	}
	return v.controlPath
}

// NewVerifier initializes the verification engine
func NewVerifier(cfg Config, prober ProbeExecutor, m *Metrics) *Verifier {
	if cfg.VerdictTTL <= 0 {
		cfg.VerdictTTL = 5 * time.Minute
	}
	if cfg.LearnedTTL <= 0 {
		cfg.LearnedTTL = 64
	}
	if m == nil {
		m = DefaultMetrics
	}
	cp := cfg.ControlPath
	if cp == "" {
		cp = ControlPathNone
	}
	return &Verifier{
		cfg:         cfg,
		blocking:    false, // no applied firewall state has been read back yet
		controlPath: cp,
		prober:      prober,
		metrics:     m,
		matrix:      ProbeMatrixForRegion(cfg.Region),
	}
}

// RunVerification executes the differential: with blocking off every probe
// must succeed (proving the network works), then with blocking on every probe
// must match the compiled prediction. It leaves blocking ON.
func (v *Verifier) RunVerification(ctx context.Context) (verdict *Verdict, runErr error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	now := time.Now().UTC()
	details := VerdictDetails{
		LearnedTTL: v.cfg.LearnedTTL,
	}

	cp := v.controlPath
	if cp == "" {
		cp = ControlPathNone
	}
	needsRestore := false
	// A cancelled request must not cancel restoration. The kernel lease also
	// expires independently if this defer cannot run after a hard process stop.
	defer func() {
		if !needsRestore {
			if runErr != nil && verdict == nil {
				verdict = v.unknownErrorLocked(now, cp, details, runErr)
			}
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), restorationBudget)
		defer cancel()
		if err := v.prober.ApplyMode(cleanup, true); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("blocking restoration unconfirmed: %w", err))
			v.blocking = false
			v.latestVerdict = &Verdict{Status: StatusUnknown, ControlPath: cp, BlockCloudVideo: false,
				VerifiedAt: now, ExpiresAt: now, CameraID: v.cfg.CameraID, CameraMAC: v.cfg.CameraMAC,
				EnforcementPoint: v.cfg.EnforcementPoint, Details: details}
			v.latestVerdict.Details.FailureReason = runErr.Error()
			v.metrics.RecordVerification(v.cfg.CameraID, StatusUnknown)
			v.metrics.SetBlockCloudVideo(false)
			verdict = v.latestVerdict
		} else {
			v.blocking = true
			v.metrics.SetBlockCloudVideo(true)
			if runErr != nil && verdict == nil {
				verdict = v.unknownErrorLocked(now, cp, details, runErr)
			}
		}
	}()

	// 1. PRECONDITION LEG
	precon, err := v.prober.CheckPreconditions(ctx, v.cfg.CameraIP, v.cfg.CameraMAC)
	if err != nil || !precon {
		details.PreconditionPassed = false
		details.FailureReason = fmt.Sprintf("preconditions failed (network fault, missing NAT or camera unreachable): %v", err)
		v.latestVerdict = &Verdict{
			Status:           StatusPreconditionFailed,
			ControlPath:      cp,
			BlockCloudVideo:  v.blocking,
			VerifiedAt:       now,
			ExpiresAt:        now.Add(v.cfg.VerdictTTL),
			CameraID:         v.cfg.CameraID,
			CameraMAC:        v.cfg.CameraMAC,
			EnforcementPoint: v.cfg.EnforcementPoint,
			Details:          details,
		}
		v.metrics.RecordVerification(v.cfg.CameraID, StatusPreconditionFailed)
		return v.latestVerdict, nil
	}
	details.PreconditionPassed = true

	// 2. CONTROL LEG: a bounded canary-only bypass; ALL probes must succeed.
	temporary, ok := v.prober.(TemporaryControlExecutor)
	if !ok {
		return nil, fmt.Errorf("verification refuses an executor without a kernel-expiring canary control leg")
	}
	needsRestore = true // also recover a partially applied lease on error
	controlCtx, cancelControl := context.WithTimeout(ctx, controlBudget)
	defer cancelControl()
	if err := temporary.BeginControlLeg(controlCtx, controlLease); err != nil {
		return nil, fmt.Errorf("temporary control leg failed: %w", err)
	}

	controlPassed := true
	for _, probe := range v.probeMatrixLocked() {
		if !v.prober.ExecuteProbe(controlCtx, probe, true) || controlCtx.Err() != nil {
			controlPassed = false
			details.FailureReason = fmt.Sprintf("control leg probe %s failed; network does not have working WAN path", probe.Name)
			break
		}
	}
	details.ControlLegPassed = controlPassed

	if !controlPassed {
		// The independent deferred restoration must finish before returning.
		v.latestVerdict = &Verdict{
			Status:           StatusPreconditionFailed,
			ControlPath:      cp,
			BlockCloudVideo:  true,
			VerifiedAt:       now,
			ExpiresAt:        now.Add(v.cfg.VerdictTTL),
			CameraID:         v.cfg.CameraID,
			CameraMAC:        v.cfg.CameraMAC,
			EnforcementPoint: v.cfg.EnforcementPoint,
			Details:          details,
		}
		v.metrics.RecordVerification(v.cfg.CameraID, StatusPreconditionFailed)
		return v.latestVerdict, nil
	}

	// 3. TEST LEG: blocking on; every result must match the compiled prediction
	cleanup, cancelCleanup := context.WithTimeout(context.Background(), restorationBudget)
	err = v.prober.ApplyMode(cleanup, true)
	cancelCleanup()
	if err != nil {
		return nil, fmt.Errorf("failed to restore blocking for test leg: %w", err)
	}
	needsRestore = false
	v.blocking = true
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	testPassed := true
	for _, probe := range v.probeMatrixLocked() {
		expected := probe.ExpectedWhenBlocking
		actual := v.prober.ExecuteProbe(ctx, probe, true)
		if actual != expected {
			testPassed = false
			details.FailureReason = fmt.Sprintf("probe %s mismatch: expected %v, got %v (differential failed)", probe.Name, expected, actual)
			break
		}
	}
	details.TestLegPassed = testPassed

	// 4. BIND LEG: Passive capture attribution and deny counter check
	counterIncr, cErr := v.prober.CheckCounterIncrement(ctx)
	details.CounterIncremented = counterIncr
	if cErr != nil {
		// A failed counter READ is not evidence that the counter did not move.
		details.FailureReason = fmt.Sprintf("deny-rule counter could not be read: %v", cErr)
		v.latestVerdict = &Verdict{
			CameraID: v.cfg.CameraID, EnforcementPoint: v.cfg.EnforcementPoint,
			Status: StatusUnknown, ControlPath: cp, BlockCloudVideo: true, Details: details,
			VerifiedAt: now, ExpiresAt: now.Add(v.cfg.VerdictTTL),
		}
		v.metrics.RecordVerification(v.cfg.CameraID, StatusUnknown)
		return v.latestVerdict, nil
	}
	l2, lErr := v.prober.AttributeL2(ctx, v.cfg.CameraMAC, v.cfg.LearnedTTL)
	if lErr != nil {
		details.FailureReason = fmt.Sprintf("L2 attribution failed: %v", lErr)
	}
	details.L2Attributed = l2

	// 5. VERDICT
	// A canary probe tells you about the CANARY's path. If the camera has a
	// different path — the bypass case, and the commonest real-world topology —
	// a blocked canary proves nothing about the camera. That is why L2
	// attribution is not optional: we must have observed THIS camera's frames
	// being routed through THIS appliance.
	var finalStatus VerdictStatus
	switch {
	case testPassed && counterIncr && details.L2Attributed:
		finalStatus = StatusEnforcedProven
	case testPassed && counterIncr && !details.L2Attributed:
		finalStatus = StatusEnforcedObserved
		if details.FailureReason == "" {
			details.FailureReason = "the policy blocked our probes, but no traffic from this camera " +
				"was observed passing through this appliance: the camera may have another route out"
		}
	default:
		finalStatus = StatusNotEnforced
	}

	v.blocking = true
	v.latestVerdict = &Verdict{
		Status:           finalStatus,
		ControlPath:      cp,
		BlockCloudVideo:  true,
		VerifiedAt:       now,
		ExpiresAt:        now.Add(v.cfg.VerdictTTL),
		CameraID:         v.cfg.CameraID,
		CameraMAC:        v.cfg.CameraMAC,
		EnforcementPoint: v.cfg.EnforcementPoint,
		Details:          details,
	}

	v.metrics.RecordVerification(v.cfg.CameraID, finalStatus)
	v.metrics.SetBlockCloudVideo(true)
	return v.latestVerdict, nil
}

func (v *Verifier) unknownErrorLocked(now time.Time, cp ControlPathType, details VerdictDetails, err error) *Verdict {
	details.FailureReason = err.Error()
	v.latestVerdict = &Verdict{Status: StatusUnknown, ControlPath: cp, BlockCloudVideo: v.blocking,
		VerifiedAt: now, ExpiresAt: now, CameraID: v.cfg.CameraID, CameraMAC: v.cfg.CameraMAC,
		EnforcementPoint: v.cfg.EnforcementPoint, Details: details}
	v.metrics.RecordVerification(v.cfg.CameraID, StatusUnknown)
	return v.latestVerdict
}

// VerifyConsumerRouterBlock checks the alternative to "Block cloud video": the
// camera's internet blocked entirely by some other router. Every probe must fail.
func (v *Verifier) VerifyConsumerRouterBlock(ctx context.Context) (*Verdict, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	now := time.Now().UTC()
	details := VerdictDetails{
		LearnedTTL:         v.cfg.LearnedTTL,
		PreconditionPassed: false,
		ControlLegPassed:   false,
		L2Attributed:       false,
		CounterIncremented: false,
	}

	precon, err := v.prober.CheckPreconditions(ctx, v.cfg.CameraIP, v.cfg.CameraMAC)
	if err != nil || !precon {
		details.PreconditionPassed = false
		details.FailureReason = fmt.Sprintf("preconditions failed (network fault, missing NAT or camera unreachable): %v", err)
		v.latestVerdict = &Verdict{
			Status:           StatusPreconditionFailed,
			ControlPath:      ControlPathNone,
			BlockCloudVideo:  false, // a full router block is a different mechanism
			VerifiedAt:       now,
			ExpiresAt:        now.Add(v.cfg.VerdictTTL),
			CameraID:         v.cfg.CameraID,
			CameraMAC:        v.cfg.CameraMAC,
			EnforcementPoint: "consumer_router",
			Details:          details,
		}
		v.metrics.RecordVerification(v.cfg.CameraID, StatusPreconditionFailed)
		return v.latestVerdict, nil
	}
	details.PreconditionPassed = true

	// For a consumer router, the probe tests whether all arbitrary WAN egress is dropped
	blocked := true
	for _, probe := range v.probeMatrixLocked() {
		if v.prober.ExecuteProbe(ctx, probe, false) {
			blocked = false
			details.FailureReason = fmt.Sprintf("traffic to %s succeeded; router block is NOT active", probe.TargetHost)
			break
		}
	}

	var status VerdictStatus
	if blocked {
		status = StatusEnforcedObserved
		details.TestLegPassed = true
		details.CounterIncremented = false
		details.L2Attributed = false
	} else {
		status = StatusNotEnforced
		details.TestLegPassed = false
		details.CounterIncremented = false
		details.L2Attributed = false
	}

	v.latestVerdict = &Verdict{
		Status:           status,
		ControlPath:      ControlPathNone,
		BlockCloudVideo:  false, // a full router block is a different mechanism
		VerifiedAt:       now,
		ExpiresAt:        now.Add(v.cfg.VerdictTTL),
		CameraID:         v.cfg.CameraID,
		CameraMAC:        v.cfg.CameraMAC,
		EnforcementPoint: "consumer_router",
		Details:          details,
	}

	v.metrics.RecordVerification(v.cfg.CameraID, status)
	return v.latestVerdict, nil
}

// GetActiveVerdict returns the current verdict, decaying to UNKNOWN if expired
func (v *Verifier) GetActiveVerdict() *Verdict {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if v.latestVerdict == nil {
		now := time.Now().UTC()
		return &Verdict{
			Status:           StatusUnknown,
			ControlPath:      ControlPathNone,
			BlockCloudVideo:  v.blocking,
			VerifiedAt:       now,
			ExpiresAt:        now,
			CameraID:         v.cfg.CameraID,
			CameraMAC:        v.cfg.CameraMAC,
			EnforcementPoint: v.cfg.EnforcementPoint,
			Details: VerdictDetails{
				FailureReason: "no verification has been executed since startup",
			},
		}
	}

	if v.latestVerdict.IsExpired() {
		// Return copy with status UNKNOWN due to expiry
		decayed := *v.latestVerdict
		decayed.Status = StatusUnknown
		decayed.Details.FailureReason = "verdict expired (perishable decay)"
		return &decayed
	}

	return v.latestVerdict
}
