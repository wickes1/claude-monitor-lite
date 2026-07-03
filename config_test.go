package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

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

func TestModifyAndSaveConfigRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	if err := modifyAndSaveConfigAt(path, func(c *Config) {
		c.SessionKey = "secret-key"
		c.MenuBarIndicator = "weeklyAll"
	}); err != nil {
		t.Fatalf("first save failed: %v", err)
	}

	// A later modification must preserve fields it did not touch.
	if err := modifyAndSaveConfigAt(path, func(c *Config) {
		c.MenuBarIndicator = "weeklySonnet"
	}); err != nil {
		t.Fatalf("second save failed: %v", err)
	}

	got := loadConfigFrom(path)
	if got.SessionKey != "secret-key" {
		t.Errorf("SessionKey = %q, want preserved 'secret-key'", got.SessionKey)
	}
	if got.MenuBarIndicator != "weeklySonnet" {
		t.Errorf("MenuBarIndicator = %q, want 'weeklySonnet'", got.MenuBarIndicator)
	}
}

// A concurrent reader must never observe a half-written config file.
func TestModifyAndSaveConfigConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := modifyAndSaveConfigAt(path, func(c *Config) { c.SessionKey = "k" }); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})

	var reader sync.WaitGroup
	reader.Add(1)
	go func() {
		defer reader.Done()
		for {
			select {
			case <-stop:
				return
			default:
				raw, err := os.ReadFile(path)
				if err != nil {
					continue
				}
				var c Config
				if err := json.Unmarshal(raw, &c); err != nil {
					t.Errorf("reader observed a corrupt config: %v", err)
					return
				}
			}
		}
	}()

	var writers sync.WaitGroup
	for i := 0; i < 50; i++ {
		writers.Add(1)
		go func(n int) {
			defer writers.Done()
			_ = modifyAndSaveConfigAt(path, func(c *Config) {
				c.MenuBarIndicator = indicators[n%len(indicators)].key
			})
		}(i)
	}
	writers.Wait()
	close(stop)
	reader.Wait()
}

// Every key in the indicators table must survive a config save/load round-trip,
// so a freshly added metric can actually be persisted as the selected one.
func TestLoadConfigAcceptsAllIndicators(t *testing.T) {
	for _, ind := range indicators {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := modifyAndSaveConfigAt(path, func(c *Config) {
			c.MenuBarIndicator = ind.key
		}); err != nil {
			t.Fatalf("save %q failed: %v", ind.key, err)
		}
		if got := loadConfigFrom(path); got.MenuBarIndicator != ind.key {
			t.Errorf("indicator %q not preserved, got %q", ind.key, got.MenuBarIndicator)
		}
	}
}

// An unknown or stale indicator key must fall back to the default on load.
func TestLoadConfigRejectsUnknownIndicator(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"menuBarIndicator":"staleKey"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := loadConfigFrom(path); got.MenuBarIndicator != "currentSession" {
		t.Errorf("stale indicator = %q, want fallback 'currentSession'", got.MenuBarIndicator)
	}
}

// withTempConfigPath points GetConfigPath at a fresh temp file for the
// duration of the test (restoring the previous override on cleanup), so any
// test that goes through the package-level Load/Save/Clear/migrate helpers —
// which all resolve their path via GetConfigPath — never reads or writes the
// real ~/.claude-monitor-lite.json. Shared by auth_test.go and main_test.go.
func withTempConfigPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	old := configPathOverride
	configPathOverride = path
	t.Cleanup(func() { configPathOverride = old })
	return path
}

// GetConfigPath must honor configPathOverride when set, and fall back to the
// real home-directory path otherwise — the seam every other config-path test
// in this package relies on.
func TestGetConfigPath_HonorsOverride(t *testing.T) {
	old := configPathOverride
	defer func() { configPathOverride = old }()

	configPathOverride = "/tmp/example-override.json"
	if got := GetConfigPath(); got != "/tmp/example-override.json" {
		t.Errorf("GetConfigPath() = %q, want override path", got)
	}

	configPathOverride = ""
	homeDir, _ := os.UserHomeDir()
	want := filepath.Join(homeDir, ".claude-monitor-lite.json")
	if got := GetConfigPath(); got != want {
		t.Errorf("GetConfigPath() = %q, want %q", got, want)
	}
}
