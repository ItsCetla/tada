package tools

import (
	"path/filepath"
	"sync"
)

// fileLocks holds one mutex per resolved file path. Edit, multiedit and
// write take the lock around their read-check-modify-write section so
// parallel tool calls on the same file never interleave and lose a change.
// Entries are never removed; there is one small mutex per file written in
// the process lifetime.
var fileLocks sync.Map // map[string]*sync.Mutex

// fileLockKey resolves path to the key its lock is stored under: the real
// path with symlinks resolved when the file exists, otherwise the parent
// directory resolved plus the base name, falling back to the cleaned
// absolute path.
//
// Known limitation: EvalSymlinks keeps the spelling it was given, so on a
// case-insensitive file system (default APFS) Foo.go and foo.go map to
// different keys and do not exclude each other, whether or not the file
// exists. Callers that spell one file two ways are rare enough to accept.
func fileLockKey(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		return filepath.Join(dir, filepath.Base(abs))
	}
	return abs
}

// lockFile blocks until the caller holds the write lock for path and
// returns the function that releases it. The returned function is safe to
// call more than once, so callers can defer it and also release early.
func lockFile(path string) (unlock func()) {
	v, _ := fileLocks.LoadOrStore(fileLockKey(path), &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return sync.OnceFunc(mu.Unlock)
}
