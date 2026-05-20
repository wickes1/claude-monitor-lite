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

	indicators := []string{"currentSession", "weeklyAll", "weeklySonnet", "weeklyDesign"}
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
				c.MenuBarIndicator = indicators[n%len(indicators)]
			})
		}(i)
	}
	writers.Wait()
	close(stop)
	reader.Wait()
}
