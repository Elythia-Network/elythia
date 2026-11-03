package activitypub

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// Note on scope:
//
// piprate/json-gold によるフル JSON-LD パイプラインは、独自の DocumentLoader と
// Compact 出力の予測しにくさが欠点で、厳密な struct タグ駆動のパースとは相性が
// 悪い。ここでは Misskey 本家相当の "context-aware な軽量正規化" を実装し、
// 受信 JSON のキー名を canonical な短形式へ揃える。これにより既存の json.
// Unmarshal-based dispatcher を維持したまま、Mastodon 系の prefix 付きキーや
// IRI 直記述、type の配列形式、JSON-LD 言語マップを処理できる。

// asPrefix is the canonical IRI prefix for ActivityStreams 2.0 terms.
const asPrefix = "https://www.w3.org/ns/activitystreams#"

// secPrefix is the canonical IRI prefix for the W3C security vocabulary used
// by HTTP Signatures.
const secPrefix = "https://w3id.org/security#"

// canonicalTerms maps JSON-LD aliases / IRIs to short names that match the
// json struct tags used elsewhere in the package. キーは小文字化せずそのまま
// 比較する。
var canonicalTerms = map[string]string{
	// AS short forms (no-op, listed for completeness so the lookup works for
	// both directions).
	"type":                      "type",
	"actor":                     "actor",
	"object":                    "object",
	"target":                    "target",
	"to":                        "to",
	"cc":                        "cc",
	"bcc":                       "bcc",
	"id":                        "id",
	"name":                      "name",
	"summary":                   "summary",
	"content":                   "content",
	"inReplyTo":                 "inReplyTo",
	"published":                 "published",
	"updated":                   "updated",
	"sensitive":                 "sensitive",
	"attributedTo":              "attributedTo",
	"inbox":                     "inbox",
	"outbox":                    "outbox",
	"followers":                 "followers",
	"following":                 "following",
	"endpoints":                 "endpoints",
	"sharedInbox":               "sharedInbox",
	"preferredUsername":         "preferredUsername",
	"publicKey":                 "publicKey",
	"publicKeyPem":              "publicKeyPem",
	"owner":                     "owner",
	"href":                      "href",
	"url":                       "url",
	"icon":                      "icon",
	"image":                     "image",
	"tag":                       "tag",
	"attachment":                "attachment",
	"manuallyApprovesFollowers": "manuallyApprovesFollowers",
	"discoverable":              "discoverable",

	// "as:" prefix.
	"as:type":              "type",
	"as:actor":             "actor",
	"as:object":            "object",
	"as:target":            "target",
	"as:to":                "to",
	"as:cc":                "cc",
	"as:bcc":               "bcc",
	"as:name":              "name",
	"as:summary":           "summary",
	"as:content":           "content",
	"as:inReplyTo":         "inReplyTo",
	"as:published":         "published",
	"as:updated":           "updated",
	"as:sensitive":         "sensitive",
	"as:attributedTo":      "attributedTo",
	"as:inbox":             "inbox",
	"as:outbox":            "outbox",
	"as:followers":         "followers",
	"as:following":         "following",
	"as:endpoints":         "endpoints",
	"as:sharedInbox":       "sharedInbox",
	"as:preferredUsername": "preferredUsername",
	"as:url":               "url",
	"as:icon":              "icon",
	"as:image":             "image",
	"as:tag":               "tag",
	"as:attachment":        "attachment",

	// Security vocab prefix.
	"sec:publicKey":    "publicKey",
	"sec:publicKeyPem": "publicKeyPem",
	"sec:owner":        "owner",
}

// ErrConflictingKeys is returned when two different JSON keys collapse to the
// same canonical name with different values.
//
// **正規化は決定的でなければならない。** `actor` と `as:actor` はどちらも
// canonical `actor` へ畳まれるが、Go の map 走査順はランダムなので、両方を
// 持つ document は**呼び出しごとに違う結果**を返していた。inbox は同じ
// バイト列を複数回 (認可ゲート / dispatch / activity id の取り出し) 独立に
// 正規化するため、**ゲートが署名者の actor を見て通し、本処理が詐称 actor で
// 動く**組み合わせが成立していた (実測で約 10%)。届く先は
// `handleDelete` の actor 削除・note 削除、`handleCreate` の作者偽装など。
//
// 片方を優先する規則にしても「攻撃者が選んだ側が常に勝つ」だけなので、
// **両方あること自体を異常として拒否する**。正当な実装は compact 形式か
// expand 形式のどちらか一方しか出さない (同じ値が重複しているだけなら通す)。
var ErrConflictingKeys = errors.New("activitypub: conflicting json-ld keys")

// Normalize re-encodes a JSON-LD activity body so that AS terms appear in
// their canonical short form. 入力が JSON object でも JSON 配列でも受け付け、
// 不明なキー / scalar はそのまま透過する。
//
// Returns ErrConflictingKeys if two source keys collapse to the same canonical
// name with different values.
func Normalize(body []byte) ([]byte, error) {
	normalized, _, err := NormalizeDecoded(body)
	return normalized, err
}

// NormalizeDecoded is Normalize that also returns body as encoding/json decoded
// it into an `any`, before normalization. Normalization builds new values and
// never modifies the decoded one, so callers may inspect it as the raw body
// (decoded is nil when body is empty or err is non-nil).
func NormalizeDecoded(body []byte) (normalized []byte, decoded any, err error) {
	if len(body) == 0 {
		return body, nil, nil
	}
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, err
	}
	out, err := normalizeValue(raw)
	if err != nil {
		return nil, nil, err
	}
	normalized, err = json.Marshal(out)
	if err != nil {
		return nil, nil, err
	}
	return normalized, raw, nil
}

// setCanonical writes one canonical key, refusing a second source key that
// carries a different value.
func setCanonical(out map[string]any, key string, val any) error {
	if prev, dup := out[key]; dup && !reflect.DeepEqual(prev, val) {
		return fmt.Errorf("%w: %q", ErrConflictingKeys, key)
	}
	out[key] = val
	return nil
}

// normalizeValue is the recursive worker that transforms map keys and unwraps
// JSON-LD specific value containers (`@value`, type arrays, etc.).
func normalizeValue(v any) (any, error) {
	switch x := v.(type) {
	case map[string]any:
		// `{"@value": "foo"}` のような JSON-LD value object はスカラーに展開する。
		// `@language` などの annotation は捨てる (本実装は単一言語のみ扱う)。
		if val, ok := x["@value"]; ok {
			return normalizeValue(val)
		}
		// `{"@id": "..."}` を持つ object は実質的に IRI 参照なので id 文字列に
		// 縮約する。これにより `"object": {"@id": "https://..."}` のような形式
		// が string と同等に扱える。
		if id, ok := x["@id"]; ok && len(x) == 1 {
			if s, ok := id.(string); ok {
				return s, nil
			}
		}
		out := make(map[string]any, len(x))
		for k, child := range x {
			canonical, ok := canonicalKey(k)
			if !ok {
				switch k {
				case "@id":
					// @id 単独 unwrap には引っかからなかったので id として残す。
					canonical = "id"
				case "@type":
					// type 配列の処理は下の flattenType に任せる。
					canonical = "type"
				case "@context":
					// 標準 JSON-LD context (array / object / AS context URL) は後段
					// dispatcher が使わないので破棄する。ただし CherryPick group chat
					// は note の @context に chat room URI を string で載せる規約なので、
					// room URI の場合のみ保持して下流 (handleChatRoomMessageCreate) が
					// room を識別できるようにする (#1209)。
					if s, ok := child.(string); ok && strings.Contains(s, "/chat/rooms/") {
						if err := setCanonical(out, "@context", s); err != nil {
							return nil, err
						}
					}
					continue
				case "@value", "@language", "@graph", "@list":
					// 後段 dispatcher が使わない reserved keyword は破棄する。
					continue
				default:
					// 未知キーはそのまま残す (Misskey 拡張など unknown vocabulary
					// が落ちないようにする)。
					canonical = k
				}
			}
			normalizedChild, err := normalizeValue(child)
			if err != nil {
				return nil, err
			}
			// type が配列で来た場合は最初の AS type 文字列を採用する。
			if canonical == "type" {
				if s := flattenType(normalizedChild); s != "" {
					if err := setCanonical(out, canonical, s); err != nil {
						return nil, err
					}
					continue
				}
			}
			if err := setCanonical(out, canonical, normalizedChild); err != nil {
				return nil, err
			}
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			normalized, err := normalizeValue(item)
			if err != nil {
				return nil, err
			}
			out[i] = normalized
		}
		return out, nil
	default:
		return x, nil
	}
}

// canonicalKey resolves a JSON property name to its canonical short form.
// 戻り値の bool は「マッピングが見つかったか」を示す。
func canonicalKey(k string) (string, bool) {
	if v, ok := canonicalTerms[k]; ok {
		return v, true
	}
	// AS IRI の直記述 ("https://www.w3.org/ns/activitystreams#type") を短縮。
	if local, ok := strings.CutPrefix(k, asPrefix); ok && local != "" {
		if v, ok := canonicalTerms[local]; ok {
			return v, true
		}
		return local, true
	}
	if local, ok := strings.CutPrefix(k, secPrefix); ok && local != "" {
		if v, ok := canonicalTerms["sec:"+local]; ok {
			return v, true
		}
		return local, true
	}
	return "", false
}

// flattenType reduces a possibly-array AS type into a single string. 配列の
// 場合は**先頭要素**が文字列のときだけ採用する (先頭が非文字列なら空)。
// 文字列でも配列でもない値は空を返す。
func flattenType(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		// **先頭が string でなければ諦める。** upstream getApType と
		// `activitypub.APType` に揃える。走査して最初の string を拾うと、
		// upstream が型判定不能とする document を inbox 経由でだけ受け入れる
		// ことになる (#2662)。
		//
		// **両経路が完全に一致するわけではない。** `normalizeValue` は
		// `{"@id": ...}` を先に string へ潰すので、`[{"@id":"as:Person"}]` は
		// inbox 経由なら `as:Person`、生 fetch なら空になる。揃えたのは
		// 「先頭以外を走査するか」の一点。
		if len(x) > 0 {
			if s, ok := x[0].(string); ok {
				return s
			}
		}
	}
	return ""
}
