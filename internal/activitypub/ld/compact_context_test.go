package ld

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// InboxCompactContext へ compact すると、送信側の context で定義された語は
// upstream と同じ短い名前に揃い、定義されていない語 (AS2 の `@vocab: "_:"` で
// blank node IRI になる = 署名に含まれない語) は `_:<name>` のキーになる。
func TestInboxCompactContext_UndefinedTermsBecomeBlankNodeKeys(t *testing.T) {
	doc := map[string]any{
		"@context": []any{
			"https://www.w3.org/ns/activitystreams",
			map[string]any{"mk": "https://misskey-hub.net/ns#", "mkBody": "mk:_misskey_content"},
		},
		"id":               "https://example.com/notes/1",
		"type":             "Note",
		"to":               []any{"https://www.w3.org/ns/activitystreams#Public"},
		"mkBody":           "signed",
		"_misskey_summary": "unsigned",
	}
	out, err := NewProcessor().Compact(doc, InboxCompactContext())
	require.NoError(t, err)

	assert.Equal(t, "signed", out["_misskey_content"], "同じ IRI は upstream の term 名に揃うこと")
	assert.NotContains(t, out, "_misskey_summary", "未定義語が短い名前で残っている")
	assert.Equal(t, "unsigned", out["_:_misskey_summary"])
	assert.Equal(t, "as:Public", out["to"])
	assert.Equal(t, "https://example.com/notes/1", out["id"])
}

// 呼び出しごとに別の値を返す (共有すると json-gold の処理状態が跨ぐ)。
func TestInboxCompactContext_FreshValue(t *testing.T) {
	a := InboxCompactContext()
	b := InboxCompactContext()
	a[2].(map[string]any)["isCat"] = "mutated"
	assert.Equal(t, "misskey:isCat", b[2].(map[string]any)["isCat"])
}
