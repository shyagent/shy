// Package neatlogs replays a shy trace tree through the Neatlogs Go SDK.
//
// shy still posts one nested JSON tree to POST /v1/trace. This package turns
// that tree into SDK spans and exports them over OTLP. The Gemini and ADK
// wrappers are not used: this process does not call those libraries.
package neatlogs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	sdk "github.com/neatlogs/neatlogs-go"
)

// ErrNotConfigured means the server has no Neatlogs key. The trace is stored
// and not forwarded.
var ErrNotConfigured = errors.New("neatlogs is not configured")

// Forwarder sends a prepared trace body.
type Forwarder interface {
	Forward(ctx context.Context, body []byte) (traceID string, spans int, err error)
}

// Disabled stores traces locally and does not call Neatlogs.
type Disabled struct{}

func (Disabled) Forward(context.Context, []byte) (string, int, error) {
	return "", 0, ErrNotConfigured
}

// Client is one long-lived Neatlogs pipeline for this process.
type Client struct {
	mu     sync.Mutex
	sdk    *sdk.Client
	closed bool
}

// Open builds the pipeline. endpoint is the ingest base URL, without /v1/traces.
// project, when set, is attached as a resource tag. The SDK selects the
// Neatlogs project from apiKey (x-api-key); it has no separate project field.
func Open(ctx context.Context, apiKey, endpoint, project string, opts ...sdk.Option) (*Client, error) {
	cfg := sdk.Config{
		APIKey:        apiKey,
		Endpoint:      endpoint,
		WorkflowName:  "shyd",
		EnableUploads: true,
	}
	if tag := projectTag(project); tag != "" {
		cfg.Tags = []string{tag}
	}
	client, err := sdk.NewClient(ctx, cfg, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{sdk: client}, nil
}

func projectTag(project string) string {
	project = strings.TrimSpace(project)
	if project == "" {
		return ""
	}
	return "project=" + strings.ReplaceAll(project, ",", "_")
}

// Forward replays body and waits until the SDK has exported it.
func (c *Client) Forward(ctx context.Context, body []byte) (string, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.sdk == nil {
		return "", 0, ErrNotConfigured
	}
	before := c.sdk.DeliveryDiagnostics()
	traceID, spans, err := replay(sdk.WithClient(ctx, c.sdk), body)
	if err != nil {
		return "", 0, err
	}
	if err := c.sdk.Flush(ctx); err != nil {
		return "", 0, err
	}
	after := c.sdk.DeliveryDiagnostics()
	if after.SpanExportFailures > before.SpanExportFailures ||
		after.SpanQueueDrops > before.SpanQueueDrops ||
		after.OTLPOverflowFailures > before.OTLPOverflowFailures ||
		after.OTLPOverflowUnavailable > before.OTLPOverflowUnavailable {
		return "", 0, fmt.Errorf("neatlogs export failed")
	}
	return traceID, spans, nil
}

// Close flushes and releases the pipeline. The host owns process signals;
// call this from the server shutdown path.
func (c *Client) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.sdk == nil {
		return nil
	}
	return c.sdk.Shutdown(ctx)
}
