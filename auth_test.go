package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnsureConfigInteractiveCustomCredentials(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	input := strings.NewReader("y\ny\nalice\nsecret123\n")
	output := &bytes.Buffer{}

	cfg, err := ensureConfig(path, input, output, true)
	if err != nil {
		t.Fatalf("ensureConfig() error = %v", err)
	}

	if !cfg.AuthEnabled {
		t.Fatalf("ensureConfig() auth should be enabled")
	}
	if cfg.Username != "alice" {
		t.Fatalf("ensureConfig() username = %q", cfg.Username)
	}
	if cfg.Password != "secret123" {
		t.Fatalf("ensureConfig() password = %q", cfg.Password)
	}

	loaded, err := readConfigFile(path)
	if err != nil {
		t.Fatalf("readConfigFile() error = %v", err)
	}
	if loaded.Username != "alice" || loaded.Password != "secret123" {
		t.Fatalf("loaded credentials mismatch: %+v", loaded)
	}
}

func TestEnsureConfigInteractiveRandomCredentials(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	input := strings.NewReader("n\nn\n")
	output := &bytes.Buffer{}

	cfg, err := ensureConfig(path, input, output, true)
	if err != nil {
		t.Fatalf("ensureConfig() error = %v", err)
	}

	if cfg.AuthEnabled {
		t.Fatalf("ensureConfig() auth should be disabled")
	}
	if len(cfg.Username) != 8 || len(cfg.Password) != 8 {
		t.Fatalf("generated credentials should be 8 chars, got %q / %q", cfg.Username, cfg.Password)
	}
	if !strings.Contains(output.String(), "用户名:") || !strings.Contains(output.String(), "密码:") {
		t.Fatalf("initialization output should print generated credentials")
	}
}

func TestRequireLoginRedirectsUnauthenticatedRequests(t *testing.T) {
	t.Parallel()

	mux := newAuthTestMux(t, authConfig{
		Enabled:  true,
		Username: "alice",
		Password: "secret123",
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)

	if resp.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusSeeOther)
	}
	if got := resp.Header().Get("Location"); got != "/login?next=%2F" {
		t.Fatalf("redirect location = %q", got)
	}
}

func TestLoginFlowAllowsAccessAfterAuthentication(t *testing.T) {
	t.Parallel()

	mux := newAuthTestMux(t, authConfig{
		Enabled:  true,
		Username: "alice",
		Password: "secret123",
	})

	form := url.Values{
		"username": {"alice"},
		"password": {"secret123"},
		"next":     {"/"},
	}

	loginReq := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginResp := httptest.NewRecorder()
	mux.ServeHTTP(loginResp, loginReq)

	if loginResp.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want %d", loginResp.Code, http.StatusSeeOther)
	}
	if got := loginResp.Header().Get("Location"); got != "/" {
		t.Fatalf("login redirect = %q", got)
	}

	cookies := loginResp.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("login should set session cookie")
	}
	if cookies[0].MaxAge != int(sessionTTL.Seconds()) {
		t.Fatalf("cookie MaxAge = %d, want %d", cookies[0].MaxAge, int(sessionTTL.Seconds()))
	}
	if cookies[0].Secure {
		t.Fatalf("cookie Secure should be false for plain HTTP requests")
	}

	indexReq := httptest.NewRequest(http.MethodGet, "/", nil)
	indexReq.AddCookie(cookies[0])
	indexResp := httptest.NewRecorder()
	mux.ServeHTTP(indexResp, indexReq)

	if indexResp.Code != http.StatusOK {
		t.Fatalf("index status = %d, want %d", indexResp.Code, http.StatusOK)
	}
	if !strings.Contains(indexResp.Body.String(), "已登录: alice") {
		t.Fatalf("index should show current user after login")
	}
	if !strings.Contains(indexResp.Body.String(), `action="/logout"`) {
		t.Fatalf("index should show logout action when auth is enabled")
	}
}

func TestLoginFlowSetsSecureCookieForForwardedHTTPS(t *testing.T) {
	t.Parallel()

	mux := newAuthTestMux(t, authConfig{
		Enabled:  true,
		Username: "alice",
		Password: "secret123",
	})

	form := url.Values{
		"username": {"alice"},
		"password": {"secret123"},
		"next":     {"/"},
	}

	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)

	cookies := resp.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("login should set session cookie")
	}
	if !cookies[0].Secure {
		t.Fatalf("cookie Secure should be true for forwarded HTTPS requests")
	}
}

func TestLogoutMirrorsSecureCookieForForwardedHTTPS(t *testing.T) {
	t.Parallel()

	mux := newAuthTestMux(t, authConfig{
		Enabled:  true,
		Username: "alice",
		Password: "secret123",
	})

	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)

	cookies := resp.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("logout should clear session cookie")
	}
	if !cookies[0].Secure {
		t.Fatalf("logout cookie should mirror Secure=true for forwarded HTTPS requests")
	}
	if cookies[0].MaxAge != -1 {
		t.Fatalf("logout cookie MaxAge = %d, want -1", cookies[0].MaxAge)
	}
}

func newAuthTestMux(t *testing.T, auth authConfig) *http.ServeMux {
	t.Helper()

	dir := t.TempDir()
	store, err := NewStore(dir, 24*time.Hour)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}

	srv := &server{
		store:          store,
		maxUploadBytes: 4 * 1024 * 1024,
		maxUploadMB:    4,
		auth:           auth,
		sessions:       newSessionManager(sessionTTL),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", srv.requireLogin(srv.handleIndex))
	mux.HandleFunc("GET /app.js", srv.requireLogin(handleAppJS))
	mux.HandleFunc("POST /upload", srv.requireLogin(srv.handleUpload))
	mux.HandleFunc("GET /raw/{id}", srv.requireLogin(srv.handleRaw))
	mux.HandleFunc("GET /download/{id}", srv.requireLogin(srv.handleDownload))
	mux.HandleFunc("GET /login", srv.handleLoginPage)
	mux.HandleFunc("POST /login", srv.handleLoginSubmit)
	mux.HandleFunc("POST /logout", srv.handleLogout)
	return mux
}
