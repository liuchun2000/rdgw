package rdgw

import (
	"context"
	"crypto/tls"
	"testing"
)

func TestSetPeerTLSConfigUsesSystemRootsAndCopiesConfig(t *testing.T) {
	s := New(nil, DefaultErrorWriter)
	cfg := &tls.Config{}
	if err := s.SetPeerTLSConfig(cfg); err != nil {
		t.Fatalf("SetPeerTLSConfig returned error: %v", err)
	}
	cfg.MinVersion = tls.VersionTLS13
	got := s.peerTLS()
	if got.MinVersion != tls.VersionTLS12 {
		t.Fatalf("peer TLS minimum version = %v, want TLS 1.2", got.MinVersion)
	}
	if got.RootCAs != nil {
		t.Fatal("expected nil RootCAs to preserve system certificate pool")
	}
}

func TestSetPeerTLSConfigRejectsWeakConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  *tls.Config
	}{
		{name: "nil", cfg: nil},
		{name: "insecure", cfg: &tls.Config{InsecureSkipVerify: true}},
		{name: "tls11", cfg: &tls.Config{MinVersion: tls.VersionTLS11}},
		{name: "max-tls11", cfg: &tls.Config{MaxVersion: tls.VersionTLS11}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(nil, DefaultErrorWriter)
			if err := s.SetPeerTLSConfig(tt.cfg); err == nil {
				t.Fatal("expected weak peer TLS config to be rejected")
			}
		})
	}
}

func TestSetPeerTLSConfigMustPrecedeAddPeer(t *testing.T) {
	s := New(nil, DefaultErrorWriter)
	s.peerStarted = true
	if err := s.SetPeerTLSConfig(&tls.Config{}); err == nil {
		t.Fatal("expected TLS config update after AddPeer to fail")
	}
}

func TestPeerStartRejectsMalformedURL(t *testing.T) {
	s := New(nil, DefaultErrorWriter)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	(&peer{url: "://bad"}).start(ctx, s)
	(&peer{url: "ws:///connect"}).start(ctx, s)
}

func TestPeerTLSConfigSetsServerName(t *testing.T) {
	cfg, err := peerTLSConfig(&tls.Config{}, "wss://peer.example.test:8443/connect")
	if err != nil {
		t.Fatalf("peerTLSConfig returned error: %v", err)
	}
	if cfg.ServerName != "peer.example.test" {
		t.Fatalf("ServerName = %q, want peer.example.test", cfg.ServerName)
	}
	if _, err := peerTLSConfig(&tls.Config{}, "wss:///connect"); err == nil {
		t.Fatal("expected malformed peer URL to fail")
	}
	for _, rawURL := range []string{
		"http://peer.example.test/connect",
		"wss://user:pass@peer.example.test/connect",
		"wss://peer.example.test:0/connect",
		"wss://peer.example.test:65536/connect",
	} {
		if _, err := peerTLSConfig(&tls.Config{}, rawURL); err == nil {
			t.Errorf("peerTLSConfig(%q) unexpectedly succeeded", rawURL)
		}
	}
}

func TestAddPeerNoopDoesNotLockTLSSetter(t *testing.T) {
	s := New(nil, DefaultErrorWriter)
	// Peer 配置缺失时 AddPeer 是 no-op，不应阻止后续补充 TLS 配置。
	s.AddPeer("ws://peer.example.test/connect", "peer", "secret")
	if err := s.SetPeerTLSConfig(&tls.Config{}); err != nil {
		t.Fatalf("no-op peer setup should not lock TLS setter: %v", err)
	}
}
