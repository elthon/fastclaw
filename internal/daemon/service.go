package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
)

// Install installs FastClaw as an OS service.
func Install() error {
	switch runtime.GOOS {
	case "darwin":
		return installLaunchd()
	case "linux":
		return installSystemd()
	default:
		printWindowsInstructions()
		return nil
	}
}

// Uninstall removes the FastClaw OS service.
func Uninstall() error {
	switch runtime.GOOS {
	case "darwin":
		return uninstallLaunchd()
	case "linux":
		return uninstallSystemd()
	default:
		fmt.Println("Manual removal required on this platform.")
		return nil
	}
}

// instanceSuffix tells a non-default FASTCLAW_HOME apart in service
// names, so installing a dev daemon (FASTCLAW_HOME=~/.fastclaw-dev)
// doesn't overwrite the release one. "" for the default home.
func instanceSuffix() string {
	h := os.Getenv("FASTCLAW_HOME")
	if h == "" {
		return ""
	}
	if def, err := os.UserHomeDir(); err == nil && filepath.Clean(h) == filepath.Join(def, ".fastclaw") {
		return ""
	}
	name := strings.TrimPrefix(strings.TrimLeft(filepath.Base(h), "."), "fastclaw")
	name = strings.Trim(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, name), "-")
	if name == "" {
		name = "custom"
	}
	return "-" + name
}

// serviceEnv is the FASTCLAW_* instance selectors forwarded into the
// service definition; launchd/systemd don't inherit the shell's env.
func serviceEnv() [][2]string {
	var env [][2]string
	for _, k := range []string{"FASTCLAW_HOME", "FASTCLAW_PORT"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, [2]string{k, v})
		}
	}
	return env
}

// --- macOS launchd ---

func launchdLabel() string { return "ai.fastclaw.gateway" + instanceSuffix() }

var launchdPlistTemplate = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{.Label}}</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{.BinaryPath}}</string>
        <string>gateway</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>{{.LogDir}}/gateway.stdout.log</string>
    <key>StandardErrorPath</key>
    <string>{{.LogDir}}/gateway.stderr.log</string>
    <key>WorkingDirectory</key>
    <string>{{.HomeDir}}</string>{{if .Env}}
    <key>EnvironmentVariables</key>
    <dict>{{range .Env}}
        <key>{{index . 0}}</key>
        <string>{{index . 1}}</string>{{end}}
    </dict>{{end}}
</dict>
</plist>
`))

func launchdPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel()+".plist"), nil
}

func installLaunchd() error {
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}
	bin, _ = filepath.EvalSymlinks(bin)

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	_, _, logDir, err := Paths()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}

	plistPath, err := launchdPlistPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return err
	}

	data := struct {
		Label      string
		BinaryPath string
		LogDir     string
		HomeDir    string
		Env        [][2]string
	}{
		Label:      launchdLabel(),
		BinaryPath: bin,
		LogDir:     logDir,
		HomeDir:    home,
		Env:        serviceEnv(),
	}

	var buf strings.Builder
	if err := launchdPlistTemplate.Execute(&buf, data); err != nil {
		return fmt.Errorf("render plist: %w", err)
	}

	if err := os.WriteFile(plistPath, []byte(buf.String()), 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}

	cmd := exec.Command("launchctl", "load", plistPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("launchctl load: %w", err)
	}

	fmt.Printf("Service installed: %s\n", plistPath)
	fmt.Println("FastClaw gateway will start automatically on login.")
	return nil
}

func uninstallLaunchd() error {
	plistPath, err := launchdPlistPath()
	if err != nil {
		return err
	}

	if _, err := os.Stat(plistPath); os.IsNotExist(err) {
		return fmt.Errorf("service not installed (no plist at %s)", plistPath)
	}

	cmd := exec.Command("launchctl", "unload", plistPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Printf("Warning: launchctl unload failed: %v\n", err)
	}

	if err := os.Remove(plistPath); err != nil {
		return fmt.Errorf("remove plist: %w", err)
	}

	fmt.Println("Service uninstalled.")
	return nil
}

// --- Linux systemd ---

func systemdUnitName() string { return "fastclaw-gateway" + instanceSuffix() + ".service" }

var systemdUnitTemplate = template.Must(template.New("unit").Parse(`[Unit]
Description=FastClaw AI Agent Gateway
After=network.target

[Service]
Type=simple
ExecStart={{.BinaryPath}} gateway
Restart=always
RestartSec=5
Environment=HOME={{.HomeDir}}
{{range .Env}}Environment={{index . 0}}={{index . 1}}
{{end}}WorkingDirectory={{.HomeDir}}
StandardOutput=append:{{.LogDir}}/gateway.stdout.log
StandardError=append:{{.LogDir}}/gateway.stderr.log

[Install]
WantedBy=default.target
`))

func systemdUnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", systemdUnitName()), nil
}

func installSystemd() error {
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find executable: %w", err)
	}
	bin, _ = filepath.EvalSymlinks(bin)

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	_, _, logDir, err := Paths()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}

	unitPath, err := systemdUnitPath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return err
	}

	data := struct {
		BinaryPath string
		HomeDir    string
		LogDir     string
		Env        [][2]string
	}{
		BinaryPath: bin,
		HomeDir:    home,
		LogDir:     logDir,
		Env:        serviceEnv(),
	}

	var buf strings.Builder
	if err := systemdUnitTemplate.Execute(&buf, data); err != nil {
		return fmt.Errorf("render unit file: %w", err)
	}

	if err := os.WriteFile(unitPath, []byte(buf.String()), 0o644); err != nil {
		return fmt.Errorf("write unit file: %w", err)
	}

	// Enable and start
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", systemdUnitName()},
		{"--user", "start", systemdUnitName()},
	} {
		cmd := exec.Command("systemctl", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
		}
	}

	fmt.Printf("Service installed: %s\n", unitPath)
	fmt.Println("FastClaw gateway is running and will start on boot.")
	return nil
}

func uninstallSystemd() error {
	unitPath, err := systemdUnitPath()
	if err != nil {
		return err
	}

	if _, err := os.Stat(unitPath); os.IsNotExist(err) {
		return fmt.Errorf("service not installed (no unit at %s)", unitPath)
	}

	for _, args := range [][]string{
		{"--user", "stop", systemdUnitName()},
		{"--user", "disable", systemdUnitName()},
	} {
		cmd := exec.Command("systemctl", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Run() // best-effort
	}

	if err := os.Remove(unitPath); err != nil {
		return fmt.Errorf("remove unit file: %w", err)
	}

	// Reload
	exec.Command("systemctl", "--user", "daemon-reload").Run()

	fmt.Println("Service uninstalled.")
	return nil
}

// --- Windows ---

func printWindowsInstructions() {
	fmt.Println("Automatic service installation is not supported on Windows.")
	fmt.Println()
	fmt.Println("Options:")
	fmt.Println("  1. Use NSSM (Non-Sucking Service Manager):")
	fmt.Println("     nssm install FastClaw <path-to-fastclaw.exe> gateway")
	fmt.Println()
	fmt.Println("  2. Use Task Scheduler:")
	fmt.Println("     - Open Task Scheduler")
	fmt.Println("     - Create a new task that runs 'fastclaw.exe gateway'")
	fmt.Println("     - Set it to run at startup")
}
