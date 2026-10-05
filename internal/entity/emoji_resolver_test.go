package entity

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubEmojiLookup implements EmojiLookup for testing purposes.
type stubEmojiLookup struct {
	// host文字列("" = local) → 返却する絵文字一覧
	data  map[string][]*model.Emoji
	calls []struct {
		names []string
		host  *string
	}
}

func (s *stubEmojiLookup) FindManyByNamesAndHost(names []string, host *string) ([]*model.Emoji, error) {
	// 呼び出し記録を保存(assertionで使う)
	cp := make([]string, len(names))
	copy(cp, names)
	s.calls = append(s.calls, struct {
		names []string
		host  *string
	}{names: cp, host: host})

	h := ""
	if host != nil {
		h = *host
	}
	return s.data[h], nil
}

// strPtr is a helper that returns a pointer to a string literal.
func strPtr(s string) *string { return &s }

func TestNewEmojiResolver_BatchResolve(t *testing.T) {
	remoteHost := "remote.example"
	lookup := &stubEmojiLookup{
		data: map[string][]*model.Emoji{
			"remote.example": {
				{ID: "e1", Name: "thumbsup", Host: strPtr("remote.example"), PublicURL: "https://remote.example/emoji/thumbsup.png", OriginalURL: "https://remote.example/emoji/thumbsup_orig.png"},
				{ID: "e2", Name: "wave", Host: strPtr("remote.example"), PublicURL: "https://remote.example/emoji/wave.png", OriginalURL: "https://remote.example/emoji/wave_orig.png"},
			},
		},
	}

	notes := []*model.Note{
		{
			ID:       "n1",
			UserID:   "u1",
			UserHost: &remoteHost,
			Emojis:   model.StringArray{"thumbsup", "wave"},
		},
		{
			ID:       "n2",
			UserID:   "u1",
			UserHost: &remoteHost,
			Emojis:   model.StringArray{"thumbsup"}, // 重複する名前はbatchで一度だけfetch
		},
	}

	r := NewEmojiResolver(lookup, notes)

	// host別に1回だけbatch fetchされる
	require.Len(t, lookup.calls, 1)
	assert.ElementsMatch(t, []string{"thumbsup", "wave"}, lookup.calls[0].names)
	require.NotNil(t, lookup.calls[0].host)
	assert.Equal(t, "remote.example", *lookup.calls[0].host)

	// cacheにURLが格納されている
	assert.Equal(t, "https://remote.example/emoji/thumbsup.png", r.cache["thumbsup@remote.example"])
	assert.Equal(t, "https://remote.example/emoji/wave.png", r.cache["wave@remote.example"])
}

func TestPopulateNoteEmojis(t *testing.T) {
	remoteHost := "remote.example"
	lookup := &stubEmojiLookup{
		data: map[string][]*model.Emoji{
			"remote.example": {
				{ID: "e1", Name: "fire", Host: strPtr("remote.example"), PublicURL: "https://remote.example/emoji/fire.webp", OriginalURL: "https://remote.example/emoji/fire.png"},
				{ID: "e2", Name: "star", Host: strPtr("remote.example"), PublicURL: "https://remote.example/emoji/star.webp", OriginalURL: "https://remote.example/emoji/star.png"},
			},
		},
	}

	note := &model.Note{
		ID:       "n1",
		UserID:   "u1",
		UserHost: &remoteHost,
		Emojis:   model.StringArray{"fire", "star"},
	}

	r := NewEmojiResolver(lookup, []*model.Note{note})
	// remote note は packNoteAtDepth が Emojis=&{} を確保する。ここでは
	// resolver 単体を試すため同様に事前確保する (#1639)。
	entity := &NoteEntity{Emojis: &map[string]string{}}
	r.PopulateNoteEmojis(note, entity)

	// note.Emojisに対応するURLがentity.Emojisに設定される
	require.NotNil(t, entity.Emojis)
	assert.Equal(t, "https://remote.example/emoji/fire.webp", (*entity.Emojis)["fire"])
	assert.Equal(t, "https://remote.example/emoji/star.webp", (*entity.Emojis)["star"])
}

func TestPopulateNoteEmojis_Empty(t *testing.T) {
	lookup := &stubEmojiLookup{data: map[string][]*model.Emoji{}}

	note := &model.Note{
		ID:     "n1",
		UserID: "u1",
		Emojis: model.StringArray{}, // 空の絵文字リスト
	}

	r := NewEmojiResolver(lookup, []*model.Note{note})
	entity := &NoteEntity{Emojis: &map[string]string{"existing": "value"}}

	r.PopulateNoteEmojis(note, entity)

	// 空emojisの場合、entity.Emojisは変更されない
	assert.Equal(t, map[string]string{"existing": "value"}, *entity.Emojis)
}

func TestPopulateUserEmojis(t *testing.T) {
	remoteHost := "remote.example"
	lookup := &stubEmojiLookup{
		data: map[string][]*model.Emoji{
			"remote.example": {
				{ID: "e1", Name: "cat", Host: strPtr("remote.example"), PublicURL: "https://remote.example/emoji/cat.png", OriginalURL: "https://remote.example/emoji/cat_orig.png"},
			},
		},
	}

	user := &model.User{
		ID:       "u1",
		Username: "alice",
		Host:     &remoteHost,
		Emojis:   model.StringArray{"cat"},
	}

	// note経由でuserの絵文字もcacheに読み込ませる
	note := &model.Note{
		ID:       "n1",
		UserID:   "u1",
		UserHost: &remoteHost,
		User:     user,
		Emojis:   model.StringArray{},
	}

	r := NewEmojiResolver(lookup, []*model.Note{note})
	lite := &UserLite{}
	r.PopulateUserEmojis(user, lite)

	// user.Emojisに対応するURLがlite.Emojisに設定される
	require.NotNil(t, lite.Emojis)
	assert.Equal(t, "https://remote.example/emoji/cat.png", lite.Emojis["cat"])
}

func TestPopulateUserEmojis_Empty(t *testing.T) {
	lookup := &stubEmojiLookup{data: map[string][]*model.Emoji{}}

	user := &model.User{
		ID:       "u1",
		Username: "bob",
		Emojis:   model.StringArray{}, // 空の絵文字リスト
	}

	r := NewEmojiResolver(lookup, []*model.Note{})
	lite := &UserLite{Emojis: map[string]string{"existing": "value"}}
	r.PopulateUserEmojis(user, lite)

	// 空emojisの場合、lite.Emojisは変更されない
	assert.Equal(t, map[string]string{"existing": "value"}, lite.Emojis)
}

func TestEmojiResolver_NilLookup(t *testing.T) {
	remoteHost := "remote.example"
	note := &model.Note{
		ID:       "n1",
		UserID:   "u1",
		UserHost: &remoteHost,
		Emojis:   model.StringArray{"fire"},
	}

	// lookup==nilでも空cacheのresolverが返りpanicしない
	r := NewEmojiResolver(nil, []*model.Note{note})
	require.NotNil(t, r)
	assert.Empty(t, r.cache)

	// PopulateNoteEmojisはno-op
	entity := &NoteEntity{Emojis: &map[string]string{"keep": "this"}}
	r.PopulateNoteEmojis(note, entity)
	assert.Equal(t, map[string]string{"keep": "this"}, *entity.Emojis)

	// PopulateUserEmojisもno-op
	user := &model.User{
		ID:       "u1",
		Username: "alice",
		Host:     &remoteHost,
		Emojis:   model.StringArray{"fire"},
	}
	lite := &UserLite{Emojis: map[string]string{"keep": "this"}}
	r.PopulateUserEmojis(user, lite)
	assert.Equal(t, map[string]string{"keep": "this"}, lite.Emojis)
}

func TestEmojiResolver_NilReceiver(t *testing.T) {
	var r *EmojiResolver

	// nilレシーバのPopulateNoteEmojisはpanicしない
	note := &model.Note{
		ID:     "n1",
		UserID: "u1",
		Emojis: model.StringArray{"fire"},
	}
	entity := &NoteEntity{}
	assert.NotPanics(t, func() {
		r.PopulateNoteEmojis(note, entity)
	})

	// nilレシーバのPopulateUserEmojisもpanicしない
	user := &model.User{
		ID:       "u1",
		Username: "alice",
		Emojis:   model.StringArray{"fire"},
	}
	lite := &UserLite{}
	assert.NotPanics(t, func() {
		r.PopulateUserEmojis(user, lite)
	})
}

func TestEmojiResolver_LocalEmojis(t *testing.T) {
	lookup := &stubEmojiLookup{
		data: map[string][]*model.Emoji{
			// host=="" はローカル絵文字
			"": {
				{ID: "e1", Name: "local_emoji", Host: nil, PublicURL: "https://local.example/emoji/local_emoji.png", OriginalURL: "https://local.example/emoji/local_emoji_orig.png"},
			},
		},
	}

	// UserHost==nil (ローカルユーザーの投稿)
	note := &model.Note{
		ID:       "n1",
		UserID:   "u1",
		UserHost: nil,
		Emojis:   model.StringArray{"local_emoji"},
	}

	r := NewEmojiResolver(lookup, []*model.Note{note})

	// ローカル絵文字のfetchではhost=nilが渡される
	require.Len(t, lookup.calls, 1)
	assert.Nil(t, lookup.calls[0].host)

	// cacheに "name@" (空host) のキーで格納される
	assert.Equal(t, "https://local.example/emoji/local_emoji.png", r.cache["local_emoji@"])

	// #1639: local note (UserHost==nil) は emojis field を出さない。packNoteAtDepth
	// が Emojis=nil にするため、resolver は populate せず nil のままになる。
	entity := &NoteEntity{}
	r.PopulateNoteEmojis(note, entity)
	assert.Nil(t, entity.Emojis, "local note must omit emojis (#1639)")
}

func TestEmojiResolver_PublicURLFallback(t *testing.T) {
	remoteHost := "remote.example"
	lookup := &stubEmojiLookup{
		data: map[string][]*model.Emoji{
			"remote.example": {
				// PublicURLが空の場合はOriginalURLにフォールバック
				{ID: "e1", Name: "rare", Host: strPtr("remote.example"), PublicURL: "", OriginalURL: "https://remote.example/emoji/rare_orig.png"},
			},
		},
	}

	note := &model.Note{
		ID:       "n1",
		UserID:   "u1",
		UserHost: &remoteHost,
		Emojis:   model.StringArray{"rare"},
	}

	r := NewEmojiResolver(lookup, []*model.Note{note})

	// PublicURLが空なのでOriginalURLが使われる
	assert.Equal(t, "https://remote.example/emoji/rare_orig.png", r.cache["rare@remote.example"])

	entity := &NoteEntity{Emojis: &map[string]string{}}
	r.PopulateNoteEmojis(note, entity)
	require.NotNil(t, entity.Emojis)
	assert.Equal(t, "https://remote.example/emoji/rare_orig.png", (*entity.Emojis)["rare"])
}

func TestEmojiResolver_MultipleHosts(t *testing.T) {
	hostA := "alpha.example"
	hostB := "beta.example"
	lookup := &stubEmojiLookup{
		data: map[string][]*model.Emoji{
			"alpha.example": {
				{ID: "e1", Name: "smile", Host: strPtr("alpha.example"), PublicURL: "https://alpha.example/emoji/smile.png", OriginalURL: "https://alpha.example/emoji/smile_orig.png"},
			},
			"beta.example": {
				{ID: "e2", Name: "smile", Host: strPtr("beta.example"), PublicURL: "https://beta.example/emoji/smile.png", OriginalURL: "https://beta.example/emoji/smile_orig.png"},
				{ID: "e3", Name: "wave", Host: strPtr("beta.example"), PublicURL: "https://beta.example/emoji/wave.png", OriginalURL: "https://beta.example/emoji/wave_orig.png"},
			},
		},
	}

	notes := []*model.Note{
		{
			ID:       "n1",
			UserID:   "u1",
			UserHost: &hostA,
			Emojis:   model.StringArray{"smile"},
		},
		{
			ID:       "n2",
			UserID:   "u2",
			UserHost: &hostB,
			Emojis:   model.StringArray{"smile", "wave"},
		},
	}

	r := NewEmojiResolver(lookup, notes)

	// host別に2回batch fetchが呼ばれる
	require.Len(t, lookup.calls, 2)

	// 同名の絵文字でもhost別に異なるURLが解決される
	assert.Equal(t, "https://alpha.example/emoji/smile.png", r.cache["smile@alpha.example"])
	assert.Equal(t, "https://beta.example/emoji/smile.png", r.cache["smile@beta.example"])
	assert.Equal(t, "https://beta.example/emoji/wave.png", r.cache["wave@beta.example"])

	// note1のemojisはalpha.exampleから解決される
	entityA := &NoteEntity{Emojis: &map[string]string{}}
	r.PopulateNoteEmojis(notes[0], entityA)
	require.NotNil(t, entityA.Emojis)
	assert.Equal(t, "https://alpha.example/emoji/smile.png", (*entityA.Emojis)["smile"])

	// note2のemojisはbeta.exampleから解決される
	entityB := &NoteEntity{Emojis: &map[string]string{}}
	r.PopulateNoteEmojis(notes[1], entityB)
	require.NotNil(t, entityB.Emojis)
	assert.Equal(t, "https://beta.example/emoji/smile.png", (*entityB.Emojis)["smile"])
	assert.Equal(t, "https://beta.example/emoji/wave.png", (*entityB.Emojis)["wave"])
}

func TestNewEmojiResolver_NilNotesInSlice(t *testing.T) {
	lookup := &stubEmojiLookup{data: map[string][]*model.Emoji{}}

	// nilを含むスライスでもpanicしない
	notes := []*model.Note{nil, nil}
	r := NewEmojiResolver(lookup, notes)
	require.NotNil(t, r)
	assert.Empty(t, r.cache)
	// nilのnoteはスキップされるのでfetchは呼ばれない
	assert.Empty(t, lookup.calls)
}

func TestNewEmojiResolver_UserEmojisCollectedFromNoteUser(t *testing.T) {
	remoteHost := "remote.example"
	lookup := &stubEmojiLookup{
		data: map[string][]*model.Emoji{
			"remote.example": {
				{ID: "e1", Name: "note_emoji", Host: strPtr("remote.example"), PublicURL: "https://remote.example/emoji/note_emoji.png", OriginalURL: ""},
				{ID: "e2", Name: "user_emoji", Host: strPtr("remote.example"), PublicURL: "https://remote.example/emoji/user_emoji.png", OriginalURL: ""},
			},
		},
	}

	user := &model.User{
		ID:       "u1",
		Username: "alice",
		Host:     &remoteHost,
		Emojis:   model.StringArray{"user_emoji"},
	}

	note := &model.Note{
		ID:       "n1",
		UserID:   "u1",
		UserHost: &remoteHost,
		Emojis:   model.StringArray{"note_emoji"},
		User:     user,
	}

	r := NewEmojiResolver(lookup, []*model.Note{note})

	// noteのemojisとuserのemojisが共にcacheに格納される
	assert.Equal(t, "https://remote.example/emoji/note_emoji.png", r.cache["note_emoji@remote.example"])
	assert.Equal(t, "https://remote.example/emoji/user_emoji.png", r.cache["user_emoji@remote.example"])

	// 1回のbatch fetchで両方取得されている
	require.Len(t, lookup.calls, 1)
	assert.ElementsMatch(t, []string{"note_emoji", "user_emoji"}, lookup.calls[0].names)
}

func TestPopulateNoteEmojis_NilNoteOrEntity(t *testing.T) {
	lookup := &stubEmojiLookup{data: map[string][]*model.Emoji{}}
	r := NewEmojiResolver(lookup, []*model.Note{})

	// note==nilでもpanicしない
	entity := &NoteEntity{}
	assert.NotPanics(t, func() {
		r.PopulateNoteEmojis(nil, entity)
	})

	// entity==nilでもpanicしない
	note := &model.Note{ID: "n1", UserID: "u1", Emojis: model.StringArray{"x"}}
	assert.NotPanics(t, func() {
		r.PopulateNoteEmojis(note, nil)
	})
}

func TestPopulateUserEmojis_NilUserOrLite(t *testing.T) {
	lookup := &stubEmojiLookup{data: map[string][]*model.Emoji{}}
	r := NewEmojiResolver(lookup, []*model.Note{})

	// user==nilでもpanicしない
	lite := &UserLite{}
	assert.NotPanics(t, func() {
		r.PopulateUserEmojis(nil, lite)
	})

	// lite==nilでもpanicしない
	user := &model.User{ID: "u1", Username: "alice", Emojis: model.StringArray{"x"}}
	assert.NotPanics(t, func() {
		r.PopulateUserEmojis(user, nil)
	})
}

func TestPopulateNoteReactionEmojis(t *testing.T) {
	// #459: note.Reactions に :name@host: / :name@.: (canonical local) /
	// raw unicode が混在するケースで batch fetch + populate が動くこと。
	// mk-go の reaction_service.resolveReaction は local emoji を
	// `:name@.:` (TS 互換 canonical) で永続化するため、parser は `@.`
	// を空 host に正規化しなければならない (PR #463 review BUG-0001)。
	remoteHost := "remote.example"
	lookup := &stubEmojiLookup{
		data: map[string][]*model.Emoji{
			"remote.example": {
				{ID: "e1", Name: "smile", Host: strPtr("remote.example"), PublicURL: "https://remote.example/emoji/smile.png"},
				{ID: "e2", Name: "wave", Host: strPtr("remote.example"), PublicURL: "https://remote.example/emoji/wave.png"},
			},
			"": {
				{ID: "e3", Name: "local", Host: nil, PublicURL: "https://local.example/emoji/local.png"},
			},
		},
	}

	// canonical local (`:local@.:`) を含めて、本番で実際に DB に書かれる
	// 形式を網羅する。レガシー `:legacy_local:` (host 省略) も 1 件混ぜて
	// 後方互換が壊れていないことを確認する。
	reactionsJSON := []byte(`{":smile@remote.example:":2,":local@.:":1,":wave@remote.example:":1,"❤️":3}`)

	note := &model.Note{
		ID:        "n1",
		UserID:    "u1",
		UserHost:  &remoteHost,
		Reactions: reactionsJSON,
	}

	r := NewEmojiResolver(lookup, []*model.Note{note})
	entity := &NoteEntity{ReactionEmojis: map[string]string{}}
	r.PopulateNoteReactionEmojis(note, entity)

	require.NotNil(t, entity.ReactionEmojis)
	// remote: key は frontend lookup `reaction.substring(1, length-1)` 仕様で "smile@remote.example"
	assert.Equal(t, "https://remote.example/emoji/smile.png", entity.ReactionEmojis["smile@remote.example"])
	assert.Equal(t, "https://remote.example/emoji/wave.png", entity.ReactionEmojis["wave@remote.example"])
	// canonical local (`:local@.:`) は upstream の `!x.includes('@.')` filter
	// と同じく reactionEmojis に載せない (frontend が instance registry から
	// 解決する、#1781)。host="" の entry は除外される。
	_, hasLocal := entity.ReactionEmojis["local"]
	assert.False(t, hasLocal, "local custom emoji should not appear in reactionEmojis")
	// raw unicode は乗らない
	_, hasHeart := entity.ReactionEmojis["❤️"]
	assert.False(t, hasHeart, "raw unicode reactions should not appear in reactionEmojis")
	// remote 2 件のみが残る
	assert.Len(t, entity.ReactionEmojis, 2)
}

func TestPopulateNoteReactionEmojis_SameNameDifferentHosts(t *testing.T) {
	// PR #463 review BUG-0002: 同名カスタム絵文字が複数 remote host から
	// 一つの note の reactions に含まれる場合、両方とも解決されること。
	// collectReactionEmojiNames が name 単独でキー化していると後勝ちで
	// 一方が消える。
	lookup := &stubEmojiLookup{
		data: map[string][]*model.Emoji{
			"alpha.example": {
				{ID: "ea", Name: "smile", Host: strPtr("alpha.example"), PublicURL: "https://alpha.example/emoji/smile.png"},
			},
			"beta.example": {
				{ID: "eb", Name: "smile", Host: strPtr("beta.example"), PublicURL: "https://beta.example/emoji/smile.png"},
			},
		},
	}
	reactionsJSON := []byte(`{":smile@alpha.example:":1,":smile@beta.example:":2}`)
	note := &model.Note{
		ID:        "n1",
		UserID:    "u1",
		Reactions: reactionsJSON,
	}

	r := NewEmojiResolver(lookup, []*model.Note{note})
	entity := &NoteEntity{ReactionEmojis: map[string]string{}}
	r.PopulateNoteReactionEmojis(note, entity)

	require.NotNil(t, entity.ReactionEmojis)
	// 両 host の URL が共存する
	assert.Equal(t, "https://alpha.example/emoji/smile.png", entity.ReactionEmojis["smile@alpha.example"])
	assert.Equal(t, "https://beta.example/emoji/smile.png", entity.ReactionEmojis["smile@beta.example"])
	assert.Len(t, entity.ReactionEmojis, 2)
}

// PopulateNoteReactionEmojis は resolver 構築時にデコードした (name, host) を
// 使い回し、note.Reactions を 2 度デコードしない。構築後に Reactions を差し替え
// ても構築時の内容で解決されることで、メモが使われていることを観測する。
//
// これは「resolver は note.Reactions の書き換えより後に構築する」という前提に
// 依存する。PackNotes は mergeBufferedReactions → NewEmojiResolver の順で
// それを満たしており、順序が壊れれば TestPackNotes_MergesBufferedReactions が
// 落ちる。
func TestPopulateNoteReactionEmojis_UsesMemoizedPairs(t *testing.T) {
	host := "remote.example"
	note := &model.Note{
		UserHost:  strPtr(host),
		Reactions: []byte(`{":party@remote.example:": 1}`),
	}
	lookup := &stubEmojiLookup{data: map[string][]*model.Emoji{
		host: {
			{Name: "party", PublicURL: "https://remote.example/party.png"},
			{Name: "other", PublicURL: "https://remote.example/other.png"},
		},
	}}
	r := NewEmojiResolver(lookup, []*model.Note{note})

	note.Reactions = []byte(`{":other@remote.example:": 1}`)

	var e NoteEntity
	r.PopulateNoteReactionEmojis(note, &e)
	assert.Equal(t, map[string]string{
		"party@remote.example": "https://remote.example/party.png",
	}, e.ReactionEmojis)
}

// resolver の構築対象に無い note を渡された場合はその場でデコードする。
// flattenNotesPlusRelations は 1 段しか辿らないが applyNoteResolvers は
// entity の木をそれより深く辿る (depth-2 の renote.renote) ので、この経路は
// 実際に踏まれる。
func TestPopulateNoteReactionEmojis_FallsBackForUnseenNote(t *testing.T) {
	host := "remote.example"
	seen := &model.Note{
		UserHost:  strPtr(host),
		Reactions: []byte(`{":party@remote.example:": 1}`),
	}
	lookup := &stubEmojiLookup{data: map[string][]*model.Emoji{
		host: {{Name: "party", PublicURL: "https://remote.example/party.png"}},
	}}
	r := NewEmojiResolver(lookup, []*model.Note{seen})

	unseen := &model.Note{
		UserHost:  strPtr(host),
		Reactions: []byte(`{":party@remote.example:": 2}`),
	}
	var e NoteEntity
	r.PopulateNoteReactionEmojis(unseen, &e)
	assert.Equal(t, map[string]string{
		"party@remote.example": "https://remote.example/party.png",
	}, e.ReactionEmojis)
}

func TestPopulateNoteReactionEmojis_NoCustomEmoji(t *testing.T) {
	// reactions が unicode のみ / 空の場合は ReactionEmojis を populate しない
	// (空 map で初期化されている前提を維持する)。
	lookup := &stubEmojiLookup{data: map[string][]*model.Emoji{}}

	cases := []struct {
		name string
		raw  []byte
	}{
		{"unicode only", []byte(`{"❤️":1,"👍":2}`)},
		{"empty json", []byte(`{}`)},
		{"nil reactions", nil},
		{"malformed json", []byte("not json")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			note := &model.Note{ID: "n1", UserID: "u1", Reactions: tc.raw}
			r := NewEmojiResolver(lookup, []*model.Note{note})
			entity := &NoteEntity{ReactionEmojis: map[string]string{"keep": "this"}}
			r.PopulateNoteReactionEmojis(note, entity)
			// custom emoji 不在なので元の map をそのまま温存する
			assert.Equal(t, map[string]string{"keep": "this"}, entity.ReactionEmojis)
		})
	}
}

func TestParseCustomEmojiReaction(t *testing.T) {
	cases := []struct {
		input string
		name  string
		host  string
		ok    bool
	}{
		{":smile:", "smile", "", true},
		{":smile@remote.example:", "smile", "remote.example", true},
		// canonical local form (mk-go reaction_service.resolveReaction 出力)
		{":smile@.:", "smile", "", true},
		{"❤️", "", "", false},
		{":invalid_no_close", "", "", false},
		{"no_open:", "", "", false},
		{"::", "", "", false}, // len < 3 で parser が早期 return、空 name は不正
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			name, host, ok := parseCustomEmojiReaction(tc.input)
			assert.Equal(t, tc.ok, ok)
			if ok {
				assert.Equal(t, tc.name, name)
				assert.Equal(t, tc.host, host)
			}
		})
	}
}

func TestPopulateNoteEmojis_UnresolvedEmojiExcluded(t *testing.T) {
	remoteHost := "remote.example"
	lookup := &stubEmojiLookup{
		data: map[string][]*model.Emoji{
			"remote.example": {
				// "known"のみ返却、"unknown"は含まない
				{ID: "e1", Name: "known", Host: strPtr("remote.example"), PublicURL: "https://remote.example/emoji/known.png", OriginalURL: ""},
			},
		},
	}

	note := &model.Note{
		ID:       "n1",
		UserID:   "u1",
		UserHost: &remoteHost,
		Emojis:   model.StringArray{"known", "unknown"},
	}

	r := NewEmojiResolver(lookup, []*model.Note{note})
	entity := &NoteEntity{Emojis: &map[string]string{}}
	r.PopulateNoteEmojis(note, entity)

	// cacheに存在する絵文字のみがentity.Emojisに含まれる
	require.NotNil(t, entity.Emojis)
	assert.Equal(t, "https://remote.example/emoji/known.png", (*entity.Emojis)["known"])
	_, exists := (*entity.Emojis)["unknown"]
	assert.False(t, exists, "unknown emoji should not be in entity.Emojis")
}

// ローカルの利用者の emojis は本家と同じく解決しない (#3270)。本家
// CustomEmojiService.populateEmoji は host が null だと null を返す。引きにも
// 行かない (タイムラインのたびに無駄な問い合わせを増やさない)。
func TestPopulateUserEmojis_LocalUserNotResolved(t *testing.T) {
	lookup := &stubEmojiLookup{data: map[string][]*model.Emoji{
		"": {{ID: "e1", Name: "cat", PublicURL: "https://local.example/emoji/cat.png"}},
	}}
	user := &model.User{ID: "u1", Username: "alice", Emojis: model.StringArray{"cat"}}
	note := &model.Note{ID: "n1", UserID: "u1", User: user, Emojis: model.StringArray{}}

	r := NewEmojiResolver(lookup, []*model.Note{note})
	lite := &UserLite{Emojis: map[string]string{}}
	r.PopulateUserEmojis(user, lite)

	assert.Empty(t, lite.Emojis)
	for _, c := range lookup.calls {
		assert.NotContains(t, c.names, "cat", "ローカルの利用者の絵文字を引きに行かない")
	}

	// ローカルのノートの本文に同じ絵文字があってキャッシュに入っていても、
	// 利用者の emojis には出さない。
	note.Emojis = model.StringArray{"cat"}
	r = NewEmojiResolver(lookup, []*model.Note{note})
	lite = &UserLite{Emojis: map[string]string{}}
	r.PopulateUserEmojis(user, lite)
	assert.Empty(t, lite.Emojis)
}
