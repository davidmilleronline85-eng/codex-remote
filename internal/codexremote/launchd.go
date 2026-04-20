package codexremote

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"text/template"
)

type launchdTemplateData struct {
	Label      string
	BinaryPath string
	ConfigPath string
	StdoutPath string
	StderrPath string
}

const launchdTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>{{ .Label }}</string>
  <key>ProgramArguments</key>
  <array>
    <string>{{ .BinaryPath }}</string>
    <string>run</string>
    <string>--config</string>
    <string>{{ .ConfigPath }}</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>{{ .StdoutPath }}</string>
  <key>StandardErrorPath</key>
  <string>{{ .StderrPath }}</string>
</dict>
</plist>
`

func RenderLaunchdPlist(label, binaryPath, configPath, stateDir string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("launchd is only supported on darwin")
	}
	binaryPath, err := expandPath(binaryPath)
	if err != nil {
		return "", err
	}
	configPath, err = expandPath(configPath)
	if err != nil {
		return "", err
	}
	stateDir, err = expandPath(stateDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "logs"), 0o755); err != nil {
		return "", fmt.Errorf("create launchd log dir: %w", err)
	}
	data := launchdTemplateData{
		Label:      label,
		BinaryPath: binaryPath,
		ConfigPath: configPath,
		StdoutPath: filepath.Join(stateDir, "logs", "launchd.stdout.log"),
		StderrPath: filepath.Join(stateDir, "logs", "launchd.stderr.log"),
	}
	tmpl, err := template.New("launchd").Parse(launchdTemplate)
	if err != nil {
		return "", fmt.Errorf("parse launchd template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute launchd template: %w", err)
	}
	return buf.String(), nil
}

func InstallLaunchd(label, binaryPath, configPath, stateDir string, force bool) (string, error) {
	plist, err := RenderLaunchdPlist(label, binaryPath, configPath, stateDir)
	if err != nil {
		return "", err
	}
	target, err := launchAgentPath(label)
	if err != nil {
		return "", err
	}
	if !force {
		if _, err := os.Stat(target); err == nil {
			return "", fmt.Errorf("launchd plist already exists at %s (use --force to overwrite)", target)
		}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", fmt.Errorf("create launch agents dir: %w", err)
	}
	if err := os.WriteFile(target, []byte(plist), 0o644); err != nil {
		return "", fmt.Errorf("write launchd plist: %w", err)
	}

	domain, err := launchdDomain()
	if err != nil {
		return "", err
	}
	_ = exec.Command("launchctl", "bootout", domain+"/"+label).Run()
	if out, err := exec.Command("launchctl", "bootstrap", domain, target).CombinedOutput(); err != nil {
		return "", fmt.Errorf("launchctl bootstrap: %w (%s)", err, string(out))
	}
	if out, err := exec.Command("launchctl", "kickstart", "-k", domain+"/"+label).CombinedOutput(); err != nil {
		return "", fmt.Errorf("launchctl kickstart: %w (%s)", err, string(out))
	}
	return target, nil
}

func UninstallLaunchd(label string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("launchd is only supported on darwin")
	}
	target, err := launchAgentPath(label)
	if err != nil {
		return err
	}
	domain, err := launchdDomain()
	if err != nil {
		return err
	}
	_ = exec.Command("launchctl", "bootout", domain+"/"+label).Run()
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove launchd plist: %w", err)
	}
	return nil
}

func launchAgentPath(label string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

func launchdDomain() (string, error) {
	currentUser, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve current user: %w", err)
	}
	uid, err := strconv.Atoi(currentUser.Uid)
	if err != nil {
		return "", fmt.Errorf("parse current uid: %w", err)
	}
	return fmt.Sprintf("gui/%d", uid), nil
}
