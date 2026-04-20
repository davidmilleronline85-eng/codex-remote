package codexremote

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

var tryCloudflareURLPattern = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

const (
	publicReadyWarnAfter = 60 * time.Second
	publicReadyInterval  = 2 * time.Second
	publicReadyCheckTTL  = 5 * time.Second
)

type PublicAccessInfo struct {
	HTTPSURL     string
	WebSocketURL string
}

func RunQuickTunnel(ctx context.Context, configPath string, stdout, stderr io.Writer, onReady func(PublicAccessInfo)) error {
	cfg, _, err := Load(configPath)
	if err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := WaitReady(waitCtx, cfg.Codex.ReadyURL, 2*time.Second); err != nil {
		return fmt.Errorf("codex app-server is not ready; run `codex-remote start --public=false` first or keep `codex-remote start` running: %w", err)
	}
	cloudflaredPath, err := resolveExecutable(cfg.Cloudflared.Path)
	if err != nil {
		return fmt.Errorf("cloudflared is required for quick exposure; rerun the installer or put `cloudflared` on PATH: %w", err)
	}
	origin, err := OriginHTTPURL(cfg.Codex.ListenURL)
	if err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, cloudflaredPath, "tunnel", "--url", origin)
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("cloudflared stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("cloudflared stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start cloudflared: %w", err)
	}

	var once sync.Once
	announce := func(line string) {
		match := tryCloudflareURLPattern.FindString(line)
		if match == "" {
			return
		}
		publicWS := "wss://" + strings.TrimPrefix(match, "https://")
		once.Do(func() {
			fmt.Fprintf(stdout, "Quick tunnel URL allocated\n")
			fmt.Fprintf(stdout, "Public HTTPS URL: %s\n", match)
			fmt.Fprintf(stdout, "Public WebSocket URL: %s\n", publicWS)
			fmt.Fprintf(stdout, "Waiting for public reachability at %s/readyz\n", strings.TrimSuffix(match, "/"))
			go waitForPublicTunnelReady(ctx, stdout, match, publicWS, onReady)
		})
	}

	copyStream := func(prefix string, reader io.Reader, writer io.Writer) {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			line := scanner.Text()
			announce(line)
			fmt.Fprintf(writer, "%s%s\n", prefix, line)
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		copyStream("[cloudflared] ", stdoutPipe, stdout)
	}()
	go func() {
		defer wg.Done()
		copyStream("[cloudflared] ", stderrPipe, stderr)
	}()

	waitErr := cmd.Wait()
	wg.Wait()
	if ctx.Err() != nil {
		return nil
	}
	if waitErr != nil {
		return fmt.Errorf("cloudflared quick tunnel exited: %w", waitErr)
	}
	return nil
}

func waitForPublicTunnelReady(ctx context.Context, stdout io.Writer, publicHTTPS, publicWS string, onReady func(PublicAccessInfo)) {
	readyzURL := strings.TrimSuffix(publicHTTPS, "/") + "/readyz"
	ticker := time.NewTicker(publicReadyInterval)
	defer ticker.Stop()

	startedAt := time.Now()
	warned := false
	var lastDetail string
	for {
		checkCtx, cancel := context.WithTimeout(ctx, publicReadyCheckTTL)
		ok, detail := checkHTTP(checkCtx, readyzURL, publicReadyCheckTTL)
		cancel()
		if ok {
			fmt.Fprintf(stdout, "Quick tunnel ready\n")
			fmt.Fprintf(stdout, "Public HTTPS URL: %s\n", publicHTTPS)
			fmt.Fprintf(stdout, "Public WebSocket URL: %s\n", publicWS)
			fmt.Fprintf(stdout, "Public readyz: %s\n", readyzURL)
			fmt.Fprintf(stdout, "Keep this `codex-remote start` process running while remote agents use the server.\n")
			fmt.Fprintf(stdout, "Press Ctrl-C to stop the tunnel.\n")
			if onReady != nil {
				onReady(PublicAccessInfo{
					HTTPSURL:     publicHTTPS,
					WebSocketURL: publicWS,
				})
			}
			return
		}
		lastDetail = detail
		if !warned && time.Since(startedAt) >= publicReadyWarnAfter {
			fmt.Fprintf(stdout, "Quick tunnel is still not publicly reachable after %s (%s)\n", publicReadyWarnAfter, lastDetail)
			fmt.Fprintf(stdout, "Cloudflare Quick Tunnels can return transient 530 errors until the edge can reach your local origin.\n")
			fmt.Fprintf(stdout, "Keeping the process alive and continuing to wait for public reachability.\n")
			warned = true
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
