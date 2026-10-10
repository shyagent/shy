package trace

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPrepareRewritesIdentity(t *testing.T) {
	raw := []byte(`{
		"name": " chat-turn ",
		"project": "evil",
		"trace_id": "nope",
		"status": "ok",
		"input": "hello",
		"attributes": {"neatlogs.end_user.id": "spoof", "neatlogs.session.id": "sess-1"},
		"children": [{
			"kind": "AGENT",
			"span_id": "abc",
			"parent_span_id": "def",
			"children": [
				{"kind": "LLM", "tokens": {"prompt": 10, "completion": 4, "total": 14}},
				{"kind": "llm", "tokens": {"prompt": 3, "completion": 1}}
			]
		}]
	}`)
	out, meta, err := Prepare(raw, "1111222233334444", 7, "laptop", "ignored", false)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Workflow != "chat-turn" || meta.Status != "OK" || meta.SessionID != "sess-1" {
		t.Fatalf("%+v", meta)
	}
	if meta.PromptTokens != 13 || meta.CompletionTokens != 5 || meta.Spans != 4 {
		t.Fatalf("tokens/spans %+v", meta)
	}
	if meta.InputPreview != "hello" {
		t.Fatal(meta.InputPreview)
	}
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if _, ok := root["project"]; ok {
		t.Fatal("project key should be removed for a project api key")
	}
	if _, ok := root["trace_id"]; ok {
		t.Fatal("trace_id should be stripped")
	}
	attrs := root["attributes"].(map[string]any)
	if attrs["neatlogs.end_user.id"] != "1111222233334444" {
		t.Fatal(attrs["neatlogs.end_user.id"])
	}
	metaStr, ok := attrs["neatlogs.end_user.metadata"].(string)
	if !ok || !strings.Contains(metaStr, `"install":"laptop"`) || !strings.Contains(metaStr, `"install_id":7`) {
		t.Fatalf("metadata %#v", attrs["neatlogs.end_user.metadata"])
	}
	kids := root["children"].([]any)
	child := kids[0].(map[string]any)
	if _, ok := child["span_id"]; ok {
		t.Fatal("child span_id remains")
	}
}

func TestPrepareAnonymous(t *testing.T) {
	out, _, err := Prepare([]byte(`{"name":"run","attributes":{"neatlogs.end_user.id":"spoof"}}`), "", 0, "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	attrs := root["attributes"].(map[string]any)
	if attrs["neatlogs.end_user.id"] != "anonymous" || !strings.Contains(attrs["neatlogs.end_user.metadata"].(string), `"anonymous":true`) {
		t.Fatalf("%#v", attrs)
	}
}

func TestPrepareWriteKeySetsProject(t *testing.T) {
	out, meta, err := Prepare([]byte(`{"name":"run","status":"ERROR","project":"evil"}`), "1", 1, "box", "demo", true)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Status != "ERROR" {
		t.Fatal(meta.Status)
	}
	var root map[string]any
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatal(err)
	}
	if root["project"] != "demo" {
		t.Fatalf("project %v", root["project"])
	}
	if root["kind"] != "WORKFLOW" {
		t.Fatal(root["kind"])
	}
}

func TestPrepareRejects(t *testing.T) {
	cases := []string{
		``,
		`[]`,
		`{"status":"OK"}`,
		`{"name":"x"} extra`,
		`{"name":"x","children":{}}`,
	}
	for _, c := range cases {
		if _, _, err := Prepare([]byte(c), "1", 1, "n", "", false); err == nil {
			t.Fatalf("accepted %s", c)
		}
	}
}

func TestPrepareTooDeep(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"name":"deep"`)
	for i := 0; i < 80; i++ {
		b.WriteString(`,"children":[{"name":"n"`)
	}
	for i := 0; i < 80; i++ {
		b.WriteString(`}]`)
	}
	b.WriteString(`}`)
	_, _, err := Prepare([]byte(b.String()), "1", 1, "n", "", false)
	if err == nil || !strings.Contains(err.Error(), "nested") {
		t.Fatal(err)
	}
}
