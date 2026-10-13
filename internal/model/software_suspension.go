package model

import "encoding/json"

// SuspendedSoftwareEntry is one entry of meta.deliverSuspendedSoftware:
// federation delivery to instances running the matching software (+ version
// range) is suspended. Misskey TS の SoftwareSuspension に対応。マッチ判定は
// core/federation.MatchSuspendedSoftware が行う (#1732)。
type SuspendedSoftwareEntry struct {
	Software     string `json:"software"`
	VersionRange string `json:"versionRange"`
}

// ParseSuspendedSoftware decodes a deliverSuspendedSoftware JSON value,
// element by element. 読めない要素 (string でない値など) だけを飛ばし、読めた
// 要素は残す。値全体が配列として読めなければ空。
//
// **配送の判定と表示 (federation/instances など) で同じ読み方をする** (#3558)。
// 以前は表示が読めた分を使い、配送の判定は全体を捨てていたので、一部だけ壊れた
// 値で「画面では配送停止なのに配送が続く」状態になった。admin/update-meta は
// 形を検査せずに保存するので、この値は入りうる。
func ParseSuspendedSoftware(raw []byte) []SuspendedSoftwareEntry {
	if len(raw) == 0 {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	out := make([]SuspendedSoftwareEntry, 0, len(items))
	for _, item := range items {
		var e SuspendedSoftwareEntry
		if err := json.Unmarshal(item, &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out
}
