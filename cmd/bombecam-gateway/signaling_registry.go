package main

import (
	"strings"
	"sync"
	"time"

	"github.com/Fever-r/BombeCam/pkg/bridge"
)

// CamSignaling is the live signaling connection of one camera, with the viewer
// publishing it. The registry below is the only place that holds them.
type CamSignaling struct {
	dev    bridge.Device
	sig    *bridge.Signaling
	viewer *bridge.Viewer

	mu            sync.Mutex
	lastAttrQuery time.Time
}

// sigRegistry maps a camera UUID to its live signaling connection. Reading a
// camera's control socket, resolving a camera by name and handing a new control
// channel to every running viewer all go through here, so the map has one
// owner instead of being walked under lock from four files.
var sigRegistry = struct {
	mu sync.RWMutex
	m  map[string]*CamSignaling
}{m: make(map[string]*CamSignaling)}

// registerSig binds the camera to the connection and makes it the live one.
func registerSig(dev bridge.Device, sig *bridge.Signaling, v *bridge.Viewer) {
	if sig != nil {
		sig.BindCamera(dev.UUID, dev.Type)
	}
	sigRegistry.mu.Lock()
	defer sigRegistry.mu.Unlock()
	sigRegistry.m[dev.UUID] = &CamSignaling{dev: dev, sig: sig, viewer: v}
}

func unregisterSig(uuid string) {
	sigRegistry.mu.Lock()
	defer sigRegistry.mu.Unlock()
	delete(sigRegistry.m, uuid)
}

// signalingFor returns the live connection of a camera by exact UUID.
func signalingFor(uuid string) *CamSignaling {
	sigRegistry.mu.RLock()
	defer sigRegistry.mu.RUnlock()
	return sigRegistry.m[uuid]
}

// findSignaling resolves a camera by UUID, then by name, then by a
// case-insensitive fragment of either; an empty reference takes any camera.
// This is the lookup the legacy routes' ?cam= has always used.
func findSignaling(cam string) *CamSignaling {
	sigRegistry.mu.RLock()
	defer sigRegistry.mu.RUnlock()
	if cam == "" {
		for _, cs := range sigRegistry.m {
			return cs
		}
		return nil
	}
	if cs, ok := sigRegistry.m[cam]; ok {
		return cs
	}
	for _, cs := range sigRegistry.m {
		if strings.EqualFold(cs.dev.Name, cam) {
			return cs
		}
	}
	lower := strings.ToLower(cam)
	for uuid, cs := range sigRegistry.m {
		if strings.Contains(strings.ToLower(uuid), lower) || strings.Contains(strings.ToLower(cs.dev.Name), lower) {
			return cs
		}
	}
	return nil
}

// setControlChannelOnSignaling hands a new control channel to every running
// viewer, so the next command from that viewer uses it.
func setControlChannelOnSignaling(cc bridge.ControlChannel) {
	sigRegistry.mu.RLock()
	defer sigRegistry.mu.RUnlock()
	for _, cs := range sigRegistry.m {
		if cs.viewer != nil {
			cs.viewer.SetControlChannel(cc)
		}
	}
}
