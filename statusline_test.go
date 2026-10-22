package main

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The status line input is Claude Code's documented shape: percentages plus
// epoch-second resets. Both windows must land in the same fields polling
// fills, or the menu bar would flip between two readings of one window.
func TestStatusLineLimits(t *testing.T) {
	input := `{"model":{"id":"x"},"rate_limits":{
		"five_hour":{"used_percentage":42.5,"resets_at":1761170400},
		"seven_day":{"used_percentage":80,"resets_at":1761181200}}}`
	limits, err := statusLineLimits([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if limits.FiveHour.Utilization != 42.5 || !limits.FiveHour.ResetsAt.Equal(time.Unix(1761170400, 0)) {
		t.Errorf("five_hour = %+v", limits.FiveHour)
	}
	if limits.SevenDay.Utilization != 80 || !limits.SevenDay.ResetsAt.Equal(time.Unix(1761181200, 0)) {
		t.Errorf("seven_day = %+v", limits.SevenDay)
	}
}

// API-key users, the first render before any API response, and a render whose
// windows have all expired carry no usage. Writing an empty update would make
// the daemon think fresh data arrived and blank out the polled values.
func TestStatusLineLimitsNothingToReport(t *testing.T) {
	for name, input := range map[string]string{
		"no rate_limits":     `{"model":{"id":"x"}}`,
		"only a spend limit": `{"rate_limits":{"spend_limit":{"used_percentage":5,"resets_at":1}}}`,
		"empty rate_limits":  `{"rate_limits":{}}`,
		"null rate_limits":   `{"rate_limits":null}`,
	} {
		limits, err := statusLineLimits([]byte(input))
		if err != nil || limits != nil {
			t.Errorf("%s: got %+v, %v; want nil, nil", name, limits, err)
		}
	}
}

// The status line only knows two windows. Merging must keep what only polling
// sees (extra usage, per-model weekly limits), and a window the update lacks
// must keep its polled value rather than vanish from the menu.
func TestMergeStatusLine(t *testing.T) {
	extra := 12.0
	polled := &UsageLimits{
		FiveHour:   &UsageLimit{Utilization: 10},
		SevenDay:   &UsageLimit{Utilization: 50},
		ExtraUsage: &ExtraUsageInfo{Utilization: &extra},
		Limits:     []LimitEntry{{Percent: 3}},
	}
	live := &UsageLimits{FiveHour: &UsageLimit{Utilization: 15}}

	merged := mergeStatusLine(polled, live)
	if merged.FiveHour.Utilization != 15 {
		t.Errorf("five_hour = %v, want the live 15", merged.FiveHour.Utilization)
	}
	if merged.SevenDay.Utilization != 50 {
		t.Errorf("seven_day = %v, want the polled 50 kept", merged.SevenDay.Utilization)
	}
	if merged.ExtraUsage != polled.ExtraUsage || len(merged.Limits) != 1 {
		t.Error("poll-only fields were dropped")
	}
	if polled.FiveHour.Utilization != 10 {
		t.Error("merge mutated the cached poll")
	}

	if got := mergeStatusLine(nil, live); got.FiveHour.Utilization != 15 {
		t.Errorf("before any poll: five_hour = %+v", got.FiveHour)
	}
}

// The daemon orders status line updates against polls by the usage file's
// mtime, so a re-render with unchanged values must not touch the file —
// otherwise every 2s render would look like news and override a newer poll.
func TestSaveStatusLineUsageSkipsUnchanged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	input := []byte(`{"rate_limits":{"five_hour":{"used_percentage":4,"resets_at":1761170400}}}`)

	if err := saveStatusLineUsage(input); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(statusLineUsageFile(), past, past); err != nil {
		t.Fatal(err)
	}

	if err := saveStatusLineUsage(input); err != nil {
		t.Fatal(err)
	}
	if limits, _, _ := readStatusLineUpdate(past); limits != nil {
		t.Error("unchanged render rewrote the usage file")
	}

	changed := []byte(`{"rate_limits":{"five_hour":{"used_percentage":5,"resets_at":1761170400}}}`)
	if err := saveStatusLineUsage(changed); err != nil {
		t.Fatal(err)
	}
	limits, _, err := readStatusLineUpdate(past)
	if err != nil || limits == nil || limits.FiveHour.Utilization != 5 {
		t.Errorf("changed render not picked up: %+v, %v", limits, err)
	}
}

const sampleSettings = `{
  "$schema": "https://json.schemastore.org/claude-code-settings.json",
  "env": {
    "A": "1"
  },
  "statusLine": {
    "type": "command",
    "command": "ccstatusline 2>/dev/null && echo '<ok>'",
    "refreshInterval": 2
  },
  "permissions": {
    "allow": []
  }
}
`

// Install edits a file the user owns. It must swap only the command — key
// order, other settings and refreshInterval survive — and hand back the
// original so the wrapper can keep running it.
func TestInstallStatusLineSettings(t *testing.T) {
	const feed = "'/opt/homebrew/bin/claude-monitor-lite' statusline"
	out, original, already, err := installStatusLineSettings([]byte(sampleSettings), feed)
	if err != nil {
		t.Fatal(err)
	}
	if already {
		t.Error("fresh install reported as already installed")
	}
	if original != "ccstatusline 2>/dev/null && echo '<ok>'" {
		t.Errorf("original = %q", original)
	}
	want := strings.Replace(sampleSettings,
		`"command": "ccstatusline 2>/dev/null && echo '<ok>'"`,
		`"command": "'/opt/homebrew/bin/claude-monitor-lite' statusline"`, 1)
	if string(out) != want {
		t.Errorf("settings.json rewritten beyond the command:\n%s", out)
	}
}

// Running install twice must not record our own feed as the "original" —
// the wrapper would then run itself instead of the user's status line.
func TestInstallStatusLineSettingsIdempotent(t *testing.T) {
	const feed = "'/usr/local/bin/claude-monitor-lite' statusline"
	once, _, _, err := installStatusLineSettings([]byte(sampleSettings), feed)
	if err != nil {
		t.Fatal(err)
	}
	twice, original, already, err := installStatusLineSettings(once, feed)
	if err != nil {
		t.Fatal(err)
	}
	if !already || !isStatusLineFeed(original) {
		t.Errorf("second install: already=%v original=%q", already, original)
	}
	if string(twice) != string(once) {
		t.Errorf("second install changed settings:\n%s", twice)
	}
}

// With no status line configured, install adds one and uninstall removes it
// again, leaving the user's settings as they were.
func TestStatusLineRoundTripWithoutStatusLine(t *testing.T) {
	const settings = "{\n  \"model\": \"opus\"\n}\n"
	out, original, _, err := installStatusLineSettings([]byte(settings), "claude-monitor-lite statusline")
	if err != nil {
		t.Fatal(err)
	}
	if original != "" {
		t.Errorf("original = %q, want empty", original)
	}
	restored, ours, err := uninstallStatusLineSettings(out, original, "claude-monitor-lite statusline")
	if err != nil || !ours {
		t.Fatalf("uninstall: ours=%v err=%v", ours, err)
	}
	if string(restored) != settings {
		t.Errorf("round trip changed settings:\n%s", restored)
	}
}

func TestStatusLineRoundTripRestoresOriginal(t *testing.T) {
	out, original, _, err := installStatusLineSettings([]byte(sampleSettings), "claude-monitor-lite statusline")
	if err != nil {
		t.Fatal(err)
	}
	restored, ours, err := uninstallStatusLineSettings(out, original, "claude-monitor-lite statusline")
	if err != nil || !ours {
		t.Fatalf("uninstall: ours=%v err=%v", ours, err)
	}
	if string(restored) != sampleSettings {
		t.Errorf("round trip changed settings:\n%s", restored)
	}
}

// If the user re-pointed their status line after installing, uninstall must
// not overwrite their newer choice with the old original.
func TestUninstallLeavesForeignStatusLine(t *testing.T) {
	out, ours, err := uninstallStatusLineSettings([]byte(sampleSettings), "old command", "claude-monitor-lite statusline")
	if err != nil {
		t.Fatal(err)
	}
	if ours || string(out) != sampleSettings {
		t.Errorf("foreign status line touched: ours=%v\n%s", ours, out)
	}
}

func TestStatusLineFeedCommand(t *testing.T) {
	cases := []struct{ bin, dir, want string }{
		{"/opt/homebrew/bin/claude-monitor-lite", "", "'/opt/homebrew/bin/claude-monitor-lite' statusline"},
		{"/Users/a b/bin/claude-monitor-lite", "/Users/a b/.claude-work",
			"CLAUDE_CONFIG_DIR='/Users/a b/.claude-work' '/Users/a b/bin/claude-monitor-lite' statusline"},
		{"/tmp/it's/claude-monitor-lite", "", `'/tmp/it'\''s/claude-monitor-lite' statusline`},
	}
	for _, tc := range cases {
		got := statusLineFeedCommand(tc.bin, tc.dir)
		if got != tc.want {
			t.Errorf("statusLineFeedCommand(%q, %q) = %q, want %q", tc.bin, tc.dir, got, tc.want)
		}
		if !isStatusLineFeed(got) {
			t.Errorf("isStatusLineFeed(%q) = false", got)
		}
	}
	if isStatusLineFeed("ccstatusline") {
		t.Error("a foreign command was taken for ours")
	}
}

// Every open Claude Code session re-renders with its own last-seen usage, so
// an idle session keeps repeating an old, lower figure. Within one window
// usage only grows; a later reset is a new window that wins outright.
func TestNewerWindow(t *testing.T) {
	reset := time.Date(2026, 10, 22, 22, 50, 0, 0, time.UTC)
	at := func(util float64, r time.Time) *UsageLimit { return &UsageLimit{Utilization: util, ResetsAt: r} }

	cases := []struct {
		name string
		a, b *UsageLimit
		want float64
	}{
		{"idle session's lower figure loses", at(51, reset), at(34, reset), 51},
		{"active session's higher figure wins", at(34, reset), at(51, reset), 51},
		{"polled sub-second reset is the same window", at(51, reset.Add(-440*time.Millisecond)), at(47, reset), 51},
		{"new window wins despite lower usage", at(90, reset), at(2, reset.Add(5*time.Hour)), 2},
		{"old window loses despite higher usage", at(2, reset.Add(5*time.Hour)), at(90, reset), 2},
		{"missing reading keeps the other", nil, at(7, reset), 7},
	}
	for _, tc := range cases {
		if got := newerWindow(tc.a, tc.b); got.Utilization != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got.Utilization, tc.want)
		}
	}
}

// Several sessions write the same file. After an active session reports 51%,
// an idle one repeating 34% must neither lower the file nor bump its mtime —
// either would make the menu bar flicker between the two.
func TestSaveStatusLineUsageAcrossSessions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	render := func(util int) []byte {
		return []byte(`{"rate_limits":{"five_hour":{"used_percentage":` + strconv.Itoa(util) + `,"resets_at":1761173400}}}`)
	}

	if err := saveStatusLineUsage(render(51)); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(statusLineUsageFile(), past, past); err != nil {
		t.Fatal(err)
	}
	if err := saveStatusLineUsage(render(34)); err != nil {
		t.Fatal(err)
	}
	if limits, _, _ := readStatusLineUpdate(past); limits != nil {
		t.Errorf("idle session's stale render advanced the file: %+v", limits.FiveHour)
	}
}

// A status line update older in effect than the last poll must not roll the
// menu bar back: the poll saw 51%, an idle session's file says 47%.
func TestMergeStatusLineKeepsFresherPoll(t *testing.T) {
	reset := time.Date(2026, 10, 22, 22, 50, 0, 0, time.UTC)
	polled := &UsageLimits{FiveHour: &UsageLimit{Utilization: 51, ResetsAt: reset.Add(393 * time.Millisecond)}}
	live := &UsageLimits{FiveHour: &UsageLimit{Utilization: 47, ResetsAt: reset}}
	if got := mergeStatusLine(polled, live).FiveHour.Utilization; got != 51 {
		t.Errorf("five_hour = %v, want the polled 51", got)
	}
}

// isStatusLineFeed recognises our feed by name. A binary built or renamed to
// something else slips past it, and the second install must still not record
// our own command as the user's original.
func TestInstallStatusLineSettingsRenamedBinary(t *testing.T) {
	const feed = "'/tmp/cml' statusline"
	once, _, _, err := installStatusLineSettings([]byte(sampleSettings), feed)
	if err != nil {
		t.Fatal(err)
	}
	_, original, already, err := installStatusLineSettings(once, feed)
	if err != nil {
		t.Fatal(err)
	}
	if !already {
		t.Errorf("reinstall under a renamed binary recorded %q as the original", original)
	}
	if _, ours, _ := uninstallStatusLineSettings(once, "", feed); !ours {
		t.Error("uninstall did not recognise its own renamed feed")
	}
}

// If the recorded original is somehow our own feed, the wrapper would start
// itself on every render until the user runs out of processes. The guard
// variable set on the wrapped command must stop the nested run.
func TestRunStatusLineGuardStopsRecursion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	marker := home + "/ran"
	if err := os.WriteFile(statusLineWrapFile(), []byte("touch "+marker), 0600); err != nil {
		t.Fatal(err)
	}
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	orig := os.Stdin
	os.Stdin = stdin
	defer func() { os.Stdin = orig }()

	t.Setenv(statusLineGuardEnv, "1")
	runStatusLine()
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("nested run executed the wrapped command")
	}

	t.Setenv(statusLineGuardEnv, "")
	runStatusLine()
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("top-level run did not execute the wrapped command")
	}
}
