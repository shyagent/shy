package neatlogs

import (
	"context"
	"errors"
	"strings"
	"testing"

	sdk "github.com/neatlogs/neatlogs-go"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestReplayUsesSDKSpans(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	client, err := Open(context.Background(), "project-key", "https://ingest.neatlogs.com", "shy", sdk.WithExporter(exp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close(context.Background())
	})

	body := []byte(`{
		"name":"chat-turn",
		"input":"fix the tests",
		"output":"done",
		"attributes":{
			"neatlogs.end_user.id":"1111222233334444",
			"neatlogs.session.id":"sess",
			"neatlogs.end_user.metadata":"{\"install\":\"laptop\"}"
		},
		"logs":[{"level":"info","message":"started"}],
		"children":[
			{"kind":"AGENT","name":"shy","attributes":{"neatlogs.agent.name":"shy","neatlogs.agent.role":"coder"},"children":[
				{"kind":"LLM","name":"deepseek.chat","model":"deepseek/deepseek-v4.1-flash","input":"fix the tests","output":"ok",
					"tokens":{"prompt":12,"completion":3,"total":15},
					"attributes":{"neatlogs.llm.provider":"deepseek","neatlogs.llm.finish_reason":"stop","neatlogs.llm.system_prompt":"be brief"}},
				{"kind":"TOOL","name":"bash","tool_name":"bash","input":"go test","output":"ok"},
				{"kind":"RETRIEVER","name":"search_docs","query":"wal mode","documents":["a","b"],"attributes":{"neatlogs.retriever.top_k":5}},
				{"kind":"HTTP","name":"ignored","children":[
					{"kind":"EMBEDDING","name":"embed","model":"embed-1","input":"wal"}
				]}
			]}
		]
	}`)
	id, spans, err := client.Forward(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 32 || spans != 6 {
		t.Fatalf("id %s spans %d", id, spans)
	}

	got := map[string]tracetest.SpanStub{}
	for _, span := range exp.GetSpans() {
		if attrOf(span, "neatlogs.internal") == "true" {
			continue
		}
		got[span.Name] = span
		if span.SpanContext.TraceID().String() != id {
			t.Fatalf("%s trace %s", span.Name, span.SpanContext.TraceID())
		}
	}
	root, ok := got["chat-turn"]
	if !ok || attrOf(root, "neatlogs.span.kind") != "workflow" {
		t.Fatalf("workflow %#v", names(got))
	}
	if attrOf(root, "neatlogs.end_user.id") != "1111222233334444" || attrOf(root, "neatlogs.session.id") != "sess" {
		t.Fatalf("identity input=%q user=%q session=%q", attrOf(root, "neatlogs.input.value"), attrOf(root, "neatlogs.end_user.id"), attrOf(root, "neatlogs.session.id"))
	}
	if !strings.Contains(attrOf(root, "neatlogs.input.value"), "fix the tests") {
		t.Fatal(attrOf(root, "neatlogs.input.value"))
	}
	if tag := resourceAttr(root, "neatlogs.tags"); tag != "project=shy" {
		t.Fatalf("tags %q", tag)
	}
	if len(root.Events) != 1 || root.Events[0].Name != "started" {
		t.Fatalf("logs %#v", root.Events)
	}

	agent := got["shy"]
	if attrOf(agent, "neatlogs.span.kind") != "agent" || attrOf(agent, "neatlogs.agent.role") != "coder" {
		t.Fatalf("agent kind %q role %q", attrOf(agent, "neatlogs.span.kind"), attrOf(agent, "neatlogs.agent.role"))
	}
	if agent.Parent.SpanID() != root.SpanContext.SpanID() {
		t.Fatal("agent is not a child of the workflow")
	}

	llm := got["deepseek.chat"]
	if attrOf(llm, "neatlogs.span.kind") != "llm" || attrOf(llm, "neatlogs.llm.model_name") != "deepseek/deepseek-v4.1-flash" {
		t.Fatalf("llm kind %q model %q", attrOf(llm, "neatlogs.span.kind"), attrOf(llm, "neatlogs.llm.model_name"))
	}
	if attrOf(llm, "neatlogs.llm.token_count.prompt") != "12" || attrOf(llm, "neatlogs.llm.input_messages.0.role") != "system" {
		t.Fatalf("tokens %q role %q", attrOf(llm, "neatlogs.llm.token_count.prompt"), attrOf(llm, "neatlogs.llm.input_messages.0.role"))
	}
	if llm.Parent.SpanID() != agent.SpanContext.SpanID() {
		t.Fatal("llm is not a child of the agent")
	}

	tool := got["bash"]
	if attrOf(tool, "neatlogs.span.kind") != "tool" || attrOf(tool, "neatlogs.tool.input") != "go test" {
		t.Fatalf("tool kind %q input %q", attrOf(tool, "neatlogs.span.kind"), attrOf(tool, "neatlogs.tool.input"))
	}

	retriever := got["search_docs"]
	if attrOf(retriever, "neatlogs.span.kind") != "retriever" || attrOf(retriever, "neatlogs.retriever.top_k") != "5" {
		t.Fatalf("retriever kind %q top_k %q", attrOf(retriever, "neatlogs.span.kind"), attrOf(retriever, "neatlogs.retriever.top_k"))
	}
	if _, ok := got["ignored"]; ok {
		t.Fatal("http span was exported")
	}
	embed := got["embed"]
	if attrOf(embed, "neatlogs.span.kind") != "embedding" || attrOf(embed, "neatlogs.embedding.model_name") != "embed-1" {
		t.Fatalf("embedding kind %q model %q", attrOf(embed, "neatlogs.span.kind"), attrOf(embed, "neatlogs.embedding.model_name"))
	}
	if embed.Parent.SpanID() != agent.SpanContext.SpanID() {
		t.Fatal("embedding under a dropped http span did not stay on the agent")
	}
}

func TestForwardExportFailure(t *testing.T) {
	client, err := Open(context.Background(), "project-key", "https://ingest.neatlogs.com", "", sdk.WithExporter(failExp{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	_, _, err = client.Forward(context.Background(), []byte(`{"name":"chat-turn"}`))
	if err == nil || !strings.Contains(err.Error(), "export") {
		t.Fatal(err)
	}
}

func TestDisabled(t *testing.T) {
	_, _, err := Disabled{}.Forward(context.Background(), []byte(`{}`))
	if err != ErrNotConfigured {
		t.Fatal(err)
	}
}

type failExp struct{}

func (failExp) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return errors.New("export down")
}
func (failExp) Shutdown(context.Context) error { return nil }

func attrOf(span tracetest.SpanStub, key string) string {
	for _, item := range span.Attributes {
		if string(item.Key) == key {
			return item.Value.Emit()
		}
	}
	return ""
}

func resourceAttr(span tracetest.SpanStub, key string) string {
	if span.Resource == nil {
		return ""
	}
	v, ok := span.Resource.Set().Value(attribute.Key(key))
	if !ok {
		return ""
	}
	return v.AsString()
}

func names(got map[string]tracetest.SpanStub) []string {
	out := make([]string, 0, len(got))
	for name := range got {
		out = append(out, name)
	}
	return out
}
