package mkqdriver

import (
	"context"
	"errors"
	"fmt"

	"github.com/elythia-network/mkq"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// Client implements driver.Client over a Driver's pre-defined
// *mkq.Queue handles.
type Client struct {
	driver *Driver
}

// Enqueue routes (taskType, payload) to the mkq.Queue named by
// opts.WithQueue. Without an explicit Queue the call returns an error:
// every queue must be Define'd ahead of time, so there is no "default"
// routing to fall back on.
//
// Unique-key dedup hits surface as ErrDuplicateJob from mkq; we
// translate that into a successful Enqueue (the caller asked for
// at-most-once within the window and got it).
func (c *Client) Enqueue(ctx context.Context, taskType string, payload []byte, opts ...driver.EnqueueOption) error {
	o := driver.ApplyEnqueueOptions(opts)
	if o.Queue == "" {
		return fmt.Errorf("mkqdriver: Enqueue requires WithQueue (taskType=%q)", taskType)
	}
	q := c.driver.queueFor(o.Queue)
	if q == nil {
		return fmt.Errorf("mkqdriver: unknown queue %q (taskType=%q)", o.Queue, taskType)
	}

	framed := framedPayload{Type: taskType, Body: payload}
	addOpts := toMkqAddOptions(o, taskType, payload)
	if _, err := q.Add(ctx, framed, addOpts...); err != nil {
		if errors.Is(err, mkq.ErrDuplicateJob) {
			// TTL 内の重複 enqueue は black-hole 扱い。
			return nil
		}
		return err
	}
	return nil
}

// Close is a no-op at the Client level — the underlying *mkq.Client
// is owned by the parent Driver, which owns the connection pool.
func (c *Client) Close() error { return nil }
