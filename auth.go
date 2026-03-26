package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookieName = "liiy_session"
	sessionTTL        = 30 * 24 * time.Hour
)

type sessionManager struct {
	mu       sync.Mutex
	ttl      time.Duration
	now      func() time.Time
	sessions map[string]time.Time
}

type loginPageData struct {
	AppName string
	Error   string
	Next    string
}

var loginTemplate = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.AppName}} - 登录</title>
  <style>
    :root {
      --bg: #eef2ea;
      --card: rgba(255,255,255,0.95);
      --text: #162019;
      --muted: #617165;
      --accent: #1d6f5a;
      --accent-soft: rgba(29,111,90,0.1);
      --warn: #a13e31;
      --line: rgba(22,32,25,0.1);
      --shadow: 0 24px 60px rgba(22,32,25,0.1);
    }

    * { box-sizing: border-box; }

    body {
      margin: 0;
      min-height: 100vh;
      display: grid;
      place-items: center;
      padding: 1rem;
      font-family: "Segoe UI Variable Text", "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", sans-serif;
      color: var(--text);
      background:
        radial-gradient(circle at top, rgba(29,111,90,0.12), transparent 25rem),
        linear-gradient(180deg, #f7f8f4 0%, var(--bg) 100%);
    }

    .card {
      width: min(28rem, 100%);
      padding: 1.5rem;
      border-radius: 1.6rem;
      background: var(--card);
      border: 1px solid var(--line);
      box-shadow: var(--shadow);
    }

    .eyebrow {
      display: inline-flex;
      padding: 0.35rem 0.72rem;
      border-radius: 999px;
      background: var(--accent-soft);
      color: var(--accent);
      font-size: 0.8rem;
      font-weight: 700;
      letter-spacing: 0.06em;
      text-transform: uppercase;
    }

    h1 {
      margin: 0.9rem 0 0.6rem;
      font-size: 2rem;
      line-height: 1.1;
    }

    p {
      margin: 0;
      color: var(--muted);
      line-height: 1.7;
    }

    form {
      display: grid;
      gap: 0.9rem;
      margin-top: 1.2rem;
    }

    label {
      display: grid;
      gap: 0.45rem;
      color: var(--muted);
      font-size: 0.94rem;
    }

    input {
      width: 100%;
      padding: 0.95rem 1rem;
      border-radius: 1rem;
      border: 1px solid rgba(22,32,25,0.14);
      background: #fff;
      color: var(--text);
      font: inherit;
    }

    button {
      border: 0;
      border-radius: 999px;
      padding: 0.95rem 1.2rem;
      background: var(--accent);
      color: #fff;
      font: inherit;
      font-weight: 700;
      cursor: pointer;
    }

    .error {
      margin-top: 1rem;
      padding: 0.9rem 1rem;
      border-radius: 1rem;
      background: rgba(161,62,49,0.08);
      color: var(--warn);
      border: 1px solid rgba(161,62,49,0.12);
    }
  </style>
</head>
<body>
  <main class="card">
    <span class="eyebrow">Login</span>
    <h1>{{.AppName}}</h1>
    <p>请输入用户名和密码后进入文件平台。</p>
    {{if .Error}}<div class="error">{{.Error}}</div>{{end}}
    <form method="post" action="/login">
      <input type="hidden" name="next" value="{{.Next}}">
      <label>
        用户名
        <input type="text" name="username" autocomplete="username" required>
      </label>
      <label>
        密码
        <input type="password" name="password" autocomplete="current-password" required>
      </label>
      <button type="submit">登录</button>
    </form>
  </main>
</body>
</html>`))

func newSessionManager(ttl time.Duration) *sessionManager {
	if ttl <= 0 {
		ttl = sessionTTL
	}
	return &sessionManager{
		ttl:      ttl,
		now:      time.Now,
		sessions: make(map[string]time.Time),
	}
}

func (m *sessionManager) Create() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.cleanupLocked()

	token := randomCredential(32)
	m.sessions[token] = m.now().UTC().Add(m.ttl)
	return token, nil
}

func (m *sessionManager) Valid(token string) bool {
	if token == "" {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.cleanupLocked()
	expiresAt, ok := m.sessions[token]
	if !ok {
		return false
	}
	return expiresAt.After(m.now().UTC())
}

func (m *sessionManager) Delete(token string) {
	if token == "" {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, token)
}

func (m *sessionManager) cleanupLocked() {
	now := m.now().UTC()
	for token, expiresAt := range m.sessions {
		if !expiresAt.After(now) {
			delete(m.sessions, token)
		}
	}
}

func (s *server) requireLogin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.auth.Enabled || s.isAuthenticated(r) {
			next(w, r)
			return
		}

		target := "/login?next=" + url.QueryEscape(r.URL.RequestURI())
		http.Redirect(w, r, target, http.StatusSeeOther)
	}
}

func (s *server) isAuthenticated(r *http.Request) bool {
	if !s.auth.Enabled {
		return true
	}

	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}

	return s.validateSessionToken(cookie.Value)
}

func (s *server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	writeSecurityHeaders(w)

	if !s.auth.Enabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if s.isAuthenticated(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	data := loginPageData{
		AppName: appName,
		Error:   strings.TrimSpace(r.URL.Query().Get("err")),
		Next:    sanitizeNext(r.URL.Query().Get("next")),
	}

	if err := loginTemplate.Execute(w, data); err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

func (s *server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	writeSecurityHeaders(w)

	if !s.auth.Enabled {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/login?err="+url.QueryEscape("无法解析登录表单"), http.StatusSeeOther)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")
	next := sanitizeNext(r.FormValue("next"))

	if !s.checkCredentials(username, password) {
		target := "/login?err=" + url.QueryEscape("用户名或密码错误")
		if next != "/" {
			target += "&next=" + url.QueryEscape(next)
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}

	token, err := s.newSessionToken()
	if err != nil {
		http.Error(w, "session create failed", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
		Expires:  time.Now().Add(sessionTTL),
	})

	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	writeSecurityHeaders(w)

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   isSecureRequest(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})

	if s.auth.Enabled {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) checkCredentials(username, password string) bool {
	userOK := subtle.ConstantTimeCompare([]byte(username), []byte(s.auth.Username)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(password), []byte(s.auth.Password)) == 1
	return userOK && passOK
}

func (s *server) newSessionToken() (string, error) {
	expiresAt := time.Now().UTC().Add(sessionTTL).Unix()
	payload := s.auth.Username + "|" + strconv.FormatInt(expiresAt, 10)
	payloadEncoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	signature := s.signSessionPayload(payload)
	return payloadEncoded + "." + signature, nil
}

func (s *server) validateSessionToken(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return false
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	payload := string(payloadBytes)
	expectedSig := s.signSessionPayload(payload)
	if subtle.ConstantTimeCompare([]byte(parts[1]), []byte(expectedSig)) != 1 {
		return false
	}

	payloadParts := strings.Split(payload, "|")
	if len(payloadParts) != 2 {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(payloadParts[0]), []byte(s.auth.Username)) != 1 {
		return false
	}

	expiresUnix, err := strconv.ParseInt(payloadParts[1], 10, 64)
	if err != nil {
		return false
	}
	return time.Unix(expiresUnix, 0).UTC().After(time.Now().UTC())
}

func (s *server) signSessionPayload(payload string) string {
	key := []byte(s.auth.Secret + ":" + s.auth.Password)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func sanitizeNext(next string) string {
	next = strings.TrimSpace(next)
	if next == "" {
		return "/"
	}
	parsed, err := url.Parse(next)
	if err != nil || parsed.IsAbs() || parsed.Host != "" {
		return "/"
	}
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/login") {
		return "/"
	}
	return next
}

func isSecureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}

	if strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		return true
	}

	for _, value := range r.Header.Values("Forwarded") {
		for _, part := range strings.Split(value, ",") {
			for _, param := range strings.Split(part, ";") {
				param = strings.TrimSpace(param)
				if !strings.HasPrefix(strings.ToLower(param), "proto=") {
					continue
				}
				proto := strings.Trim(strings.TrimSpace(param[len("proto="):]), `"`)
				if strings.EqualFold(proto, "https") {
					return true
				}
			}
		}
	}

	return false
}
