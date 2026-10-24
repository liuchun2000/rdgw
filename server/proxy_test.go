package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type blockingReadCloser struct {
	closed chan struct{}
	once   sync.Once
}

func (b *blockingReadCloser) Read([]byte) (int, error) {
	<-b.closed
	return 0, errors.New("closed")
}

func (b *blockingReadCloser) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

func TestIdleTimeoutBodyClosesStalledResponse(t *testing.T) {
	body := &blockingReadCloser{closed: make(chan struct{})}
	wrapped := &idleTimeoutBody{ReadCloser: body, timeout: 20 * time.Millisecond}
	_, err := wrapped.Read(make([]byte, 1))
	if err == nil || !strings.Contains(err.Error(), "idle timeout") {
		t.Fatalf("Read error = %v, want idle timeout", err)
	}
	select {
	case <-body.closed:
	default:
		t.Fatal("underlying response body was not closed")
	}
}

func TestIdleTimeoutBodyDisabled(t *testing.T) {
	body := io.NopCloser(strings.NewReader("ok"))
	wrapped := &idleTimeoutBody{ReadCloser: body}
	buf := make([]byte, 2)
	n, err := wrapped.Read(buf)
	if err != nil || n != 2 || string(buf) != "ok" {
		t.Fatalf("Read = (%d, %q, %v), want (2, %q, nil)", n, buf, err, "ok")
	}
}

func TestTransportDialerUsesRequestAddress(t *testing.T) {
	var gotNetwork, gotAddress string
	transport := newTransport(func(_ context.Context, network, address string) (net.Conn, error) {
		gotNetwork, gotAddress = network, address
		return nil, errors.New("stop")
	}, proxyConfig{})
	if transport.TLSClientConfig.ServerName != "" {
		t.Fatalf("shared transport must derive TLS server name from request URL")
	}
	_, err := transport.DialContext(context.Background(), "tcp", "service.internal:443")
	if err == nil || gotNetwork != "tcp" || gotAddress != "service.internal:443" {
		t.Fatalf("DialContext = network %q address %q error %v", gotNetwork, gotAddress, err)
	}
}

// 回归：%2F 等编码路径必须原样转发到上游，不能被解码后丢失语义
func TestTargetPathPreservesEscaping(t *testing.T) {
	cases := map[string]string{
		"/client/foo/https/example.com/api/v1/a%2Fb":     "/api/v1/a%2Fb",
		"/client/foo/https/example.com/x%3Fy/z":          "/x%3Fy/z",
		"/client/foo/https/example.com":                  "/",
		"/client/foo/wss/services.test/exec%20container": "/exec%20container",
	}
	for raw, want := range cases {
		req := httptest.NewRequest(http.MethodGet, raw, nil)
		got, err := targetPath(req)
		if err != nil {
			t.Fatalf("targetPath(%q) error: %v", raw, err)
		}
		if got != want {
			t.Fatalf("targetPath(%q) = %q, want %q", raw, got, want)
		}
	}
	// 模拟真实转发场景：Path 是解码值、RawPath 保留编码语义（如 %2F）
	req := &http.Request{Method: http.MethodGet, URL: &url.URL{Path: "/client/foo/https/host/a/b", RawPath: "/client/foo/https/host/a%2Fb"}}
	got, err := targetPath(req)
	if err != nil {
		t.Fatalf("targetPath error: %v", err)
	}
	if got != "/a%2Fb" {
		t.Fatalf("targetPath = %q, want /a%%2Fb", got)
	}
}

// 回归：IPv6 裸地址缺端口时 Host authority 必须带方括号
func TestNormalizeTargetIPv6Authority(t *testing.T) {
	_, _, authority, err := normalizeTarget("http", "::1")
	if err != nil {
		t.Fatalf("normalizeTarget error: %v", err)
	}
	if authority != "[::1]" {
		t.Fatalf("authority = %q, want [::1]", authority)
	}
	_, _, authority, err = normalizeTarget("https", "[2001:db8::1]:8443")
	if err != nil {
		t.Fatalf("normalizeTarget error: %v", err)
	}
	if authority != "[2001:db8::1]:8443" {
		t.Fatalf("authority = %q, want [2001:db8::1]:8443", authority)
	}
}

func TestNormalizeTargetRejectsInvalidHostCharacters(t *testing.T) {
	for _, host := range []string{"a/b", `a\\b`, "user@host", "host?query", "host#fragment", "host%2fpath", "host\tname"} {
		if _, _, _, err := normalizeTarget("http", host); err == nil {
			t.Fatalf("normalizeTarget(%q) accepted invalid host", host)
		}
	}
	if _, _, _, err := normalizeTarget("http", "service.internal"); err != nil {
		t.Fatalf("normalizeTarget valid host failed: %v", err)
	}
}

// 回归：countFlag 必须同时识别 -x 和 --x 形式，且 -- 后停止扫描
func TestCountFlagDoubleDash(t *testing.T) {
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	os.Args = []string{"prog", "--listen", ":1", "-listen=:2", "--", "--listen"}
	if countFlag("listen") != 2 {
		t.Fatalf("countFlag = %d, want 2", countFlag("listen"))
	}
}

// 保证字符串处理函数满足调用假设（避免未来重构静默破坏 SOCKS5 域名校验）
func TestDomainValidation(t *testing.T) {
	invalid := []string{"", strings.Repeat("a", 254), "bad\x00host", "bad\rhost", "bad\nhost"}
	for _, d := range invalid {
		if validDomain(d) {
			t.Fatalf("domain %q should be invalid", d)
		}
	}
	if !validDomain("example.com") {
		t.Fatalf("example.com should be valid")
	}
}
