// daemon.go - Background process handling

package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

const (
	daemonStartupDelay = 100 * time.Millisecond
)

// daemonize starts the process in background if not already daemonized
func daemonize() {
	// Check if we're already the background process
	if os.Getenv("CLAUDE_MONITOR_DAEMON") == "1" {
		// We're the daemon child, continue normally
		return
	}

	// Get the executable path
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to get executable path: %v\n", err)
		os.Exit(1)
	}

	// Start a new process in background
	cmd := exec.Command(executable)
	cmd.Env = append(os.Environ(), "CLAUDE_MONITOR_DAEMON=1")

	// Detach from terminal (don't inherit stdin/stdout/stderr)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	// Start the background process
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start background process: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Claude Monitor Lite started in background (PID: %d)\n", cmd.Process.Pid)
	fmt.Println("Click the menu bar icon to view usage.")
	fmt.Println("Quit via the menu bar to stop.")

	// Wait a moment for the child to create its PID file
	time.Sleep(daemonStartupDelay)

	// Exit the parent process
	os.Exit(0)
}
