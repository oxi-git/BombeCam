package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	shadowshim "github.com/Fever-r/BombeCam/pkg/bridge/shadow_shim"
)

// MQTTControlChannel implements ControlChannel using the AWS IoT Shadow Shim
// running on local MQTTS port 8883 with Invariant I4 monotonic readback confirmation.
type MQTTControlChannel struct {
	mu              sync.RWMutex
	broker          *MosquittoBridge
	daemon          *shadowshim.ShadowShimDaemon
	timeout         time.Duration
	requireReadback bool
	ownsDaemon      bool
	stopCh          chan struct{}
	closed          bool
	ptzCancel       map[string]chan struct{}
}

// Ensure MQTTControlChannel implements ControlChannel interface at compile time.
var _ ControlChannel = (*MQTTControlChannel)(nil)

// NewMQTTControlChannel creates a new local MQTT shadow control channel.
// If daemon is nil, a new in-process shadow shim daemon is created and started.
func NewMQTTControlChannel(daemon *shadowshim.ShadowShimDaemon, timeout time.Duration) *MQTTControlChannel {
	if timeout <= 0 {
		timeout = 2000 * time.Millisecond
	}

	owns := false
	if daemon == nil {
		daemon = shadowshim.NewDaemon(shadowshim.Config{DefaultTimeout: timeout})
		_ = daemon.Start(context.Background())
		owns = true
	}

	ch := &MQTTControlChannel{
		daemon:          daemon,
		timeout:         timeout,
		requireReadback: true,
		ownsDaemon:      owns,
		stopCh:          make(chan struct{}),
		ptzCancel:       make(map[string]chan struct{}),
	}

	return ch
}

// Daemon returns the underlying ShadowShimDaemon.
func (m *MQTTControlChannel) Daemon() *shadowshim.ShadowShimDaemon {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.daemon
}

// MovePTZ pulses the pan/tilt stepper motors (dir: 1=left, 2=right, 3=up, 4=down, 0=stop).
// If durationMs > 0 and direction != 0, it schedules a stop command (direction=0) after durationMs.
func (m *MQTTControlChannel) MovePTZ(ctx context.Context, cameraUUID string, direction int, durationMs int) error {
	if direction < 0 || direction > 4 || durationMs < 0 || durationMs > 2000 {
		return fmt.Errorf("invalid PTZ direction or duration")
	}
	if err := m.ensureBroker(); err != nil {
		return err
	}
	if direction != 0 && durationMs == 0 {
		durationMs = 250
	}
	m.mu.Lock()
	if prev := m.ptzCancel[cameraUUID]; prev != nil {
		close(prev)
		delete(m.ptzCancel, cameraUUID)
	}
	d, timeout, require, stopCh := m.daemon, m.timeout, m.requireReadback, m.stopCh
	cancelCh := make(chan struct{})
	m.ptzCancel[cameraUUID] = cancelCh
	m.mu.Unlock()
	if direction != 0 {
		go func() {
			timer := time.NewTimer(time.Duration(durationMs) * time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
				m.mu.Lock()
				if !m.closed && m.ptzCancel[cameraUUID] == cancelCh {
					delete(m.ptzCancel, cameraUUID)
					_ = d.DispatchDesired(context.Background(), cameraUUID, map[string]any{"direction": 0})
				}
				m.mu.Unlock()
			case <-cancelCh:
			case <-stopCh:
			}
		}()
	}
	desired := map[string]any{"direction": direction}
	var err error
	if require {
		_, err = d.DispatchAndConfirm(ctx, cameraUUID, desired, desired, timeout)
	} else {
		err = d.DispatchDesired(ctx, cameraUUID, desired)
	}
	if err != nil {
		m.mu.Lock()
		if m.ptzCancel[cameraUUID] == cancelCh {
			close(cancelCh)
			delete(m.ptzCancel, cameraUUID)
			_ = d.DispatchDesired(context.Background(), cameraUUID, map[string]any{"direction": 0})
		}
		m.mu.Unlock()
		return fmt.Errorf("local PTZ unconfirmed: %w", err)
	}
	return nil
}

// SetAttribute sends a local shadow command and requires a fresh matching report.
// Attribute names are the retained experimental mapping until validated on hardware.
func (m *MQTTControlChannel) SetAttribute(ctx context.Context, uuid, key string, value any) error {
	return m.setAttributes(ctx, uuid, map[string]any{key: value}, map[string]any{key: value})
}

func (m *MQTTControlChannel) ensureBroker() error {
	m.mu.RLock()
	closed, b := m.closed, m.broker
	m.mu.RUnlock()
	if closed {
		return fmt.Errorf("local control channel is closed")
	}
	if b != nil && !b.IsConnected() {
		return fmt.Errorf("local broker is unavailable; no cloud fallback")
	}
	return nil
}

func (m *MQTTControlChannel) setAttributes(ctx context.Context, uuid string, desired, expected map[string]any) error {
	if err := m.ensureBroker(); err != nil {
		return err
	}
	m.mu.RLock()
	timeout, require, d := m.timeout, m.requireReadback, m.daemon
	m.mu.RUnlock()
	if !require {
		return d.DispatchDesired(ctx, uuid, desired)
	}
	res, err := d.DispatchAndConfirm(ctx, uuid, desired, expected, timeout)
	if err != nil {
		// Withdraw failed desired commands so a later connection does not replay them.
		clear := make(map[string]any)
		for k := range desired {
			clear[k] = nil
		}
		_ = d.DispatchDesired(context.Background(), uuid, clear)
		fmt.Printf("[local-control] command unconfirmed thing=%s: %v\n", uuid, err)
		return err
	}
	fmt.Printf("[local-control] command confirmed thing=%s latency=%s version=%d\n", uuid, res.Duration, res.Version)
	return nil
}

func (m *MQTTControlChannel) SetIR(ctx context.Context, uuid string, mode int) error {
	return m.SetAttribute(ctx, uuid, "IrLedMode", mode)
}
func (m *MQTTControlChannel) SetLED(ctx context.Context, uuid string, on bool) error {
	n := 0
	if on {
		n = 1
	}
	return m.SetAttribute(ctx, uuid, "LedOnOff", n)
}
func (m *MQTTControlChannel) SetLight(ctx context.Context, uuid string, on bool) error {
	n := 0
	if on {
		n = 1
	}
	return m.SetAttribute(ctx, uuid, "LightSW", n)
}
func (m *MQTTControlChannel) ArmTalk(ctx context.Context, uuid string, enable bool, sessionID string) error {
	n := 0
	if enable {
		n = 1
	}
	return m.setAttributes(ctx, uuid, map[string]any{"TalkEnable": n, "SessionId": sessionID}, map[string]any{"TalkEnable": n})
}
func (m *MQTTControlChannel) reportedInt(uuid, key string) (int, error) {
	if err := m.ensureBroker(); err != nil {
		return 0, err
	}
	doc, err := m.ReportedShadow(uuid)
	if err != nil {
		return 0, err
	}
	v, ok := doc.State.Reported[key]
	if !ok {
		return 0, fmt.Errorf("no fresh local camera report for %s", key)
	}
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		return int(n), nil
	case json.Number:
		i, err := n.Int64()
		return int(i), err
	case bool:
		if n {
			return 1, nil
		}
		return 0, nil
	}
	return 0, fmt.Errorf("invalid camera report for %s", key)
}
func (m *MQTTControlChannel) GetIR(ctx context.Context, uuid string) (int, error) {
	return m.reportedInt(uuid, "IrLedMode")
}
func (m *MQTTControlChannel) GetLED(ctx context.Context, uuid string) (bool, error) {
	n, err := m.reportedInt(uuid, "LedOnOff")
	return n == 1, err
}
func (m *MQTTControlChannel) GetLight(ctx context.Context, uuid string) (bool, error) {
	n, err := m.reportedInt(uuid, "LightSW")
	return n == 1, err
}

// Close releases resources and closes the daemon if owned.
func (m *MQTTControlChannel) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil
	}
	m.closed = true
	close(m.stopCh)

	// Cancel and cleanup all active PTZ timers
	for uuid, cancelCh := range m.ptzCancel {
		close(cancelCh)
		delete(m.ptzCancel, uuid)
	}

	if m.ownsDaemon && m.daemon != nil {
		return m.daemon.Close()
	}
	return nil
}
