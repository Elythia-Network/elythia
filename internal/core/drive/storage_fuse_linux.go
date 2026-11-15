package drive

import (
	"fmt"
	"syscall"
)

// fuseSuperMagic is the f_type statfs reports for a FUSE file system
// (FUSE_SUPER_MAGIC in linux/magic.h).
const fuseSuperMagic = 0x65735546

// onFUSE reports whether dir is on a FUSE file system.
func onFUSE(dir string) (bool, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return false, fmt.Errorf("statfs %s: %w", dir, err)
	}
	return st.Type == fuseSuperMagic, nil
}
