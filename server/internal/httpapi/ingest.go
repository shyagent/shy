package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"shy/server/internal/neatlogs"
	"shy/server/internal/store"
	"shy/server/internal/trace"
)

func (s *Server) ingest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	key, presented := presentedKey(r)
	if presented && key == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "invalid api key"})
		return
	}
	var accountID, installID int64
	var accountNumber, installName string
	if presented {
		inst, acc, err := s.store.InstallByKey(r.Context(), key)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "invalid api key"})
			return
		}
		accountID = acc.ID
		installID = inst.ID
		accountNumber = acc.Number
		installName = inst.Name
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.BodyLimit)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"success": false, "error": "trace is too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "invalid body"})
		return
	}
	writeKey := strings.HasPrefix(s.cfg.NeatlogsAPIKey, "nlw_")
	patched, meta, err := trace.Prepare(body, accountNumber, installID, installName, s.cfg.NeatlogsProject, writeKey)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
		return
	}
	id, err := s.store.InsertTrace(r.Context(), store.NewTrace{
		AccountID:        accountID,
		InstallID:        installID,
		Workflow:         meta.Workflow,
		Status:           meta.Status,
		SessionID:        meta.SessionID,
		InputPreview:     meta.InputPreview,
		OutputPreview:    meta.OutputPreview,
		PromptTokens:     meta.PromptTokens,
		CompletionTokens: meta.CompletionTokens,
		SpanCount:        meta.Spans,
		Payload:          string(patched),
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	if installID != 0 {
		if err := s.store.TouchInstall(r.Context(), installID); err != nil {
			slog.Error("touch install", "err", err, "install", installID)
		}
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Second)
	defer cancel()
	neatID, spans, ferr := s.forward.Forward(ctx, patched)
	if errors.Is(ferr, neatlogs.ErrNotConfigured) {
		if err := s.store.MarkForward(r.Context(), id, "skipped", "", ""); err != nil {
			slog.Error("mark skipped", "err", err, "trace", id)
		}
		slog.Info("trace stored", "account", accountID, "install", installID, "workflow", meta.Workflow, "forward", "skipped")
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "stored": true, "forwarded": false})
		return
	}
	if ferr != nil {
		msg := clipErr(ferr.Error())
		if err := s.store.MarkForward(r.Context(), id, "failed", "", msg); err != nil {
			slog.Error("mark failed", "err", err, "trace", id)
		}
		slog.Warn("forward failed", "account", accountID, "install", installID, "workflow", meta.Workflow, "err", msg)
		// Stored either way. 502 so shy prints its ingest warning. shy does not retry.
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"success": false, "stored": true, "error": "forwarding to neatlogs failed",
		})
		return
	}
	if spans == 0 {
		spans = meta.Spans
	}
	if err := s.store.MarkForward(r.Context(), id, "sent", neatID, ""); err != nil {
		slog.Error("mark sent", "err", err, "trace", id)
	}
	slog.Info("trace stored", "account", accountID, "install", installID, "workflow", meta.Workflow, "forward", "sent", "neatlogs_trace_id", neatID)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "stored": true, "forwarded": true, "trace_id": neatID, "spans": spans,
	})
}

// presentedKey reports the install key. presented is false when both
// Authorization and X-Api-Key are absent, which is an anonymous submission.
// A header that is present but unusable yields presented with an empty key.
func presentedKey(r *http.Request) (key string, presented bool) {
	var bearer string
	h := r.Header.Get("Authorization")
	if len(h) >= 7 && strings.EqualFold(h[:7], "bearer ") {
		bearer = strings.TrimSpace(h[7:])
	}
	x := strings.TrimSpace(r.Header.Get("X-Api-Key"))
	if bearer == "" && x == "" {
		return "", false
	}
	if bearer != "" && x != "" && bearer != x {
		return "", true
	}
	key = bearer
	if key == "" {
		key = x
	}
	if len(key) < 8 || len(key) > 200 {
		return "", true
	}
	return key, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
