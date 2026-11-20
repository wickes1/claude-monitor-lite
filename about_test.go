package main

import (
	"strings"
	"testing"
)

func aboutRow(rows [][2]string, label string) string {
	for _, row := range rows {
		if row[0] == label {
			return row[1]
		}
	}
	return ""
}

// The About window is where a user checks how fresh the numbers can be. It
// must say what is actually set up: promising live updates that are not
// installed leaves them waiting on a menu bar that moves every 3 minutes, and
// the way to turn them on is only useful while they are off.
func TestAboutRowsReportLiveUpdatesAsInstalled(t *testing.T) {
	two := []string{"Ada · Personal (Max)", "Ada · Acme (Team)"}
	for _, tc := range []struct {
		name          string
		live          int
		want, wantNot string
	}{
		{"off", 0, "statusline install", "within seconds"},
		{"on for every account", 2, "within seconds", "statusline install"},
		{"on for some", 1, "for 1 of 2 accounts", "statusline install"},
	} {
		got := aboutRow(aboutRows(two, tc.live), "Updates")
		if !strings.Contains(got, tc.want) || strings.Contains(got, tc.wantNot) {
			t.Errorf("%s: Updates = %q, want it to mention %q and not %q", tc.name, got, tc.want, tc.wantNot)
		}
	}
}

// Every watched account is listed by the name its menu group carries, so the
// window answers "whose usage is this" for all of them.
func TestAboutRowsListAccounts(t *testing.T) {
	rows := aboutRows([]string{"Ada · Personal (Max)", "Ada · Acme (Team)"}, 0)
	if got, want := aboutRow(rows, "Accounts"), "Ada · Personal (Max)\nAda · Acme (Team)"; got != want {
		t.Errorf("Accounts = %q, want %q", got, want)
	}
	if got := aboutRow(aboutRows([]string{"Ada · Personal (Max)"}, 0), "Account"); got != "Ada · Personal (Max)" {
		t.Errorf("one account: Account = %q", got)
	}
}

// Paths in the window are shown from ~ so a screenshot does not carry the
// user name; a path outside the home directory is left as it is.
func TestTildePath(t *testing.T) {
	home := withTempHome(t)
	if got := tildePath(home + "/.claude-monitor-lite.log"); got != "~/.claude-monitor-lite.log" {
		t.Errorf("tildePath under home = %q", got)
	}
	if got := tildePath("/var/log/x.log"); got != "/var/log/x.log" {
		t.Errorf("tildePath outside home = %q", got)
	}
}
