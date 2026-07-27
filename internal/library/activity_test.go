package library

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// withShortActivityIdlePeriod temporarily shrinks driveActivityIdlePeriod
// so tests don't have to wait out the real (3s) default.
func withShortActivityIdlePeriod(t *testing.T, d time.Duration) {
	t.Helper()
	orig := driveActivityIdlePeriod
	driveActivityIdlePeriod = d
	t.Cleanup(func() { driveActivityIdlePeriod = orig })
}

// drainActivityWatcherStops blocks until every driveActivityWatcher.stop()
// call so far has fully finished its async cleanup (see
// pendingWatcherStops' doc comment - stop() itself no longer blocks, to
// avoid a deadlock against Library.mu). Tests that call stop() (directly,
// or indirectly via Unload/Library teardown) and then mutate a shared
// package var like driveActivityIdlePeriod/activityPollInterval must call
// this first, or risk exactly the leaked-goroutine race go test -race is
// designed to catch.
func drainActivityWatcherStops(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		pendingWatcherStops.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for activity watchers to stop")
	}
}

// runDebounceActivity starts debounceActivity in its own goroutine and
// returns an events channel plus a stop func that signals it and blocks
// until it has actually returned - unlike a bare "close(stopCh)", this
// guarantees the goroutine can't still be mid-read of a package var
// (e.g. driveActivityIdlePeriod) by the time a later test's cleanup
// mutates it, which is exactly the kind of leaked-goroutine race
// go test -race is designed to catch.
func runDebounceActivity(onActivity func(kind string)) (events chan string, stop func()) {
	events = make(chan string, 8)
	stopCh := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		debounceActivity(events, stopCh, onActivity)
		close(finished)
	}()
	stop = func() {
		close(stopCh)
		<-finished
	}
	return events, stop
}

// TestDebounceActivityEdgeTriggered verifies debounceActivity only calls
// onActivity on an actual kind change, not once per raw event, and
// eventually reports back to idle ("") after a quiet period.
func TestDebounceActivityEdgeTriggered(t *testing.T) {
	withShortActivityIdlePeriod(t, 40*time.Millisecond)

	var got []string
	done := make(chan struct{})
	events, stop := runDebounceActivity(func(kind string) {
		got = append(got, kind)
		if kind == "" {
			close(done)
		}
	})
	defer stop()

	// Three "writing" signals in a row must collapse to a single edge.
	events <- driveActivityWrite
	events <- driveActivityWrite
	events <- driveActivityWrite

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for idle callback")
	}

	want := []string{driveActivityWrite, ""}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("entry %d: expected %q, got %q (full: %v)", i, w, got[i], got)
		}
	}
}

// TestDebounceActivityKindChange verifies a genuine kind change (read ->
// write) is reported as its own edge, not swallowed by the "only report
// on change" rule.
func TestDebounceActivityKindChange(t *testing.T) {
	withShortActivityIdlePeriod(t, time.Hour) // never let idle fire during this test

	got := make(chan string, 8)
	events, stop := runDebounceActivity(func(kind string) { got <- kind })
	defer stop()

	events <- driveActivityRead
	events <- driveActivityRead
	events <- driveActivityWrite

	want := []string{driveActivityRead, driveActivityWrite}
	for i, w := range want {
		select {
		case k := <-got:
			if k != w {
				t.Fatalf("edge %d: expected %q, got %q", i, w, k)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for edge %d (%q)", i, w)
		}
	}
}

// TestLoadStartsActivityWatcherAndUnloadStopsIt is the full-stack test:
// Load must start monitoring the real backing file (whichever platform
// implementation startDriveActivityWatcher resolves to), a real write to
// the drive's device-path symlink must surface as Drive.Activity +
// DRIVE.ACTIVITY.WRITE.STARTED, and Unload must stop monitoring (no
// further activity events, even if the file is written to afterward).
func TestLoadStartsActivityWatcherAndUnloadStopsIt(t *testing.T) {
	withShortActivityIdlePeriod(t, 200*time.Millisecond)
	activityPollInterval = 20 * time.Millisecond // speed up the non-Linux/inotify-unavailable fallback too
	t.Cleanup(func() { activityPollInterval = 250 * time.Millisecond })

	lib := newTestLibrary(t)
	placeVolumeInFirstSlot(lib, "VOLA0001")
	if err := os.WriteFile(lib.slots[0].Volume.Path, []byte("seed"), 0o640); err != nil {
		t.Fatalf("seed backing file: %v", err)
	}
	fromAddr := lib.slots[0].Address

	if err := lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 0, ""); err != nil {
		t.Fatalf("load: %v", err)
	}

	devicePath := lib.drives[0].DevicePath
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		lib.mu.RLock()
		activity := lib.drives[0].Activity
		lib.mu.RUnlock()
		if activity == "" {
			if err := os.WriteFile(devicePath, []byte("more data via device path"), 0o640); err != nil {
				t.Fatalf("write via device path: %v", err)
			}
		}
		if activity == driveActivityWrite {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	lib.mu.RLock()
	activity := lib.drives[0].Activity
	lib.mu.RUnlock()
	if activity != driveActivityWrite {
		t.Fatalf("expected Drive.Activity to become %q after a real write, got %q", driveActivityWrite, activity)
	}

	found := false
	for _, e := range lib.Events() {
		if e.Code == EventCodeDriveActivityWriteStarted {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a %s event", EventCodeDriveActivityWriteStarted)
	}

	if err := lib.Unload(0, ElementRef{Kind: KindSlot, Address: fromAddr}, ""); err != nil {
		t.Fatalf("unload: %v", err)
	}
	lib.mu.RLock()
	_, stillWatched := lib.driveWatchers[0]
	lib.mu.RUnlock()
	if stillWatched {
		t.Fatalf("expected the drive 0 watcher to be stopped after Unload")
	}
	// stop() itself is async now (see pendingWatcherStops' doc comment) -
	// drain it before this test's t.Cleanup mutates activityPollInterval,
	// or a still-running watcher goroutine could race that mutation.
	drainActivityWatcherStops(t)
}

// TestUnloadDoesNotDeadlockUnderConcurrentDriveActivity is the direct
// regression test for the deadlock fixed by making driveActivityWatcher.stop
// async (see pendingWatcherStops' doc comment): Unload holds l.mu and used
// to synchronously wg.Wait() for the watcher's debounceActivity goroutine
// to exit, but that goroutine can itself be blocked acquiring l.mu inside
// recordDriveActivity at the exact same moment - a real circular wait. This
// keeps a real writer goroutine hammering the backing file (to keep
// recordDriveActivity callbacks firing) right up until the instant Unload
// is called, so the race window is actually exercised, not just plausible
// in theory. Run with -race.
func TestUnloadDoesNotDeadlockUnderConcurrentDriveActivity(t *testing.T) {
	withShortActivityIdlePeriod(t, 10*time.Millisecond)
	activityPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { activityPollInterval = 250 * time.Millisecond })

	lib := newTestLibrary(t)
	placeVolumeInFirstSlot(lib, "VOLA0001")
	path := lib.slots[0].Volume.Path
	if err := os.WriteFile(path, []byte("seed"), 0o640); err != nil {
		t.Fatalf("seed backing file: %v", err)
	}
	fromAddr := lib.slots[0].Address

	if err := lib.Load(ElementRef{Kind: KindSlot, Address: fromAddr}, 0, ""); err != nil {
		t.Fatalf("load: %v", err)
	}

	stopWriting := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		i := 0
		for {
			select {
			case <-stopWriting:
				return
			default:
			}
			i++
			_ = os.WriteFile(path, []byte(fmt.Sprintf("data-%d", i)), 0o640)
			time.Sleep(time.Millisecond)
		}
	}()

	unloadDone := make(chan error, 1)
	go func() {
		unloadDone <- lib.Unload(0, ElementRef{Kind: KindSlot, Address: fromAddr}, "")
	}()

	select {
	case err := <-unloadDone:
		close(stopWriting)
		<-writerDone
		if err != nil {
			t.Fatalf("unload: %v", err)
		}
	case <-time.After(3 * time.Second):
		close(stopWriting)
		t.Fatal("Unload appears deadlocked under concurrent drive activity")
	}
	drainActivityWatcherStops(t)
}
