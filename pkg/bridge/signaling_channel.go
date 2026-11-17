package bridge

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// SignalingProvider resolves a *Signaling connection for a given camera UUID.
type SignalingProvider func(cameraUUID string) (*Signaling, error)

// SignalingControlChannel implements ControlChannel using vendor cloud signaling WebSockets (fallback).
type SignalingControlChannel struct {
	mu        sync.RWMutex
	sig       *Signaling
	provider  SignalingProvider
	stopCh    chan struct{}
	closed    bool
	ptzCancel map[string]chan struct{}
}

// Ensure SignalingControlChannel implements ControlChannel interface at compile time.
var _ ControlChannel = (*SignalingControlChannel)(nil)

// NewSignalingControlChannel creates a ControlChannel with a single Signaling instance.
func NewSignalingControlChannel(sig *Signaling) *SignalingControlChannel {
	return &SignalingControlChannel{
		sig:       sig,
		stopCh:    make(chan struct{}),
		ptzCancel: make(map[string]chan struct{}),
	}
}

// NewSignalingControlChannelWithProvider creates a ControlChannel with a dynamic Signaling provider.
func NewSignalingControlChannelWithProvider(provider SignalingProvider) *SignalingControlChannel {
	return &SignalingControlChannel{
		provider:  provider,
		stopCh:    make(chan struct{}),
		ptzCancel: make(map[string]chan struct{}),
	}
}

func (s *SignalingControlChannel) getSig(cameraUUID string) (*Signaling, error) {
	s.mu.RLock()
	closed, sig, provider := s.closed, s.sig, s.provider
	s.mu.RUnlock()

	if closed {
		return nil, fmt.Errorf("signaling channel is closed")
	}

	if sig != nil {
		return sig, nil
	}

	if provider != nil {
		return provider(cameraUUID)
	}

	return nil, fmt.Errorf("no signaling connection available for %s", cameraUUID)
}

// MovePTZ pulses the pan/tilt stepper motors.
func (s *SignalingControlChannel) MovePTZ(ctx context.Context, cameraUUID string, direction int, durationMs int) error {
	sig, err := s.getSig(cameraUUID)
	if err != nil {
		return err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("signaling channel is closed")
	}

	// Preemption & Explicit Stop Rule:
	if prevCancel, ok := s.ptzCancel[cameraUUID]; ok {
		close(prevCancel)
		delete(s.ptzCancel, cameraUUID)
	}

	var cancelCh chan struct{}
	if direction != 0 && durationMs > 0 {
		cancelCh = make(chan struct{})
		s.ptzCancel[cameraUUID] = cancelCh
	}
	stopCh := s.stopCh
	s.mu.Unlock()

	if err := sig.Send("service.MovePTZ", map[string]any{"direction": direction}); err != nil {
		if cancelCh != nil {
			s.mu.Lock()
			if s.ptzCancel[cameraUUID] == cancelCh {
				delete(s.ptzCancel, cameraUUID)
			}
			s.mu.Unlock()
		}
		return fmt.Errorf("move ptz failed: %w", err)
	}

	if cancelCh != nil {
		select {
		case <-cancelCh:
			return nil
		case <-stopCh:
			return nil
		default:
		}

		go func(ch chan struct{}) {
			timer := time.NewTimer(time.Duration(durationMs) * time.Millisecond)
			defer timer.Stop()

			select {
			case <-timer.C:
				s.mu.Lock()
				if !s.closed && s.ptzCancel[cameraUUID] == ch {
					delete(s.ptzCancel, cameraUUID)
					_ = sig.Send("service.MovePTZ", map[string]any{"direction": 0})
				}
				s.mu.Unlock()
			case <-ch:
				return
			case <-stopCh:
				return
			}
		}(cancelCh)
	}

	return nil
}

// SetIR sets infrared illuminator mode (see IRModeAuto/IRModeOn/IRModeOff).
func (s *SignalingControlChannel) SetIR(ctx context.Context, cameraUUID string, mode int) error {
	sig, err := s.getSig(cameraUUID)
	if err != nil {
		return err
	}

	payload := map[string]any{
		"IrLedMode": mode,
		"LightSW":   0,
	}

	if err := sig.Send("atr.set", payload); err != nil {
		return fmt.Errorf("set ir failed: %w", err)
	}

	return nil
}

// GetIR queries current infrared mode.
func (s *SignalingControlChannel) GetIR(ctx context.Context, cameraUUID string) (int, error) {
	values, err := s.GetAttributes(ctx, cameraUUID, "IrLedMode")
	if err != nil {
		return 0, err
	}
	v, ok := values["IrLedMode"]
	n, valid := ParseCameraAttributeNumber(v)
	if !ok || !valid || n < 0 || n > 2 {
		return 0, fmt.Errorf("invalid or absent IrLedMode camera readback")
	}
	return n, nil
}

// SetLED controls physical indicator LED.
func (s *SignalingControlChannel) SetLED(ctx context.Context, cameraUUID string, on bool) error {
	sig, err := s.getSig(cameraUUID)
	if err != nil {
		return err
	}

	ledVal := 0
	if on {
		ledVal = 1
	}

	if err := sig.Send("atr.set", map[string]any{"LedOnOff": ledVal}); err != nil {
		return fmt.Errorf("set led failed: %w", err)
	}

	return nil
}

// GetLED queries physical indicator LED status.
func (s *SignalingControlChannel) GetLED(ctx context.Context, cameraUUID string) (bool, error) {
	values, err := s.GetAttributes(ctx, cameraUUID, "LedOnOff")
	if err != nil {
		return false, err
	}
	v, ok := values["LedOnOff"]
	n, valid := ParseCameraAttributeNumber(v)
	if !ok || !valid || n < 0 || n > 1 {
		return false, fmt.Errorf("invalid or absent LedOnOff camera readback")
	}
	return n == 1, nil
}

// SetLight controls physical spotlight.
func (s *SignalingControlChannel) SetLight(ctx context.Context, cameraUUID string, on bool) error {
	sig, err := s.getSig(cameraUUID)
	if err != nil {
		return err
	}

	lightVal := 0
	if on {
		lightVal = 1
	}

	payload := map[string]any{
		"LightSW":      lightVal,
		"WhiteLightSw": lightVal,
	}

	if err := sig.Send("atr.set", payload); err != nil {
		return fmt.Errorf("set light failed: %w", err)
	}

	return nil
}

// GetLight queries physical spotlight status.
func (s *SignalingControlChannel) GetLight(ctx context.Context, cameraUUID string) (bool, error) {
	values, err := s.GetAttributes(ctx, cameraUUID, "LightSW", "WhiteLightSw")
	if err != nil {
		return false, err
	}
	v, ok := values["LightSW"]
	if !ok {
		v, ok = values["WhiteLightSw"]
	}
	n, valid := ParseCameraAttributeNumber(v)
	if !ok || !valid || n < 0 || n > 1 {
		return false, fmt.Errorf("invalid or absent LightSW camera readback")
	}
	return n == 1, nil
}

// ArmTalk arms or disarms camera speaker for talkback audio.
func (s *SignalingControlChannel) ArmTalk(ctx context.Context, cameraUUID string, enable bool, sessionID string) error {
	sig, err := s.getSig(cameraUUID)
	if err != nil {
		return err
	}

	payload := map[string]any{
		"SessionId":     sessionID,
		"EnableSpeaker": enable,
		"EnableMic":     enable,
	}

	if err := sig.Send("service.Talk", payload); err != nil {
		return fmt.Errorf("arm talk failed: %w", err)
	}

	return nil
}

// SetAttribute sets one camera setting by its attribute name.
func (s *SignalingControlChannel) SetAttribute(ctx context.Context, cameraUUID, key string, value any) error {
	sig, err := s.getSig(cameraUUID)
	if err != nil {
		return err
	}
	if err := sig.Send("atr.set", map[string]any{key: value}); err != nil {
		return fmt.Errorf("set %s failed: %w", key, err)
	}
	return nil
}

// Close releases resources.
func (s *SignalingControlChannel) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true
	close(s.stopCh)

	for uuid, cancelCh := range s.ptzCancel {
		close(cancelCh)
		delete(s.ptzCancel, uuid)
	}

	if s.sig != nil {
		return s.sig.Close()
	}
	return nil
}

// GetAttributes waits for a correlated camera reply; it never returns sent values.
func (s *SignalingControlChannel) GetAttributes(ctx context.Context, camera string, keys ...string) (map[string]any, error) {
	sig, err := s.getSig(camera)
	if err != nil {
		return nil, err
	}
	return sig.QueryAttributes(ctx, camera, keys...)
}

// GetSDCardInfo returns the camera's card state: Status, TotalSizeK, UsedSizeK.
func (s *SignalingControlChannel) GetSDCardInfo(ctx context.Context, cameraUUID string) (map[string]any, error) {
	return s.atrGet(ctx, cameraUUID, "SdCardInfo")
}

// GetRecordDays returns curMonth, curMonDays, preMonth, preMonDays, the day
// lists comma-separated.
func (s *SignalingControlChannel) GetRecordDays(ctx context.Context, cameraUUID string) (map[string]any, error) {
	return s.atrGet(ctx, cameraUUID, "RecordDays")
}

// GetLocalVideoTime lists the recordings of one day on the card. start and
// duration are in seconds from midnight; the camera answers with chunk start
// times and lengths, the recording list the Osaio app shows.
//
// Evidence: yripc/websocket/WebSocketHandlerKt.java:16,40 (the message and its
// response), YRPlayerComponentViewModel.java:618-623 (payload), :641-646
// (parse). The reply is response.GetLocalVideoTimeResp, an envelope different
// from atr.get, so it is asked and unwrapped on its own.
func (s *SignalingControlChannel) GetLocalVideoTime(ctx context.Context, cameraUUID string, start, duration int) (map[string]any, error) {
	sig, err := s.getSig(cameraUUID)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{"Start": start, "Duration": duration, "Order": 0, "Limited": 0}
	reply, err := sig.QueryCommand(ctx, cameraUUID, "service.GetLocalVideoTime", payload)
	if err != nil {
		return nil, fmt.Errorf("get local video time failed: %w", err)
	}
	body, ok := reply.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("local video time reply is not an object: %T", reply)
	}
	// Unwrap the response envelope; leave an already-flat reply alone.
	if resp, ok := body["response.GetLocalVideoTimeResp"].(map[string]any); ok {
		if data, ok := resp["data"].(map[string]any); ok {
			return data, nil
		}
		return resp, nil
	}
	if data, ok := body["data"].(map[string]any); ok {
		return data, nil
	}
	return body, nil
}

// atrGet asks for one key the way the Osaio app does: atr.get with the key
// names as the payload (WebSocketHandler.getAllSettingAttribute lists
// SdCardInfo among them, and WebSocketApi.getAtr sends the cmd object as-is).
func (s *SignalingControlChannel) atrGet(ctx context.Context, cameraUUID, cmd string) (map[string]any, error) {
	sig, err := s.getSig(cameraUUID)
	if err != nil {
		return nil, err
	}
	reply, err := sig.QueryCommand(ctx, cameraUUID, "atr.get", []string{cmd})
	if err != nil {
		return nil, fmt.Errorf("get %s failed: %w", cmd, err)
	}
	body, ok := reply.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s reply is not an object: %T", cmd, reply)
	}
	// The camera answers {cmd: {...}}, while callers want the values.
	if nested, ok := body[cmd].(map[string]any); ok {
		return nested, nil
	}
	return body, nil
}

func ParseCameraAttributeNumber(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case float64:
		if n >= 0 && n <= 2 && n == float64(int(n)) {
			return int(n), true
		}
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case string:
		switch n {
		case "0":
			return 0, true
		case "1":
			return 1, true
		case "2":
			return 2, true
		}
	}
	return 0, false
}
