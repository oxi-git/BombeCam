package main

import (
	"context"
	"encoding/json"
	"github.com/Fever-r/BombeCam/pkg/profile"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func unknownCountryEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{"OSAIO_COUNTRY", "LC_ALL", "LC_MESSAGES", "LANG"} {
		t.Setenv(key, "")
	}
}

func TestHostCountryDoesNotGuessUnknownOrLanguageOnlyLocale(t *testing.T) {
	for _, tc := range []struct{ locale, want string }{{"", ""}, {"C", ""}, {"fr", ""}, {"en", ""}, {"zz_ZZ.UTF-8", ""}, {"en_GB.UTF-8", "44"}, {"fr_CA.UTF-8", "1"}} {
		t.Run(tc.locale, func(t *testing.T) {
			unknownCountryEnvironment(t)
			t.Setenv("LANG", tc.locale)
			if got := detectHostCountry(); got != tc.want {
				t.Fatalf("locale %q: %q != %q", tc.locale, got, tc.want)
			}
		})
	}
	unknownCountryEnvironment(t)
	t.Setenv("OSAIO_COUNTRY", "garbage")
	if detectHostCountry() != "" {
		t.Fatal("invalid environment country accepted")
	}
	t.Setenv("OSAIO_COUNTRY", "+44")
	if detectHostCountry() != "44" {
		t.Fatal("valid explicit environment country not normalized")
	}
}

func TestAccountCountryUsesMatchingAccountAndExplicitCorrection(t *testing.T) {
	unknownCountryEnvironment(t)
	sm := NewSessionManager("1", "fixture", testServerKey)
	p := &profile.Profile{}
	p.Credentials = profile.CloudCredentials{AccountEmail: "a@example.test", Country: "44"}
	p.UpsertAccount(profile.CloudCredentials{AccountEmail: "b@example.test", Country: "39"})
	for _, tc := range []struct{ explicit, email, want string }{{"", "a@example.test", "44"}, {"", "b@example.test", "39"}, {"+81", "a@example.test", "81"}} {
		got, err := resolveAccountCountry(tc.explicit, tc.email, p, sm)
		if err != nil || got != tc.want {
			t.Fatalf("%+v: %q %v", tc, got, err)
		}
	}
	if _, err := resolveAccountCountry("", "new@example.test", p, sm); err == nil {
		t.Fatal("new account inherited primary's country")
	}
	for _, bad := range []string{"0", "01", "US", "1234", "1;anything"} {
		if _, err := resolveAccountCountry(bad, "a@example.test", p, sm); err == nil {
			t.Fatalf("invalid code accepted: %q", bad)
		}
	}
}

func TestUnknownCountryStopsSetupAndSearchBeforeVendorAuthentication(t *testing.T) {
	unknownCountryEnvironment(t)
	sm, stm, pm, _, mux, _ := setupFullTestEnvironment(t)
	sm.SetCountry("")
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "must not contact vendor", 500)
	}))
	defer srv.Close()
	sm.Cloud().GlobalBase = srv.URL
	ensureTestRegistry(stm, sm, "", "fixture", testServerKey)
	defer stm.SetSessionRegistry(nil)
	for _, request := range []struct{ path, body string }{{"/api/v1/onboarding/setup", `{"account_email":"a@example.test","password":"synthetic","force":true}`}, {"/api/v1/cameras/search", `{"email":"a@example.test","password":"synthetic"}`}} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest("POST", request.path, strings.NewReader(request.body)))
		if rr.Code != 400 || !strings.Contains(rr.Body.String(), "country_required") {
			t.Fatalf("unknown country: %d %s", rr.Code, rr.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unknown country sent credentials to vendor")
	}
	if exists, _ := pm.HasProfile(context.Background()); exists {
		t.Fatal("unknown country saved a profile")
	}
	_ = stm
}

func TestCountryCorrectionAuthenticatesOnceAndPersistsNormalizedCode(t *testing.T) {
	unknownCountryEnvironment(t)
	sm, _, pm, _, mux, _ := setupFullTestEnvironment(t)
	sm.SetCountry("")
	logins := 0
	srv, url := setupMockCloudServer(t, &logins, nil)
	defer srv.Close()
	sm.Cloud().GlobalBase = url
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/onboarding/setup", strings.NewReader(`{"account_email":"a@example.test","password":"synthetic","country":"+44","force":true}`)))
	if rr.Code != 200 || logins != 1 {
		t.Fatalf("corrected setup: %d %s logins=%d", rr.Code, rr.Body.String(), logins)
	}
	saved, err := pm.Load(context.Background())
	if err != nil || len(saved.Accounts) != 1 || saved.Accounts[0].Country != "44" {
		t.Fatalf("saved country: %+v %v", saved, err)
	}
	raw, _ := json.Marshal(saved.RedactSecrets())
	var public struct {
		Accounts []map[string]any `json:"accounts"`
	}
	_ = json.Unmarshal(raw, &public)
	if len(public.Accounts) != 1 || public.Accounts[0]["country"] != "44" {
		t.Fatalf("correction unavailable in public view: %s", raw)
	}
}
