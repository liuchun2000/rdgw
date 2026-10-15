package main

import (
	"net/http"
	"testing"
)

func TestValidateListenAddressAllowsWildcard(t *testing.T) {
	for _, address := range []string{":8123", "0.0.0.0:8123", "[::]:8123", "127.0.0.1:8123"} {
		if err := validateListenAddress(address); err != nil {
			t.Errorf("validateListenAddress(%q) = %v", address, err)
		}
	}
}

func TestValidateListenAddressRejectsInvalidPort(t *testing.T) {
	for _, address := range []string{":0", ":65536", "127.0.0.1:bad", "127.0.0.1"} {
		if err := validateListenAddress(address); err == nil {
			t.Errorf("validateListenAddress(%q) unexpectedly succeeded", address)
		}
	}
}

func TestNormalizeTarget(t *testing.T) {
	tests := []struct {
		scheme, host, wantAddr string
		ok                     bool
	}{
		{"http", "example.test", "example.test:80", true},
		{"https", "example.test:8443", "example.test:8443", true},
		{"wss", "[2001:db8::1]:443", "[2001:db8::1]:443", true},
		{"tcp", "example.test", "", false},
		{"http", "example.test:0", "", false},
		{"http", "example.test:bad", "", false},
		{"http", "example.test:80:1", "", false},
	}
	for _, tt := range tests {
		_, addr, _, err := normalizeTarget(tt.scheme, tt.host)
		if tt.ok && (err != nil || addr != tt.wantAddr) {
			t.Errorf("normalizeTarget(%q,%q) = %q, %v", tt.scheme, tt.host, addr, err)
		}
		if !tt.ok && err == nil {
			t.Errorf("normalizeTarget(%q,%q) unexpectedly succeeded", tt.scheme, tt.host)
		}
	}
}

func TestNormalizeTargetPreservesExplicitHTTPPort(t *testing.T) {
	host, addr, authority, err := normalizeTarget("https", "example.test:8443")
	if err != nil {
		t.Fatal(err)
	}
	if host != "example.test" || addr != "example.test:8443" || authority != "example.test:8443" {
		t.Fatalf("got host=%q addr=%q authority=%q", host, addr, authority)
	}
}

func TestAuthStoreTokenAndOriginSnapshot(t *testing.T) {
	store := &authStore{authFile: "configured"}
	store.snapshot = authSnapshot{tokens: map[string][]string{"foo": {"secret"}}, origins: map[string]struct{}{"https://console.test": {}}}
	req, _ := http.NewRequest(http.MethodGet, "http://server/connect", nil)
	req.Header.Set("X-Tunnel-ID", "foo")
	req.Header.Set("X-Tunnel-Token", "secret")
	if _, ok, _ := store.authorize(req); !ok {
		t.Fatal("valid token rejected")
	}
	req.Header.Set("X-Tunnel-Token", "wrong")
	if _, ok, _ := store.authorize(req); ok {
		t.Fatal("invalid token accepted")
	}
	req.Header.Set("X-Tunnel-Token", "secret")
	req.Header.Set("Origin", "https://evil.test")
	if _, ok, _ := store.authorize(req); ok {
		t.Fatal("unapproved origin accepted")
	}
}
