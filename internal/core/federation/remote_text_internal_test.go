package federation

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/testutil"
)

// remoteText は NUL を落とし、指定があれば rune 単位で切ること (#2723)。
func TestRemoteText(t *testing.T) {
	nul := string(rune(0))
	cases := []struct {
		name string
		raw  string
		max  int
		want string
	}{
		{"passes through", "hello", 10, "hello"},
		{"drops NUL", "he" + nul + "llo", 10, "hello"},
		{"truncates by rune", strings.Repeat("\u3042", 20), 5, strings.Repeat("\u3042", 5)},
		{"max<=0 keeps length", strings.Repeat("\u3042", 20), 0, strings.Repeat("\u3042", 20)},
		{"NUL without truncation", "a" + nul + "b", 0, "ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := remoteText(tc.raw, tc.max)
			assert.Equal(t, tc.want, got)
			assert.True(t, utf8.ValidString(got))
			assert.NotContains(t, got, nul)
		})
	}
}

// ingestCWEnv builds a resolver whose fetches resolve one remote actor.
func ingestCWEnv(t *testing.T) (*Resolver, *testutil.MockNoteRepository) {
	t.Helper()
	noteRepo := testutil.NewMockNoteRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	fetcher := &countingFetcher{docs: map[string]string{
		"https://remote.example/users/cw": featuredActorDoc("remote.example", "cw", ""),
	}}
	r := NewResolver(testutil.NewMockUserRepository(), noteRepo,
		activitypub.NewURLBuilder("https://example.com"), fetcher, idGen)
	return r, noteRepo
}

// quoteJSON renders s as a JSON string literal.
func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// 長い CW でも取り込みが失敗しないこと (#2723)。
//
// `note.cw` は varchar(512)。CW は**相手が自由に決められる値**で、長さの制限は
// 送信側の実装次第。切らずに入れると Create ごと 22001 で落ち、ingest が error を
// 返す (トップレベル配送ならその inbox job は retry を使い切って dead になる) —
// 添付 1 枚が消えるより重い。
func TestIngestNote_TruncatesLongCW(t *testing.T) {
	r, noteRepo := ingestCWEnv(t)
	// 先頭と末尾を別の文字にする (末尾から切る実装を見逃さないため)。
	cw := "\u3055\u304d" + strings.Repeat("\u3042", 900) + "\u304a\u308f\u308a"
	doc := `{"@context":"https://www.w3.org/ns/activitystreams","id":"https://remote.example/notes/cw1",` +
		`"type":"Note","attributedTo":"https://remote.example/users/cw","content":"<p>hi</p>",` +
		`"summary":` + quoteJSON(cw) + `,"to":["https://www.w3.org/ns/activitystreams#Public"]}`

	note, err := r.IngestNote([]byte(doc))
	require.NoError(t, err, "長い CW で取り込みが失敗している")
	require.NotNil(t, note)
	require.NotNil(t, note.CW)
	// **定数ではなく列の実値 (512) で assert する。** 定数を assert すると、
	// 定数を動かしたときに両側が一緒に動いて検査が空振りする。列の上限そのものは
	// internal/repository の TestNote_CWColumnLimit... が schema から固定する。
	assert.Equal(t, 512, len([]rune(*note.CW)), "列の上限で切られていない")
	assert.True(t, strings.HasPrefix(*note.CW, "\u3055\u304d"), "先頭から切っていない")
	assert.Len(t, noteRepo.Notes, 1)
}

// CW / text の NUL を落とすこと (#2723)。
//
// `note.text` は無制限の text 列なので長さは問題にならないが、NUL は 22021 で
// 同じく行ごと落ちる。`content` 経路は mfm.FromHTML が落とすが、
// `_misskey_content` は FromHTML を通らない。
func TestIngestNote_StripsNULFromTextAndCW(t *testing.T) {
	r, _ := ingestCWEnv(t)
	nul := string(rune(0))
	doc := `{"@context":"https://www.w3.org/ns/activitystreams","id":"https://remote.example/notes/cw2",` +
		`"type":"Note","attributedTo":"https://remote.example/users/cw",` +
		`"_misskey_content":` + quoteJSON("te"+nul+"xt") + `,"summary":` + quoteJSON("c"+nul+"w") + `,` +
		`"to":["https://www.w3.org/ns/activitystreams#Public"]}`

	note, err := r.IngestNote([]byte(doc))
	require.NoError(t, err)
	require.NotNil(t, note)
	require.NotNil(t, note.Text)
	assert.Equal(t, "text", *note.Text, "text の NUL が残っている")
	require.NotNil(t, note.CW)
	assert.Equal(t, "cw", *note.CW, "cw の NUL が残っている")
}

// Update 経路も同じく切り、NUL を落とすこと (#2723)。
//
// create だけ直しても、`Update(Note)` で長い CW が来ると UpdateFields ごと落ちて
// ingest が error を返す (create 側と同じ結末)。
func TestUpdateRemoteNote_TruncatesCWAndStripsNUL(t *testing.T) {
	r, noteRepo := ingestCWEnv(t)
	nul := string(rune(0))

	// まず短い CW で取り込む。
	base := `{"@context":"https://www.w3.org/ns/activitystreams","id":"https://remote.example/notes/up1",` +
		`"type":"Note","attributedTo":"https://remote.example/users/cw","content":"<p>hi</p>",` +
		`"summary":"short","to":["https://www.w3.org/ns/activitystreams#Public"]}`
	note, err := r.IngestNote([]byte(base))
	require.NoError(t, err)
	require.NotNil(t, note)

	// 長い CW + NUL 入りの本文で Update する。
	cw := "\u3055\u304d" + strings.Repeat("\u3042", 900)
	upd := `{"@context":"https://www.w3.org/ns/activitystreams","id":"https://remote.example/notes/up1",` +
		`"type":"Note","attributedTo":"https://remote.example/users/cw",` +
		`"_misskey_content":` + quoteJSON("te"+nul+"xt") + `,"summary":` + quoteJSON(cw) + `,` +
		`"to":["https://www.w3.org/ns/activitystreams#Public"]}`
	updated, err := r.UpdateRemoteNote([]byte(upd), "https://remote.example/users/cw")
	require.NoError(t, err, "Update で取り込みが失敗している")
	require.NotNil(t, updated)

	require.NotNil(t, updated.CW)
	assert.Equal(t, 512, len([]rune(*updated.CW)), "Update 経路で切られていない")
	assert.True(t, strings.HasPrefix(*updated.CW, "\u3055\u304d"), "先頭から切っていない")
	require.NotNil(t, updated.Text)
	assert.Equal(t, "text", *updated.Text, "Update 経路で text の NUL が残っている")

	stored := noteRepo.Notes[note.ID]
	require.NotNil(t, stored)
	require.NotNil(t, stored.CW)
	assert.Equal(t, 512, len([]rune(*stored.CW)))
}

// 列に入らない id の Note は取り込まないこと (#2723)。
//
// `note.uri` は varchar(512)。**切ると別の note を指す URI になる**うえ、dedup
// (`FindByURI`) の鍵でもある。切らずに拒否する。
func TestIngestNote_RejectsOversizedURI(t *testing.T) {
	r, noteRepo := ingestCWEnv(t)
	longID := "https://remote.example/notes/" + strings.Repeat("n", 512)
	doc := `{"@context":"https://www.w3.org/ns/activitystreams","id":` + quoteJSON(longID) + `,` +
		`"type":"Note","attributedTo":"https://remote.example/users/cw","content":"<p>hi</p>",` +
		`"to":["https://www.w3.org/ns/activitystreams#Public"]}`

	_, err := r.IngestNote([]byte(doc))
	require.Error(t, err, "列に入らない id の Note を受理している")
	assert.ErrorIs(t, err, ErrInvalidNote)
	// **permanent 分類であること。** この関数は `ResolveNote` からも来るので、
	// `isPermanentSkipError` を通すハンドラはこの分類を見て ack する。transient に
	// 落ちると、同じ document を取り直す activity が retry のたびに走る。
	//
	// **ack はそれらの経路だけ**で、結末は呼び出し元によって 4 種類に分かれる
	// (一覧は `ingestNoteWithCreated` の gate のコメント)。どちらにも一般化しないこと。
	assert.True(t, isPermanentSkipError(err), "permanent 分類から外れている")
	assert.Empty(t, noteRepo.Notes)
}

// NUL を含む id も同じ扱い (#2723)。
//
// **DB を引く前に弾くこと。** `FindByURI` に NUL を渡すと SELECT 自体が 22021 で
// 落ちる。
func TestIngestNote_RejectsURIWithNUL(t *testing.T) {
	r, noteRepo := ingestCWEnv(t)
	doc := `{"@context":"https://www.w3.org/ns/activitystreams",` +
		`"id":` + quoteJSON("https://remote.example/notes/a\x00b") + `,` +
		`"type":"Note","attributedTo":"https://remote.example/users/cw","content":"<p>hi</p>",` +
		`"to":["https://www.w3.org/ns/activitystreams#Public"]}`

	_, err := r.IngestNote([]byte(doc))
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidNote)
	assert.Empty(t, noteRepo.Notes)
}

// ちょうど列に収まる id は受理すること (境界を 1 つずらす実装を弾く)。
func TestIngestNote_AcceptsURIAtColumnLimit(t *testing.T) {
	r, noteRepo := ingestCWEnv(t)
	prefix := "https://remote.example/notes/"
	// 512 は migration の列長そのもの。定数を参照すると両側が一緒に動く。
	id := prefix + strings.Repeat("n", 512-len(prefix))
	require.Len(t, []rune(id), 512)
	doc := `{"@context":"https://www.w3.org/ns/activitystreams","id":` + quoteJSON(id) + `,` +
		`"type":"Note","attributedTo":"https://remote.example/users/cw","content":"<p>hi</p>",` +
		`"to":["https://www.w3.org/ns/activitystreams#Public"]}`

	note, err := r.IngestNote([]byte(doc))
	require.NoError(t, err)
	require.NotNil(t, note)
	assert.Len(t, noteRepo.Notes, 1)
}

// NUL だけの summary で CW を付けないこと (#2723)。
//
// `cw = ""` は Misskey では「ラベル無しの CW」= 本文を折りたたむ指示になる。
// NUL を落とした結果が空になっただけで折りたたむのは、送信側の意図と違う。
// `text` 側は同じ書き分けをしているので、揃えないと非対称になる。
func TestIngestNote_DoesNotSetEmptyCWFromNULOnlySummary(t *testing.T) {
	r, noteRepo := ingestCWEnv(t)
	doc := `{"@context":"https://www.w3.org/ns/activitystreams","id":"https://remote.example/notes/nulcw",` +
		`"type":"Note","attributedTo":"https://remote.example/users/cw","content":"<p>hi</p>",` +
		`"summary":` + quoteJSON("\x00\x00") + `,"to":["https://www.w3.org/ns/activitystreams#Public"]}`

	note, err := r.IngestNote([]byte(doc))
	require.NoError(t, err)
	require.NotNil(t, note)
	assert.Nil(t, note.CW, "空になった CW を付けている (本文が折りたたまれる)")
	assert.Len(t, noteRepo.Notes, 1)
}

// sensitive なら空 CW を付ける既存の挙動は変えないこと。
//
// 上の書き分けで `note.CW == nil` のまま来るので、sensitive の分岐がそのまま効く。
func TestIngestNote_SensitiveStillGetsEmptyCW(t *testing.T) {
	r, _ := ingestCWEnv(t)
	doc := `{"@context":"https://www.w3.org/ns/activitystreams","id":"https://remote.example/notes/nulcw2",` +
		`"type":"Note","attributedTo":"https://remote.example/users/cw","content":"<p>hi</p>",` +
		`"summary":` + quoteJSON("\x00") + `,"sensitive":true,` +
		`"to":["https://www.w3.org/ns/activitystreams#Public"]}`

	note, err := r.IngestNote([]byte(doc))
	require.NoError(t, err)
	require.NotNil(t, note)
	require.NotNil(t, note.CW, "sensitive の空 CW が落ちている")
	assert.Equal(t, "", *note.CW)
}
