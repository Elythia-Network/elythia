package activitypub

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"io"
	"sort"
	"strings"
)

// semanticAPReferenceFields are ActivityPub fields whose values identify an
// actor, object, or embedded object. Arbitrary text fields (including content,
// summary, and profile properties) are deliberately never traversed.
var semanticAPReferenceFields = map[string]struct{}{
	"id": {}, "@id": {}, "actor": {}, "object": {}, "attributedTo": {},
	"to": {}, "cc": {}, "bto": {}, "bcc": {}, "audience": {}, "inReplyTo": {},
	"quoteUrl": {}, "_misskey_quote": {}, "quote": {}, "quoteUri": {}, "quoteAuthorization": {},
	"tag": {}, "href": {}, "alsoKnownAs": {}, "movedTo": {}, "target": {}, "instrument": {}, "result": {},
	"interactingObject": {}, "interactionTarget": {}, "items": {}, "orderedItems": {},
}

// ExtractLocalAPReferences finds canonical local actor and note URIs in
// ActivityPub reference fields only. It performs no lookups or federation
// state changes. IDs are unique and sorted for deterministic validation.
//
// The result is exactly what decoding serialized with encoding/json into an
// `any` and walking the reference fields of that tree would return; a document
// that encoding/json cannot decode yields no references. It is shared by the
// inbox admission, the outbound delivery preflight, and the ActivityPub GET
// responses.
func ExtractLocalAPReferences(baseURL string, serialized []byte) (userIDs []string, noteIDs []string) {
	refs := localReferenceSet{baseURL: baseURL}
	// 受付・配送・GETのどれでも本文全体を木へdecodeするのが重かった(#3988)。
	// 意味のあるキーの下だけを読み、他の値は読み飛ばす。
	//
	// ストリームの走査は厳格に読む(重複したキー・不正なUTF-8を誤りにする)。
	// encoding/jsonは重複したキーを後勝ちで1つにし、不正なUTF-8をU+FFFDへ
	// 置き換えるので、そうした文書ではストリームで全部を見ると結果がずれる。
	// また encoding/json は float64 に収まらない数(1e400など)を any へ decode
	// できず、文書全体を誤りにする。走査はそうした数も見つけ次第やめる。
	// 厳格に読めなかった文書だけを従来の木の走査で読み直し、結果を今と
	// 同じにする。厳格に読めた文書は、encoding/jsonでも同じ木になる。
	if !refs.scan(serialized) {
		refs = localReferenceSet{baseURL: baseURL}
		if !refs.walkTree(serialized) {
			return nil, nil
		}
	}
	return refs.sorted()
}

// localReferenceSet accumulates the local user and note IDs found in reference
// fields.
type localReferenceSet struct {
	baseURL string
	users   map[string]struct{}
	notes   map[string]struct{}
}

func (s *localReferenceSet) add(value string) {
	if id := canonicalLocalReferenceID(s.baseURL, "/users/", value); id != "" {
		if s.users == nil {
			s.users = map[string]struct{}{}
		}
		s.users[id] = struct{}{}
	}
	if id := canonicalLocalReferenceID(s.baseURL, "/notes/", value); id != "" {
		if s.notes == nil {
			s.notes = map[string]struct{}{}
		}
		s.notes[id] = struct{}{}
	}
}

func (s *localReferenceSet) sorted() (userIDs []string, noteIDs []string) {
	for id := range s.users {
		userIDs = append(userIDs, id)
	}
	for id := range s.notes {
		noteIDs = append(noteIDs, id)
	}
	sort.Strings(userIDs)
	sort.Strings(noteIDs)
	return userIDs, noteIDs
}

// scan walks serialized as a token stream, reading only the values under
// reference fields. It reports false when the document is not strictly valid
// JSON (syntax error, trailing data, duplicate object names, invalid UTF-8,
// nesting too deep); the caller then falls back to walkTree.
func (s *localReferenceSet) scan(serialized []byte) bool {
	// 既定のOptionsは厳格(AllowDuplicateNames / AllowInvalidUTF8が偽)で、
	// 読み飛ばす値(skipValue)の中まで同じ検査をする。*bytes.Bufferを渡すと、
	// Decoderは本文を自分のバッファへ写さずに直接読む(書き換えはしない)。
	dec := jsontext.NewDecoder(bytes.NewBuffer(serialized))
	var unquoted []byte
	// unquote returns the decoded contents of the raw JSON string v.
	unquote := func(v jsontext.Value) ([]byte, bool) {
		raw := v[1 : len(v)-1]
		if bytes.IndexByte(raw, '\\') < 0 {
			// 厳格に読んでいるので、エスケープが無ければ中身がそのまま値になる。
			return raw, true
		}
		var err error
		unquoted, err = jsontext.AppendUnquote(unquoted[:0], v)
		return unquoted, err == nil
	}
	for {
		if depth := dec.StackDepth(); depth > 0 {
			if kind, length := dec.StackIndex(depth); kind == '{' && length%2 == 0 {
				// オブジェクトの中で、次はキーか '}'。
				if dec.PeekKind() == '}' {
					if _, err := dec.ReadToken(); err != nil {
						return false
					}
					if dec.StackDepth() == 0 {
						break
					}
					continue
				}
				name, err := dec.ReadValue()
				if err != nil || name.Kind() != '"' {
					return false
				}
				key, ok := unquote(name)
				if !ok {
					return false
				}
				if _, semantic := semanticAPReferenceFields[string(key)]; !semantic {
					if !skipValue(dec) {
						return false
					}
				}
				continue
			}
		}
		// ここで読む値は、最上位・走査する配列の要素・意味のあるキーの値のどれか。
		switch dec.PeekKind() {
		case '"':
			value, err := dec.ReadValue()
			if err != nil {
				return false
			}
			str, ok := unquote(value)
			if !ok {
				return false
			}
			// baseURLで始まらない値は参照にならない(canonicalLocalReferenceIDは
			// '#'の後ろを落としてから接頭辞を見るが、接頭辞の側は変わらない)。
			// 文字列への変換を、候補だけに絞る。
			if s.baseURL != "" && bytes.HasPrefix(str, []byte(s.baseURL)) {
				s.add(string(str))
			}
		case '{', '[', '}', ']':
			if _, err := dec.ReadToken(); err != nil {
				return false
			}
		default:
			// 数・真偽値・null、または読めない入力。読めない入力はここで誤りになる。
			if !skipValue(dec) {
				return false
			}
		}
		if dec.StackDepth() == 0 {
			break
		}
	}
	// 値の後ろに余計なデータがあれば、encoding/jsonは誤りにする。
	if _, err := dec.ReadToken(); err != io.EOF {
		return false
	}
	return true
}

// skipValue reads past the next value without collecting references. It
// reports false when the value cannot be read, or when it contains a number
// that encoding/json cannot decode into an `any` (out of float64's range).
func skipValue(dec *jsontext.Decoder) bool {
	// SkipValueは数の範囲を見ないので使えない。値を1つずつ読み、数だけ確かめる。
	// 文字列・名前はReadValueで読むので、unquoteも確保もしない。
	depth := dec.StackDepth()
	for {
		switch dec.PeekKind() {
		case '0':
			number, err := dec.ReadValue()
			if err != nil || !numberDecodes(number) {
				return false
			}
		case '"', 'n', 't', 'f':
			if _, err := dec.ReadValue(); err != nil {
				return false
			}
		case '{', '[', '}', ']':
			if _, err := dec.ReadToken(); err != nil {
				return false
			}
		default:
			return false
		}
		if dec.StackDepth() == depth {
			return true
		}
	}
}

// numberDecodes reports whether encoding/json decodes the JSON number into an
// `any` without error.
func numberDecodes(number jsontext.Value) bool {
	// 指数が無く桁も少なければ float64 に収まる(最大値は約1.8e308で309桁)。
	// それ以外だけ encoding/json に読ませ、判定をencoding/jsonと完全に揃える。
	if len(number) < 300 && !bytes.ContainsAny(number, "eE") {
		return true
	}
	var v any
	return json.Unmarshal(number, &v) == nil
}

// walkTree is the reference implementation: decode serialized with
// encoding/json into an `any` and walk the reference fields of the tree. It
// reports false when encoding/json cannot decode serialized.
func (s *localReferenceSet) walkTree(serialized []byte) bool {
	var document any
	if json.Unmarshal(serialized, &document) != nil {
		return false
	}
	var visit func(any)
	visit = func(value any) {
		switch value := value.(type) {
		case string:
			s.add(value)
		case []any:
			for _, item := range value {
				visit(item)
			}
		case map[string]any:
			for key, item := range value {
				if _, semantic := semanticAPReferenceFields[key]; semantic {
					visit(item)
				}
			}
		}
	}
	visit(document)
	return true
}

func canonicalLocalReferenceID(baseURL, path, uri string) string {
	if baseURL == "" || strings.Contains(uri, "?") {
		return ""
	}
	// AP actor keys and quote request IDs may append a fragment to the same
	// canonical path. Query strings and path suffixes are never accepted.
	uri, _, _ = strings.Cut(uri, "#")
	prefix := baseURL + path
	if !strings.HasPrefix(uri, prefix) {
		return ""
	}
	id := strings.TrimPrefix(uri, prefix)
	if path == "/notes/" {
		parts := strings.Split(id, "/")
		switch {
		case len(parts) == 1:
			// Canonical Note URI.
		case len(parts) == 2 && parts[1] == "activity":
			id = parts[0]
		case len(parts) == 3 && parts[1] == "quote-authorizations" && parts[2] != "" && !strings.ContainsAny(parts[2], "?#%"):
			id = parts[0]
		default:
			return ""
		}
	}
	if id == "" || strings.ContainsAny(id, "/?#%") {
		return ""
	}
	return id
}
