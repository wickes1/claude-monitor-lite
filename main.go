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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/getlantern/systray"
)

const (
	// Base poll interval. oauth/usage rate-limits non-Claude-Code clients per
	// account: on a Max account a 60s poll got 429 on every second request
	// (roughly one request per ~2min allowed), while a Team account took 60s
	// fine. 180s clears the strictest observed limit with jitter headroom and
	// matches ccstatusline's cache age for the same endpoint.
	refreshInterval    = 180 * time.Second
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

	appConfig    Config
	pidFile      string
	claudeClient *ClaudeUsageClient

	// Guards appConfig — written by menu clicks, read by the update worker.
	indicatorMutex sync.RWMutex

	// Last fetched limits, for instant display switching. Guarded by limitsMutex.
	lastLimits  *UsageLimits
	limitsMutex sync.RWMutex

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

// indicators lists the selectable metrics; the first is the default.
var indicators = []indicator{
	{"currentSession", "5-Hour Session", func(u *UsageLimits) *UsageLimit { return u.FiveHour }},
	{"weeklyAll", "Weekly (All)", func(u *UsageLimits) *UsageLimit { return u.SevenDay }},
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

// findIndicator returns the indicator for key, or the first (default) entry
// and false when key is unknown.
func findIndicator(key string) (indicator, bool) {
	for i := range indicators {
		if indicators[i].key == key {
			return indicators[i], true
		}
	}
	return indicators[0], false
}

func getMenuBarIndicator() string {
	indicatorMutex.RLock()
	defer indicatorMutex.RUnlock()
	return appConfig.MenuBarIndicator
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
	hours, minutes, hasTime := calculateTimeUntilReset(limit.ResetsAt)

	// Special case: no active session (0% with no reset time)
	if !hasTime && utilization == 0 {
		return fmt.Sprintf("%s %d%% (no active session)", label, utilization)
	}

	if hasTime {
		return fmt.Sprintf("%s %d%% (resets %s, in %dh %dm)",
			label, utilization, formatResetTime(limit.ResetsAt), hours, minutes)
	}

	return fmt.Sprintf("%s %d%%", label, utilization)
}

// Helper function to format usage limit for console display
func formatConsoleUsage(limit *UsageLimit, label string, noSessionMsg string) string {
	if limit == nil {
		return fmt.Sprintf("%s  --\n", label)
	}

	utilization := roundUtilization(limit.Utilization)
	hours, minutes, hasTime := calculateTimeUntilReset(limit.ResetsAt)

	if hasTime {
		return fmt.Sprintf("%s  %3d%%  (resets %s, in %dh %dm)\n",
			label, utilization, formatResetTime(limit.ResetsAt), hours, minutes)
	}

	if noSessionMsg != "" {
		return fmt.Sprintf("%s  %3d%%  (%s)\n", label, utilization, noSessionMsg)
	}
	return fmt.Sprintf("%s  %3d%%\n", label, utilization)
}

// scopedLimitLabel builds the console label for a model-scoped
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

// formatScopedLimitLine renders one model-scoped limits[] entry as a console
// line, in the same format as formatConsoleUsage.
func formatScopedLimitLine(entry LimitEntry) string {
	limit := &UsageLimit{Utilization: entry.Percent, ResetsAt: entry.ResetsAt}
	return formatConsoleUsage(limit, scopedLimitLabel(entry)+":", "")
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
	// Scoped limits (a weekly window narrowed to one model) are display only,
	// not selectable indicators.
	for _, entry := range limits.Limits {
		if entry.Scope != nil {
			fmt.Print(formatScopedLimitLine(entry))
		}
	}
	fmt.Println()
}

// setMenuBarTitle prefixes the title with the instance name so side-by-side
// monitors (one per CLAUDE_CONFIG_DIR) are told apart at a glance.
func setMenuBarTitle(title string) {
	if name := instanceName(); name != "" {
		title = name + " " + title
	}
	systray.SetTitle(title)
}

// Helper function to update menu bar display
func updateMenuBarDisplay(limits *UsageLimits) {
	selected, _ := findIndicator(getMenuBarIndicator())
	limit := selected.get(limits)

	if limit == nil {
		setMenuBarTitle("⚪ --")
		return
	}

	utilization := roundUtilization(limit.Utilization)
	hours, minutes, hasTime := calculateTimeUntilReset(limit.ResetsAt)
	color := getColorIndicator(limit.Utilization)

	if hasTime {
		setMenuBarTitle(fmt.Sprintf("%s %d%% (%dh%dm)", color, utilization, hours, minutes))
	} else {
		setMenuBarTitle(fmt.Sprintf("%s %d%%", color, utilization))
	}
}

func main() {
	// Every dotfile path hangs off the home directory; without it they would
	// silently resolve relative to the working directory.
	if _, err := os.UserHomeDir(); err != nil {
		log.Fatal("Failed to get home directory:", err)
	}
	appConfig = LoadConfig()
	pidFile = instanceFile(".pid")

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "start":
			handleAutoStart()
		case "stop":
			handleStop()
		case "install":
			handleInstall()
		case "uninstall":
			handleUninstall()
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
	fmt.Println("  claude-monitor-lite              Start monitor (show status if running)")
	fmt.Println("  claude-monitor-lite start        Same as above")
	fmt.Println("  claude-monitor-lite stop         Stop the monitor")
	fmt.Println("  claude-monitor-lite install      Start automatically at login (LaunchAgent)")
	fmt.Println("  claude-monitor-lite uninstall    Stop starting automatically at login")
	fmt.Println("  claude-monitor-lite version      Show version")
	fmt.Println("  claude-monitor-lite help         Show this help")
	fmt.Println()
	fmt.Println("Reads your Claude Code login; set CLAUDE_CONFIG_DIR to watch another environment.")
}

// handleAutoStart shows status if the monitor is running, otherwise starts it.
// A missing Claude Code login is reported here, at the terminal, rather than
// as a menu bar that can only say "Login".
func handleAutoStart() {
	if isRunning() {
		handleStatusDisplay()
		return
	}
	if _, err := readCredential(); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	fmt.Println("⚙️  Starting Claude Monitor Lite...")
	handleStart()
}

func handleStatusDisplay() {
	pid, _ := readValidPID()
	fmt.Printf("✓ Already running (PID: %d)\n", pid)
	fmt.Println()

	path := instanceFile(".usage.json")
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("No usage fetched yet — check the menu bar in a moment.")
		return
	}
	var limits *UsageLimits
	if err := json.Unmarshal(data, &limits); err != nil {
		fmt.Fprintf(os.Stderr, "Unreadable usage cache %s: %v\n", path, err)
		os.Exit(1)
	}
	if info, err := os.Stat(path); err == nil {
		fmt.Printf("As of %s:\n", info.ModTime().Format("15:04"))
	}

	displayUsageStats(limits)

	// Show which indicator is displayed in menu bar
	selected, _ := findIndicator(getMenuBarIndicator())
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
// after the fact. Logs go to this instance's ~/.claude-monitor-lite*.log.
func setupDaemonLogging() {
	f, err := os.OpenFile(instanceFile(".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, logFilePermissions)
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

	setMenuBarTitle("⚪ Loading...")
	systray.SetTooltip(strings.TrimSpace("Claude Monitor Lite " + instanceName()))

	claudeClient = NewUsageClient(CurrentOAuthToken)

	indicatorMenuItems = make([]*systray.MenuItem, len(indicators))
	for i := range indicators {
		indicatorMenuItems[i] = systray.AddMenuItem(indicators[i].label+": --", "Click to show in menu bar")
	}
	systray.AddSeparator()

	mRefresh := systray.AddMenuItem("Refresh Now", "Refresh usage data")
	mAbout := systray.AddMenuItem("About", "Show version info")
	systray.AddSeparator()

	mQuit := systray.AddMenuItem("Quit", "Quit the application")

	updateMenuCheckmarks()

	// A single worker owns all usage fetching, so updateStats never runs
	// concurrently with itself.
	go runUpdateWorker(appCtx)

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
			case <-mAbout.ClickedCh:
				go showAboutDialog()
			}
		}
	}()
}

// selectIndicator switches which usage metric the menu bar shows, updating the
// display immediately from cached data so the click feels instant.
func selectIndicator(key string) {
	// Saving under the same lock keeps rapid clicks from persisting out of order.
	indicatorMutex.Lock()
	appConfig.MenuBarIndicator = key
	err := SaveConfig(appConfig)
	indicatorMutex.Unlock()
	if err != nil {
		log.Printf("Failed to save menu bar indicator: %v", err)
	}
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
// before showing an error in the menu bar — about 9 minutes at refreshInterval.
const maxTransientErrors = 3

// updateAction is how the menu bar should react to a failed usage fetch.
type updateAction int

const (
	actionShowError updateAction = iota // show the error state
	actionShowAuth                      // show "Login" — session expired
	actionKeepStale                     // tolerate a transient blip, keep last good data
)

// classifyUpdate decides how the menu bar should react to a failed usage fetch,
// given the error, how many consecutive failures have occurred, and whether
// last-good data is cached.
//
// Being rate-limited keeps the last-good numbers for as long as it lasts: the
// backoff recovers on its own, and "Error" would only alarm a user whose
// account is fine. Any other transient failure (expired token, outage) gets
// maxTransientErrors polls of grace and then shows "Error", so the menu bar
// never presents frozen numbers as current.
func classifyUpdate(err error, consecutiveCount int, haveCache bool) updateAction {
	var rl *RateLimitError
	switch {
	case errors.Is(err, ErrAuthFailed):
		return actionShowAuth
	case errors.As(err, &rl) && haveCache:
		return actionKeepStale
	case errors.Is(err, ErrTransient) && consecutiveCount <= maxTransientErrors:
		return actionKeepStale
	}
	return actionShowError
}

// haveLastLimits reports whether a previous successful fetch is cached.
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

// withJitter spreads a delay by ±10% so monitors polling the same account don't
// beat in lockstep.
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
	limits, err := claudeClient.GetUsageLimits()
	if errors.Is(err, ErrAuthFailed) {
		// The cached token was rejected. Drop it and retry once so
		// CurrentOAuthToken re-reads Claude Code's credential (which Claude Code
		// may have refreshed) before we tell a logged-in user to log in again.
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

		action := classifyUpdate(err, count, haveLastLimits())

		// The error message goes on the first item, which must be visible even
		// if auto-hide had hidden it (e.g. the 5-hour window had no data).
		switch action {
		case actionShowAuth:
			indicatorMenuItems[0].Show()
			indicatorMenuItems[0].SetTitle("Not logged in to Claude Code")
			setMenuBarTitle("⚪ Login")
		case actionKeepStale:
			log.Printf("Transient error (%d/%d): %v", count, maxTransientErrors, err)
		case actionShowError:
			log.Printf("Error fetching usage: %v", err)
			setMenuBarTitle("⚪ Error")
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
	limitsMutex.Unlock()

	// The status command reads this instead of calling the API itself, which
	// would spend the same per-account rate-limit budget as this poll.
	if data, err := json.Marshal(limits); err == nil {
		if err := writeFileAtomic(instanceFile(".usage.json"), data, configFilePermissions); err != nil {
			log.Printf("Failed to write usage cache: %v", err)
		}
	}

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
