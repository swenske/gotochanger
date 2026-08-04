package scsi

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReadFilemarksNoSidecarFile(t *testing.T) {
	got, err := readFilemarks(filepath.Join(t.TempDir(), "VOL001"))
	if err != nil {
		t.Fatalf("readFilemarks: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestRecordFilemarkAppendsInOrder(t *testing.T) {
	vol := filepath.Join(t.TempDir(), "VOL001")
	for _, pos := range []int64{1000, 2000, 3000} {
		if err := recordFilemark(vol, pos); err != nil {
			t.Fatalf("recordFilemark(%d): %v", pos, err)
		}
	}
	got, err := readFilemarks(vol)
	if err != nil {
		t.Fatalf("readFilemarks: %v", err)
	}
	want := []int64{1000, 2000, 3000}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestRecordFilemarkInvalidatesStaleEntries reproduces re-labeling/
// reusing a volume: writing a new filemark earlier than a previous
// session's markers must drop those now-stale entries, not just append
// past them - real tape physically loses whatever used to be recorded
// past the point new data gets written.
func TestRecordFilemarkInvalidatesStaleEntries(t *testing.T) {
	vol := filepath.Join(t.TempDir(), "VOL001")
	for _, pos := range []int64{1000, 2000, 3000} {
		if err := recordFilemark(vol, pos); err != nil {
			t.Fatalf("recordFilemark(%d): %v", pos, err)
		}
	}
	if err := recordFilemark(vol, 500); err != nil {
		t.Fatalf("recordFilemark(500): %v", err)
	}
	got, err := readFilemarks(vol)
	if err != nil {
		t.Fatalf("readFilemarks: %v", err)
	}
	want := []int64{500}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (1000/2000/3000 should all have been invalidated)", got, want)
	}
}

func TestInvalidateFilemarksFromDropsOnlyStrictlyAfter(t *testing.T) {
	vol := filepath.Join(t.TempDir(), "VOL001")
	for _, pos := range []int64{1000, 2000, 3000} {
		if err := recordFilemark(vol, pos); err != nil {
			t.Fatalf("recordFilemark(%d): %v", pos, err)
		}
	}
	if err := invalidateFilemarksFrom(vol, 2000); err != nil {
		t.Fatalf("invalidateFilemarksFrom: %v", err)
	}
	got, err := readFilemarks(vol)
	if err != nil {
		t.Fatalf("readFilemarks: %v", err)
	}
	// 2000 is kept (writing real data starting exactly at a recorded
	// mark's position is the ordinary next-file-after-a-filemark case,
	// not evidence of stale structure - see the function's own doc
	// comment for the real bug this guards against); 3000 (strictly
	// after) is dropped as genuinely stale.
	want := []int64{1000, 2000}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestSequentialFileAppendSurvivesOwnFilemark reproduces the real bug
// found against a real kernel (2026-07-26): the most basic real tape
// usage - write file1, write its filemark, write file2 immediately after
// (starting exactly where that filemark landed, since write6 never
// advances position for a filemark) - used to silently delete file1's
// own just-recorded filemark the moment file2's write ran, because the
// write started exactly at that mark's own position.
func TestSequentialFileAppendSurvivesOwnFilemark(t *testing.T) {
	vol := filepath.Join(t.TempDir(), "VOL001")
	if err := recordFilemark(vol, 7); err != nil { // file1's own trailing mark
		t.Fatalf("recordFilemark: %v", err)
	}
	// file2's write starts at position 7, exactly where that mark is.
	if err := invalidateFilemarksFrom(vol, 7); err != nil {
		t.Fatalf("invalidateFilemarksFrom: %v", err)
	}
	got, err := readFilemarks(vol)
	if err != nil {
		t.Fatalf("readFilemarks: %v", err)
	}
	if want := []int64{7}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v - file1's own filemark must survive file2's write starting at the same position", got, want)
	}
}

func TestFilemarkForward(t *testing.T) {
	marks := []int64{1000, 2000, 3000}
	cases := []struct {
		name      string
		pos       int64
		count     int64
		wantPos   int64
		wantFound int64
	}{
		{"one from BOT lands at first mark", 0, 1, 1000, 1},
		{"two from BOT lands at second mark", 0, 2, 2000, 2},
		{"one from mid-file lands at next mark", 1500, 1, 2000, 1},
		{"already at a mark, one more lands at the next", 1000, 1, 2000, 1},
		{"more than exist runs out", 0, 5, 3000, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pos, found := filemarkForward(marks, c.pos, c.count)
			if pos != c.wantPos || found != c.wantFound {
				t.Errorf("filemarkForward(marks, %d, %d) = (%d, %d), want (%d, %d)", c.pos, c.count, pos, found, c.wantPos, c.wantFound)
			}
		})
	}
}

func TestFilemarkReverse(t *testing.T) {
	marks := []int64{1000, 2000, 3000}
	cases := []struct {
		name      string
		pos       int64
		count     int64
		wantPos   int64
		wantFound int64
	}{
		{"one back from end of file3 lands at mark3", 3500, 1, 3000, 1},
		{"two back lands at mark2", 3500, 2, 2000, 2},
		{"already at a mark, one more lands at the previous", 2000, 1, 1000, 1},
		// filemarkReverse itself lands on the earliest mark it could
		// find, not an absolute clamp to 0 - that clamp is the caller's
		// job (space6's own reverse branch), matching filemarkForward's
		// own contract where the *caller* clamps to end-of-data too.
		{"more than exist stops at the earliest mark found", 3500, 5, 1000, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pos, found := filemarkReverse(marks, c.pos, c.count)
			if pos != c.wantPos || found != c.wantFound {
				t.Errorf("filemarkReverse(marks, %d, %d) = (%d, %d), want (%d, %d)", c.pos, c.count, pos, found, c.wantPos, c.wantFound)
			}
		})
	}
}

func TestInvalidateFilemarksFromNoOpWhenNothingChanges(t *testing.T) {
	vol := filepath.Join(t.TempDir(), "VOL001")
	if err := recordFilemark(vol, 1000); err != nil {
		t.Fatalf("recordFilemark: %v", err)
	}
	// Nothing is >= 5000, so this must not even touch the sidecar file.
	before, err := os.Stat(filemarksPath(vol))
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}
	if err := invalidateFilemarksFrom(vol, 5000); err != nil {
		t.Fatalf("invalidateFilemarksFrom: %v", err)
	}
	after, err := os.Stat(filemarksPath(vol))
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("sidecar file was rewritten despite nothing changing")
	}
}

func TestPartitionPath(t *testing.T) {
	vol := "/data/volumes/VOL001"
	if got := partitionPath(vol, 0); got != vol {
		t.Errorf("partition 0 = %q, want %q unchanged", got, vol)
	}
	if got := partitionPath(vol, -1); got != vol {
		t.Errorf("negative partition = %q, want %q unchanged (treated as 0)", got, vol)
	}
	if got, want := partitionPath(vol, 1), vol+".p1"; got != want {
		t.Errorf("partition 1 = %q, want %q", got, want)
	}
	if got, want := partitionPath(vol, 2), vol+".p2"; got != want {
		t.Errorf("partition 2 = %q, want %q", got, want)
	}
}
