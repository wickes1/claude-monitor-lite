// about.go - What the About window says.

package main

import (
	"fmt"
	"os"
	"strings"
)

const (
	aboutTitle   = "Claude Monitor Lite"
	aboutSummary = "Shows your Claude plan's usage limits in the menu bar."
	aboutLicense = "MIT License. © 2025 Wickes1"
)

// aboutRows are the facts the About window lists: what is running, whose
// usage it watches, how it gets it, and where its files are. labels names the
// watched accounts and live counts those whose Claude Code status line feeds
// the monitor.
func aboutRows(labels []string, live int) [][2]string {
	updates := fmt.Sprintf("Every %d minutes", int(refreshInterval.Minutes()))
	switch {
	case live == 0:
		updates += ".\nLive updates are off; turn them on with\nclaude-monitor-lite statusline install"
	case live == len(labels):
		updates += ", and within seconds\nwhile Claude Code is in use."
	default:
		updates += fmt.Sprintf(", and within seconds\nwhile Claude Code is in use, for %d of %d accounts.", live, len(labels))
	}
	accounts := "Accounts"
	if len(labels) == 1 {
		accounts = "Account"
	}
	return [][2]string{
		{"Version", version},
		{accounts, strings.Join(labels, "\n")},
		{"Updates", updates},
		{"Login", "Claude Code's own, read only.\nNo token is stored or refreshed."},
		{"Settings", tildePath(GetConfigPath())},
		{"Log", tildePath(monitorFile(".log"))},
		{"License", aboutLicense},
	}
}

// tildePath shortens a path under the home directory to ~/…, which reads
// better and keeps the user name out of a screenshot.
func tildePath(path string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rest, ok := strings.CutPrefix(path, home+string(os.PathSeparator)); ok {
			return "~/" + rest
		}
	}
	return path
}

// showAbout opens the About window for the monitor as it is now running.
func showAbout() {
	displayMu.Lock()
	labels := monitorLabels()
	displayMu.Unlock()

	live := 0
	for _, m := range monitors {
		if _, err := os.Stat(statusLineWrapFile(m.profile)); err == nil {
			live++
		}
	}
	showAboutPanel(aboutTitle, aboutSummary, aboutRows(labels, live), githubURL, githubURL+"/releases")
}
