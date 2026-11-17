package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Fever-r/BombeCam/pkg/bridge"
	"github.com/Fever-r/BombeCam/pkg/profile"
)

// Signing in again mid-run. When a stream start fails because the Osaio
// sign-in has expired, BombeCam signs in again with the saved login, at most
// once every 10 minutes per login (a wrong password is not retried in a loop).

const renewMinInterval = 10 * time.Minute

var (
	renewMu       sync.Mutex
	renewLast     = map[string]time.Time{}            // by login (email)
	renewedClouds = map[*bridge.Cloud]*bridge.Cloud{} // old sign-in -> its replacement
)

// renewCloudLogin signs the login behind old in again and installs the new
// sign-in on that login's session. It returns the new cloud, or nil if
// nothing changed.
func renewCloudLogin(old *bridge.Cloud) *bridge.Cloud {
	renewMu.Lock()
	if nc := renewedClouds[old]; nc != nil {
		renewMu.Unlock()
		return nc // another camera's stream renewed this sign-in already
	}
	renewMu.Unlock()
	mgr := currentStreamManager()
	sm, email := sessionForCloud(old, mgr)
	if sm == nil || email == "" {
		return nil
	}
	password := savedPassword(email, mgr)
	if password == "" {
		fmt.Printf("[session] the Osaio sign-in for %s seems to have expired; no saved password to sign in again (sign in on the page)\n", email)
		return nil
	}
	renewMu.Lock()
	if t, ok := renewLast[email]; ok && time.Since(t) < renewMinInterval {
		renewMu.Unlock()
		return nil
	}
	renewLast[email] = time.Now()
	renewMu.Unlock()

	fmt.Printf("[session] the Osaio sign-in for %s seems to have expired; signing in again with the saved login\n", email)
	nc := old.Clone()
	nc.GetBaseURL(email)
	if err := nc.Login(email, password); err != nil {
		fmt.Printf("[session] signing in again failed: %v\n", err)
		return nil
	}
	renewMu.Lock()
	renewedClouds[old] = nc
	renewMu.Unlock()
	sm.SetCloud(nc)
	saveLoginToken(email, nc, mgr)
	fmt.Printf("[session] signed in again (UID: %s)\n", nc.UID)
	return nc
}

// forgetRenewAttempts lets every login sign in again at once. A new server
// key or app ID makes the failures that started the 10-minute wait
// meaningless.
func forgetRenewAttempts() {
	renewMu.Lock()
	renewLast = map[string]time.Time{}
	renewMu.Unlock()
}

// sessionForCloud finds the login session that owns c.
func sessionForCloud(c *bridge.Cloud, sm *StreamManager) (*SessionManager, string) {
	if sm != nil {
		if reg := sm.Registry(); reg != nil {
			for _, session := range reg.Sessions() {
				if session.Cloud() == c {
					return session, profile.NormalizeEmail(session.Email())
				}
			}
		}
	}
	return nil, ""
}

// savedPassword returns the stored password for a login ("" if none).
func savedPassword(email string, sm *StreamManager) string {
	pm := sm.ProfileManager()
	if pm == nil {
		return ""
	}
	prof := pm.GetProfile()
	if prof == nil {
		return ""
	}
	if a, ok := prof.FindAccount(email); ok {
		return a.Password.Expose()
	}
	return ""
}

// saveLoginToken keeps a login's new sign-in for the next start.
func saveLoginToken(email string, c *bridge.Cloud, sm *StreamManager) {
	pm := sm.ProfileManager()
	if pm == nil || c == nil {
		return
	}
	email = profile.NormalizeEmail(email)
	_, err := pm.Update(context.Background(), func(p *profile.Profile) error {
		for i, a := range p.Accounts {
			if profile.NormalizeEmail(a.AccountEmail) == email {
				a.VendorUID = c.UID
				a.AuthToken = profile.SecretString(c.APIToken)
				a.TokenExpiresAt = time.Now().UTC().Add(24 * time.Hour)
				a.Region = c.Web
				p.Accounts[i] = a
			}
		}
		return nil
	})
	if err != nil {
		fmt.Printf("[session] could not save the new sign-in: %v\n", err)
	}
}
