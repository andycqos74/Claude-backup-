//go:build linux

package agent

import "golang.org/x/sys/unix"

// Kernel filesystem magic numbers for filesystems that expose kernel state
// rather than stored data. Backing these up is meaningless — their contents
// are synthesised per-read, many entries are unreadable even as root, and
// /proc alone contributes a file per open descriptor of every process on the
// host. A job pointed at a filesystem root (a Docker host's /host, say) would
// otherwise drown in permission errors before reaching any real data.
var pseudoFSMagic = map[int64]bool{
	0x9fa0:     true, // proc
	0x62656572: true, // sysfs
	0x1cd1:     true, // devpts
	0x27e0eb:   true, // cgroup
	0x63677270: true, // cgroup2
	0x64626720: true, // debugfs
	0x74726163: true, // tracefs
	0x73636673: true, // securityfs
	0xcafe4a11: true, // bpf
	0x19800202: true, // mqueue
	0x42494e4d: true, // binfmt_misc
	0x6165676c: true, // pstore
	0x6e736673: true, // nsfs
	0x9fa2:     true, // usbdevfs
	0x11307854: true, // mtd inode fs
	0x6c6f6f70: true, // loopfs
}

// isPseudoFS reports whether path is the mount point of a kernel
// pseudo-filesystem. Checked by filesystem type rather than by name so it
// works under any prefix — /proc and /host/proc alike — and never
// misidentifies an ordinary directory that happens to be called "proc".
func isPseudoFS(path string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false
	}
	return pseudoFSMagic[int64(st.Type)]
}
