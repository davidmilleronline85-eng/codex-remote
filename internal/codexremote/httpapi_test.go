package codexremote

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHTTPServerCreateThread(t *testing.T) {
	t.Parallel()

	fakeCodex := writeFakeCodex(t)
	defaultCWD := t.TempDir()
	server := NewHTTPServer(HTTPServerConfig{
		CodexPath:  fakeCodex,
		Token:      "secret-token",
		DefaultCWD: defaultCWD,
	})

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	body := `{"prompt":"Reply with exactly: OK","skip_git_repo_check":true}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/threads", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create thread request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %s", resp.Status)
	}

	var payload HTTPExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.ThreadID != "thread-new" {
		t.Fatalf("unexpected thread id: %q", payload.ThreadID)
	}
	if payload.Message != "OK" {
		t.Fatalf("unexpected message: %q", payload.Message)
	}
	if len(payload.Events) == 0 {
		t.Fatalf("expected events in response")
	}
}

func TestHTTPServerResumeThreadRequiresAuth(t *testing.T) {
	t.Parallel()

	server := NewHTTPServer(HTTPServerConfig{
		CodexPath:  "/bin/sh",
		Token:      "secret-token",
		DefaultCWD: t.TempDir(),
	})

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/threads/thread-1/turns", strings.NewReader(`{"prompt":"hello"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("resume thread request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unexpected status: %s", resp.Status)
	}
}

func TestPrintHTTPAgentHandoff(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	PrintHTTPAgentHandoff(&buf, "HTTP Ready", "https://example.trycloudflare.com", "tok", "/tmp/worktree")
	out := buf.String()
	if !strings.Contains(out, "BEGIN_HTTP_AGENT_HANDOFF") {
		t.Fatalf("missing handoff header: %q", out)
	}
	if !strings.Contains(out, "POST /v1/threads") {
		t.Fatalf("missing create thread endpoint: %q", out)
	}
	if !strings.Contains(out, "CODEX_REMOTE_HTTP_URL=https://example.trycloudflare.com") {
		t.Fatalf("missing base URL: %q", out)
	}
}

func writeFakeCodex(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	script := `#!/bin/sh
if [ "$1" = "exec" ] && [ "$2" = "--json" ]; then
  echo '{"type":"thread.started","thread_id":"thread-new"}'
  echo '{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"OK"}}'
  echo '{"type":"turn.completed","usage":{"output_tokens":1}}'
  exit 0
fi
if [ "$1" = "exec" ] && [ "$2" = "resume" ] && [ "$3" = "--json" ]; then
  echo '{"type":"thread.started","thread_id":"'"$4"'"}'
  echo '{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"NEXT"}}'
  echo '{"type":"turn.completed","usage":{"output_tokens":1}}'
  exit 0
fi
echo 'unexpected args:' "$@" >&2
exit 1
`
	if runtime.GOOS == "windows" {
		t.Fatalf("tests require a POSIX shell")
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	return path
}
