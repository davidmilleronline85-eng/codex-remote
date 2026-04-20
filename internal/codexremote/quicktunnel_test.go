package codexremote

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
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
	if !strings.Contains(out, "Keep this `codex-remote start` process running") {
		t.Fatalf("expected keep-running message in output, got %q", out)
	}
}
