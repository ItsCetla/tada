package remote

import (
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// setAddrs substitutes the interface address list for one test. Tests
// that call it must not run in parallel since it swaps a package var.
func setAddrs(t *testing.T, addrs ...string) {
	t.Helper()
	orig := interfaceAddrs
	t.Cleanup(func() { interfaceAddrs = orig })
	parsed := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		parsed = append(parsed, netip.MustParseAddr(a))
	}
	interfaceAddrs = func() ([]netip.Addr, error) { return parsed, nil }
}

func hosts(eps []Endpoint) []string {
	out := make([]string, 0, len(eps))
	for _, e := range eps {
		out = append(out, e.Host)
	}
	return out
}

func TestResolveAutoPrefersTailscale(t *testing.T) {
	setAddrs(t, "192.168.1.20", "100.101.102.103")
	eps, err := Resolve([]string{"auto"})
	require.NoError(t, err)
	require.Equal(t, []Endpoint{{Kind: KindTailscale, Host: "100.101.102.103"}}, eps)
}

func TestResolveAutoFallsBackToLAN(t *testing.T) {
	setAddrs(t, "192.168.1.20")
	eps, err := Resolve([]string{"auto"})
	require.NoError(t, err)
	require.Equal(t, []Endpoint{{Kind: KindLAN, Host: "192.168.1.20"}}, eps)
}

func TestResolveEmptySpecsIsAuto(t *testing.T) {
	setAddrs(t, "192.168.1.20", "100.101.102.103")
	auto, err := Resolve([]string{"auto"})
	require.NoError(t, err)
	empty, err := Resolve(nil)
	require.NoError(t, err)
	require.Equal(t, auto, empty)
}

func TestResolveAutoNoAddress(t *testing.T) {
	setAddrs(t, "8.8.8.8")
	_, err := Resolve([]string{"auto"})
	require.Error(t, err)
}

func TestResolveTailscaleMissing(t *testing.T) {
	setAddrs(t, "192.168.1.20")
	_, err := Resolve([]string{"tailscale"})
	require.Error(t, err)
}

func TestResolveAll(t *testing.T) {
	setAddrs(t, "192.168.1.20", "100.101.102.103", "10.0.0.4")
	eps, err := Resolve([]string{"all"})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"100.101.102.103", "192.168.1.20", "10.0.0.4"}, hosts(eps))
}

func TestResolveSkipsLoopbackLinkLocalAndIPv6(t *testing.T) {
	setAddrs(t, "127.0.0.1", "169.254.1.1", "fd7a:115c:a1e0::1", "fe80::1", "::1", "192.168.1.20")
	eps, err := Resolve([]string{"all"})
	require.NoError(t, err)
	require.Equal(t, []string{"192.168.1.20"}, hosts(eps))

	setAddrs(t, "127.0.0.1", "169.254.1.1", "fd7a:115c:a1e0::1")
	_, err = Resolve([]string{"auto"})
	require.Error(t, err)
}

func TestResolveAddresses(t *testing.T) {
	setAddrs(t)

	eps, err := Resolve([]string{"192.168.1.5:8080"})
	require.NoError(t, err)
	require.Equal(t, []Endpoint{{Kind: KindLAN, Host: "192.168.1.5", Port: 8080}}, eps)

	eps, err = Resolve([]string{"100.100.1.1"})
	require.NoError(t, err)
	require.Equal(t, []Endpoint{{Kind: KindTailscale, Host: "100.100.1.1"}}, eps)

	eps, err = Resolve([]string{"0.0.0.0:9000"})
	require.NoError(t, err)
	require.Equal(t, []Endpoint{{Kind: KindCustom, Host: "0.0.0.0", Port: 9000}}, eps)

	for _, bad := range []string{"192.168.1.5:http", "192.168.1.5:0", "192.168.1.5:70000", ":8080"} {
		_, err = Resolve([]string{bad})
		require.Error(t, err, bad)
	}
}

func TestResolveDedup(t *testing.T) {
	setAddrs(t, "192.168.1.20", "100.101.102.103")
	eps, err := Resolve([]string{"auto", "tailscale", "all", "192.168.1.20", "192.168.1.20:9000"})
	require.NoError(t, err)
	require.Equal(t, []Endpoint{
		{Kind: KindTailscale, Host: "100.101.102.103"},
		{Kind: KindLAN, Host: "192.168.1.20"},
		{Kind: KindLAN, Host: "192.168.1.20", Port: 9000},
	}, eps)
}

func freePort(t *testing.T) (net.Listener, int) {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	return ln, ln.Addr().(*net.TCPAddr).Port
}

func TestListenMovesPastTakenPort(t *testing.T) {
	t.Parallel()
	_, taken := freePort(t)

	lns, bound, err := Listen([]Endpoint{{Host: "127.0.0.1"}}, taken, false)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, l := range lns {
			l.Close()
		}
	})
	require.Len(t, lns, 1)
	require.Len(t, bound, 1)
	require.Greater(t, bound[0].Port, taken)
	require.Equal(t, bound[0].Port, lns[0].Addr().(*net.TCPAddr).Port)
}

func TestListenFixedPortTaken(t *testing.T) {
	t.Parallel()
	_, taken := freePort(t)

	lns, _, err := Listen([]Endpoint{{Host: "127.0.0.1"}}, taken, true)
	require.Error(t, err)
	require.Empty(t, lns)
}

func TestEndpointURL(t *testing.T) {
	t.Parallel()

	e := Endpoint{Kind: KindTailscale, Host: "100.101.102.103", Name: "box.tail1234.ts.net", Port: 7532}
	u := e.URL("secret-token")
	require.Equal(t, "http://box.tail1234.ts.net:7532/#token=secret-token", u)
	require.NotContains(t, u, "?")

	e = Endpoint{Kind: KindLAN, Host: "192.168.1.20", Port: 7532}
	require.Equal(t, "http://192.168.1.20:7532/", e.URL(""))
	require.Equal(t, "http://192.168.1.20:7532/#token=abc", e.URL("abc"))
}

func TestEndpointEncrypted(t *testing.T) {
	t.Parallel()
	require.True(t, Endpoint{Kind: KindTailscale, Host: "100.101.102.103"}.Encrypted())
	require.True(t, Endpoint{Kind: KindCustom, Host: "127.0.0.1"}.Encrypted())
	require.False(t, Endpoint{Kind: KindLAN, Host: "192.168.1.20"}.Encrypted())
	require.False(t, Endpoint{Kind: KindCustom, Host: "0.0.0.0"}.Encrypted())
}

func TestToken(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sub", "remote-token")

	tok, err := LoadOrCreateToken(path)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(tok), 32)
	if runtime.GOOS != "windows" {
		st, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	}

	again, err := LoadOrCreateToken(path)
	require.NoError(t, err)
	require.Equal(t, tok, again)

	rotated, err := RotateToken(path)
	require.NoError(t, err)
	require.NotEqual(t, tok, rotated)
	loaded, err := LoadOrCreateToken(path)
	require.NoError(t, err)
	require.Equal(t, rotated, loaded)

	require.NoError(t, os.WriteFile(path, []byte("short\n"), 0o600))
	replaced, err := LoadOrCreateToken(path)
	require.NoError(t, err)
	require.NotEqual(t, "short", replaced)
	require.GreaterOrEqual(t, len(replaced), 32)
}

func TestStates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ln, port := freePort(t)

	live := State{
		PID:       os.Getpid(),
		Path:      "/tmp/project",
		Endpoints: []Endpoint{{Kind: KindCustom, Host: "127.0.0.1", Port: port}},
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}
	livePath, err := WriteState(dir, live)
	require.NoError(t, err)

	got := ReadStates(dir)
	require.Len(t, got, 1)
	require.Equal(t, live.PID, got[0].PID)
	require.Equal(t, live.Endpoints, got[0].Endpoints)
	require.True(t, live.StartedAt.Equal(got[0].StartedAt))

	// Once the port closes the record is dropped and its file removed.
	require.NoError(t, ln.Close())
	require.Empty(t, ReadStates(dir))
	require.NoFileExists(t, livePath)

	// A dead PID is dropped even when the port answers.
	_, port2 := freePort(t)
	deadPath, err := WriteState(dir, State{
		PID:       1 << 30,
		Endpoints: []Endpoint{{Kind: KindCustom, Host: "127.0.0.1", Port: port2}},
	})
	require.NoError(t, err)
	require.Empty(t, ReadStates(dir))
	require.NoFileExists(t, deadPath)
}

func TestQR(t *testing.T) {
	t.Parallel()
	out, err := QR("http://192.168.1.20:7532/#token=abcdefghijklmnopqrstuvwxyz012345")
	require.NoError(t, err)
	require.NotEmpty(t, out)
	require.Greater(t, strings.Count(out, "\n"), 10)
}
