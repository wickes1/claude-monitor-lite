// install.go - LaunchAgent registration so the monitor starts at login.
//
// macOS menu bar apps only auto-start at login if registered as a Login Item
// or a LaunchAgent. The Homebrew Cask binary does neither, so after a reboot
// the monitor does not come back. `install` writes a LaunchAgent plist that
// runs `<binary> start` with RunAtLoad and registers it with launchctl;
// `uninstall` reverses that. These manage autostart wiring only and are
// orthogonal to start/stop (process lifecycle).

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// launchAgentBaseLabel is the reverse-DNS LaunchAgent label, aligned to the Go
// module path so it is OSS-neutral and stable across machines.
const launchAgentBaseLabel = "io.github.wickes1.claude-monitor-lite"

// launchAgentLabel gives each CLAUDE_CONFIG_DIR instance its own launchd job,
// so installing one instance's autostart never boots out another's.
func launchAgentLabel() string {
	if id := instanceID(); id != "" {
		return launchAgentBaseLabel + "." + id
	}
	return launchAgentBaseLabel
}

// launchAgentPath returns ~/Library/LaunchAgents/<label>.plist.
func launchAgentPath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, "Library", "LaunchAgents", launchAgentLabel()+".plist"), nil
}

// resolveBinaryPath returns the absolute path to embed in the LaunchAgent.
// It prefers the PATH entry — but only when that entry is the SAME file we are
// running. A Homebrew install and its version-independent `/opt/homebrew/bin`
// symlink share an inode, so the symlink still wins and the plist survives a
// `brew upgrade` that bumps the Caskroom version directory. But a freshly-built
// ./claude-monitor-lite run while an OLDER brew copy is first on PATH has a
// different inode — there we honor the binary the user actually invoked instead
// of silently wiring up the stale copy. os.Executable() is also the fallback
// for `go install` users and any non-PATH invocation.
func resolveBinaryPath() (string, error) {
	self, selfErr := os.Executable()
	if p, err := exec.LookPath("claude-monitor-lite"); err == nil {
		if abs, absErr := filepath.Abs(p); absErr == nil {
			if selfErr != nil {
				return abs, nil
			}
			if pathInfo, e1 := os.Stat(abs); e1 == nil {
				if selfInfo, e2 := os.Stat(self); e2 == nil && os.SameFile(pathInfo, selfInfo) {
					return abs, nil
				}
			}
		}
	}
	if selfErr != nil {
		return "", selfErr
	}
	return self, nil
}

// xmlEscape escapes the three XML metacharacters that can appear in a file
// path (an ampersand or angle bracket in a home directory name would
// otherwise produce a malformed plist).
func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// renderLaunchAgent builds the plist XML for binaryPath. Pure function so it
// is unit-testable.
//
// RunAtLoad makes launchd run `<binary> start` at login, which forks the
// systray daemon and exits. AbandonProcessGroup keeps launchd from killing
// that daemon as soon as the launcher exits (observed on macOS 26: without
// it, `install` from a shell left no monitor running). It also means unloading
// the agent no longer reliably stops the daemon, so install and uninstall
// stop it by PID themselves. No KeepAlive: launchd must not respawn the
// monitor after the user quits it from the menu bar. No StandardOutPath/StandardErrorPath: the
// daemon rebinds its own log to ~/.claude-monitor-lite*.log (setupDaemonLogging),
// so a launchd-level redirect would only capture the millisecond-long launcher.
// A non-empty configDir is passed through as CLAUDE_CONFIG_DIR, because launchd
// starts the job without the installing shell's environment.
func renderLaunchAgent(binaryPath, configDir string) string {
	env := ""
	if configDir != "" {
		env = fmt.Sprintf(`	<key>EnvironmentVariables</key>
	<dict>
		<key>CLAUDE_CONFIG_DIR</key>
		<string>%s</string>
	</dict>
`, xmlEscape(configDir))
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>start</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>AbandonProcessGroup</key>
	<true/>
%s</dict>
</plist>
`, xmlEscape(launchAgentLabel()), xmlEscape(binaryPath), env)
}

// serviceTarget returns the launchctl service target gui/<uid>/<label>.
func serviceTarget() string {
	return fmt.Sprintf("gui/%s/%s", strconv.Itoa(os.Getuid()), launchAgentLabel())
}

// domainTarget returns the launchctl domain target gui/<uid>.
func domainTarget() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

func handleInstall() {
	if runtime.GOOS != "darwin" {
		fmt.Fprintln(os.Stderr, "install is only supported on macOS.")
		os.Exit(1)
	}

	// LaunchAgents are per-user; sudo would target gui/0 (no GUI login session)
	// and write the plist under /var/root, failing with an opaque error.
	if os.Geteuid() == 0 {
		fmt.Fprintln(os.Stderr, "Do not run install with sudo — LaunchAgents are per-user. Run as your normal user.")
		os.Exit(1)
	}

	binPath, err := resolveBinaryPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to resolve binary path: %v\n", err)
		os.Exit(1)
	}

	plistPath, err := launchAgentPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to resolve LaunchAgents path: %v\n", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(filepath.Dir(plistPath), 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create LaunchAgents directory: %v\n", err)
		os.Exit(1)
	}

	// launchd runs the job from /, so a relative CLAUDE_CONFIG_DIR would point
	// at the wrong directory there.
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if configDir != "" {
		if configDir, err = filepath.Abs(configDir); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to resolve CLAUDE_CONFIG_DIR: %v\n", err)
			os.Exit(1)
		}
	}
	plist := renderLaunchAgent(binPath, configDir)

	if err := writeFileAtomic(plistPath, []byte(plist), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write LaunchAgent: %v\n", err)
		os.Exit(1)
	}

	// Boot out any previously-registered job so bootstrap cannot fail with
	// "service already loaded" (makes install idempotent), and stop a running
	// monitor so the bootstrap below restarts it via RunAtLoad on the binary
	// just installed — a reinstall is a restart.
	_ = exec.Command("launchctl", "bootout", serviceTarget()).Run()
	restarted, err := stopRunning()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to stop the running monitor: %v\n", err)
		os.Exit(1)
	}

	if out, err := exec.Command("launchctl", "bootstrap", domainTarget(), plistPath).CombinedOutput(); err != nil {
		// Roll the plist back so a failed install leaves nothing that launchd
		// would auto-bootstrap at the next login (e.g. install over SSH has no
		// gui/<uid> GUI domain and fails right here). Keeps failure atomic.
		_ = os.Remove(plistPath)
		fmt.Fprintf(os.Stderr, "Failed to load LaunchAgent: %v\n%s", err, out)
		os.Exit(1)
	}

	fmt.Println("✓ Installed. claude-monitor-lite will start automatically at login.")
	if restarted != 0 {
		fmt.Printf("  Restarted the running monitor (was PID %d).\n", restarted)
	}
	fmt.Printf("  LaunchAgent: %s\n", plistPath)
	fmt.Printf("  Binary:      %s\n", binPath)
	fmt.Println("  Remove with: claude-monitor-lite uninstall")
}

func handleUninstall() {
	if runtime.GOOS != "darwin" {
		fmt.Fprintln(os.Stderr, "uninstall is only supported on macOS.")
		os.Exit(1)
	}

	if os.Geteuid() == 0 {
		fmt.Fprintln(os.Stderr, "Do not run uninstall with sudo — LaunchAgents are per-user. Run as your normal user.")
		os.Exit(1)
	}

	plistPath, err := launchAgentPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to resolve LaunchAgents path: %v\n", err)
		os.Exit(1)
	}

	// Unregister first, then remove the plist. A "not loaded" error is benign
	// (nothing was registered); any other failure means the job is still
	// registered, so surface it (Rule 12) while still removing the plist.
	if out, err := exec.Command("launchctl", "bootout", serviceTarget()).CombinedOutput(); err != nil {
		s := string(out)
		notLoaded := strings.Contains(s, "No such process") || strings.Contains(s, "Could not find specified service")
		if !notLoaded {
			fmt.Fprintf(os.Stderr, "Warning: launchctl bootout did not cleanly unload (may stay loaded until logout): %v\n%s", err, s)
		}
	}

	switch err := os.Remove(plistPath); {
	case err == nil:
		fmt.Println("✓ Uninstalled. claude-monitor-lite will no longer start at login.")
	case os.IsNotExist(err):
		fmt.Println("✓ Not installed at login (nothing to remove).")
	default:
		fmt.Fprintf(os.Stderr, "Failed to remove LaunchAgent: %v\n", err)
		os.Exit(1)
	}

	switch pid, err := stopRunning(); {
	case err != nil:
		fmt.Fprintf(os.Stderr, "Failed to stop the running monitor: %v\n", err)
		os.Exit(1)
	case pid != 0:
		fmt.Printf("  Stopped the running monitor (PID %d).\n", pid)
	}
	fmt.Println("  Start it again anytime with: claude-monitor-lite")
}
