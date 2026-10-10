package httpapi

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"shy/server/internal/config"
	"shy/server/internal/store"
	"shy/server/internal/trace"
)

type view struct {
	Title         string
	Version       string
	CSRF          string
	FormToken     string
	PublicURL     string
	LoginError    string
	RegisterError string
	NumberRaw     string
	NumberPretty  string
	NewAccount    bool
	Installs      []installView
	Reveal        *revealView
	Traces        []traceView
	Detail        *detailView
}

type installView struct {
	ID       int64
	Name     string
	Prefix   string
	Created  string
	LastSeen string
	Revoked  bool
}

type revealView struct {
	Name string
	Key  string
	Env  string
}

type traceView struct {
	ID       int64
	When     string
	Workflow string
	Status   string
	Tokens   string
	Install  string
	Forward  string
}

type detailView struct {
	When         string
	Workflow     string
	Status       string
	Session      string
	Install      string
	Forward      string
	ForwardError string
	NeatlogsID   string
	Prompt       int64
	Completion   int64
	SpanCount    int
	Input        string
	Output       string
	Spans        []trace.Node
	Raw          string
}

func buildView(cfg config.Config, sess store.Session, flash store.Flash, installs []store.Install, traces []store.Trace) view {
	public := strings.TrimRight(cfg.PublicURL, "/")
	v := view{
		Title:        "shy",
		CSRF:         sess.CSRF,
		PublicURL:    public,
		NumberRaw:    sess.Number,
		NumberPretty: prettyNumber(sess.Number),
		NewAccount:   flash.NewAccount,
	}
	for _, in := range installs {
		v.Installs = append(v.Installs, installView{
			ID:       in.ID,
			Name:     in.Name,
			Prefix:   in.KeyPrefix,
			Created:  fmtTime(in.CreatedAt),
			LastSeen: fmtTime(in.LastSeenAt),
			Revoked:  in.RevokedAt != "",
		})
	}
	for _, tr := range traces {
		v.Traces = append(v.Traces, traceView{
			ID:       tr.ID,
			When:     fmtTime(tr.ReceivedAt),
			Workflow: tr.Workflow,
			Status:   tr.Status,
			Tokens:   fmt.Sprintf("%d in · %d out", tr.PromptTokens, tr.CompletionTokens),
			Install:  tr.InstallName,
			Forward:  tr.ForwardStatus,
		})
	}
	if flash.Key != "" {
		name := flash.InstallName
		if name == "" {
			name = "install"
		}
		v.Reveal = &revealView{
			Name: name,
			Key:  flash.Key,
			Env:  "export NEATLOGS_INGEST_URL=" + public + "\nexport NEATLOGS_API_KEY=" + flash.Key,
		}
	}
	return v
}

func toDetail(tr store.Trace) *detailView {
	session := tr.SessionID
	if session == "" {
		session = "—"
	}
	nid := tr.NeatlogsTraceID
	if nid == "" {
		nid = "—"
	}
	d := &detailView{
		When:         fmtTime(tr.ReceivedAt),
		Workflow:     tr.Workflow,
		Status:       tr.Status,
		Session:      session,
		Install:      tr.InstallName,
		Forward:      tr.ForwardStatus,
		ForwardError: tr.ForwardError,
		NeatlogsID:   nid,
		Prompt:       tr.PromptTokens,
		Completion:   tr.CompletionTokens,
		SpanCount:    tr.SpanCount,
		Input:        tr.InputPreview,
		Output:       tr.OutputPreview,
		Raw:          clipRaw(tr.Payload, 65536),
	}
	if node, err := trace.Tree([]byte(tr.Payload)); err == nil {
		d.Spans = []trace.Node{node}
	}
	return d
}

func prettyNumber(n string) string {
	if len(n) != 16 {
		return n
	}
	return n[0:4] + " " + n[4:8] + " " + n[8:12] + " " + n[12:16]
}

func normalizeNumber(s string) (string, bool) {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case ' ', '-', '\t', '\n', '\r':
			continue
		}
		if r < '0' || r > '9' {
			return "", false
		}
		b.WriteRune(r)
	}
	if b.Len() != 16 {
		return "", false
	}
	return b.String(), true
}

func fmtTime(s string) string {
	if s == "" {
		return "—"
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.UTC().Format("2006-01-02 15:04")
}

func clipRaw(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "\n… truncated"
}

func clipErr(s string) string {
	n := 500
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
