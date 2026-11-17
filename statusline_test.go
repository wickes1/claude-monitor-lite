package main

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The status line input is Claude Code's documented shape: percentages plus
// epoch-second resets, under the session that rendered it. Both windows must
// land in the same fields polling fills, or the menu bar would flip between
// two readings of one window.
func TestStatusLineLimits(t *testing.T) {
	input := `{"session_id":"abc","model":{"id":"x"},"rate_limits":{
		"five_hour":{"used_percentage":42.5,"resets_at":1761170400},
		"seven_day":{"used_percentage":80,"resets_at":1761181200}}}`
	session, limits, err := statusLineLimits([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	if session != "abc" {
		t.Errorf("session = %q, want abc", session)
	}
	if limits.FiveHour.Utilization != 42.5 || !limits.FiveHour.ResetsAt.Equal(time.Unix(1761170400, 0)) {
		t.Errorf("five_hour = %+v", limits.FiveHour)
	}
	if limits.SevenDay.Utilization != 80 || !limits.SevenDay.ResetsAt.Equal(time.Unix(1761181200, 0)) {
		t.Errorf("seven_day = %+v", limits.SevenDay)
	}
}

// API-key users, the first render before any API response, and a render whose
// windows have all expired carry no usage. Recording an empty reading would
// make the daemon think fresh data arrived.
func TestStatusLineLimitsNothingToReport(t *testing.T) {
	for name, input := range map[string]string{
		"no rate_limits":         `{"model":{"id":"x"}}`,
		"only a spend limit":     `{"rate_limits":{"spend_limit":{"used_percentage":5,"resets_at":1}}}`,
		"empty rate_limits":      `{"rate_limits":{}}`,
		"null rate_limits":       `{"rate_limits":null}`,
		"window without a reset": `{"rate_limits":{"five_hour":{"used_percentage":5}}}`,
	} {
		_, limits, err := statusLineLimits([]byte(input))
		if err != nil || limits != nil {
			t.Errorf("%s: got %+v, %v; want nil, nil", name, limits, err)
		}
	}
}

// The status line only knows two windows. Merging must move the session row
// — what the menu shows — with five_hour, keep a window the update lacks at
// its polled value, and keep what only polling sees (usage credits, per-model
// weekly limits).
func TestMergeStatusLine(t *testing.T) {
	extra := 12.0
	polled := &UsageLimits{
		ExtraUsage: &ExtraUsageInfo{Utilization: &extra},
		Limits: []LimitEntry{
			{Kind: kindSession, Percent: 10},
			{Kind: kindWeeklyAll, Percent: 50},
			{Kind: kindWeeklyScoped, Percent: 3},
		},
	}
	live := &liveWindows{FiveHour: &UsageLimit{Utilization: 15}}

	merged := mergeStatusLine(polled, live)
	if got := []float64{merged.Limits[0].Percent, merged.Limits[1].Percent, merged.Limits[2].Percent}; got[0] != 15 || got[1] != 50 || got[2] != 3 {
		t.Errorf("rows = %v, want the session row at the live 15 and the others as polled", got)
	}
	if merged.ExtraUsage != polled.ExtraUsage {
		t.Error("poll-only fields were dropped")
	}
	if polled.Limits[0].Percent != 10 {
		t.Error("merge mutated the cached poll")
	}
}

// Which meters an account has is the server's to say. A verified Ultra
// account has no 5-hour limit, so a five_hour reading must not conjure a
// session row the poll did not list.
func TestMergeStatusLineAddsNoRows(t *testing.T) {
	polled := &UsageLimits{Limits: []LimitEntry{{Kind: kindWeeklyAll, Percent: 50}}}
	live := &liveWindows{FiveHour: &UsageLimit{Utilization: 15}, SevenDay: &UsageLimit{Utilization: 51}}

	merged := mergeStatusLine(polled, live)
	if len(merged.Limits) != 1 || merged.Limits[0].Kind != kindWeeklyAll || merged.Limits[0].Percent != 51 {
		t.Errorf("rows = %+v, want only the weekly row, at the live 51", merged.Limits)
	}
}

// statusLineRender is one session's status line input at the given readings.
func statusLineRender(session string, fiveHour, sevenDay int, reset time.Time) []byte {
	r := strconv.FormatInt(reset.Unix(), 10)
	return []byte(`{"session_id":"` + session + `","rate_limits":{` +
		`"five_hour":{"used_percentage":` + strconv.Itoa(fiveHour) + `,"resets_at":` + r + `},` +
		`"seven_day":{"used_percentage":` + strconv.Itoa(sevenDay) + `,"resets_at":` + r + `}}}`)
}

// The daemon notices status line news by the usage file's mtime, so a
// re-render with unchanged values must not touch the file — otherwise every
// 2s render would look like news.
func TestSaveStatusLineUsageSkipsUnchanged(t *testing.T) {
	withTempHome(t)
	reset := time.Now().Add(time.Hour)

	if err := saveStatusLineUsage(statusLineRender("a", 4, 20, reset)); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(statusLineUsageFile(), past, past); err != nil {
		t.Fatal(err)
	}

	if err := saveStatusLineUsage(statusLineRender("a", 4, 20, reset)); err != nil {
		t.Fatal(err)
	}
	if feed, _, _ := readStatusLineUpdate(past); feed != nil {
		t.Error("unchanged render rewrote the usage file")
	}

	if err := saveStatusLineUsage(statusLineRender("a", 5, 20, reset)); err != nil {
		t.Fatal(err)
	}
	feed, _, err := readStatusLineUpdate(past)
	if err != nil || feed == nil {
		t.Fatalf("changed render not picked up: %+v, %v", feed, err)
	}
	live := feed.liveSince(past, time.Now())
	if live == nil || live.FiveHour == nil || live.FiveHour.Utilization != 5 {
		t.Errorf("live = %+v, want five_hour at 5", live)
	}
	if live != nil && live.SevenDay != nil {
		t.Errorf("seven_day did not change, yet was reported live: %+v", live.SevenDay)
	}
}

// A session's first reading says nothing about its age: the session may have
// sat idle on that figure for hours. It must not be laid over a poll — this
// is also what keeps every idle session's old figure from looking new the
// moment a fresh usage file starts recording.
func TestStatusLineFirstSightIsNotLive(t *testing.T) {
	now := time.Now()
	feed := &statusLineFeed{Sessions: map[string]*sessionWindows{}}
	reading := &liveWindows{SevenDay: &UsageLimit{Utilization: 92, ResetsAt: now.Add(time.Hour)}}

	if !feed.record("idle", reading, now) {
		t.Fatal("a first reading must be recorded, or its next change cannot be recognised")
	}
	if live := feed.liveSince(now.Add(-24*time.Hour), now); live != nil {
		t.Errorf("first-sight reading reported live: %+v", live.SevenDay)
	}
}

// Anthropic sometimes resets a limit in the middle of its window. An idle
// session keeps re-rendering the pre-reset 92%; the session that is actually
// talking to the API reports the post-reset figure. The newest change must
// win even though it is lower — taking the highest reading would leave the
// menu bar on 92% until the window ends.
func TestStatusLineNewestChangeWinsAfterReset(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	reset := time.Now().Add(48 * time.Hour)
	at := func(util float64) *liveWindows {
		return &liveWindows{SevenDay: &UsageLimit{Utilization: util, ResetsAt: reset}}
	}
	feed := &statusLineFeed{Sessions: map[string]*sessionWindows{}}

	feed.record("idle", at(91), t0)
	feed.record("idle", at(92), t0.Add(time.Minute)) // its last response
	feed.record("active", at(92), t0.Add(2*time.Minute))
	feed.record("active", at(1), t0.Add(30*time.Minute)) // first response after the reset
	if feed.record("idle", at(92), t0.Add(31*time.Minute)) {
		t.Error("an idle session repeating its figure counted as a change")
	}

	now := t0.Add(32 * time.Minute)
	live := feed.liveSince(t0.Add(20*time.Minute), now)
	if live == nil || live.SevenDay == nil || live.SevenDay.Utilization != 1 {
		t.Fatalf("live = %+v, want the active session's post-reset 1%%", live)
	}
	// A poll sent after that change already includes it: nothing is newer.
	if live := feed.liveSince(t0.Add(31*time.Minute), now); live != nil {
		t.Errorf("reading older than the poll reported live: %+v", live.SevenDay)
	}
}

// A window whose reset has passed is over; Claude Code stops reporting it,
// and a leftover reading must not be shown as current usage. A session with
// no open window left has nothing more to say and is dropped from the file.
func TestStatusLineFeedDropsExpiredWindows(t *testing.T) {
	now := time.Now()
	expired := &liveWindows{FiveHour: &UsageLimit{Utilization: 80, ResetsAt: now.Add(-time.Minute)}}
	feed := &statusLineFeed{Sessions: map[string]*sessionWindows{}}
	feed.record("old", &liveWindows{FiveHour: &UsageLimit{Utilization: 79, ResetsAt: now.Add(-time.Minute)}}, now.Add(-2*time.Hour))
	feed.record("old", expired, now.Add(-time.Hour))

	if live := feed.liveSince(now.Add(-3*time.Hour), now); live != nil {
		t.Errorf("expired window reported live: %+v", live.FiveHour)
	}
	feed.prune(now)
	if len(feed.Sessions) != 0 {
		t.Errorf("session with no open window kept: %+v", feed.Sessions)
	}
}

// A crash can leave the usage file empty or half-written. The status line
// runs on every render, so it must replace such a file rather than fail on it
// forever.
func TestSaveStatusLineUsageReplacesUnreadableFile(t *testing.T) {
	for _, content := range []string{"", "{not json"} {
		withTempHome(t)
		if err := os.WriteFile(statusLineUsageFile(), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := saveStatusLineUsage(statusLineRender("a", 2, 3, time.Now().Add(time.Hour))); err != nil {
			t.Fatal(err)
		}
		feed, err := readStatusLineFeed()
		if err != nil || len(feed.Sessions) != 1 || feed.Sessions["a"] == nil {
			t.Errorf("after %q: feed = %+v, %v; want session a recorded", content, feed.Sessions, err)
		}
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

// newerWindow decides between a poll and a status line reading that changed
// after it. A later reset is a new window that wins outright; within one
// window the higher figure wins, so header lag never ticks the menu bar back.
func TestNewerWindow(t *testing.T) {
	reset := time.Date(2026, 10, 22, 22, 50, 0, 0, time.UTC)
	at := func(util float64, r time.Time) *UsageLimit { return &UsageLimit{Utilization: util, ResetsAt: r} }

	cases := []struct {
		name string
		a, b *UsageLimit
		want float64
	}{
		{"a reading lagging the poll loses", at(51, reset), at(50, reset), 51},
		{"a reading ahead of the poll wins", at(34, reset), at(51, reset), 51},
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

// A status line reading that lags the last poll must not roll the menu bar
// back: the poll saw 51%, the response headers still say 47%.
func TestMergeStatusLineKeepsFresherPoll(t *testing.T) {
	reset := time.Date(2026, 10, 22, 22, 50, 0, 0, time.UTC)
	polled := &UsageLimits{Limits: []LimitEntry{{Kind: kindSession, Percent: 51, ResetsAt: reset.Add(393 * time.Millisecond)}}}
	live := &liveWindows{FiveHour: &UsageLimit{Utilization: 47, ResetsAt: reset}}
	if got := mergeStatusLine(polled, live).Limits[0].Percent; got != 51 {
		t.Errorf("session row = %v, want the polled 51", got)
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
