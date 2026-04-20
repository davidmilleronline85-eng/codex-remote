package codexremote

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

const (
	DefaultHTTPListenURL = "http://127.0.0.1:8787"
	defaultHTTPSandbox   = "workspace-write"
	maxJSONBodyBytes     = 1 << 20
)

type HTTPServeOptions struct {
	InitOptions
	ListenURL   string
	Public      bool
	WaitTimeout time.Duration
}

type HTTPExecRequest struct {
	Prompt            string   `json:"prompt"`
	CWD               string   `json:"cwd,omitempty"`
	Model             string   `json:"model,omitempty"`
	Sandbox           string   `json:"sandbox,omitempty"`
	SkipGitRepoCheck  bool     `json:"skip_git_repo_check,omitempty"`
	FullAuto          bool     `json:"full_auto,omitempty"`
	DangerouslyBypass bool     `json:"dangerously_bypass_approvals_and_sandbox,omitempty"`
	Ephemeral         bool     `json:"ephemeral,omitempty"`
	AddDirs           []string `json:"add_dirs,omitempty"`
}

type HTTPResumeRequest struct {
	Prompt            string `json:"prompt"`
	Model             string `json:"model,omitempty"`
	SkipGitRepoCheck  bool   `json:"skip_git_repo_check,omitempty"`
	FullAuto          bool   `json:"full_auto,omitempty"`
	DangerouslyBypass bool   `json:"dangerously_bypass_approvals_and_sandbox,omitempty"`
	Ephemeral         bool   `json:"ephemeral,omitempty"`
}

type HTTPExecResponse struct {
	ThreadID string            `json:"thread_id"`
	Message  string            `json:"message,omitempty"`
	Usage    json.RawMessage   `json:"usage,omitempty"`
	Events   []json.RawMessage `json:"events,omitempty"`
	Logs     []string          `json:"logs,omitempty"`
}

type HTTPErrorResponse struct {
	Error   string            `json:"error"`
	Details string            `json:"details,omitempty"`
	Logs    []string          `json:"logs,omitempty"`
	Events  []json.RawMessage `json:"events,omitempty"`
}

type HTTPServerConfig struct {
	CodexPath  string
	Token      string
	DefaultCWD string
}

type HTTPServer struct {
	codexPath  string
	token      string
	defaultCWD string
	threadMu   sync.Map
}

func StartHTTPForeground(ctx context.Context, opts HTTPServeOptions, stdout, stderr io.Writer) error {
	if opts.WaitTimeout == 0 {
		opts.WaitTimeout = 20 * time.Second
	}
	if strings.TrimSpace(opts.ListenURL) == "" {
		opts.ListenURL = DefaultHTTPListenURL
	}

	cfg, resolvedConfig, err := EnsureConfig(opts.InitOptions)
	if err != nil {
		return err
	}
	if err := EnsureListenAddressAvailable(opts.ListenURL); err != nil {
		return err
	}
	if _, err := resolveExecutable(cfg.Codex.Path); err != nil {
		return fmt.Errorf("codex CLI was not found; install Codex and log in first: %w", err)
	}
	if opts.Public {
		if _, err := resolveExecutable(cfg.Cloudflared.Path); err != nil {
			return fmt.Errorf("cloudflared was not found; rerun the installer or use --public=false: %w", err)
		}
	}

	token, err := ReadToken(resolvedConfig)
	if err != nil {
		return err
	}
	defaultCWD, err := os.Getwd()
	if err != nil {
		defaultCWD = cfg.StateDir
	}

	originURL, err := OriginHTTPURL(opts.ListenURL)
	if err != nil {
		return err
	}
	readyURL, err := DeriveHTTPURL(opts.ListenURL, "/readyz")
	if err != nil {
		return err
	}

	api := NewHTTPServer(HTTPServerConfig{
		CodexPath:  cfg.Codex.Path,
		Token:      token,
		DefaultCWD: defaultCWD,
	})
	httpServer, listener, err := api.Server(ctx, opts.ListenURL)
	if err != nil {
		return err
	}

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	})
	g.Go(func() error {
		err := httpServer.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})

	waitCtx, cancel := context.WithTimeout(ctx, opts.WaitTimeout)
	defer cancel()
	if err := WaitReady(waitCtx, readyURL, 2*time.Second); err != nil {
		return err
	}

	if !opts.Public {
		PrintHTTPAgentHandoff(stdout, "Local Codex Remote HTTP Ready", originURL, token, defaultCWD)
		<-ctx.Done()
		return g.Wait()
	}

	g.Go(func() error {
		return RunQuickTunnelOrigin(ctx, cfg.Cloudflared.Path, originURL, stdout, stderr, func(info PublicAccessInfo) {
			PrintHTTPAgentHandoff(stdout, "Public Codex Remote HTTP Ready", info.HTTPSURL, token, defaultCWD)
		})
	})

	return g.Wait()
}

func NewHTTPServer(cfg HTTPServerConfig) *HTTPServer {
	return &HTTPServer{
		codexPath:  cfg.CodexPath,
		token:      cfg.Token,
		defaultCWD: cfg.DefaultCWD,
	}
}

func (s *HTTPServer) Server(ctx context.Context, listenURL string) (*http.Server, net.Listener, error) {
	parsed, err := url.Parse(listenURL)
	if err != nil {
		return nil, nil, fmt.Errorf("parse http listen url: %w", err)
	}
	if parsed.Scheme != "http" {
		return nil, nil, fmt.Errorf("http listen url must use http, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, nil, fmt.Errorf("http listen url %q did not include a host:port", listenURL)
	}

	listener, err := net.Listen("tcp", parsed.Host)
	if err != nil {
		return nil, nil, err
	}

	server := &http.Server{
		Handler: s.Handler(),
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}
	return server, listener, nil
}

func (s *HTTPServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"name":         "codex-remote-http",
			"readyz":       "/readyz",
			"healthz":      "/healthz",
			"createThread": "POST /v1/threads",
			"resumeThread": "POST /v1/threads/{thread_id}/turns",
		})
	})
	mux.Handle("POST /v1/threads", s.requireAuth(http.HandlerFunc(s.handleCreateThread)))
	mux.Handle("POST /v1/threads/{threadID}/turns", s.requireAuth(http.HandlerFunc(s.handleResumeThread)))
	return mux
}

func (s *HTTPServer) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := extractBearerToken(r.Header.Get("Authorization"))
		if token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="codex-remote"`)
			writeJSON(w, http.StatusUnauthorized, HTTPErrorResponse{Error: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *HTTPServer) handleCreateThread(w http.ResponseWriter, r *http.Request) {
	var req HTTPExecRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, HTTPErrorResponse{Error: "invalid_request", Details: err.Error()})
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeJSON(w, http.StatusBadRequest, HTTPErrorResponse{Error: "invalid_request", Details: "prompt is required"})
		return
	}

	result, err := s.runCreateThread(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, HTTPErrorResponse{
			Error:   "codex_exec_failed",
			Details: err.Error(),
			Logs:    result.Logs,
			Events:  result.Events,
		})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *HTTPServer) handleResumeThread(w http.ResponseWriter, r *http.Request) {
	threadID := strings.TrimSpace(r.PathValue("threadID"))
	if threadID == "" {
		writeJSON(w, http.StatusBadRequest, HTTPErrorResponse{Error: "invalid_request", Details: "threadID is required"})
		return
	}

	var req HTTPResumeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, HTTPErrorResponse{Error: "invalid_request", Details: err.Error()})
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeJSON(w, http.StatusBadRequest, HTTPErrorResponse{Error: "invalid_request", Details: "prompt is required"})
		return
	}

	lock := s.threadLock(threadID)
	lock.Lock()
	defer lock.Unlock()

	result, err := s.runResumeThread(r.Context(), threadID, req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, HTTPErrorResponse{
			Error:   "codex_exec_failed",
			Details: err.Error(),
			Logs:    result.Logs,
			Events:  result.Events,
		})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *HTTPServer) runCreateThread(ctx context.Context, req HTTPExecRequest) (HTTPExecResponse, error) {
	args, cwd := buildCreateThreadArgs(req, s.defaultCWD)
	return s.runCodexJSON(ctx, args, cwd)
}

func (s *HTTPServer) runResumeThread(ctx context.Context, threadID string, req HTTPResumeRequest) (HTTPExecResponse, error) {
	args := buildResumeThreadArgs(threadID, req)
	return s.runCodexJSON(ctx, args, "")
}

func (s *HTTPServer) runCodexJSON(ctx context.Context, args []string, cwd string) (HTTPExecResponse, error) {
	resolvedCodex, err := resolveExecutable(s.codexPath)
	if err != nil {
		return HTTPExecResponse{}, err
	}

	cmd := exec.CommandContext(ctx, resolvedCodex, args...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	output, err := cmd.CombinedOutput()
	result, parseErr := parseCodexJSONOutput(output)
	if parseErr != nil {
		return result, parseErr
	}
	if err != nil {
		return result, fmt.Errorf("%w (%s)", err, strings.Join(result.Logs, "; "))
	}
	if result.ThreadID == "" {
		return result, fmt.Errorf("codex did not emit a thread id")
	}
	return result, nil
}

func buildCreateThreadArgs(req HTTPExecRequest, defaultCWD string) ([]string, string) {
	args := []string{"exec", "--json"}
	if req.Model != "" {
		args = append(args, "-m", req.Model)
	}

	sandbox := req.Sandbox
	if sandbox == "" {
		sandbox = defaultHTTPSandbox
	}
	if sandbox != "" {
		args = append(args, "-s", sandbox)
	}

	cwd := strings.TrimSpace(req.CWD)
	if cwd == "" {
		cwd = defaultCWD
	}
	if cwd != "" {
		args = append(args, "-C", cwd)
	}
	for _, addDir := range req.AddDirs {
		if trimmed := strings.TrimSpace(addDir); trimmed != "" {
			args = append(args, "--add-dir", trimmed)
		}
	}
	if req.SkipGitRepoCheck {
		args = append(args, "--skip-git-repo-check")
	}
	if req.FullAuto {
		args = append(args, "--full-auto")
	}
	if req.DangerouslyBypass {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	}
	if req.Ephemeral {
		args = append(args, "--ephemeral")
	}
	args = append(args, req.Prompt)
	return args, cwd
}

func buildResumeThreadArgs(threadID string, req HTTPResumeRequest) []string {
	args := []string{"exec", "resume", "--json"}
	if req.Model != "" {
		args = append(args, "-m", req.Model)
	}
	if req.SkipGitRepoCheck {
		args = append(args, "--skip-git-repo-check")
	}
	if req.FullAuto {
		args = append(args, "--full-auto")
	}
	if req.DangerouslyBypass {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	}
	if req.Ephemeral {
		args = append(args, "--ephemeral")
	}
	args = append(args, threadID, req.Prompt)
	return args
}

func parseCodexJSONOutput(output []byte) (HTTPExecResponse, error) {
	var result HTTPExecResponse
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "{") {
			result.Logs = append(result.Logs, line)
			continue
		}

		raw := json.RawMessage(append([]byte(nil), line...))
		var event map[string]any
		if err := json.Unmarshal(raw, &event); err != nil {
			result.Logs = append(result.Logs, line)
			continue
		}
		result.Events = append(result.Events, raw)

		switch eventType := stringValue(event["type"]); eventType {
		case "thread.started":
			if threadID := stringValue(event["thread_id"]); threadID != "" {
				result.ThreadID = threadID
			}
		case "item.completed":
			item, _ := event["item"].(map[string]any)
			if stringValue(item["type"]) == "agent_message" {
				result.Message = stringValue(item["text"])
			}
		case "turn.completed":
			if usage, ok := event["usage"]; ok {
				data, err := json.Marshal(usage)
				if err == nil {
					result.Usage = data
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return result, err
	}
	return result, nil
}

func stringValue(v any) string {
	s, _ := v.(string)
	return s
}

func extractBearerToken(header string) string {
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxJSONBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if decoder.More() {
		return fmt.Errorf("request body must contain a single JSON object")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

func (s *HTTPServer) threadLock(threadID string) *sync.Mutex {
	lock, _ := s.threadMu.LoadOrStore(threadID, &sync.Mutex{})
	return lock.(*sync.Mutex)
}
