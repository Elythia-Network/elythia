// Package mkqdriver implements queue/driver against mkq
// (github.com/elythia-network/mkq), a BullMQ-compatible Go-native job queue.
//
// Wire format note: every job mkqdriver enqueues carries a small
// framing wrapper {type, bodyJson} ({type, body} when the payload cannot be
// embedded as JSON, #3614) as its mkq payload. The wrapper is
// what the worker dispatches on; the BullMQ Job.name field is set to
// the same task type for ad-hoc Add calls (so bull-board / Misskey
// admin UI sees the task type natively), but mkq schedule API does
// not currently allow overriding the per-fire job name and will use
// the queue name there. The dispatch logic ignores Job.name for that
// reason.
package mkqdriver

import (
	"encoding/json"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// framedPayload wraps the driver-level (taskType, []byte) tuple into
// a single value so mkq.Queue[framedPayload] can carry every flavour
// of mk-go job without per-task generic explosion.
//
// The payload is opaque bytes: callers may pass arbitrary payloads
// (including non-JSON test inputs). It is stored in one of two fields:
//
//   - BodyJSON ("bodyJson") holds JSON that survives being embedded
//     verbatim (driver.IsVerbatimJSON)
//   - Body ("body", base64) holds everything else, and every job
//     written before #3614
//
// Use newFramedPayload to build one and payload to read it back. The reader
// accepts both shapes, so jobs enqueued by an older process (retries stay in
// Redis for about 32 hours, longer when held by the delivery breaker) keep
// working after an upgrade. Writers use the BodyJSON shape while
// driver.CompactJobPayloads is enabled.
//
// Body (base64) だけでは、配送と受信の payload はそれ自体が JSON で、中の本文も
// すでに base64 なので、Redis に本文の約 1.86 倍で載る (#3614)。
//
// **新しい形は別の key (bodyJson) に置く。** body の値を文字列からオブジェクトに
// 変えると、古い版の Elythia が []byte として読もうとして job の組み立て
// (mkq の buildJob) ごと失敗し、管理画面の一覧も 1 件のせいで全体が読めなくなる。
// 別の key なら古い版は知らない key として無視し、本文の無い job として扱う
// (どうなるかは docs/divergence/config.md)。
//
// **JSON のメソッド (MarshalJSON / UnmarshalJSON) を持たせない。** mkq は data を
// json.Marshal / json.Unmarshal で読み書きするので、メソッドを挟むとその出力を
// encoding/json がもう一度走査する。field のままなら本文の走査は 1 回で済む。
type framedPayload struct {
	Type     string          `json:"type"`
	Body     []byte          `json:"body,omitempty"`
	BodyJSON json.RawMessage `json:"bodyJson,omitempty"`
}

// newFramedPayload frames payload for writing: the #3614 shape when
// driver.CompactJobPayloads is enabled, the pre-#3614 base64 shape otherwise.
//
// 書く側の切り替えはこの関数と queue の activity_body.go の 2 か所で、どちらも
// driver.CompactJobPayloads だけを見る (#3614 は 2 段に分けて出した)。
func newFramedPayload(taskType string, payload []byte) framedPayload {
	return framePayload(taskType, payload, false)
}

// framePayload is newFramedPayload for a payload the caller may have marked
// with driver.WithVerbatimJSONPayload.
func framePayload(taskType string, payload []byte, verbatim bool) framedPayload {
	if !driver.CompactJobPayloads {
		return legacyFramedPayload(taskType, payload)
	}
	if verbatim {
		return trustedFramedPayload(taskType, payload)
	}
	return compactFramedPayload(taskType, payload)
}

// trustedFramedPayload is compactFramedPayload without the byte-for-byte
// check, for a payload the caller marked with driver.WithVerbatimJSONPayload.
//
// 検査は飛ばすが、オブジェクトの形かどうかだけは見る (1 バイト目で足りる)。
// 約束が破れていて mkq の Marshal が失敗したときは、addFramed が以前の形に戻す。
func trustedFramedPayload(taskType string, payload []byte) framedPayload {
	if len(payload) > 0 && payload[0] == '{' {
		return framedPayload{Type: taskType, BodyJSON: json.RawMessage(payload)}
	}
	return compactFramedPayload(taskType, payload)
}

// compactFramedPayload frames payload in the #3614 shape, embedding it
// verbatim when it is JSON that round-trips byte for byte. The embedded shape
// is only a candidate: when mkq fails to marshal it (not valid JSON), retry
// with legacyFramedPayload (addFramed).
func compactFramedPayload(taskType string, payload []byte) framedPayload {
	// オブジェクトと配列に限る (task の payload はどれもオブジェクト)。"hello" の
	// ような文字列も形の検査は通るので、ここで外して Marshal の失敗を減らす。
	// 深さは本文の上限より数段多く見る (driver.MaxFramedJSONDepth の doc)。
	if len(payload) > 0 && (payload[0] == '{' || payload[0] == '[') &&
		driver.IsVerbatimJSONWithin(payload, driver.MaxFramedJSONDepth) {
		return framedPayload{Type: taskType, BodyJSON: json.RawMessage(payload)}
	}
	return legacyFramedPayload(taskType, payload)
}

// addFramed hands framed to add, and retries with the legacy shape when add
// fails because the embedded payload turned out not to be valid JSON.
//
// 文法の検査は mkq の json.Marshal に任せている (driver.ClassifyJSONEmbedding)
// ので、埋め込みの候補が JSON として読めなければ、ここで以前の形に戻す。Marshal は
// Redis に触る前に失敗するので、二重に積むことは無い。
func addFramed(framed framedPayload, taskType string, payload []byte, add func(framedPayload) error) error {
	err := add(framed)
	if err != nil && framed.embedded() && driver.IsInvalidRawJSON(err) {
		return add(legacyFramedPayload(taskType, payload))
	}
	return err
}

// legacyFramedPayload frames payload in the pre-#3614 base64 shape.
func legacyFramedPayload(taskType string, payload []byte) framedPayload {
	return framedPayload{Type: taskType, Body: payload}
}

// embedded reports whether f carries its payload in BodyJSON.
func (f framedPayload) embedded() bool {
	return len(f.BodyJSON) > 0
}

// payload returns the framed bytes from either shape.
func (f framedPayload) payload() []byte {
	// null は「無い」と同じに扱う (RawMessage は null をそのまま持つ)。
	if len(f.BodyJSON) > 0 && string(f.BodyJSON) != "null" {
		return f.BodyJSON
	}
	return f.Body
}
