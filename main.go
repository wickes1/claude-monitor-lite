package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/getlantern/systray"
)

const (
	refreshInterval    = 60 * time.Second  // base poll interval; 30s was too aggressive for oauth/usage (persistent 429)
	maxBackoffInterval = 300 * time.Second // cap when the endpoint rate-limits us (429)
	pidCheckTimeout    = 500 * time.Millisecond
	pidFilePermissions = 0644 // Owner read/write, others read
	logFilePermissions = 0600 // Owner read/write only (may contain error bodies)
)

// version is set via ldflags at build time (e.g., -X main.version=v1.0.0)
var version = "dev"

// githubURL is the project repository, shown in (and opened from) the About dialog.
const githubURL = "https://github.com/wickes1/claude-monitor-lite"

var (
	// indicatorMenuItems holds the systray menu items, parallel to the
	// indicators table. Populated once in onReady.
	indicatorMenuItems []*systray.MenuItem

	// Refresh button
	mRefresh *systray.MenuItem

	// App config
	appConfig    Config
	pidFile      string
	claudeClient *ClaudeUsageClient

	// Guards appConfig.MenuBarIndicator — written by menu clicks (event loop),
	// read by the update worker.
	indicatorMutex sync.RWMutex

	// Last fetched limits for instant display switching, and when they were
	// fetched — the timestamp feeds the multi-profile menu's "(as of ...)"
	// staleness marker when the active profile itself goes stale (ISC-89).
	// lastLimitsProfile records which profile these globals belong to
	// (captured in updateStats from the active profile at fetch time — see
	// the comment there). CurrentOAuthToken/activeProfileDir() already
	// re-resolve the active profile fresh on every call, so the active
	// poller itself always fetches the right profile's data (ISC-42/53/54)
	// — but a `profile switch` made via the CLI is a separate process, so it
	// never fires requestRefresh, and the daemon's active poller only
	// notices on its own next tick (up to refreshInterval later). Until
	// then these globals still hold the PREVIOUS active profile's numbers.
	// profileSnapshotFunc compares lastLimitsProfile against the current
	// active name so the multi-profile menu never attributes a stale,
	// wrong-account fetch to the newly-active profile's row (ISC-89
	// regression, caught in adversarial review). All three fields are
	// protected by limitsMutex.
	lastLimits        *UsageLimits
	lastLimitsAt      time.Time
	lastLimitsProfile string
	limitsMutex       sync.RWMutex

	// Consecutive error tracking for transient error resilience. Both counters
	// are guarded by consecutiveMutex.
	consecutiveErrors     int
	consecutiveRateLimits int
	consecutiveMutex      sync.Mutex

	// Context for graceful shutdown
	appCtx    context.Context
	appCancel context.CancelFunc
)

// indicator is one selectable usage metric: a stable config key, a menu label,
// and how to pull its window out of a usage response. This table is the single
// source of truth — menu items, config validation, display and selection all
// derive from it, so a new API field means adding exactly one row here.
type indicator struct {
	key   string
	label string
	get   func(*UsageLimits) *UsageLimit
}

// indicators lists every metric the usage API returns, in API response order.
var indicators = []indicator{
	{"currentSession", "5-Hour Session", func(u *UsageLimits) *UsageLimit { return u.FiveHour }},
	{"weeklyAll", "Weekly (All)", func(u *UsageLimits) *UsageLimit { return u.SevenDay }},
	{"weeklyOAuthApps", "Weekly (OAuth Apps)", func(u *UsageLimits) *UsageLimit { return u.SevenDayOAuthApps }},
	{"weeklyOpus", "Weekly (Opus)", func(u *UsageLimits) *UsageLimit { return u.SevenDayOpus }},
	{"weeklySonnet", "Weekly (Sonnet)", func(u *UsageLimits) *UsageLimit { return u.SevenDaySonnet }},
	{"weeklyCowork", "Weekly (Cowork)", func(u *UsageLimits) *UsageLimit { return u.SevenDayCowork }},
	{"weeklyDesign", "Weekly (Design)", func(u *UsageLimits) *UsageLimit { return u.SevenDayOmelette }},
	{"tangelo", "Tangelo", func(u *UsageLimits) *UsageLimit { return u.Tangelo }},
	{"iguanaNecktie", "Iguana Necktie", func(u *UsageLimits) *UsageLimit { return u.IguanaNecktie }},
	{"designPromotional", "Design (Promotional)", func(u *UsageLimits) *UsageLimit { return u.OmelettePromotional }},
	{"nimbusQuill", "Nimbus Quill", func(u *UsageLimits) *UsageLimit { return u.NimbusQuill }},
	{"cinderCove", "Cinder Cove", func(u *UsageLimits) *UsageLimit { return u.CinderCove }},
	{"amberLadder", "Amber Ladder", func(u *UsageLimits) *UsageLimit { return u.AmberLadder }},
	{"extraUsage", "Extra Usage", extraUsageWindow},
}

// extraUsageWindow adapts the pay-as-you-go extra_usage object to the window
// shape so it can be displayed like any other metric. It has no reset time and
// reports nothing until pay-as-you-go is enabled and reporting a utilization.
func extraUsageWindow(u *UsageLimits) *UsageLimit {
	if u.ExtraUsage == nil || u.ExtraUsage.Utilization == nil {
		return nil
	}
	return &UsageLimit{Utilization: *u.ExtraUsage.Utilization}
}

// findIndicator returns the indicator for key, falling back to the first
// (default) entry when key is unknown.
func findIndicator(key string) indicator {
	for i := range indicators {
		if indicators[i].key == key {
			return indicators[i]
		}
	}
	return indicators[0]
}

// isValidIndicator reports whether key names a known indicator.
func isValidIndicator(key string) bool {
	for i := range indicators {
		if indicators[i].key == key {
			return true
		}
	}
	return false
}

// Helper function to create Claude client from session
func createClientFromSession(session *AuthSession) *ClaudeUsageClient {
	if session.AuthMode == AuthModeOAuth {
		return NewOAuthUsageClient(CurrentOAuthToken)
	}
	if session.OrganizationID != "" {
		return NewClaudeUsageClientWithOrg(session.SessionKey, session.OrganizationID)
	}
	return NewClaudeUsageClient(session.SessionKey)
}

// getMenuBarIndicator and setMenuBarIndicator guard appConfig.MenuBarIndicator,
// which is written by menu clicks (event loop) and read by the update worker.
func getMenuBarIndicator() string {
	indicatorMutex.RLock()
	defer indicatorMutex.RUnlock()
	return appConfig.MenuBarIndicator
}

func setMenuBarIndicator(key string) {
	indicatorMutex.Lock()
	appConfig.MenuBarIndicator = key
	indicatorMutex.Unlock()
}

// Helper function to round utilization to nearest integer
func roundUtilization(utilization float64) int {
	return int(utilization + 0.5)
}

// Helper function to get color indicator based on utilization
func getColorIndicator(utilization float64) string {
	if utilization < 50.0 {
		return "🟢"
	}
	if utilization < 80.0 {
		return "🟡"
	}
	return "🔴"
}

// Helper function to round minutes to nearest 10
func roundToTenMinutes(minutes int) int {
	return ((minutes + 5) / 10) * 10
}

// Helper function to calculate time until reset
func calculateTimeUntilReset(resetTime time.Time) (hours, minutes int, valid bool) {
	if resetTime.IsZero() {
		return 0, 0, false
	}

	// Truncate current time to the minute (ignore seconds)
	now := time.Now()
	nowTruncated := time.Date(now.Year(), now.Month(), now.Day(),
		now.Hour(), now.Minute(), 0, 0, now.Location())

	duration := resetTime.Sub(nowTruncated)
	if duration < 0 {
		return 0, 0, false
	}

	// Whole minutes remaining, truncated to the minute
	totalMinutes := int(duration.Minutes())

	return totalMinutes / 60, totalMinutes % 60, true
}

// Helper function to format reset time for display
func formatResetTime(resetTime time.Time) string {
	local := resetTime.Local()

	// Round minutes to nearest 10
	roundedMinutes := roundToTenMinutes(local.Minute())

	// Adjust hour if minutes rolled over
	hour := local.Hour()
	if roundedMinutes >= 60 {
		hour = (hour + 1) % 24
		roundedMinutes = 0
	}

	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d",
		local.Year(), local.Month(), local.Day(), hour, roundedMinutes)
}

// Helper function to format a single usage limit with reset time
func formatUsageWithReset(limit *UsageLimit, label string) string {
	if limit == nil {
		return fmt.Sprintf("%s --", label)
	}

	utilization := roundUtilization(limit.Utilization)
	hours, minutes, hasTime := calculateTimeUntilReset(limit.ResetsAtTime)

	// Special case: no active session (0% with no reset time)
	if !hasTime && utilization == 0 {
		return fmt.Sprintf("%s %d%% (no active session)", label, utilization)
	}

	if hasTime {
		return fmt.Sprintf("%s %d%% (resets %s, in %dh %dm)",
			label, utilization, formatResetTime(limit.ResetsAtTime), hours, minutes)
	}

	return fmt.Sprintf("%s %d%%", label, utilization)
}

// Helper function to format usage limit for console display
func formatConsoleUsage(limit *UsageLimit, label string, noSessionMsg string) string {
	if limit == nil {
		return fmt.Sprintf("%s  --\n", label)
	}

	utilization := roundUtilization(limit.Utilization)
	hours, minutes, hasTime := calculateTimeUntilReset(limit.ResetsAtTime)

	if hasTime {
		return fmt.Sprintf("%s  %3d%%  (resets %s, in %dh %dm)\n",
			label, utilization, formatResetTime(limit.ResetsAtTime), hours, minutes)
	}

	if noSessionMsg != "" {
		return fmt.Sprintf("%s  %3d%%  (%s)\n", label, utilization, noSessionMsg)
	}
	return fmt.Sprintf("%s  %3d%%\n", label, utilization)
}

// Helper function to get the selected limit based on indicator setting
func getSelectedLimit(limits *UsageLimits, indicatorKey string) *UsageLimit {
	return findIndicator(indicatorKey).get(limits)
}

// scopedLimitLabel builds the console label for a model/surface-scoped
// limits[] entry, e.g. "Weekly (Fable)". It prefers the model display name,
// falls back to the model ID, and finally to a generic "Scoped" label when
// neither is present.
func scopedLimitLabel(entry LimitEntry) string {
	if entry.Scope != nil && entry.Scope.Model != nil {
		if entry.Scope.Model.DisplayName != nil {
			return fmt.Sprintf("Weekly (%s)", *entry.Scope.Model.DisplayName)
		}
		if entry.Scope.Model.ID != nil {
			return fmt.Sprintf("Weekly (%s)", *entry.Scope.Model.ID)
		}
	}
	return "Weekly (Scoped)"
}

// scopedLimitToUsageLimit adapts a limits[] entry to the UsageLimit shape so
// it can be rendered with the existing formatConsoleUsage helper.
func scopedLimitToUsageLimit(entry LimitEntry) *UsageLimit {
	return &UsageLimit{
		Utilization:  entry.Percent,
		ResetsAt:     entry.ResetsAt,
		ResetsAtTime: entry.ResetsAtTime,
	}
}

// formatScopedLimitLine renders one model/surface-scoped limits[] entry as a
// console line, in the same format as formatConsoleUsage.
func formatScopedLimitLine(entry LimitEntry) string {
	return formatConsoleUsage(scopedLimitToUsageLimit(entry), scopedLimitLabel(entry)+":", "")
}

// Helper function to display usage stats. Windows with no data are skipped to
// match the menu, except the 5-hour session, which is always shown as the
// headline metric.
func displayUsageStats(limits *UsageLimits) {
	fmt.Println("=== Current Usage ===")
	for i := range indicators {
		limit := indicators[i].get(limits)
		isCurrentSession := indicators[i].key == "currentSession"
		if limit == nil && !isCurrentSession {
			continue
		}
		noSessionMsg := ""
		if isCurrentSession {
			noSessionMsg = "no active session"
		}
		fmt.Print(formatConsoleUsage(limit, indicators[i].label+":", noSessionMsg))
	}
	// Scoped limits (e.g. a weekly window narrowed to one model) are display
	// only — they are not selectable indicators and never touch config
	// validation or persistence. Unscoped entries (session, weekly_all) merely
	// duplicate five_hour/seven_day already shown above, so only scoped
	// entries are printed here.
	if limits != nil {
		for _, entry := range limits.Limits {
			if entry.Scope != nil {
				fmt.Print(formatScopedLimitLine(entry))
			}
		}
	}
	fmt.Println()
}

// Helper function to update menu bar display
func updateMenuBarDisplay(limits *UsageLimits) {
	limit := getSelectedLimit(limits, getMenuBarIndicator())

	if limit == nil {
		systray.SetTitle("⚪ --")
		return
	}

	utilization := roundUtilization(limit.Utilization)
	hours, minutes, hasTime := calculateTimeUntilReset(limit.ResetsAtTime)
	color := getColorIndicator(limit.Utilization)

	if hasTime {
		systray.SetTitle(fmt.Sprintf("%s %d%% (%dh%dm)", color, utilization, hours, minutes))
	} else {
		systray.SetTitle(fmt.Sprintf("%s %d%%", color, utilization))
	}
}

// migrateStripLegacySecrets rewrites the config file once if it still carries
// a plaintext OAuth token from a version predating this security fix — the
// access/refresh token used to be copied from Claude Code's credential store
// into our own config; it now lives only in memory (see CurrentOAuthToken in
// credentials.go). Config no longer declares those fields, so re-marshaling
// the already-parsed struct is enough to drop them. A no-op if the config
// file is absent or already clean, so this costs one read on every normal
// startup.
func migrateStripLegacySecrets() {
	data, err := os.ReadFile(GetConfigPath())
	if err != nil {
		return // no config file yet - nothing to migrate
	}

	raw := string(data)
	hasLegacySecret := strings.Contains(raw, "accessToken") ||
		strings.Contains(raw, "refreshToken") ||
		strings.Contains(raw, "expiresAt")
	if !hasLegacySecret {
		return
	}

	if err := modifyAndSaveConfig(func(c *Config) {}); err != nil {
		log.Printf("Failed to migrate legacy plaintext token out of config: %v", err)
	}
}

// ============================================================================
// Multi-profile support (F5-F8): per-profile polling, per-profile menu
// sections, per-profile terminal status, and the CLI surface (`profile ...`).
//
// Design note: the ACTIVE profile keeps using the pre-existing single-profile
// machinery above (claudeClient, lastLimits, consecutiveErrors, updateStats,
// runUpdateWorker) completely unmodified in its polling logic — it already
// tracks the active profile automatically, because CurrentOAuthToken /
// CurrentOAuthTokenForDir(activeProfileDir()) re-resolve the active profile's
// config dir fresh from disk on every call (see credentials.go, profiles.go).
// That is what keeps ISC-42/53/54 true for free: with <=1 profile configured,
// or for whichever profile is active, nothing here changes existing behavior.
//
// Everything below is an ADDITIONAL layer that polls every OTHER (non-active)
// profile independently, and renders a per-profile menu/terminal section once
// more than one profile is registered.
// ============================================================================

// profileState is one non-active profile's independent poll state: its own
// OAuth client (token resolved per-dir, never the active profile's), its own
// cached usage data, and its own error/backoff counters — a failure here can
// never block or delay another profile's poll (ISC-36, 39, 40).
type profileState struct {
	meta   ProfileMeta
	client *ClaudeUsageClient

	mu                    sync.RWMutex
	lastLimits            *UsageLimits
	lastGoodAt            time.Time
	frozen                bool // true once a poll fails while lastLimits is still cached (ISC-85)
	consecutiveErrors     int
	consecutiveRateLimits int
}

var (
	profileStatesMu sync.RWMutex
	profileStates   = make(map[string]*profileState)
)

func setProfileState(name string, s *profileState) {
	profileStatesMu.Lock()
	profileStates[name] = s
	profileStatesMu.Unlock()
}

func removeProfileState(name string) {
	profileStatesMu.Lock()
	delete(profileStates, name)
	profileStatesMu.Unlock()
}

func getProfileState(name string) (*profileState, bool) {
	profileStatesMu.RLock()
	defer profileStatesMu.RUnlock()
	s, ok := profileStates[name]
	return s, ok
}

// profileStaggerStep is the delay between successive non-active profiles'
// first poll, so N profiles' pollers do not all fire in the same instant
// (ISC-37).
const profileStaggerStep = 7 * time.Second

// staggerOffsetFor returns the initial delay before the profile at index idx
// (0-based, in registry order among non-active profiles) starts polling.
func staggerOffsetFor(idx int) time.Duration {
	return time.Duration(idx) * profileStaggerStep
}

// otherProfilesInOrder returns every profile in cfg OTHER than the active
// one, preserving registry order — the set the multi-profile poller
// supervises. The active profile is excluded: it already has its own
// dedicated poll loop (runUpdateWorker) driving the menubar title directly
// (ISC-53), so including it here would poll it twice. With <=1 profile
// configured this returns nil, so the entire multi-profile layer becomes a
// no-op (ISC-42, 54).
func otherProfilesInOrder(cfg Config) []ProfileMeta {
	if len(cfg.Profiles) <= 1 {
		return nil
	}
	active := activeProfileName(cfg)
	others := make([]ProfileMeta, 0, len(cfg.Profiles)-1)
	for _, p := range cfg.Profiles {
		if p.Name == active {
			continue
		}
		others = append(others, p)
	}
	return others
}

// profileErrorBackoff is the retry interval for a profile whose credential is
// expired or unreadable (ErrTransient/ErrAuthFailed from
// CurrentOAuthTokenForDir): we already know Claude Code has not refreshed it
// yet, so polling every refreshInterval would be spam with no chance of a
// different answer; a slower retry still notices promptly once Claude Code
// refreshes in the background (ISC-85).
const profileErrorBackoff = 5 * time.Minute

// pollProfileOnce fetches usage for one non-active profile and updates its
// state, returning the next poll delay. On failure, lastLimits/lastGoodAt are
// left untouched (frozen last-good display, ISC-85) and reset countdowns for
// that cached data keep computing locally at display time (ISC-86) — nothing
// extra is needed for that, since calculateTimeUntilReset/formatUsageWithReset
// already work purely off ResetsAtTime on the cached UsageLimits.
func pollProfileOnce(state *profileState) time.Duration {
	limits, err := state.client.GetUsageLimits()
	if err != nil {
		var rl *RateLimitError
		isRateLimit := errors.As(err, &rl)

		state.mu.Lock()
		state.consecutiveErrors++
		if isRateLimit {
			state.consecutiveRateLimits++
		} else {
			state.consecutiveRateLimits = 0
		}
		hadData := state.lastLimits != nil
		state.frozen = hadData
		rateFails := state.consecutiveRateLimits
		state.mu.Unlock()

		label := "no cached data yet"
		if hadData {
			label = "frozen last-good data kept"
		}
		log.Printf("profile %q: usage fetch failed (%s): %s", state.meta.Name, label, redactedFetchErr(err))

		if isRateLimit {
			return nextPollDelay(refreshInterval, maxBackoffInterval, rateFails, rl.RetryAfter)
		}
		if errors.Is(err, ErrTransient) || errors.Is(err, ErrAuthFailed) {
			return profileErrorBackoff
		}
		return refreshInterval
	}

	state.mu.Lock()
	state.lastLimits = limits
	state.lastGoodAt = time.Now()
	state.frozen = false
	state.consecutiveErrors = 0
	state.consecutiveRateLimits = 0
	state.mu.Unlock()
	return refreshInterval
}

// runProfilePoller is one non-active profile's independent poll loop —
// entirely separate from the active profile's runUpdateWorker/updateStats.
func runProfilePoller(ctx context.Context, meta ProfileMeta, stagger time.Duration) {
	dir := meta.Dir
	client := NewOAuthUsageClient(func() (string, error) { return CurrentOAuthTokenForDir(dir) })
	state := &profileState{meta: meta, client: client}
	setProfileState(meta.Name, state)
	defer removeProfileState(meta.Name)

	select {
	case <-ctx.Done():
		return
	case <-time.After(stagger):
	}

	delay := pollProfileOnce(state)
	refreshProfileMenu()
	timer := time.NewTimer(withJitter(delay))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			delay = pollProfileOnce(state)
			refreshProfileMenu()
			timer.Reset(withJitter(delay))
		}
	}
}

// multiProfileSuperviseInterval is how often runMultiProfileSupervisor
// re-reads the profile registry to start pollers for newly added profiles and
// stop them for removed/now-active ones. It is independent of, and much
// shorter than, refreshInterval — the 60s-per-profile poll floor (ISC-38)
// lives in each profile's own poller, not here.
const multiProfileSuperviseInterval = 10 * time.Second

// runMultiProfileSupervisor watches the profile registry and starts/stops one
// polling goroutine per non-active profile, so profile adds/removes and
// active-profile switches (made via the CLI while this daemon is running,
// ISC-49) are picked up within one supervise cycle (ISC-28, 56) without
// restarting the daemon.
func runMultiProfileSupervisor(ctx context.Context) {
	cancels := make(map[string]context.CancelFunc)

	reconcile := func() {
		cfg := LoadConfig()
		others := otherProfilesInOrder(cfg)

		wanted := make(map[string]ProfileMeta, len(others))
		for _, p := range others {
			wanted[p.Name] = p
		}
		for name, cancel := range cancels {
			if _, ok := wanted[name]; !ok {
				cancel()
				delete(cancels, name)
			}
		}
		for i, p := range others {
			if _, ok := cancels[p.Name]; ok {
				continue
			}
			pctx, cancel := context.WithCancel(ctx)
			cancels[p.Name] = cancel
			go runProfilePoller(pctx, p, staggerOffsetFor(i))
		}
		refreshProfileMenu()
	}

	reconcile()

	ticker := time.NewTicker(multiProfileSuperviseInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			for _, cancel := range cancels {
				cancel()
			}
			return
		case <-ticker.C:
			reconcile()
		}
	}
}

// --- Menubar: per-profile section (F7, ISC-50..56, 89) ---------------------

// maxMenuProfiles bounds the pre-created menu-item pool. getlantern/systray
// cannot remove items once added (same constraint as applyIndicatorMenu
// above), so every possible profile slot is pre-created once, hidden, and
// Show/Hide'd from then on.
const maxMenuProfiles = 8

// profileMenuSlot is one pre-created, initially-hidden row in the
// multi-profile menu section.
type profileMenuSlot struct {
	header     *systray.MenuItem
	session    *systray.MenuItem
	weekly     *systray.MenuItem
	switchItem *systray.MenuItem

	targetMu sync.RWMutex
	target   string // profile name this slot's switchItem currently targets, "" if none
}

var profileMenuItems [maxMenuProfiles]*profileMenuSlot

// profileMenuMu serializes all writes to the profile menu-item pool.
// refreshProfileMenu is called from several independent goroutines (the
// active poller, each non-active profile's poller, the supervisor, and the
// menu switch-click handler) — without this, concurrent SetTitle/Show/Hide
// calls on the same *systray.MenuItem from different goroutines would race.
var profileMenuMu sync.Mutex

// createProfileMenuPool pre-creates every slot in the bounded pool, hidden.
// Must be called once from onReady, after the existing menu items and before
// the Quit item (order doesn't matter visually since every slot starts
// hidden).
func createProfileMenuPool() {
	for i := range profileMenuItems {
		slot := &profileMenuSlot{
			header:     systray.AddMenuItem("", "Profile"),
			session:    systray.AddMenuItem("", "Profile 5-hour session usage"),
			weekly:     systray.AddMenuItem("", "Profile weekly usage"),
			switchItem: systray.AddMenuItem("", "Switch Claude Code to this profile"),
		}
		slot.header.Disable()
		slot.session.Disable()
		slot.weekly.Disable()
		slot.header.Hide()
		slot.session.Hide()
		slot.weekly.Hide()
		slot.switchItem.Hide()
		profileMenuItems[i] = slot
	}
}

// wireProfileMenuClicks starts one goroutine per pool slot forwarding
// switchItem clicks to SwitchProfile — the indicator-click pattern above does
// the same thing for a static list; here the *target* profile name for each
// slot changes over time as refreshProfileMenu re-renders, so each click
// reads the slot's current target under its own lock instead of a name
// captured at goroutine-start time.
func wireProfileMenuClicks(ctx context.Context) {
	for i := range profileMenuItems {
		go func(idx int) {
			slot := profileMenuItems[idx]
			for {
				select {
				case <-ctx.Done():
					return
				case <-slot.switchItem.ClickedCh:
					slot.targetMu.RLock()
					name := slot.target
					slot.targetMu.RUnlock()
					if name == "" {
						continue
					}
					result, err := SwitchProfile(name)
					if err != nil {
						log.Printf("menu switch to %q failed: %v", name, err)
						continue
					}
					if !result.AlreadyActive {
						log.Printf("menu switch: %s -> %s (credential: %s)", result.From, result.To, result.Health)
					}
					refreshProfileMenu()
					requestRefresh()
				}
			}
		}(i)
	}
}

// profileMenuRow is the pure, testable render model for one profile's menu
// slot — computed by buildProfileMenuRows, applied to real systray widgets by
// applyProfileMenu.
type profileMenuRow struct {
	profileName   string
	header        string
	sessionLine   string
	weeklyLine    string
	switchVisible bool
}

// staleMenuTimeFormat matches formatResetTime's clock style (no seconds).
const staleMenuTimeFormat = "3:04 PM"

// buildProfileMenuRows computes the per-profile menu rows for the
// multi-profile section (ISC-50, 51, 89). profiles is the full registry in
// registration order; activeName is the currently active profile; snapshot
// looks up cached data for a profile by name, returning
// (limits, lastGoodAt, frozen, hasData). Callers gate on len(profiles) > 1
// themselves (ISC-54) — this function only decides what to render, not
// whether to.
func buildProfileMenuRows(
	profiles []ProfileMeta,
	activeName string,
	snapshot func(name string) (limits *UsageLimits, lastGoodAt time.Time, frozen bool, hasData bool),
) []profileMenuRow {
	rows := make([]profileMenuRow, 0, len(profiles))
	for _, p := range profiles {
		row := profileMenuRow{profileName: p.Name}
		isActive := p.Name == activeName
		if isActive {
			row.header = fmt.Sprintf("%s — active", p.Name)
		} else {
			row.header = p.Name
			row.switchVisible = true
		}

		limits, lastGoodAt, frozen, hasData := snapshot(p.Name)
		var five, weekly *UsageLimit
		if hasData && limits != nil {
			five, weekly = limits.FiveHour, limits.SevenDay
		}
		sessionLine := formatUsageWithReset(five, "5-Hour Session:")
		weeklyLine := formatUsageWithReset(weekly, "Weekly (All):")
		if frozen && !lastGoodAt.IsZero() {
			asOf := fmt.Sprintf(" (as of %s)", lastGoodAt.Local().Format(staleMenuTimeFormat))
			sessionLine += asOf
		}
		row.sessionLine = sessionLine
		row.weeklyLine = weeklyLine
		rows = append(rows, row)
	}
	return rows
}

// profileSnapshotFunc returns the snapshot function buildProfileMenuRows
// needs: the active profile's data comes from the pre-existing single-profile
// globals (lastLimits/consecutiveErrors — untouched by this feature), every
// other profile's data comes from its own profileState.
func profileSnapshotFunc(activeName string) func(name string) (*UsageLimits, time.Time, bool, bool) {
	return func(name string) (*UsageLimits, time.Time, bool, bool) {
		if name == activeName {
			limitsMutex.RLock()
			l := lastLimits
			at := lastLimitsAt
			taggedFor := lastLimitsProfile
			limitsMutex.RUnlock()

			// The globals are shared with the pre-existing single-profile
			// machinery and only belong to THIS row when they were actually
			// fetched while this profile was active. A `profile switch` made
			// via the CLI (a separate process, so it never fires
			// requestRefresh) flips activeName immediately, but the globals
			// keep holding the PREVIOUS active profile's numbers until the
			// daemon's own active poller catches up on its next tick (up to
			// refreshInterval later, main.go updateStats). Rendering those
			// stale globals under the new profile's "— active" header would
			// silently show the wrong account (ISC-89 regression). A tag
			// mismatch — including daemon-start, where lastLimitsProfile is
			// still "" because no fetch has completed yet — falls through to
			// no-data instead, exactly like a non-active profile with no
			// cached poll yet: hasData=false, the row renders "awaiting
			// data" via formatUsageWithReset(nil, ...), never a crash.
			if l == nil || taggedFor != activeName {
				return nil, time.Time{}, false, false
			}

			consecutiveMutex.Lock()
			frozen := consecutiveErrors > 0
			consecutiveMutex.Unlock()
			// The active profile row shows the same "(as of ...)" staleness
			// marker as any other frozen profile (ISC-89); the menubar title
			// itself keeps the pre-existing keep-stale behavior (ISC-53).
			return l, at, frozen, true
		}
		s, ok := getProfileState(name)
		if !ok {
			return nil, time.Time{}, false, false
		}
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.lastLimits, s.lastGoodAt, s.frozen, s.lastLimits != nil
	}
}

// refreshProfileMenu re-reads the profile registry and re-renders the
// multi-profile menu section (or hides it entirely at <=1 profile, ISC-54)
// from whatever data is currently cached per profile. Called after every
// active-profile poll, every non-active-profile poll, every supervisor
// reconcile, and every menu-triggered switch, so the menu picks up CLI-made
// switches and registry changes within one poll cycle (ISC-56) without a
// dedicated timer of its own.
func refreshProfileMenu() {
	if profileMenuItems[0] == nil {
		return // onReady has not created the pool yet (not the systray daemon process)
	}

	cfg := LoadConfig()
	applyProfileMenu(cfg.Profiles, activeProfileName(cfg))
}

// applyProfileMenu renders profiles/activeName into the pre-created menu
// pool, hiding every slot when <=1 profile is configured (ISC-54) and any
// slot beyond len(profiles) (bounded pool, up to maxMenuProfiles — a profile
// count beyond the pool size degrades to showing only the first
// maxMenuProfiles, never a crash).
func applyProfileMenu(profiles []ProfileMeta, activeName string) {
	profileMenuMu.Lock()
	defer profileMenuMu.Unlock()

	show := len(profiles) > 1
	var rows []profileMenuRow
	if show {
		rows = buildProfileMenuRows(profiles, activeName, profileSnapshotFunc(activeName))
	}

	for i, slot := range profileMenuItems {
		if !show || i >= len(rows) {
			slot.header.Hide()
			slot.session.Hide()
			slot.weekly.Hide()
			slot.switchItem.Hide()
			slot.targetMu.Lock()
			slot.target = ""
			slot.targetMu.Unlock()
			continue
		}

		row := rows[i]
		slot.header.SetTitle(row.header)
		slot.header.Show()
		slot.session.SetTitle(row.sessionLine)
		slot.session.Show()
		slot.weekly.SetTitle(row.weeklyLine)
		slot.weekly.Show()

		if row.switchVisible {
			slot.switchItem.SetTitle("Switch Claude Code here: " + row.profileName)
			slot.switchItem.Show()
			slot.targetMu.Lock()
			slot.target = row.profileName
			slot.targetMu.Unlock()
		} else {
			slot.switchItem.Hide()
			slot.targetMu.Lock()
			slot.target = ""
			slot.targetMu.Unlock()
		}
	}
}

// --- Terminal status: per-profile sections (F6, ISC-48) ---------------------

// credentialStatusLabel renders a CredentialStatus for one-line CLI display.
func credentialStatusLabel(s CredentialStatus) string {
	switch s {
	case CredentialLoggedIn:
		return "logged in"
	case CredentialExpired:
		return "expired (waiting for Claude Code to refresh)"
	default:
		return "needs login"
	}
}

// displayMultiProfileStatus prints a status section per registered profile
// (ISC-48), each independently fetched via that profile's own per-dir token
// so one profile's expired/missing credential never blocks another's section
// from printing (mirrors the poller's independence, ISC-40).
func displayMultiProfileStatus(profiles []ProfileMeta, activeName string) {
	for _, p := range profiles {
		marker := ""
		if p.Name == activeName {
			marker = " (active)"
		}
		fmt.Printf("=== Profile: %s%s ===\n", p.Name, marker)

		dir := p.Dir
		client := NewOAuthUsageClient(func() (string, error) { return CurrentOAuthTokenForDir(dir) })
		limits, err := client.GetUsageLimits()
		if err != nil {
			fmt.Printf("  Usage unavailable (%s): %s\n", credentialStatusLabel(CredentialHealth(dir)), briefStatusErr(err))
			fmt.Println()
			continue
		}
		displayUsageStats(limits)
	}
}

// redactedFetchErr renders a fetch error for log output. A raw error can
// embed an HTTP response body verbatim (RateLimitError keeps it for
// diagnostics), and a response body is not guaranteed token-free — so the
// daemon log gets only a classification plus token-free metadata, never the
// raw error (ISC-60; same caution as briefStatusErr below for the CLI).
func redactedFetchErr(err error) string {
	var rl *RateLimitError
	if errors.As(err, &rl) {
		if rl.RetryAfter > 0 {
			return fmt.Sprintf("rate limited (status %d), retry after %s", rl.StatusCode, rl.RetryAfter)
		}
		return fmt.Sprintf("rate limited (status %d)", rl.StatusCode)
	}
	return briefStatusErr(err)
}

// briefStatusErr renders a fetch error for the terminal status view without
// ever including a raw HTTP response body (which, unlike this program's own
// error messages, is not guaranteed token-free — ISC-60 caution).
func briefStatusErr(err error) string {
	switch {
	case errors.Is(err, ErrAuthFailed):
		return "authentication failed"
	case errors.Is(err, ErrTransient):
		return "temporarily unavailable"
	default:
		return "fetch error"
	}
}

// printUsageStatusSection prints today's usage for the CLI: a per-profile
// breakdown when more than one profile is configured (ISC-48), or the single
// active fetch (limits) otherwise — identical to pre-feature output at <=1
// profile (ISC-42/54 CLI analogue).
func printUsageStatusSection(limits *UsageLimits) {
	profiles := ListProfiles()
	if len(profiles) > 1 {
		displayMultiProfileStatus(profiles, activeProfileName(LoadConfig()))
		return
	}
	displayUsageStats(limits)
}

// --- CLI surface: `profile ...` subcommands (F6, ISC-15, 16, 43-49) --------

// claudeAccountIdentity is the tiny slice of <dir>/.claude.json this program
// reads to show an email/identity for `profile list` (ISC-16).
type claudeAccountIdentity struct {
	OauthAccount *struct {
		EmailAddress string `json:"emailAddress"`
	} `json:"oauthAccount"`
}

// profileIdentityFor reads <dir>/.claude.json (if present) and returns the
// signed-in account's email, or "" if the file is absent, unreadable, or has
// no oauthAccount — never an error, since this is display-only best-effort
// and read-only (ISC-16).
func profileIdentityFor(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, ".claude.json"))
	if err != nil {
		return ""
	}
	var id claudeAccountIdentity
	if err := json.Unmarshal(data, &id); err != nil {
		return ""
	}
	if id.OauthAccount == nil {
		return ""
	}
	return id.OauthAccount.EmailAddress
}

// filterEnv returns env with every entry whose key equals key removed — used
// to strip an inherited CLAUDE_CONFIG_DIR before exec'ing into the default
// profile, so a caller's shell export can never leak through and silently
// override "unset" (ISC-15's default-profile clause, the login-flow analogue
// of ISC-83).
func filterEnv(env []string, key string) []string {
	prefix := key + "="
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// realClaudeBinary resolves the actual `claude` binary on PATH for `profile
// login`, explicitly excluding this program's own installed shim directory —
// `profile login` must reach the REAL claude, never our own shim (which would
// just read the active-profile state file and point right back at whatever
// is already active, defeating the point of logging into a SPECIFIC
// profile).
func realClaudeBinary() (string, error) {
	shim, shimErr := shimDir()
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		if shimErr == nil && dir == shim {
			continue
		}
		candidate := filepath.Join(dir, "claude")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not find the real 'claude' binary on PATH (excluding the claude-monitor-lite shim directory) — install Claude Code first")
}

func handleProfileCommand(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: claude-monitor-lite profile <add|list|switch|remove|login|shim> ...")
		os.Exit(1)
	}
	switch args[0] {
	case "add":
		handleProfileAdd(args[1:])
	case "list":
		handleProfileList()
	case "switch":
		handleProfileSwitch(args[1:])
	case "remove":
		handleProfileRemove(args[1:])
	case "login":
		handleProfileLogin(args[1:])
	case "shim":
		handleProfileShim(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "Unknown profile subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

func handleProfileAdd(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: claude-monitor-lite profile add <name> [--dir path]")
		os.Exit(1)
	}
	name := args[0]
	dir := ""
	for i := 1; i < len(args); i++ {
		if args[i] == "--dir" && i+1 < len(args) {
			dir = args[i+1]
			i++
		}
	}
	meta, err := AddProfile(name, dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to add profile: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ Profile %q added at %s\n", meta.Name, meta.Dir)
	fmt.Printf("  Next: claude-monitor-lite profile login %s\n", meta.Name)
}

func handleProfileList() {
	if err := EnsureDefaultProfile(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to prepare profile registry: %v\n", err)
		os.Exit(1)
	}
	cfg := LoadConfig()
	active := activeProfileName(cfg)

	fmt.Println("Profiles:")
	for _, p := range cfg.Profiles {
		marker := " "
		if p.Name == active {
			marker = "*"
		}
		identity := profileIdentityFor(p.Dir)
		if identity == "" {
			identity = "(no identity on file)"
		}
		fmt.Printf(" %s %-20s %-40s %-14s %s\n", marker, p.Name, p.Dir, identity, credentialStatusLabel(CredentialHealth(p.Dir)))
	}
}

func handleProfileSwitch(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: claude-monitor-lite profile switch <name>")
		os.Exit(1)
	}
	result, err := SwitchProfile(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Switch failed: %v\n", err)
		os.Exit(1)
	}
	if result.AlreadyActive {
		fmt.Printf("Profile %q is already active.\n", result.To)
		return
	}
	fmt.Printf("✓ Switched active profile: %s -> %s (credential: %s)\n", result.From, result.To, credentialStatusLabel(result.Health))
	fmt.Println("Only NEW claude sessions (started after this, via the installed shim) pick this up — already-running sessions are unaffected.")
}

func handleProfileRemove(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: claude-monitor-lite profile remove <name>")
		os.Exit(1)
	}
	if err := RemoveProfile(args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to remove profile: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✓ Profile %q removed from the registry (its config directory and credentials were left untouched).\n", args[0])
}

func handleProfileLogin(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: claude-monitor-lite profile login <name>")
		os.Exit(1)
	}
	name := args[0]

	if err := EnsureDefaultProfile(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to prepare profile registry: %v\n", err)
		os.Exit(1)
	}
	profiles := ListProfiles()
	meta, ok := findProfile(profiles, name)
	if !ok {
		fmt.Fprintf(os.Stderr, "Unknown profile %q. Known profiles: %s\n", name, strings.Join(profileNames(profiles), ", "))
		os.Exit(1)
	}

	realClaude, err := realClaudeBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	env := filterEnv(os.Environ(), "CLAUDE_CONFIG_DIR")
	dirDesc := "CLAUDE_CONFIG_DIR unset — default"
	if meta.Name != defaultProfileName {
		env = append(env, "CLAUDE_CONFIG_DIR="+meta.Dir)
		dirDesc = "CLAUDE_CONFIG_DIR=" + meta.Dir
	}

	fmt.Printf("Launching claude for profile %q (%s)...\n", meta.Name, dirDesc)

	cmd := exec.Command(realClaude)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "claude exited with error: %v\n", err)
		os.Exit(1)
	}
}

func handleProfileShim(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: claude-monitor-lite profile shim <install|uninstall>")
		os.Exit(1)
	}
	switch args[0] {
	case "install":
		pathLine, err := InstallShim()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to install shim: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("✓ Shim installed.")
		fmt.Println("Add this to your shell rc (e.g. ~/.zshrc or ~/.bashrc), then restart your shell:")
		fmt.Println()
		fmt.Println("  " + pathLine)
		fmt.Println()
		fmt.Println("This makes every NEW `claude` invocation honor `claude-monitor-lite profile switch`.")
	case "uninstall":
		if err := UninstallShim(); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to uninstall shim: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("✓ Shim uninstalled.")
	default:
		fmt.Fprintf(os.Stderr, "Unknown shim subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

func main() {
	appConfig = LoadConfig()
	migrateStripLegacySecrets()

	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatal("Failed to get home directory:", err)
	}
	pidFile = filepath.Join(homeDir, ".claude-monitor-lite.pid")

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "start":
			handleAutoStart()
		case "stop":
			handleStop()
		case "logout":
			handleLogout()
		case "install":
			handleInstall()
		case "uninstall":
			handleUninstall()
		case "profile":
			handleProfileCommand(os.Args[2:])
		case "version", "--version", "-v":
			fmt.Printf("claude-monitor-lite %s\n", version)
			os.Exit(0)
		case "help", "--help", "-h":
			printUsage()
			os.Exit(0)
		default:
			fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
			printUsage()
			os.Exit(1)
		}
		return
	}

	// Default command (no args) - auto-handle everything
	handleAutoStart()
}

func printUsage() {
	fmt.Printf("Claude Monitor Lite %s - Menu bar monitor for Claude usage\n", version)
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  claude-monitor-lite              Start monitor (login if needed, show status if running)")
	fmt.Println("  claude-monitor-lite start        Same as above")
	fmt.Println("  claude-monitor-lite stop         Stop the monitor")
	fmt.Println("  claude-monitor-lite logout       Clear session and stop monitor")
	fmt.Println("  claude-monitor-lite install      Start automatically at login (LaunchAgent)")
	fmt.Println("  claude-monitor-lite uninstall    Stop starting automatically at login")
	fmt.Println("  claude-monitor-lite version      Show version")
	fmt.Println("  claude-monitor-lite help         Show this help")
	fmt.Println()
	fmt.Println("Multi-profile (isolated Claude Code environments):")
	fmt.Println("  claude-monitor-lite profile add <name> [--dir path]   Register a new isolated profile")
	fmt.Println("  claude-monitor-lite profile list                     List profiles (dir, identity, health, active)")
	fmt.Println("  claude-monitor-lite profile switch <name>             Make <name> the active profile")
	fmt.Println("  claude-monitor-lite profile remove <name>             Unregister a profile (keeps its config dir)")
	fmt.Println("  claude-monitor-lite profile login <name>              Interactive claude login for a profile")
	fmt.Println("  claude-monitor-lite profile shim install              Install the global claude-switching shim")
	fmt.Println("  claude-monitor-lite profile shim uninstall            Remove the shim")
	fmt.Println()
	fmt.Println("First time? Just run: claude-monitor-lite")
}

func handleAutoStart() {
	// Check authentication first
	_, err := LoadAuthSession()
	if err != nil {
		// Not logged in - run login flow
		fmt.Println("⚠️  Not authenticated")
		fmt.Println()
		_, err = handleLoginFlow()
		if err != nil {
			os.Exit(1)
		}
	}

	// Check if already running
	if isRunning() {
		// Already running - show status
		handleStatusDisplay()
		return
	}

	// Not running - show current usage then start it
	session, err := LoadAuthSession()
	if err == nil {
		client := createClientFromSession(session)
		if limits, err := client.GetUsageLimits(); err == nil {
			printUsageStatusSection(limits)
		}
	}

	fmt.Println("⚙️  Starting Claude Monitor Lite...")
	fmt.Println()
	handleStart()
}

// loginValidationAttempts and loginRetryDelay control how hard the login flow
// tries to validate a freshly entered key before giving up.
const (
	loginValidationAttempts = 3
	loginRetryDelay         = 1 * time.Second
)

// validateSession checks that a session key works, retrying on transient
// failures (Cloudflare interstitials, network blips) so a momentary hiccup
// does not reject a valid key. A real auth failure returns immediately.
func validateSession(client *ClaudeUsageClient, attempts int, delay time.Duration) error {
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		err = client.TestSession()
		if err == nil || errors.Is(err, ErrSessionExpired) {
			return err
		}
		if attempt < attempts {
			time.Sleep(delay)
		}
	}
	return err
}

func handleLoginFlow() (*AuthSession, error) {
	// Zero-paste path: reuse Claude Code's existing login if present.
	if session, err := LoginWithClaudeCode(); err == nil {
		fmt.Println("✓ Connected using your Claude Code login — no copy-paste needed.")
		fmt.Println()
		if limits, err := createClientFromSession(session).GetUsageLimits(); err != nil {
			fmt.Printf("Note: Could not fetch usage data: %v\n", err)
			fmt.Println()
		} else {
			displayUsageStats(limits)
		}
		return session, nil
	} else {
		fmt.Printf("Claude Code login not found (%v).\n", err)
		fmt.Println("Falling back to manual session key entry.")
		fmt.Println()
	}

	// Fallback: manual sessionKey paste.
	session, err := LoginWithBrowser()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Login failed: %v\n", err)
		return nil, err
	}

	// Test the session and fetch organization ID
	client := NewClaudeUsageClient(session.SessionKey)
	if err := validateSession(client, loginValidationAttempts, loginRetryDelay); err != nil {
		fmt.Fprintf(os.Stderr, "Session validation failed: %v\n", err)
		fmt.Println("The session key may be invalid. Please try again.")
		return nil, err
	}

	// Save the organization ID
	session.OrganizationID = client.organizationID
	if err := SaveAuthSession(session); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to save organization ID: %v\n", err)
	}

	fmt.Println("✓ Session validated successfully!")
	fmt.Println()

	// Fetch and display current usage
	if limits, err := client.GetUsageLimits(); err != nil {
		fmt.Printf("Note: Could not fetch usage data: %v\n", err)
		fmt.Println()
	} else {
		displayUsageStats(limits)
	}

	return session, nil
}

func handleStatusDisplay() {
	pid, _ := readValidPID()
	fmt.Printf("✓ Already running (PID: %d)\n", pid)
	fmt.Println()

	// Load session
	session, err := LoadAuthSession()
	if err != nil {
		fmt.Println("❌ Not authenticated. Run 'claude-monitor-lite logout' then restart.")
		os.Exit(1)
	}

	client := createClientFromSession(session)
	limits, err := client.GetUsageLimits()
	if err != nil {
		fmt.Printf("Error loading usage data: %v\n", err)
		fmt.Println("Try running 'claude-monitor-lite logout' then restart.")
		os.Exit(1)
	}

	printUsageStatusSection(limits)

	// Show which indicator is displayed in menu bar
	selected := findIndicator(getMenuBarIndicator())

	limit := selected.get(limits)
	utilization := 0.0
	if limit != nil {
		utilization = limit.Utilization
	}

	fmt.Printf("Menu Bar Shows:  %s (%s %d%%)\n", selected.label, getColorIndicator(utilization), roundUtilization(utilization))
}

func handleStart() {
	if os.Getenv("CLAUDE_MONITOR_DAEMON") != "1" {
		if isRunning() {
			fmt.Println("Claude Monitor Lite is already running.")
			fmt.Println("Use 'claude-monitor-lite stop' to stop it first.")
			os.Exit(1)
		}
	}

	daemonize()

	// We are now the detached daemon child — route logs to a file.
	setupDaemonLogging()

	if err := createPIDFile(); err != nil {
		log.Fatal("Failed to create PID file:", err)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		cleanup()
		os.Exit(0)
	}()

	systray.Run(onReady, onExit)
}

func handleStop() {
	pid, ok := readValidPID()
	if !ok {
		fmt.Println("Claude Monitor Lite is not running.")
		os.Exit(0)
	}

	// readValidPID already confirmed this PID is our process.
	process, _ := os.FindProcess(pid) // never errors on Unix
	if err := process.Signal(syscall.SIGTERM); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to stop process: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Claude Monitor Lite (PID: %d) stopped.\n", pid)
	time.Sleep(pidCheckTimeout)
	if err := os.Remove(pidFile); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Warning: Failed to remove PID file: %v\n", err)
	}
}

func handleLogout() {
	// Stop daemon if running
	if pid, ok := readValidPID(); ok {
		fmt.Println("Stopping monitor...")
		process, _ := os.FindProcess(pid) // never errors on Unix
		process.Signal(syscall.SIGTERM)
		time.Sleep(pidCheckTimeout)
		if err := os.Remove(pidFile); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Warning: Failed to remove PID file: %v\n", err)
		}
	}

	// Clear session data only (preserves menu bar settings)
	if err := ClearAuthSession(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to clear session: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✓ Logged out! Session data cleared.")
}

// isOurProcess reports whether pid belongs to a live claude-monitor-lite
// process. os.FindProcess never fails on Unix and PIDs are recycled, so a
// bare liveness check (Signal 0) can return a false positive for an unrelated
// process that inherited the PID; checking the command name guards against that.
func isOurProcess(pid int) bool {
	if pid <= 0 {
		return false
	}
	// `ps` exits non-zero if the PID does not exist.
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "claude-monitor-lite")
}

// readValidPID returns the PID from the PID file only if it belongs to a live
// claude-monitor-lite process. A stale or recycled-PID file is removed and
// (0, false) returned.
func readValidPID() (int, bool) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || !isOurProcess(pid) {
		os.Remove(pidFile)
		return 0, false
	}
	return pid, true
}

func isRunning() bool {
	_, ok := readValidPID()
	return ok
}

// createPIDFile writes the daemon PID file. O_EXCL makes creation fail if the
// file already exists, so two daemons racing to start cannot both claim
// ownership (callers clear stale PID files via readValidPID before reaching here).
func createPIDFile() error {
	f, err := os.OpenFile(pidFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, pidFilePermissions)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(strconv.Itoa(os.Getpid()))
	return err
}

// setupDaemonLogging routes the daemon's log output to a file. The daemon runs
// detached with no terminal attached, so otherwise every log line (transient
// errors, fetch failures) is discarded and the process cannot be diagnosed
// after the fact. Logs go to ~/.claude-monitor-lite.log.
func setupDaemonLogging() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return
	}
	logPath := filepath.Join(homeDir, ".claude-monitor-lite.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, logFilePermissions)
	if err != nil {
		return
	}
	log.SetOutput(f)
	// O_CREATE only sets permissions for a newly created file; enforce them on
	// a pre-existing log file too.
	if err := f.Chmod(logFilePermissions); err != nil {
		log.Printf("Warning: could not restrict log file permissions: %v", err)
	}
	log.Printf("claude-monitor-lite %s started (pid %d)", version, os.Getpid())
}

func cleanup() {
	if pidFile != "" {
		if err := os.Remove(pidFile); err != nil && !os.IsNotExist(err) {
			log.Printf("Warning: Failed to remove PID file: %v\n", err)
		}
	}
}

func onReady() {
	// Create context for graceful shutdown
	appCtx, appCancel = context.WithCancel(context.Background())

	systray.SetTitle("⚪ Loading...")
	systray.SetTooltip("Claude Monitor Lite")

	// Check authentication. With no stored session, try the zero-paste Claude
	// Code path before asking the user to do anything.
	session, err := LoadAuthSession()
	if err != nil {
		if s, e := LoginWithClaudeCode(); e == nil {
			session, err = s, nil
		} else {
			// Daemon has no terminal, so log why auto-login fell through.
			log.Printf("Claude Code auto-login unavailable: %v", e)
		}
	}
	if err != nil {
		systray.SetTitle("⚪ Not logged in")
		mLogin := systray.AddMenuItem("⚠️  Please login first", "Login required")
		mLogin.Disable()
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quit", "Quit the application")

		go func() {
			<-mQuit.ClickedCh
			systray.Quit()
		}()

		fmt.Println("ERROR: Not authenticated. Please run 'claude-monitor-lite' to login first.")
		return
	}

	claudeClient = createClientFromSession(session)

	indicatorMenuItems = make([]*systray.MenuItem, len(indicators))
	for i := range indicators {
		indicatorMenuItems[i] = systray.AddMenuItem(indicators[i].label+": --", "Click to show in menu bar")
	}
	systray.AddSeparator()

	mRefresh = systray.AddMenuItem("Refresh Now", "Refresh usage data")
	mOpenConfig := systray.AddMenuItem("Open Config", "Open config file in editor")
	mAbout := systray.AddMenuItem("About", "Show version info")
	systray.AddSeparator()

	// Multi-profile section: a bounded pool of pre-created, hidden menu
	// items (systray cannot remove items once added — see applyIndicatorMenu
	// above for the same pattern). With <=1 profile configured every slot
	// stays hidden, so the menu renders exactly as pre-feature (ISC-54).
	createProfileMenuPool()
	wireProfileMenuClicks(appCtx)

	mQuit := systray.AddMenuItem("Quit", "Quit the application")

	updateMenuCheckmarks()
	refreshProfileMenu()

	// A single worker owns all usage fetching, so updateStats never runs
	// concurrently with itself.
	go runUpdateWorker(appCtx)

	// Independent poll loop per non-active profile (ISC-36..42, 85-87);
	// no-op layer when <=1 profile is configured.
	go runMultiProfileSupervisor(appCtx)

	// One goroutine per indicator forwards its clicks — the indicator count is
	// dynamic, so a single static select cannot cover them.
	for i := range indicators {
		go func(idx int) {
			for {
				select {
				case <-appCtx.Done():
					return
				case <-indicatorMenuItems[idx].ClickedCh:
					selectIndicator(indicators[idx].key)
				}
			}
		}(i)
	}

	// Event loop — handles non-indicator menu interaction.
	go func() {
		for {
			select {
			case <-appCtx.Done():
				return
			case <-mQuit.ClickedCh:
				appCancel()
				systray.Quit()
				return
			case <-mRefresh.ClickedCh:
				requestRefresh()
			case <-mOpenConfig.ClickedCh:
				go openConfigFile()
			case <-mAbout.ClickedCh:
				go showAboutDialog()
			}
		}
	}()
}

// selectIndicator switches which usage metric the menu bar shows, updating the
// display immediately from cached data so the click feels instant.
func selectIndicator(key string) {
	setMenuBarIndicator(key)
	updateMenuCheckmarks()

	limitsMutex.RLock()
	cached := lastLimits
	limitsMutex.RUnlock()
	if cached != nil {
		// Re-apply visibility: the newly deselected window may now hide, and
		// the newly selected one must show even if it has no data.
		applyIndicatorMenu(cached)
		updateMenuBarDisplay(cached)
	}

	go func() {
		if err := SaveConfigPreservingSession(key); err != nil {
			log.Printf("Failed to save menu bar indicator: %v", err)
		}
	}()
}

// openConfigFile opens the config file in the user's default editor.
func openConfigFile() {
	if err := exec.Command("open", GetConfigPath()).Start(); err != nil {
		log.Printf("Failed to open config file: %v", err)
	}
}

// showAboutDialog shows a version dialog with a button that opens the project
// on GitHub.
func showAboutDialog() {
	script := fmt.Sprintf(`display dialog "Claude Monitor Lite\nVersion: %s" buttons {"View on GitHub", "OK"} default button "OK" with title "About"
if button returned of result is "View on GitHub" then open location "%s"`, version, githubURL)
	if err := exec.Command("osascript", "-e", script).Start(); err != nil {
		log.Printf("Failed to show about dialog: %v", err)
	}
}

func updateMenuCheckmarks() {
	current := getMenuBarIndicator()
	for i := range indicators {
		if indicators[i].key == current {
			indicatorMenuItems[i].Check()
		} else {
			indicatorMenuItems[i].Uncheck()
		}
	}
}

// applyIndicatorMenu refreshes every indicator menu item's title from limits and
// hides the ones with no data, so the menu is not cluttered with permanently
// empty windows. The selected indicator always stays visible — hiding it would
// hide its checkmark and leave the user unable to see their current choice.
func applyIndicatorMenu(limits *UsageLimits) {
	selected := getMenuBarIndicator()
	for i := range indicators {
		limit := indicators[i].get(limits)
		indicatorMenuItems[i].SetTitle(formatUsageWithReset(limit, indicators[i].label+":"))
		if limit != nil || indicators[i].key == selected {
			indicatorMenuItems[i].Show()
		} else {
			indicatorMenuItems[i].Hide()
		}
	}
}

// maxTransientErrors is the number of consecutive transient failures tolerated
// before showing an error in the menu bar. At 60s intervals, 3 = ~3 minutes.
const maxTransientErrors = 3

// updateAction is how the menu bar should react to a failed usage fetch.
type updateAction int

const (
	actionShowError updateAction = iota // show the error state
	actionShowAuth                      // show "Login" — session expired
	actionKeepStale                     // tolerate a transient blip, keep last good data
)

// classifyUpdate decides how the menu bar should react to a failed usage fetch,
// given the error and how many consecutive failures have occurred.
func classifyUpdate(err error, consecutiveCount int) updateAction {
	if errors.Is(err, ErrAuthFailed) {
		return actionShowAuth
	}
	if errors.Is(err, ErrTransient) && consecutiveCount <= maxTransientErrors {
		return actionKeepStale
	}
	return actionShowError
}

// haveLastLimits reports whether a previous successful fetch is cached, so a
// rate-limited poll can serve stale data instead of falling through to the
// error state.
func haveLastLimits() bool {
	limitsMutex.RLock()
	defer limitsMutex.RUnlock()
	return lastLimits != nil
}

// nextPollDelay returns the base interval normally, and on consecutive
// rate-limited (429) polls backs off exponentially from base toward max.
// rateLimitFailures is the count of consecutive 429s including this one
// (0 = not rate-limited). A usable server Retry-After (>0) overrides only
// upward — we never poll sooner than told.
func nextPollDelay(base, max time.Duration, rateLimitFailures int, retryAfter time.Duration) time.Duration {
	if rateLimitFailures <= 0 {
		return base
	}
	d := base
	for i := 1; i < rateLimitFailures && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	if retryAfter > d {
		d = retryAfter
	}
	return d
}

// pollJitterFraction is how far withJitter may spread a delay, as a fraction
// of the delay itself (0.10 = ±10%).
const pollJitterFraction = 0.10

// withJitter spreads a delay by ±10% so many monitors (and Claude Code's own
// usage polls, which share the same rate-limit budget) don't beat in lockstep.
func withJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	delta := (rand.Float64()*2 - 1) * pollJitterFraction * float64(d)
	return d + time.Duration(delta)
}

// refreshTrigger asks the update worker to fetch now. Sends coalesce, so rapid
// refresh requests do not pile up.
var refreshTrigger = make(chan struct{}, 1)

func requestRefresh() {
	select {
	case refreshTrigger <- struct{}{}:
	default:
	}
}

// runUpdateWorker is the single goroutine that fetches usage data — on a timer
// and on demand. Confining fetches to one goroutine means updateStats never
// runs concurrently with itself. The timer is rescheduled after every fetch
// using the delay updateStats returns — the base interval normally, backed
// off further while the endpoint is rate-limiting us — with jitter applied so
// this monitor does not beat in lockstep with others polling the same
// endpoint.
func runUpdateWorker(ctx context.Context) {
	delay := updateStats() // initial fetch
	timer := time.NewTimer(withJitter(delay))
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			delay = updateStats()
			timer.Reset(withJitter(delay))
		case <-refreshTrigger:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			delay = updateStats()
			timer.Reset(withJitter(delay))
		}
	}
}

// updateStats fetches usage data once and updates the menu bar accordingly.
// It returns the next base poll delay (pre-jitter): refreshInterval on
// success or on a non-rate-limit error, or a value backed off toward
// maxBackoffInterval while the endpoint keeps returning 429.
func updateStats() time.Duration {
	// Re-read the profile registry/active profile every cycle so a CLI-made
	// `profile switch` or `profile add`/`remove` is reflected in the menu
	// within one poll cycle (ISC-28, 56), without a dedicated timer of its
	// own — this is the cheapest place to piggyback since it already runs on
	// the same interval as the active profile's own poll.
	refreshProfileMenu()

	if claudeClient == nil {
		systray.SetTitle("⚪ Error")
		return refreshInterval
	}

	// Capture which profile this fetch is for now, before the request goes
	// out — claudeClient's token provider (CurrentOAuthToken ->
	// activeProfileDir()) re-resolves the active profile fresh per call, so
	// this reads the same config state the provider is about to read.
	// Tagging with the name captured at the OLD assignment time (after the
	// round trip) would misattribute exactly the wrong-account data this
	// fix exists to prevent, if a CLI switch landed mid-fetch; capturing
	// before the call ties the tag to whichever credentials actually
	// produced the response (ISC-89).
	fetchedForProfile := activeProfileName(LoadConfig())

	limits, err := claudeClient.GetUsageLimits()
	if err != nil && errors.Is(err, ErrAuthFailed) && claudeClient.authMode == AuthModeOAuth {
		// The cached in-memory token was rejected. Drop it and retry once so
		// the provider (CurrentOAuthToken) re-reads — and, if needed,
		// refreshes — Claude Code's credential before we tell an actually
		// logged-in user to log in again. There is no stored refresh token of
		// our own to fall back to; CurrentOAuthToken owns that entirely.
		InvalidateOAuthToken()
		limits, err = claudeClient.GetUsageLimits()
	}
	if err != nil {
		var rl *RateLimitError
		isRateLimit := errors.As(err, &rl)

		consecutiveMutex.Lock()
		consecutiveErrors++
		count := consecutiveErrors
		var delay time.Duration
		if isRateLimit {
			consecutiveRateLimits++
			delay = nextPollDelay(refreshInterval, maxBackoffInterval, consecutiveRateLimits, rl.RetryAfter)
		} else {
			consecutiveRateLimits = 0
			delay = refreshInterval
		}
		consecutiveMutex.Unlock()

		action := classifyUpdate(err, count)
		// A logged-in user being rate-limited should keep seeing their
		// last-good numbers, not "Error loading data" — but only once there is
		// stale data to show. A cold start with no cache yet still falls
		// through to actionShowError, and a real auth failure (actionShowAuth)
		// is never downgraded.
		if action == actionShowError && errors.Is(err, ErrTransient) && haveLastLimits() {
			action = actionKeepStale
		}

		// The error message goes on the first item, which must be visible even
		// if auto-hide had hidden it (e.g. the 5-hour window had no data).
		switch action {
		case actionShowAuth:
			indicatorMenuItems[0].Show()
			indicatorMenuItems[0].SetTitle("Session expired - please login again")
			systray.SetTitle("⚪ Login")
		case actionKeepStale:
			log.Printf("Transient error (%d/%d): %s", count, maxTransientErrors, redactedFetchErr(err))
		case actionShowError:
			log.Printf("Error fetching usage: %s", redactedFetchErr(err))
			systray.SetTitle("⚪ Error")
			indicatorMenuItems[0].Show()
			indicatorMenuItems[0].SetTitle("Error loading data")
		}
		return delay
	}

	// Success — reset both error counters
	consecutiveMutex.Lock()
	consecutiveErrors = 0
	consecutiveRateLimits = 0
	consecutiveMutex.Unlock()

	// Refresh every indicator's title and hide the ones with no data.
	applyIndicatorMenu(limits)

	// Store limits for instant display switching (thread-safe)
	limitsMutex.Lock()
	lastLimits = limits
	lastLimitsAt = time.Now()
	lastLimitsProfile = fetchedForProfile
	limitsMutex.Unlock()

	// Update menu bar display
	updateMenuBarDisplay(limits)

	return refreshInterval
}

func onExit() {
	if appCancel != nil {
		appCancel()
	}
	cleanup()
}
