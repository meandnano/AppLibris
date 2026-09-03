package scanner

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// mountInfoPath is the kernel's per-process mount table. A variable so
// tests can point at a fixture; nothing else reassigns it.
var mountInfoPath = "/proc/self/mountinfo"

// mountFor reports the mount point backing path and its filesystem type.
// Returns an error when the mount table can't be read at all, which on a
// non-Linux development machine simply means the file isn't there.
func mountFor(path string) (mountPoint, fsType string, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	f, err := os.Open(mountInfoPath)
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	mountPoint, fsType = mountForReader(f, abs)
	return mountPoint, fsType, nil
}

// mountForReader picks the longest mount point that contains path — the
// most specific one, since mounts nest.
//
// Each mountinfo line is a fixed set of fields, then a variable number of
// optional ones, then a "-" separator, then the filesystem type:
//
//	36 35 98:0 /mnt1 /mnt2 rw,noatime master:1 - ext3 /dev/root rw
//
// so the mount point is always field 5 and the type always follows the
// separator, wherever that lands.
func mountForReader(r io.Reader, path string) (mountPoint, fsType string) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		sep := -1
		for i, field := range fields {
			if field == "-" {
				sep = i
				break
			}
		}
		// A separator needs the five fixed fields before it and a type
		// after it; anything else is a line we don't understand.
		if sep < 5 || sep+1 >= len(fields) {
			continue
		}

		candidate := unescapeMountField(fields[4])
		if !pathWithin(path, candidate) {
			continue
		}
		if len(candidate) >= len(mountPoint) {
			mountPoint, fsType = candidate, fields[sep+1]
		}
	}
	return mountPoint, fsType
}

// pathWithin reports whether path is mountPoint or sits beneath it.
func pathWithin(path, mountPoint string) bool {
	if mountPoint == "/" {
		return true
	}
	return path == mountPoint || strings.HasPrefix(path, mountPoint+"/")
}

// unescapeMountField undoes the octal escaping the kernel applies to the
// few characters that would otherwise break the field split.
func unescapeMountField(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	return strings.NewReplacer(
		`\040`, " ",
		`\011`, "\t",
		`\012`, "\n",
		`\134`, `\`,
	).Replace(field)
}

// mountHidesChanges reports whether fsType is one where the filesystem is a
// view over storage that something else can write to directly — so a change
// can land without the kernel ever generating an event on this mount.
//
// Unraid's user shares are the case that matters here: they are FUSE
// (fuse.shfs) over several disks, and the mover relocates files by working
// on those disks rather than through the share.
func mountHidesChanges(fsType string) bool {
	if strings.HasPrefix(fsType, "fuse") {
		return true
	}
	switch fsType {
	case "nfs", "nfs4", "cifs", "smb3", "9p":
		return true
	}
	return false
}
