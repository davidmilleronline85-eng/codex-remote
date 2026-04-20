package codexremote

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	DefaultLabel = "com.emergent.codex-remote"
)

type Config struct {
	StateDir    string            `yaml:"state_dir"`
	Codex       CodexConfig       `yaml:"codex"`
	Cloudflared CloudflaredConfig `yaml:"cloudflared"`
	Supervisor  SupervisorConfig  `yaml:"supervisor"`
}

type CodexConfig struct {
	Path      string            `yaml:"path"`
	ListenURL string            `yaml:"listen_url"`
	ReadyURL  string            `yaml:"ready_url"`
	HealthURL string            `yaml:"health_url"`
	TokenFile string            `yaml:"token_file"`
	ExtraArgs []string          `yaml:"extra_args,omitempty"`
	Env       map[string]string `yaml:"env,omitempty"`
}

type CloudflaredConfig struct {
	Enabled    bool              `yaml:"enabled"`
	Path       string            `yaml:"path,omitempty"`
	ConfigFile string            `yaml:"config_file,omitempty"`
	TunnelName string            `yaml:"tunnel_name,omitempty"`
	ExtraArgs  []string          `yaml:"extra_args,omitempty"`
	Env        map[string]string `yaml:"env,omitempty"`
}

type SupervisorConfig struct {
	HealthCheckInterval        string `yaml:"health_check_interval"`
	HealthCheckTimeout         string `yaml:"health_check_timeout"`
	ReadyFailuresBeforeRestart int    `yaml:"ready_failures_before_restart"`
	RestartInitialBackoff      string `yaml:"restart_initial_backoff"`
	RestartMaxBackoff          string `yaml:"restart_max_backoff"`
	ShutdownGracePeriod        string `yaml:"shutdown_grace_period"`
}

type RuntimeConfig struct {
	HealthCheckInterval        time.Duration
	HealthCheckTimeout         time.Duration
	RestartInitialBackoff      time.Duration
	RestartMaxBackoff          time.Duration
	ShutdownGracePeriod        time.Duration
	ReadyFailuresBeforeRestart int
}

type InitOptions struct {
	ConfigPath        string
	StateDir          string
	CodexPath         string
	ListenURL         string
	Force             bool
	EnableCloudflared bool
	CloudflaredPath   string
	CloudflaredConfig string
	CloudflaredTunnel string
}

type InitResult struct {
	ConfigPath   string
	StateDir     string
	TokenFile    string
	LaunchdPlist string
}

func DefaultStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "codex-remote"), nil
	}
	return filepath.Join(home, ".local", "share", "codex-remote"), nil
}

func DefaultConfigPath() (string, error) {
	stateDir, err := DefaultStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "config.yaml"), nil
}

func ConfigPathForStateDir(stateDir string) (string, error) {
	if strings.TrimSpace(stateDir) == "" {
		return DefaultConfigPath()
	}
	expanded, err := expandPath(stateDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(expanded, "config.yaml"), nil
}

func DefaultConfig(stateDir string) Config {
	listenURL := "ws://127.0.0.1:8765"
	return Config{
		StateDir: stateDir,
		Codex: CodexConfig{
			Path:      "codex",
			ListenURL: listenURL,
			ReadyURL:  mustDeriveHTTPURL(listenURL, "/readyz"),
			HealthURL: mustDeriveHTTPURL(listenURL, "/healthz"),
			TokenFile: filepath.Join(stateDir, "codex-ws.token"),
		},
		Cloudflared: CloudflaredConfig{
			Enabled: false,
			Path:    "cloudflared",
		},
		Supervisor: SupervisorConfig{
			HealthCheckInterval:        "3s",
			HealthCheckTimeout:         "2s",
			ReadyFailuresBeforeRestart: 3,
			RestartInitialBackoff:      "1s",
			RestartMaxBackoff:          "30s",
			ShutdownGracePeriod:        "5s",
		},
	}
}

func ResolveConfigPath(configPath string) (string, error) {
	if strings.TrimSpace(configPath) == "" {
		return DefaultConfigPath()
	}
	return expandPath(configPath)
}

func Load(configPath string) (Config, string, error) {
	resolvedPath, err := ResolveConfigPath(configPath)
	if err != nil {
		return Config{}, "", err
	}
	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		return Config{}, "", fmt.Errorf("read config %s: %w", resolvedPath, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, "", fmt.Errorf("parse config %s: %w", resolvedPath, err)
	}
	if cfg.StateDir == "" {
		cfg.StateDir = filepath.Dir(resolvedPath)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return Config{}, "", err
	}
	return cfg, resolvedPath, nil
}

func Save(configPath string, cfg Config) error {
	resolvedPath, err := ResolveConfigPath(configPath)
	if err != nil {
		return err
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(resolvedPath), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := yaml.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(resolvedPath, data, 0o644)
}

func (c *Config) ApplyDefaults() {
	if c.StateDir == "" {
		if stateDir, err := DefaultStateDir(); err == nil {
			c.StateDir = stateDir
		}
	}
	defaults := DefaultConfig(c.StateDir)
	if c.Codex.Path == "" {
		c.Codex.Path = defaults.Codex.Path
	}
	if c.Codex.ListenURL == "" {
		c.Codex.ListenURL = defaults.Codex.ListenURL
	}
	if c.Codex.ReadyURL == "" {
		c.Codex.ReadyURL = mustDeriveHTTPURL(c.Codex.ListenURL, "/readyz")
	}
	if c.Codex.HealthURL == "" {
		c.Codex.HealthURL = mustDeriveHTTPURL(c.Codex.ListenURL, "/healthz")
	}
	if c.Codex.TokenFile == "" {
		c.Codex.TokenFile = filepath.Join(c.StateDir, "codex-ws.token")
	}
	if c.Cloudflared.Path == "" {
		c.Cloudflared.Path = defaults.Cloudflared.Path
	}
	if c.Supervisor.HealthCheckInterval == "" {
		c.Supervisor.HealthCheckInterval = defaults.Supervisor.HealthCheckInterval
	}
	if c.Supervisor.HealthCheckTimeout == "" {
		c.Supervisor.HealthCheckTimeout = defaults.Supervisor.HealthCheckTimeout
	}
	if c.Supervisor.ReadyFailuresBeforeRestart == 0 {
		c.Supervisor.ReadyFailuresBeforeRestart = defaults.Supervisor.ReadyFailuresBeforeRestart
	}
	if c.Supervisor.RestartInitialBackoff == "" {
		c.Supervisor.RestartInitialBackoff = defaults.Supervisor.RestartInitialBackoff
	}
	if c.Supervisor.RestartMaxBackoff == "" {
		c.Supervisor.RestartMaxBackoff = defaults.Supervisor.RestartMaxBackoff
	}
	if c.Supervisor.ShutdownGracePeriod == "" {
		c.Supervisor.ShutdownGracePeriod = defaults.Supervisor.ShutdownGracePeriod
	}
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.StateDir) == "" {
		return errors.New("state_dir is required")
	}
	if strings.TrimSpace(c.Codex.Path) == "" {
		return errors.New("codex.path is required")
	}
	if strings.TrimSpace(c.Codex.ListenURL) == "" {
		return errors.New("codex.listen_url is required")
	}
	if _, err := url.Parse(c.Codex.ListenURL); err != nil {
		return fmt.Errorf("parse codex.listen_url: %w", err)
	}
	if strings.TrimSpace(c.Codex.ReadyURL) == "" {
		return errors.New("codex.ready_url is required")
	}
	if strings.TrimSpace(c.Codex.HealthURL) == "" {
		return errors.New("codex.health_url is required")
	}
	if strings.TrimSpace(c.Codex.TokenFile) == "" {
		return errors.New("codex.token_file is required")
	}
	if c.Supervisor.ReadyFailuresBeforeRestart < 1 {
		return errors.New("supervisor.ready_failures_before_restart must be >= 1")
	}
	if _, err := c.Runtime(); err != nil {
		return err
	}
	if c.Cloudflared.Enabled {
		if strings.TrimSpace(c.Cloudflared.Path) == "" {
			return errors.New("cloudflared.path is required when cloudflared is enabled")
		}
		if strings.TrimSpace(c.Cloudflared.TunnelName) == "" {
			return errors.New("cloudflared.tunnel_name is required when cloudflared is enabled")
		}
		if strings.TrimSpace(c.Cloudflared.ConfigFile) == "" {
			return errors.New("cloudflared.config_file is required when cloudflared is enabled")
		}
	}
	return nil
}

func (c Config) Runtime() (RuntimeConfig, error) {
	healthInterval, err := time.ParseDuration(c.Supervisor.HealthCheckInterval)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("parse supervisor.health_check_interval: %w", err)
	}
	healthTimeout, err := time.ParseDuration(c.Supervisor.HealthCheckTimeout)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("parse supervisor.health_check_timeout: %w", err)
	}
	initialBackoff, err := time.ParseDuration(c.Supervisor.RestartInitialBackoff)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("parse supervisor.restart_initial_backoff: %w", err)
	}
	maxBackoff, err := time.ParseDuration(c.Supervisor.RestartMaxBackoff)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("parse supervisor.restart_max_backoff: %w", err)
	}
	shutdownGrace, err := time.ParseDuration(c.Supervisor.ShutdownGracePeriod)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("parse supervisor.shutdown_grace_period: %w", err)
	}
	if maxBackoff < initialBackoff {
		return RuntimeConfig{}, errors.New("supervisor.restart_max_backoff must be >= restart_initial_backoff")
	}
	return RuntimeConfig{
		HealthCheckInterval:        healthInterval,
		HealthCheckTimeout:         healthTimeout,
		RestartInitialBackoff:      initialBackoff,
		RestartMaxBackoff:          maxBackoff,
		ShutdownGracePeriod:        shutdownGrace,
		ReadyFailuresBeforeRestart: c.Supervisor.ReadyFailuresBeforeRestart,
	}, nil
}

func Init(opts InitOptions) (InitResult, error) {
	stateDir := opts.StateDir
	var err error
	if strings.TrimSpace(stateDir) == "" {
		stateDir, err = DefaultStateDir()
		if err != nil {
			return InitResult{}, err
		}
	}
	stateDir, err = expandPath(stateDir)
	if err != nil {
		return InitResult{}, err
	}

	configPath := opts.ConfigPath
	if strings.TrimSpace(configPath) == "" {
		configPath = filepath.Join(stateDir, "config.yaml")
	}
	configPath, err = expandPath(configPath)
	if err != nil {
		return InitResult{}, err
	}
	if !opts.Force {
		if _, err := os.Stat(configPath); err == nil {
			return InitResult{}, fmt.Errorf("config already exists at %s (use --force to overwrite)", configPath)
		}
	}

	cfg := DefaultConfig(stateDir)
	if opts.CodexPath != "" {
		cfg.Codex.Path = opts.CodexPath
	}
	if opts.ListenURL != "" {
		cfg.Codex.ListenURL = opts.ListenURL
		cfg.Codex.ReadyURL = mustDeriveHTTPURL(opts.ListenURL, "/readyz")
		cfg.Codex.HealthURL = mustDeriveHTTPURL(opts.ListenURL, "/healthz")
	}
	if resolved, err := resolveExecutable(cfg.Codex.Path); err == nil {
		cfg.Codex.Path = resolved
	}
	if opts.EnableCloudflared {
		cfg.Cloudflared.Enabled = true
		if opts.CloudflaredPath != "" {
			cfg.Cloudflared.Path = opts.CloudflaredPath
		}
		if resolved, err := resolveExecutable(cfg.Cloudflared.Path); err == nil {
			cfg.Cloudflared.Path = resolved
		}
		cfg.Cloudflared.ConfigFile = opts.CloudflaredConfig
		cfg.Cloudflared.TunnelName = opts.CloudflaredTunnel
	}

	if err := os.MkdirAll(filepath.Join(stateDir, "logs"), 0o755); err != nil {
		return InitResult{}, fmt.Errorf("create log dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Codex.TokenFile), 0o755); err != nil {
		return InitResult{}, fmt.Errorf("create token dir: %w", err)
	}
	if err := ensureTokenFile(cfg.Codex.TokenFile); err != nil {
		return InitResult{}, err
	}
	if err := Save(configPath, cfg); err != nil {
		return InitResult{}, err
	}

	executable, err := os.Executable()
	if err != nil {
		return InitResult{}, fmt.Errorf("resolve current executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return InitResult{}, fmt.Errorf("resolve current executable abs path: %w", err)
	}
	launchdPlist, err := RenderLaunchdPlist(DefaultLabel, executable, configPath, stateDir)
	if err != nil {
		return InitResult{}, err
	}
	launchdPath := filepath.Join(stateDir, "codex-remote.plist")
	if err := os.WriteFile(launchdPath, []byte(launchdPlist), 0o644); err != nil {
		return InitResult{}, fmt.Errorf("write launchd plist: %w", err)
	}

	return InitResult{
		ConfigPath:   configPath,
		StateDir:     stateDir,
		TokenFile:    cfg.Codex.TokenFile,
		LaunchdPlist: launchdPath,
	}, nil
}

func ensureTokenFile(path string) error {
	if info, err := os.Stat(path); err == nil {
		if info.Mode().Perm() != 0o600 {
			if err := os.Chmod(path, 0o600); err != nil {
				return fmt.Errorf("chmod token file: %w", err)
			}
		}
		return nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("generate token: %w", err)
	}
	token := hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		return fmt.Errorf("write token file: %w", err)
	}
	return nil
}

func resolveExecutable(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("empty executable path")
	}
	if strings.Contains(name, string(os.PathSeparator)) {
		return expandPath(name)
	}
	if resolved, err := exec.LookPath(name); err == nil {
		return resolved, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	sibling := filepath.Join(filepath.Dir(executable), name)
	info, err := os.Stat(sibling)
	if err != nil {
		return "", exec.ErrNotFound
	}
	if info.IsDir() {
		return "", exec.ErrNotFound
	}
	return sibling, nil
}

func expandPath(path string) (string, error) {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return filepath.Abs(path)
}

func DeriveHTTPURL(listenURL, path string) (string, error) {
	parsed, err := url.Parse(listenURL)
	if err != nil {
		return "", fmt.Errorf("parse listen url: %w", err)
	}
	switch parsed.Scheme {
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	default:
		return "", fmt.Errorf("listen url must use ws or wss, got %q", parsed.Scheme)
	}
	parsed.Path = path
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func mustDeriveHTTPURL(listenURL, path string) string {
	derived, err := DeriveHTTPURL(listenURL, path)
	if err != nil {
		return ""
	}
	return derived
}
