//go:build !darwin

package shell

import "os"

// openedPath reports false: finding an open file's path is only done on
// macOS, the one platform with a sandbox.
func openedPath(*os.File) (string, bool) { return "", false }

// openAt opens rel through r. Only macOS has a sandbox, so this fallback
// doesn't pin the parent folder.
func openAt(r *os.Root, _ *os.File, rel string, flag int, perm os.FileMode) (*os.File, error) {
	return r.OpenFile(rel, flag, perm)
}
