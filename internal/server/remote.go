package server

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/crush/internal/projects"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/version"
)

//go:embed remoteui
var remoteUI embed.FS

const (
	// MinRemoteTokenLen is the shortest token remote control accepts.
	MinRemoteTokenLen = 8

	// remoteAuthMaxFailures is how many wrong tokens one address may send
	// inside remoteAuthWindow before it is turned away without a check.
	remoteAuthMaxFailures = 10
	remoteAuthWindow      = time.Minute

	// remoteMaxCreateBody caps the workspace-create body the remote guard
	// buffers to inspect.
	remoteMaxCreateBody = 1 << 20
)

// RemoteOptions configures the remote control surface: the API and the
// web app served to other devices over a network listener.
type RemoteOptions struct {
	// Token is the bearer token every API request must carry.
	Token string
	// AllowSkipPermissions lets remote clients turn permission prompts
	// off (Auto mode). Off by default: a remote device may answer
	// prompts, not remove them.
	AllowSkipPermissions bool
	// StartAuto opens every workspace the web app joins in Auto mode.
	// It implies AllowSkipPermissions.
	StartAuto bool
	// DefaultPath is the project directory the web app opens first.
	DefaultPath string
}

// RemoteInfo is what the web app needs to get started.
type RemoteInfo struct {
	Version              string          `json:"version"`
	Hostname             string          `json:"hostname"`
	DefaultPath          string          `json:"default_path"`
	Projects             []RemoteProject `json:"projects"`
	AllowSkipPermissions bool            `json:"allow_skip_permissions"`
	StartAuto            bool            `json:"start_auto"`
}

// RemoteProject is a project directory the host knows about.
type RemoteProject struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// RemoteHandler returns the handler for a network listener: the web app
// at the root, and the API behind the bearer token with the calls a
// remote device must not make turned away.
func (s *Server) RemoteHandler(opts RemoteOptions) (http.Handler, error) {
	if len(opts.Token) < MinRemoteTokenLen {
		return nil, fmt.Errorf("remote token must be at least %d characters", MinRemoteTokenLen)
	}
	if opts.StartAuto {
		opts.AllowSkipPermissions = true
	}
	static, err := fs.Sub(remoteUI, "remoteui")
	if err != nil {
		return nil, err
	}
	auth := &remoteAuth{
		want:     sha256.Sum256([]byte(opts.Token)),
		failures: make(map[string]*authFailures),
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/", auth.require(remoteGuard(opts, s.h.Handler)))
	// Patterns carry no method: a method-specific "GET /" would conflict
	// with "/v1/", which has to match every method.
	mux.Handle("/remote/api/info", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		jsonEncode(w, remoteInfo(opts))
	})))
	mux.Handle("/remote/", http.NotFoundHandler())
	files := http.FileServerFS(static)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	return mux, nil
}

// ServeRemote serves the remote control surface on ln until the server
// shuts down. It may be called for several listeners (one per address).
func (s *Server) ServeRemote(ln net.Listener, opts RemoteOptions) error {
	h, err := s.RemoteHandler(opts)
	if err != nil {
		ln.Close()
		return err
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.remoteMu.Lock()
	s.remotes = append(s.remotes, srv)
	s.remoteMu.Unlock()
	return srv.Serve(ln)
}

// closeRemotes closes every remote listener and its connections. Event
// streams never finish on their own, so there is no graceful variant:
// waiting for them would only run out the caller's deadline.
func (s *Server) closeRemotes() {
	s.remoteMu.Lock()
	remotes := s.remotes
	s.remotes = nil
	s.remoteMu.Unlock()
	for _, srv := range remotes {
		_ = srv.Close()
	}
}

func remoteInfo(opts RemoteOptions) RemoteInfo {
	host, _ := os.Hostname()
	info := RemoteInfo{
		Version:              version.Version,
		Hostname:             host,
		DefaultPath:          opts.DefaultPath,
		Projects:             []RemoteProject{},
		AllowSkipPermissions: opts.AllowSkipPermissions,
		StartAuto:            opts.StartAuto,
	}
	list, err := projects.List()
	if err != nil {
		return info
	}
	for _, p := range list {
		if p.Path == "" {
			continue
		}
		if st, err := os.Stat(p.Path); err != nil || !st.IsDir() {
			continue
		}
		info.Projects = append(info.Projects, RemoteProject{Path: p.Path, Name: filepath.Base(p.Path)})
	}
	return info
}

// remoteWriteRoutes are the only state-changing calls a remote device may
// make: the ones the remote web app uses. Everything else that writes,
// such as config, MCP, shell or server control, is refused.
var remoteWriteRoutes = func() *http.ServeMux {
	m := http.NewServeMux()
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for _, p := range []string{
		"POST /v1/workspaces",
		"POST /v1/workspaces/{id}/agent",
		"POST /v1/workspaces/{id}/agent/sessions/{sid}/cancel",
		"POST /v1/workspaces/{id}/current-session",
		"POST /v1/workspaces/{id}/permissions/grant",
		"POST /v1/workspaces/{id}/permissions/skip",
		"POST /v1/workspaces/{id}/questions/answer",
		"POST /v1/workspaces/{id}/questions/cancel",
		"POST /v1/workspaces/{id}/sessions",
	} {
		m.Handle(p, ok)
	}
	return m
}()

// remoteGuard lets reads through and allows only the writes in
// [remoteWriteRoutes]. Switching permission prompts off is refused unless
// the host allowed it.
func remoteGuard(opts RemoteOptions, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		if _, pattern := remoteWriteRoutes.Handler(r); pattern == "" {
			jsonError(w, http.StatusForbidden, "this action is not available over remote control")
			return
		}
		path := strings.TrimSuffix(r.URL.Path, "/")
		switch {
		case strings.HasSuffix(path, "/permissions/skip"):
			if !opts.AllowSkipPermissions {
				jsonError(w, http.StatusForbidden, "turning permission prompts off is not allowed over remote control; start the host with --allow-auto to permit it")
				return
			}
		case path == "/v1/workspaces":
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, remoteMaxCreateBody))
			if err != nil {
				jsonError(w, http.StatusBadRequest, "failed to read request")
				return
			}
			var args proto.Workspace
			if err := json.Unmarshal(body, &args); err != nil {
				jsonError(w, http.StatusBadRequest, "failed to decode request")
				return
			}
			if args.YOLO && !opts.AllowSkipPermissions {
				jsonError(w, http.StatusForbidden, "opening a workspace in Auto mode is not allowed over remote control; start the host with --allow-auto to permit it")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		next.ServeHTTP(w, r)
	})
}

// remoteAuth checks the bearer token and slows down guessing.
type remoteAuth struct {
	want [sha256.Size]byte

	mu       sync.Mutex
	failures map[string]*authFailures
}

type authFailures struct {
	count int
	reset time.Time
}

func (a *remoteAuth) require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		if a.blocked(ip) {
			w.Header().Set("Retry-After", "60")
			jsonError(w, http.StatusTooManyRequests, "too many failed attempts; try again in a minute")
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		got := sha256.Sum256([]byte(strings.TrimSpace(token)))
		if !ok || subtle.ConstantTimeCompare(got[:], a.want[:]) != 1 {
			a.fail(ip)
			w.Header().Set("WWW-Authenticate", `Bearer realm="tada-remote"`)
			jsonError(w, http.StatusUnauthorized, "missing or wrong remote control token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *remoteAuth) blocked(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, ok := a.failures[ip]
	if !ok {
		return false
	}
	if time.Now().After(f.reset) {
		delete(a.failures, ip)
		return false
	}
	return f.count >= remoteAuthMaxFailures
}

func (a *remoteAuth) fail(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if len(a.failures) > 1024 {
		for k, f := range a.failures {
			if now.After(f.reset) {
				delete(a.failures, k)
			}
		}
	}
	f, ok := a.failures[ip]
	if !ok || now.After(f.reset) {
		f = &authFailures{reset: now.Add(remoteAuthWindow)}
		a.failures[ip] = f
	}
	f.count++
}
