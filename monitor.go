// monitor.go - One watched profile: its polling, its status line feed and its
// group of menu items — and the drawing of the menu the groups share.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/getlantern/systray"
)

// failure is why a profile's numbers are missing or no longer current.
type failure int

const (
	failNone    failure = iota
	failAuth            // no usable Claude Code login
	failWaiting         // Claude Code's token ran out; it renews when Claude Code next runs there
	failError           // the fetch keeps failing
)

// monitor watches one profile.
type monitor struct {
	profile profile
	creds   *credentialSource
	client  *ClaudeUsageClient
	// refresh asks the update worker to fetch now. Sends coalesce, so rapid
	// refresh requests do not pile up.
	refresh chan struct{}

	// headItem heads the profile's group in the menu: its name when several
	// profiles are watched, and why there are no current numbers — loading,
	// not logged in, a failing fetch, a plan without limits.
	headItem *systray.MenuItem
	// rowItems is a fixed pool of menu items for the usage rows; item i shows
	// row i and the rest stay hidden. A pool because the rows are only known
	// once usage arrives, while systray can only append items — a row added
	// later would land below Quit.
	rowItems []*systray.MenuItem

	// The update worker's view of the two usage sources. Touched only by the
	// update worker.
	//
	// polledLimits is what the last successful poll returned: the base that
	// status line readings are laid over.
	polledLimits *UsageLimits
	// lastPollAt is when that poll was sent. Only a status line reading that
	// changed after it can add to what the poll said. Starting at process
	// start keeps a reading left over from before the daemon ran from being
	// shown as current.
	lastPollAt time.Time
	// statusLineSeenAt is the mtime of the status line feed last looked at.
	statusLineSeenAt time.Time
	// Consecutive failed polls, for transient error resilience and backoff.
	consecutiveErrors     int
	consecutiveRateLimits int

	// What the menu shows for this profile. Guarded by displayMu.
	account account      // who is logged in, for the group's name
	limits  *UsageLimits // nil before the first successful fetch
	goodAt  time.Time    // when a poll last succeeded
	failure failure
}

var (
	// monitors holds one monitor per watched profile, in menu order. Set once
	// in onReady: systray can only append menu items, so the profiles are
	// fixed for the life of the process, and add/remove restart it.
	monitors []*monitor

	// displayMu guards appConfig and every monitor's display state, and
	// serializes redrawing: update workers and menu clicks both redraw.
	displayMu sync.Mutex
)

func newMonitor(p profile) *monitor {
	creds := &credentialSource{read: p.readCredential}
	return &monitor{
		profile:    p,
		creds:      creds,
		client:     NewUsageClient(creds.token),
		refresh:    make(chan struct{}, 1),
		lastPollAt: time.Now(),
		account:    p.readAccount(),
	}
}

// logf logs for this profile, naming it when the log is shared with others.
func (m *monitor) logf(format string, args ...any) {
	if len(monitors) > 1 {
		format = "[" + m.profile.folder() + "] " + format
	}
	log.Printf(format, args...)
}

func (m *monitor) requestRefresh() {
	select {
	case m.refresh <- struct{}{}:
	default:
	}
}

// run is the single goroutine that fetches this profile's usage — on a timer
// and on demand. Confining fetches to one goroutine means updateStats never
// runs concurrently with itself. The timer is rescheduled after every fetch
// using the delay updateStats returns — the base interval normally, backed
// off further while the endpoint is rate-limiting us — with jitter applied so
// this monitor does not beat in lockstep with others polling the same
// endpoint. Between polls it also applies status line updates, which cost no
// request (statusline.go).
func (m *monitor) run(ctx context.Context) {
	delay := m.updateStats() // initial fetch
	timer := time.NewTimer(withJitter(delay))
	defer timer.Stop()
	statusLineTicker := time.NewTicker(statusLineCheckInterval)
	defer statusLineTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-statusLineTicker.C:
			m.applyStatusLineUpdate()
		case <-timer.C:
			delay = m.updateStats()
			timer.Reset(withJitter(delay))
		case <-m.refresh:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			delay = m.updateStats()
			timer.Reset(withJitter(delay))
		}
	}
}

// updateStats fetches usage data once and updates the menu accordingly.
// It returns the next base poll delay (pre-jitter): refreshInterval on
// success or on a non-rate-limit error, or a value backed off toward
// maxBackoffInterval while the endpoint keeps returning 429.
func (m *monitor) updateStats() time.Duration {
	// Stamped before the request, so a status line update written while it is
	// in flight still counts as newer than this poll.
	polledAt := time.Now()
	limits, err := m.client.GetUsageLimits()
	if errors.Is(err, ErrAuthFailed) {
		// The cached token was rejected. Drop it and retry once so the token
		// source re-reads Claude Code's credential (which Claude Code may have
		// refreshed) before we tell a logged-in user to log in again.
		m.creds.invalidate()
		limits, err = m.client.GetUsageLimits()
	}
	if err != nil {
		var rl *RateLimitError
		delay := refreshInterval
		m.consecutiveErrors++
		if errors.As(err, &rl) {
			m.consecutiveRateLimits++
			delay = nextPollDelay(refreshInterval, maxBackoffInterval, m.consecutiveRateLimits, rl.RetryAfter)
		} else {
			m.consecutiveRateLimits = 0
		}

		switch classifyUpdate(err, m.consecutiveErrors, m.haveLimits()) {
		case actionShowAuth:
			m.fail(failAuth, err)
		case actionKeepStale:
			m.logf("Transient error (%d/%d): %v", m.consecutiveErrors, maxTransientErrors, err)
		case actionShowError:
			if errors.Is(err, ErrTokenExpired) {
				m.fail(failWaiting, err)
			} else {
				m.fail(failError, err)
			}
		}
		return delay
	}

	// Success — reset both error counters
	m.consecutiveErrors = 0
	m.consecutiveRateLimits = 0

	m.polledLimits, m.lastPollAt = limits, polledAt
	// A reading that changed while the request was in flight is newer than
	// this poll, and the tick that applied it will not fire again for it.
	feed, _ := readStatusLineFeed(m.profile)
	if live := feed.liveSince(polledAt, time.Now()); live != nil {
		limits = mergeStatusLine(limits, live)
	}
	// Read on every poll rather than once: logging in to another account
	// changes who the group should be named after.
	account := m.profile.readAccount()

	displayMu.Lock()
	recovered := m.failure != failNone
	m.account, m.goodAt, m.failure = account, polledAt, failNone
	displayMu.Unlock()
	if recovered {
		m.logf("Usage fetch recovered")
	}
	m.showLimits(limits)
	return refreshInterval
}

// fail records why this profile has no current numbers. A failure that
// persists is logged once, when it starts: an environment Claude Code is not
// running in stays failWaiting for hours, and a line per poll would bury
// everything else in the log.
func (m *monitor) fail(f failure, err error) {
	displayMu.Lock()
	started := m.failure != f
	m.failure = f
	render()
	displayMu.Unlock()
	if started {
		m.logf("Error fetching usage: %v", err)
	}
}

// haveLimits reports whether a previous successful fetch is on display.
func (m *monitor) haveLimits() bool {
	displayMu.Lock()
	defer displayMu.Unlock()
	return m.limits != nil
}

// applyStatusLineUpdate lays the status line readings that changed after the
// last poll over it. It runs when the feed file changes; a change that brings
// nothing newer than the poll leaves the display alone.
func (m *monitor) applyStatusLineUpdate() {
	feed, seenAt, err := readStatusLineUpdate(m.profile, m.statusLineSeenAt)
	m.statusLineSeenAt = seenAt
	if err != nil {
		m.logf("Failed to read status line usage: %v", err)
		return
	}
	// Before the first poll there are no rows for a reading to update.
	if feed == nil || m.polledLimits == nil {
		return
	}
	if live := feed.liveSince(m.lastPollAt, time.Now()); live != nil {
		m.showLimits(mergeStatusLine(m.polledLimits, live))
	}
}

// showLimits makes limits this profile's displayed usage: its menu rows, the
// menu bar if it shows one of them, and the cache the status command reads.
func (m *monitor) showLimits(limits *UsageLimits) {
	displayMu.Lock()
	m.limits = limits
	render()
	displayMu.Unlock()

	// The status command reads this instead of calling the API itself, which
	// would spend the same per-account rate-limit budget as this poll.
	if data, err := json.Marshal(limits); err == nil {
		if err := writeFileAtomic(m.profile.file(".usage.json"), data, configFilePermissions); err != nil {
			m.logf("Failed to write usage cache: %v", err)
		}
	}
}

// rows lists the profile's usage rows; none before the first fetch. Callers
// hold displayMu.
func (m *monitor) rows() []usageRow {
	if m.limits == nil {
		return nil
	}
	return usageRows(m.limits)
}

// rowKeyAt returns the key of the row menu item idx currently shows.
func (m *monitor) rowKeyAt(idx int) (string, bool) {
	displayMu.Lock()
	defer displayMu.Unlock()
	rows := m.rows()
	if idx >= len(rows) {
		return "", false
	}
	return rows[idx].key, true
}

// noLimitsMessage is shown when a usage response lists no meter at all — a
// seat billed by usage, with no plan limits to count against.
const noLimitsMessage = "No usage limits reported for this plan"

// statusText says why the profile has no current numbers, or is empty when
// it has them. Rows left on display by a fetch that now fails are dated, so
// a frozen number is never read as a current one. Callers hold displayMu.
func (m *monitor) statusText(now time.Time) string {
	var text string
	switch m.failure {
	case failAuth:
		text = "No Claude subscription login in Claude Code"
	case failWaiting:
		text = "Waiting for Claude Code"
	case failError:
		text = "Error loading data"
	default:
		switch {
		case m.limits == nil:
			return "Loading…"
		case len(m.rows()) == 0:
			return noLimitsMessage
		}
		return ""
	}
	if m.limits != nil {
		text += " (as of " + formatAsOf(m.goodAt, now) + ")"
	}
	return text
}

// formatAsOf is the time of a reading: the clock time for one from today,
// with the date for an older one.
func formatAsOf(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

// headTitle is the title of the group's head item, and whether it shows at
// all: with one profile there is no name to give, so it shows only while
// there is something to explain.
func headTitle(label, status string, grouped bool) (title string, show bool) {
	switch {
	case !grouped:
		return status, status != ""
	case status == "":
		return label, true
	}
	return label + " — " + status, true
}

// barTitle is the menu bar text while the menu bar shows this profile.
// Callers hold displayMu.
func (m *monitor) barTitle(head usageRow, ok bool) string {
	switch {
	case m.failure == failAuth:
		return "⚪ Login"
	case m.failure != failNone:
		return "⚪ Error"
	case m.limits == nil:
		return "⚪ Loading..."
	}
	return menuBarTitle(head, ok)
}

// headlineMonitor is the profile the menu bar shows a row of: the selected
// one, or the first when the selection names a profile no longer watched.
// Callers hold displayMu.
func headlineMonitor() *monitor {
	for _, m := range monitors {
		if m.profile.dir == appConfig.MenuBarIndicator.Dir {
			return m
		}
	}
	return monitors[0]
}

// render redraws every group, the checkmark and the menu bar title. The
// checkmark sits on the row the menu bar shows, pinned or not: with several
// groups and no name in the menu bar, it is what says whose number that is.
// Callers hold displayMu.
func render() {
	now := time.Now()
	grouped := len(monitors) > 1
	profiles := make([]profile, len(monitors))
	accounts := make([]account, len(monitors))
	for i, m := range monitors {
		profiles[i], accounts[i] = m.profile, m.account
	}
	labels := profileLabels(profiles, accounts)

	target := headlineMonitor()
	var head usageRow
	var ok bool
	for i, m := range monitors {
		rows := m.rows()
		if m == target {
			head, _, ok = headlineRow(rows, appConfig.MenuBarIndicator.Key)
		}

		if title, show := headTitle(labels[i], m.statusText(now), grouped); show {
			m.headItem.SetTitle(title)
			m.headItem.Show()
		} else {
			m.headItem.Hide()
		}
		for j, item := range m.rowItems {
			if j >= len(rows) {
				item.Hide()
				continue
			}
			item.SetTitle(formatRow(rows[j], false))
			if m == target && ok && rows[j].key == head.key {
				item.Check()
			} else {
				item.Uncheck()
			}
			item.Show()
		}
	}

	systray.SetTitle(target.barTitle(head, ok))
}

// selectIndicator switches which usage row the menu bar shows, redrawing
// immediately from what is on display so the click feels instant.
func selectIndicator(ind Indicator) {
	// Saving under the same lock keeps rapid clicks from persisting out of order.
	displayMu.Lock()
	defer displayMu.Unlock()
	appConfig.MenuBarIndicator = ind
	if err := SaveConfig(appConfig); err != nil {
		log.Printf("Failed to save menu bar indicator: %v", err)
	}
	render()
}
