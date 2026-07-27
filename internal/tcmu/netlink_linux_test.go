//go:build linux

package tcmu

import (
	"syscall"
	"testing"
)

// TestResolveGenlFamilyAgainstRealKernel is the one test in this package
// that talks to an actual kernel facility rather than a fake - "nlctrl"
// (the generic-netlink controller itself) is always present, at the fixed
// id genlIDCtrl, on any Linux kernel with CONFIG_NET enabled, and
// resolving it needs no root and no target_core_user module. It's a real,
// if narrow, end-to-end proof that this package's netlink framing
// (buildGetFamilyRequest/parseMessage/parseFamilyReply) round-trips
// against the real kernel, not just against hand-built fixtures like
// netlink_message_test.go's. Skips gracefully (not a failure) if this
// sandbox/CI environment doesn't permit creating a netlink socket at all.
func TestResolveGenlFamilyAgainstRealKernel(t *testing.T) {
	fd, err := openGenlSocket()
	if err != nil {
		t.Skipf("netlink socket unavailable in this environment: %v", err)
	}
	defer syscall.Close(fd)

	familyID, groups, err := resolveGenlFamily(fd, "nlctrl", 1)
	if err != nil {
		t.Fatalf("resolveGenlFamily(nlctrl): %v", err)
	}
	if familyID != genlIDCtrl {
		t.Errorf("familyID = %d, want %d (GENL_ID_CTRL)", familyID, genlIDCtrl)
	}
	t.Logf("nlctrl multicast groups: %v", groups)
}

// TestResolveGenlFamilyUnknownName confirms an unknown family name comes
// back as an error (the kernel replies with a netlink error message, ENOENT)
// rather than this package misparsing it as a successful-but-empty reply.
func TestResolveGenlFamilyUnknownName(t *testing.T) {
	fd, err := openGenlSocket()
	if err != nil {
		t.Skipf("netlink socket unavailable in this environment: %v", err)
	}
	defer syscall.Close(fd)

	if _, _, err := resolveGenlFamily(fd, "this-family-does-not-exist", 1); err == nil {
		t.Fatal("expected an error resolving a nonexistent generic-netlink family")
	}
}
