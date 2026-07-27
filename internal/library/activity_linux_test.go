//go:build linux

package library

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestInotifyWatcherDetectsWriteAndRead exercises the real Linux
// implementation (startDriveActivityWatcher resolving to inotifyWatcher)
// against a real temp file, with real write(2)/read(2) syscalls - not a
// simulated event - confirming the detection mechanism this whole
// workstream is about actually works, not just the platform-neutral
// debounce logic already covered by TestDebounceActivityEdgeTriggered.
func TestInotifyWatcherDetectsWriteAndRead(t *testing.T) {
	withShortActivityIdlePeriod(t, 100*time.Millisecond)
	path := filepath.Join(t.TempDir(), "volume.bin")
	if err := os.WriteFile(path, []byte("initial"), 0o640); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	kinds := make(chan string, 16)
	w := startDriveActivityWatcher(path, func(kind string) { kinds <- kind })
	// stop() is async now (see pendingWatcherStops' doc comment) - drain
	// it before this test's withShortActivityIdlePeriod cleanup restores
	// driveActivityIdlePeriod, or a still-running watcher goroutine could
	// race that mutation.
	defer func() { w.stop(); drainActivityWatcherStops(t) }()

	// A real write must surface as "writing".
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open for write: %v", err)
	}
	if _, err := f.WriteString("more data"); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()

	select {
	case k := <-kinds:
		if k != driveActivityWrite {
			t.Fatalf("expected %q after a real write, got %q", driveActivityWrite, k)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for write activity to be detected")
	}

	// Wait for it to settle back to idle before checking read detection,
	// so the two don't get coalesced into one edge.
	select {
	case k := <-kinds:
		if k != "" {
			t.Fatalf("expected idle after the write settled, got %q", k)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for idle after write")
	}

	// A real read must surface as "reading".
	rf, err := os.Open(path)
	if err != nil {
		t.Fatalf("open for read: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := rf.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	rf.Close()

	select {
	case k := <-kinds:
		if k != driveActivityRead {
			t.Fatalf("expected %q after a real read, got %q", driveActivityRead, k)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for read activity to be detected")
	}
}
