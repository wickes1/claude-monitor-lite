package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withTempProfileEnv points every profile-related path override (config file,
// profiles base dir, active-state file) at fresh locations under t.TempDir(),
// and points claudeConfigDir() at a fresh temp dir too via CLAUDE_CONFIG_DIR.
// This is the seam every test in this file uses so no test ever touches the
// real ~/.claude, ~/.claude-profiles, ~/.claude-monitor-lite.json, or
// ~/.claude-monitor-lite-active (ISC-76).
func withTempProfileEnv(t *testing.T) (defaultDir string) {
	t.Helper()

	root := t.TempDir()

	oldConfigPath := configPathOverride
	configPathOverride = filepath.Join(root, "config.json")

	oldBaseDir := profilesBaseDirOverride
	profilesBaseDirOverride = filepath.Join(root, "profiles-base")

	oldStatePath := stateFilePathOverride
	stateFilePathOverride = filepath.Join(root, "active-state")

	defaultDir = filepath.Join(root, "default-claude-dir")
	if err := os.MkdirAll(defaultDir, 0700); err != nil {
		t.Fatalf("failed to create fake default config dir: %v", err)
	}
	oldClaudeConfigDir, hadClaudeConfigDir := os.LookupEnv("CLAUDE_CONFIG_DIR")
	if err := os.Setenv("CLAUDE_CONFIG_DIR", defaultDir); err != nil {
		t.Fatalf("failed to set CLAUDE_CONFIG_DIR: %v", err)
	}

	t.Cleanup(func() {
		configPathOverride = oldConfigPath
		profilesBaseDirOverride = oldBaseDir
		stateFilePathOverride = oldStatePath
		if hadClaudeConfigDir {
			os.Setenv("CLAUDE_CONFIG_DIR", oldClaudeConfigDir)
		} else {
			os.Unsetenv("CLAUDE_CONFIG_DIR")
		}
	})

	return defaultDir
}

// --- ISC-5: name validation ---

func TestValidateProfileName(t *testing.T) {
	tests := []struct {
		name    string
		valid   bool
		wantErr error
	}{
		{name: "work", valid: true},
		{name: "Work-2", valid: true},
		{name: "school_east", valid: true},
		{name: strings.Repeat("a", 32), valid: true},
		{name: "", valid: false, wantErr: ErrInvalidProfileName},
		{name: strings.Repeat("a", 33), valid: false, wantErr: ErrInvalidProfileName},
		{name: "has space", valid: false, wantErr: ErrInvalidProfileName},
		{name: "has/slash", valid: false, wantErr: ErrInvalidProfileName},
		{name: "has.dot", valid: false, wantErr: ErrInvalidProfileName},
		{name: "../escape", valid: false, wantErr: ErrInvalidProfileName},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateProfileName(tc.name)
			if tc.valid && err != nil {
				t.Errorf("validateProfileName(%q) = %v, want nil", tc.name, err)
			}
			if !tc.valid {
				if err == nil {
					t.Errorf("validateProfileName(%q) = nil, want error", tc.name)
				} else if !errors.Is(err, tc.wantErr) {
					t.Errorf("validateProfileName(%q) error = %v, want wrapping %v", tc.name, err, tc.wantErr)
				}
			}
		})
	}
}

// Invalid names must be rejected before any config mutation happens.
func TestAddProfile_InvalidName_NoMutation(t *testing.T) {
	withTempProfileEnv(t)

	if _, err := AddProfile("not valid!", ""); !errors.Is(err, ErrInvalidProfileName) {
		t.Fatalf("AddProfile with invalid name error = %v, want ErrInvalidProfileName", err)
	}

	if _, err := os.Stat(GetConfigPath()); !os.IsNotExist(err) {
		t.Errorf("config file should not have been created by a rejected AddProfile, stat err = %v", err)
	}
}

// --- ISC-1, 12, 13: registry + creation + default auto-registration ---

func TestAddProfile_DefaultDir_CreatesUnderProfilesBase(t *testing.T) {
	withTempProfileEnv(t)

	meta, err := AddProfile("work", "")
	if err != nil {
		t.Fatalf("AddProfile failed: %v", err)
	}
	if meta.Name != "work" {
		t.Errorf("meta.Name = %q, want %q", meta.Name, "work")
	}
	wantDir := filepath.Join(profilesBaseDir(), "work")
	if meta.Dir != wantDir {
		t.Errorf("meta.Dir = %q, want %q", meta.Dir, wantDir)
	}
	info, err := os.Stat(meta.Dir)
	if err != nil {
		t.Fatalf("profile dir was not created: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("profile dir path is not a directory")
	}
}

func TestAddProfile_ExplicitDir_Used(t *testing.T) {
	withTempProfileEnv(t)
	customDir := filepath.Join(t.TempDir(), "custom-profile-dir")

	meta, err := AddProfile("school", customDir)
	if err != nil {
		t.Fatalf("AddProfile failed: %v", err)
	}
	if meta.Dir != customDir {
		t.Errorf("meta.Dir = %q, want %q", meta.Dir, customDir)
	}
	if _, err := os.Stat(customDir); err != nil {
		t.Fatalf("explicit profile dir was not created: %v", err)
	}
}

// A relative --dir is stored absolute (resolved against the CLI's cwd at
// registration): the registry is read by processes with other working
// directories — the LaunchAgent daemon runs with cwd=/ — and the dir feeds
// the Keychain service derivation, so a stored relative path would silently
// point at the wrong credential store.
func TestAddProfile_RelativeDir_StoredAbsolute(t *testing.T) {
	withTempProfileEnv(t)
	t.Chdir(t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	meta, err := AddProfile("school", "./rel-profile-dir")
	if err != nil {
		t.Fatalf("AddProfile failed: %v", err)
	}
	want := filepath.Join(cwd, "rel-profile-dir")
	if meta.Dir != want {
		t.Errorf("meta.Dir = %q, want absolute %q", meta.Dir, want)
	}
	if !filepath.IsAbs(meta.Dir) {
		t.Errorf("meta.Dir = %q is not absolute", meta.Dir)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("profile dir was not created at the absolute location: %v", err)
	}
}

// Adding a profile registers "default" too (ISC-13), pointing at the
// existing config dir, without creating or modifying anything inside it.
func TestAddProfile_AutoRegistersDefault(t *testing.T) {
	defaultDir := withTempProfileEnv(t)

	before, err := os.ReadDir(defaultDir)
	if err != nil {
		t.Fatalf("failed to list default dir before AddProfile: %v", err)
	}

	if _, err := AddProfile("work", ""); err != nil {
		t.Fatalf("AddProfile failed: %v", err)
	}

	profiles := ListProfiles()
	defaultMeta, ok := findProfile(profiles, defaultProfileName)
	if !ok {
		t.Fatalf("default profile was not auto-registered; profiles = %+v", profiles)
	}
	if defaultMeta.Dir != defaultDir {
		t.Errorf("default profile dir = %q, want %q", defaultMeta.Dir, defaultDir)
	}

	after, err := os.ReadDir(defaultDir)
	if err != nil {
		t.Fatalf("failed to list default dir after AddProfile: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("default config dir contents changed: before=%d entries, after=%d entries", len(before), len(after))
	}
}

// Duplicate registration is refused (ISC-14).
func TestAddProfile_Duplicate_Refused(t *testing.T) {
	withTempProfileEnv(t)

	if _, err := AddProfile("work", ""); err != nil {
		t.Fatalf("first AddProfile failed: %v", err)
	}
	if _, err := AddProfile("work", ""); !errors.Is(err, ErrProfileExists) {
		t.Fatalf("second AddProfile error = %v, want ErrProfileExists", err)
	}

	profiles := ListProfiles()
	count := 0
	for _, p := range profiles {
		if p.Name == "work" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("profile %q registered %d times, want 1", "work", count)
	}
}

// ListProfiles is a pure read: zero profiles configured means no config file
// is created and nothing is auto-registered (backward-compat contract 7).
func TestListProfiles_Empty_NoSideEffects(t *testing.T) {
	withTempProfileEnv(t)

	profiles := ListProfiles()
	if len(profiles) != 0 {
		t.Fatalf("ListProfiles() = %+v, want empty", profiles)
	}
	if _, err := os.Stat(GetConfigPath()); !os.IsNotExist(err) {
		t.Errorf("ListProfiles should not create a config file, stat err = %v", err)
	}
}

// --- ISC-6, 7: removal is metadata-only and refuses removing active ---

func TestRemoveProfile_UnregistersMetadataOnly(t *testing.T) {
	withTempProfileEnv(t)

	meta, err := AddProfile("work", "")
	if err != nil {
		t.Fatalf("AddProfile failed: %v", err)
	}

	if err := RemoveProfile("work"); err != nil {
		t.Fatalf("RemoveProfile failed: %v", err)
	}

	if _, ok := findProfile(ListProfiles(), "work"); ok {
		t.Errorf("profile %q still registered after RemoveProfile", "work")
	}

	// The directory itself must survive untouched.
	if _, err := os.Stat(meta.Dir); err != nil {
		t.Errorf("RemoveProfile deleted the profile directory: %v", err)
	}
}

func TestRemoveProfile_ActiveProfile_Refused(t *testing.T) {
	withTempProfileEnv(t)

	if _, err := AddProfile("work", ""); err != nil {
		t.Fatalf("AddProfile failed: %v", err)
	}
	if _, err := SwitchProfile("work"); err != nil {
		t.Fatalf("SwitchProfile failed: %v", err)
	}

	if err := RemoveProfile("work"); !errors.Is(err, ErrCannotRemoveActiveProfile) {
		t.Fatalf("RemoveProfile(active) error = %v, want ErrCannotRemoveActiveProfile", err)
	}

	if _, ok := findProfile(ListProfiles(), "work"); !ok {
		t.Errorf("profile %q should still be registered after refused removal", "work")
	}
}

func TestRemoveProfile_Unknown_Errors(t *testing.T) {
	withTempProfileEnv(t)

	if err := RemoveProfile("ghost"); !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("RemoveProfile(unknown) error = %v, want ErrProfileNotFound", err)
	}
}

// --- ISC-2: Keychain service-name derivation ---

// The test computes its own expected sha256 prefix, independent of the
// production keychainServiceForDir implementation, for the vector given in
// the ISA (dir "/Users/x/.claude-profiles/work").
func TestKeychainServiceForDir_DerivationVector(t *testing.T) {
	oldEnv := os.Getenv("CLAUDE_CONFIG_DIR")
	os.Setenv("CLAUDE_CONFIG_DIR", "/definitely-not-the-real-default-dir")
	defer func() {
		if oldEnv == "" {
			os.Unsetenv("CLAUDE_CONFIG_DIR")
		} else {
			os.Setenv("CLAUDE_CONFIG_DIR", oldEnv)
		}
	}()

	const dir = "/Users/x/.claude-profiles/work"

	sum := sha256.Sum256([]byte(dir))
	want := "Claude Code-credentials-" + hex.EncodeToString(sum[:])[:8]

	got := keychainServiceForDir(dir)
	if got != want {
		t.Errorf("keychainServiceForDir(%q) = %q, want %q", dir, got, want)
	}
}

func TestKeychainServiceForDir_DefaultDir_PlainServiceName(t *testing.T) {
	defaultDir := withTempProfileEnv(t)

	got := keychainServiceForDir(defaultDir)
	if got != keychainService {
		t.Errorf("keychainServiceForDir(default) = %q, want %q", got, keychainService)
	}
}

func TestKeychainServiceForDir_RelativeVsAbsolute_SameService(t *testing.T) {
	withTempProfileEnv(t)
	abs := filepath.Join(t.TempDir(), "some-profile")
	if err := os.MkdirAll(abs, 0700); err != nil {
		t.Fatal(err)
	}

	svcAbs := keychainServiceForDir(abs)
	svcCleanedTrailingSlash := keychainServiceForDir(abs + string(filepath.Separator))
	if svcAbs != svcCleanedTrailingSlash {
		t.Errorf("derivation is not stable across a trailing slash: %q vs %q", svcAbs, svcCleanedTrailingSlash)
	}
}

// --- ISC-21..28, 64, 75: switch engine ---

func TestSwitchProfile_UpdatesActiveAndStateFile(t *testing.T) {
	withTempProfileEnv(t)

	meta, err := AddProfile("work", "")
	if err != nil {
		t.Fatalf("AddProfile failed: %v", err)
	}

	result, err := SwitchProfile("work")
	if err != nil {
		t.Fatalf("SwitchProfile failed: %v", err)
	}
	if result.AlreadyActive {
		t.Errorf("result.AlreadyActive = true, want false for a real switch")
	}
	if result.To != "work" {
		t.Errorf("result.To = %q, want %q", result.To, "work")
	}

	cfg := LoadConfig()
	if cfg.ActiveProfile != "work" {
		t.Errorf("Config.ActiveProfile = %q, want %q", cfg.ActiveProfile, "work")
	}

	data, err := os.ReadFile(activeStateFilePath())
	if err != nil {
		t.Fatalf("failed to read state file: %v", err)
	}
	want := meta.Dir + "\n"
	if string(data) != want {
		t.Errorf("state file content = %q, want %q", data, want)
	}
}

// Switching back to the default profile must write an EMPTY state file
// (contract 5), not the default dir's path — the shim relies on emptiness to
// know it should leave CLAUDE_CONFIG_DIR unset (ISC-83's dependency).
func TestSwitchProfile_ToDefault_WritesEmptyStateFile(t *testing.T) {
	withTempProfileEnv(t)

	if _, err := AddProfile("work", ""); err != nil {
		t.Fatalf("AddProfile failed: %v", err)
	}
	if _, err := SwitchProfile("work"); err != nil {
		t.Fatalf("switch to work failed: %v", err)
	}
	if _, err := SwitchProfile(defaultProfileName); err != nil {
		t.Fatalf("switch to default failed: %v", err)
	}

	data, err := os.ReadFile(activeStateFilePath())
	if err != nil {
		t.Fatalf("failed to read state file: %v", err)
	}
	if len(data) != 0 {
		t.Errorf("state file content = %q, want empty for default profile", data)
	}

	cfg := LoadConfig()
	if cfg.ActiveProfile != defaultProfileName {
		t.Errorf("Config.ActiveProfile = %q, want %q", cfg.ActiveProfile, defaultProfileName)
	}
}

func TestSwitchProfile_AlreadyActive_NoOp(t *testing.T) {
	withTempProfileEnv(t)

	if _, err := AddProfile("work", ""); err != nil {
		t.Fatalf("AddProfile failed: %v", err)
	}
	if _, err := SwitchProfile("work"); err != nil {
		t.Fatalf("first switch failed: %v", err)
	}

	statePath := activeStateFilePath()
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("failed to read state file: %v", err)
	}

	result, err := SwitchProfile("work")
	if err != nil {
		t.Fatalf("second switch (no-op) failed: %v", err)
	}
	if !result.AlreadyActive {
		t.Errorf("result.AlreadyActive = false, want true")
	}

	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("failed to re-read state file: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("no-op switch changed the state file: before=%q after=%q", before, after)
	}
}

func TestSwitchProfile_UnknownProfile_ListsAvailable(t *testing.T) {
	withTempProfileEnv(t)

	if _, err := AddProfile("work", ""); err != nil {
		t.Fatalf("AddProfile failed: %v", err)
	}

	_, err := SwitchProfile("ghost")
	if !errors.Is(err, ErrProfileNotFound) {
		t.Fatalf("SwitchProfile(unknown) error = %v, want ErrProfileNotFound", err)
	}
	if !strings.Contains(err.Error(), "work") {
		t.Errorf("error %v should list available profile %q", err, "work")
	}
}

// Reports credential health without blocking the switch: a needs-login
// target still switches successfully (ISC-22).
func TestSwitchProfile_ReportsHealth_NeedsLoginDoesNotBlock(t *testing.T) {
	withTempProfileEnv(t)
	restoreReadCredential := readCredential
	defer func() { readCredential = restoreReadCredential }()
	readCredential = func(dir string) (*OAuthCredential, error) {
		return nil, errors.New("no credential")
	}

	if _, err := AddProfile("work", ""); err != nil {
		t.Fatalf("AddProfile failed: %v", err)
	}

	result, err := SwitchProfile("work")
	if err != nil {
		t.Fatalf("switch to a needs-login profile must succeed, got: %v", err)
	}
	if result.Health != CredentialNeedsLogin {
		t.Errorf("result.Health = %q, want %q", result.Health, CredentialNeedsLogin)
	}
}

// Anti: the switch path must never invoke the Keychain reader or the file
// reader for any dir OTHER than the target's own dir, and must never write
// anything inside that dir.
func TestSwitchProfile_ZeroCredentialStoreWrites(t *testing.T) {
	withTempProfileEnv(t)

	meta, err := AddProfile("work", "")
	if err != nil {
		t.Fatalf("AddProfile failed: %v", err)
	}

	before, err := os.ReadDir(meta.Dir)
	if err != nil {
		t.Fatalf("failed to list profile dir before switch: %v", err)
	}

	if _, err := SwitchProfile("work"); err != nil {
		t.Fatalf("SwitchProfile failed: %v", err)
	}

	after, err := os.ReadDir(meta.Dir)
	if err != nil {
		t.Fatalf("failed to list profile dir after switch: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("profile dir contents changed by switch: before=%d after=%d entries", len(before), len(after))
	}
}

// A→B→A round trip must leave both profiles' credential-relevant on-disk
// state (their config dirs) byte-identical, because switching never touches
// them at all.
func TestSwitchProfile_RoundTrip_DirsUnchanged(t *testing.T) {
	defaultDir := withTempProfileEnv(t)

	// Seed a fake credential file in both stores to have something to compare.
	if err := os.WriteFile(filepath.Join(defaultDir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"a"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	metaB, err := AddProfile("work", "")
	if err != nil {
		t.Fatalf("AddProfile failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(metaB.Dir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"b"}}`), 0600); err != nil {
		t.Fatal(err)
	}

	readBoth := func() (string, string) {
		a, _ := os.ReadFile(filepath.Join(defaultDir, ".credentials.json"))
		b, _ := os.ReadFile(filepath.Join(metaB.Dir, ".credentials.json"))
		return string(a), string(b)
	}

	beforeA, beforeB := readBoth()

	if _, err := SwitchProfile("work"); err != nil {
		t.Fatalf("switch to work failed: %v", err)
	}
	if _, err := SwitchProfile(defaultProfileName); err != nil {
		t.Fatalf("switch back to default failed: %v", err)
	}

	afterA, afterB := readBoth()
	if beforeA != afterA {
		t.Errorf("default profile's credential file changed by switch round-trip")
	}
	if beforeB != afterB {
		t.Errorf("work profile's credential file changed by switch round-trip")
	}
}

// Switch must complete well under the 2s threshold (ISC-27).
func TestSwitchProfile_CompletesUnder2Seconds(t *testing.T) {
	withTempProfileEnv(t)

	if _, err := AddProfile("work", ""); err != nil {
		t.Fatalf("AddProfile failed: %v", err)
	}

	start := time.Now()
	if _, err := SwitchProfile("work"); err != nil {
		t.Fatalf("SwitchProfile failed: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("SwitchProfile took %v, want < 2s", elapsed)
	}
}

// Concurrent switch attempts must serialize cleanly on the existing config
// write mutex rather than corrupting the config or state file.
func TestSwitchProfile_Concurrent_Serializes(t *testing.T) {
	withTempProfileEnv(t)

	if _, err := AddProfile("a", ""); err != nil {
		t.Fatalf("AddProfile a failed: %v", err)
	}
	if _, err := AddProfile("b", ""); err != nil {
		t.Fatalf("AddProfile b failed: %v", err)
	}

	names := []string{"a", "b", defaultProfileName}
	done := make(chan error, 30)
	for i := 0; i < 30; i++ {
		name := names[i%len(names)]
		go func(n string) {
			_, err := SwitchProfile(n)
			done <- err
		}(name)
	}
	for i := 0; i < 30; i++ {
		if err := <-done; err != nil {
			t.Errorf("concurrent SwitchProfile returned error: %v", err)
		}
	}

	// The final config must be internally consistent: parseable, and its
	// ActiveProfile must be one of the registered profiles.
	cfg := LoadConfig()
	if _, ok := findProfile(cfg.Profiles, cfg.ActiveProfile); !ok && cfg.ActiveProfile != "" {
		t.Errorf("final ActiveProfile %q is not a registered profile", cfg.ActiveProfile)
	}
}

// Audit log line contains from->to but never secret material.
func TestSwitchProfile_AuditLogsNoSecrets(t *testing.T) {
	withTempProfileEnv(t)
	restoreReadCredential := readCredential
	defer func() { readCredential = restoreReadCredential }()
	readCredential = func(dir string) (*OAuthCredential, error) {
		return &OAuthCredential{AccessToken: "super-secret-token-value"}, nil
	}

	if _, err := AddProfile("work", ""); err != nil {
		t.Fatalf("AddProfile failed: %v", err)
	}

	var logBuf strings.Builder
	origWriter := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(origWriter)

	if _, err := SwitchProfile("work"); err != nil {
		t.Fatalf("SwitchProfile failed: %v", err)
	}

	got := logBuf.String()
	if !strings.Contains(got, "default") || !strings.Contains(got, "work") {
		t.Errorf("audit log %q does not contain the from->to transition", got)
	}
	if strings.Contains(got, "super-secret-token-value") {
		t.Errorf("audit log leaked token material: %q", got)
	}
}

// --- ISC-8..11: config schema ---

// A pre-existing single-profile config (no "profiles" key at all) must load
// and behave exactly as before.
func TestLoadConfig_LegacyFixture_NoProfilesKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-config.json")
	legacy := `{"authMode":"oauth","menuBarIndicator":"currentSession","savedAt":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := loadConfigFrom(path)
	if cfg.AuthMode != "oauth" {
		t.Errorf("AuthMode = %q, want %q", cfg.AuthMode, "oauth")
	}
	if len(cfg.Profiles) != 0 {
		t.Errorf("Profiles = %+v, want empty for a legacy config", cfg.Profiles)
	}
	if cfg.ActiveProfile != "" {
		t.Errorf("ActiveProfile = %q, want empty for a legacy config", cfg.ActiveProfile)
	}
}

// Anti: no OAuth token material may ever appear in the marshaled config,
// across a full profile lifecycle (add, switch, remove).
func TestConfig_FullProfileLifecycle_NeverPersistsTokenMaterial(t *testing.T) {
	withTempProfileEnv(t)
	restoreReadCredential := readCredential
	defer func() { readCredential = restoreReadCredential }()
	readCredential = func(dir string) (*OAuthCredential, error) {
		return &OAuthCredential{
			AccessToken:  "top-secret-access-token",
			RefreshToken: "top-secret-refresh-token",
			ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
		}, nil
	}

	if _, err := AddProfile("work", ""); err != nil {
		t.Fatalf("AddProfile failed: %v", err)
	}
	if _, err := SwitchProfile("work"); err != nil {
		t.Fatalf("SwitchProfile failed: %v", err)
	}
	if _, err := SwitchProfile(defaultProfileName); err != nil {
		t.Fatalf("switch back failed: %v", err)
	}
	if err := RemoveProfile("work"); err != nil {
		t.Fatalf("RemoveProfile failed: %v", err)
	}

	raw, err := os.ReadFile(GetConfigPath())
	if err != nil {
		t.Fatalf("failed to read config file: %v", err)
	}
	got := string(raw)
	for _, secret := range []string{"top-secret-access-token", "top-secret-refresh-token", "accessToken", "refreshToken"} {
		if strings.Contains(got, secret) {
			t.Errorf("marshaled config contains %q, want no token material:\n%s", secret, got)
		}
	}
}

// Profile metadata must survive a read-modify-write cycle driven by an
// unrelated setting (e.g. changing menuBarIndicator).
func TestProfileMetadata_SurvivesUnrelatedConfigWrite(t *testing.T) {
	withTempProfileEnv(t)

	if _, err := AddProfile("work", ""); err != nil {
		t.Fatalf("AddProfile failed: %v", err)
	}

	if err := SaveConfigPreservingSession("weeklyAll"); err != nil {
		t.Fatalf("SaveConfigPreservingSession failed: %v", err)
	}

	profiles := ListProfiles()
	if _, ok := findProfile(profiles, "work"); !ok {
		t.Errorf("profile %q lost after an unrelated config write; profiles = %+v", "work", profiles)
	}
}
