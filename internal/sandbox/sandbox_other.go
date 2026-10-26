//go:build !darwin

package sandbox

// Available reports whether this platform has a sandbox. Only macOS does.
func Available() bool { return false }

// Wrap returns path and args unchanged; there is no sandbox here.
func (p *Policy) Wrap(path string, args []string) (string, []string) {
	return path, args
}
