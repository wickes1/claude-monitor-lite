package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/getlantern/systray"
)

const (
	refreshInterval    = 30 * time.Second
	pidCheckTimeout    = 500 * time.Millisecond
	pidFilePermissions = 0644 // Owner read/write, others read
	logFilePermissions = 0644 // Owner read/write, others read
)

// version is set via ldflags at build time (e.g., -X main.version=v1.0.0)
var version = "dev"

var (
	// Menu items (also serve as indicator selectors)
	mCurrentSession *systray.MenuItem
	mWeeklyAll      *systray.MenuItem
	mWeeklySonnet   *systray.MenuItem
	mWeeklyDesign   *systray.MenuItem

	// Refresh button
	mRefresh *systray.MenuItem

	// App config
	appConfig    Config
	pidFile      string
	claudeClient *ClaudeUsageClient

	// Last fetched limits for instant display switching (protected by mutex)
	lastLimits  *UsageLimits
	limitsMutex sync.RWMutex

	// Consecutive error tracking for transient error resilience
	consecutiveErrors int
	consecutiveMutex  sync.Mutex

	// Context for graceful shutdown
	appCtx    context.Context
	appCancel context.CancelFunc
)

// Helper function to create Claude client from session
func createClientFromSession(session *AuthSession) *ClaudeUsageClient {
	if session.OrganizationID != "" {
		return NewClaudeUsageClientWithOrg(session.SessionKey, session.OrganizationID)
	}
	return NewClaudeUsageClient(session.SessionKey)
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

	// Convert to total minutes (no rounding - show exact time remaining)
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
func getSelectedLimit(limits *UsageLimits, indicator string) *UsageLimit {
	switch indicator {
	case "currentSession":
		return limits.FiveHour
	case "weeklyAll":
		return limits.SevenDay
	case "weeklySonnet":
		return limits.SevenDaySonnet
	case "weeklyDesign":
		return limits.SevenDayOmelette
	default:
		return limits.FiveHour
	}
}

// Helper function to display usage stats
func displayUsageStats(limits *UsageLimits) {
	fmt.Println("=== Current Usage ===")
	fmt.Print(formatConsoleUsage(limits.FiveHour, "5-Hour Session:", "no active session"))
	fmt.Print(formatConsoleUsage(limits.SevenDay, "Weekly (All):", ""))
	fmt.Print(formatConsoleUsage(limits.SevenDaySonnet, "Weekly (Sonnet):", ""))
	if limits.SevenDayOmelette != nil {
		fmt.Print(formatConsoleUsage(limits.SevenDayOmelette, "Weekly (Design):", ""))
	}
	fmt.Println()
}

// Helper function to update menu bar display
func updateMenuBarDisplay(limits *UsageLimits) {
	limit := getSelectedLimit(limits, appConfig.MenuBarIndicator)

	if limit == nil {
		systray.SetTitle("⚪ --")
		return
	}

	utilization := roundUtilization(limit.Utilization)
	hours, minutes, hasTime := calculateTimeUntilReset(limit.ResetsAtTime)
	indicator := getColorIndicator(limit.Utilization)

	if hasTime {
		systray.SetTitle(fmt.Sprintf("%s %d%% (%dh%dm)", indicator, utilization, hours, minutes))
	} else {
		systray.SetTitle(fmt.Sprintf("%s %d%%", indicator, utilization))
	}
}

func main() {
	appConfig = LoadConfig()

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
	fmt.Println("  claude-monitor-lite version      Show version")
	fmt.Println("  claude-monitor-lite help         Show this help")
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
			displayUsageStats(limits)
		}
	}

	fmt.Println("⚙️  Starting Claude Monitor Lite...")
	fmt.Println()
	handleStart()
}

func handleLoginFlow() (*AuthSession, error) {
	session, err := LoginWithBrowser()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Login failed: %v\n", err)
		return nil, err
	}

	// Test the session and fetch organization ID
	client := NewClaudeUsageClient(session.SessionKey)
	if err := client.TestSession(); err != nil {
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
	data, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(string(data))
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

	displayUsageStats(limits)

	// Show which indicator is displayed in menu bar
	indicatorNames := map[string]string{
		"currentSession": "5-Hour Session",
		"weeklyAll":      "Weekly (All)",
		"weeklySonnet":   "Weekly (Sonnet)",
		"weeklyDesign":   "Weekly (Design)",
	}

	indicatorName := indicatorNames[appConfig.MenuBarIndicator]
	if indicatorName == "" {
		indicatorName = "5-Hour Session"
	}

	limit := getSelectedLimit(limits, appConfig.MenuBarIndicator)
	utilization := 0.0
	if limit != nil {
		utilization = limit.Utilization
	}

	fmt.Printf("Menu Bar Shows:  %s (%s %d%%)\n", indicatorName, getColorIndicator(utilization), roundUtilization(utilization))
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
	if !isRunning() {
		fmt.Println("Claude Monitor Lite is not running.")
		os.Exit(0)
	}

	data, err := os.ReadFile(pidFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read PID file: %v\n", err)
		os.Exit(1)
	}

	pid, err := strconv.Atoi(string(data))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid PID: %v\n", err)
		os.Exit(1)
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to find process: %v\n", err)
		os.Exit(1)
	}

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
	if isRunning() {
		fmt.Println("Stopping monitor...")
		data, err := os.ReadFile(pidFile)
		if err == nil {
			pid, err := strconv.Atoi(string(data))
			if err == nil {
				process, err := os.FindProcess(pid)
				if err == nil {
					process.Signal(syscall.SIGTERM)
					time.Sleep(pidCheckTimeout)
				}
			}
		}
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

func isRunning() bool {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return false
	}

	pid, err := strconv.Atoi(string(data))
	if err != nil {
		// Invalid PID file, clean it up
		os.Remove(pidFile)
		return false
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		// Process doesn't exist, clean up stale PID file
		os.Remove(pidFile)
		return false
	}

	// Send signal 0 to check if process is alive
	err = process.Signal(syscall.Signal(0))
	if err != nil {
		// Process is dead, clean up stale PID file
		os.Remove(pidFile)
		return false
	}

	return true
}

func createPIDFile() error {
	return os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), pidFilePermissions)
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

	// Check authentication
	session, err := LoadAuthSession()
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

	mCurrentSession = systray.AddMenuItem("5-Hour Session: --", "Click to show in menu bar")
	mWeeklyAll = systray.AddMenuItem("Weekly (All): --", "Click to show in menu bar")
	mWeeklySonnet = systray.AddMenuItem("Weekly (Sonnet): --", "Click to show in menu bar")
	mWeeklyDesign = systray.AddMenuItem("Weekly (Design): --", "Click to show in menu bar")
	systray.AddSeparator()

	mRefresh = systray.AddMenuItem("Refresh Now", "Refresh usage data")
	mOpenConfig := systray.AddMenuItem("Open Config", "Open config file in editor")
	mAbout := systray.AddMenuItem("About", "Show version info")
	systray.AddSeparator()

	mQuit := systray.AddMenuItem("Quit", "Quit the application")

	updateMenuCheckmarks()
	go updateStats()

	go func() {
		ticker := time.NewTicker(refreshInterval)
		defer ticker.Stop()

		for {
			select {
			case <-appCtx.Done():
				return
			case <-ticker.C:
				go updateStats()
			case <-mQuit.ClickedCh:
				appCancel()
				systray.Quit()
				return
			case <-mRefresh.ClickedCh:
				go updateStats()
			case <-mOpenConfig.ClickedCh:
				go func() {
					homeDir, _ := os.UserHomeDir()
					configPath := filepath.Join(homeDir, ".claude-monitor-lite.json")
					exec.Command("open", configPath).Start()
				}()
			case <-mAbout.ClickedCh:
				go func() {
					exec.Command("osascript", "-e",
						fmt.Sprintf(`display dialog "Claude Monitor Lite\nVersion: %s" buttons {"OK"} default button "OK" with title "About"`, version)).Start()
				}()
			case <-mCurrentSession.ClickedCh:
				appConfig.MenuBarIndicator = "currentSession"
				updateMenuCheckmarks()
				limitsMutex.RLock()
				cached := lastLimits
				limitsMutex.RUnlock()
				if cached != nil {
					updateMenuBarDisplay(cached)
				}
				go SaveConfigPreservingSession("currentSession")
			case <-mWeeklyAll.ClickedCh:
				appConfig.MenuBarIndicator = "weeklyAll"
				updateMenuCheckmarks()
				limitsMutex.RLock()
				cached := lastLimits
				limitsMutex.RUnlock()
				if cached != nil {
					updateMenuBarDisplay(cached)
				}
				go SaveConfigPreservingSession("weeklyAll")
			case <-mWeeklySonnet.ClickedCh:
				appConfig.MenuBarIndicator = "weeklySonnet"
				updateMenuCheckmarks()
				limitsMutex.RLock()
				cached := lastLimits
				limitsMutex.RUnlock()
				if cached != nil {
					updateMenuBarDisplay(cached)
				}
				go SaveConfigPreservingSession("weeklySonnet")
			case <-mWeeklyDesign.ClickedCh:
				appConfig.MenuBarIndicator = "weeklyDesign"
				updateMenuCheckmarks()
				limitsMutex.RLock()
				cached := lastLimits
				limitsMutex.RUnlock()
				if cached != nil {
					updateMenuBarDisplay(cached)
				}
				go SaveConfigPreservingSession("weeklyDesign")
			}
		}
	}()
}

func updateMenuCheckmarks() {
	mCurrentSession.Uncheck()
	mWeeklyAll.Uncheck()
	mWeeklySonnet.Uncheck()
	mWeeklyDesign.Uncheck()

	switch appConfig.MenuBarIndicator {
	case "currentSession":
		mCurrentSession.Check()
	case "weeklyAll":
		mWeeklyAll.Check()
	case "weeklySonnet":
		mWeeklySonnet.Check()
	case "weeklyDesign":
		mWeeklyDesign.Check()
	default:
		mCurrentSession.Check()
	}
}

// maxTransientErrors is the number of consecutive transient failures tolerated
// before showing an error in the menu bar. At 30s intervals, 3 = ~1.5 minutes.
const maxTransientErrors = 3

func updateStats() {
	if claudeClient == nil {
		systray.SetTitle("⚪ Error")
		return
	}

	limits, err := claudeClient.GetUsageLimits()
	if err != nil {
		consecutiveMutex.Lock()
		consecutiveErrors++
		count := consecutiveErrors
		consecutiveMutex.Unlock()

		// Real auth failure — show immediately
		if errors.Is(err, ErrAuthFailed) {
			mCurrentSession.SetTitle("Session expired - please login again")
			systray.SetTitle("⚪ Login")
			return
		}

		// Transient error — keep showing last good data if within tolerance
		if errors.Is(err, ErrTransient) && count <= maxTransientErrors {
			log.Printf("Transient error (%d/%d): %v", count, maxTransientErrors, err)
			return // keep current display unchanged
		}

		// Exceeded tolerance or unknown error — show error state
		log.Printf("Error fetching usage: %v", err)
		systray.SetTitle("⚪ Error")
		mCurrentSession.SetTitle("Error loading data")
		return
	}

	// Success — reset error counter
	consecutiveMutex.Lock()
	consecutiveErrors = 0
	consecutiveMutex.Unlock()

	// Update menu items using helper functions
	mCurrentSession.SetTitle(formatUsageWithReset(limits.FiveHour, "5-Hour Session:"))
	mWeeklyAll.SetTitle(formatUsageWithReset(limits.SevenDay, "Weekly (All):"))
	mWeeklySonnet.SetTitle(formatUsageWithReset(limits.SevenDaySonnet, "Weekly (Sonnet):"))
	mWeeklyDesign.SetTitle(formatUsageWithReset(limits.SevenDayOmelette, "Weekly (Design):"))

	// Store limits for instant display switching (thread-safe)
	limitsMutex.Lock()
	lastLimits = limits
	limitsMutex.Unlock()

	// Update menu bar display
	updateMenuBarDisplay(limits)
}

func onExit() {
	if appCancel != nil {
		appCancel()
	}
	cleanup()
}
