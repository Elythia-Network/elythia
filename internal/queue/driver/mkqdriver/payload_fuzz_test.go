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
// **queue が付ける driver.WithVerbatimJSONPayload の印の裏付けもここで取る。**
// 印の付いた payload は検査せずに埋め込むので、配送・受信の payload がどの形でも
// encoding/json と同じ書き方であることが要る。書き方が違えば、埋め込んだ本文が
// mkq の json.Marshal で書き直され、読み戻したバイト列が変わって落ちる。
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
		dp := queue.DeliverPayload{Inbox: "https://remote.example/inbox?a=1&b=<x>", Body: body, KeyID: "k&<>", SignerUserID: "u1"}
		deliverCompact, err := dp.MarshalCompactJSON()
		if err != nil {
			t.Fatalf("deliver MarshalCompactJSON: %v", err)
		}
		deliverCurrent, err := dp.MarshalJSON()
		if err != nil {
			t.Fatalf("deliver MarshalJSON: %v", err)
		}
		for name, payload := range map[string][]byte{"current": current, "compact": compact} {
			for _, frame := range []func(string, []byte) framedPayload{newFramedPayload, compactFramedPayload, trustedFramedPayload} {
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

		// 配送の payload も、印を付けて埋め込んだまま元のバイト列に戻ること。
		for name, payload := range map[string][]byte{"deliver current": deliverCurrent, "deliver compact": deliverCompact} {
			data, err := json.Marshal(trustedFramedPayload(queue.TaskTypeDeliver, payload))
			if err != nil {
				t.Fatalf("%s: a marked payload is not valid JSON: %v", name, err)
			}
			var back framedPayload
			if err := json.Unmarshal(data, &back); err != nil {
				t.Fatalf("%s: unmarshal data: %v", name, err)
			}
			if !bytes.Equal(back.payload(), payload) {
				t.Fatalf("%s: a marked payload changed:\n got %q\nwant %q", name, back.payload(), payload)
			}
			got, err := queue.DecodeDeliverPayload(back.payload())
			if err != nil {
				t.Fatalf("%s: decode: %v", name, err)
			}
			if !bytes.Equal(got.Body, body) || (got.Body == nil) != (body == nil) {
				t.Fatalf("%s: body changed:\n got %q\nwant %q", name, got.Body, body)
			}
		}
	})
}

// FuzzFramedPayloadRoundTrip frames an arbitrary task payload (plugins may
// enqueue any bytes) in every unmarked writer shape, stores it the way mkq
// stores job data, and checks that the worker reads the same bytes back.
//
// 包みの側は、本文と違ってプラグインが任意のバイト列を積める。埋め込めないものは
// 以前の形に戻すので、どんな入力でも読み戻したバイト列が一致すること。種
// (testdata/fuzz/FuzzFramedPayloadRoundTrip) は FuzzActivityBodyRoundTrip と同じ
// 壊れ方に、包みの深さの境目 (driver.MaxFramedJSONDepth) と、オブジェクトでない
// JSON を足したもの。広く回すときは
// `go test -run '^$' -fuzz FuzzFramedPayloadRoundTrip ./internal/queue/driver/mkqdriver/`。
func FuzzFramedPayloadRoundTrip(f *testing.F) {
	f.Fuzz(func(t *testing.T, payload []byte) {
		for name, frame := range map[string]func(string, []byte) framedPayload{
			"current": newFramedPayload,
			"compact": compactFramedPayload,
		} {
			var data []byte
			err := addFramed(frame("plugin:x", payload), "plugin:x", payload, func(fp framedPayload) error {
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
			if back.Type != "plugin:x" {
				t.Fatalf("%s: type changed: %q", name, back.Type)
			}
			if !bytes.Equal(back.payload(), payload) {
				t.Fatalf("%s: payload changed:\n got %q\nwant %q", name, back.payload(), payload)
			}
		}
	})
}
