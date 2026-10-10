package httpapi

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"

	"shy/server/internal/store"
)

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		http.Error(w, "db", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	sess, raw, ok := s.session(w, r)
	if !ok {
		s.gate(w, r, "", "", http.StatusOK)
		return
	}
	flash, err := s.store.TakeFlash(r.Context(), raw)
	if errors.Is(err, store.ErrNotFound) {
		s.clearSessionCookie(w)
		s.gate(w, r, "", "", http.StatusOK)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	v, err := s.dashboardView(r, sess, flash)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "dashboard", v, http.StatusOK)
}

func (s *Server) gate(w http.ResponseWriter, r *http.Request, loginErr, registerErr string, status int) {
	if status == 0 {
		status = http.StatusOK
	}
	s.render(w, "gate", view{
		Title:         "shy",
		FormToken:     s.formToken(w, r),
		LoginError:    loginErr,
		RegisterError: registerErr,
	}, status)
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if !s.signUp.allow(clientIP(r, s.cfg.TrustProxy)) {
		s.gate(w, r, "", "Too many new accounts from this network. Try again later.", http.StatusTooManyRequests)
		return
	}
	if !s.formOK(r) {
		http.Error(w, "Form expired. Reload and try again.", http.StatusForbidden)
		return
	}
	acc, raw, err := s.store.CreateAccount(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.SetFlash(r.Context(), raw, store.Flash{NewAccount: true}); err != nil {
		slog.Error("flash", "err", err, "account", acc.ID)
	}
	s.setSessionCookie(w, raw)
	slog.Info("account created", "account", acc.ID)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if !s.signIn.allow(clientIP(r, s.cfg.TrustProxy)) {
		s.gate(w, r, "Too many sign-in attempts. Wait a few minutes.", "", http.StatusTooManyRequests)
		return
	}
	if !s.formOK(r) {
		http.Error(w, "Form expired. Reload and try again.", http.StatusForbidden)
		return
	}
	number, ok := normalizeNumber(r.FormValue("number"))
	if !ok {
		s.gate(w, r, "Enter the 16-digit account number.", "", http.StatusUnauthorized)
		return
	}
	acc, err := s.store.AccountByNumber(r.Context(), number)
	if err != nil || acc.Status != "active" {
		s.gate(w, r, "No account uses that number.", "", http.StatusUnauthorized)
		return
	}
	raw, err := s.store.CreateSession(r.Context(), acc.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.setSessionCookie(w, raw)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	sess, raw, ok := s.session(w, r)
	if ok {
		if !s.csrfOK(r, sess) {
			http.Error(w, "Form expired. Reload and try again.", http.StatusForbidden)
			return
		}
		if err := s.store.DeleteSession(r.Context(), raw); err != nil {
			slog.Error("logout", "err", err)
		}
	}
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) createInstall(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	sess, raw, ok := s.session(w, r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.csrfOK(r, sess) {
		http.Error(w, "Form expired. Reload and try again.", http.StatusForbidden)
		return
	}
	inst, key, err := s.store.CreateInstall(r.Context(), sess.AccountID, r.FormValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.SetFlash(r.Context(), raw, store.Flash{Key: key, InstallName: inst.Name}); err != nil {
		s.fail(w, err)
		return
	}
	slog.Info("install created", "account", sess.AccountID, "install", inst.ID)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) revokeInstall(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	sess, _, ok := s.session(w, r)
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.csrfOK(r, sess) {
		http.Error(w, "Form expired. Reload and try again.", http.StatusForbidden)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	if err := s.store.RevokeInstall(r.Context(), sess.AccountID, id); errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	slog.Info("install revoked", "account", sess.AccountID, "install", id)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) traces(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.session(w, r)
	if !ok {
		if hx(r) {
			w.Header().Set("HX-Redirect", "/")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	list, err := s.store.ListTraces(r.Context(), sess.AccountID, 50)
	if err != nil {
		s.fail(w, err)
		return
	}
	if hx(r) {
		s.render(w, "traces", buildView(s.cfg, sess, store.Flash{}, nil, list), http.StatusOK)
		return
	}
	v, err := s.dashboardView(r, sess, store.Flash{})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "dashboard", v, http.StatusOK)
}

func (s *Server) traceDetail(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.session(w, r)
	if !ok {
		if hx(r) {
			w.Header().Set("HX-Redirect", "/")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	tr, err := s.store.Trace(r.Context(), sess.AccountID, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if hx(r) {
		s.render(w, "detail", view{CSRF: sess.CSRF, Detail: toDetail(tr)}, http.StatusOK)
		return
	}
	v, err := s.dashboardView(r, sess, store.Flash{})
	if err != nil {
		s.fail(w, err)
		return
	}
	v.Detail = toDetail(tr)
	v.Title = tr.Workflow
	s.render(w, "dashboard", v, http.StatusOK)
}

func (s *Server) dashboardView(r *http.Request, sess store.Session, flash store.Flash) (view, error) {
	installs, err := s.store.ListInstalls(r.Context(), sess.AccountID)
	if err != nil {
		return view{}, err
	}
	traces, err := s.store.ListTraces(r.Context(), sess.AccountID, 50)
	if err != nil {
		return view{}, err
	}
	return buildView(s.cfg, sess, flash, installs, traces), nil
}

func clientIP(r *http.Request, trust bool) string {
	if trust {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first := strings.TrimSpace(strings.Split(xff, ",")[0])
			if first != "" {
				return first
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
