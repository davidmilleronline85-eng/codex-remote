package codexremote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

type InstallOptions struct {
	InitOptions
	Label       string
	WaitTimeout time.Duration
}

type StartOptions struct {
	InitOptions
	Public      bool
	WaitTimeout time.Duration
}

type InstallResult struct {
	ConfigPath string `json:"config_path"`
	StateDir   string `json:"state_dir"`
	TokenFile  string `json:"token_file"`
	Label      string `json:"label"`
	ListenURL  string `json:"listen_url"`
	ReadyURL   string `json:"ready_url"`
}

type ServiceStatus struct {
	Label     string `json:"label"`
	Installed bool   `json:"installed"`
	Loaded    bool   `json:"loaded"`
	PID       int    `json:"pid,omitempty"`
	PlistPath string `json:"plist_path,omitempty"`
}

type CombinedStatus struct {
	Service ServiceStatus `json:"service"`
	Doctor  DoctorReport  `json:"doctor"`
}

func Install(ctx context.Context, opts InstallOptions) (InstallResult, error) {
	if opts.Label == "" {
		opts.Label = DefaultLabel
	}
	if opts.WaitTimeout == 0 {
		opts.WaitTimeout = 20 * time.Second
	}

	configPath, err := configPathFromOptions(opts.ConfigPath, opts.StateDir)
	if err != nil {
		return InstallResult{}, err
	}

	var cfg Config
	var resolvedConfig string
	if _, statErr := os.Stat(configPath); statErr == nil && !opts.Force {
		cfg, resolvedConfig, err = Load(configPath)
		if err != nil {
			return InstallResult{}, err
		}
	} else {
		initResult, err := Init(opts.InitOptions)
		if err != nil {
			return InstallResult{}, err
		}
		cfg, resolvedConfig, err = Load(initResult.ConfigPath)
		if err != nil {
			return InstallResult{}, err
		}
	}

	binaryPath, err := os.Executable()
	if err != nil {
		return InstallResult{}, fmt.Errorf("resolve current executable: %w", err)
	}
	if _, err := InstallLaunchd(opts.Label, binaryPath, resolvedConfig, cfg.StateDir, true); err != nil {
		return InstallResult{}, err
	}

	waitCtx, cancel := context.WithTimeout(ctx, opts.WaitTimeout)
	defer cancel()
	if err := WaitReady(waitCtx, cfg.Codex.ReadyURL, 2*time.Second); err != nil {
		return InstallResult{}, err
	}

	return InstallResult{
		ConfigPath: resolvedConfig,
		StateDir:   cfg.StateDir,
		TokenFile:  cfg.Codex.TokenFile,
		Label:      opts.Label,
		ListenURL:  cfg.Codex.ListenURL,
		ReadyURL:   cfg.Codex.ReadyURL,
	}, nil
}

func EnsureConfig(opts InitOptions) (Config, string, error) {
	configPath, err := configPathFromOptions(opts.ConfigPath, opts.StateDir)
	if err != nil {
		return Config{}, "", err
	}
	if _, statErr := os.Stat(configPath); statErr == nil && !opts.Force {
		return Load(configPath)
	}
	if _, err := Init(opts); err != nil {
		return Config{}, "", err
	}
	return Load(configPath)
}

func configPathFromOptions(configPath, stateDir string) (string, error) {
	if strings.TrimSpace(configPath) != "" {
		return ResolveConfigPath(configPath)
	}
	return ConfigPathForStateDir(stateDir)
}

func StartForeground(ctx context.Context, opts StartOptions, stdout, stderr io.Writer) error {
	if opts.WaitTimeout == 0 {
		opts.WaitTimeout = 20 * time.Second
	}
	cfg, resolvedConfig, err := EnsureConfig(opts.InitOptions)
	if err != nil {
		return err
	}
	if err := EnsureListenAddressAvailable(cfg.Codex.ListenURL); err != nil {
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

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		runner := Runner{Config: cfg}
		return runner.Run(ctx)
	})

	waitCtx, cancel := context.WithTimeout(ctx, opts.WaitTimeout)
	defer cancel()
	if err := WaitReady(waitCtx, cfg.Codex.ReadyURL, 2*time.Second); err != nil {
		return err
	}

	token, err := ReadToken(resolvedConfig)
	if err != nil {
		return err
	}

	if !opts.Public {
		PrintAgentHandoff(stdout, "Local Codex Remote Ready", cfg.Codex.ListenURL, token)
		<-ctx.Done()
		return g.Wait()
	}

	g.Go(func() error {
		return RunQuickTunnel(ctx, resolvedConfig, stdout, stderr, func(info PublicAccessInfo) {
			PrintAgentHandoff(stdout, "Public Codex Remote Ready", info.WebSocketURL, token)
		})
	})

	return g.Wait()
}

func EnsureListenAddressAvailable(listenURL string) error {
	parsed, err := url.Parse(listenURL)
	if err != nil {
		return fmt.Errorf("parse listen url: %w", err)
	}
	addr := parsed.Host
	if addr == "" {
		return fmt.Errorf("listen url %q did not include a host:port", listenURL)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen address %s is unavailable; stop the existing service or choose another --listen-url", addr)
	}
	_ = ln.Close()
	return nil
}

func StartService(label string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("service management is currently only supported on macOS")
	}
	if label == "" {
		label = DefaultLabel
	}
	target, err := launchAgentPath(label)
	if err != nil {
		return err
	}
	if _, err := os.Stat(target); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("launchd agent not installed at %s; run `codex-remote daemon install` first", target)
		}
		return err
	}
	domain, err := launchdDomain()
	if err != nil {
		return err
	}
	if out, err := exec.Command("launchctl", "bootstrap", domain, target).CombinedOutput(); err != nil {
		if !strings.Contains(string(out), "already bootstrapped") {
			return fmt.Errorf("launchctl bootstrap: %w (%s)", err, strings.TrimSpace(string(out)))
		}
	}
	if out, err := exec.Command("launchctl", "kickstart", "-k", domain+"/"+label).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl kickstart: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func StopService(label string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("service management is currently only supported on macOS")
	}
	if label == "" {
		label = DefaultLabel
	}
	domain, err := launchdDomain()
	if err != nil {
		return err
	}
	out, err := exec.Command("launchctl", "bootout", domain+"/"+label).CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed == "" || strings.Contains(trimmed, "Could not find service") {
			return nil
		}
		return fmt.Errorf("launchctl bootout: %w (%s)", err, trimmed)
	}
	return nil
}

func RestartService(label string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("service management is currently only supported on macOS")
	}
	if label == "" {
		label = DefaultLabel
	}
	domain, err := launchdDomain()
	if err != nil {
		return err
	}
	if out, err := exec.Command("launchctl", "kickstart", "-k", domain+"/"+label).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl kickstart: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func GetServiceStatus(label string) (ServiceStatus, error) {
	if label == "" {
		label = DefaultLabel
	}
	status := ServiceStatus{Label: label}
	if runtime.GOOS != "darwin" {
		return status, nil
	}
	plistPath, err := launchAgentPath(label)
	if err != nil {
		return status, err
	}
	status.PlistPath = plistPath
	if _, err := os.Stat(plistPath); err == nil {
		status.Installed = true
	}
	domain, err := launchdDomain()
	if err != nil {
		return status, err
	}
	out, err := exec.Command("launchctl", "print", domain+"/"+label).CombinedOutput()
	if err != nil {
		return status, nil
	}
	status.Loaded = true
	re := regexp.MustCompile(`\bpid = (\d+)`)
	matches := re.FindStringSubmatch(string(out))
	if len(matches) == 2 {
		fmt.Sscanf(matches[1], "%d", &status.PID)
	}
	return status, nil
}

func WaitReady(ctx context.Context, target string, timeout time.Duration) error {
	for {
		ok, detail := checkHTTP(ctx, target, timeout)
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w (%s)", target, ctx.Err(), detail)
		case <-time.After(1 * time.Second):
		}
	}
}

func ReadToken(configPath string) (string, error) {
	cfg, _, err := Load(configPath)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(cfg.Codex.TokenFile)
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

func Uninstall(configPath, label string, purge bool) error {
	if label == "" {
		label = DefaultLabel
	}
	cfg, _, err := Load(configPath)
	if err != nil {
		if purge && os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if runtime.GOOS == "darwin" {
		if err := UninstallLaunchd(label); err != nil {
			return err
		}
	}
	if purge {
		if err := os.RemoveAll(cfg.StateDir); err != nil {
			return fmt.Errorf("remove state dir: %w", err)
		}
	}
	return nil
}

func StatusJSON(service ServiceStatus, report DoctorReport) ([]byte, error) {
	return json.MarshalIndent(CombinedStatus{Service: service, Doctor: report}, "", "  ")
}

func OriginHTTPURL(listenURL string) (string, error) {
	httpURL, err := DeriveHTTPURL(listenURL, "")
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(httpURL, "/"), nil
}

func DefaultQuickTunnelOrigin(configPath string) (string, error) {
	cfg, _, err := Load(configPath)
	if err != nil {
		return "", err
	}
	return OriginHTTPURL(cfg.Codex.ListenURL)
}

func DefaultStateLogDir(configPath string) (string, error) {
	cfg, _, err := Load(configPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg.StateDir, "logs"), nil
}
