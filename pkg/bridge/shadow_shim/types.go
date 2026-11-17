// Package shadowshim implements a lightweight AWS IoT Device Shadow engine
// and daemon with Invariant I4 monotonic readback confirmation.
package shadowshim

import (
	"fmt"
	"time"
)

// Standard AWS IoT Shadow Topic Templates
const (
	TopicShadowUpdate         = "$aws/things/%s/shadow/update"
	TopicShadowUpdateAccepted = "$aws/things/%s/shadow/update/accepted"
	TopicShadowUpdateRejected = "$aws/things/%s/shadow/update/rejected"
	TopicShadowUpdateDelta    = "$aws/things/%s/shadow/update/delta"
	TopicShadowGetAccepted    = "$aws/things/%s/shadow/get/accepted"
	TopicShadowGetRejected    = "$aws/things/%s/shadow/get/rejected"
)

// FieldMetadata tracks attribute update timestamps.
type FieldMetadata struct {
	Timestamp int64 `json:"timestamp"`
}

// ShadowState holds desired, reported, and delta state dictionaries.
type ShadowState struct {
	Desired  map[string]any `json:"desired,omitempty"`
	Reported map[string]any `json:"reported,omitempty"`
	Delta    map[string]any `json:"delta,omitempty"`
}

// ShadowMetadata tracks timestamp metadata per field.
type ShadowMetadata struct {
	Desired  map[string]FieldMetadata `json:"desired,omitempty"`
	Reported map[string]FieldMetadata `json:"reported,omitempty"`
}

// ShadowDocument represents the full AWS IoT Shadow Document.
type ShadowDocument struct {
	State       ShadowState    `json:"state"`
	Metadata    ShadowMetadata `json:"metadata"`
	Timestamp   int64          `json:"timestamp"`
	Version     int64          `json:"version"`
	ClientToken string         `json:"clientToken,omitempty"`
}

// ShadowUpdateRequest represents an incoming shadow update payload.
type ShadowUpdateRequest struct {
	State       ShadowState `json:"state"`
	ClientToken string      `json:"clientToken,omitempty"`
	Version     int64       `json:"version,omitempty"`
	Timestamp   int64       `json:"timestamp,omitempty"`
}

// ShadowDeltaMessage represents a delta event dispatched to the device.
type ShadowDeltaMessage struct {
	State     map[string]any           `json:"state"`
	Metadata  map[string]FieldMetadata `json:"metadata"`
	Timestamp int64                    `json:"timestamp"`
	Version   int64                    `json:"version"`
}

// ConfirmationResult captures successful Invariant I4 confirmation details.
type ConfirmationResult struct {
	ThingName            string         `json:"thing_name"`
	DispatchTime         time.Time      `json:"dispatch_time"`
	ConfirmationTime     time.Time      `json:"confirmation_time"`
	Duration             time.Duration  `json:"duration_ms"`
	ConfirmedState       map[string]any `json:"confirmed_state"`
	ReportedTimestampSec int64          `json:"reported_timestamp_sec"`
	Version              int64          `json:"version"`
}

// ErrReadbackTimeout indicates that the camera failed to confirm reported state within deadline.
type ErrReadbackTimeout struct {
	ThingName    string
	DispatchTime time.Time
	Timeout      time.Duration
	Expected     map[string]any
	LastReported map[string]any
}

func (e *ErrReadbackTimeout) Error() string {
	return fmt.Sprintf("readback confirmation timeout for thing '%s' after %v: expected %v, last reported %v (dispatched at %s)",
		e.ThingName, e.Timeout, e.Expected, e.LastReported, e.DispatchTime.Format(time.RFC3339Nano))
}

// TopicMessage holds a routed message payload with its topic.
type TopicMessage struct {
	Topic     string
	Payload   []byte
	Timestamp time.Time
}
