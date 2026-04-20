package codexremote

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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

type dnsAnswer struct {
	Type int    `json:"type"`
	Data string `json:"data"`
}

type dnsResponse struct {
	Status int         `json:"Status"`
	Answer []dnsAnswer `json:"Answer"`
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

	return RunQuickTunnelOrigin(ctx, cloudflaredPath, origin, stdout, stderr, onReady)
}

func RunQuickTunnelOrigin(ctx context.Context, cloudflaredPath, origin string, stdout, stderr io.Writer, onReady func(PublicAccessInfo)) error {
	resolvedCloudflared, err := resolveExecutable(cloudflaredPath)
	if err != nil {
		return fmt.Errorf("cloudflared is required for quick exposure; rerun the installer or put `cloudflared` on PATH: %w", err)
	}

	cmd := exec.CommandContext(ctx, resolvedCloudflared, "tunnel", "--url", origin)
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
		ok, detail := checkTunnelReady(checkCtx, readyzURL, publicReadyCheckTTL)
		cancel()
		if ok {
			fmt.Fprintf(stdout, "Quick tunnel ready\n")
			fmt.Fprintf(stdout, "Public HTTPS URL: %s\n", publicHTTPS)
			fmt.Fprintf(stdout, "Public WebSocket URL: %s\n", publicWS)
			fmt.Fprintf(stdout, "Public readyz: %s\n", readyzURL)
			fmt.Fprintf(stdout, "Keep the original `codex-remote` process running while remote agents use the server.\n")
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

func checkTunnelReady(ctx context.Context, target string, timeout time.Duration) (bool, string) {
	ok, detail := checkHTTP(ctx, target, timeout)
	if ok {
		return true, detail
	}

	ok, fallbackDetail := checkHTTPViaPublicDNS(ctx, target, timeout)
	if ok {
		return true, fallbackDetail
	}
	if fallbackDetail != "" {
		return false, fallbackDetail
	}
	return false, detail
}

func checkHTTPViaPublicDNS(ctx context.Context, target string, timeout time.Duration) (bool, string) {
	parsed, err := url.Parse(target)
	if err != nil {
		return false, err.Error()
	}
	host := parsed.Hostname()
	if host == "" {
		return false, "missing hostname"
	}

	ips, err := resolveHostViaPublicDNS(ctx, host, timeout)
	if err != nil {
		return false, err.Error()
	}
	if len(ips) == 0 {
		return false, fmt.Sprintf("public DNS returned no A records for %s", host)
	}

	return checkHTTPViaIPs(ctx, target, timeout, ips)
}

func checkHTTPViaIPs(ctx context.Context, target string, timeout time.Duration, ips []string) (bool, string) {
	parsed, err := url.Parse(target)
	if err != nil {
		return false, err.Error()
	}
	host := parsed.Hostname()
	if host == "" {
		return false, "missing hostname"
	}
	port := parsed.Port()
	switch {
	case port != "":
	case parsed.Scheme == "https":
		port = "443"
	default:
		port = "80"
	}

	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{ServerName: host},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var lastErr error
			for _, ip := range ips {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if lastErr == nil {
				lastErr = fmt.Errorf("no reachable IPs for %s", host)
			}
			return nil, lastErr
		},
	}
	defer transport.CloseIdleConnections()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false, err.Error()
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Sprintf("%s via direct IP", resp.Status)
	}
	return true, fmt.Sprintf("%s via direct IP", resp.Status)
}

func resolveHostViaPublicDNS(ctx context.Context, host string, timeout time.Duration) ([]string, error) {
	endpoints := []string{
		"https://cloudflare-dns.com/dns-query?name=%s&type=A",
		"https://dns.google/resolve?name=%s&type=A",
	}
	client := &http.Client{Timeout: timeout}

	for _, endpoint := range endpoints {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(endpoint, url.QueryEscape(host)), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("accept", "application/dns-json")

		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		var payload dnsResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if decodeErr != nil {
			continue
		}
		if payload.Status != 0 {
			continue
		}

		var ips []string
		for _, answer := range payload.Answer {
			if answer.Type == 1 && answer.Data != "" {
				ips = append(ips, answer.Data)
			}
		}
		if len(ips) > 0 {
			return ips, nil
		}
	}

	return nil, fmt.Errorf("public DNS could not resolve %s", host)
}
