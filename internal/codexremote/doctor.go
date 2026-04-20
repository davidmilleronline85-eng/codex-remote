package codexremote

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"time"
)

type DoctorReport struct {
	OK     bool          `json:"ok"`
	Config string        `json:"config"`
	Checks []DoctorCheck `json:"checks"`
}

type DoctorCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Details string `json:"details"`
}

func RunDoctor(ctx context.Context, configPath string) (DoctorReport, error) {
	cfg, resolvedPath, err := Load(configPath)
	if err != nil {
		return DoctorReport{}, err
	}
	runtimeCfg, err := cfg.Runtime()
	if err != nil {
		return DoctorReport{}, err
	}

	report := DoctorReport{
		OK:     true,
		Config: resolvedPath,
	}
	add := func(name string, ok bool, details string) {
		report.Checks = append(report.Checks, DoctorCheck{Name: name, OK: ok, Details: details})
		if !ok {
			report.OK = false
		}
	}

	add("config", true, fmt.Sprintf("loaded %s", resolvedPath))
	if _, err := os.Stat(cfg.StateDir); err != nil {
		add("state_dir", false, err.Error())
	} else {
		add("state_dir", true, cfg.StateDir)
	}

	if path, err := resolveExecutable(cfg.Codex.Path); err != nil {
		add("codex_binary", false, err.Error())
	} else {
		add("codex_binary", true, path)
	}

	if info, err := os.Stat(cfg.Codex.TokenFile); err != nil {
		add("token_file", false, err.Error())
	} else {
		add("token_file", info.Mode().Perm() == 0o600, fmt.Sprintf("%s (mode %s)", cfg.Codex.TokenFile, info.Mode().Perm()))
	}

	if cfg.Cloudflared.Enabled {
		if path, err := resolveExecutable(cfg.Cloudflared.Path); err != nil {
			add("cloudflared_binary", false, err.Error())
		} else {
			add("cloudflared_binary", true, path)
		}
		if _, err := os.Stat(cfg.Cloudflared.ConfigFile); err != nil {
			add("cloudflared_config", false, err.Error())
		} else {
			add("cloudflared_config", true, cfg.Cloudflared.ConfigFile)
		}
	} else {
		add("cloudflared", true, "disabled")
	}

	readyOK, readyDetail := checkHTTP(ctx, cfg.Codex.ReadyURL, runtimeCfg.HealthCheckTimeout)
	add("readyz", readyOK, readyDetail)
	healthOK, healthDetail := checkHTTP(ctx, cfg.Codex.HealthURL, runtimeCfg.HealthCheckTimeout)
	add("healthz", healthOK, healthDetail)

	return report, nil
}

func (r DoctorReport) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

func checkHTTP(ctx context.Context, target string, timeout time.Duration) (bool, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false, err.Error()
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, resp.Status
	}
	return true, resp.Status
}

func CheckBinaryVersion(path string) (string, error) {
	resolved, err := resolveExecutable(path)
	if err != nil {
		return "", err
	}
	out, err := exec.Command(resolved, "--version").CombinedOutput()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
