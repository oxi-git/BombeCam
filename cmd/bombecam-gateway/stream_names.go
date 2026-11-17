package main

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Fever-r/BombeCam/internal/mediamtx"
	"github.com/Fever-r/BombeCam/pkg/profile"
)

// Friendly stream names: every camera also gets a readable RTSP path such as
// rtsp://<pc>:8554/test_camera. It is fixed when the camera is added (from
// its name at that moment), saved in the profile, and only changes when the
// user edits it on the Cameras tab, so renaming the camera in the Osaio app never
// breaks an NVR. The camera-ID path keeps working: the friendly path is a
// MediaMTX path that relays the camera-ID path on demand.

var (
	streamNameInvalid     = regexp.MustCompile(`[^a-z0-9_]+`)
	streamNameUnderscores = regexp.MustCompile(`_+`)
	streamNameValid       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
	cameraIDLike          = regexp.MustCompile(`^[0-9a-f]{32}$`)
	// MediaMTX path-config keys and Frigate's reserved camera name
	reservedStreamNames = map[string]bool{"all": true, "all_others": true, "birdseye": true}
)

const maxStreamNameLen = 40

// slugStreamName turns a camera name into a stream name: lower-case letters,
// digits and underscores, starting with a letter.
func slugStreamName(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = streamNameInvalid.ReplaceAllString(s, "_")
	s = streamNameUnderscores.ReplaceAllString(s, "_")
	s = strings.Trim(s, "_")
	if s == "" {
		s = "camera"
	}
	if s[0] >= '0' && s[0] <= '9' {
		s = "cam_" + s
	}
	if len(s) > maxStreamNameLen {
		s = strings.TrimRight(s[:maxStreamNameLen], "_")
	}
	return s
}

// validateStreamName checks a stream name typed by the user.
func validateStreamName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("enter a stream name")
	case !streamNameValid.MatchString(name):
		return fmt.Errorf("use lower-case letters, digits and underscores, starting with a letter (at most %d characters)", maxStreamNameLen)
	case reservedStreamNames[name]:
		return fmt.Errorf("%q is reserved; choose another name", name)
	case cameraIDLike.MatchString(name):
		return fmt.Errorf("that looks like a camera ID; choose another name")
	}
	return nil
}

// assignStreamNames gives every camera in p that has no stream name yet a
// unique one based on its name. It reports whether p changed.
func assignStreamNames(p *profile.Profile) bool {
	if p == nil || len(p.Cameras) == 0 {
		return false
	}
	taken := map[string]bool{}
	for id, cam := range p.Cameras {
		taken[id] = true
		if n := cam.StreamConfig.RTSPPath; n != "" {
			taken[n] = true
		}
	}
	// stable order: oldest camera first, then by ID
	ids := make([]string, 0, len(p.Cameras))
	for id := range p.Cameras {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := p.Cameras[ids[i]], p.Cameras[ids[j]]
		if !a.EnrolledAt.Equal(b.EnrolledAt) {
			return a.EnrolledAt.Before(b.EnrolledAt)
		}
		return ids[i] < ids[j]
	})
	changed := false
	for _, id := range ids {
		cam := p.Cameras[id]
		if cam.StreamConfig.RTSPPath != "" {
			continue
		}
		base := slugStreamName(cam.Name)
		if reservedStreamNames[base] || cameraIDLike.MatchString(base) {
			base = "cam_" + base
		}
		name := base
		for i := 2; taken[name]; i++ {
			suffix := fmt.Sprintf("_%d", i)
			trim := base
			if len(trim)+len(suffix) > maxStreamNameLen {
				trim = strings.TrimRight(trim[:maxStreamNameLen-len(suffix)], "_")
			}
			name = trim + suffix
		}
		taken[name] = true
		cam.StreamConfig.RTSPPath = name
		p.Cameras[id] = cam
		changed = true
	}
	return changed
}

// streamNamesOf returns camera ID -> stream name.
func streamNamesOf(p *profile.Profile) map[string]string {
	out := map[string]string{}
	if p == nil {
		return out
	}
	for id, cam := range p.Cameras {
		if cam.StreamConfig.RTSPPath != "" {
			out[id] = cam.StreamConfig.RTSPPath
		}
	}
	return out
}

// syncStreamNames names new cameras in the profile, then hands the names to
// the stream manager.
func syncStreamNames(ctx context.Context, pm profile.ProfileManager, sm *StreamManager) map[string]string {
	if pm == nil || sm == nil {
		return nil
	}
	prof := pm.GetProfile()
	if prof == nil {
		sm.SetStreamNames(nil)
		return nil
	}
	if assignStreamNames(prof.Clone()) {
		if updated, err := pm.Update(ctx, func(p *profile.Profile) error {
			assignStreamNames(p)
			return nil
		}); err == nil && updated != nil {
			prof = updated
		}
	}
	names := streamNamesOf(prof)
	sm.SetStreamNames(names)
	return names
}

// aliasSource is the relay source of a friendly path.
func aliasSource(rtspPort int, cameraID string) string {
	return fmt.Sprintf("rtsp://127.0.0.1:%d/%s", rtspPort, cameraID)
}

// isBombeCamAlias recognises the relay paths BombeCam adds.
func isBombeCamAlias(p mediamtx.ConfigPath) bool {
	if !p.SourceOnDemand || !strings.HasPrefix(p.Source, "rtsp://127.0.0.1:") {
		return false
	}
	rest := strings.TrimPrefix(p.Source, "rtsp://127.0.0.1:")
	i := strings.Index(rest, "/")
	return i > 0 && !strings.Contains(rest[i+1:], "/")
}

// syncMediaAliases makes MediaMTX's friendly paths match names (camera ID ->
// name): missing ones are added, stale ones changed or removed. Paths it did
// not create are left alone.
func syncMediaAliases(apiBase string, rtspPort int, names map[string]string) error {
	have, err := mediamtx.ListConfigPaths(apiBase)
	if err != nil {
		return err
	}
	want := map[string]string{} // name -> source
	for id, n := range names {
		want[n] = aliasSource(rtspPort, id)
	}
	existing := map[string]mediamtx.ConfigPath{}
	for _, p := range have {
		existing[p.Name] = p
	}
	var errs []string
	for n, src := range want {
		p, ok := existing[n]
		switch {
		case !ok:
			if err := mediamtx.AddConfigPath(apiBase, n, mediamtx.ProxyPathConfig(src)); err != nil {
				errs = append(errs, err.Error())
			} else {
				fmt.Printf("[streams] %s is also available as rtsp://<this PC>:%d/%s\n", strings.TrimPrefix(src, fmt.Sprintf("rtsp://127.0.0.1:%d/", rtspPort)), rtspPort, n)
			}
		case !isBombeCamAlias(p):
			errs = append(errs, fmt.Sprintf("stream name %q is already used by your MediaMTX configuration", n))
		case p.Source != src:
			if err := mediamtx.PatchConfigPath(apiBase, n, mediamtx.ProxyPathConfig(src)); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}
	for _, p := range have {
		if _, ok := want[p.Name]; !ok && isBombeCamAlias(p) {
			if err := mediamtx.DeleteConfigPath(apiBase, p.Name); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

var (
	integrationKickCh = make(chan struct{}, 1)
	aliasErrMu        sync.Mutex
	aliasLastErr      string
)

// kickIntegrations asks the integration loop to sync now (names, MediaMTX
// paths, Home Assistant MQTT).
func kickIntegrations() {
	select {
	case integrationKickCh <- struct{}{}:
	default:
	}
}

func lastAliasError() string {
	aliasErrMu.Lock()
	defer aliasErrMu.Unlock()
	return aliasLastErr
}

// runIntegrationLoop keeps stream names and MediaMTX's friendly paths in
// step with the profile: at once when kicked, and every 20 s (which also
// restores the paths after a MediaMTX restart).
func runIntegrationLoop(ctx context.Context, sm *StreamManager) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		names := syncStreamNames(ctx, sm.ProfileManager(), sm)
		if ok, _ := mediaServerHealth(); ok {
			_, apiBase := currentMediaRuntime()
			err := syncMediaAliases(apiBase, sm.RTSPPort(), names)
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			aliasErrMu.Lock()
			if msg != "" && msg != aliasLastErr {
				fmt.Printf("[streams] friendly stream names: %s\n", msg)
			}
			aliasLastErr = msg
			aliasErrMu.Unlock()
		}
		mqttSync(sm)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-integrationKickCh:
		}
	}
}
