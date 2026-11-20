package main

import (
	"testing"
	"time"
)

var oneRow = &UsageLimits{Limits: []LimitEntry{{Kind: kindWeeklyAll, Percent: 12, IsActive: true}}}

// A profile whose fetch fails keeps its last rows on display, so the group
// must say they are old and since when: an environment Claude Code has not
// run in for hours is the normal case, and its frozen numbers must not read
// as current ones.
func TestStatusText(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 30, 0, 0, time.Local)
	today := time.Date(2026, 11, 20, 9, 3, 0, 0, time.Local)
	yesterday := time.Date(2026, 11, 19, 23, 8, 0, 0, time.Local)

	for _, tc := range []struct {
		name string
		m    monitor
		want string
	}{
		{"before the first fetch", monitor{}, "Loading…"},
		{"current numbers", monitor{limits: oneRow, goodAt: today}, ""},
		{"a plan without limits", monitor{limits: &UsageLimits{}, goodAt: today}, noLimitsMessage},
		{"waiting, never fetched", monitor{failure: failWaiting}, "Waiting for Claude Code"},
		{"waiting on today's reading", monitor{failure: failWaiting, limits: oneRow, goodAt: today}, "Waiting for Claude Code (as of 09:03)"},
		{"waiting on an older reading", monitor{failure: failWaiting, limits: oneRow, goodAt: yesterday}, "Waiting for Claude Code (as of Nov 19 23:08)"},
		{"failing fetch", monitor{failure: failError, limits: oneRow, goodAt: today}, "Error loading data (as of 09:03)"},
		{"not logged in", monitor{failure: failAuth}, "No Claude subscription login in Claude Code"},
	} {
		if got := tc.m.statusText(now); got != tc.want {
			t.Errorf("%s: statusText = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// With one profile the menu must look as it did before groups existed: no
// name, and a head line only while there is something to explain. With
// several, every group is named whether or not it has anything to explain.
func TestHeadTitle(t *testing.T) {
	for _, tc := range []struct {
		label, status string
		grouped       bool
		wantTitle     string
		wantShow      bool
	}{
		{"Ada (Max)", "", false, "", false},
		{"Ada (Max)", "Loading…", false, "Loading…", true},
		{"Ada (Max)", "", true, "Ada (Max)", true},
		{"Ada · Acme (Team)", "Waiting for Claude Code (as of 09:03)", true, "Ada · Acme (Team) — Waiting for Claude Code (as of 09:03)", true},
	} {
		title, show := headTitle(tc.label, tc.status, tc.grouped)
		if title != tc.wantTitle || show != tc.wantShow {
			t.Errorf("headTitle(%q, %q, %v) = %q, %v, want %q, %v",
				tc.label, tc.status, tc.grouped, title, show, tc.wantTitle, tc.wantShow)
		}
	}
}

// The menu bar belongs to the selected profile alone. Another profile going
// stale — the work seat nobody has used since yesterday — must not turn the
// menu bar to "Error" while the selected account is fine; that was the whole
// reason to stop running one monitor per profile.
func TestMenuBarFollowsSelectedProfileOnly(t *testing.T) {
	origMonitors, origConfig := monitors, appConfig
	t.Cleanup(func() { monitors, appConfig = origMonitors, origConfig })

	const work = "/Users/alice/.claude-work"
	personal := &monitor{profile: newProfile(""), limits: oneRow}
	idle := &monitor{profile: newProfile(work), limits: oneRow, failure: failWaiting}
	monitors = []*monitor{personal, idle}

	bar := func() string {
		target := headlineMonitor()
		head, _, ok := headlineRow(target.rows(), appConfig.MenuBarIndicator.Key)
		return target.barTitle(head, ok)
	}

	appConfig.MenuBarIndicator = Indicator{Dir: "", Key: autoKey}
	if got := bar(); got != "🟢 12%" {
		t.Errorf("personal selected, work idle: menu bar = %q, want the personal reading", got)
	}

	// Pinned to the idle profile, the menu bar says so rather than showing its
	// frozen number as current.
	appConfig.MenuBarIndicator = Indicator{Dir: work, Key: kindWeeklyAll}
	if got := bar(); got != "⚪ Error" {
		t.Errorf("idle profile selected: menu bar = %q, want %q", got, "⚪ Error")
	}

	// A selection left pointing at a profile that is gone falls back to the first.
	appConfig.MenuBarIndicator = Indicator{Dir: "/Users/alice/.claude-gone", Key: autoKey}
	if got := headlineMonitor(); got != personal {
		t.Errorf("unknown profile selected: headline is %+v, want the first profile", got.profile)
	}
}
