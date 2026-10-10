package httpapi

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"shy/server/internal/config"
	"shy/server/internal/neatlogs"
	"shy/server/internal/store"
	"shy/server/internal/version"
)

const (
	sessionCookie = "shy_session"
	formCookie    = "shy_form"
	sessionMaxAge = 30 * 24 * 3600
	formMaxAge    = 12 * 3600

	// ChatTimeout is the budget for one proxied completion, including the body.
	// main sets the HTTP write timeout a minute longer so a timeout can still
	// be written back to the client.
	ChatTimeout = 15 * time.Minute
)

//go:embed ui/pages.html
var pagesFS embed.FS

//go:embed ui/static/*
var staticFS embed.FS

// Options builds a Server. Zero rate limits get the production defaults.
type Options struct {
	Config         config.Config
	Store          *store.Store
	Forward        neatlogs.Forwarder
	LoginLimit     int
	LoginWindow    time.Duration
	RegisterLimit  int
	RegisterWindow time.Duration
}

// Server is the HTTP API and the dashboard.
type Server struct {
	cfg      config.Config
	store    *store.Store
	forward  neatlogs.Forwarder
	tpl      *template.Template
	static   http.Handler
	signIn   *limiter
	signUp   *limiter
	upstream *http.Client
}

// New parses templates and returns a server. It does not listen.
func New(opt Options) (*Server, error) {
	if opt.Forward == nil {
		opt.Forward = neatlogs.Disabled{}
	}
	if opt.LoginLimit == 0 {
		opt.LoginLimit = 20
	}
	if opt.LoginWindow == 0 {
		opt.LoginWindow = 10 * time.Minute
	}
	if opt.RegisterLimit == 0 {
		opt.RegisterLimit = 5
	}
	if opt.RegisterWindow == 0 {
		opt.RegisterWindow = time.Hour
	}
	tpl, err := template.New("pages").Funcs(template.FuncMap{
		"tone": toneClass,
	}).ParseFS(pagesFS, "ui/pages.html")
	if err != nil {
		return nil, err
	}
	sub, err := fs.Sub(staticFS, "ui/static")
	if err != nil {
		return nil, err
	}
	files := http.FileServer(http.FS(sub))
	return &Server{
		cfg:     opt.Config,
		store:   opt.Store,
		forward: opt.Forward,
		tpl:     tpl,
		static: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "public, max-age=3600")
			files.ServeHTTP(w, r)
		}),
		signIn:   &limiter{limit: opt.LoginLimit, window: opt.LoginWindow, hits: map[string][]time.Time{}},
		signUp:   &limiter{limit: opt.RegisterLimit, window: opt.RegisterWindow, hits: map[string][]time.Time{}},
		upstream: newUpstreamClient(),
	}, nil
}

func newUpstreamClient() *http.Client {
	client := &http.Client{
		Timeout: ChatTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirects are not followed")
		},
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return client
	}
	tr := base.Clone()
	tr.DisableCompression = true
	tr.ResponseHeaderTimeout = ChatTimeout
	client.Transport = tr
	return client
}

// Handler is the full mux, including security headers.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("POST /v1/chat/completions", s.chatCompletions)
	mux.HandleFunc("POST /v1/trace", s.ingest)
	mux.Handle("GET /static/", http.StripPrefix("/static/", s.static))
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("POST /accounts", s.createAccount)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("POST /installs", s.createInstall)
	mux.HandleFunc("POST /installs/{id}/revoke", s.revokeInstall)
	mux.HandleFunc("GET /traces", s.traces)
	mux.HandleFunc("GET /traces/{id}", s.traceDetail)
	return secure(mux)
}

func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) render(w http.ResponseWriter, name string, v view, status int) {
	v.Version = version.Version
	var buf bytes.Buffer
	if err := s.tpl.ExecuteTemplate(&buf, name, v); err != nil {
		slog.Error("template", "name", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	slog.Error("request", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) (store.Session, string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return store.Session{}, "", false
	}
	sess, err := s.store.Session(r.Context(), c.Value)
	if err != nil {
		s.clearSessionCookie(w)
		return store.Session{}, "", false
	}
	return sess, c.Value, true
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   sessionMaxAge,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// formToken is a double-submit cookie for the signed-out forms.
func (s *Server) formToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(formCookie); err == nil && validToken(c.Value) {
		return c.Value
	}
	tok, err := randomToken()
	if err != nil {
		tok = "missing-token-retry"
	}
	http.SetCookie(w, &http.Cookie{
		Name:     formCookie,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   formMaxAge,
	})
	return tok
}

func (s *Server) formOK(r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		return false
	}
	c, err := r.Cookie(formCookie)
	if err != nil {
		return false
	}
	return tokensEqual(c.Value, r.FormValue("form"))
}

func (s *Server) csrfOK(r *http.Request, sess store.Session) bool {
	if err := r.ParseForm(); err != nil {
		return false
	}
	if tokensEqual(r.Header.Get("X-Shy-CSRF"), sess.CSRF) {
		return true
	}
	return tokensEqual(r.FormValue("csrf"), sess.CSRF)
}

func tokensEqual(a, b string) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func validToken(s string) bool {
	if len(s) != 43 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func toneClass(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "OK", "SENT":
		return "ok"
	case "ERROR", "FAILED":
		return "bad"
	case "SKIPPED", "PENDING":
		return "wait"
	default:
		return ""
	}
}

func hx(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

type limiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
}

func (l *limiter) allow(key string) bool {
	if l == nil || l.limit <= 0 {
		return true
	}
	now := time.Now()
	cut := now.Add(-l.window)
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hits) > 10000 {
		l.hits = map[string][]time.Time{}
	}
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.limit {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}
