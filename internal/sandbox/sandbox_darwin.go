//go:build darwin

package sandbox

import (
	"fmt"
	"os"
	"strings"
)

// sandboxExec is called by absolute path so a PATH entry can't stand in
// for it.
const sandboxExec = "/usr/bin/sandbox-exec"

// Available reports whether sandbox-exec is installed.
func Available() bool {
	info, err := os.Stat(sandboxExec)
	return err == nil && !info.IsDir()
}

// Wrap returns the program and arguments that run path with args confined
// by p. args[0] is the name the command was called by and is replaced by
// path. When p is not active it returns path and args unchanged.
func (p *Policy) Wrap(path string, args []string) (string, []string) {
	if !p.Active() {
		return path, args
	}
	profile, params := p.profile()
	out := make([]string, 0, 4+len(params)+len(args))
	out = append(out, sandboxExec, "-p", profile)
	out = append(out, params...)
	out = append(out, "--", path)
	if len(args) > 1 {
		out = append(out, args[1:]...)
	}
	return sandboxExec, out
}

// profile builds the SBPL profile for p and its -D parameters. Paths reach
// the profile only as (param "...") references, so no path text is ever
// parsed as SBPL.
func (p *Policy) profile() (string, []string) {
	var b strings.Builder
	var params []string
	param := func(prefix string, i int, path string) string {
		name := fmt.Sprintf("%s%d", prefix, i)
		params = append(params, "-D", name+"="+path)
		return fmt.Sprintf("(param %q)", name)
	}

	b.WriteString("(version 1)\n(allow default)\n(deny file-write*)\n")
	// Apple Events would let a command script other apps, which run
	// outside the sandbox.
	b.WriteString("(deny appleevent-send)\n")
	// Terminal and pseudo devices that ordinary commands write to.
	b.WriteString(`(allow file-write* (literal "/dev/null") (literal "/dev/zero") ` +
		`(literal "/dev/stdout") (literal "/dev/stderr") (literal "/dev/dtracehelper") ` +
		`(literal "/dev/ptmx") (regex #"^/dev/fd/") (regex #"^/dev/tty"))` + "\n")
	if len(p.WritableRoots) > 0 {
		b.WriteString("(allow file-write*")
		for i, r := range p.WritableRoots {
			b.WriteString(" (subpath " + param("ROOT", i, r) + ")")
		}
		b.WriteString(")\n")
	}
	// Later rules win, so these denies override the roots above.
	if len(p.ProtectedPaths) > 0 {
		b.WriteString("(deny file-write*")
		for i, r := range p.ProtectedPaths {
			b.WriteString(" (subpath " + param("PROT", i, r) + ")")
		}
		b.WriteString(")\n")
	}
	b.WriteString(`(deny file-write* (regex #"` + GitMetaPattern + `"))` + "\n")
	if len(p.ProtectedEntries) > 0 {
		b.WriteString("(deny file-write*")
		for i, r := range p.ProtectedEntries {
			b.WriteString(" (literal " + param("ENTRY", i, r) + ")")
		}
		b.WriteString(")\n")
	}
	if !p.Network {
		// Loopback stays open so local test servers work.
		// Unix sockets reach local daemons, such as Docker, that act
		// outside the sandbox.
		b.WriteString("(deny network-outbound (remote ip))\n" +
			"(deny network-outbound (remote unix-socket))\n" +
			"(deny network-inbound (local ip))\n" +
			"(deny network-bind (local ip))\n" +
			`(allow network-bind (local ip "localhost:*"))` + "\n" +
			`(allow network-inbound (local ip "localhost:*"))` + "\n" +
			`(allow network-outbound (remote ip "localhost:*"))` + "\n")
	}
	return b.String(), params
}
