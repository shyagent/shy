package neatlogs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	sdk "github.com/neatlogs/neatlogs-go"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func replay(ctx context.Context, body []byte) (string, int, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil || dec.More() {
		return "", 0, errors.New("invalid JSON")
	}
	obj, ok := root.(map[string]any)
	if !ok {
		return "", 0, errors.New("trace must be a JSON object")
	}
	traceID, spans, err := emitWorkflow(ctx, obj)
	if err != nil {
		return "", 0, err
	}
	return traceID, spans, nil
}

func emitWorkflow(ctx context.Context, node map[string]any) (string, int, error) {
	ctx = sdk.Identify(ctx, identity(node))
	ctx, span, end := sdk.Trace(ctx, nodeName(node, "workflow"))
	defer end()
	if !span.SpanContext().TraceID().IsValid() {
		return "", 0, errors.New("neatlogs pipeline is not accepting spans")
	}
	if v, ok := node["input"]; ok && v != nil {
		if err := sdk.SetTraceInput(span, v); err != nil {
			return "", 0, err
		}
	}
	n, err := emitChildren(ctx, node)
	if err != nil {
		return "", 0, err
	}
	if v, ok := node["output"]; ok && v != nil {
		if err := sdk.SetTraceOutput(span, v); err != nil {
			return "", 0, err
		}
	}
	markStatus(span, node)
	addLogs(span, node)
	return span.SpanContext().TraceID().String(), n + 1, nil
}

func emitChildren(ctx context.Context, node map[string]any) (int, error) {
	raw, exists := node["children"]
	if !exists || raw == nil {
		return 0, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return 0, errors.New("children must be an array")
	}
	n := 0
	for _, item := range list {
		child, ok := item.(map[string]any)
		if !ok {
			continue
		}
		got, err := emitNode(ctx, child)
		if err != nil {
			return n, err
		}
		n += got
	}
	return n, nil
}

func emitNode(ctx context.Context, node map[string]any) (int, error) {
	switch kindOf(node) {
	case "llm":
		return emitLLM(ctx, node)
	case "retriever":
		return emitRetriever(ctx, node)
	case "http":
		// The SDK drops HTTP spans. Keep their children on the current parent.
		return emitChildren(ctx, node)
	default:
		return emitGeneric(ctx, node)
	}
}

func emitLLM(ctx context.Context, node map[string]any) (int, error) {
	attrs := attrMap(node)
	opts := sdk.LLMCallOptions{
		Name:      nodeName(node, "llm"),
		Provider:  attrString(attrs, "neatlogs.llm.provider"),
		Model:     firstString(str(node["model"]), attrString(attrs, "neatlogs.llm.model_name")),
		Messages:  llmMessages(node, attrs),
		Streaming: attrBool(attrs, "neatlogs.llm.is_streaming"),
	}
	if n, ok := attrInt(attrs, "neatlogs.llm.max_tokens"); ok {
		opts.MaxTokens = n
	}
	if f, ok := attrFloat(attrs, "neatlogs.llm.temperature"); ok {
		opts.Temperature = &f
	}
	if f, ok := attrFloat(attrs, "neatlogs.llm.top_p"); ok {
		opts.TopP = &f
	}
	ctx, span := sdk.StartLLMSpan(ctx, opts)
	if id := attrString(attrs, "neatlogs.llm.response_id"); id != "" {
		span.SetResponseID(id)
	}
	if reason := firstString(attrString(attrs, "neatlogs.llm.finish_reason"), str(node["finish_reason"])); reason != "" {
		span.SetFinishReason(reason)
	}
	prompt, completion, total := tokenCounts(node, attrs)
	span.SetUsage(prompt, completion, total)
	if out := text(node["output"]); out != "" {
		span.SetOutputMessage("assistant", out)
	}
	if msg, failed := failure(node); failed {
		span.SetError(errors.New(msg))
	}
	n, err := emitChildren(ctx, node)
	span.End()
	if err != nil {
		return n, err
	}
	return n + 1, nil
}

func emitRetriever(ctx context.Context, node map[string]any) (int, error) {
	attrs := attrMap(node)
	query := firstString(str(node["query"]), text(node["input"]), attrString(attrs, "neatlogs.retriever.query"))
	topK, _ := attrInt(attrs, "neatlogs.retriever.top_k")
	ctx, span := sdk.StartRetrieverSpan(ctx, nodeName(node, "retriever"), query, topK)
	docs := node["documents"]
	if docs == nil {
		docs = attrs["neatlogs.retriever.documents"]
	}
	if docs == nil {
		docs = node["output"]
	}
	if docs != nil {
		span.SetDocuments(docs, valueCount(docs))
	}
	if msg, failed := failure(node); failed {
		span.SetError(errors.New(msg))
	}
	n, err := emitChildren(ctx, node)
	span.End()
	if err != nil {
		return n, err
	}
	return n + 1, nil
}

func emitGeneric(ctx context.Context, node map[string]any) (int, error) {
	kind := kindOf(node)
	attrs := attrMap(node)
	var extra []attribute.KeyValue
	switch kind {
	case "tool", "mcp_tool":
		name := firstString(str(node["tool_name"]), nodeName(node, kind))
		extra = append(extra, attribute.String("neatlogs.tool.name", name))
		if in := text(node["input"]); in != "" {
			extra = append(extra,
				attribute.String("neatlogs.tool.input", in),
				attribute.String("neatlogs.input.value", in),
			)
		}
		if out := text(node["output"]); out != "" {
			extra = append(extra,
				attribute.String("neatlogs.tool.output", out),
				attribute.String("neatlogs.output.value", out),
			)
		}
	case "agent":
		if name := firstString(attrString(attrs, "neatlogs.agent.name"), nodeName(node, kind)); name != "" {
			extra = append(extra, attribute.String("neatlogs.agent.name", name))
		}
		if role := attrString(attrs, "neatlogs.agent.role"); role != "" {
			extra = append(extra, attribute.String("neatlogs.agent.role", role))
		}
		extra = appendIO(extra, node)
	case "embedding":
		if model := firstString(str(node["model"]), attrString(attrs, "neatlogs.embedding.model_name")); model != "" {
			extra = append(extra, attribute.String("neatlogs.embedding.model_name", model))
		}
		extra = appendIO(extra, node)
	default:
		extra = appendIO(extra, node)
	}
	ctx, span, end := sdk.StartSpan(ctx, nodeName(node, kind), strings.ToUpper(kind), extra...)
	defer end()
	markStatus(span, node)
	addLogs(span, node)
	n, err := emitChildren(ctx, node)
	if err != nil {
		return n, err
	}
	return n + 1, nil
}

func appendIO(extra []attribute.KeyValue, node map[string]any) []attribute.KeyValue {
	if in := text(node["input"]); in != "" {
		extra = append(extra, attribute.String("neatlogs.input.value", in))
	}
	if out := text(node["output"]); out != "" {
		extra = append(extra, attribute.String("neatlogs.output.value", out))
	}
	return extra
}

func identity(node map[string]any) sdk.IdentifyOptions {
	attrs := attrMap(node)
	meta := map[string]any{}
	if raw := attrString(attrs, "neatlogs.end_user.metadata"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &meta); err != nil {
			meta = map[string]any{"raw": raw}
		}
	}
	return sdk.IdentifyOptions{
		SessionID:       attrString(attrs, "neatlogs.session.id"),
		ParentSessionID: attrString(attrs, "neatlogs.session.parent_id"),
		EndUserID:       attrString(attrs, "neatlogs.end_user.id"),
		EndUserMetadata: meta,
	}
}

func llmMessages(node map[string]any, attrs map[string]any) []sdk.LLMMessage {
	var msgs []sdk.LLMMessage
	if prompt := attrString(attrs, "neatlogs.llm.system_prompt"); prompt != "" {
		msgs = append(msgs, sdk.LLMMessage{Role: "system", Content: prompt})
	}
	switch input := node["input"].(type) {
	case []any:
		for _, item := range input {
			m, ok := item.(map[string]any)
			if !ok {
				if s := text(item); s != "" {
					msgs = append(msgs, sdk.LLMMessage{Role: "user", Content: s})
				}
				continue
			}
			msgs = append(msgs, sdk.LLMMessage{
				Role:    firstString(str(m["role"]), "user"),
				Content: firstString(str(m["content"]), text(m["text"])),
			})
		}
	default:
		if s := text(input); s != "" {
			msgs = append(msgs, sdk.LLMMessage{Role: "user", Content: s})
		}
	}
	return msgs
}

func tokenCounts(node map[string]any, attrs map[string]any) (prompt, completion, total int) {
	if tok, ok := node["tokens"].(map[string]any); ok {
		prompt, _ = asInt(tok["prompt"])
		completion, _ = asInt(tok["completion"])
		total, _ = asInt(tok["total"])
	}
	if prompt == 0 {
		prompt, _ = attrInt(attrs, "neatlogs.llm.token_count.prompt")
	}
	if completion == 0 {
		completion, _ = attrInt(attrs, "neatlogs.llm.token_count.completion")
	}
	if total == 0 {
		total, _ = attrInt(attrs, "neatlogs.llm.token_count.total")
	}
	if total == 0 && (prompt != 0 || completion != 0) {
		total = prompt + completion
	}
	return prompt, completion, total
}

func kindOf(node map[string]any) string {
	switch strings.ToUpper(strings.TrimSpace(str(node["kind"]))) {
	case "", "WORKFLOW":
		return "workflow"
	case "LLM":
		return "llm"
	case "TOOL":
		return "tool"
	case "MCP_TOOL":
		return "mcp_tool"
	case "AGENT":
		return "agent"
	case "CHAIN":
		return "chain"
	case "RETRIEVER":
		return "retriever"
	case "EMBEDDING":
		return "embedding"
	case "RERANKER":
		return "reranker"
	case "GUARDRAIL":
		return "guardrail"
	case "VECTOR_STORE":
		return "vector_store"
	case "TASK":
		return "task"
	case "EVALUATOR":
		return "evaluator"
	case "MEMORY":
		return "memory"
	case "LOG":
		return "log"
	case "HTTP":
		return "http"
	default:
		return strings.ToLower(strings.TrimSpace(str(node["kind"])))
	}
}

func nodeName(node map[string]any, fallback string) string {
	if name := strings.TrimSpace(str(node["name"])); name != "" {
		return name
	}
	if name := strings.TrimSpace(str(node["tool_name"])); name != "" {
		return name
	}
	return fallback
}

func markStatus(span trace.Span, node map[string]any) {
	msg, failed := failure(node)
	if !failed || span == nil {
		return
	}
	span.RecordError(errors.New(msg))
	span.SetStatus(codes.Error, msg)
}

func failure(node map[string]any) (string, bool) {
	status := strings.ToUpper(strings.TrimSpace(str(node["status"])))
	msg := strings.TrimSpace(str(node["error"]))
	if status != "ERROR" && msg == "" {
		return "", false
	}
	if msg == "" {
		msg = "error"
	}
	return msg, true
}

func addLogs(span trace.Span, node map[string]any) {
	logs, _ := node["logs"].([]any)
	for _, item := range logs {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		msg := str(m["message"])
		if msg == "" {
			continue
		}
		span.AddEvent(msg, trace.WithAttributes(attribute.String("level", str(m["level"]))))
	}
}

func attrMap(node map[string]any) map[string]any {
	m, _ := node["attributes"].(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func attrString(attrs map[string]any, key string) string {
	return strings.TrimSpace(str(attrs[key]))
}

func attrBool(attrs map[string]any, key string) bool {
	switch v := attrs[key].(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes":
			return true
		}
	}
	return false
}

func attrInt(attrs map[string]any, key string) (int, bool) {
	return asInt(attrs[key])
}

func attrFloat(attrs map[string]any, key string) (float64, bool) {
	switch n := attrs[key].(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			f, ferr := n.Float64()
			if ferr != nil {
				return 0, false
			}
			return int(f), true
		}
		return int(i), true
	case float64:
		return int(n), true
	case int:
		return n, true
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		return i, err == nil
	default:
		return 0, false
	}
}

func text(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func valueCount(v any) int {
	switch t := v.(type) {
	case []any:
		return len(t)
	case nil:
		return 0
	default:
		return 1
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func firstString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
