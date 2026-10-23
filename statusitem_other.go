//go:build !darwin

package main

// setStatusItemName is a no-op off macOS: the shared-autosave-name problem is
// specific to NSStatusItem.
func setStatusItemName() {}
