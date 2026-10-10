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
	Name    string
	Key     string
	Command string
	Env     string
}

type traceView struct {
	ID       int64
	When     string
	Workflow string
	Input    string
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
		NumberPretty: maskNumber(sess.Number),
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
			Input:    redactAccount(tr.InputPreview, sess.Number),
			Status:   tr.Status,
			Tokens:   fmt.Sprintf("%d in · %d out", tr.PromptTokens, tr.CompletionTokens),
			Install:  tr.InstallName,
			Forward:  tr.ForwardStatus,
		})
	}
	if flash.Key != "" {
		name := flash.InstallName
		if name == "" {
			name = "agent"
		}
		v.Reveal = &revealView{
			Name:    name,
			Key:     flash.Key,
			Command: runLine(public, flash.Key),
			Env:     "export SHY_BASE_URL=" + public + "\nexport SHY_API_KEY=" + flash.Key + "\nexport NEATLOGS_INGEST_URL=" + public + "\nexport NEATLOGS_API_KEY=" + flash.Key,
		}
	}
	return v
}

// runLine is the one-shot install. The local default already points shy at
// this server, so SHY_BASE_URL is added only when the public origin differs.
func runLine(public, key string) string {
	line := "curl github.com/shyagent/shy/shy.sh > shy.sh && "
	if public != "" && public != "http://127.0.0.1:8080" {
		line += "SHY_BASE_URL=" + shellWord(public) + " "
	}
	return line + "SHY_API_KEY=" + shellWord(key) + " ./shy.sh"
}

func shellWord(s string) string {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-' || r == '.' || r == '_' || r == ':' || r == '/':
		default:
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	if s == "" {
		return "''"
	}
	return s
}

func toDetail(tr store.Trace, accountNumber string) *detailView {
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
		ForwardError: redactAccount(tr.ForwardError, accountNumber),
		NeatlogsID:   nid,
		Prompt:       tr.PromptTokens,
		Completion:   tr.CompletionTokens,
		SpanCount:    tr.SpanCount,
		Input:        redactAccount(tr.InputPreview, accountNumber),
		Output:       redactAccount(tr.OutputPreview, accountNumber),
		Raw:          clipRaw(redactAccount(tr.Payload, accountNumber), 65536),
	}
	if node, err := trace.Tree([]byte(tr.Payload)); err == nil {
		d.Spans = redactNodes([]trace.Node{node}, accountNumber)
	}
	return d
}

func prettyNumber(n string) string {
	if len(n) != 16 {
		return n
	}
	return n[0:4] + " " + n[4:8] + " " + n[8:12] + " " + n[12:16]
}

// maskNumber hides an account number on screen, leaving the last four digits.
func maskNumber(n string) string {
	if len(n) < 4 {
		return "••••"
	}
	return "•••• •••• •••• " + n[len(n)-4:]
}

// redactAccount removes an account number from text that is about to be shown.
func redactAccount(s, number string) string {
	if s == "" || len(number) != 16 {
		return s
	}
	s = strings.ReplaceAll(s, number, strings.Repeat("•", 12)+number[12:])
	return strings.ReplaceAll(s, prettyNumber(number), maskNumber(number))
}

func redactNodes(nodes []trace.Node, number string) []trace.Node {
	if len(number) != 16 {
		return nodes
	}
	out := make([]trace.Node, len(nodes))
	for i, n := range nodes {
		n.Name = redactAccount(n.Name, number)
		n.Status = redactAccount(n.Status, number)
		n.Meta = redactAccount(n.Meta, number)
		n.Children = redactNodes(n.Children, number)
		out[i] = n
	}
	return out
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
