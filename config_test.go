package main

import (
	"os"
	"path/filepath"
	"slices"
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

// A selection must survive a save/load round-trip — including a scoped row's
// key, which carries a colon and a model name, and the profile it belongs to —
// or a click would not stick.
func TestConfigRoundTripsSelection(t *testing.T) {
	withTempHome(t)
	const work = "/Users/alice/.claude-work"
	for _, ind := range []Indicator{
		{Dir: "", Key: autoKey}, {Dir: "", Key: kindSession}, {Dir: work, Key: kindWeeklyAll},
		{Dir: work, Key: "weekly_scoped:Fable"}, {Dir: "", Key: extraUsageKey},
	} {
		if err := SaveConfig(Config{Profiles: []string{"", work}, MenuBarIndicator: ind}); err != nil {
			t.Fatalf("save %+v failed: %v", ind, err)
		}
		if got := LoadConfig().MenuBarIndicator; got != ind {
			t.Errorf("selection %+v not preserved, got %+v", ind, got)
		}
	}
}

// With nothing chosen — a missing or unreadable config — the monitor watches
// the default environment and the menu bar follows the server's pick, so a
// plan without a 5-hour limit does not start on "--".
func TestLoadConfigDefaults(t *testing.T) {
	home := withTempHome(t)
	path := filepath.Join(home, ".claude-monitor-lite.json")

	check := func(name string) {
		t.Helper()
		config := LoadConfig()
		if !slices.Equal(config.Profiles, []string{""}) {
			t.Errorf("%s: profiles = %q, want the default environment alone", name, config.Profiles)
		}
		if want := (Indicator{Dir: "", Key: autoKey}); config.MenuBarIndicator != want {
			t.Errorf("%s: selection = %+v, want %+v", name, config.MenuBarIndicator, want)
		}
	}
	check("missing config")
	for _, body := range []string{`not json`, `{}`, `{"profiles":[]}`, `{"menuBarIndicator":{"key":""}}`} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		check("config " + body)
	}
}

// The profile list is what decides whose usage is polled, so it is cleaned
// on the way in: one group per environment however a dir is spelled, and a
// selection that points at a profile no longer watched falls back to the
// first one rather than leaving the menu bar on an account that is gone.
func TestLoadConfigProfiles(t *testing.T) {
	home := withTempHome(t)
	path := filepath.Join(home, ".claude-monitor-lite.json")
	body := `{"profiles":["/Users/alice/.claude-work/","","/Users/alice/x/../.claude-work"],
		"menuBarIndicator":{"dir":"/Users/alice/.claude-gone","key":"session"}}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	config := LoadConfig()
	if want := []string{"/Users/alice/.claude-work", ""}; !slices.Equal(config.Profiles, want) {
		t.Errorf("profiles = %q, want %q", config.Profiles, want)
	}
	if want := (Indicator{Dir: "/Users/alice/.claude-work", Key: autoKey}); config.MenuBarIndicator != want {
		t.Errorf("selection = %+v, want %+v", config.MenuBarIndicator, want)
	}
}

// The monitor's own files are single, whatever it watches. The files kept
// per profile use the plain names for the default environment and carry the
// profile's id for any other, or two profiles would overwrite each other's
// usage cache and status line feed.
func TestProfileFiles(t *testing.T) {
	home := withTempHome(t)

	for _, ext := range []string{".pid", ".json", ".log"} {
		if got, want := monitorFile(ext), filepath.Join(home, ".claude-monitor-lite"+ext); got != want {
			t.Errorf("monitorFile(%s) = %q, want %q", ext, got, want)
		}
	}
	if got, want := newProfile("").file(".usage.json"), filepath.Join(home, ".claude-monitor-lite.usage.json"); got != want {
		t.Errorf("default profile file = %q, want %q", got, want)
	}

	// be865d75 is sha256(dir)[:8], the hash Claude Code names this dir's files by.
	work := newProfile("/Users/alice/.claude-work/")
	for _, ext := range []string{".usage.json", ".statusline-usage.json", ".statusline-wrap"} {
		if got, want := work.file(ext), filepath.Join(home, ".claude-monitor-lite.claude-work-be865d75"+ext); got != want {
			t.Errorf("file(%s) = %q, want %q", ext, got, want)
		}
	}
	// A dotted dir name must not produce a doubled dot or path separator.
	if got := work.folder(); got != "claude-work" {
		t.Errorf("folder() = %q, want %q", got, "claude-work")
	}
}

// Two environments whose dirs share a folder name are still different
// accounts and must not share their files.
func TestProfileIDDistinguishesSameFolderName(t *testing.T) {
	work, home := newProfile("/Users/x/work/.claude").id(), newProfile("/Users/x/home/.claude").id()
	if home == work {
		t.Errorf("id() = %q for both dirs, want distinct", home)
	}
}

// A relative dir (as typed for `add` in a shell) and its absolute form (as
// Claude Code's CLAUDE_CONFIG_DIR) are the same environment and must resolve
// to the same profile, or its status line would feed a file nobody reads.
func TestProfileIgnoresPathSpelling(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if relative, abs := newProfile("env"), newProfile(filepath.Join(dir, "env")); relative != abs {
		t.Errorf("relative = %+v, absolute = %+v, want equal", relative, abs)
	}
}

// Where Claude Code keeps a profile's files differs between the default
// environment and a CLAUDE_CONFIG_DIR one; reading the wrong file would name
// a group after another profile's account.
func TestProfilePaths(t *testing.T) {
	home := withTempHome(t)
	def, work := newProfile(""), newProfile("/Users/alice/.claude-work")
	for _, tc := range []struct{ name, got, want string }{
		{"default config dir", def.configDir(), filepath.Join(home, ".claude")},
		{"default account file", def.accountFile(), filepath.Join(home, ".claude.json")},
		{"work config dir", work.configDir(), "/Users/alice/.claude-work"},
		{"work account file", work.accountFile(), "/Users/alice/.claude-work/.claude.json"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// A group is named after the account logged in to its profile, always in the
// same shape: person, whose account it is, plan in brackets. The names come
// from Claude Code's own bookkeeping, so each rule here guards what the menu —
// and any screenshot of it — shows: an organization only for a seat in one,
// and "Personal" rather than the "<email>'s Organization" Claude gives a
// personal account.
func TestAccountName(t *testing.T) {
	const personalOrg = "ada@example.com's Organization"
	for _, tc := range []struct {
		name    string
		account account
		want    string
	}{
		{"team seat", account{DisplayName: "Ada", OrganizationName: "Acme", OrganizationType: "claude_team"}, "Ada · Acme (Team)"},
		{"enterprise seat", account{DisplayName: "Ada", OrganizationName: "Acme", OrganizationType: "claude_enterprise"}, "Ada · Acme (Enterprise)"},
		{"personal plan", account{DisplayName: "Ada", OrganizationName: personalOrg, OrganizationType: "claude_max", PlanDisplayName: "Max 20x"}, "Ada · Personal (Max 20x)"},
		{"personal plan without a plan name", account{DisplayName: "Ada", OrganizationName: personalOrg, OrganizationType: "claude_max_5x"}, "Ada · Personal (Max 5x)"},
		{"no display name", account{OrganizationName: "Acme", OrganizationType: "claude_team"}, "Acme (Team)"},
		{"no display name on a personal plan", account{OrganizationName: personalOrg, OrganizationType: "claude_pro"}, "Personal (Pro)"},
		{"unknown type", account{DisplayName: "Ada", OrganizationName: personalOrg, OrganizationType: "other"}, "Ada"},
		{"nothing recorded", account{}, ""},
	} {
		if got := tc.account.name(); got != tc.want {
			t.Errorf("%s: name() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Every group must read differently, or two accounts could not be told apart
// in the menu: a profile without an account name falls back to its folder,
// and profiles sharing a name have their folder appended.
func TestProfileLabels(t *testing.T) {
	profiles := []profile{newProfile(""), newProfile("/Users/alice/.claude-work"), newProfile("/Users/alice/.claude-side"), newProfile("/Users/alice/.claude-new")}
	max := account{DisplayName: "Ada", OrganizationType: "claude_max"}
	accounts := []account{max, {DisplayName: "Ada", OrganizationName: "Acme", OrganizationType: "claude_team"}, max, {}}

	got := profileLabels(profiles, accounts)
	want := []string{"Ada · Personal (Max) [default]", "Ada · Acme (Team)", "Ada · Personal (Max) [claude-side]", "claude-new"}
	if !slices.Equal(got, want) {
		t.Errorf("profileLabels = %q, want %q", got, want)
	}
}

// The account is read from a file Claude Code owns and may reshape; an
// unreadable one must leave the profile nameless (so it falls back to its
// folder), never stop the monitor.
func TestReadAccount(t *testing.T) {
	home := withTempHome(t)
	p := newProfile("")
	if got := p.readAccount(); got != (account{}) {
		t.Errorf("missing file: got %+v, want empty", got)
	}
	for body, want := range map[string]account{
		`not json`:                 {},
		`{"oauthAccount":"moved"}`: {},
		`{"oauthAccount":{"displayName":"Ada","organizationName":"Acme","organizationType":"claude_team","emailAddress":"ada@example.com"}}`: {DisplayName: "Ada", OrganizationName: "Acme", OrganizationType: "claude_team"},
	} {
		if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if got := p.readAccount(); got != want {
			t.Errorf("%s: got %+v, want %+v", body, got, want)
		}
	}
}
