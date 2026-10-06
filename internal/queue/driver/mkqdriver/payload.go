// Package mkqdriver implements queue/driver against mkq
// (github.com/elythia-network/mkq), a BullMQ-compatible Go-native job queue.
//
// Wire format note: every job mkqdriver enqueues carries a small
// framing wrapper {type, body} as its mkq payload. The wrapper is
// what the worker dispatches on; the BullMQ Job.name field is set to
// the same task type for ad-hoc Add calls (so bull-board / Misskey
// admin UI sees the task type natively), but mkq schedule API does
// not currently allow overriding the per-fire job name and will use
// the queue name there. The dispatch logic ignores Job.name for that
// reason.
package mkqdriver

// framedPayload wraps the driver-level (taskType, []byte) tuple into
// a single value so mkq.Queue[framedPayload] can carry every flavour
// of mk-go job without per-task generic explosion.
//
// Body is plain []byte rather than json.RawMessage because the driver
// contract is opaque bytes — callers may pass arbitrary payloads
// (including non-JSON test inputs). Go's encoding/json marshals
// []byte as a base64 string automatically, so the wire format stays
// valid JSON without forcing payload validity on callers.
type framedPayload struct {
	Type string `json:"type"`
	Body []byte `json:"body,omitempty"`
}
