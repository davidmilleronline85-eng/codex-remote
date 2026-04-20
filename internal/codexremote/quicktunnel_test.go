package codexremote

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestWaitForPublicTunnelReady(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var stdout bytes.Buffer
	var got PublicAccessInfo
	waitForPublicTunnelReady(ctx, &stdout, server.URL, "wss://example.trycloudflare.com", func(info PublicAccessInfo) {
		got = info
	})

	if got.HTTPSURL != server.URL {
		t.Fatalf("got HTTPS URL %q, want %q", got.HTTPSURL, server.URL)
	}
	if got.WebSocketURL != "wss://example.trycloudflare.com" {
		t.Fatalf("got WebSocket URL %q", got.WebSocketURL)
	}

	out := stdout.String()
	if !strings.Contains(out, "Quick tunnel ready") {
		t.Fatalf("expected ready message in output, got %q", out)
	}
	if !strings.Contains(out, "Keep the original `codex-remote` process running") {
		t.Fatalf("expected keep-running message in output, got %q", out)
	}
}

func TestCheckHTTPViaPublicDNSDirectDial(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	port := parsed.Port()
	hostURL := fmt.Sprintf("http://example.trycloudflare.com:%s", port)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ok, detail := checkHTTPViaIPs(ctx, hostURL+"/readyz", 2*time.Second, []string{"127.0.0.1"})
	if !ok {
		t.Fatalf("expected success, got %q", detail)
	}
	if !strings.Contains(detail, "200 OK") {
		t.Fatalf("expected 200 OK detail, got %q", detail)
	}
}
