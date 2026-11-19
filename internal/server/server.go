package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/charmbracelet/crush/internal/backend"
	"github.com/charmbracelet/crush/internal/config"
)

// maxUnixSocketPathLen is the maximum length of a Unix domain socket
// path. The macOS sun_path field is 104 bytes; Linux allows 108. We
// use 104 so the resulting path is portable across both platforms.
const maxUnixSocketPathLen = 104

// socketDir returns the directory used for the Ta-Da Unix socket.
// It prefers $XDG_RUNTIME_DIR when set (systemd's per-user runtime
// directory on Linux). Otherwise it uses a private per-user directory
// (see [privateSocketDir]) inside [os.TempDir], which resolves to the
// per-user $TMPDIR on macOS but to the world-writable /tmp on Linux.
func socketDir(uid string) string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return dir
	}
	return privateSocketDir(os.TempDir(), uid)
}

// privateSocketDir returns the per-user directory holding the socket
// inside a shared temporary directory. The server creates it with mode
// 0700 and refuses to use it unless the current user owns it and nobody
// else can write to it (see [ensurePrivateSocketDir]), so other users can
// neither connect to the socket nor plant one of their own in its place.
func privateSocketDir(base, uid string) string {
	return filepath.Join(base, privateSocketDirName(uid))
}

func privateSocketDirName(uid string) string {
	if uid == "" {
		return "tada"
	}
	return "tada-" + uid
}

// currentUID returns the current user id as a string, or "" if unknown.
func currentUID() string {
	usr, err := user.Current()
	if err != nil {
		return ""
	}
	return usr.Uid
}

// ErrServerClosed is returned when the server is closed.
var ErrServerClosed = http.ErrServerClosed

// ParseHostURL parses a host URL into a [url.URL].
func ParseHostURL(host string) (*url.URL, error) {
	proto, addr, ok := strings.Cut(host, "://")
	if !ok {
		return nil, fmt.Errorf("invalid host format: %s", host)
	}

	var basePath string
	if proto == "tcp" {
		parsed, err := url.Parse("tcp://" + addr)
		if err != nil {
			return nil, fmt.Errorf("invalid tcp address: %v", err)
		}
		addr = parsed.Host
		basePath = parsed.Path
	}
	return &url.URL{
		Scheme: proto,
		Host:   addr,
		Path:   basePath,
	}, nil
}

// DefaultHost returns the default server host.
//
// On Windows the address is a named pipe under \\.\pipe\. On Unix
// platforms the socket lives in the per-user directory returned by
// [socketDir] and is named tada-<uid>.sock, falling back to
// tada.sock when the current uid cannot be determined. If the
// composed path would exceed [maxUnixSocketPathLen] bytes (the macOS
// sun_path limit), we fall back to /tmp/tada-<uid>/tada-<uid>.sock
// so the socket remains bindable.
func DefaultHost() string {
	uid := currentUID()
	sock := "tada.sock"
	if uid != "" {
		sock = fmt.Sprintf("tada-%s.sock", uid)
	}
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("npipe:////./pipe/%s", sock)
	}
	path := filepath.Join(socketDir(uid), sock)
	if len(path) > maxUnixSocketPathLen {
		path = filepath.Join(privateSocketDir("/tmp", uid), sock)
	}
	return "unix://" + path
}

// Server represents a Ta-Da server bound to a specific address.
type Server struct {
	// Addr can be a TCP address, a Unix socket path, or a Windows named pipe.
	Addr    string
	network string

	h  *http.Server
	ln net.Listener

	backend *backend.Backend
	logger  *slog.Logger

	// remotes are the remote control listeners' servers; see
	// [Server.ServeRemote].
	remoteMu sync.Mutex
	remotes  []*http.Server
}

// SetLogger sets the logger for the server.
func (s *Server) SetLogger(logger *slog.Logger) {
	s.logger = logger
}

// Backend returns the server's backend. Intended for integration tests
// that drive lifecycle transitions (detach, grace tuning) against a live
// HTTP surface.
func (s *Server) Backend() *backend.Backend {
	return s.backend
}

// DefaultServer returns a new [Server] with the default address.
func DefaultServer(cfg *config.ConfigStore) *Server {
	hostURL, err := ParseHostURL(DefaultHost())
	if err != nil {
		panic("invalid default host")
	}
	return NewServer(cfg, hostURL.Scheme, hostURL.Host)
}

// NewServer creates a new [Server] with the given network and address.
func NewServer(cfg *config.ConfigStore, network, address string) *Server {
	s := new(Server)
	s.Addr = address
	s.network = network

	// The backend is created with a shutdown callback that triggers
	// a graceful server shutdown (e.g. when the last workspace is
	// removed).
	s.backend = backend.New(context.Background(), cfg, func() {
		go func() {
			slog.Info("Shutting down server...")
			if err := s.Shutdown(context.Background()); err != nil {
				slog.Error("Failed to shutdown server", "error", err)
			}
		}()
	})
	s.installHandler()
	if network == "tcp" {
		s.h.Addr = address
	}
	return s
}

// installHandler builds the protocol/router around s.backend and
// assigns the resulting http.Server to s.h. Extracted from
// [NewServer] so test harnesses can wire a Server around a
// pre-constructed backend.
func (s *Server) installHandler() {
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	c := &controllerV1{backend: s.backend, server: s}
	mux := http.NewServeMux()
	for _, e := range c.endpoints() {
		mux.HandleFunc(e.Method()+" "+e.Path(), e.Handler())
	}
	mux.HandleFunc("GET /v1/docs/", c.handleDocsIndex)
	mux.HandleFunc("GET /v1/docs/openapi.json", c.handleDocsSpec)
	s.h = &http.Server{
		Protocols: &p,
		Handler:   s.recoverHandler(s.loggingHandler(mux)),
	}
}

// Handler returns the server's HTTP handler. Exposed so test harnesses
// can wrap it in an httptest.Server without going through the
// production listener setup.
func (s *Server) Handler() http.Handler {
	return s.h.Handler
}

// Serve accepts incoming connections on the listener.
func (s *Server) Serve(ln net.Listener) error {
	return s.h.Serve(ln)
}

// ListenAndServe starts the server and begins accepting connections.
func (s *Server) ListenAndServe() error {
	if s.ln != nil {
		return fmt.Errorf("server already started")
	}
	if s.network == "tcp" {
		slog.Warn("The Ta-Da server listens on TCP without authentication: any local process can connect, drive the agent and read the environment clients send", "address", s.Addr)
	}
	ln, err := s.Listen()
	if err != nil {
		return err
	}
	return s.Serve(ln)
}

// Listen binds the server's own address and returns the listener for
// [Server.Serve]. Splitting the two lets a caller know the address is
// accepting connections before it starts a client against it.
func (s *Server) Listen() (net.Listener, error) {
	ln, removedStale, err := listen(s.network, s.Addr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", s.Addr, err)
	}
	if removedStale && s.logger != nil {
		s.logger.Warn("Removed stale socket before binding", "address", s.Addr)
	}
	return ln, nil
}

func (s *Server) closeListener() {
	if s.ln != nil {
		s.ln.Close()
		s.ln = nil
	}
}

// Close force closes all listeners and connections.
func (s *Server) Close() error {
	defer func() { s.closeListener() }()
	s.closeRemotes()
	return s.h.Close()
}

// Shutdown gracefully shuts down the server without interrupting active
// connections.
func (s *Server) Shutdown(ctx context.Context) error {
	defer func() { s.closeListener() }()
	s.closeRemotes()
	return s.h.Shutdown(ctx)
}

func (s *Server) logDebug(r *http.Request, msg string, args ...any) {
	if s.logger != nil {
		s.logger.With(
			slog.String("method", r.Method),
			slog.String("url", r.URL.String()),
			slog.String("remote_addr", r.RemoteAddr),
		).Debug(msg, args...)
	}
}

func (s *Server) logError(r *http.Request, msg string, args ...any) {
	if s.logger != nil {
		s.logger.With(
			slog.String("method", r.Method),
			slog.String("url", r.URL.String()),
			slog.String("remote_addr", r.RemoteAddr),
		).Error(msg, args...)
	}
}
