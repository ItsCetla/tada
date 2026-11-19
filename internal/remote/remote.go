// Package remote holds the pieces of remote control that do not depend
// on the server: which addresses to listen on, the access token, the
// record of running hosts, and the QR code for the link.
package remote

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"rsc.io/qr"
)

// DefaultPort is the port remote control listens on unless told
// otherwise.
const DefaultPort = 7532

// portTries is how many ports after the default are tried when it is
// taken (a second host on the same machine).
const portTries = 20

// Endpoint kinds.
const (
	KindTailscale = "tailscale"
	KindLAN       = "lan"
	KindCustom    = "custom"
)

// Endpoint is one address remote control listens on.
type Endpoint struct {
	// Kind says which network the address is on.
	Kind string `json:"kind"`
	// Host is the address to bind.
	Host string `json:"host"`
	// Name is a friendlier host for the link (a Tailscale MagicDNS
	// name). Empty means use Host.
	Name string `json:"name,omitempty"`
	// Port is the port to bind; 0 means the caller's default.
	Port int `json:"port"`
}

// URL returns the link that opens the web app signed in.
func (e Endpoint) URL(token string) string {
	u := "http://" + net.JoinHostPort(e.displayHost(), strconv.Itoa(e.Port)) + "/"
	if token != "" {
		u += "#token=" + token
	}
	return u
}

func (e Endpoint) displayHost() string {
	if e.Name != "" {
		return e.Name
	}
	if e.Host == "0.0.0.0" || e.Host == "::" || e.Host == "" {
		if h, err := os.Hostname(); err == nil {
			return h
		}
	}
	return e.Host
}

// Label names the endpoint's network for display.
func (e Endpoint) Label() string {
	switch e.Kind {
	case KindTailscale:
		return "Tailscale"
	case KindLAN:
		return "LAN"
	default:
		return "Address"
	}
}

// Encrypted reports whether traffic to the endpoint is protected in
// transit without TLS: Tailscale encrypts it, and loopback never leaves
// the machine.
func (e Endpoint) Encrypted() bool {
	if e.Kind == KindTailscale {
		return true
	}
	ip, err := netip.ParseAddr(e.Host)
	return err == nil && ip.IsLoopback()
}

var tailscaleRange = netip.MustParsePrefix("100.64.0.0/10")

// interfaceAddrs lists the addresses of the interfaces that are up. It
// is a variable so tests can substitute a fixed set.
var interfaceAddrs = func() ([]netip.Addr, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ip, ok := netip.AddrFromSlice(ipnet.IP); ok {
				out = append(out, ip.Unmap())
			}
		}
	}
	return out, nil
}

// Resolve turns listen specs into endpoints. A spec is "auto"
// (Tailscale when this machine is on a tailnet, otherwise the LAN),
// "tailscale", "lan", "all" (both), or an address to bind: "host" or
// "host:port". Endpoints from a spec without a port have Port 0.
func Resolve(specs []string) ([]Endpoint, error) {
	if len(specs) == 0 {
		specs = []string{"auto"}
	}
	addrs, err := interfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("failed to list network interfaces: %w", err)
	}
	var ts, lan []Endpoint
	for _, ip := range addrs {
		switch {
		case !ip.Is4() || ip.IsLoopback() || ip.IsLinkLocalUnicast():
		case tailscaleRange.Contains(ip):
			ts = append(ts, Endpoint{Kind: KindTailscale, Host: ip.String()})
		case ip.IsPrivate():
			lan = append(lan, Endpoint{Kind: KindLAN, Host: ip.String()})
		}
	}

	var out []Endpoint
	add := func(eps ...Endpoint) {
		for _, e := range eps {
			if !slices.ContainsFunc(out, func(o Endpoint) bool { return o.Host == e.Host && o.Port == e.Port }) {
				out = append(out, e)
			}
		}
	}
	for _, spec := range specs {
		spec = strings.TrimSpace(spec)
		switch strings.ToLower(spec) {
		case "":
		case "auto":
			switch {
			case len(ts) > 0:
				add(ts...)
			case len(lan) > 0:
				add(lan...)
			default:
				return nil, errors.New("no Tailscale or LAN address found; pass an address to listen on, e.g. --listen 192.168.1.20")
			}
		case "tailscale", "ts":
			if len(ts) == 0 {
				return nil, errors.New("no Tailscale address found; is Tailscale running on this machine?")
			}
			add(ts...)
		case "lan":
			if len(lan) == 0 {
				return nil, errors.New("no LAN address found; pass an address to listen on, e.g. --listen 192.168.1.20")
			}
			add(lan...)
		case "all":
			if len(ts)+len(lan) == 0 {
				return nil, errors.New("no Tailscale or LAN address found; pass an address to listen on, e.g. --listen 192.168.1.20")
			}
			add(ts...)
			add(lan...)
		default:
			e, err := parseAddr(spec)
			if err != nil {
				return nil, err
			}
			add(e)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("nothing to listen on")
	}
	return out, nil
}

func parseAddr(spec string) (Endpoint, error) {
	host, port := spec, 0
	if h, p, err := net.SplitHostPort(spec); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return Endpoint{}, fmt.Errorf("invalid port in %q", spec)
		}
		host, port = h, n
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return Endpoint{}, fmt.Errorf("invalid listen address %q: want auto, tailscale, lan, all, or host[:port]", spec)
	}
	e := Endpoint{Kind: KindCustom, Host: host, Port: port}
	if ip, err := netip.ParseAddr(host); err == nil {
		switch {
		case tailscaleRange.Contains(ip):
			e.Kind = KindTailscale
		case ip.IsPrivate():
			e.Kind = KindLAN
		}
	}
	return e, nil
}

// Listen binds every endpoint. Endpoints without a port get port, and
// when that is taken and fixed is false the next free port that works
// for all of them. The returned endpoints carry the ports bound.
func Listen(endpoints []Endpoint, port int, fixed bool) ([]net.Listener, []Endpoint, error) {
	if port == 0 {
		port = DefaultPort
	}
	tries := portTries
	if fixed {
		tries = 0
	}
	var lastErr error
	for p := port; p <= port+tries && p <= 65535; p++ {
		lns, bound, err := listenAll(endpoints, p)
		if err == nil {
			return lns, bound, nil
		}
		lastErr = err
	}
	return nil, nil, lastErr
}

func listenAll(endpoints []Endpoint, port int) ([]net.Listener, []Endpoint, error) {
	var lns []net.Listener
	bound := make([]Endpoint, 0, len(endpoints))
	for _, e := range endpoints {
		if e.Port == 0 {
			e.Port = port
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(e.Host, strconv.Itoa(e.Port))) //nolint:noctx
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			return nil, nil, fmt.Errorf("failed to listen on %s: %w", net.JoinHostPort(e.Host, strconv.Itoa(e.Port)), err)
		}
		// An ephemeral port (":0") is only known once bound.
		if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
			e.Port = tcp.Port
		}
		lns = append(lns, ln)
		bound = append(bound, e)
	}
	return lns, bound, nil
}

// TailscaleName returns this machine's MagicDNS name, or "" when the
// tailscale command is missing or does not answer quickly.
func TailscaleName(ctx context.Context) string {
	bin, err := exec.LookPath("tailscale")
	if err != nil {
		if runtime.GOOS != "darwin" {
			return ""
		}
		bin = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
		if _, err := os.Stat(bin); err != nil {
			return ""
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "status", "--json", "--peers=false").Output()
	if err != nil {
		return ""
	}
	var status struct {
		Self struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return ""
	}
	return strings.TrimSuffix(status.Self.DNSName, ".")
}

// LoadOrCreateToken returns the access token stored at path, creating
// it on first use. The token is kept between runs so a link saved on
// another device goes on working.
func LoadOrCreateToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if token := strings.TrimSpace(string(data)); len(token) >= 32 {
			return token, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("failed to read remote token: %w", err)
	}
	return RotateToken(path)
}

// RotateToken writes a fresh token to path and returns it. Links that
// carry the old token stop working once running hosts restart.
func RotateToken(path string) (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("failed to create remote token directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("failed to write remote token: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("failed to write remote token: %w", err)
	}
	return token, nil
}

// State records a running remote control host so `tada remote url` can
// print its link again.
type State struct {
	PID       int        `json:"pid"`
	Path      string     `json:"path"`
	Endpoints []Endpoint `json:"endpoints"`
	// CustomToken is set when the host was started with its own token
	// rather than the saved one.
	CustomToken bool      `json:"custom_token,omitempty"`
	StartedAt   time.Time `json:"started_at"`
}

// WriteState records a running host in dir and returns the file to
// remove when it stops.
func WriteState(dir string, st State) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, strconv.Itoa(st.PID)+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// ReadStates returns the hosts recorded in dir that still answer, and
// removes the records of those that do not.
func ReadStates(dir string) []State {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []State
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var st State
		if err := json.Unmarshal(data, &st); err != nil || len(st.Endpoints) == 0 || !alive(st) {
			_ = os.Remove(path)
			continue
		}
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b State) int { return a.StartedAt.Compare(b.StartedAt) })
	return out
}

func alive(st State) bool {
	if !PIDAlive(st.PID) {
		return false
	}
	for _, e := range st.Endpoints {
		host := e.Host
		if host == "0.0.0.0" || host == "::" {
			host = "localhost"
		}
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(e.Port)), 300*time.Millisecond) //nolint:noctx
		if err == nil {
			conn.Close()
			return true
		}
	}
	return false
}

// QR renders text as a QR code for a terminal, two modules per line of
// text. It sets its own colors (black on white) so it scans on light
// and dark themes alike.
func QR(text string) (string, error) {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return "", err
	}
	const (
		quiet = 2
		on    = "\x1b[30;47m"
		off   = "\x1b[0m"
	)
	black := func(x, y int) bool {
		if x < 0 || y < 0 || x >= code.Size || y >= code.Size {
			return false
		}
		return code.Black(x, y)
	}
	var b strings.Builder
	for y := -quiet; y < code.Size+quiet; y += 2 {
		b.WriteString("  " + on)
		for x := -quiet; x < code.Size+quiet; x++ {
			top, bottom := black(x, y), black(x, y+1)
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString(off + "\n")
	}
	return b.String(), nil
}

// PIDAlive reports whether a process with the given id exists. A port
// answering is not enough: another host may have taken it since.
func PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		// FindProcess only succeeds there for a live process.
		_ = p.Release()
		return true
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
