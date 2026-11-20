//go:build !darwin

package main

// showAboutPanel is a no-op off macOS: the window is built from AppKit.
func showAboutPanel(title, summary string, rows [][2]string, repoURL, notesURL string) {}
