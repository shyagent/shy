package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"shy/server/internal/store"
)

// chatCompletions proxies an OpenAI-compatible chat request to the configured
// upstream (OpenRouter by default). The caller authenticates with an install
// key. The upstream key stays on this server.
func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.cfg.OpenAIAPIKey == "" || s.cfg.OpenAIBaseURL == "" {
		writeOpenAIError(w, http.StatusServiceUnavailable, "model proxy is not configured")
		return
	}
	key, presented := presentedKey(r)
	if !presented || key == "" {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid api key")
		return
	}
	inst, acc, err := s.store.InstallByKey(r.Context(), key)
	if errors.Is(err, store.ErrNotFound) {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid api key")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}

	limit := s.cfg.BodyLimit
	if limit <= 0 {
		limit = 8 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request is too large")
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid body")
		return
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "request body must be a JSON object")
		return
	}
	model := chatModel(obj["model"])

	if err := s.store.TouchInstall(r.Context(), inst.ID); err != nil {
		slog.Error("touch install", "err", err, "install", inst.ID)
	}

	ctx, cancel := context.WithTimeout(r.Context(), ChatTimeout)
	defer cancel()
	up, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.OpenAIBaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		s.fail(w, err)
		return
	}
	up.Header.Set("Authorization", "Bearer "+s.cfg.OpenAIAPIKey)
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("Accept", acceptHeader(r))
	up.Header.Set("HTTP-Referer", s.cfg.PublicURL)
	up.Header.Set("X-Title", "shy")

	start := time.Now()
	resp, err := s.upstream.Do(up)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		slog.Warn("chat upstream", "account", acc.ID, "install", inst.ID, "model", model, "err", err)
		writeOpenAIError(w, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer resp.Body.Close()

	level := slog.LevelInfo
	if resp.StatusCode >= 400 {
		level = slog.LevelWarn
	}
	slog.Log(r.Context(), level, "chat", "account", acc.ID, "install", inst.ID, "model", model, "status", resp.StatusCode, "ms", time.Since(start).Milliseconds())
	writeUpstream(w, resp)
}

func acceptHeader(r *http.Request) string {
	accept := strings.TrimSpace(r.Header.Get("Accept"))
	if accept == "" || strings.ContainsAny(accept, "\r\n") {
		return "application/json"
	}
	return accept
}

func chatModel(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var model string
	if err := json.Unmarshal(raw, &model); err != nil || model == "" || len(model) > 200 {
		return ""
	}
	for _, r := range model {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/' || r == '.' || r == '_' || r == ':' || r == '-' || r == '@' || r == '+':
		default:
			return ""
		}
	}
	return model
}

func writeUpstream(w http.ResponseWriter, resp *http.Response) {
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		w.Header().Set("Content-Length", cl)
	}
	w.WriteHeader(resp.StatusCode)
	if resp.StatusCode == http.StatusNoContent {
		return
	}
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func writeOpenAIError(w http.ResponseWriter, status int, msg string) {
	kind := "server_error"
	switch status {
	case http.StatusUnauthorized:
		kind = "authentication_error"
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		kind = "invalid_request_error"
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": msg, "type": kind},
	})
}
