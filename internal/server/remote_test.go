package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/backend"
	"github.com/stretchr/testify/require"
)

const testRemoteToken = "test-remote-token-0123456789"

// newRemoteTestHandler builds a Server around an empty backend and
// returns its remote handler.
func newRemoteTestHandler(t *testing.T, opts RemoteOptions) http.Handler {
	t.Helper()
	srv := &Server{}
	srv.backend = backend.New(context.Background(), nil, func() {})
	srv.installHandler()
	if opts.Token == "" {
		opts.Token = testRemoteToken
	}
	h, err := srv.RemoteHandler(opts)
	require.NoError(t, err)
	return h
}

func remoteRequest(t *testing.T, h http.Handler, method, path, token, remoteAddr string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRemoteAuth(t *testing.T) {
	t.Parallel()
	h := newRemoteTestHandler(t, RemoteOptions{})

	rec := remoteRequest(t, h, http.MethodGet, "/v1/workspaces", "", "", nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.NotEmpty(t, rec.Header().Get("WWW-Authenticate"))

	rec = remoteRequest(t, h, http.MethodGet, "/v1/workspaces", "wrong-token-xyz", "", nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	rec = remoteRequest(t, h, http.MethodGet, "/v1/workspaces", testRemoteToken, "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestRemoteInfo(t *testing.T) {
	t.Parallel()
	for _, allow := range []bool{false, true} {
		h := newRemoteTestHandler(t, RemoteOptions{
			DefaultPath:          "/some/project",
			AllowSkipPermissions: allow,
		})

		rec := remoteRequest(t, h, http.MethodGet, "/remote/api/info", "", "", nil)
		require.Equal(t, http.StatusUnauthorized, rec.Code)

		rec = remoteRequest(t, h, http.MethodGet, "/remote/api/info", testRemoteToken, "", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		var info RemoteInfo
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &info))
		require.Equal(t, "/some/project", info.DefaultPath)
		require.Equal(t, allow, info.AllowSkipPermissions)
	}
}

func TestRemoteAuthRateLimit(t *testing.T) {
	t.Parallel()
	h := newRemoteTestHandler(t, RemoteOptions{})
	const attacker = "203.0.113.7:5555"

	for range remoteAuthMaxFailures {
		rec := remoteRequest(t, h, http.MethodGet, "/v1/workspaces", "wrong-token-xyz", attacker, nil)
		require.Equal(t, http.StatusUnauthorized, rec.Code)
	}

	// Blocked even with the right token, and from another port.
	rec := remoteRequest(t, h, http.MethodGet, "/v1/workspaces", testRemoteToken, "203.0.113.7:6666", nil)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.NotEmpty(t, rec.Header().Get("Retry-After"))

	rec = remoteRequest(t, h, http.MethodGet, "/v1/workspaces", testRemoteToken, "198.51.100.9:5555", nil)
	require.Equal(t, http.StatusOK, rec.Code, "another address must not be blocked")
}

func TestRemoteGuardEndpoints(t *testing.T) {
	t.Parallel()

	deny := newRemoteTestHandler(t, RemoteOptions{})
	allow := newRemoteTestHandler(t, RemoteOptions{AllowSkipPermissions: true})

	rec := remoteRequest(t, deny, http.MethodPost, "/v1/control", testRemoteToken, "", strings.NewReader(`{}`))
	require.Equal(t, http.StatusForbidden, rec.Code)
	rec = remoteRequest(t, allow, http.MethodPost, "/v1/control", testRemoteToken, "", strings.NewReader(`{}`))
	require.Equal(t, http.StatusForbidden, rec.Code, "control is never allowed")

	const skip = "/v1/workspaces/does-not-exist/permissions/skip"
	rec = remoteRequest(t, deny, http.MethodPost, skip, testRemoteToken, "", strings.NewReader(`{"skip":true}`))
	require.Equal(t, http.StatusForbidden, rec.Code)
	rec = remoteRequest(t, allow, http.MethodPost, skip, testRemoteToken, "", strings.NewReader(`{"skip":true}`))
	require.NotEqual(t, http.StatusForbidden, rec.Code)

	rec = remoteRequest(t, deny, http.MethodPost, "/v1/workspaces", testRemoteToken, "", strings.NewReader(`{"yolo":true}`))
	require.Equal(t, http.StatusForbidden, rec.Code)

	// Writes the remote web app doesn't use are refused, even with
	// --allow-auto.
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v1/workspaces/w/config/set"},
		{http.MethodPost, "/v1/workspaces/w/agent/sessions/s/shell"},
		{http.MethodPost, "/v1/workspaces/w/mcp/start"},
		{http.MethodPost, "/v1/workspaces/w/control"},
		{http.MethodDelete, "/v1/workspaces/w"},
		{http.MethodPut, "/v1/workspaces/w/sessions/s"},
	} {
		rec = remoteRequest(t, allow, c.method, c.path, testRemoteToken, "", strings.NewReader(`{}`))
		require.Equal(t, http.StatusForbidden, rec.Code, "%s %s", c.method, c.path)
	}

	// The web app's own writes get past the guard.
	for _, p := range []string{
		"/v1/workspaces/w/agent",
		"/v1/workspaces/w/permissions/grant",
		"/v1/workspaces/w/questions/answer",
		"/v1/workspaces/w/sessions",
	} {
		rec = remoteRequest(t, deny, http.MethodPost, p, testRemoteToken, "", strings.NewReader(`{}`))
		require.NotEqual(t, http.StatusForbidden, rec.Code, p)
	}
}

func TestRemoteGuardPassesCreateBody(t *testing.T) {
	t.Parallel()

	body := `{"yolo":false,"path":"/tmp/project","client_id":"abc"}`
	var got []byte
	var gotLen int64
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		gotLen = r.ContentLength
		w.WriteHeader(http.StatusTeapot)
	})
	h := remoteGuard(RemoteOptions{}, next)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/workspaces", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusTeapot, rec.Code)
	require.Equal(t, body, string(got))
	require.Equal(t, int64(len(body)), gotLen)

	// A YOLO create is allowed through when the host permits it.
	got = nil
	h = remoteGuard(RemoteOptions{AllowSkipPermissions: true}, next)
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/workspaces", strings.NewReader(`{"yolo":true}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusTeapot, rec.Code)
	require.JSONEq(t, `{"yolo":true}`, string(got))
}

func TestRemoteStaticUI(t *testing.T) {
	t.Parallel()
	h := newRemoteTestHandler(t, RemoteOptions{})

	// Static files need no token.
	rec := remoteRequest(t, h, http.MethodGet, "/", "", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/html")
	require.NotEmpty(t, rec.Header().Get("Content-Security-Policy"))
	require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	require.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))

	rec = remoteRequest(t, h, http.MethodGet, "/app.js", "", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	rec = remoteRequest(t, h, http.MethodPost, "/", "", "", nil)
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)

	rec = remoteRequest(t, h, http.MethodGet, "/remote/whatever", testRemoteToken, "", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestRemoteHandlerRejectsShortToken(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	srv.backend = backend.New(context.Background(), nil, func() {})
	srv.installHandler()

	_, err := srv.RemoteHandler(RemoteOptions{Token: strings.Repeat("x", MinRemoteTokenLen-1)})
	require.Error(t, err)
	_, err = srv.RemoteHandler(RemoteOptions{})
	require.Error(t, err)
}

// TestRemoteUINoInlineScripts guards the CSP: script-src falls back to
// 'self', so an inline script in index.html would silently not run.
func TestRemoteUINoInlineScripts(t *testing.T) {
	t.Parallel()
	data, err := fs.ReadFile(remoteUI, "remoteui/index.html")
	require.NoError(t, err)

	tags := regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`).FindAllStringSubmatch(string(data), -1)
	require.NotEmpty(t, tags, "index.html should load at least one script")
	for _, m := range tags {
		require.Contains(t, m[1], "src=", "script tag without src: %s", m[0])
		require.Empty(t, strings.TrimSpace(m[2]), "inline script body: %s", m[0])
	}
}

func TestRemoteStartAutoAllowsSkip(t *testing.T) {
	t.Parallel()
	h := newRemoteTestHandler(t, RemoteOptions{StartAuto: true})

	rec := remoteRequest(t, h, http.MethodGet, "/remote/api/info", testRemoteToken, "", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var info RemoteInfo
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &info))
	require.True(t, info.StartAuto)
	require.True(t, info.AllowSkipPermissions, "--auto implies --allow-auto")

	const skip = "/v1/workspaces/does-not-exist/permissions/skip"
	rec = remoteRequest(t, h, http.MethodPost, skip, testRemoteToken, "", strings.NewReader(`{"skip":true}`))
	require.NotEqual(t, http.StatusForbidden, rec.Code)
	rec = remoteRequest(t, h, http.MethodPost, "/v1/workspaces", testRemoteToken, "", strings.NewReader(`{"yolo":true}`))
	require.NotEqual(t, http.StatusForbidden, rec.Code)
}
