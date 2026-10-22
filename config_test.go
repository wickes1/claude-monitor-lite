package main

import (
	"os"
	"path/filepath"
	"testing"
)

// withTempHome points the home directory at a temp dir for the test, so config
// helpers never read or write the real ~/.claude-monitor-lite*.json.
func withTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	return home
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	content := []byte(`{"hello":"world"}`)

	if err := writeFileAtomic(path, content, 0600); err != nil {
		t.Fatalf("writeFileAtomic failed: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back failed: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content = %q, want %q", got, content)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("perm = %o, want 600", info.Mode().Perm())
	}

	// The temp file must not be left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("directory has %d entries, want 1 (leftover temp file?)", len(entries))
	}
}

// Every indicator must survive a save/load round-trip, so any metric the menu
// offers can actually be persisted as the selected one.
func TestConfigRoundTripsEveryIndicator(t *testing.T) {
	withTempHome(t)
	for _, ind := range indicators {
		if err := SaveConfig(Config{MenuBarIndicator: ind.key}); err != nil {
			t.Fatalf("save %q failed: %v", ind.key, err)
		}
		if got := LoadConfig().MenuBarIndicator; got != ind.key {
			t.Errorf("indicator %q not preserved, got %q", ind.key, got)
		}
	}
}

// A missing config, an unreadable one, or an unknown indicator key must all
// load as the default indicator rather than an empty selection.
func TestLoadConfigFallsBackToDefault(t *testing.T) {
	home := withTempHome(t)
	path := filepath.Join(home, ".claude-monitor-lite.json")
	want := indicators[0].key

	if got := LoadConfig().MenuBarIndicator; got != want {
		t.Errorf("missing config: indicator = %q, want %q", got, want)
	}
	for _, body := range []string{`not json`, `{"menuBarIndicator":"staleKey"}`} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if got := LoadConfig().MenuBarIndicator; got != want {
			t.Errorf("config %s: indicator = %q, want %q", body, got, want)
		}
	}
}

// The default instance uses the plain dotfile names the README documents; a
// CLAUDE_CONFIG_DIR instance must get its own, or a second monitor refuses to
// start ("already running") and the two overwrite each other's config.
func TestInstanceFile(t *testing.T) {
	home := withTempHome(t)

	if got, want := instanceFile(".pid"), filepath.Join(home, ".claude-monitor-lite.pid"); got != want {
		t.Errorf("default instanceFile(.pid) = %q, want %q", got, want)
	}

	// be865d75 is sha256(dir)[:8], the hash Claude Code names this dir's files by.
	t.Setenv("CLAUDE_CONFIG_DIR", "/Users/alice/.claude-work/")
	for _, ext := range []string{".pid", ".json", ".log", ".usage.json"} {
		if got, want := instanceFile(ext), filepath.Join(home, ".claude-monitor-lite.claude-work-be865d75"+ext); got != want {
			t.Errorf("instanceFile(%s) = %q, want %q", ext, got, want)
		}
	}
	if got := instanceName(); got != "claude-work" {
		t.Errorf("instanceName() = %q, want the readable %q for the menu bar", got, "claude-work")
	}

	// A dotted dir name must not produce a doubled dot or path separator.
	t.Setenv("CLAUDE_CONFIG_DIR", "/Users/x/.claude-work")
	if got := instanceName(); got != "claude-work" {
		t.Errorf("instanceName() = %q, want %q", got, "claude-work")
	}
}

// Two environments whose dirs share a folder name are still different
// accounts and must not share an instance (PID file, config, LaunchAgent).
func TestInstanceIDDistinguishesSameFolderName(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/Users/x/work/.claude")
	work := instanceID()
	t.Setenv("CLAUDE_CONFIG_DIR", "/Users/x/home/.claude")
	if home := instanceID(); home == work {
		t.Errorf("instanceID() = %q for both dirs, want distinct", home)
	}
}

// A relative CLAUDE_CONFIG_DIR (as typed in a shell) and its absolute form (as
// recorded in the LaunchAgent) are the same environment and must resolve to
// the same instance, or stop/uninstall cannot find the autostarted daemon.
func TestInstanceIDIgnoresPathSpelling(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("CLAUDE_CONFIG_DIR", "env")
	relative := instanceID()
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "env"))
	if abs := instanceID(); abs != relative {
		t.Errorf("instanceID() relative = %q, absolute = %q, want equal", relative, abs)
	}
}
