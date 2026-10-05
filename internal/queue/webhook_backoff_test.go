package queue

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// recordingClient is a driver.Client that captures the enqueue options of the
// last call so option-building can be asserted without a live Redis backend.
type recordingClient struct {
	lastOpts []driver.EnqueueOption
}

func (r *recordingClient) Enqueue(_ context.Context, _ string, _ []byte, opts ...driver.EnqueueOption) error {
	r.lastOpts = opts
	return nil
}

func (r *recordingClient) Close() error { return nil }

// TestEnqueueWebhookAppliesFederationBackoff verifies that both webhook enqueue
// paths attach the custom federation backoff option, matching deliver/inbox (#1408).
func TestEnqueueWebhookAppliesFederationBackoff(t *testing.T) {
	tests := []struct {
		name    string
		enqueue func(*Client) error
	}{
		{
			name:    "user webhook",
			enqueue: func(c *Client) error { return c.EnqueueUserWebhook(context.Background(), WebhookPayload{}) },
		},
		{
			name:    "system webhook",
			enqueue: func(c *Client) error { return c.EnqueueSystemWebhook(context.Background(), WebhookPayload{}) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recordingClient{}
			c := &Client{inner: rec}
			if err := tt.enqueue(c); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			got := driver.ApplyEnqueueOptions(rec.lastOpts)
			if got.BackoffType != driver.BackoffCustom {
				t.Fatalf("expected custom backoff, got %q", got.BackoffType)
			}
		})
	}
}

// withCapturedSlog installs a text handler writing into the returned buffer as
// the default logger for the duration of the test, restoring the previous
// logger via t.Cleanup.
func withCapturedSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// TestBackoffOptFromPolicyWarnsOnMissingDelay verifies that a built-in backoff
// type without a positive delay emits a warning and falls back to the federation
// backoff, while valid policies stay silent (#1409).
func TestBackoffOptFromPolicyWarnsOnMissingDelay(t *testing.T) {
	tests := []struct {
		name     string
		policy   Policy
		wantWarn bool
		wantType string
	}{
		{
			name:     "built-in exponential without delay warns and falls back",
			policy:   Policy{BackoffType: driver.BackoffExponential},
			wantWarn: true,
			wantType: driver.BackoffCustom,
		},
		{
			name:     "built-in fixed without delay warns and falls back",
			policy:   Policy{BackoffType: driver.BackoffFixed},
			wantWarn: true,
			wantType: driver.BackoffCustom,
		},
		{
			name:     "custom backoff does not warn",
			policy:   Policy{BackoffType: driver.BackoffCustom},
			wantWarn: false,
			wantType: driver.BackoffCustom,
		},
		{
			name:     "built-in with positive delay does not warn",
			policy:   Policy{BackoffType: driver.BackoffExponential, BackoffDelay: 2 * time.Second},
			wantWarn: false,
			wantType: driver.BackoffExponential,
		},
		{
			name:     "empty policy does not warn",
			policy:   Policy{},
			wantWarn: false,
			wantType: driver.BackoffCustom,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := withCapturedSlog(t)
			got := driver.ApplyEnqueueOptions([]driver.EnqueueOption{backoffOptFromPolicy(tt.policy)})
			if got.BackoffType != tt.wantType {
				t.Fatalf("BackoffType = %q, want %q", got.BackoffType, tt.wantType)
			}
			warned := strings.Contains(buf.String(), "built-in backoff type specified without a positive delay")
			if warned != tt.wantWarn {
				t.Fatalf("warn = %v, want %v (log: %q)", warned, tt.wantWarn, buf.String())
			}
		})
	}
}
