package mkqdriver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/elythia-network/elythia/internal/queue"
)

// FuzzActivityBodyRoundTrip passes an inbox activity through every writer
// shape (the payload as this release writes it, and the #3614 compact shape),
// frames it the way mkq stores job data, and checks that the worker reads the
// same bytes back.
//
// **encoding/json の json.RawMessage の書き方に依存している** (空白を詰める・
// HTML の文字をエスケープする以外は書き直さない)。Go の版を上げてそれが変わった
// ときに落ちるよう、種 (testdata/fuzz/FuzzActivityBodyRoundTrip) には #3614 の
// レビューで試した壊れ方 (エスケープ列・数値の表記・重複したキー・NUL・
// U+2028 / U+2029・BOM・空白・壊れた JSON・深さの境目) を入れてある。CI では種
// だけが走る。広く回すときは
// `go test -run '^$' -fuzz FuzzActivityBodyRoundTrip ./internal/queue/driver/mkqdriver/`。
func FuzzActivityBodyRoundTrip(f *testing.F) {
	f.Fuzz(func(t *testing.T, body []byte) {
		ip := queue.InboxPayload{
			Body:    body,
			Method:  "POST",
			Path:    "/inbox?a=1&b=<x>",
			Headers: map[string]string{"Digest": "SHA-256=x", "Signature": `keyId="https://remote.example/u#k&<>"`},
		}
		current, err := ip.MarshalJSON()
		if err != nil {
			t.Fatalf("MarshalJSON: %v", err)
		}
		compact, err := ip.MarshalCompactJSON()
		if err != nil {
			t.Fatalf("MarshalCompactJSON: %v", err)
		}
		for name, payload := range map[string][]byte{"current": current, "compact": compact} {
			for _, frame := range []func(string, []byte) framedPayload{newFramedPayload, compactFramedPayload} {
				// mkq と同じく data を json.Marshal し、失敗は包んで返す。
				var data []byte
				err := addFramed(frame(queue.TaskTypeInbox, payload), queue.TaskTypeInbox, payload, func(fp framedPayload) error {
					b, err := json.Marshal(fp)
					if err != nil {
						return fmt.Errorf("mkq: marshal payload: %w", err)
					}
					data = b
					return nil
				})
				if err != nil {
					t.Fatalf("%s: add: %v", name, err)
				}
				var back framedPayload
				if err := json.Unmarshal(data, &back); err != nil {
					t.Fatalf("%s: unmarshal data: %v", name, err)
				}
				if !bytes.Equal(back.payload(), payload) {
					t.Fatalf("%s: payload changed:\n got %q\nwant %q", name, back.payload(), payload)
				}
				got, err := queue.DecodeInboxPayload(back.payload())
				if err != nil {
					t.Fatalf("%s: decode: %v", name, err)
				}
				if !bytes.Equal(got.Body, body) || (got.Body == nil) != (body == nil) {
					t.Fatalf("%s: body changed:\n got %q\nwant %q", name, got.Body, body)
				}
				if got.Path != ip.Path || got.Method != ip.Method || got.Headers["Signature"] != ip.Headers["Signature"] {
					t.Fatalf("%s: fields changed: %+v", name, got)
				}
			}
		}
	})
}
