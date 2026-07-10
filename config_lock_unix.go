//go:build !windows

// Cross-process serialization for config read-modify-write cycles. The
// in-process configWriteMutex cannot serialize the daemon against a
// concurrently invoked CLI command (two separate processes), so the full
// cycle also holds an advisory flock(2) on a sidecar lock file. A sidecar is
// used instead of the config file itself because writeFileAtomic replaces
// the config's inode on every save, which would silently detach a lock held
// on the old inode.
package main

import (
	"os"
	"syscall"
)

// acquireConfigLock takes an exclusive advisory lock on lockPath, blocking
// until it is available, and returns the function that releases it. The lock
// file is left in place after release; its presence carries no meaning, only
// holding the flock does.
func acquireConfigLock(lockPath string) (release func(), err error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		// Errors here are unrecoverable and harmless: closing the descriptor
		// drops the flock regardless.
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
