package federation_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/core/federation"
)

// ExtractActivityFields は inbox gate と同じ unwrap+Normalize を **1 パス**で
// 通すこと (#2716 / #2724 review MEDIUM-4)。
//
// 生 body を直接見ると、配列 wrap / `as:type` / `{"@id":...}` で読めず、gate が
// 見ているものとログがずれる。1 パスなのは、破棄経路が「解決できる actor で
// HTTP 署名が通る」だけで到達でき、パス数がそのまま攻撃者に押せる CPU になるため。
func TestExtractActivityFields(t *testing.T) {
	cases := []struct {
		name                    string
		body                    string
		wantActor, wantID, want string
	}{
		{"plain", `{"id":"https://h/a/1","type":"Create","actor":"https://h/u"}`,
			"https://h/u", "https://h/a/1", "Create"},
		{"singleton array wrap", `[{"id":"https://h/a/2","type":"Announce","actor":"https://h/u"}]`,
			"https://h/u", "https://h/a/2", "Announce"},
		{"array type takes the first", `{"type":["Create","Public"],"actor":"https://h/u"}`,
			"https://h/u", "", "Create"},
		// `as:` 接頭辞は Normalize が畳む。生 body を見る実装だと読めない。
		{"as: prefixed terms", `{"as:type":"Follow","as:actor":"https://h/u"}`,
			"https://h/u", "", "Follow"},
		// actor が object 形式でも id を拾う (normalizeActor)。
		{"object actor", `{"type":"Create","actor":{"id":"https://h/u","type":"Person"}}`,
			"https://h/u", "", "Create"},
		{"missing", `{}`, "", "", ""},
		{"broken json", `{`, "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := federation.ExtractActivityFields([]byte(tc.body))
			assert.Equal(t, tc.wantActor, got.Actor, "actor")
			assert.Equal(t, tc.wantID, got.ID, "id")
			assert.Equal(t, tc.want, got.Type, "type")
		})
	}
}

// gate が使う個別の抽出と値が一致すること。ずれると「gate が見た actor」と
// 「ログに出た actor」が食い違い、破棄の追跡が嘘になる。
func TestExtractActivityFields_MatchesIndividualExtractors(t *testing.T) {
	for _, body := range []string{
		`{"id":"https://h/a/1","type":"Create","actor":"https://h/u"}`,
		`[{"id":"https://h/a/2","type":"Announce","actor":{"id":"https://h/u"}}]`,
		`{"as:type":"Follow","as:actor":"https://h/u"}`,
		`{`,
	} {
		f := federation.ExtractActivityFields([]byte(body))
		assert.Equal(t, federation.ExtractActorIRI([]byte(body)), f.Actor, body)
		assert.Equal(t, federation.ExtractActivityID([]byte(body)), f.ID, body)
	}
}
