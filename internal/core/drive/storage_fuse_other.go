//go:build !linux

package drive

// onFUSE reports whether dir is on a FUSE file system. Only Linux is
// checked; elsewhere it reports false.
func onFUSE(string) (bool, error) { return false, nil }
