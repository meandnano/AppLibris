package scanner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A real /proc/self/mountinfo, trimmed: a root, a nested mount, an Unraid
// user share, and a line carrying an optional field ("shared:1") before the
// separator, since the separator's position is what the parser has to find
// rather than assume.
const sampleMountInfo = `21 27 0:20 / /proc rw,nosuid - proc proc rw
25 27 0:22 / /sys rw,nosuid shared:7 - sysfs sysfs rw
27 1 8:2 / / rw,relatime - ext4 /dev/sda2 rw
41 27 0:41 / /mnt/disk1 rw,relatime - xfs /dev/sdb1 rw
46 27 0:38 / /mnt/user rw,nosuid,nodev,relatime - fuse.shfs shfs rw,user_id=0
52 27 0:52 / /mnt/backup\040copy rw,relatime - nfs4 nas:/vol rw
`

func TestMountForReaderPicksTheMostSpecificMount(t *testing.T) {
	for _, tc := range []struct {
		path      string
		wantMount string
		wantType  string
	}{
		{"/etc/hosts", "/", "ext4"},
		{"/mnt/disk1/books/a.epub", "/mnt/disk1", "xfs"},
		{"/mnt/user", "/mnt/user", "fuse.shfs"},
		{"/mnt/user/books/a.epub", "/mnt/user", "fuse.shfs"},
		// The nested mount wins over "/", which also contains the path.
		{"/proc/self/mountinfo", "/proc", "proc"},
		// An escaped space in the mount point is decoded before matching.
		{"/mnt/backup copy/books", "/mnt/backup copy", "nfs4"},
		// A path that only shares a prefix with the mount's name, not its
		// directory, belongs to the parent mount.
		{"/mnt/user-other/books", "/", "ext4"},
	} {
		mount, fsType := mountForReader(strings.NewReader(sampleMountInfo), tc.path)
		if mount != tc.wantMount || fsType != tc.wantType {
			t.Errorf("mountForReader(%q) = (%q, %q), want (%q, %q)",
				tc.path, mount, fsType, tc.wantMount, tc.wantType)
		}
	}
}

func TestMountForReaderIgnoresMalformedLines(t *testing.T) {
	malformed := "not a mountinfo line\n" +
		"1 2 3 / /only-four-fields\n" +
		"1 2 0:1 / /nosep rw,relatime ext4 /dev/sda1 rw\n" +
		"1 2 0:1 / /trailing rw,relatime -\n" +
		"27 1 8:2 / / rw,relatime - ext4 /dev/sda2 rw\n"

	mount, fsType := mountForReader(strings.NewReader(malformed), "/somewhere")
	if mount != "/" || fsType != "ext4" {
		t.Errorf("mountForReader = (%q, %q), want the one well-formed line (\"/\", \"ext4\")", mount, fsType)
	}
}

// A filesystem that presents storage something else can write to directly
// is one where an event can simply never arrive.
func TestMountHidesChanges(t *testing.T) {
	for _, fsType := range []string{"fuse", "fuse.shfs", "fuse.rawBridge", "fuseblk", "nfs", "nfs4", "cifs", "smb3", "9p"} {
		if !mountHidesChanges(fsType) {
			t.Errorf("mountHidesChanges(%q) = false, want true", fsType)
		}
	}
	for _, fsType := range []string{"ext4", "xfs", "btrfs", "zfs", "overlay", "tmpfs", "apfs"} {
		if mountHidesChanges(fsType) {
			t.Errorf("mountHidesChanges(%q) = true, want false", fsType)
		}
	}
}

// On a machine with no /proc/self/mountinfo — a macOS development box —
// this reports an error rather than guessing, and the caller keeps quiet
// about it.
func TestMountForReportsAMissingMountTable(t *testing.T) {
	original := mountInfoPath
	t.Cleanup(func() { mountInfoPath = original })
	mountInfoPath = filepath.Join(t.TempDir(), "no-such-mountinfo")

	if _, _, err := mountFor("/tmp"); err == nil {
		t.Error("mountFor with no mount table returned no error")
	}
}

func TestMountForReadsTheRealMountTable(t *testing.T) {
	if _, err := os.Stat(mountInfoPath); err != nil {
		t.Skipf("no %s on this platform", mountInfoPath)
	}
	mount, fsType, err := mountFor(t.TempDir())
	if err != nil {
		t.Fatalf("mountFor: %v", err)
	}
	if mount == "" || fsType == "" {
		t.Errorf("mountFor = (%q, %q), want a mount point and a type", mount, fsType)
	}
}
