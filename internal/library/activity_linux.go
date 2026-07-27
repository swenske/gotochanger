//go:build linux

package library

import (
	"encoding/binary"
	"sync"
	"syscall"
	"time"
)

// This is the only platform gotochangerd actually ships on (the .deb
// packaging targets Debian trixie exclusively - see CLAUDE.md), so it's
// the only one that needs to genuinely detect read/write activity. A
// hand-rolled inotify watcher via the stdlib syscall package needs zero
// vendored dependencies (InotifyInit1/InotifyAddWatch/InotifyRmWatch and
// the IN_* mask constants are all already in package syscall on linux),
// matching this codebase's existing convention of re-implementing a
// primitive from stdlib rather than bumping go.mod's pinned go 1.22 for a
// dependency (see the PBKDF2 password hashing implementation).

// inotifyEventHeaderSize is sizeof(struct inotify_event) minus the
// variable-length trailing name (wd int32, mask/cookie/len uint32 each).
const inotifyEventHeaderSize = 16

type inotifyWatcher struct {
	fd, wd int
	events chan string
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// startDriveActivityWatcher watches path (a loaded volume's real backing
// file) for read/write activity. If inotify itself is unavailable for
// any reason (e.g. a restrictive sandbox), it falls back to the same
// polling watcher activity_other.go provides on non-Linux platforms,
// rather than leaving the drive permanently unmonitored.
func startDriveActivityWatcher(path string, onActivity func(kind string)) driveActivityWatcher {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		return startPollingActivityWatcher(path, onActivity)
	}
	wd, err := syscall.InotifyAddWatch(fd, path, syscall.IN_ACCESS|syscall.IN_MODIFY|syscall.IN_CLOSE_WRITE)
	if err != nil {
		_ = syscall.Close(fd)
		return startPollingActivityWatcher(path, onActivity)
	}
	w := &inotifyWatcher{fd: fd, wd: wd, events: make(chan string, 8), stopCh: make(chan struct{})}
	w.wg.Add(2)
	go func() { defer w.wg.Done(); w.readLoop() }()
	go func() { defer w.wg.Done(); debounceActivity(w.events, w.stopCh, onActivity) }()
	return w
}

// stop signals both of the watcher's goroutines and returns immediately -
// it does NOT wait for them to exit before releasing the inotify fd. This
// is deliberate, not an oversight: stop is called from
// Library.stopDriveWatcherLocked, itself called from inside
// Unload/ejectCleaningTapeAfterCycleLocked while l.mu is already held. A
// synchronous wg.Wait() here can deadlock against l.mu: if debounceActivity
// is mid-callback inside recordDriveActivity (which needs l.mu.Lock())
// at the exact moment stop is called, the caller (holding l.mu, waiting
// on wg.Wait()) and the watcher goroutine (holding nothing, waiting on
// l.mu) form a real circular wait - found in production code review, not
// theoretical. Safe to make async: recordDriveActivity already guards
// against a stale callback arriving after this drive's volume/path has
// changed (d.Volume == nil || d.Volume.Path != path), so a callback that
// lands after stop() has returned but before the goroutines have actually
// exited is a harmless no-op. The fd/watch cleanup itself is deferred into
// the same detached goroutine that waits for both goroutines to exit,
// so it still always happens, just not before the caller (still holding
// l.mu) gets to proceed.
func (w *inotifyWatcher) stop() {
	close(w.stopCh)
	pendingWatcherStops.Add(1)
	go func() {
		defer pendingWatcherStops.Done()
		w.wg.Wait()
		_, _ = syscall.InotifyRmWatch(w.fd, uint32(w.wd))
		_ = syscall.Close(w.fd)
	}()
}

// readLoop reads raw inotify events off the non-blocking fd, translating
// each into "reading"/"writing" on w.events for debounceActivity to
// coalesce. The fd is non-blocking specifically so this loop can notice
// stopCh promptly instead of being stuck in a blocking Read - the 20ms
// poll between reads is an implementation detail of waiting for more
// data, not the detection mechanism itself (that's still inotify: this
// loop only wakes usefully when the kernel actually has an event
// queued, it isn't stat-ing the file).
func (w *inotifyWatcher) readLoop() {
	buf := make([]byte, 4096)
	for {
		select {
		case <-w.stopCh:
			return
		default:
		}
		n, err := syscall.Read(w.fd, buf)
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EINTR {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			return // fd closed by stop(), or a real error - either way, done
		}
		off := 0
		for off+inotifyEventHeaderSize <= n {
			mask := binary.LittleEndian.Uint32(buf[off+4 : off+8])
			nameLen := binary.LittleEndian.Uint32(buf[off+12 : off+16])
			off += inotifyEventHeaderSize + int(nameLen)

			var kind string
			switch {
			case mask&(syscall.IN_MODIFY|syscall.IN_CLOSE_WRITE) != 0:
				kind = driveActivityWrite
			case mask&syscall.IN_ACCESS != 0:
				kind = driveActivityRead
			default:
				continue
			}
			select {
			case w.events <- kind:
			default:
			}
		}
	}
}
