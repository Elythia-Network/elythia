package federation_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ingestNoteWithURL は `url` を持つ Create(Note) を流し、保存された note を返す。
func ingestNoteWithURL(t *testing.T, urlJSON string) *model.Note {
	t.Helper()
	p, _, _, noteRepo := newProcessor(t, aliceActor)
	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Note",
			"id": "https://remote.example/users/alice/statuses/1",
			"attributedTo": "https://remote.example/users/alice",
			"content": "hello",
			"url": ` + urlJSON + `,
			"to": ["https://www.w3.org/ns/activitystreams#Public"]
		}
	}`)
	require.NoError(t, p.Process(body))
	require.Len(t, noteRepo.Notes, 1)
	for _, n := range noteRepo.Notes {
		return n
	}
	return nil
}

// **AP の `id` と HTML の permalink は別物。** Mastodon 系では `id` が AP object、
// `url` が Web ページを指すので、保存しないとクライアントが原文へ辿れない (#2729)。
func TestIngest_StoresNoteURL(t *testing.T) {
	got := ingestNoteWithURL(t, `"https://remote.example/@alice/1"`)
	require.NotNil(t, got.URL)
	assert.Equal(t, "https://remote.example/@alice/1", *got.URL)
	// uri は AP object の id のまま (別物であることの確認)。
	require.NotNil(t, got.URI)
	assert.Equal(t, "https://remote.example/users/alice/statuses/1", *got.URI)
}

// 読み方は upstream の `getOneApHrefNullable` と同じ — 配列なら先頭、object なら
// `href`。**`APLenientHref` は `id` を見ない** (**単一キーの** `{"@id": ...}` は
// inbox 経路だと手前で string に潰れるので別扱い。2 キーあると潰れない。
// `TestNoteURL_JSONLDExpandedFormsArePathDependent`)。
func TestIngest_NoteURLShapes(t *testing.T) {
	cases := map[string]struct {
		in   string
		want string
	}{
		"string":        {`"https://remote.example/@a/1"`, "https://remote.example/@a/1"},
		"object の href": {`{"type":"Link","href":"https://remote.example/@a/1"}`, "https://remote.example/@a/1"},
		"配列は先頭":         {`["https://remote.example/@a/1","https://remote.example/@a/2"]`, "https://remote.example/@a/1"},
		"配列の先頭が object": {`[{"type":"Link","href":"https://remote.example/@a/1"}]`, "https://remote.example/@a/1"},
		"object に href が無ければ捨てる": {`{"type":"Link","id":"https://remote.example/@a/1"}`, ""},
		"空配列は捨てる":                {`[]`, ""},
		"数値は捨てる":                 {`123`, ""},
		"null は捨てる":              {`null`, ""},
		"href が string でなければ捨てる": {`{"href":123}`, ""},
		// **upstream は空文字を保存する** (`if (url && !checkHttps(url))` は
		// falsy を素通りし、`if (data.url != null)` で `""` が入る)。mk-go は
		// 捨てる — 空の permalink はどこも指さない (#2729)。
		"空文字は捨てる": {`""`, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := ingestNoteWithURL(t, tc.in)
			if tc.want == "" {
				assert.Nil(t, got.URL)
				return
			}
			require.NotNil(t, got.URL)
			assert.Equal(t, tc.want, *got.URL)
		})
	}
}

// **http(s) 以外は値だけ捨てて note は作る** (#2729)。upstream は note ごと
// reject するが、permalink の scheme が変なだけで本文ごと失うほうが害が大きい。
// **`javascript:` を保存しない点は upstream と同等** — upstream は throw するので
// 結果として保存されない。この検査が無いと mk-go だけが保存してしまうので要る。
func TestIngest_DropsNonHTTPNoteURL(t *testing.T) {
	for _, raw := range []string{
		`"javascript:alert(1)"`,
		`"data:text/html,<script>alert(1)</script>"`,
		`"ftp://remote.example/x"`,
		`"/@alice/1"`,
	} {
		t.Run(raw, func(t *testing.T) {
			got := ingestNoteWithURL(t, raw)
			assert.Nil(t, got.URL)
			// note 自体は作られる。
			require.NotNil(t, got.URI)
		})
	}
}

// **mk-go が保存する scheme を固定する。** `http://` と大文字 scheme は
// **upstream なら note ごと落ちる**値だが、mk-go は保存する (#2729)。
//
// 固定しないと、divergence の表を読んで「捨てるはず」と実装を直したときに
// **無検出で挙動が変わる**。scheme を case-insensitive に見るのは RFC 3986 の
// ためで、`internal/core/urlpreview` の `isHTTPScheme` と同じ方針。
func TestIngest_KeepsHTTPAndUppercaseSchemeNoteURL(t *testing.T) {
	for _, raw := range []string{
		"https://remote.example/@alice/1",
		"HTTPS://remote.example/@alice/1",
		"http://remote.example/@alice/1",
		"HTTP://remote.example/@alice/1",
	} {
		t.Run(raw, func(t *testing.T) {
			got := ingestNoteWithURL(t, `"`+raw+`"`)
			require.NotNil(t, got.URL)
			// **正規化しない** — 受けた文字列をそのまま保存する。
			assert.Equal(t, raw, *got.URL)
		})
	}
}

// `note.url` は varchar(512)。**切ると別の URL になる**ので値ごと捨てて note は
// 作る (#2723 の「URL / ID 系」の規則)。
func TestIngest_DropsOversizedNoteURL(t *testing.T) {
	// **全角で埋める。** ASCII だけだと byte で数える実装でも同じ結果になり、
	// rune 判定が守られていることを確かめられない (#2729 のレビュー 2 周目)。
	// permalink に非 ASCII が入る形 (`https://host/@ユーザー/…`) は実在する。
	long := "https://remote.example/" + strings.Repeat("あ", 500)
	require.Greater(t, len([]rune(long)), 512)
	got := ingestNoteWithURL(t, `"`+long+`"`)
	assert.Nil(t, got.URL)
	require.NotNil(t, got.URI)

	// **境界ちょうどと +1 の両方を見る** (resolver_test.go の既存の境界テストと
	// 同じ規約)。**砦は +1 (513) の側** — `noteURLMaxRunes` を 513 に広げる変異は
	// ここだけが殺す。ちょうど (512) が殺す `colfit.Fits` の `<= max` → `< max` は
	// 判定が共有なので、`internal/misc/colfit` と他の列の境界テストでも落ちる
	// (#2729 のレビュー 4 / 5 / 7 周目で実測)。
	fit := "https://remote.example/" + strings.Repeat("あ", 512-23)
	require.Equal(t, 512, len([]rune(fit)))
	// 512 rune / 1490 byte。byte で数える実装ならここで落ちる。
	require.Greater(t, len(fit), 512)
	got = ingestNoteWithURL(t, `"`+fit+`"`)
	require.NotNil(t, got.URL)
	assert.Equal(t, fit, *got.URL)

	over := fit + "あ"
	require.Equal(t, 513, len([]rune(over)))
	got = ingestNoteWithURL(t, `"`+over+`"`)
	assert.Nil(t, got.URL, "1 rune でも溢れたら捨てる (列は 512)")
}

// NUL 入りは列に入らないので捨てる (`fitsColumn`)。
func TestIngest_DropsNoteURLWithNUL(t *testing.T) {
	got := ingestNoteWithURL(t, `"https://remote.example/\u0000x"`)
	assert.Nil(t, got.URL)
}

// inbound Update(Note) でも permalink を追従する (#2729)。
func TestUpdateRemoteNote_UpdatesURL(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	uri := "https://remote.example/notes/n1"
	host := "remote.example"
	old := "https://remote.example/@alice/old"
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", URI: &uri, UserID: "alice-id", UserHost: &host, URL: &old,
	}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)

	body := []byte(`{"id":"https://remote.example/notes/n1","type":"Note",
		"attributedTo":"https://remote.example/users/alice","content":"x",
		"url":"https://remote.example/@alice/new"}`)
	got, err := r.UpdateRemoteNote(body, "")
	require.NoError(t, err)
	require.NotNil(t, got.URL)
	assert.Equal(t, "https://remote.example/@alice/new", *got.URL)
	// **`fields` に載ったことまで見る。** 返り値 (`existing`) は `Notes["n1"]` と
	// 同じ pointer なので、in-memory の値を見ても「載せ忘れ」を検出できない。
	// 実 DB では `fields` に無い列は UPDATE 文に出ないので、**呼び出し側から
	// 見えている値と DB の値が食い違う** (#2729 のレビュー 2 周目)。
	require.Len(t, noteRepo.UpdateFieldsCalls, 1)
	// **型アサーションを裸で書かない。** 載っていないときに panic すると
	// testing がバイナリごと止まり、同じパッケージの残りが実行されない。
	v, ok := noteRepo.UpdateFieldsCalls[0]["url"].(*string)
	require.True(t, ok, "UPDATE に url が載っていない")
	assert.Equal(t, "https://remote.example/@alice/new", *v)
}

// **同じ値では UPDATE に載せない** (#2729)。載せると、他に変化が無い
// `Update(Note)` でも毎回 UPDATE を撃つことになる。
func TestUpdateRemoteNote_SameURLIsNotWrittenAgain(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	uri := "https://remote.example/notes/n1"
	host := "remote.example"
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", URI: &uri, UserID: "alice-id", UserHost: &host,
	}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)

	body := []byte(`{"id":"https://remote.example/notes/n1","type":"Note",
		"attributedTo":"https://remote.example/users/alice","content":"x",
		"url":"https://remote.example/@alice/1"}`)
	_, err := r.UpdateRemoteNote(body, "")
	require.NoError(t, err)
	require.Len(t, noteRepo.UpdateFieldsCalls, 1)
	_, ok := noteRepo.UpdateFieldsCalls[0]["url"]
	require.True(t, ok, "1 回目は載る")

	// 2 回目は url が同じなので載らない。
	_, err = r.UpdateRemoteNote(body, "")
	require.NoError(t, err)
	require.Len(t, noteRepo.UpdateFieldsCalls, 2)
	_, ok = noteRepo.UpdateFieldsCalls[1]["url"]
	assert.False(t, ok, "同じ値では UPDATE に載せない")
}

// **捨てられた値では上書きしない。** 読めない `url` が来ただけで、取り込み時に
// 保存した正しい permalink を消してはいけない (#2729)。
func TestUpdateRemoteNote_KeepsURLWhenNewOneIsDropped(t *testing.T) {
	for _, raw := range []string{`"javascript:alert(1)"`, `null`, `{"id":"https://x/y"}`} {
		t.Run(raw, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			noteRepo := testutil.NewMockNoteRepository()
			uri := "https://remote.example/notes/n1"
			host := "remote.example"
			stored := "https://remote.example/@alice/good"
			noteRepo.Notes["n1"] = &model.Note{
				ID: "n1", URI: &uri, UserID: "alice-id", UserHost: &host, URL: &stored,
			}
			urls := activitypub.NewURLBuilder("https://example.com")
			idGen, _ := id.NewGenerator("aidx")
			r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)

			body := []byte(`{"id":"https://remote.example/notes/n1","type":"Note",
				"attributedTo":"https://remote.example/users/alice","content":"x",
				"url":` + raw + `}`)
			got, err := r.UpdateRemoteNote(body, "")
			require.NoError(t, err)
			require.NotNil(t, got.URL)
			assert.Equal(t, stored, *got.URL)
			// `fields` に url を載せない (= UPDATE 文に出さない) ことまで見る。
			for _, f := range noteRepo.UpdateFieldsCalls {
				_, ok := f["url"]
				assert.False(t, ok, "捨てた値を UPDATE に載せない")
			}
		})
	}
}

// ingestNoteURLByRawFetch は `Normalize` を通らない生 fetch 経路 (`IngestNote`) で
// 同じ `url` を流し、保存された note を返す。
func ingestNoteURLByRawFetch(t *testing.T, urlJSON string) *model.Note {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(userRepo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	got, err := r.IngestNote([]byte(`{
		"type": "Note",
		"id": "https://remote.example/users/alice/statuses/1",
		"attributedTo": "https://remote.example/users/alice",
		"content": "hello",
		"url": ` + urlJSON + `,
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`))
	require.NoError(t, err)
	require.NotNil(t, got)
	return got
}

// **JSON-LD の展開形を `url` に置くと、経路によって結果が違う** (#2729)。
//
// AS2 の `@context` は `url` を `{"@id":"as:url","@type":"@id"}` と定義しているので
// 展開形は正規の表現で、架空の形ではない。upstream の `getApHrefNullable` は `href`
// しか見ないので展開形は `undefined` = 保存しないが、mk-go の inbox 経路は
// `Normalize` が先に剥がすため `APLenientHref` に読める形で届く。
//
// `Normalize` を通らない生 fetch 経路では剥がれないので、**同じ note でも入口に
// よって結末が変わる**。結末は 3 通りある — 余計に入る / 入るはずが入らない /
// **別の URL に差し替わる** (`@value` の置き換えが兄弟キーを見ないため)。
// 揃えるなら `Normalize` 側の話になるので、ここでは差を固定するに留める。
func TestNoteURL_JSONLDExpandedFormsArePathDependent(t *testing.T) {
	const atID = `{"@id":"https://remote.example/@a/1"}`

	// inbox 経路: 単一キーなら潰れて保存される。
	got := ingestNoteWithURL(t, atID)
	require.NotNil(t, got.URL)
	assert.Equal(t, "https://remote.example/@a/1", *got.URL)

	// 配列の要素も同じく潰れ、`APLenientHref` が先頭を採る。
	got = ingestNoteWithURL(t, `[`+atID+`]`)
	require.NotNil(t, got.URL)
	assert.Equal(t, "https://remote.example/@a/1", *got.URL)

	// **キーが 2 つあると潰れない** (`@type` 付きは AS2 の term 定義そのものの形)。
	// `id` は読まないので捨てる = upstream と同じ。
	got = ingestNoteWithURL(t, `{"@id":"https://remote.example/@a/1","@type":"@id"}`)
	assert.Nil(t, got.URL)

	// 生 fetch 経路では単一キーでも潰れない。
	got = ingestNoteURLByRawFetch(t, atID)
	assert.Nil(t, got.URL)
	// 同じ入口で `href` 形式なら保存されるので、経路そのものが url を捨てて
	// いるわけではない (この対比が無いと上の Nil が何も証明しない)。
	got = ingestNoteURLByRawFetch(t, `{"type":"Link","href":"https://remote.example/@a/1"}`)
	require.NotNil(t, got.URL)
	assert.Equal(t, "https://remote.example/@a/1", *got.URL)

	// **`@value` の潰しは `@id` と違って兄弟キーを見ない** (`jsonld.go` の
	// `x["@value"]` に `len(x) == 1` のガードが無い)。`@language` が付いても
	// 潰れるので、「2 キーなら潰れない」は `@id` 限定の規則 (#2729 のレビュー
	// 8 周目)。
	got = ingestNoteWithURL(t, `{"@value":"https://remote.example/@a/1","@language":"en"}`)
	require.NotNil(t, got.URL)
	assert.Equal(t, "https://remote.example/@a/1", *got.URL)

	// **差の向きは一方向ではない。** 兄弟キーを見ないので、`href` を持つ object
	// でも `@value` があると inbox 経路だけが値を失う (`"x"` は http(s) でない)。
	// upstream と生 fetch 経路は `href` を読んで保存する。
	const hrefWithValue = `{"href":"https://remote.example/@a/1","@value":"x"}`
	got = ingestNoteWithURL(t, hrefWithValue)
	assert.Nil(t, got.URL)
	got = ingestNoteURLByRawFetch(t, hrefWithValue)
	require.NotNil(t, got.URL)
	assert.Equal(t, "https://remote.example/@a/1", *got.URL)

	// **結末は「入る / 入らない」の 2 通りではない。** `@value` 側も http(s) なら
	// inbox 経路は `href` を捨ててそちらを permalink として保存する = **別の URL
	// に差し替わる**。欠けた permalink と違って表示上は正常に見えるので、こちらの
	// ほうが気付きにくい (#2729 のレビュー 9 周目)。
	const hrefWithURLValue = `{"href":"https://remote.example/@a/1","@value":"https://other.example/@b/2"}`
	got = ingestNoteWithURL(t, hrefWithURLValue)
	require.NotNil(t, got.URL)
	assert.Equal(t, "https://other.example/@b/2", *got.URL)
	got = ingestNoteURLByRawFetch(t, hrefWithURLValue)
	require.NotNil(t, got.URL)
	assert.Equal(t, "https://remote.example/@a/1", *got.URL)
}

// **`url` を持たない note でログを出さない** (#2729 のレビュー 18 周目)。
//
// `remoteNoteURL` は取り込む全 note で呼ばれる。Misskey どうしの連合では `url` は
// 常に無いので、空文字の早期 return を落とすと**取り込み 1 件ごとに warn が出る**。
// 保存される値は変わらない (空文字は scheme 判定でも落ちる) ため、この表明が無いと
// 早期 return を消す変異がどのテストにも掛からない。
//
// **出る側も見る。** 「出ない」だけだと、ログを別 logger に移したり文言を変えたり
// した時点でテストが緑のまま無検証になる (このファイルの `href` 形式の対比と同じ
// 規約。#2729 のレビュー 19 周目)。
//
// `slog.SetDefault` はプロセス global なので、このパッケージで `t.Parallel()` を
// 使わない前提に乗っている (現状 0 件。`internal/queue/processors` の
// `inbox_drop_logging_test.go` と同じ)。
func TestIngest_NoteWithoutURLDoesNotWarn(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	p, _, _, noteRepo := newProcessor(t, aliceActor)
	require.NoError(t, p.Process([]byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Note",
			"id": "https://remote.example/users/alice/statuses/1",
			"attributedTo": "https://remote.example/users/alice",
			"content": "hello",
			"to": ["https://www.w3.org/ns/activitystreams#Public"]
		}
	}`)))
	require.Len(t, noteRepo.Notes, 1)
	assert.NotContains(t, buf.String(), "dropping remote note url",
		"url を持たない note で warn が出ている (remoteNoteURL の空文字 early return を確認)")

	// positive control: 捨てる値なら同じ buf に載る。
	_ = ingestNoteWithURL(t, `"javascript:alert(1)"`)
	assert.Contains(t, buf.String(), "dropping remote note url",
		"捨てた url の warn が出ていない (この表明が無いと上の NotContains が空振りする)")
}
