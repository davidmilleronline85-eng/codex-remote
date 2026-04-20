package codexremote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
)

type Runner struct {
	Config Config
	Logger *stdlog.Logger
}

func (r Runner) Run(ctx context.Context) error {
	cfg := r.Config
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return err
	}
	runtimeCfg, err := cfg.Runtime()
	if err != nil {
		return err
	}
	logger := r.logger()

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return superviseProcess(ctx, processSpec{
			Name:         "codex",
			Path:         cfg.Codex.Path,
			Args:         buildCodexArgs(cfg),
			Env:          cfg.Codex.Env,
			HealthURL:    cfg.Codex.ReadyURL,
			Runtime:      runtimeCfg,
			StdoutWriter: newPrefixedWriter(os.Stdout, "[codex] "),
			StderrWriter: newPrefixedWriter(os.Stderr, "[codex] "),
			Logger:       logger,
		})
	})
	if cfg.Cloudflared.Enabled {
		g.Go(func() error {
			if err := waitForHTTP(ctx, cfg.Codex.ReadyURL, runtimeCfg.HealthCheckTimeout, logger); err != nil {
				return err
			}
			return superviseProcess(ctx, processSpec{
				Name:         "cloudflared",
				Path:         cfg.Cloudflared.Path,
				Args:         buildCloudflaredArgs(cfg),
				Env:          cfg.Cloudflared.Env,
				Runtime:      runtimeCfg,
				StdoutWriter: newPrefixedWriter(os.Stdout, "[cloudflared] "),
				StderrWriter: newPrefixedWriter(os.Stderr, "[cloudflared] "),
				Logger:       logger,
			})
		})
	}
	return g.Wait()
}

type processSpec struct {
	Name         string
	Path         string
	Args         []string
	Env          map[string]string
	HealthURL    string
	Runtime      RuntimeConfig
	StdoutWriter io.Writer
	StderrWriter io.Writer
	Logger       *stdlog.Logger
}

func superviseProcess(ctx context.Context, spec processSpec) error {
	resolvedPath, err := resolveExecutable(spec.Path)
	if err != nil {
		return fmt.Errorf("%s executable: %w", spec.Name, err)
	}

	backoff := spec.Runtime.RestartInitialBackoff
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return nil
		}

		cmd := exec.CommandContext(ctx, resolvedPath, spec.Args...)
		cmd.Env = append(os.Environ(), flattenEnv(spec.Env)...)
		cmd.Stdout = spec.StdoutWriter
		cmd.Stderr = spec.StderrWriter
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

		spec.Logger.Printf("%s: starting attempt %d: %s %s", spec.Name, attempt, resolvedPath, strings.Join(spec.Args, " "))
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("%s start: %w", spec.Name, err)
		}

		exitCh := make(chan error, 1)
		go func() {
			exitCh <- cmd.Wait()
		}()

		restartCh := make(chan struct{}, 1)
		healthCtx, cancelHealth := context.WithCancel(ctx)
		if spec.HealthURL != "" {
			go monitorHealth(healthCtx, spec, restartCh)
		}

		var exitErr error
		needsRestart := false

		select {
		case <-ctx.Done():
			cancelHealth()
			stopProcess(cmd, exitCh, spec.Runtime.ShutdownGracePeriod, spec.Logger, spec.Name)
			return nil
		case <-restartCh:
			cancelHealth()
			spec.Logger.Printf("%s: health checks failed, restarting", spec.Name)
			stopProcess(cmd, exitCh, spec.Runtime.ShutdownGracePeriod, spec.Logger, spec.Name)
			needsRestart = true
		case exitErr = <-exitCh:
			cancelHealth()
			if ctx.Err() != nil {
				return nil
			}
			needsRestart = true
			if exitErr == nil {
				spec.Logger.Printf("%s: exited cleanly, restarting", spec.Name)
			} else {
				spec.Logger.Printf("%s: exited with error: %v", spec.Name, exitErr)
			}
		}

		if !needsRestart || ctx.Err() != nil {
			return nil
		}

		spec.Logger.Printf("%s: waiting %s before restart", spec.Name, backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > spec.Runtime.RestartMaxBackoff {
			backoff = spec.Runtime.RestartMaxBackoff
		}
	}
}

func buildCodexArgs(cfg Config) []string {
	args := []string{
		"app-server",
		"--listen", cfg.Codex.ListenURL,
		"--ws-auth", "capability-token",
		"--ws-token-file", cfg.Codex.TokenFile,
	}
	args = append(args, cfg.Codex.ExtraArgs...)
	return args
}

func buildCloudflaredArgs(cfg Config) []string {
	args := []string{"tunnel"}
	if cfg.Cloudflared.ConfigFile != "" {
		args = append(args, "--config", cfg.Cloudflared.ConfigFile)
	}
	args = append(args, "run", cfg.Cloudflared.TunnelName)
	args = append(args, cfg.Cloudflared.ExtraArgs...)
	return args
}

func waitForHTTP(ctx context.Context, target string, timeout time.Duration, logger *stdlog.Logger) error {
	for {
		ok, detail := checkHTTP(ctx, target, timeout)
		if ok {
			logger.Printf("dependency ready: %s (%s)", target, detail)
			return nil
		}
		logger.Printf("waiting for dependency: %s (%s)", target, detail)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func monitorHealth(ctx context.Context, spec processSpec, restartCh chan<- struct{}) {
	failures := 0
	ticker := time.NewTicker(spec.Runtime.HealthCheckInterval)
	defer ticker.Stop()

	for {
		ok, detail := checkHTTP(ctx, spec.HealthURL, spec.Runtime.HealthCheckTimeout)
		if ok {
			failures = 0
		} else {
			failures++
			spec.Logger.Printf("%s: health check failed (%d/%d): %s", spec.Name, failures, spec.Runtime.ReadyFailuresBeforeRestart, detail)
			if failures >= spec.Runtime.ReadyFailuresBeforeRestart {
				select {
				case restartCh <- struct{}{}:
				default:
				}
				return
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func stopProcess(cmd *exec.Cmd, exitCh <-chan error, grace time.Duration, logger *stdlog.Logger, name string) {
	if cmd.Process == nil {
		return
	}
	_ = signalProcessGroup(cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-exitCh:
		return
	case <-time.After(grace):
		logger.Printf("%s: graceful shutdown timed out after %s, sending SIGKILL", name, grace)
		_ = signalProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
		<-exitCh
	}
}

func signalProcessGroup(pid int, signal syscall.Signal) error {
	err := syscall.Kill(-pid, signal)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func flattenEnv(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, fmt.Sprintf("%s=%s", k, v))
	}
	return out
}

func (r Runner) logger() *stdlog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return stdlog.New(os.Stderr, "codex-remote: ", stdlog.LstdFlags)
}

type prefixedWriter struct {
	mu        sync.Mutex
	target    io.Writer
	prefix    string
	lineStart bool
}

func newPrefixedWriter(target io.Writer, prefix string) *prefixedWriter {
	return &prefixedWriter{
		target:    target,
		prefix:    prefix,
		lineStart: true,
	}
}

func (w *prefixedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	inputLen := len(p)
	for len(p) > 0 {
		if w.lineStart {
			if _, err := io.WriteString(w.target, w.prefix); err != nil {
				return 0, err
			}
			w.lineStart = false
		}

		idx := bytes.IndexByte(p, '\n')
		if idx == -1 {
			if _, err := w.target.Write(p); err != nil {
				return 0, err
			}
			return inputLen, nil
		}

		chunk := p[:idx+1]
		if _, err := w.target.Write(chunk); err != nil {
			return 0, err
		}
		w.lineStart = true
		p = p[idx+1:]
	}
	return inputLen, nil
}
