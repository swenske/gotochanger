package tcmu

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// lockPathForTest points hostDiscoveryLockPath at a throwaway path for the
// duration of one test, returning a func that restores the original -
// so tests never flock the real system-wide lock, which may legitimately
// be held by a real gotochanger-tcmud instance on the machine running
// `go test`.
func lockPathForTest(path string) func() {
	orig := hostDiscoveryLockPath
	hostDiscoveryLockPath = path
	return func() { hostDiscoveryLockPath = orig }
}

// TestAcquireHostDiscoveryLockExcludesConcurrentHolder proves the lock
// actually serializes: a second acquisition attempt (via a raw,
// non-blocking flock against the same path, so the test itself doesn't
// block forever if the code under test is broken) must fail while the
// first is held, and must succeed immediately after it's released.
func TestAcquireHostDiscoveryLockExcludesConcurrentHolder(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/tcmud-hostscan.lock"
	orig := lockPathForTest(path)
	defer orig()

	release, err := AcquireHostDiscoveryLock()
	if err != nil {
		t.Fatalf("AcquireHostDiscoveryLock: %v", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock file for probe: %v", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		t.Fatal("expected a concurrent non-blocking flock to fail while the lock is held")
	}

	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("expected the lock to be free after release, got: %v", err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// TestAcquireHostDiscoveryLockBlocksUntilReleased proves a second real
// (blocking) acquisition via the exported function itself waits for the
// first to release rather than racing past it.
func TestAcquireHostDiscoveryLockBlocksUntilReleased(t *testing.T) {
	dir := t.TempDir()
	orig := lockPathForTest(dir + "/tcmud-hostscan.lock")
	defer orig()

	release1, err := AcquireHostDiscoveryLock()
	if err != nil {
		t.Fatalf("first AcquireHostDiscoveryLock: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		release2, err := AcquireHostDiscoveryLock()
		if err != nil {
			t.Errorf("second AcquireHostDiscoveryLock: %v", err)
			return
		}
		close(acquired)
		_ = release2()
	}()

	select {
	case <-acquired:
		t.Fatal("second acquisition succeeded while the first was still held")
	case <-time.After(100 * time.Millisecond):
	}

	if err := release1(); err != nil {
		t.Fatalf("release1: %v", err)
	}

	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second acquisition never completed after the first was released")
	}
}
