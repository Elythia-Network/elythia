package federation

import (
	"encoding/json"
	"errors"
)

// normalizeActor populates act.Actor when JSON tag-driven Unmarshal could not
// pull a plain string (= upstream Misskey #17340 fix; activity.actor may be
// either a string IRI or an embedded {"id": "..."} object). Pass the raw bytes
// that were Unmarshaled into act. No-op when Actor is already populated.
//
// 同 normalization は inner activity (Undo / Accept / Reject の object) にも
// 適用する必要があるため、handler 側でも inner.normalizeActor(act.Object) を
// 呼ぶ運用にする。
func (act *genericActivity) normalizeActor(raw json.RawMessage) {
	if act.Actor != "" {
		return
	}
	var probe struct {
		Actor json.RawMessage `json:"actor"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || len(probe.Actor) == 0 {
		return
	}
	if id, err := readObjectString(probe.Actor); err == nil {
		act.Actor = id
	}
}

// readObjectString reads an activity Object field that is either a plain
// string IRI or a nested object with an "id" field.
//
// **戻り値は `trimWHATWGURL` を通す。** 書き込み側 (`ingestNoteWithCreated` /
// `fetchActor`) が正規化した値を `note.uri` / `user.uri` に入れているので、
// 読み出し側が生値のままだと `FindByURI` が miss する。id に改行を付ける実装の
// ノートが「取り込めるが Delete / Update / ピン留めが一切届かない」状態になる
// (#2662)。upstream は書き込みも読み出しも生値なので対称。
func readObjectString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("missing object")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		if s = trimWHATWGURL(s); s != "" {
			return s, nil
		}
		return "", errors.New("object missing id")
	}
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", err
	}
	obj.ID = trimWHATWGURL(obj.ID)
	if obj.ID == "" {
		return "", errors.New("object missing id")
	}
	return obj.ID, nil
}

// readApHref mirrors upstream getApHrefNullable: it returns the value when it is
// a bare URI string, or its `href` property when it is an AS Link/object, and ""
// otherwise (including {id}-only objects, which upstream also treats as absent).
// Used for fields like Move.target that upstream reads via getApHrefNullable
// rather than getApId (#1948-21).
func readApHref(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj struct {
		Href string `json:"href"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj.Href
	}
	return ""
}

// readActorString reads an activity's actor field, supporting both string and
// object forms (mirrors readObjectString but on the actor field of an inner
// activity such as the Follow inside a Reject).
func readActorString(act genericActivity) (string, error) {
	if act.Actor != "" {
		return act.Actor, nil
	}
	return "", errors.New("inner activity missing actor")
}
