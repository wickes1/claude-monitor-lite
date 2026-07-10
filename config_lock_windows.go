//go:build windows

package main

// acquireConfigLock is a no-op on Windows: flock(2) does not exist there and
// Windows remains untested (see README). writeFileAtomic's rename still
// prevents file corruption; cross-process lost-update protection is
// POSIX-only.
func acquireConfigLock(lockPath string) (release func(), err error) {
	return func() {}, nil
}
