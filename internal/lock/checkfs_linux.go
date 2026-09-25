//go:build linux

package lock

import "golang.org/x/sys/unix"

// refusedMagic is every statfs magic number CheckFS refuses: NFS, SMB/CIFS,
// FUSE (including virtiofs, which reports FUSE_SUPER_MAGIC) and 9p, and the
// cluster and network filesystems whose flock may be local to one client
// (Lustre, GPFS, CephFS, AFS, Coda, OCFS2, GFS2). A two-open self-test cannot
// catch those: it only proves exclusion within this machine.
var refusedMagic = map[int64]string{
	int64(unix.NFS_SUPER_MAGIC):  "NFS",
	int64(unix.SMB_SUPER_MAGIC):  "SMB",
	int64(unix.SMB2_SUPER_MAGIC): "SMB",
	int64(unix.CIFS_SUPER_MAGIC): "CIFS",
	int64(unix.FUSE_SUPER_MAGIC): "FUSE",
	int64(unix.V9FS_MAGIC):       "9p",
	int64(unix.AFS_SUPER_MAGIC):  "AFS",
	int64(unix.CODA_SUPER_MAGIC): "Coda",
	// Lustre and GPFS are out-of-tree, with no kernel header to check these
	// against; a wrong value only leaves that filesystem to the self-test.
	0x0bd00bd0: "Lustre",
	0x47504653: "GPFS",
	0x00c36400: "CephFS",
	0x7461636f: "OCFS2",
	0x01161970: "GFS2",
}

// unsupportedFSType reports dir's filesystem type and whether CheckFS
// refuses it, by looking up its statfs magic number in refusedMagic.
func unsupportedFSType(dir string) (string, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return "", false // let the self-test, or the caller's own use, surface a real error
	}
	if name, refused := refusedMagic[st.Type]; refused {
		return name, true
	}
	return "", false
}
