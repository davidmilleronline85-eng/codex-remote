package codexremote

import (
	"strings"
	"testing"
)

func TestDeriveHTTPURL(t *testing.T) {
	t.Parallel()

	got, err := DeriveHTTPURL("ws://127.0.0.1:8765", "/readyz")
	if err != nil {
		t.Fatalf("DeriveHTTPURL returned error: %v", err)
	}
	if got != "http://127.0.0.1:8765/readyz" {
		t.Fatalf("unexpected derived url: %s", got)
	}
}

func TestRenderLaunchdPlist(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("skipping launchd render test in short mode")
	}

	plist, err := RenderLaunchdPlist(DefaultLabel, "/tmp/codex-remote", "/tmp/config.yaml", "/tmp/codex-remote-state")
	if err != nil && strings.Contains(err.Error(), "darwin") {
		t.Skip("launchd only supported on darwin")
	}
	if err != nil {
		t.Fatalf("RenderLaunchdPlist returned error: %v", err)
	}
	if !strings.Contains(plist, "<string>run</string>") {
		t.Fatalf("expected plist to contain run argument, got: %s", plist)
	}
}
