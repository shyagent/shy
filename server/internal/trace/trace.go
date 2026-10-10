// Package trace prepares a shy trace for storage and for Neatlogs.
//
// The body is the nested JSON tree shy already posts to /v1/trace.
// Prepare keeps that shape, stamps the account on as the end user, and
// removes client-chosen project and span ids so the server's Neatlogs
// key is what selects the project.
package trace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	previewLen = 240
	maxName    = 200
	maxNodes   = 10000
	maxDepth   = 64
)

var (
	errInvalid = errors.New("invalid JSON")
	errShape   = errors.New("trace must be a JSON object")
	errName    = errors.New("trace name is required")
	errTooBig  = errors.New("trace has too many spans")
	errTooDeep = errors.New("trace is nested too deeply")
	errKids    = errors.New("children must be an array")
)

// Meta is the part the database indexes. The full tree is stored separately.
type Meta struct {
	Workflow         string
	Status           string
	SessionID        string
	InputPreview     string
	OutputPreview    string
	PromptTokens     int64
	CompletionTokens int64
	Spans            int
}

// Node is one span for the dashboard tree.
type Node struct {
	Name     string
	Kind     string
	Status   string
	Meta     string
	Children []Node
}

// Prepare validates raw, stamps accountNumber onto the root, and returns the
// JSON that should be stored and forwarded. writeKey selects whether project
// is set from projectName (nlw_ keys) or removed (project keys).
func Prepare(raw []byte, accountNumber string, installID int64, installName, projectName string, writeKey bool) ([]byte, Meta, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil || dec.More() {
		return nil, Meta{}, errInvalid
	}
	obj, ok := root.(map[string]any)
	if !ok {
		return nil, Meta{}, errShape
	}
	name, _ := obj["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, Meta{}, errName
	}
	name = clip(name, maxName)
	obj["name"] = name

	stripIDs(obj)
	if writeKey {
		obj["project"] = projectName
	} else {
		delete(obj, "project")
	}
	if kind, _ := obj["kind"].(string); strings.TrimSpace(kind) == "" {
		obj["kind"] = "WORKFLOW"
	}

	attrs, ok := obj["attributes"].(map[string]any)
	if !ok {
		attrs = map[string]any{}
		obj["attributes"] = attrs
	}
	session, _ := attrs["neatlogs.session.id"].(string)
	var metaJSON []byte
	var err error
	if accountNumber == "" {
		attrs["neatlogs.end_user.id"] = "anonymous"
		metaJSON, err = json.Marshal(map[string]any{"anonymous": true})
	} else {
		attrs["neatlogs.end_user.id"] = accountNumber
		metaJSON, err = json.Marshal(map[string]any{
			"install_id": installID,
			"install":    installName,
		})
	}
	if err != nil {
		return nil, Meta{}, err
	}
	// Neatlogs wants this value as a JSON string, not an object.
	attrs["neatlogs.end_user.metadata"] = string(metaJSON)

	status := "OK"
	if s, ok := obj["status"].(string); ok && strings.EqualFold(strings.TrimSpace(s), "ERROR") {
		status = "ERROR"
	}
	obj["status"] = status

	var st stats
	if err := walk(obj, &st, 0); err != nil {
		return nil, Meta{}, err
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, Meta{}, err
	}
	return out, Meta{
		Workflow:         name,
		Status:           status,
		SessionID:        clip(session, maxName),
		InputPreview:     preview(obj["input"]),
		OutputPreview:    preview(obj["output"]),
		PromptTokens:     st.prompt,
		CompletionTokens: st.completion,
		Spans:            st.spans,
	}, nil
}

// Tree parses a stored payload into a span tree for the dashboard.
func Tree(payload []byte) (Node, error) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return Node{}, err
	}
	n, ok := toNode(v, 0)
	if !ok {
		return Node{}, errShape
	}
	return n, nil
}

func stripIDs(v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	delete(m, "trace_id")
	delete(m, "span_id")
	delete(m, "parent_span_id")
	kids, _ := m["children"].([]any)
	for _, c := range kids {
		stripIDs(c)
	}
}

type stats struct {
	spans      int
	prompt     int64
	completion int64
}

func walk(v any, st *stats, depth int) error {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	if st.spans >= maxNodes {
		return errTooBig
	}
	if depth > maxDepth {
		return errTooDeep
	}
	st.spans++
	if kind, _ := m["kind"].(string); strings.EqualFold(kind, "LLM") {
		if tok, ok := m["tokens"].(map[string]any); ok {
			st.prompt += integer(tok["prompt"])
			st.completion += integer(tok["completion"])
		}
	}
	kids, exists := m["children"]
	if !exists || kids == nil {
		return nil
	}
	list, ok := kids.([]any)
	if !ok {
		return errKids
	}
	for _, c := range list {
		if err := walk(c, st, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func toNode(v any, depth int) (Node, bool) {
	if depth > maxDepth {
		return Node{Name: "…", Kind: "SPAN"}, true
	}
	m, ok := v.(map[string]any)
	if !ok {
		return Node{}, false
	}
	n := Node{
		Name:   str(m["name"]),
		Kind:   str(m["kind"]),
		Status: str(m["status"]),
	}
	if n.Kind == "" {
		n.Kind = "SPAN"
	}
	var bits []string
	if model := str(m["model"]); model != "" {
		bits = append(bits, model)
	}
	if tool := str(m["tool_name"]); tool != "" {
		bits = append(bits, tool)
	}
	if d := integer(m["duration_ms"]); d > 0 {
		bits = append(bits, fmt.Sprintf("%dms", d))
	}
	if msg := str(m["error"]); msg != "" {
		bits = append(bits, clip(msg, 160))
	}
	n.Meta = strings.Join(bits, " · ")
	if kids, ok := m["children"].([]any); ok {
		for _, c := range kids {
			if child, ok := toNode(c, depth+1); ok {
				n.Children = append(n.Children, child)
			}
		}
	}
	return n, true
}

func preview(v any) string {
	if v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		s = string(b)
	}
	return clip(s, previewLen)
}

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func integer(v any) int64 {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
		f, err := n.Float64()
		if err != nil {
			return 0
		}
		return int64(f)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}
