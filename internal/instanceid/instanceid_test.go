package instanceid

import (
	"os"
	"path/filepath"
	"testing"
)

// withHardwarePaths points the package-level path vars at fake files
// under t.TempDir() for the duration of the test, restoring the real
// paths afterward - mirrors internal/api/kernel_mode_test.go's
// withKernelModePaths.
func withHardwarePaths(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	origPaths, origDocker := hardwareIDPaths, dockerEnvPath
	t.Cleanup(func() { hardwareIDPaths, dockerEnvPath = origPaths, origDocker })

	hardwareIDPaths = []string{
		filepath.Join(dir, "machine-id"),
		filepath.Join(dir, "product_uuid"),
	}
	dockerEnvPath = filepath.Join(dir, ".dockerenv")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateMissingFilesFallsBackToRandom(t *testing.T) {
	withHardwarePaths(t)

	id1, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	id2, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(id1) != 64 || len(id2) != 64 {
		t.Fatalf("id1 = %q, id2 = %q, want 64 hex chars each", id1, id2)
	}
	if id1 == id2 {
		t.Fatalf("two random fallbacks produced the same id %q, want independent random values", id1)
	}
}

func TestGenerateEmptyFileFallsThroughToNextSource(t *testing.T) {
	withHardwarePaths(t)
	writeFile(t, hardwareIDPaths[0], "")
	writeFile(t, hardwareIDPaths[1], "abc123-real-looking-uuid")

	id, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want := hashHardwareValue("abc123-real-looking-uuid")
	if id != want {
		t.Fatalf("id = %q, want %q (derived from second source)", id, want)
	}
}

func TestGeneratePlaceholderValuesFallBackToRandom(t *testing.T) {
	for _, placeholder := range []string{
		"uninitialized",
		"00000000000000000000000000000000",
		"00000000-0000-0000-0000-000000000000",
	} {
		t.Run(placeholder, func(t *testing.T) {
			withHardwarePaths(t)
			writeFile(t, hardwareIDPaths[0], placeholder)

			id1, err := Generate()
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			id2, err := Generate()
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if id1 == id2 {
				t.Fatalf("placeholder %q produced deterministic output, want random fallback", placeholder)
			}
		})
	}
}

func TestGenerateRealValueIsDeterministic(t *testing.T) {
	withHardwarePaths(t)
	writeFile(t, hardwareIDPaths[0], "d34db33f00112233445566778899aabb")

	id1, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want := hashHardwareValue("d34db33f00112233445566778899aabb")
	if id1 != want {
		t.Fatalf("id = %q, want %q", id1, want)
	}

	id2, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if id2 != id1 {
		t.Fatalf("second Generate() = %q, want same as first %q (survives a state.db wipe)", id2, id1)
	}
}

func TestGenerateSkipsHardwareInDocker(t *testing.T) {
	withHardwarePaths(t)
	writeFile(t, hardwareIDPaths[0], "d34db33f00112233445566778899aabb")
	writeFile(t, dockerEnvPath, "")

	id1, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	id2, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if id1 == id2 {
		t.Fatalf("id derived deterministically despite /.dockerenv present, want random fallback")
	}
	if want := hashHardwareValue("d34db33f00112233445566778899aabb"); id1 == want || id2 == want {
		t.Fatalf("id derived from hardware despite /.dockerenv present")
	}
}
