//go:build darwin

package shell

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openedPath returns where the open file f really is, as the kernel sees
// it. The path is "" if that can't be found, which callers treat as not
// allowed. The second result is always true here: the check is possible.
func openedPath(f *os.File) (string, bool) {
	conn, err := f.SyscallConn()
	if err != nil {
		return "", true
	}
	buf := make([]byte, 1024) // MAXPATHLEN; F_GETPATH needs this size.
	var errno syscall.Errno
	if err := conn.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETPATH, uintptr(unsafe.Pointer(&buf[0])))
	}); err != nil || errno != 0 {
		return "", true
	}
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		buf = buf[:i]
	}
	return string(buf), true
}

// openAt opens the last part of rel inside the already open folder dir,
// refusing to follow a link there, so the file is in the folder that was
// checked.
func openAt(_ *os.Root, dir *os.File, rel string, flag int, perm os.FileMode) (*os.File, error) {
	conn, err := dir.SyscallConn()
	if err != nil {
		return nil, err
	}
	base := filepath.Base(rel)
	fd := -1
	var openErr error
	if err := conn.Control(func(dfd uintptr) {
		fd, openErr = unix.Openat(int(dfd), base, flag|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm.Perm()))
	}); err != nil {
		return nil, err
	}
	if openErr != nil {
		return nil, &os.PathError{Op: "open", Path: rel, Err: openErr}
	}
	return os.NewFile(uintptr(fd), filepath.Join(dir.Name(), base)), nil
}
