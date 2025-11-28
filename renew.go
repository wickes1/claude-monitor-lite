package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// Time window tolerance for schedule matching
	// Also serves as retry window (~3 attempts at 30s intervals)
	scheduleToleranceSeconds = 90
	// Log file for auto-renew events
	renewLogFileName = ".claude-monitor-lite-renew.log"
)

// CheckAndTriggerAutoRenew checks if current time matches any scheduled time
// and triggers activation if not already sent today
func CheckAndTriggerAutoRenew(config *Config) {
	if !config.AutoRenew.Enabled || len(config.AutoRenew.Schedule) == 0 {
		return
	}

	now := time.Now()
	currentTime := now.Format("15:04")
	currentDate := now.Format("2006-01-02")

	for _, scheduledTime := range config.AutoRenew.Schedule {
		if shouldTrigger(currentTime, scheduledTime, config.AutoRenew.LastSent, currentDate) {
			message := config.AutoRenew.Message
			if message == "" {
				message = "hello"
			}

			logRenewEvent(fmt.Sprintf("Triggering auto-renew at %s (scheduled: %s)", currentTime, scheduledTime))

			if err := SendActivationMessage(message); err != nil {
				logRenewEvent(fmt.Sprintf("Auto-renew failed: %v", err))
				if updateErr := UpdateAutoRenewLastFailed(scheduledTime, currentDate, err.Error()); updateErr != nil {
					logRenewEvent(fmt.Sprintf("Failed to update lastFailed: %v", updateErr))
				}
			} else {
				logRenewEvent(fmt.Sprintf("Auto-renew successful with message: %s", message))
				if err := UpdateAutoRenewLastSent(scheduledTime, currentDate); err != nil {
					logRenewEvent(fmt.Sprintf("Failed to update lastSent: %v", err))
				}
				*config = LoadConfig()
			}
		}
	}
}

// shouldTrigger checks if we should trigger for a given scheduled time
func shouldTrigger(currentTime, scheduledTime string, lastSent map[string]string, currentDate string) bool {
	// Parse times for comparison
	current, err := time.Parse("15:04", currentTime)
	if err != nil {
		return false
	}

	scheduled, err := time.Parse("15:04", scheduledTime)
	if err != nil {
		return false
	}

	// Check if within tolerance window (current time is within X seconds after scheduled time)
	diff := current.Sub(scheduled)
	if diff < 0 || diff > time.Duration(scheduleToleranceSeconds)*time.Second {
		return false
	}

	// Check if already sent today
	if lastSent != nil {
		if lastSentDate, ok := lastSent[scheduledTime]; ok && lastSentDate == currentDate {
			return false
		}
	}

	return true
}

// SendActivationMessage sends a message to Claude CLI to activate the session
func SendActivationMessage(message string) error {
	// Try to find claude executable
	claudePath, err := findClaudeExecutable()
	if err != nil {
		return fmt.Errorf("claude CLI not found: %w", err)
	}

	// Execute: claude -p "message"
	cmd := exec.Command(claudePath, "-p", message)

	// Don't inherit stdin/stdout/stderr
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to execute claude: %w", err)
	}

	return nil
}

// findClaudeExecutable attempts to find the claude CLI executable
func findClaudeExecutable() (string, error) {
	// Check common locations
	possiblePaths := []string{
		"claude", // In PATH
		"/usr/local/bin/claude",
		"/opt/homebrew/bin/claude",
	}

	// Also check user's home bin
	homeDir, err := os.UserHomeDir()
	if err == nil {
		possiblePaths = append(possiblePaths,
			filepath.Join(homeDir, ".local", "bin", "claude"),
			filepath.Join(homeDir, "bin", "claude"),
		)
	}

	for _, path := range possiblePaths {
		if fullPath, err := exec.LookPath(path); err == nil {
			return fullPath, nil
		}
	}

	return "", fmt.Errorf("claude executable not found in PATH or common locations")
}

// logRenewEvent logs auto-renew events to a file
func logRenewEvent(message string) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return
	}

	logPath := filepath.Join(homeDir, renewLogFileName)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	fmt.Fprintf(f, "[%s] %s\n", timestamp, message)
}

// GetNextScheduledTime returns the next scheduled auto-renew time
func GetNextScheduledTime(config *Config) string {
	if !config.AutoRenew.Enabled || len(config.AutoRenew.Schedule) == 0 {
		return ""
	}

	now := time.Now()
	currentTime := now.Format("15:04")
	currentDate := now.Format("2006-01-02")

	var nextTime string
	var minDiff time.Duration = 24 * time.Hour

	for _, scheduledTime := range config.AutoRenew.Schedule {
		// Skip if already sent today
		if config.AutoRenew.LastSent != nil {
			if lastSentDate, ok := config.AutoRenew.LastSent[scheduledTime]; ok && lastSentDate == currentDate {
				continue
			}
		}

		scheduled, err := time.Parse("15:04", scheduledTime)
		if err != nil {
			continue
		}

		current, err := time.Parse("15:04", currentTime)
		if err != nil {
			continue
		}

		diff := scheduled.Sub(current)
		if diff < 0 {
			// Already passed today, will be tomorrow
			diff += 24 * time.Hour
		}

		if diff < minDiff {
			minDiff = diff
			nextTime = scheduledTime
		}
	}

	return nextTime
}

// HandleRenewCommand handles the 'renew' CLI subcommand
func HandleRenewCommand(args []string) {
	if len(args) == 0 {
		printRenewUsage()
		return
	}

	config := LoadConfig()

	switch args[0] {
	case "on":
		config.AutoRenew.Enabled = true
		if err := SaveAutoRenewConfig(config.AutoRenew); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to save config: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Auto-renew enabled.")
		printAutoRenewStatus(&config)

	case "off":
		config.AutoRenew.Enabled = false
		if err := SaveAutoRenewConfig(config.AutoRenew); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to save config: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Auto-renew disabled.")

	case "now":
		message := config.AutoRenew.Message
		if message == "" {
			message = "hello"
		}
		// Check for custom message in args
		for i, arg := range args {
			if arg == "--message" && i+1 < len(args) {
				message = args[i+1]
				break
			}
		}
		fmt.Printf("Sending activation message: %s\n", message)
		if err := SendActivationMessage(message); err != nil {
			fmt.Fprintf(os.Stderr, "Failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Activation sent successfully!")

	case "status":
		printAutoRenewStatus(&config)

	case "log":
		showRenewLog()

	default:
		// Parse flags: --schedule, --message
		handleRenewFlags(args, &config)
	}
}

func handleRenewFlags(args []string, config *Config) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--schedule":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "Error: --schedule requires a value (e.g., 09:00,14:00)")
				os.Exit(1)
			}
			parts := strings.Split(args[i+1], ",")
			schedule := make([]string, 0, len(parts))
			for _, t := range parts {
				t = strings.TrimSpace(t)
				if _, err := time.Parse("15:04", t); err != nil {
					fmt.Fprintf(os.Stderr, "Invalid time format: %s (use HH:MM)\n", t)
					os.Exit(1)
				}
				schedule = append(schedule, t)
			}
			config.AutoRenew.Schedule = schedule
			i++

		case "--message":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "Error: --message requires a value")
				os.Exit(1)
			}
			config.AutoRenew.Message = args[i+1]
			i++

		default:
			fmt.Fprintf(os.Stderr, "Unknown option: %s\n", args[i])
			printRenewUsage()
			os.Exit(1)
		}
	}

	if err := SaveAutoRenewConfig(config.AutoRenew); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to save config: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Auto-renew settings updated:")
	printAutoRenewStatus(config)
}

func printAutoRenewStatus(config *Config) {
	fmt.Println()
	status := "OFF"
	if config.AutoRenew.Enabled {
		status = "ON"
	}
	fmt.Printf("  Enabled:  %s\n", status)

	if len(config.AutoRenew.Schedule) > 0 {
		fmt.Printf("  Schedule: %s\n", strings.Join(config.AutoRenew.Schedule, ", "))
	} else {
		fmt.Println("  Schedule: (not set)")
	}

	message := config.AutoRenew.Message
	if message == "" {
		message = "hello (default)"
	}
	fmt.Printf("  Message:  %s\n", message)

	if config.AutoRenew.Enabled {
		next := GetNextScheduledTime(config)
		if next != "" {
			fmt.Printf("  Next:     %s\n", next)
		}
	}
	fmt.Println()
}

func showRenewLog() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}

	logPath := filepath.Join(homeDir, renewLogFileName)
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No auto-renew log found yet.")
			return
		}
		log.Fatal(err)
	}

	lines := strings.Split(string(data), "\n")
	// Show last 20 lines
	start := 0
	if len(lines) > 20 {
		start = len(lines) - 20
	}

	fmt.Println("=== Recent Auto-Renew Events ===")
	for _, line := range lines[start:] {
		if line != "" {
			fmt.Println(line)
		}
	}
}

func printRenewUsage() {
	fmt.Println("Auto-renew - Schedule automatic session activation")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  claude-monitor-lite renew on                    Enable auto-renew")
	fmt.Println("  claude-monitor-lite renew off                   Disable auto-renew")
	fmt.Println("  claude-monitor-lite renew now                   Trigger activation now")
	fmt.Println("  claude-monitor-lite renew status                Show current settings")
	fmt.Println("  claude-monitor-lite renew log                   Show recent events")
	fmt.Println("  claude-monitor-lite renew --schedule 09:00,14:00")
	fmt.Println("  claude-monitor-lite renew --message \"hello\"")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  claude-monitor-lite renew --schedule 09:00,14:00 --message hi")
	fmt.Println("  claude-monitor-lite renew on")
}
