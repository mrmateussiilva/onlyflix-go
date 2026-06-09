package handlers

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	adminSessions = make(map[string]time.Time)
	sessionMutex  sync.Mutex
)

func constantTimeEqual(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func wantsLoginRedirect(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/json") || strings.Contains(accept, "application/vnd.apple.mpegurl") {
		return false
	}
	return true
}

func hasValidAdminSession(r *http.Request) bool {
	cookie, err := r.Cookie("onlyflix_admin")
	if err != nil || cookie.Value == "" {
		return false
	}

	sessionMutex.Lock()
	defer sessionMutex.Unlock()

	expires, ok := adminSessions[cookie.Value]
	if !ok {
		return false
	}
	if time.Now().After(expires) {
		delete(adminSessions, cookie.Value)
		return false
	}
	return true
}

func createAdminSession(w http.ResponseWriter, r *http.Request) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := time.Now().Add(24 * time.Hour)

	sessionMutex.Lock()
	adminSessions[token] = expires
	sessionMutex.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     "onlyflix_admin",
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
	return nil
}

func clearAdminSession(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("onlyflix_admin"); err == nil {
		sessionMutex.Lock()
		delete(adminSessions, cookie.Value)
		sessionMutex.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "onlyflix_admin",
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
}

func Secure(h http.HandlerFunc, authUser, authPass string) http.HandlerFunc {
	if authUser == "" || authPass == "" {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if hasValidAdminSession(r) {
			h(w, r)
			return
		}
		u, p, ok := r.BasicAuth()
		if ok && constantTimeEqual(u, authUser) && constantTimeEqual(p, authPass) {
			h(w, r)
			return
		}

		if wantsLoginRedirect(r) {
			next := url.QueryEscape(r.URL.RequestURI())
			http.Redirect(w, r, "/login?next="+next, http.StatusSeeOther)
			return
		}

		w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	}
}

func HandleLogin(authUser, authPass string) http.HandlerFunc {
	const loginHTML = `<!DOCTYPE html>
<html lang="pt-br">
<head>
	<meta charset="UTF-8">
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<title>OnlyFlix - Login Admin</title>
	<style>
		:root { --bg:#09090b; --surface:#18181b; --border:#27272a; --text:#f4f4f5; --muted:#a1a1aa; --accent:#e11d48; --danger:#ef4444; }
		* { box-sizing: border-box; }
		body { margin:0; min-height:100vh; display:grid; place-items:center; background:var(--bg); color:var(--text); font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
		.login { width:min(420px, calc(100vw - 32px)); background:var(--surface); border:1px solid var(--border); border-radius:12px; padding:28px; box-shadow:0 24px 60px rgba(0,0,0,.45); }
		h1 { margin:0 0 6px; color:var(--accent); font-size:28px; }
		p { margin:0 0 24px; color:var(--muted); }
		label { display:block; margin:16px 0 8px; color:var(--muted); font-size:14px; }
		input { width:100%; border:1px solid var(--border); border-radius:8px; background:#09090b; color:var(--text); padding:12px 14px; font-size:16px; outline:none; }
		input:focus { border-color:var(--accent); }
		button { width:100%; margin-top:22px; border:0; border-radius:8px; background:var(--accent); color:white; padding:12px 14px; font-size:15px; font-weight:700; cursor:pointer; }
		.error { margin-top:16px; color:var(--danger); font-size:14px; }
	</style>
</head>
<body>
	<form class="login" method="post" action="/login">
		<h1>OnlyFlix</h1>
		<p>Painel administrativo</p>
		<input type="hidden" name="next" value="{{.Next}}">
		<label for="username">Usuário</label>
		<input id="username" name="username" autocomplete="username" autofocus>
		<label for="password">Senha</label>
		<input id="password" name="password" type="password" autocomplete="current-password">
		<button type="submit">Entrar</button>
		{{if .Error}}<div class="error">{{.Error}}</div>{{end}}
	</form>
</body>
</html>`

	tmpl := template.Must(template.New("login").Parse(loginHTML))
	return func(w http.ResponseWriter, r *http.Request) {
		if authUser == "" || authPass == "" {
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		}

		next := r.URL.Query().Get("next")
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			next = r.Form.Get("next")
			if next == "" {
				next = "/admin"
			}
			if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
				next = "/admin"
			}

			username := r.Form.Get("username")
			password := r.Form.Get("password")
			if constantTimeEqual(username, authUser) && constantTimeEqual(password, authPass) {
				if err := createAdminSession(w, r); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				http.Redirect(w, r, next, http.StatusSeeOther)
				return
			}

			w.WriteHeader(http.StatusUnauthorized)
			tmpl.Execute(w, map[string]string{
				"Next":  next,
				"Error": "Usuário ou senha inválidos.",
			})
			return
		}

		if next == "" {
			next = "/admin"
		}
		tmpl.Execute(w, map[string]string{"Next": next})
	}
}

func HandleLogout() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clearAdminSession(w, r)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}
