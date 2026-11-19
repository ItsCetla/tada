package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/config"
	crushlog "github.com/charmbracelet/crush/internal/log"
	"github.com/charmbracelet/crush/internal/remote"
	"github.com/charmbracelet/crush/internal/server"
	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

// remoteAutoFlag is the value --remote takes when given without one.
const remoteAutoFlag = "auto"

// remoteTUI is set while the TUI hosts remote control in this process;
// setupWorkspace then attaches the TUI to that host as a client, so the
// terminal and the remote device share one workspace.
var remoteTUI bool

func init() {
	remoteCmd.Flags().StringSliceP("listen", "l", []string{remoteAutoFlag}, "Where to listen: auto, tailscale, lan, all, or an address (host or host:port); repeatable")
	remoteCmd.Flags().IntP("port", "p", remote.DefaultPort, "Port to listen on")
	remoteCmd.Flags().String("token", "", "Access token to use instead of the saved one")
	remoteCmd.Flags().Bool("allow-auto", false, "Let remote devices turn permission prompts off (Auto mode)")
	remoteCmd.Flags().Bool("auto", false, "Start remote sessions in Auto mode, with no permission prompts (implies --allow-auto)")
	remoteCmd.Flags().Bool("no-qr", false, "Do not print the QR code")
	remoteCmd.AddCommand(remoteURLCmd, remoteRotateTokenCmd)
	remoteURLCmd.Flags().Bool("no-qr", false, "Do not print the QR code")
	rootCmd.AddCommand(remoteCmd)

	rootCmd.Flags().String("remote", "", "Also control this session from another device: auto, tailscale, lan, all, or an address (use --remote=VALUE)")
	rootCmd.Flags().Lookup("remote").NoOptDefVal = remoteAutoFlag
}

var remoteCmd = &cobra.Command{
	Use:   "remote",
	Short: "Control Ta-Da from another device on your LAN or tailnet",
	Long: `Start remote control: a web app for your phone or another computer,
served from this machine over Tailscale or your local network.

Open the printed link (or scan the QR code) on the other device. The link
carries an access token; anyone who has it can drive the agent on this
machine, so treat it like a password.

Tailscale encrypts the connection. A plain LAN address does not: on a
network you do not trust, use Tailscale.

To share a session between this terminal and the other device, start the
TUI with "tada --remote" instead.`,
	Example: `
# Tailscale when available, otherwise the LAN
tada remote

# Only the local network, on another port
tada remote --listen lan --port 8080

# A specific address
tada remote --listen 192.168.1.20:7532

# Run the agent with no permission prompts
tada remote --auto

# Print the link of the running host again
tada remote url

# Replace the access token
tada remote rotate-token
  `,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		specs, _ := cmd.Flags().GetStringSlice("listen")
		noQR, _ := cmd.Flags().GetBool("no-qr")
		debug, _ := cmd.Flags().GetBool("debug")

		logFile := filepath.Join(config.GlobalCacheDir(), "server-remote", "tada.log")
		crushlog.Setup(logFile, debug)

		host, err := startRemoteHost(cmd, specs)
		if err != nil {
			return err
		}
		defer host.Close()

		printRemoteLinks(os.Stdout, host.endpoints, host.token, !noQR && term.IsTerminal(os.Stdout.Fd()))
		fmt.Fprintln(os.Stdout, "Press ctrl+c to stop.")

		sigch := make(chan os.Signal, 1)
		sigs := []os.Signal{os.Interrupt}
		sigs = append(sigs, addSignals(sigs)...)
		signal.Notify(sigch, sigs...)
		defer signal.Stop(sigch)

		select {
		case <-sigch:
			fmt.Fprintln(os.Stdout, "\nStopping remote control...")
			return nil
		case <-cmd.Context().Done():
			return nil
		case err := <-host.errch:
			return fmt.Errorf("remote control stopped: %v", err)
		}
	},
}

var remoteURLCmd = &cobra.Command{
	Use:   "url",
	Short: "Print the link of the running remote control host",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		noQR, _ := cmd.Flags().GetBool("no-qr")
		states := remote.ReadStates(remoteStateDir())
		if len(states) == 0 {
			return errors.New("remote control is not running; start it with `tada remote` or `tada --remote`")
		}
		token, err := remote.LoadOrCreateToken(remoteTokenPath())
		if err != nil {
			return err
		}
		for _, st := range states {
			if len(states) > 1 {
				fmt.Fprintf(os.Stdout, "%s (pid %d)\n", st.Path, st.PID)
			}
			linkToken := token
			if st.CustomToken {
				linkToken = ""
			}
			printRemoteLinks(os.Stdout, st.Endpoints, linkToken, !noQR && term.IsTerminal(os.Stdout.Fd()))
			if st.CustomToken {
				fmt.Fprintln(os.Stdout, "This host was started with --token; add #token=<your token> to the link.")
			}
		}
		return nil
	},
}

var remoteRotateTokenCmd = &cobra.Command{
	Use:   "rotate-token",
	Short: "Replace the saved access token",
	Long: `Replace the saved access token. Links that carry the old token stop
working the next time remote control starts; restart a running host to
cut them off now.`,
	Args: cobra.NoArgs,
	RunE: func(*cobra.Command, []string) error {
		if _, err := remote.RotateToken(remoteTokenPath()); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "New access token saved. Restart remote control to start using it.")
		return nil
	},
}

func remoteTokenPath() string {
	return filepath.Join(config.GlobalWorkspaceDir(), "remote-token")
}

func remoteStateDir() string {
	return filepath.Join(config.GlobalCacheDir(), "remote")
}

// remoteSocketHost returns the address of the private socket a remote
// control host serves local clients on. It sits beside the shared
// server's socket (same private directory) under a per-process name, so
// the two never collide.
func remoteSocketHost() string {
	name := fmt.Sprintf("tada-rc-%d", os.Getpid())
	if runtime.GOOS == "windows" {
		return "npipe:////./pipe/" + name
	}
	dir := filepath.Dir(strings.TrimPrefix(server.DefaultHost(), "unix://"))
	return "unix://" + filepath.Join(dir, name+".sock")
}

// sweepRemoteSockets removes the private sockets of hosts that are gone
// (crashed or killed before they could clean up).
func sweepRemoteSockets(socket string) {
	hostURL, err := server.ParseHostURL(socket)
	if err != nil || hostURL.Scheme != "unix" {
		return
	}
	stale, _ := filepath.Glob(filepath.Join(filepath.Dir(hostURL.Host), "tada-rc-*.sock"))
	for _, path := range stale {
		var pid int
		if _, err := fmt.Sscanf(filepath.Base(path), "tada-rc-%d.sock", &pid); err != nil {
			continue
		}
		if !remote.PIDAlive(pid) {
			_ = os.Remove(path)
		}
	}
}

// remoteHost is a running remote control host: a server in this process
// with a private socket for local clients and one authenticated network
// listener per address.
type remoteHost struct {
	srv       *server.Server
	socket    string
	endpoints []remote.Endpoint
	token     string
	stateFile string
	errch     chan error
}

// startRemoteHost starts remote control on the addresses the specs
// name. The flags --port, --token and --allow-auto are read from cmd
// when it defines them.
func startRemoteHost(cmd *cobra.Command, specs []string) (*remoteHost, error) {
	dataDir, _ := cmd.Flags().GetString("data-dir")
	debug, _ := cmd.Flags().GetBool("debug")
	port := remote.DefaultPort
	portFixed := false
	if f := cmd.Flags().Lookup("port"); f != nil {
		port, _ = cmd.Flags().GetInt("port")
		portFixed = f.Changed
	}
	var token string
	if cmd.Flags().Lookup("token") != nil {
		token, _ = cmd.Flags().GetString("token")
	}
	customToken := token != ""
	allowAuto := false
	if cmd.Flags().Lookup("allow-auto") != nil {
		allowAuto, _ = cmd.Flags().GetBool("allow-auto")
	}
	startAuto := false
	if cmd.Flags().Lookup("auto") != nil {
		startAuto, _ = cmd.Flags().GetBool("auto")
	}
	allowAuto = allowAuto || startAuto

	if customToken && len(token) < server.MinRemoteTokenLen {
		return nil, fmt.Errorf("--token must be at least %d characters", server.MinRemoteTokenLen)
	}
	if !customToken {
		var err error
		if token, err = remote.LoadOrCreateToken(remoteTokenPath()); err != nil {
			return nil, err
		}
	}

	cwd, err := ResolveCwd(cmd)
	if err != nil {
		return nil, err
	}
	endpoints, err := remote.Resolve(specs)
	if err != nil {
		return nil, err
	}
	listeners, endpoints, err := remote.Listen(endpoints, port, portFixed)
	if err != nil {
		return nil, err
	}
	closeListeners := func() {
		for _, ln := range listeners {
			ln.Close()
		}
	}
	nameTailscaleEndpoints(cmd.Context(), endpoints)

	cfg, err := config.Load(config.GlobalWorkspaceDir(), dataDir, debug)
	if err != nil {
		closeListeners()
		return nil, fmt.Errorf("failed to load configuration: %v", err)
	}
	socket := remoteSocketHost()
	sweepRemoteSockets(socket)
	hostURL, err := server.ParseHostURL(socket)
	if err != nil {
		closeListeners()
		return nil, err
	}
	srv := server.NewServer(cfg, hostURL.Scheme, hostURL.Host)
	srv.SetLogger(slog.Default())
	// The host lives as long as this process, not as long as it has
	// workspaces: a phone may connect minutes after it starts.
	srv.Backend().SetPersistent(true)

	local, err := srv.Listen()
	if err != nil {
		closeListeners()
		return nil, err
	}

	h := &remoteHost{
		srv:       srv,
		socket:    socket,
		endpoints: endpoints,
		token:     token,
		errch:     make(chan error, len(listeners)+1),
	}
	serve := func(fn func() error) {
		go func() {
			if err := fn(); err != nil && !errors.Is(err, server.ErrServerClosed) {
				slog.Error("Remote control listener stopped", "error", err)
				h.errch <- err
			}
		}()
	}
	serve(func() error { return srv.Serve(local) })
	opts := server.RemoteOptions{
		Token:                token,
		AllowSkipPermissions: allowAuto,
		StartAuto:            startAuto,
		DefaultPath:          cwd,
	}
	for _, ln := range listeners {
		serve(func() error { return srv.ServeRemote(ln, opts) })
	}

	h.stateFile, err = remote.WriteState(remoteStateDir(), remote.State{
		PID:         os.Getpid(),
		Path:        cwd,
		Endpoints:   endpoints,
		CustomToken: customToken,
		StartedAt:   time.Now(),
	})
	if err != nil {
		slog.Warn("Failed to record remote control host", "error", err)
	}
	for _, e := range endpoints {
		slog.Info("Remote control listening", "kind", e.Kind, "host", e.Host, "port", e.Port)
		if !e.Encrypted() {
			slog.Warn("Remote control is served over plain HTTP on this address; the access token can be read by others on the network", "host", e.Host)
		}
	}
	return h, nil
}

// Close stops the host: its workspaces first (so agents and their
// processes stop cleanly), then the listeners.
func (h *remoteHost) Close() {
	if h.stateFile != "" {
		_ = os.Remove(h.stateFile)
	}
	h.srv.Backend().ShutdownWorkspaces()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := h.srv.Shutdown(ctx); err != nil {
		_ = h.srv.Close()
	}
}

// nameTailscaleEndpoints gives Tailscale endpoints their MagicDNS name
// when that name resolves to the address here, so the link survives the
// address changing and reads better.
func nameTailscaleEndpoints(ctx context.Context, endpoints []remote.Endpoint) {
	if !slices.ContainsFunc(endpoints, func(e remote.Endpoint) bool { return e.Kind == remote.KindTailscale }) {
		return
	}
	name := remote.TailscaleName(ctx)
	if name == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, name)
	if err != nil {
		return
	}
	for i, e := range endpoints {
		if e.Kind == remote.KindTailscale && slices.Contains(addrs, e.Host) {
			endpoints[i].Name = name
		}
	}
}

// printRemoteLinks writes the link for each endpoint, a QR code for the
// first, and a warning when a link is not encrypted in transit.
func printRemoteLinks(w io.Writer, endpoints []remote.Endpoint, token string, qr bool) {
	if len(endpoints) == 0 {
		return
	}
	fmt.Fprintln(w, "Ta-Da remote control is on. Open this link on your other device:")
	fmt.Fprintln(w)
	plain := false
	for _, e := range endpoints {
		fmt.Fprintf(w, "  %-10s %s\n", e.Label(), e.URL(token))
		plain = plain || !e.Encrypted()
	}
	fmt.Fprintln(w)
	if qr {
		if code, err := remote.QR(endpoints[0].URL(token)); err == nil {
			fmt.Fprintln(w, code)
		}
	}
	fmt.Fprintln(w, "The link holds your access token. Anyone with it can drive the agent on this machine.")
	if plain {
		fmt.Fprintln(w, "LAN links use plain HTTP: others on the network could read the token. Prefer Tailscale on networks you do not trust.")
	}
}

// startRemoteForTUI starts remote control inside the TUI process and
// points the TUI at it. It shows the link before the TUI takes over the
// screen, and waits for enter so there is time to scan the QR code.
func startRemoteForTUI(cmd *cobra.Command) (func(), error) {
	spec, _ := cmd.Flags().GetString("remote")
	debug, _ := cmd.Flags().GetBool("debug")
	crushlog.Setup(filepath.Join(config.GlobalCacheDir(), "server-remote", "tada.log"), debug)

	host, err := startRemoteHost(cmd, strings.Split(spec, ","))
	if err != nil {
		return nil, fmt.Errorf("failed to start remote control: %w", err)
	}
	clientHost = host.socket
	remoteTUI = true

	if term.IsTerminal(os.Stdin.Fd()) && term.IsTerminal(os.Stderr.Fd()) {
		printRemoteLinks(os.Stderr, host.endpoints, host.token, true)
		fmt.Fprintln(os.Stderr, "Run `tada remote url` in another terminal to see this again.")
		fmt.Fprint(os.Stderr, "\nPress enter to open Ta-Da...")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
	return func() {
		remoteTUI = false
		host.Close()
	}, nil
}
