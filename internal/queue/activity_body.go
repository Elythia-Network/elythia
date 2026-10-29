package queue

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"sync"
	"unicode/utf8"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// The activity carried by deliver and inbox payloads (DeliverPayload.Body /
// InboxPayload.Body) is a []byte in Go and takes one of four shapes on the
// wire:
//
//   - "body": <JSON object> — the activity embedded as is, when it is
//     written byte for byte (driver.EmbedVerbatim; what json.Marshal
//     produces, so every delivery mk-go renders)
//   - "bodyEscaped": <JSON object> — the activity embedded with encoding/json
//     having escaped its <, > and &, which the reader turns back
//     (driver.EmbedHTMLEscaped; Misskey's JSON.stringify leaves < raw)
//   - "bodyText": "<string>" — the activity as a JSON string, when it is
//     valid UTF-8 but cannot be embedded (whitespace between tokens, nesting
//     too deep, not JSON) and the string is not larger than base64
//   - "body": "<base64>" — anything else, every job written before #3614,
//     and every job written while driver.CompactJobPayloads is disabled
//
// base64 の形だけでは、外側の framing (mkqdriver.framedPayload) でもう一度
// base64 になるため、Redis に本文の約 1.86 倍で載る (#3614)。
//
// **本文はバイト列として厳密に戻す。** 受信の本文は受付で Digest と照合した
// バイト列で、worker はそれをそのまま処理する (LD 署名の検証も同じ本文で行う)。
// 意味が同じでも、別のバイト列を渡さない。埋め込みは元に戻せる値に限り、文字列の
// 形は正しい UTF-8 に限る (encoding/json は不正なバイトを U+FFFD に置き換える)。
// どれにも当てはまらなければ従来の base64 で運ぶ。
//
// 文字列の形は最後の手段にしている。日本語や引用符の多い本文を JSON の文字列に
// すると、引用符を 1 つずつエスケープするぶん大きくなり、書くのも読むのも埋め込み
// より遅い。
//
// **書く側はまだ有効にしていない** (driver.CompactJobPayloads。#3614 は読む側を
// 先に出す 2 段)。MarshalJSON は有効になるまで以前の形を書く。
//
// 読む側は 4 つの形をすべて受け付ける。再送を待つ job は Redis に約 32 時間
// (落ちた配送先へのブレーカーで待たされる job はもっと長く) 残るので、更新の前に
// 積まれた base64 の job もそのまま処理できる。

// activityBodyWire holds the wire fields of the activity, for reading.
type activityBodyWire struct {
	Body        json.RawMessage `json:"body,omitempty"`
	BodyEscaped json.RawMessage `json:"bodyEscaped,omitempty"`
	BodyText    *string         `json:"bodyText,omitempty"`
}

// activityBodyField is the wire key and the JSON value of the activity, ready
// to be appended to the rest of the payload.
type activityBodyField struct {
	key   string
	value []byte
}

// encodeActivityBody picks the most compact shape for b.
//
// **埋め込む値は json.Marshal に通さず、自分で検査して継ぎ足す**
// (appendActivityBody)。json.RawMessage を json.Marshal に通すと、文字列を
// 1 文字ずつ書き直す (Go 1.27 の encoding/json)。日本語の多い本文では
// json.Valid の約 3.6 倍かかった (73KB で 755µs 対 208µs、#3614 の測定)。job の
// data は mkq がもう一度 json.Marshal するので、書き直しはそちらの 1 回だけにする。
func encodeActivityBody(b []byte) activityBodyField {
	// オブジェクトに限る。JSON の文字列をそのまま埋め込むと、読む側で以前の
	// 形 (base64 の文字列) と区別できない。
	if len(b) > 0 && b[0] == '{' {
		switch driver.ClassifyJSONEmbedding(b) {
		case driver.EmbedVerbatim:
			if json.Valid(b) {
				return activityBodyField{"body", b}
			}
		case driver.EmbedHTMLEscaped:
			// <, >, & は 1 文字が 6 バイトになる。HTML ばかりの本文では base64
			// (1.33 倍) の方が小さいので、そのときは下へ回す。
			if htmlEscapedLen(b) <= base64.StdEncoding.EncodedLen(len(b)) && json.Valid(b) {
				return activityBodyField{"bodyEscaped", driver.EscapeJSONHTML(b)}
			}
		}
	}
	return compactOrLegacyActivityBody(b)
}

// deliverBodyMemo remembers the shape picked for the last deliver activity.
//
// **1 つの投稿は inbox の数だけ同じ本文で積まれる** (DeliverService の宛先の
// ループ)。形の判定は本文の全体を 2 回走査するので、宛先ごとに繰り返すと 73KB の
// 本文で 1 件あたり約 250µs かかる (#3614)。直前の 1 件だけを覚え、中身が同じなら
// 判定を使い回す。
//
// **中身で照合し、本文は複製して持つ。** 同じ配列が後から書き換えられても、
// 覚えた値は複製から作ったものなので、別の本文と取り違えない。照合は memcmp で、
// 判定より桁で速い。並行して別の投稿を配ると交互に外れるが、そのときは判定し直す
// だけで、結果は変わらない。
var deliverBodyMemo struct {
	mu    sync.Mutex
	body  []byte
	field activityBodyField
}

// encodeDeliverActivityBody is encodeActivityBody for deliver payloads,
// reusing the result for the same activity (deliverBodyMemo).
func encodeDeliverActivityBody(b []byte) activityBodyField {
	// nil (null) と空 (文字列の "") は書く形が違うので、空は覚えない。
	if len(b) == 0 {
		return encodeActivityBody(b)
	}
	deliverBodyMemo.mu.Lock()
	if bytes.Equal(deliverBodyMemo.body, b) {
		f := deliverBodyMemo.field
		deliverBodyMemo.mu.Unlock()
		return f
	}
	deliverBodyMemo.mu.Unlock()

	own := bytes.Clone(b)
	f := encodeActivityBody(own)
	deliverBodyMemo.mu.Lock()
	deliverBodyMemo.body, deliverBodyMemo.field = own, f
	deliverBodyMemo.mu.Unlock()
	return f
}

// compactOrLegacyActivityBody returns the string shape when it is valid and
// no larger than base64, and the legacy base64 shape otherwise.
func compactOrLegacyActivityBody(b []byte) activityBodyField {
	// 文字列にすると <, >, & は 6 バイト、" は 2 バイトになる。HTML の多い
	// 本文では base64 (1.33 倍) より大きくなるので、小さい方を選ぶ。
	if b != nil && utf8.Valid(b) && quotedLen(b) <= base64.StdEncoding.EncodedLen(len(b)) {
		enc, _ := json.Marshal(string(b)) // 正しい UTF-8 の文字列の Marshal は失敗しない
		return activityBodyField{"bodyText", enc}
	}
	// 以前の形 (base64。nil は null)。
	enc, _ := json.Marshal(b) // []byte の Marshal は失敗しない
	return activityBodyField{"body", enc}
}

// appendActivityBody appends the activity field to head, the JSON object of
// the other payload fields.
func appendActivityBody(head []byte, f activityBodyField) []byte {
	// 容量を足し算で求めると CodeQL が overflow を指摘する (go/allocation-size-overflow)。
	// 本文の大きさは受付の上限で抑えられているが、append の伸長に任せる。
	out := make([]byte, 0, len(head))
	out = append(out, head[:len(head)-1]...)
	if len(head) > 2 {
		out = append(out, ',')
	}
	out = append(out, '"')
	out = append(out, f.key...)
	out = append(out, '"', ':')
	out = append(out, f.value...)
	return append(out, '}')
}

// htmlEscapedLen returns the length of b once encoding/json has escaped its
// <, >, &, U+2028 and U+2029.
func htmlEscapedLen(b []byte) int {
	n := len(b)
	for i := 0; i < len(b); i++ {
		switch c := b[i]; {
		case c == '<' || c == '>' || c == '&':
			n += 5
		case c == 0xE2 && i+2 < len(b) && b[i+1] == 0x80 && (b[i+2] == 0xA8 || b[i+2] == 0xA9):
			n += 3
			i += 2
		}
	}
	return n
}

// quotedLen returns the length of b written as a JSON string by
// encoding/json (with HTML escaping), excluding the surrounding quotes.
// b must be valid UTF-8.
func quotedLen(b []byte) int {
	n := len(b)
	for i := 0; i < len(b); i++ {
		switch c := b[i]; {
		case c == '"' || c == '\\' || c == '\n' || c == '\r' || c == '\t' || c == '\b' || c == '\f':
			n++
		case c < 0x20 || c == '<' || c == '>' || c == '&':
			n += 5
		case c == 0xE2 && i+2 < len(b) && b[i+1] == 0x80 && (b[i+2] == 0xA8 || b[i+2] == 0xA9):
			// U+2028 / U+2029 (3 バイト) は \u2028 (6 バイト) になる。
			n += 3
			i += 2
		}
	}
	return n
}

// decode returns the activity bytes from any of the wire shapes.
func (w activityBodyWire) decode() ([]byte, error) {
	if len(w.BodyEscaped) > 0 && string(w.BodyEscaped) != "null" {
		return driver.UnescapeJSONHTML(w.BodyEscaped), nil
	}
	if w.BodyText != nil {
		return []byte(*w.BodyText), nil
	}
	if len(w.Body) == 0 || string(w.Body) == "null" {
		return nil, nil
	}
	if w.Body[0] == '"' {
		// 以前の形 (base64)。
		var b []byte
		if err := json.Unmarshal(w.Body, &b); err != nil {
			return nil, err
		}
		return b, nil
	}
	return []byte(w.Body), nil
}

// deliverPayloadPlain is DeliverPayload without its JSON methods.
type deliverPayloadPlain DeliverPayload

// inboxPayloadPlain is InboxPayload without its JSON methods.
type inboxPayloadPlain InboxPayload

// 埋め込んだ plain の Body (key "body") は、浅い階層に置いた wire の Body が
// 隠す。activityBodyWire を埋め込むと同じ深さで key がぶつかり、両方とも
// 消える (encoding/json の規則) ので、field を直接並べる。
type deliverPayloadWire struct {
	deliverPayloadPlain
	Body        json.RawMessage `json:"body,omitempty"`
	BodyEscaped json.RawMessage `json:"bodyEscaped,omitempty"`
	BodyText    *string         `json:"bodyText,omitempty"`
}

type deliverPayloadWireIn struct {
	*deliverPayloadPlain
	Body        json.RawMessage `json:"body"`
	BodyEscaped json.RawMessage `json:"bodyEscaped"`
	BodyText    *string         `json:"bodyText"`
}

type inboxPayloadWire struct {
	inboxPayloadPlain
	Body        json.RawMessage `json:"body,omitempty"`
	BodyEscaped json.RawMessage `json:"bodyEscaped,omitempty"`
	BodyText    *string         `json:"bodyText,omitempty"`
}

type inboxPayloadWireIn struct {
	*inboxPayloadPlain
	Body        json.RawMessage `json:"body"`
	BodyEscaped json.RawMessage `json:"bodyEscaped"`
	BodyText    *string         `json:"bodyText"`
}

// MarshalJSON writes the payload in the shape selected by
// driver.CompactJobPayloads: the #3614 shape (MarshalCompactJSON) when it
// is enabled, the pre-#3614 shape (the activity as base64) otherwise.
func (p DeliverPayload) MarshalJSON() ([]byte, error) {
	if driver.CompactJobPayloads {
		return p.MarshalCompactJSON()
	}
	// 以前の版と同じバイト列 ([]byte の Body は base64 になる)。
	return json.Marshal(deliverPayloadPlain(p))
}

// MarshalCompactJSON writes the payload with the activity in its most
// compact wire shape (#3614), regardless of driver.CompactJobPayloads.
// MarshalJSON uses it once the writers are enabled; tests use it to pin the
// shape before that.
func (p DeliverPayload) MarshalCompactJSON() ([]byte, error) {
	// body を除いた field を書き (wire の body 系は nil なので出ない)、本文を
	// 継ぎ足す。
	head, err := json.Marshal(deliverPayloadWire{deliverPayloadPlain: deliverPayloadPlain(p)})
	if err != nil {
		return nil, err
	}
	return appendActivityBody(head, encodeDeliverActivityBody(p.Body)), nil
}

// UnmarshalJSON reads every wire shape of the activity, including the
// base64 one written before #3614.
func (p *DeliverPayload) UnmarshalJSON(b []byte) error {
	w := deliverPayloadWireIn{deliverPayloadPlain: (*deliverPayloadPlain)(p)}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	body, err := activityBodyWire{Body: w.Body, BodyEscaped: w.BodyEscaped, BodyText: w.BodyText}.decode()
	if err != nil {
		return err
	}
	p.Body = body
	return nil
}

// MarshalJSON writes the payload in the shape selected by
// driver.CompactJobPayloads: the #3614 shape (MarshalCompactJSON) when it
// is enabled, the pre-#3614 shape (the activity as base64) otherwise.
func (p InboxPayload) MarshalJSON() ([]byte, error) {
	if driver.CompactJobPayloads {
		return p.MarshalCompactJSON()
	}
	// 以前の版と同じバイト列 ([]byte の Body は base64 になる)。
	return json.Marshal(inboxPayloadPlain(p))
}

// MarshalCompactJSON writes the payload with the activity in its most
// compact wire shape (#3614), regardless of driver.CompactJobPayloads.
// MarshalJSON uses it once the writers are enabled; tests use it to pin the
// shape before that.
func (p InboxPayload) MarshalCompactJSON() ([]byte, error) {
	// body を除いた field を書き (wire の body 系は nil なので出ない)、本文を
	// 継ぎ足す。
	head, err := json.Marshal(inboxPayloadWire{inboxPayloadPlain: inboxPayloadPlain(p)})
	if err != nil {
		return nil, err
	}
	return appendActivityBody(head, encodeActivityBody(p.Body)), nil
}

// UnmarshalJSON reads every wire shape of the activity, including the
// base64 one written before #3614.
func (p *InboxPayload) UnmarshalJSON(b []byte) error {
	w := inboxPayloadWireIn{inboxPayloadPlain: (*inboxPayloadPlain)(p)}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	body, err := activityBodyWire{Body: w.Body, BodyEscaped: w.BodyEscaped, BodyText: w.BodyText}.decode()
	if err != nil {
		return err
	}
	p.Body = body
	return nil
}
