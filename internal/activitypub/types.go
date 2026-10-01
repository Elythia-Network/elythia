// Package activitypub provides ActivityStreams types and the helpers for
// rendering local model entities into AP-compatible JSON-LD documents.
package activitypub

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// jsonNullLiteral is the byte representation of the JSON null literal.
// Pre-declared so Image.UnmarshalJSON can use bytes.Equal without an
// allocation on every call (= avoids string(data) heap conversion).
var jsonNullLiteral = []byte("null")

// TypeOf extracts the AS `type` from an already-decoded JSON value, mirroring
// upstream's getApType (a bare string, or the first element of an array).
//
// 生 fetch した body を `map[string]any` で覗く経路 (`api/ap/show` など) 用。
// struct 経由なら `APType` が同じことをする。
func TypeOf(raw any) string {
	switch t := raw.(type) {
	case string:
		return t
	case []any:
		if len(t) > 0 {
			if s, ok := t[0].(string); ok {
				return s
			}
		}
	}
	return ""
}

// APType holds an ActivityStreams `type`, which may arrive as a string or as
// an array of strings. It mirrors upstream's getApType.
//
// JSON-LD の `@type` は配列表現が正規で、compaction 後も配列で残る実装がある。
// `string` 決め打ちだと `"type": ["Person"]` で **document ごと unmarshal に
// 失敗し、その actor / Note がまったく取り込めない** (#2662)。同じ罠は
// core/federation の apTypeOf が attachment 側で既に潰している。
type APType string

// UnmarshalJSON never fails; unreadable shapes yield the empty string.
//
// upstream getApType は `typeof value.type === 'string'` か
// `Array.isArray(value.type) && typeof value.type[0] === 'string'` のときだけ
// 値を返し、それ以外は null。**配列の先頭が string でなければ諦める**ので
// ここも同じにする (走査して最初の string を拾うと upstream が型判定不能と
// する document を受け入れてしまう)。型判定ができないときにエラーではなく
// null を返す設計 (misskey-dev/misskey#14239) に合わせて空文字にし、
// 下流の type 判定に委ねる。
func (v *APType) UnmarshalJSON(data []byte) error {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		*v = ""
		return nil
	}
	switch t := raw.(type) {
	case string:
		*v = APType(t)
	case []any:
		// **先頭が string でなければ空。** 走査して「最初に見つかった string」を
		// 採ると upstream より緩くなり、upstream が型判定不能として扱う
		// document を mk-go だけが受け入れてしまう。
		*v = ""
		if len(t) > 0 {
			if s, ok := t[0].(string); ok {
				*v = APType(s)
			}
		}
	default:
		*v = ""
	}
	return nil
}

// String returns the value as a plain string.
func (v APType) String() string { return string(v) }

// ParseBoolish decodes a boolean-ish AP value, unwrapping JSON-LD expanded
// forms, and reports whether the shape was recognised at all.
//
// **JS の truthy は真似しない。** upstream が `isLocked: person.manuallyApprovesFollowers`
// のように生値を代入する field は、TypeORM が `@Column('boolean')` (型を文字列で
// 指定) なので丸めが効かず **PostgreSQL の boolean 入力構文**でキャストされる。
// つまり `"true"` は true だが `"false"` / `"0"` / `"no"` / `"off"` / `"f"` は
// **false**。JS truthy で「空文字以外は true」にすると、これらが軒並み反転する
// (#2662)。
//
// **読むのは完全形と PostgreSQL が挙げる 1 文字表記 (`t` / `f` / `y` / `n`) まで。**
// PostgreSQL は `'tr'` / `'fals'` のような一意な接頭辞も受け付けるが、そこまでは
// 追わない (`known=false` で呼び出し側の既定値に倒れる = 安全側)。曖昧で
// PostgreSQL 自身も拒否する `'o'` は当然読まない。PostgreSQL が受け付けない形も同じ扱い — upstream ではその actor の
// insert ごと落ちるので「取り込むが緩い状態にする」よりは既定値のほうが近い。
func ParseBoolish(raw any) (value bool, known bool) {
	switch t := raw.(type) {
	case bool:
		return t, true
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "t", "yes", "y", "on", "1":
			return true, true
		case "false", "f", "no", "n", "off", "0":
			return false, true
		}
		return false, false
	case json.Number:
		// **数値も PostgreSQL の入力構文で読む。** `node-postgres` は数値を
		// `String(val)` で送るので、upstream にとって数値 `2` と文字列 `"2"` は
		// 同じ (`'2'::boolean` は `invalid input syntax`)。受けるのは `1` / `0`
		// だけ。レンジ外 (`1e999`) は Float64() が失敗する = JS では
		// `Infinity` になり PostgreSQL が受けないので、同じく「読めない」。
		f, err := t.Float64()
		if err != nil {
			return false, false
		}
		return numberBoolish(f)
	case float64:
		return numberBoolish(t)
	case nil:
		return false, true
	case []any:
		// 単一要素配列は JSON-LD の展開形。中身で判断する。
		if len(t) == 1 {
			return ParseBoolish(t[0])
		}
		// **空配列・複数要素は「読めない」。** upstream は boolean 列への
		// キャストに失敗して insert ごと落ちるので、`false` と断定せず
		// 呼び出し側の既定値 (= 安全側) に倒す。
		//
		// 一方 `null` は「値なし」として false にする (Go の zero value と
		// 揃える)。単一要素配列は中身に再帰するので、`[null]` /
		// `{"@value": null}` も同じく false になる。
		return false, false
	case map[string]any:
		if val, ok := t["@value"]; ok {
			return ParseBoolish(val)
		}
		return false, false
	}
	return false, false
}

// numberBoolish maps a JSON number through PostgreSQL's boolean input syntax.
// Only `1` and `0` are accepted; anything else is unknown.
func numberBoolish(f float64) (value bool, known bool) {
	// `1.0` は JS の `String(1.0)` が "1" なので upstream では true。
	// `-0` も同様に "0"。
	switch f {
	case 1:
		return true, true
	case 0:
		return false, true
	}
	return false, false
}

// decodeBoolish decodes data into a boolean, using fallback when the shape is
// not recognised.
func decodeBoolish(data []byte, fallback bool) bool {
	// **UseNumber を使う。** 素の decode は `1e999` のような float64 レンジ外の
	// 値を型エラーにするので、構文上正当な JSON なのに fallback に倒れる。
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return fallback
	}
	if v, known := ParseBoolish(raw); known {
		return v
	}
	return fallback
}

// APTruthyBool decodes a boolean whose **unrecognised** shapes default to true.
//
// 使うのは「読めないときに false へ倒すと危険側になる」flag だけ:
//
//   - `Note.sensitive` — false にすると **送信側が sensitive と宣言したノートが
//     CW 無しで表示される**。mk-go は `sensitive` が立った note に空 CW を付ける
//     独自実装なので (upstream は CW を `summary` からしか作らない)、隠す側に倒す。
//   - `manuallyApprovesFollowers` — false にすると **承認制の相手に即 Following が
//     成立し「ローカルはフォロー済み・相手は承認待ち」の乖離**が黙って作られる。
//     upstream は PostgreSQL のキャストに失敗して insert ごと落ちるので、
//     「取り込むが緩い状態にする」よりは locked に倒すほうが近い。
//
// **`_misskey_canChat` はこれを使わない。** あちらは true = DM 許可なので、
// 読めないときに true へ倒すと相手が拒否しているのに送ってしまう。
type APTruthyBool bool

// UnmarshalJSON never fails; unrecognised shapes yield true.
func (v *APTruthyBool) UnmarshalJSON(data []byte) error {
	*v = APTruthyBool(decodeBoolish(data, true))
	return nil
}

// Bool returns the value as a plain bool.
func (v APTruthyBool) Bool() bool { return bool(v) }

// APLenientTimestamp holds an ActivityStreams `published` value.
//
// upstream は `new Date(object.published)` に投げるだけなので、JS の変換規則で
// 単一要素配列 (`["2024-01-01T00:00:00Z"]`) や epoch ミリ秒の数値がそのまま
// 通る。`string` 決め打ちだと **Note document ごと unmarshal に失敗する**
// (#2662)。読めない形は空にして、呼び出し側の fallback に委ねる。
type APLenientTimestamp string

// UnmarshalJSON never fails; unreadable shapes yield the empty string.
func (v *APLenientTimestamp) UnmarshalJSON(data []byte) error {
	*v = ""
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	*v = APLenientTimestamp(timestampValue(raw))
	return nil
}

// String returns the value as a plain string.
func (v APLenientTimestamp) String() string { return string(v) }

// timestampValue unwraps JSON-LD wrappers and converts epoch milliseconds.
func timestampValue(raw any) string {
	switch t := raw.(type) {
	case string:
		return t
	case float64:
		// JS の `new Date(number)` は epoch ミリ秒として解釈する。
		return time.UnixMilli(int64(t)).UTC().Format(time.RFC3339)
	case []any:
		// 単一要素配列は JSON-LD の展開形。JS でも `new Date([x])` は
		// 文字列化されて x として解釈される。
		if len(t) == 1 {
			return timestampValue(t[0])
		}
	case map[string]any:
		if val, ok := t["@value"]; ok {
			return timestampValue(val)
		}
	}
	return ""
}

// APLenientInt holds a JSON number that must not fail the surrounding document
// when it arrives in another shape.
//
// JS には整数型が無いので `3.0` や `"3"` を送ってくる実装がある。`int` 決め打ち
// だと **document ごと unmarshal に失敗する** (#2662)。読めない値は 0。
type APLenientInt int

// UnmarshalJSON never fails; unreadable shapes yield 0.
func (v *APLenientInt) UnmarshalJSON(data []byte) error {
	*v = 0
	var num json.Number
	if err := json.Unmarshal(data, &num); err == nil {
		// **整数として先に読む。** float64 を経由すると
		// `9223372036854775807` のような値が精度を失い、範囲外は
		// 実装依存の値になる (Go 仕様上 panic はしないが負値に化ける)。
		if n, err := num.Int64(); err == nil {
			*v = APLenientInt(n)
			return nil
		}
		// `3.0` のような小数表記も JS 由来では整数のつもりで来る。
		// **上限だけ厳密不等号。** `f <= math.MaxInt64` と書くと定数が float64 に
		// 丸め上がって `2^63` になり、`f == 2^63` がすり抜けて int64 変換が
		// 実装依存値 (負値) に化ける。下限の `math.MinInt64` は float64 で
		// 厳密に表現できるので `>=` でよい (`>` にすると -2^63 が読めなくなる)。
		if f, err := num.Float64(); err == nil && f >= math.MinInt64 && f < math.MaxInt64 {
			*v = APLenientInt(int64(f))
		}
		return nil
	}
	// **JSON number literal でない文字列**も受ける (JS の `Number()` と同じ扱い)。
	// `"3"` / `"3.0"` は `encoding/json` が `json.Number` に入れるので上で
	// 処理され、ここに来るのは `" 3 "` / `"+3"` / `"007"` のような形。
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		str = strings.TrimSpace(str)
		if n, err := strconv.ParseInt(str, 10, 64); err == nil {
			*v = APLenientInt(n)
			return nil
		}
		if f, err := strconv.ParseFloat(str, 64); err == nil && f >= math.MinInt64 && f < math.MaxInt64 {
			*v = APLenientInt(int64(f))
		}
	}
	return nil
}

// Int returns the value as a plain int.
func (v APLenientInt) Int() int { return int(v) }

// lenientIntPtr converts an optional int to the lenient wire type.
func lenientIntPtr(v *int) *APLenientInt {
	if v == nil {
		return nil
	}
	out := APLenientInt(*v)
	return &out
}

// lenientStringPtr converts an optional string to the lenient wire type.
func lenientStringPtr(v *string) *APLenientString {
	if v == nil {
		return nil
	}
	out := APLenientString(*v)
	return &out
}

// APLenientBool holds a JSON value that is expected to be a boolean but must
// not fail the surrounding document when it is not.
//
// upstream は `isCat: (person as any).isCat === true` /
// `isExplorable: person.discoverable` のように truthy 判定するだけなので、
// `"true"` のような非 bool でも actor は作られる。`bool` 決め打ちだと
// **actor / Note document ごと unmarshal に失敗する** (#2662)。
type APLenientBool bool

// UnmarshalJSON never fails; non-boolean shapes yield false.
//
// **読めない値は false に倒す。** JS の truthy をそのまま真似すると文字列
// `"false"` が true になり、意味が反転する。
//
// **値の読み方は `APTruthyBool` と同じ** (`ParseBoolish` = PostgreSQL の
// boolean 入力構文)。違うのは「読めない形」の既定値だけで、こちらは false。
//
// 使うのは **false に倒すほうが安全な flag**: `isCat` / `discoverable` /
// `_misskey_*` (`_misskey_canChat` は true = DM 許可なので特に false 側)。
// `sensitive` / `manuallyApprovesFollowers` は false が危険側なので
// `APTruthyBool` を使う。
//
// なお upstream で `=== true` の厳密比較をしているのは `isCat` と
// `requireSigninToViewContents` の 2 つで、`discoverable` は
// `isExplorable: person.discoverable` の生値代入。それでも既定を false に
// するのは、読めない値で explore に載せないため。
func (v *APLenientBool) UnmarshalJSON(data []byte) error {
	*v = APLenientBool(decodeBoolish(data, false))
	return nil
}

// Bool returns the value as a plain bool.
func (v APLenientBool) Bool() bool { return bool(v) }

// APRawList accepts a JSON value that is either a single value or an array,
// keeping the elements undecoded.
//
// `items` / `orderedItems` のように「要素を後段で個別に解釈する」場所で使う。
// `[]json.RawMessage` 決め打ちだと、単一 object の collection でその field の
// unmarshal が失敗する。**巻き添えの範囲は呼び出し側次第。** decode の error を
// 見て捨てる側 (`featured.go` の fetchFeaturedItems は `nil, false` を返す) では
// collection ごと落ち、error を握る側 (`processor.go` の handleCollection) では
// 別 field が生き残る (#2662)。upstream は `toArray(...)` で 1 件として拾う。
type APRawList []json.RawMessage

// UnmarshalJSON never fails; unreadable shapes yield a nil slice.
func (l *APRawList) UnmarshalJSON(data []byte) error {
	*l = nil
	if len(data) == 0 || bytes.Equal(data, jsonNullLiteral) {
		return nil
	}
	if data[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(data, &arr); err != nil {
			return nil
		}
		*l = arr
		return nil
	}
	*l = []json.RawMessage{append([]byte(nil), data...)}
	return nil
}

// APObjectList accepts a JSON value that is either a single object or an array,
// and exposes it uniformly as []any.
//
// AP の `attachment` は **配列でも単一 object でもよい**。upstream は
// `IObject | IObject[]` を受ける。`[]any` で決め打ちすると単一 object の
// document で json.Unmarshal ごと失敗し、**その actor / note が一切取り込め
// なくなる** (プロフィールだけでなくフォローもノートも通らない)。
//
// 単一 object は 1 要素のリストとして扱う。upstream の analyzeAttachments は
// 非配列を `[]` に潰すが、捨てる理由が無いので拾う。JSON 送出時は []any と
// 同じ array shape で emit する。
type APObjectList []any

// UnmarshalJSON decodes either a JSON array or a single value.
// null / 空 input は nil slice として解釈する。
func (l *APObjectList) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || bytes.Equal(data, jsonNullLiteral) {
		*l = nil
		return nil
	}
	// encoding/json は Unmarshaler に**値の先頭バイトから**渡す (前置の空白は
	// decodeState 側で読み飛ばされている) ので、data[0] を見れば配列か否かが
	// 決まる (空白 skip のループは不要)。
	if data[0] == '[' {
		var arr []any
		if err := json.Unmarshal(data, &arr); err != nil {
			// **読めた要素は残す。** 1 要素が数値レンジ外 (`1e999`) なだけで
			// 配列ごと捨てると、mention / emoji / hashtag が全滅する (#2662)。
			var typeErr *json.UnmarshalTypeError
			if !errors.As(err, &typeErr) {
				return err
			}
		}
		*l = arr
		return nil
	}
	// 配列でないなら単一の値。1 件のリストとして扱う。
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		// **error を返さない。** field の UnmarshalJSON が error を返すと
		// `encoding/json` は**親 struct の decode をその場で打ち切る**ので、
		// actor は丸ごと落ち、Note は `tag` より後ろの `content` / `summary` /
		// `to` を失って「本文なし・誰にも見えない」ノートになる。組み込み型の
		// 型不一致 (`saveError`) が decode を継続するのとは挙動が違う (#2662)。
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			return err
		}
		*l = nil
		return nil
	}
	*l = []any{v}
	return nil
}

// APLenientString holds a JSON value that is expected to be a string but must
// not fail the surrounding document when it is not.
//
// `vcard:bday` / `vcard:Address` を `string` で決め打ちすると、object や数値で
// 送ってくる実装 (JSON-LD の展開形など) で **actor ごと reject される**。
// これらは表示用の付加情報でしかないので、読めなければ空にして document は
// 通す。
//
// **JSON-LD の展開形は剥がして値を拾う** (`{"@value": "x"}` / `["x"]` /
// `[{"@value": "x"}]`)。`Normalize` は `{"@value": ...}` しか潰さないうえ
// 生 fetch 経路は通らないので、ここで剥がさないと「読めたはずの値が黙って
// 消える」(CW が付かない / 誕生日が入らない) ことになる (#2662)。
type APLenientString string

// UnmarshalJSON keeps a plain JSON string as-is and unwraps JSON-LD expanded
// forms (`{"@value": "x"}` / `["x"]` / `[{"@value": "x"}]`). それ以外の形
// (数値 / null / 複数要素の配列 / `@value` を持たない object) は空文字にして
// **error を返さない**。
func (v *APLenientString) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*v = APLenientString(s)
		return nil
	}
	// **JSON-LD の展開形は剥がして値を拾う。** `[{"@value": "x"}]` は
	// compaction されなかったときの正規形で、捨てると「読めたはずの値が
	// 黙って消える」(CW が付かない等) ことになる。`Normalize` は
	// `{"@value": ...}` しか潰さないうえ、生 fetch 経路は通らない。
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		*v = ""
		return nil
	}
	*v = APLenientString(unwrapLDString(raw))
	return nil
}

// unwrapLDString pulls a string out of a JSON-LD expanded value.
func unwrapLDString(raw any) string {
	switch t := raw.(type) {
	case string:
		return t
	case []any:
		if len(t) == 1 {
			return unwrapLDString(t[0])
		}
	case map[string]any:
		if val, ok := t["@value"]; ok {
			return unwrapLDString(val)
		}
	}
	return ""
}

// String returns the value as a plain string.
func (v APLenientString) String() string { return string(v) }

// apFirstRef extracts one reference string from an ActivityStreams value.
//
// upstream の `getOneApId` / `getOneApHrefNullable` (type.ts) と同じ形を受ける:
// 素の string、`key` を持つ object、そのどちらかの配列 (先頭のみ)。**配列を
// 剥がすのは 1 段だけ**で、upstream もそうしている (`getApId` は配列を見ない)。
//
// 読めない形は空文字を返し、**エラーにしない**。ここでエラーにすると
// document 全体の unmarshal が失敗し、その actor / note がまったく取り込め
// なくなる (#2662)。必須かどうかは呼び出し側が空文字で判断する。
func apFirstRef(data []byte, key string) string {
	// **数値は `json.Number` で受ける。** 素の `any` へ decode すると数値が
	// float64 になり、`{"href":"https://a","id":1e999}` のような**壊れていない
	// JSON** で `cannot unmarshal number` になって参照ごと落ちる。読むのは
	// string だけなのに、兄弟 field にレンジ外の数値が 1 つあるだけで
	// `attributedTo` も `featured` も `url` も空になる。**`attributedTo` が空に
	// なると note ごと落ちる** (`ingestNoteWithCreated` の明示 gate) ので、
	// リモートが 1 トークン置くだけで取り込みを止められた (#2730 で nodeinfo に
	// 入れたのと同じ対処)。
	//
	// **救われるのは `Normalize` を通らない生 fetch 経路だけ** (actor / note /
	// featured の取得)。inbox 経路は `Process` の入口で `Normalize` (`jsonld.go`)
	// を通り、そこが素の `any` へ decode するので、同じ 1 トークンで activity
	// ごと `invalid activity json` になる。**この class 自体は閉じていない。**
	//
	// **`inbox` はこの型を通らない** (`Person.Inbox` は string 決め打ち)。
	// object 形式の `inbox` は **actor ごと落ちる** — `Person` には `Note` の
	// ような catch-all `UnmarshalJSON` が無く、`fetchActor` が unmarshal の
	// error を全部 `ErrInvalidActor` に潰すため。**この修正では救われない。**
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return ""
	}
	if arr, ok := v.([]any); ok {
		if len(arr) == 0 {
			return ""
		}
		v = arr[0]
	}
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		s, _ := t[key].(string)
		return s
	}
	return ""
}

// APLenientID holds an ActivityStreams object reference that may arrive as a
// bare id string, as an object carrying `id`, or as an array of either.
// It mirrors upstream's getOneApId.
type APLenientID string

// UnmarshalJSON never fails; unreadable shapes yield the empty string.
func (v *APLenientID) UnmarshalJSON(data []byte) error {
	*v = APLenientID(apFirstRef(data, "id"))
	return nil
}

// String returns the value as a plain string.
func (v APLenientID) String() string { return string(v) }

// APLenientHref holds a link that may arrive as a bare URL string, as an
// object carrying `href`, or as an array of either. It mirrors upstream's
// getOneApHrefNullable.
type APLenientHref string

// UnmarshalJSON never fails; unreadable shapes yield the empty string.
func (v *APLenientHref) UnmarshalJSON(data []byte) error {
	*v = APLenientHref(apFirstRef(data, "href"))
	return nil
}

// String returns the value as a plain string.
func (v APLenientHref) String() string { return string(v) }

// APIDList holds a list of ActivityStreams object references, each of which
// may be a bare id string or an object carrying `id`. It mirrors upstream's
// getApIds.
type APIDList []string

// UnmarshalJSON decodes an array or a single value into a list of ids.
//
// **読めない要素は落とす。** upstream の `getApIds` は要素ごとに `getApId` を
// 呼ぶので 1 つでも読めないと throw して note ごと reject するが、mk-go は
// 残りを活かす。`to` / `cc` は可視性の計算に使うため、要素を落とすと
// 「public のはずが home」のように**狭い側**に倒れる。document ごと捨てるより
// 影響が小さい (#2662)。
func (l *APIDList) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || bytes.Equal(data, jsonNullLiteral) {
		*l = nil
		return nil
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		// **読めた要素は残す。** `APObjectList` と同じ理由で、1 要素が数値
		// レンジ外 (`1e999`) なだけで audience ごと捨てない。`Person` には
		// Note のような catch-all が無いので、`alsoKnownAs` で actor が丸ごと
		// 落ちる (#2662)。
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			return err
		}
	}
	arr, ok := v.([]any)
	if !ok {
		arr = []any{v}
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		switch t := item.(type) {
		case string:
			out = append(out, t)
		case map[string]any:
			if s, ok := t["id"].(string); ok {
				out = append(out, s)
			}
		}
	}
	*l = out
	return nil
}

// ContextURL is the standard ActivityStreams 2.0 JSON-LD context.
const ContextURL = "https://www.w3.org/ns/activitystreams"

// SecurityContextURL is the W3C security vocabulary used for HTTP Signatures.
const SecurityContextURL = "https://w3id.org/security/v1"

// MultikeyContextURL is the W3C Multikey vocabulary (FEP-521a) used to expose
// Ed25519 public keys via Person.assertionMethod[]. Fedibird / Mastodon
// glitch-soc などが解釈可能。
const MultikeyContextURL = "https://w3id.org/security/multikey/v1"

// DataIntegrityContextURL is the W3C VC Data Integrity vocabulary that defines
// the `assertionMethod` term used to attach Multikey entries to actors.
const DataIntegrityContextURL = "https://w3id.org/security/data-integrity/v1"

// Public is the magic IRI used to denote a publicly addressable activity.
const Public = "https://www.w3.org/ns/activitystreams#Public"

// ValidActorTypes lists the AP object types acceptable as an Actor.
// Mirrors Misskey's `validActor` array in core/activitypub/type.ts so that
// inbound documents of any other Object type are rejected at parse time.
var ValidActorTypes = []string{
	"Person", "Service", "Group", "Organization", "Application",
}

// IsValidActorType reports whether t is one of the recognized actor types.
// Empty strings are rejected.
func IsValidActorType(t string) bool {
	return slices.Contains(ValidActorTypes, t)
}

// IsBotActorType reports whether the given actor type maps to an automated
// (non-human) account. Misskey treats both Service and Application as bots.
func IsBotActorType(t string) bool {
	return t == "Service" || t == "Application"
}

// MimeType is the canonical Content-Type for ActivityPub objects.
const MimeType = `application/activity+json`

// LDMimeType is the more specific JSON-LD content type with profile parameter.
const LDMimeType = `application/ld+json; profile="https://www.w3.org/ns/activitystreams"`

// Object is the base type for any AS object. 拡張するときは embed する。
type Object struct {
	Context any    `json:"@context,omitempty"`
	ID      string `json:"id,omitempty"`
	Type    APType `json:"type,omitempty"`
	// upstream `validateActor` は `if (x.name)` の truthy ガードの内側で
	// 非 string を throw するので、`name: 0` のような falsy な値は受理して
	// name=null の actor を作る。string 決め打ちだと **mk-go だけが actor を
	// 丸ごと落とす**。あわせて JSON-LD の展開形も拾う (#2662)。
	Name APLenientString `json:"name,omitempty"`
}

// OrderedCollection is an AS OrderedCollection used for actor-advertised
// collections (outbox / followers / following / collections/featured)。upstream
// ApRendererService.renderOrderedCollection と同 shape。orderedItems は inline 提供時
// (featured 等) のみ、first/last は paginate 時のみ出力する (#1876)。
type OrderedCollection struct {
	Context      any    `json:"@context,omitempty"`
	ID           string `json:"id"`
	Type         string `json:"type"`
	TotalItems   int    `json:"totalItems"`
	First        string `json:"first,omitempty"`
	Last         string `json:"last,omitempty"`
	OrderedItems []any  `json:"orderedItems,omitempty"`
}

// OrderedCollectionPage is a single page of an OrderedCollection (paginated
// followers/following/outbox)。upstream ApRendererService.renderOrderedCollectionPage
// と同 shape。orderedItems は常に出力し (空でも []), prev/next は cursor がある時のみ (#1877)。
type OrderedCollectionPage struct {
	Context      any    `json:"@context,omitempty"`
	ID           string `json:"id"`
	Type         string `json:"type"`
	PartOf       string `json:"partOf"`
	TotalItems   int    `json:"totalItems"`
	OrderedItems []any  `json:"orderedItems"`
	Prev         string `json:"prev,omitempty"`
	Next         string `json:"next,omitempty"`
}

// PublicKey is the embedded JSON-LD object describing a user's signing key.
type PublicKey struct {
	ID    string          `json:"id"`
	Owner APLenientString `json:"owner"`
	// upstream は非 string でも throw せず (JS の暗黙変換で文字列化される)
	// actor を作る。string 決め打ちだと actor document ごと落ちて、その actor が
	// 一切連合できなくなる (#2662)。PEM として不正なら後段の parse が弾く。
	PublicKeyPEM APLenientString `json:"publicKeyPem"`
}

// Multikey represents a single FEP-521a Multikey entry expose-d via
// Person.assertionMethod[]. Used by mk-go to publish Ed25519 public keys
// alongside the legacy RSA-only `publicKey` field, so capability-aware
// receivers can verify HTTP Signatures with Ed25519 if the local user owns
// an Ed25519 keypair.
//
// PublicKeyMultibase encodes the raw key with the standard "z" + base58btc +
// multicodec prefix format (see internal/activitypub/multikey.go).
type Multikey struct {
	ID string `json:"id"`
	// `"type": ["Multikey"]` を送る実装がある。string 決め打ちだと
	// MultikeyList が Unreadable になり、**stale key の purge が恒久的に
	// 止まる** (= ローテーション済みの鍵で署名した activity が verify を
	// 通り続ける、#2662)。
	Type               APType `json:"type"`
	Controller         string `json:"controller"`
	PublicKeyMultibase string `json:"publicKeyMultibase"`
}

// MultikeyType is the canonical `type` value for a Multikey entry.
const MultikeyType = "Multikey"

// Endpoints holds the endpoints sub-object for an Actor.
type Endpoints struct {
	SharedInbox APLenientID `json:"sharedInbox,omitempty"`
}

// UnmarshalJSON tolerates a non-object `endpoints`.
//
// upstream は `x.endpoints ? x.endpoints.sharedInbox : undefined` なので、
// `endpoints` が string でも undefined になるだけで actor は通る。struct
// 決め打ちだと **actor document ごと unmarshal に失敗する** (#2662)。
func (e *Endpoints) UnmarshalJSON(data []byte) error {
	type endpointsAlias Endpoints
	var v endpointsAlias
	if err := json.Unmarshal(data, &v); err != nil {
		*e = Endpoints{}
		return nil
	}
	*e = Endpoints(v)
	return nil
}

// Image is a generic ActivityStreams Image (used for icon/image fields).
type Image struct {
	// upstream の isDocument は getApType 経由なので `"type": ["Image"]` を
	// 受ける。`url` も ApImageService が `typeof image.url !== 'string'` を
	// null で返すだけで actor は作る。string 決め打ちだと **actor document
	// ごと unmarshal に失敗してその actor が一切連合できない** (#2662)。
	Type APType        `json:"type"`
	URL  APLenientHref `json:"url"`
	// MediaType / Sensitive / Name は upstream renderImage / renderEmoji icon の
	// 追加プロパティ (ApRendererService.ts:186-191,253-260)。omitempty なので
	// 未設定時は従来どおり {type,url} のみ出力し後方互換を保つ (#1948-11)。
	MediaType string `json:"mediaType,omitempty"`
	Sensitive *bool  `json:"sensitive,omitempty"`
	Name      string `json:"name,omitempty"`
}

// URLOrEmpty returns the image URL, tolerating a nil receiver.
func (i *Image) URLOrEmpty() string {
	if i == nil {
		return ""
	}
	return i.URL.String()
}

// UnmarshalJSON accepts both forms allowed by ActivityStreams 2.0 for
// `icon` / `image` properties:
//
//	"icon": {"type": "Image", "url": "https://..."}
//	"icon": [{"type": "Image", "url": "..."}, {"type": "Image", "url": "..."}]
//
// 参照: https://www.w3.org/TR/activitystreams-vocabulary/#dfn-icon
//
// 旧 mk-go は単一 object 形のみ受理しており、iceshrimp / 一部 Pleroma fork
// など array 形式で multi-resolution アイコンを expose する実装からの actor
// fetch が JSON unmarshal 失敗で avatar/banner 取得漏れになっていた (= TL の
// `@mention` 横にアイコンが出ない症状)。upstream Misskey TS の
// `ApPersonService.resolveAvatarAndBanner` は array 形式を `find(item =>
// item.url)` で pick する semantics なので mk-go も同等に揃える。
//
// **string / number 等でも error を返さない** (#2662)。bare string の `icon` は
// AS2 で正当 (`icon` の range は `Image | Link` で Link は compaction で bare
// IRI になる) で、ここで落とすと actor document ごと reject されてその actor が
// 一切連合できなくなる。null / 空 array / object with 空 url も同じく Image{}
// (zero value) を残し、呼び出し側 (`actor.Icon.URL != ""` チェック) が
// 「avatar 無し」として扱えるようにする。
func (i *Image) UnmarshalJSON(data []byte) error {
	// "null" は親 decoder が *Image を nil pointer のままにするので
	// 本関数は呼ばれないが、上位 field が値型 (`Image` 直、例: EmojiTag.Icon)
	// の場合に届くことがあるので明示的に no-op する。bytes.Equal で alloc 0。
	if bytes.Equal(data, jsonNullLiteral) {
		return nil
	}
	// 先頭の非空白文字で array / object を判別する。array は AS2.0 で許容。
	for _, b := range data {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '[':
			var arr []Image
			if err := json.Unmarshal(data, &arr); err != nil {
				return err
			}
			// upstream と同じく url が non-empty な最初の要素を採用する。
			// すべて空 url なら zero value を残して呼び出し側に判断を委ねる。
			for _, item := range arr {
				if item.URL != "" {
					*i = item
					return nil
				}
			}
			return nil
		default:
			// 単一 object 形式。alias で recursion を回避しつつ decode。
			type imageAlias Image
			var alias imageAlias
			if err := json.Unmarshal(data, &alias); err != nil {
				// **エラーを返さない。** `"icon": "https://e/a.png"` のような
				// bare string は AS2 では正当 (icon の range は Image | Link で、
				// Link は compaction で bare IRI になる)。ここで error を返すと
				// actor document ごと落ちて、その actor がフォローもノートも
				// 含めて一切連合できなくなる (#2662)。
				//
				// upstream は `createImage` が値を resolver で解決してから
				// `typeof image.url !== 'string'` で null を返し、
				// `resolveAvatarAndBanner` が `.catch(() => null)` で包む。
				// **bare string は dereference されるので upstream は
				// アバターを得ることがある**が、mk-go は取りに行かない
				// (取得を増幅させないため)。actor が作られる点は同じ。
				//
				// **兄弟 field が 1 つ読めないだけで全部捨てない。**
				// encoding/json は UnmarshalTypeError でも残りの field の
				// decode を続けるので、この時点の alias には読めた値が
				// 入っている。それをそのまま使う。
				*i = Image(alias)
				return nil
			}
			*i = Image(alias)
			return nil
		}
	}
	return nil
}

// MultikeyList accepts a FEP-521a `assertionMethod` value that may arrive as
// an array, as a single object, or in a shape we cannot read.
//
// これは mk-go 独自の拡張で upstream は読まない。したがって読めない形でも
// **actor document を落としてはいけない** (#2662)。
//
// **「無い」と「読めなかった」は区別する。** 呼び出し側 (cacheAssertionMethods)
// は「actor が申告しなかった keyId を purge する」設計なので、読めなかったのに
// 空リストを渡すと**キャッシュ済みの Ed25519 鍵を全部消す**。Ed25519 のみを
// publish する相手では inbound の署名検証が恒久的に失敗する。Unreadable で
// それを判別できるようにする。
type MultikeyList struct {
	Keys []Multikey
	// Refs holds bare IRI entries. DID Core / FEP-521a の `assertionMethod` は
	// `VerificationMethod | URI` なので、鍵素材を持たない参照形式も正当。
	// 鍵は取り込めないが「actor がその keyId を申告している」ことは分かるので、
	// purge の判定には使える。
	Refs []string
	// Unreadable is true when the value was present but could not be decoded.
	Unreadable bool
}

// UnmarshalJSON never fails; unreadable shapes set Unreadable instead.
//
// 配列は**要素ごとに** decode する。1 件でも読めないものがあると全体を捨てる
// 実装だと、正常な鍵が混ざっていても消えてしまう (呼び出し側は entry ごとに
// warn + continue する設計なので、その手前で全滅させるのは矛盾)。
func (l *MultikeyList) UnmarshalJSON(data []byte) error {
	*l = MultikeyList{}
	if len(data) == 0 || bytes.Equal(data, jsonNullLiteral) {
		return nil
	}
	if data[0] == '[' {
		var raw []json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			l.Unreadable = true
			return nil
		}
		for _, item := range raw {
			if ref, ok := apBareIRI(item); ok {
				l.Refs = append(l.Refs, ref)
				continue
			}
			var one Multikey
			if err := json.Unmarshal(item, &one); err != nil {
				l.Unreadable = true
				continue
			}
			l.Keys = append(l.Keys, one)
		}
		return nil
	}
	if ref, ok := apBareIRI(data); ok {
		l.Refs = []string{ref}
		return nil
	}
	var one Multikey
	if err := json.Unmarshal(data, &one); err != nil {
		l.Unreadable = true
		return nil
	}
	l.Keys = []Multikey{one}
	return nil
}

// apBareIRI reports whether the raw JSON value is a plain string that looks
// like an absolute IRI.
//
// **URL の形をしていない文字列は参照として扱わない。** 参照と見なすと、
// 呼び出し側は「actor が申告した keyId の集合」を読めたことにして
// stale key の purge を実行してしまう。ゴミ文字列は「申告を読めなかった」
// (= purge しない) 側に倒すのが安全 (#2662)。
func apBareIRI(data []byte) (string, bool) {
	var str string
	if err := json.Unmarshal(data, &str); err != nil || str == "" {
		return "", false
	}
	u, err := url.Parse(str)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	return str, true
}

// MarshalJSON emits the plain array form so the wire shape is unchanged.
func (l MultikeyList) MarshalJSON() ([]byte, error) { return json.Marshal(l.Keys) }

// IsZero lets `omitzero` drop the key when there is nothing to publish.
// struct 型なので `omitempty` は効かない (常に key が出て null になる)。
func (l MultikeyList) IsZero() bool { return len(l.Keys) == 0 }

// Person represents a user actor.
type Person struct {
	Object
	Inbox             string          `json:"inbox"`
	Outbox            APLenientID     `json:"outbox"`
	Followers         APLenientID     `json:"followers"`
	Following         APLenientID     `json:"following"`
	PreferredUsername string          `json:"preferredUsername"`
	Summary           APLenientString `json:"summary,omitempty"`
	URL               APLenientHref   `json:"url,omitempty"`
	// SharedInbox は upstream renderPerson が endpoints.sharedInbox とは別に
	// top-level にも出す (#1560、ApRendererService.ts:551)。古い実装/一部の
	// 受信側は top-level を見るため両方出す。
	SharedInbox APLenientID  `json:"sharedInbox,omitempty"`
	Endpoints   Endpoints    `json:"endpoints,omitzero"`
	PublicKey   PublicKey    `json:"publicKey"`
	Icon        *Image       `json:"icon,omitempty"`
	Image       *Image       `json:"image,omitempty"`
	Attachment  APObjectList `json:"attachment,omitempty"`
	Tag         APObjectList `json:"tag,omitempty"`
	Featured    APLenientID  `json:"featured,omitempty"`
	// upstream renderPerson は manuallyApprovesFollowers/discoverable/isCat/
	// _misskey_requireSigninToViewContents を常に boolean で出力する。omitempty だと
	// false で key 自体が消えて wire-shape が乖離するため omitempty を外す (#1948-11)。
	// 値は RenderPerson で user 状態から populate 済み。
	ManuallyApproves APTruthyBool `json:"manuallyApprovesFollowers"`
	// Suspended is the Mastodon extension `toot:suspended` (#2951).
	//
	// **読むだけで、こちらからは出さない。** `RenderPerson` は設定しないので
	// 常に false になり、`omitempty` で wire にも出ない (upstream Misskey も
	// 出さないので shape は変わらない)。Misskey 系は凍結時に `Delete` を送り、
	// Mastodon 系は actor にこれを立てるだけ、という違いを埋めるために読む。
	//
	// **`APLenientBool` を使う** (読めない形は false)。`APTruthyBool` だと
	// 壊れた値で誤って凍結してしまう。
	Suspended    APLenientBool   `json:"suspended,omitempty"`
	Discoverable APLenientBool   `json:"discoverable"`
	IsCat        APLenientBool   `json:"isCat"`
	VcardBday    APLenientString `json:"vcard:bday,omitempty"`
	VcardAddress APLenientString `json:"vcard:Address,omitempty"`
	// #2106 L50: upstream renderPerson は _misskey_summary / _misskey_followedMessage を常時
	// 出力する (description/followedMessage が null なら JSON null)。*string + omitempty 無しで
	// nil→null を明示出力し wire-shape を揃える。
	MisskeySummary                      *APLenientString `json:"_misskey_summary"`
	MisskeyFollowedMessage              *APLenientString `json:"_misskey_followedMessage"`
	MisskeyRequireSigninToViewContents  APLenientBool    `json:"_misskey_requireSigninToViewContents"`
	MisskeyMakeNotesFollowersOnlyBefore *APLenientInt    `json:"_misskey_makeNotesFollowersOnlyBefore,omitempty"`
	MisskeyMakeNotesHiddenBefore        *APLenientInt    `json:"_misskey_makeNotesHiddenBefore,omitempty"`
	// MisskeyCanChat は CherryPick の chat 連合 capability flag (#692)。
	// 受信側 instance が DM を受け付けるか (false なら拒絶) を表す boolean。
	// pointer で持つことで「未指定 (= 旧実装 / 互換) → 許可」を区別する。
	MisskeyCanChat *APLenientBool `json:"_misskey_canChat,omitempty"`
	MovedTo        APLenientID    `json:"movedTo,omitempty"`
	AlsoKnownAs    APIDList       `json:"alsoKnownAs,omitempty"`
	// AssertionMethod は FEP-521a Multikey 形式で expose する追加公開鍵リスト
	// (mk-go では現状 Ed25519 のみ)。omitzero (IsZero) なので Ed25519 鍵を持たない
	// user / TS で signup した user では出力されず、drop-in 互換を維持する
	// (#1067 / #1069)。
	AssertionMethod MultikeyList `json:"assertionMethod,omitzero"`
	// FollowersRaw / FollowingRaw hold the raw `followers` / `following` values
	// of a fetched actor document (an IRI string or an embedded collection).
	// They are filled by the federation resolver and never serialized.
	//
	// Followers / Following (APLenientID) は埋め込みの collection を id に潰す
	// ので、中身 (first / items / orderedItems) を見るために生の値も持つ。
	FollowersRaw json.RawMessage `json:"-"`
	FollowingRaw json.RawMessage `json:"-"`
}

// Note represents a note object (microblog post).
type Note struct {
	Object
	AttributedTo APLenientID        `json:"attributedTo"`
	Content      string             `json:"content"`
	Source       *Source            `json:"source,omitempty"`
	Published    APLenientTimestamp `json:"published"`
	To           APIDList           `json:"to"`
	CC           APIDList           `json:"cc,omitempty"`
	InReplyTo    APLenientID        `json:"inReplyTo,omitempty"`
	// CW は summary からしか作らない。読めずに空へ倒すと **CW 無しのノートが
	// できる** ので、JSON-LD の展開形も剥がして拾う (#2662)。
	Summary    APLenientString `json:"summary,omitempty"`
	Sensitive  APTruthyBool    `json:"sensitive,omitempty"`
	Tag        APObjectList    `json:"tag,omitempty"`
	Attachment APObjectList    `json:"attachment,omitempty"`
	// URL は HTML 版の permalink。**`id` とは別物**で、Mastodon 系では
	// `id` が AP object、`url` が Web ページを指す (#2729)。読み方は
	// upstream の `getOneApHrefNullable` = `APLenientHref` (配列なら先頭 →
	// string ならそれ / object なら `href`)。**`id` は見ない**。ただし inbox
	// 経路では手前の `Normalize` が JSON-LD の展開形を剥がすので、**単一キーの**
	// `{"@id": ...}` のような形は string になって届く (2 キーあると潰れない。
	// `docs/divergence.md` の `note.url` の節)。
	//
	// upstream の `renderNote` は note に `url` を出さないので、outbound には
	// 影響しない (omitempty で消える)。
	URL            APLenientHref   `json:"url,omitempty"`
	MisskeyContent APLenientString `json:"_misskey_content,omitempty"`
	MisskeyQuote   APLenientID     `json:"_misskey_quote,omitempty"`
	QuoteURL       APLenientID     `json:"quoteUrl,omitempty"`
	// Quote / QuoteURI は FEP-044f と Fedibird の引用 (#3234)。Mastodon は
	// `_misskey_quote` も並べて付けるので、引用先の解決は今も `_misskey_quote`
	// で足りる。ここで読むのは QuoteRequest の instrument を確かめるときだけ。
	Quote              APLenientID `json:"quote,omitempty"`
	QuoteURI           APLenientID `json:"quoteUri,omitempty"`
	QuoteAuthorization APLenientID `json:"quoteAuthorization,omitempty"`
	// InteractionPolicy は FEP-044f の「引用してよい範囲」(#3234)。送るときは
	// *InteractionPolicy を入れる。**受け取る側で型を決めない** — 相手の形が
	// 想定と違うだけで Note 全体の unmarshal が失敗し、投稿を取り込めなくなる。
	InteractionPolicy any `json:"interactionPolicy,omitempty"`
	// MisskeyTalk は CherryPick / レガシー Misskey のチャット連合 flag (#692)。
	// `_misskey_talk: true` が立った Note は ApInboxService で chat message
	// として処理される (notes テーブルではなく chat_messages テーブルに保存)。
	MisskeyTalk APLenientBool `json:"_misskey_talk,omitempty"`
	// Name は AP poll vote (Question への投票) で choice 名を運ぶ field。
	// Misskey TS の vote AP payload は `{type: "Note", name: "<choice>",
	// inReplyTo: <poll URI>}` 形式で、ApNoteService が name + inReplyTo の
	// 組合せで vote と判定する。通常の投稿には付かない (#690)。
	// 読めずに空へ倒すと vote 判定を素通りして **本文空の reply note** に
	// なるので、こちらも展開形を剥がす (#2662)。Person 側の `name` とは
	// 別扱いにするため Object.Name を shadow する。
	Name APLenientString `json:"name,omitempty"`
	// Question (poll) fields — AP Question typeで使用
	OneOf   []QuestionChoice `json:"oneOf,omitempty"`
	AnyOf   []QuestionChoice `json:"anyOf,omitempty"`
	EndTime APLenientString  `json:"endTime,omitempty"`
	Closed  APLenientString  `json:"closed,omitempty"`
}

// QuestionChoice represents a single choice in an AP Question (poll).
type QuestionChoice struct {
	// upstream ApQuestionService は選択肢の `type` を一切見ない。string
	// 決め打ちだと `"type": ["Note"]` で **Note ごと reject される** (#2662)。
	Type    APType                 `json:"type"` // "Note"
	Name    string                 `json:"name"`
	Replies *QuestionChoiceReplies `json:"replies,omitempty"`
}

// QuestionChoiceReplies holds the vote count for a poll choice.
//
// AS2 では `replies` を IRI 参照にするのも正規の形。upstream は
// `x.replies?.totalItems ?? x._misskey_votes ?? 0` と optional chaining で
// 読むので、非 object でも**票数 0 でアンケートは作られる** (#2662)。
//
// `Source` と同じく、**catch-all では代替できない** (field の UnmarshalJSON が
// error を返すと親の decode がその場で打ち切られる)。
type QuestionChoiceReplies struct {
	Type APType `json:"type"` // "Collection"
	// JS には整数型が無いので `3.0` で送ってくる実装がある。`int` 決め打ちだと
	// Note ごと reject される (#2662)。
	TotalItems APLenientInt `json:"totalItems"`
}

// UnmarshalJSON tolerates a non-object `replies` (IRI reference etc.).
func (q *QuestionChoiceReplies) UnmarshalJSON(data []byte) error {
	type repliesAlias QuestionChoiceReplies
	var v repliesAlias
	if err := json.Unmarshal(data, &v); err != nil {
		*q = QuestionChoiceReplies{}
		return nil
	}
	*q = QuestionChoiceReplies(v)
	return nil
}

// Mention is an ActivityStreams Mention tag used inside Note.tag to inform
// receiving instances which actors are mentioned in the note content.
// href は actor URI (ローカルなら urls.UserURI, リモートなら user.URI)、
// name は Misskey 互換の "@username" / "@username@host" 形式。
type Mention struct {
	Type string `json:"type"` // "Mention"
	Href string `json:"href"`
	Name string `json:"name,omitempty"`
}

// NewMention constructs a Mention with the fixed `Type` label pre-filled.
// 呼び出し側で毎回 Type を書かなくて済むようにした factory。name は
// 任意 (空文字でも許容される)。
func NewMention(href, name string) Mention {
	return Mention{Type: "Mention", Href: href, Name: name}
}

// UnmarshalJSON keeps a Note whose optional fields arrive in the wrong shape.
//
// upstream は JS なので型検査をほとんどしない。`content` は
// `typeof note.content === 'string'` のガードを通って **text=null のノートが
// 作られる**し、`oneOf` / `anyOf` / choice の `name` が読めなくても
// `extractPollFromQuestion(...).catch(() => undefined)` で poll 無しのノートが
// できる。mk-go は struct 決め打ちなので `json.Unmarshal` が失敗し、
// **そのノートが丸ごと取り込めない** (#2662)。
//
// `encoding/json` は型不一致でも残りの field の decode を続け、最初の
// `*json.UnmarshalTypeError` を保持して返す。読めた分をそのまま採用すれば、
// field ごとに型を緩めなくても upstream と同じ「読めるものだけ使う」挙動に
// なる。**構文エラー (壊れた JSON) は従来どおり弾く。**
//
// **`published` を特別扱いしない。** upstream は
// `isSafeT(new Date(object.published).valueOf())` で malformed を reject するが、
// mk-go は元々 `parseAPPublishedTime` が読めない値を受信時刻に fallback する
// 設計で、published で Note を落とさない。ここだけ reject に倒すと、
// **upstream が受理する形 (`["2024-01-01T00:00:00Z"]` / epoch ミリ秒 / `0`) まで
// 巻き込んで落とす**うえ、`encoding/json` は最初の型エラーしか報告しないので
// 先行する field のエラーで判定自体が飛ぶ。読める形は `APLenientTimestamp` が
// 拾い、読めなければ従来どおり fallback する。
func (n *Note) UnmarshalJSON(data []byte) error {
	type noteAlias Note
	var alias noteAlias
	err := json.Unmarshal(data, &alias)
	if err != nil {
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			return err
		}
	}
	*n = Note(alias)
	return nil
}

// Source represents the original markup of a note (Markdown / MFM).
type Source struct {
	Content   string `json:"content"`
	MediaType string `json:"mediaType"`
}

// UnmarshalJSON tolerates a non-object `source`.
//
// upstream は `note.source?.mediaType === 'text/x.misskeymarkdown'` と optional
// chaining で読むので、`"source": "raw text"` のような非 object でも **Note は
// 通る** (#2662)。
//
// **`Note.UnmarshalJSON` の catch-all では代替できない。** field の
// `UnmarshalJSON` が error を返すと `encoding/json` は**親 struct の decode を
// その場で打ち切る**ので、`source` より後ろの `content` / `summary` / `to` が
// 丸ごと消える (組み込み型の型不一致は `saveError` で継続するので catch-all が
// 効くが、こちらは別経路)。
func (s *Source) UnmarshalJSON(data []byte) error {
	type sourceAlias Source
	var v sourceAlias
	if err := json.Unmarshal(data, &v); err != nil {
		*s = Source{}
		return nil
	}
	*s = Source(v)
	return nil
}

// Document represents an attached file (image/video/audio/...) in a Note.
//
// Width / Height は `as:width` / `as:height` (ActivityStreams 拡張)。
// Icon は Document を表示する際のサムネイル候補で、Misskey TS 系が
// 連合させる。Blurhash は Misskey の `_misskey_blurhash` カスタム拡張で、
// frontend がメディアロード前にぼかし背景を表示するために使う。
// いずれも欠落している remote 実装も多いので omitempty で受ける。
type Document struct {
	Type      string `json:"type"` // "Document"
	MediaType string `json:"mediaType"`
	URL       string `json:"url"`
	Name      string `json:"name,omitempty"`
	Sensitive bool   `json:"sensitive,omitempty"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Icon      *Image `json:"icon,omitempty"`
	Blurhash  string `json:"_misskey_blurhash,omitempty"`
}

// PropertyValue represents a profile field (schema.org PropertyValue).
type PropertyValue struct {
	Type  string `json:"type"` // "PropertyValue"
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Hashtag is an ActivityStreams Hashtag tag.
type Hashtag struct {
	Type string `json:"type"` // "Hashtag"
	Href string `json:"href"`
	Name string `json:"name"` // "#tag"
}

// EmojiTag represents a toot:Emoji tag for custom emoji.
type EmojiTag struct {
	Type    string `json:"type"` // "Emoji"
	Name    string `json:"name"` // ":name:"
	Icon    Image  `json:"icon"`
	ID      string `json:"id,omitempty"`
	Updated string `json:"updated,omitempty"`
	// License は upstream Misskey TS の AP renderer (renderEmoji) が
	// `_misskey_license: { freeText: ... }` 構造で federate する license 情報
	// (#731)。AP 標準には emoji license 概念が無いので Misskey 拡張として
	// 送受信する。nil = 受信時 license フィールド無し / 送信時は wrapper を
	// 出さない、non-nil = wrapper を出力 (FreeText が空でも対称性のため
	// オブジェクトは出す)。`,omitempty` はポインタ nil でフィールド省略する。
	License *MisskeyLicense `json:"_misskey_license,omitempty"`
}

// MisskeyLicense is the upstream `_misskey_license` AP extension wrapper.
// Misskey TS の renderEmoji 互換 (`{freeText: emoji.license}`)。
type MisskeyLicense struct {
	FreeText *string `json:"freeText"`
}

// Activity is the base type embedded by all activity types.
type Activity struct {
	Object
	Actor     string   `json:"actor"`
	Published string   `json:"published,omitempty"`
	To        []string `json:"to,omitempty"`
	CC        []string `json:"cc,omitempty"`
}

// Create wraps a Note (or other Object) inside a Create activity.
type Create struct {
	Activity
	Object any `json:"object"`
}

// Follow represents a Follow activity.
type Follow struct {
	Activity
	Object string `json:"object"` // followee actor URI
}

// Block represents a Block activity. object is the blockee actor URI.
type Block struct {
	Activity
	Object string `json:"object"` // blockee actor URI
}

// Accept represents an Accept activity.
type Accept struct {
	Activity
	Object any `json:"object"`
	// Result は QuoteRequest への Accept で承認の URI を運ぶ (FEP-044f、#3234)。
	// Follow への Accept では空 (omitempty で出ない)。
	Result string `json:"result,omitempty"`
}

// InteractionPolicy is the FEP-044f `interactionPolicy` of an object
// (GoToSocial vocabulary, as used by Mastodon).
type InteractionPolicy struct {
	CanQuote *InteractionRule `json:"canQuote,omitempty"`
}

// InteractionRule lists who may interact without and with manual approval.
type InteractionRule struct {
	AutomaticApproval []string `json:"automaticApproval,omitempty"`
	ManualApproval    []string `json:"manualApproval,omitempty"`
}

// QuoteAuthorization is the FEP-044f approval stamp a quoted author publishes
// for a quote post (#3234). It references, and never embeds, the quote post
// (interactingObject) and the quoted post (interactionTarget).
type QuoteAuthorization struct {
	Object
	AttributedTo      string `json:"attributedTo"`
	InteractingObject string `json:"interactingObject"`
	InteractionTarget string `json:"interactionTarget"`
}

// Reject represents a Reject activity.
type Reject struct {
	Activity
	Object any `json:"object"`
}

// ChatRoomGroup is the ActivityStreams `Group` object representing a chat
// room in CherryPick group chat federation. It is used as the object of
// Invite / Accept / Reject activities and mirrors CherryPick
// ApRendererService.renderChatRoom (type=Group, id=room URI, attributedTo=owner).
type ChatRoomGroup struct {
	Object
	Summary      string `json:"summary,omitempty"`
	AttributedTo string `json:"attributedTo,omitempty"`
}

// Invite represents an Invite activity used to invite a (typically remote)
// user into a chat room (CherryPick group chat federation). object is the
// ChatRoomGroup, target is the invitee's actor URI.
type Invite struct {
	Activity
	Object any    `json:"object"`
	Target string `json:"target,omitempty"`
}

// Remove represents a Remove activity. CherryPick group chat federation uses
// it to announce a member leaving a chat room: actor and object are the
// leaving member's actor URI, target is the room URI. Mirrors
// ApRendererService.renderRemove. featured collection からの note unpin にも使う
// (target=featured collection, object=note URI、#2024)。
type Remove struct {
	Activity
	Object any    `json:"object"`
	Target string `json:"target,omitempty"`
}

// Add represents an Add activity, used to federate a note pin into the actor's
// featured collection (target=featured collection, object=note URI). Mirrors
// ApRendererService.renderAdd (#2024)。
type Add struct {
	Activity
	Object any    `json:"object"`
	Target string `json:"target,omitempty"`
}

// Undo represents an Undo activity.
type Undo struct {
	Activity
	Object any `json:"object"`
}

// Delete represents a Delete activity.
type Delete struct {
	Activity
	Object any `json:"object"`
}

// Update represents an Update activity.
type Update struct {
	Activity
	Object any `json:"object"`
}

// QuoteRequest asks the author of a note for permission to quote it
// (FEP-044f、#3234)。object は引用される投稿、instrument は引用する投稿。
type QuoteRequest struct {
	Activity
	Object     string `json:"object"`
	Instrument any    `json:"instrument"`
}

// Like represents a Like (reaction) activity.
type Like struct {
	Activity
	Object          string `json:"object"`                      // target note URI
	Content         string `json:"content,omitempty"`           // reaction emoji (standard)
	MisskeyReaction string `json:"_misskey_reaction,omitempty"` // Misskey拡張: contentより優先
	// Tag は Misskey/CherryPick がカスタム絵文字リアクションを連合させる
	// 際に乗せる Emoji オブジェクト群。Note ingestion と同じ形なので
	// federation.extractEmojiTags / upsertEmojis でそのまま処理できる。
	Tag APObjectList `json:"tag,omitempty"`
}

// Tombstone represents a deleted object placeholder used as the object of a
// Delete activity.
type Tombstone struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// Announce represents a Renote (boost) activity.
type Announce struct {
	Activity
	Object string `json:"object"` // target note URI
}

// Flag represents an abuse-report forwarding activity delivered from the
// local instance's system actor to the origin instance of a reported user.
// Content is the moderator-visible comment; Object is the target user's URI.
type Flag struct {
	Activity
	Content string `json:"content"`
	Object  string `json:"object"`
}

// Move represents an account-migration activity. Per Mastodon / Misskey
// convention, actor == object (the old account URI) and target is the new
// account URI. Follower instances use this signal to re-follow the target.
type Move struct {
	Activity
	Object string `json:"object"`
	Target string `json:"target"`
}

// MisskeyContext は Misskey/Mastodon/schema.org 拡張語彙を含むコンテキストオブジェクト。
// TS版 contexts.ts と同等。
var MisskeyContext = map[string]any{
	"misskey":                               "https://misskey-hub.net/ns#",
	"toot":                                  "http://joinmastodon.org/ns#",
	"schema":                                "http://schema.org/",
	"vcard":                                 "http://www.w3.org/2006/vcard/ns#",
	"Key":                                   SecurityContextURL + "#Key",
	"Hashtag":                               ContextURL + "#Hashtag",
	"sensitive":                             ContextURL + "#sensitive",
	"quoteUrl":                              "https://misskey-hub.net/ns#quoteUrl",
	"Emoji":                                 "toot:Emoji",
	"featured":                              "toot:featured",
	"discoverable":                          "toot:discoverable",
	"PropertyValue":                         "schema:PropertyValue",
	"value":                                 "schema:value",
	"isCat":                                 "misskey:isCat",
	"_misskey_content":                      "misskey:_misskey_content",
	"_misskey_quote":                        "misskey:_misskey_quote",
	"_misskey_reaction":                     "misskey:_misskey_reaction",
	"_misskey_talk":                         "misskey:_misskey_talk",
	"_misskey_canChat":                      "misskey:_misskey_canChat",
	"_misskey_votes":                        "misskey:_misskey_votes",
	"_misskey_summary":                      "misskey:_misskey_summary",
	"_misskey_followedMessage":              "misskey:_misskey_followedMessage",
	"_misskey_requireSigninToViewContents":  "misskey:_misskey_requireSigninToViewContents",
	"_misskey_makeNotesFollowersOnlyBefore": "misskey:_misskey_makeNotesFollowersOnlyBefore",
	"_misskey_makeNotesHiddenBefore":        "misskey:_misskey_makeNotesHiddenBefore",
	"_misskey_license":                      "misskey:_misskey_license",
	"freeText":                              map[string]string{"@id": "misskey:freeText", "@type": "schema:text"},
	// FEP-044f (引用の承認、#3234)。IRI は Mastodon と同じ (GoToSocial の語彙を含む)。
	"gts":                "https://gotosocial.org/ns#",
	"quote":              map[string]string{"@id": "https://w3id.org/fep/044f#quote", "@type": "@id"},
	"quoteAuthorization": map[string]string{"@id": "https://w3id.org/fep/044f#quoteAuthorization", "@type": "@id"},
	"QuoteRequest":       "https://w3id.org/fep/044f#QuoteRequest",
	"QuoteAuthorization": "https://w3id.org/fep/044f#QuoteAuthorization",
	"interactionPolicy":  map[string]string{"@id": "gts:interactionPolicy", "@type": "@id"},
	"canQuote":           map[string]string{"@id": "gts:canQuote", "@type": "@id"},
	"automaticApproval":  map[string]string{"@id": "gts:automaticApproval", "@type": "@id"},
	"manualApproval":     map[string]string{"@id": "gts:manualApproval", "@type": "@id"},
	"interactingObject":  map[string]string{"@id": "gts:interactingObject", "@type": "@id"},
	"interactionTarget":  map[string]string{"@id": "gts:interactionTarget", "@type": "@id"},
}

// fullContext は全AP出力で使われる完全なJSON-LDコンテキスト。
// 不変として扱うこと。各オブジェクトには newContext() で新しいコピーを渡す。
var fullContext = []any{ContextURL, SecurityContextURL, MisskeyContext}

// personContext は Person actor 専用の拡張コンテキスト。FEP-521a の
// `assertionMethod` (Multikey) を JSON-LD term として認識させるため、
// fullContext に Multikey / Data-Integrity vocabulary を append している。
// Note / Activity 等の他オブジェクトは Multikey を含まないので fullContext
// 側は変更せず、Person 出力のみ context を拡張する設計 (#1067 / #1069)。
var personContext = []any{
	ContextURL,
	SecurityContextURL,
	MultikeyContextURL,
	DataIntegrityContextURL,
	MisskeyContext,
}

// newContext returns a fresh copy of fullContext so callers cannot
// accidentally mutate the shared template via append.
func newContext() []any {
	c := make([]any, len(fullContext))
	copy(c, fullContext)
	return c
}

// newPersonContext returns a fresh copy of personContext (= fullContext +
// Multikey / Data-Integrity vocab) for Person actor output.
func newPersonContext() []any {
	c := make([]any, len(personContext))
	copy(c, personContext)
	return c
}

// AddContext attaches the standard AS+security+Misskey context to any object
// that embeds Object. 配列で持つことで複数 vocabulary を表現する。
// Person 専用には Multikey / Data-Integrity vocabulary を追加で含めて、
// `assertionMethod` term を JSON-LD として正しく解釈可能にする。
func AddContext(o any) {
	ctx := newContext()
	switch v := o.(type) {
	case *Person:
		v.Context = newPersonContext()
		return
	case *Note:
		v.Context = ctx
	case *Create:
		v.Context = ctx
	case *Follow:
		v.Context = ctx
	case *Block:
		v.Context = ctx
	case *Accept:
		v.Context = ctx
	case *QuoteAuthorization:
		v.Context = ctx
	case *QuoteRequest:
		v.Context = ctx
	case *Reject:
		v.Context = ctx
	case *Invite:
		v.Context = ctx
	case *Remove:
		v.Context = ctx
	case *Add:
		v.Context = ctx
	case *Undo:
		v.Context = ctx
	case *Delete:
		v.Context = ctx
	case *Update:
		v.Context = ctx
	case *Like:
		v.Context = ctx
	case *Announce:
		v.Context = ctx
	case *Flag:
		v.Context = ctx
	case *Move:
		v.Context = ctx
	case *OrderedCollection:
		v.Context = ctx
	case *OrderedCollectionPage:
		v.Context = ctx
	}
}
