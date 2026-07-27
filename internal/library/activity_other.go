//go:build !linux

package library

// Non-Linux fallback: the real deployment target is Debian trixie only
// (see activity_linux.go's doc comment), so this exists purely so
// `go build`/`go vet`/`go test` keep working on a non-Linux dev machine -
// it preserves this codebase's previous (pre-rework) detection
// granularity (a periodic file-size poll) rather than leaving a loaded
// drive completely unmonitored.
func startDriveActivityWatcher(path string, onActivity func(kind string)) driveActivityWatcher {
	return startPollingActivityWatcher(path, onActivity)
}
