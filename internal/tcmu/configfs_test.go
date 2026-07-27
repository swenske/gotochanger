package tcmu

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCreateAndEnableBackstore(t *testing.T) {
	root := t.TempDir()
	cfg := BackstoreConfig{HBA: "user_1", Name: "changer0", Subtype: "gotochanger", CfgString: "lib0", SizeBytes: 1024, BlockSize: 512}

	if err := CreateBackstore(root, cfg); err != nil {
		t.Fatalf("CreateBackstore: %v", err)
	}
	dir := backstoreDir(root, cfg)
	control, err := os.ReadFile(filepath.Join(dir, "control"))
	if err != nil {
		t.Fatalf("read control: %v", err)
	}
	want := "dev_config=gotochanger/lib0,dev_size=1024,hw_block_size=512,nl_reply_supported=1"
	if string(control) != want {
		t.Errorf("control = %q, want %q", control, want)
	}

	if err := EnableBackstore(root, cfg); err != nil {
		t.Fatalf("EnableBackstore: %v", err)
	}
	enable, err := os.ReadFile(filepath.Join(dir, "enable"))
	if err != nil {
		t.Fatalf("read enable: %v", err)
	}
	if string(enable) != "1" {
		t.Errorf("enable = %q, want %q", enable, "1")
	}

	// There is deliberately no DisableBackstore to call here - see its
	// removal note in configfs.go (writing "0" to "enable" is rejected by
	// a real kernel). On real configfs, "control"/"enable" are kernel-
	// managed attribute pseudo-files that a single rmdir on the item
	// directory tears down automatically - RemoveBackstore relies on that
	// and deliberately issues only a plain rmdir, not a recursive remove.
	// A plain temp-dir fixture has no equivalent behavior, so the test
	// approximates "the item is otherwise ready to be removed" by
	// clearing the fake attribute files itself first.
	if err := os.Remove(filepath.Join(dir, "control")); err != nil {
		t.Fatalf("test setup: remove fake control file: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "enable")); err != nil {
		t.Fatalf("test setup: remove fake enable file: %v", err)
	}
	if err := RemoveBackstore(root, cfg); err != nil {
		t.Fatalf("RemoveBackstore: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("backstore dir %s still exists after RemoveBackstore", dir)
	}
}

func TestLoopbackTargetLUNAndNexus(t *testing.T) {
	root := t.TempDir()
	backstoreCfg := BackstoreConfig{HBA: "user_1", Name: "drive0", Subtype: "gotochanger", CfgString: "drive0", SizeBytes: 2048, BlockSize: 512}
	if err := CreateBackstore(root, backstoreCfg); err != nil {
		t.Fatalf("CreateBackstore: %v", err)
	}

	loopbackCfg := LoopbackConfig{WWN: "1234567812345678"}
	if err := CreateLoopbackTarget(root, loopbackCfg); err != nil {
		t.Fatalf("CreateLoopbackTarget: %v", err)
	}

	if err := SetLoopbackNexus(root, loopbackCfg); err != nil {
		t.Fatalf("SetLoopbackNexus: %v", err)
	}
	nexus, err := os.ReadFile(filepath.Join(loopbackTPGTDir(root, loopbackCfg), "nexus"))
	if err != nil {
		t.Fatalf("read nexus: %v", err)
	}
	if want := "naa." + loopbackCfg.WWN; string(nexus) != want {
		t.Errorf("nexus = %q, want %q", nexus, want)
	}

	if err := CreateLoopbackLUN(root, loopbackCfg, 0, backstoreCfg); err != nil {
		t.Fatalf("CreateLoopbackLUN: %v", err)
	}
	link := filepath.Join(loopbackTPGTDir(root, loopbackCfg), "lun", "lun_0", backstoreCfg.Name)
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("Readlink(%s): %v", link, err)
	}
	if want := backstoreDir(root, backstoreCfg); target != want {
		t.Errorf("LUN symlink target = %q, want %q", target, want)
	}

	// Teardown, in the order confirmed necessary against a real kernel:
	// unmap the LUN before removing the target, which itself requires the
	// nexus cleared first (see RemoveLoopbackTarget's doc comment).
	if err := RemoveLoopbackLUN(root, loopbackCfg, 0, backstoreCfg); err != nil {
		t.Fatalf("RemoveLoopbackLUN: %v", err)
	}
	if _, err := os.Stat(link); !os.IsNotExist(err) {
		t.Fatalf("lun link %s still exists after RemoveLoopbackLUN", link)
	}

	// RemoveLoopbackTarget's own rmdir can only fully succeed against
	// real configfs, where "nexus" is a kernel attribute pseudo-file
	// rmdir cleans up automatically (same as a backstore's
	// "control"/"enable" - see TestCreateAndEnableBackstore) - unlike
	// those, RemoveLoopbackTarget always re-clears (and thus, against a
	// plain-directory fake, re-creates) "nexus" as its own first internal
	// step, so this fixture can't pre-delete it out of the way first. Test
	// ClearLoopbackNexus's own write behavior directly instead.
	if err := ClearLoopbackNexus(root, loopbackCfg); err != nil {
		t.Fatalf("ClearLoopbackNexus: %v", err)
	}
	nexus, err = os.ReadFile(filepath.Join(loopbackTPGTDir(root, loopbackCfg), "nexus"))
	if err != nil {
		t.Fatalf("read nexus after clear: %v", err)
	}
	if len(nexus) != 0 {
		t.Errorf("nexus after clear = %q, want empty", nexus)
	}
}

func TestNewSCSIHost(t *testing.T) {
	before := []string{"host0", "host1"}
	after := []string{"host0", "host1", "host2"}
	name, path, ok := NewSCSIHost(before, after)
	if !ok {
		t.Fatal("expected a new host to be found")
	}
	if name != "host2" {
		t.Errorf("name = %q, want %q", name, "host2")
	}
	if want := filepath.Join(sysClassSCSIHost, "host2"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	if _, _, ok := NewSCSIHost(before, before); ok {
		t.Fatal("expected no new host when before == after")
	}
}
