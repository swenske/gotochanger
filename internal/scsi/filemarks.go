package scsi

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// filemarksPath returns the sidecar file this project records a volume's
// filemark positions in, alongside its own backing file - deliberately
// kept out of the backing file itself, which represents only real
// recorded data and is watched byte-for-byte by gotochangerd's own
// inotify activity-watcher/capacity-poll mechanisms; a filemark write
// must never touch that byte stream.
func filemarksPath(volPath string) string {
	return volPath + ".filemarks"
}

// readFilemarks returns every filemark position recorded for volPath, in
// whatever order they're stored (ascending, by construction - see
// recordFilemark). No sidecar file at all is not an error - it just means
// the volume has no filemarks yet (a freshly created cartridge).
func readFilemarks(volPath string) ([]int64, error) {
	data, err := os.ReadFile(filemarksPath(volPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		n, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			continue // a corrupt line is skipped, not fatal to the whole read
		}
		out = append(out, n)
	}
	return out, nil
}

func writeFilemarksFile(volPath string, positions []int64) error {
	var b strings.Builder
	for _, p := range positions {
		b.WriteString(strconv.FormatInt(p, 10))
		b.WriteByte('\n')
	}
	return os.WriteFile(filemarksPath(volPath), []byte(b.String()), 0o644)
}

// recordFilemark appends one filemark at pos, first dropping any existing
// entries >= pos. Real magnetic tape physically retains its recorded
// filemarks regardless of head position - REWIND never clears them - but
// writing new structure at an earlier position than a previous session's
// markers correctly invalidates those now-stale entries, the same reason
// invalidateFilemarksFrom exists for real data writes.
func recordFilemark(volPath string, pos int64) error {
	existing, err := readFilemarks(volPath)
	if err != nil {
		return err
	}
	kept := existing[:0]
	for _, p := range existing {
		if p < pos {
			kept = append(kept, p)
		}
	}
	kept = append(kept, pos)
	return writeFilemarksFile(volPath, kept)
}

// filemarkForward walks marks (must be sorted ascending) forward from
// pos, landing on the position of the count-th filemark strictly ahead
// of pos - see Drive.space6's own doc comment for the exact SCSI-2 spec
// wording this matches. found is how many were actually located (equal
// to count on full success, less than count if marks ran out first, in
// which case the caller is responsible for the spec's own BLANK
// CHECK/residual-info handling - this function only computes the pure
// position math).
func filemarkForward(marks []int64, pos, count int64) (newPos int64, found int64) {
	newPos = pos
	for _, m := range marks {
		if found >= count {
			break
		}
		if m > newPos {
			newPos = m
			found++
		}
	}
	return newPos, found
}

// filemarkReverse is filemarkForward's mirror for reverse spacing -
// walks marks (must be sorted ascending) backward from pos, landing on
// the position of the count-th filemark strictly behind pos. Per the
// same spec text, this lands on the *same numeric position* a forward
// space over that marker would (just approached from the other side),
// which is why this returns a plain position with no separate residual/
// BLANK CHECK handling the way filemarkForward's caller has - running
// out of markers going backward is a silent clamp to 0 (see space6's own
// doc comment for why that deliberately mirrors code=0's existing
// backward-underrun convention instead of inventing a new one).
func filemarkReverse(marks []int64, pos, count int64) (newPos int64, found int64) {
	newPos = pos
	for i := len(marks) - 1; i >= 0; i-- {
		if found >= count {
			break
		}
		if marks[i] < newPos {
			newPos = marks[i]
			found++
		}
	}
	return newPos, found
}

// invalidateFilemarksFrom drops every recorded filemark strictly *after*
// pos - called by write6 before it writes real data. Deliberately keeps
// (does not drop) a mark exactly at pos: real, normal sequential tape
// usage always writes each new file's data starting exactly at the
// position the previous file's own trailing filemark was recorded at
// (write6 never advances d.position for a filemark - see
// Drive.writeFilemarks' own doc comment) - so "write real data at
// exactly a recorded mark's position" is the ordinary multi-file-append
// case, not evidence of stale structure, and must not erase that
// boundary. Only a mark *beyond* the new write position represents
// genuinely stale structure left over from an earlier, now-superseded
// pass (e.g. rewinding and overwriting from an earlier point than a
// previous session's data reached).
//
// Found the hard way, against a real kernel: an earlier version of this
// function used "at or after", which passed every unit test (none of
// them wrote a second file immediately after the first) but broke the
// most basic real sequential-append usage - write file1, write its
// filemark, write file2 starting where that filemark landed - by
// silently deleting file1's own just-recorded filemark the moment
// file2's write6 call ran, since that write's own start position was
// exactly the filemark's position. A real `mt fsf 1` after this landed
// at the right position (nothing wrong was visible from *that* command),
// but attempting to *read* file2 from there then returned zero bytes,
// because the marker meant to bound it no longer existed to stop at -
// SPACE(6)/READ(6) both silently ran off the end of the (no-longer-
// tracked) file, and no meaningful boundary existed to report at all.
//
// A no-op (skips the write entirely) when nothing actually changes, the
// common case for an ordinary sequential append.
func invalidateFilemarksFrom(volPath string, pos int64) error {
	existing, err := readFilemarks(volPath)
	if err != nil {
		return err
	}
	kept := existing[:0]
	for _, p := range existing {
		if p <= pos {
			kept = append(kept, p)
		}
	}
	if len(kept) == len(existing) {
		return nil
	}
	return writeFilemarksFile(volPath, kept)
}
