package bridge

import (
	shadowshim "github.com/Fever-r/BombeCam/pkg/bridge/shadow_shim"
	"time"
)

// LocalControlStatus separates broker reachability from camera-originated traffic.
// MQTT traffic is evidence from the configured broker, not proof of WAN isolation
// or cryptographic device identity on an anonymous experimental listener.
type LocalControlStatus struct {
	Route           string    `json:"route"`
	BrokerConnected bool      `json:"broker_connected"`
	State           string    `json:"status"`
	LastReport      time.Time `json:"last_report,omitempty"`
	ReportCount     uint64    `json:"report_count"`
	LastError       string    `json:"last_error,omitempty"`
	CloudFallback   bool      `json:"cloud_fallback"`
}

const LocalReportMaxAge = 30 * time.Second

func (m *MQTTControlChannel) AttachBroker(b *MosquittoBridge) {
	m.mu.Lock()
	m.broker = b
	m.mu.Unlock()
}

func (m *MQTTControlChannel) LocalStatus(uuid string) LocalControlStatus {
	m.mu.RLock()
	b, closed := m.broker, m.closed
	m.mu.RUnlock()
	s := LocalControlStatus{Route: "local_mqtt", State: "broker_unavailable"}
	if b != nil && !closed {
		s = b.Status(uuid)
	}
	return s
}

// ReportedShadow never exposes desired state as an observed camera value.
func (m *MQTTControlChannel) ReportedShadow(uuid string) (*shadowshim.ShadowDocument, error) {
	doc, err := m.daemon.StateMachine().FreshReportedShadow(uuid, LocalReportMaxAge)
	if err != nil {
		return nil, err
	}
	doc.State.Desired = nil
	doc.State.Delta = nil
	doc.Metadata.Desired = nil
	for key := range doc.State.Reported {
		meta, ok := doc.Metadata.Reported[key]
		if !ok || time.Since(time.Unix(meta.Timestamp, 0)) > LocalReportMaxAge {
			delete(doc.State.Reported, key)
			delete(doc.Metadata.Reported, key)
		}
	}
	m.mu.RLock()
	b := m.broker
	m.mu.RUnlock()
	if b != nil && b.Status(uuid).State != "camera_report_received" {
		doc.State.Reported = map[string]any{}
		doc.Metadata.Reported = map[string]shadowshim.FieldMetadata{}
	}
	return doc, nil
}

// LocalControl is a control channel that answers from the local firewall
// backend (its broker) instead of the vendor cloud. Callers detect it by
// capability, not by concrete type, so another local backend can be added
// without hunting down every type switch.
type LocalControl interface {
	LocalStatus(uuid string) LocalControlStatus
	ReportedShadow(uuid string) (*shadowshim.ShadowDocument, error)
}

// AsLocalControl returns the channel as a local backend, or false when it
// reads the cameras through the vendor cloud.
func AsLocalControl(cc ControlChannel) (LocalControl, bool) {
	local, ok := cc.(LocalControl)
	return local, ok && local != nil
}
