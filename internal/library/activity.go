package library

import (
	"os"
	"sync"
	"time"
)

// Drive.Activity values: what a per-drive filesystem watcher last
// observed on the loaded volume's real backing file. Empty means idle
// (no read/write seen within driveActivityIdlePeriod).
const (
	driveActivityRead  = "reading"
	driveActivityWrite = "writing"
)

// driveActivityIdlePeriod is how long a drive's activity watcher waits
// with no new read/write events before reporting back to idle - a
// package var (not a const) so tests can shrink it instead of waiting
// out the real duration.
var driveActivityIdlePeriod = 3 * time.Second

// driveActivityUnloadSettleDelay is a small, fixed pause Unload/
// ejectCleaningTapeAfterCycleLocked take before stopping a drive's
// activity watcher - not a user-configurable simulated latency (it's not
// part of config.LatencySettings), but a real fix for a genuine race: a
// watcher's detection (inotify readLoop -> debounceActivity ->
// recordDriveActivity) happens on its own goroutine, and
// recordDriveActivity needs l.mu - if a real, very fast operation (e.g.
// Bareos labeling a tape: a tiny, near-instant write) is immediately
// followed by Unload, Unload can win the race for l.mu and clear
// drive.Volume before the watcher's already-in-flight callback for that
// write gets to run - at which point recordDriveActivity's staleness
// guard (d.Volume == nil) makes it a silent no-op, and the write
// activity is never reported at all, even though it genuinely happened
// while the drive still held the volume. This pause gives that pipeline
// (a few, at most tens of, milliseconds of real work) a comfortable
// margin to finish and report before the drive's state is torn down -
// see Unload/ejectCleaningTapeAfterCycleLocked. A package var, not a
// const, so tests can shrink it to 0.
var driveActivityUnloadSettleDelay = 250 * time.Millisecond

// driveActivityWatcher is the per-platform interface for observing
// read/write activity on a loaded drive's real backing file - started by
// Library.startDriveWatcherLocked when a volume is committed into a
// drive (Load), stopped by Library.stopDriveWatcherLocked right before
// its device-path symlink is removed (Unload,
// ejectCleaningTapeAfterCycleLocked). Implemented per-platform:
// activity_linux.go uses inotify (IN_ACCESS/IN_MODIFY/IN_CLOSE_WRITE),
// activity_other.go falls back to polling the file size so
// `go build`/`go vet`/`go test` still work on a non-Linux dev machine -
// see activity_linux.go's doc comment for why Linux is the only platform
// that needs to actually detect anything (the real deployment target is
// Debian trixie).
type driveActivityWatcher interface {
	// stop must be safe to call at most once and must never block - see
	// pendingWatcherStops' doc comment for why.
	stop()
}

// pendingWatcherStops tracks in-flight async stop() cleanups (see
// inotifyWatcher.stop/pollingActivityWatcher.stop). Production code never
// waits on it - it exists only so tests can deterministically drain those
// goroutines before mutating shared package vars like
// driveActivityIdlePeriod/activityPollInterval, instead of racing them.
var pendingWatcherStops sync.WaitGroup

// startDriveActivityWatcher is implemented once per platform (see
// activity_linux.go / activity_other.go). onActivity is called from the
// watcher's own goroutine - never holding Library's lock - with
// driveActivityRead, driveActivityWrite, or "" (idle), only on a
// transition (edge-triggered via debounceActivity), not once per
// syscall/poll tick.

// debounceActivity turns a raw stream of "reading"/"writing" signals
// (however chatty the underlying detection mechanism is) into edge-
// triggered onActivity calls: one when the kind actually changes, and one
// back to "" after driveActivityIdlePeriod of silence. Shared by both
// platform implementations so only the detection mechanism differs.
func debounceActivity(events <-chan string, stopCh <-chan struct{}, onActivity func(kind string)) {
	var current string
	timer := time.NewTimer(driveActivityIdlePeriod)
	if !timer.Stop() {
		<-timer.C
	}
	timerActive := false
	for {
		select {
		case <-stopCh:
			return
		case kind, ok := <-events:
			if !ok {
				return
			}
			if kind != current {
				current = kind
				onActivity(current)
			}
			if timerActive && !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(driveActivityIdlePeriod)
			timerActive = true
		case <-timer.C:
			timerActive = false
			if current != "" {
				current = ""
				onActivity("")
			}
		}
	}
}

// activityPollInterval is how often the polling fallback watcher (see
// startPollingActivityWatcher) checks the backing file's size.
var activityPollInterval = 250 * time.Millisecond

type pollingActivityWatcher struct {
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// stop signals both of the watcher's goroutines and returns immediately -
// it does NOT wait for them to exit. This is deliberate: stop is called
// from Library.stopDriveWatcherLocked, itself called from inside
// Unload/ejectCleaningTapeAfterCycleLocked while l.mu is already held. If
// stop blocked here (as it used to, via a synchronous wg.Wait()), and the
// watcher's debounceActivity goroutine happened to be mid-callback inside
// recordDriveActivity (which needs l.mu.Lock()) at that exact moment,
// this would deadlock: the caller holds l.mu and waits for the goroutine
// to exit, while the goroutine waits for l.mu to become available - a
// real circular wait, not theoretical (found in production code review).
// Safe to make async: recordDriveActivity already guards against a stale
// callback arriving after this drive's volume/path has changed
// (d.Volume == nil || d.Volume.Path != path), so a late callback from a
// goroutine that hasn't finished exiting yet is a harmless no-op, not a
// correctness issue.
func (w *pollingActivityWatcher) stop() {
	close(w.stopCh)
	pendingWatcherStops.Add(1)
	go func() {
		defer pendingWatcherStops.Done()
		w.wg.Wait()
	}()
}

// startPollingActivityWatcher is the platform-independent fallback
// detection mechanism: it can only ever notice writes (a size change),
// never reads (there is no portable, dependency-free way to observe a
// read without inotify or an equivalent kernel facility) - used directly
// as activity_other.go's non-Linux implementation, and as
// activity_linux.go's own fallback if inotify itself is unavailable.
func startPollingActivityWatcher(path string, onActivity func(kind string)) driveActivityWatcher {
	events := make(chan string, 8)
	stopCh := make(chan struct{})
	w := &pollingActivityWatcher{stopCh: stopCh}
	w.wg.Add(2)
	go func() { defer w.wg.Done(); pollActivityLoop(path, events, stopCh) }()
	go func() { defer w.wg.Done(); debounceActivity(events, stopCh, onActivity) }()
	return w
}

func pollActivityLoop(path string, events chan<- string, stopCh <-chan struct{}) {
	lastSize := int64(-1)
	ticker := time.NewTicker(activityPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			size := info.Size()
			if lastSize >= 0 && size != lastSize {
				select {
				case events <- driveActivityWrite:
				default:
				}
			}
			lastSize = size
		}
	}
}
