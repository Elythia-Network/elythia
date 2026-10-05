package entity

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func newTestIDGen(t *testing.T) id.Generator {
	t.Helper()
	g, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return g
}

func TestPackNote_Basic(t *testing.T) {
	idGen := newTestIDGen(t)

	text := "Hello, world!"
	cw := "CW"
	noteID := idGen.Generate(time.Now())
	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		Text:       &text,
		CW:         &cw,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
		FileIDs:    model.StringArray{"file1", "file2"},
		Tags:       model.StringArray{"tag1"},
	}

	entity := PackNote(note, idGen)

	assert.Equal(t, noteID, entity.ID)
	assert.Equal(t, "user1", entity.UserID)
	assert.Equal(t, &text, entity.Text)
	assert.Equal(t, &cw, entity.CW)
	assert.Equal(t, "public", entity.Visibility)
	assert.Equal(t, []string{"file1", "file2"}, entity.FileIDs)
	assert.Equal(t, []string{"tag1"}, entity.Tags)
	assert.NotEmpty(t, entity.CreatedAt)
	// #1639: local note (UserHost==nil) は emojis を出さない (nil → 省略)。
	assert.Nil(t, entity.Emojis)
	assert.NotNil(t, entity.Files)
	assert.Empty(t, entity.Files)
}

func TestPackNote_NilArrays(t *testing.T) {
	idGen := newTestIDGen(t)
	noteID := idGen.Generate(time.Now())

	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		Visibility: model.NoteVisibilityHome,
		Reactions:  datatypes.JSON([]byte("{}")),
	}

	entity := PackNote(note, idGen)

	// FileIDsはnilでも空スライスに変換、Tagsはomitemptyでnil維持
	assert.NotNil(t, entity.FileIDs)
	assert.Empty(t, entity.FileIDs)
	assert.Nil(t, entity.Tags) // omitempty: nil → JSON省略
}

func TestPackNote_WithUser(t *testing.T) {
	idGen := newTestIDGen(t)
	noteID := idGen.Generate(time.Now())

	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
		User: &model.User{
			ID:                "user1",
			Username:          "testuser",
			AvatarDecorations: datatypes.JSON([]byte("[]")),
		},
	}

	entity := PackNote(note, idGen)

	assert.Equal(t, "user1", entity.User.ID)
	assert.Equal(t, "testuser", entity.User.Username)
}

func TestPackNote_ReactionCount(t *testing.T) {
	idGen := newTestIDGen(t)
	noteID := idGen.Generate(time.Now())

	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte(`{"👍":3,"❤️":2,"🎉":1}`)),
	}

	entity := PackNote(note, idGen)
	assert.Equal(t, 6, entity.ReactionCount)
}

func TestPackNote_ReactionCount_Empty(t *testing.T) {
	idGen := newTestIDGen(t)
	noteID := idGen.Generate(time.Now())

	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}

	entity := PackNote(note, idGen)
	assert.Equal(t, 0, entity.ReactionCount)
}

func TestPackNote_ReactionCount_NilReactions(t *testing.T) {
	idGen := newTestIDGen(t)
	noteID := idGen.Generate(time.Now())

	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		Visibility: model.NoteVisibilityPublic,
	}

	entity := PackNote(note, idGen)
	assert.Equal(t, 0, entity.ReactionCount)
}

func TestPackNote_ReactionCount_InvalidJSON(t *testing.T) {
	idGen := newTestIDGen(t)
	noteID := idGen.Generate(time.Now())

	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("invalid")),
	}

	entity := PackNote(note, idGen)
	assert.Equal(t, 0, entity.ReactionCount)
}

func TestPackNote_VisibleUserIDs_Mentions_HasPoll(t *testing.T) {
	idGen := newTestIDGen(t)
	noteID := idGen.Generate(time.Now())

	note := &model.Note{
		ID:             noteID,
		UserID:         "user1",
		Visibility:     model.NoteVisibilitySpecified,
		Reactions:      datatypes.JSON([]byte("{}")),
		VisibleUserIDs: model.StringArray{"user2", "user3"},
		Mentions:       model.StringArray{"user2"},
		HasPoll:        true,
	}

	entity := PackNote(note, idGen)

	assert.Equal(t, []string{"user2", "user3"}, entity.VisibleUserIDs)
	assert.Equal(t, []string{"user2"}, entity.Mentions)
	assert.True(t, entity.HasPoll)
}

func TestPackNote_NilVisibleUserIDs_Mentions(t *testing.T) {
	idGen := newTestIDGen(t)
	noteID := idGen.Generate(time.Now())

	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}

	entity := PackNote(note, idGen)

	// #1561: public note では visibleUserIds は省略 (specified 限定) / mentions は
	// 空なら省略 / hasPoll は false なら省略 → いずれも nil で omitempty が落とす。
	assert.Nil(t, entity.VisibleUserIDs, "public note omits visibleUserIds")
	assert.Nil(t, entity.Mentions, "empty mentions omitted")
	assert.False(t, entity.HasPoll)

	// JSON でも該当 key が出ないこと。
	b, err := json.Marshal(entity)
	require.NoError(t, err)
	s := string(b)
	assert.NotContains(t, s, `"visibleUserIds"`)
	assert.NotContains(t, s, `"mentions"`)
	assert.NotContains(t, s, `"hasPoll"`)
}

func TestNormalizeReactionKey(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"local legacy", ":smile:", ":smile@.:"},
		{"local canonical", ":smile@.:", ":smile@.:"},
		{"remote", ":smile@remote.example:", ":smile@remote.example:"},
		{"unicode emoji", "👍", "👍"},
		{"heart", "❤", "❤"},
		{"empty", "", ""},
		{"hyphen name", ":cat-smile:", ":cat-smile@.:"},
		{"plus name", ":thumbs+up:", ":thumbs+up@.:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, NormalizeReactionKey(tc.in))
		})
	}
}

func TestPackNote_ReactionsKeyNormalized(t *testing.T) {
	idGen := newTestIDGen(t)
	noteID := idGen.Generate(time.Now())

	// TS時代の `:lifelog:` とmk時代の `:lifelog@.:` が混在
	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte(`{":lifelog:":1,":lifelog@.:" :1,"👍":3}`)),
	}

	e := PackNote(note, idGen)

	var reactions map[string]float64
	require.NoError(t, json.Unmarshal(e.Reactions, &reactions))
	// `:lifelog:` と `:lifelog@.:` が統合される
	assert.Equal(t, float64(2), reactions[":lifelog@.:"])
	assert.Equal(t, float64(3), reactions["👍"])
	_, hasLegacy := reactions[":lifelog:"]
	assert.False(t, hasLegacy)
	assert.Equal(t, 5, e.ReactionCount)
}

// #1816: legacy text reaction (like→👍 等) を packer で Unicode に変換する。
// 既存の同 Unicode reaction とは count がマージされる。
func TestPackNote_ReactionsLegacyTextAlias(t *testing.T) {
	idGen := newTestIDGen(t)
	noteID := idGen.Generate(time.Now())

	// legacy "like" (2) + 既存 "👍" (3) → "👍":5 に集約。"love" (1) → "❤":1。
	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte(`{"like":2,"👍":3,"love":1}`)),
	}

	e := PackNote(note, idGen)

	var reactions map[string]float64
	require.NoError(t, json.Unmarshal(e.Reactions, &reactions))
	assert.Equal(t, float64(5), reactions["👍"], "like は 👍 に変換され既存 count とマージ")
	assert.Equal(t, float64(1), reactions["❤"], "love は ❤ に変換")
	_, hasLike := reactions["like"]
	assert.False(t, hasLike, "legacy text key は残らない")
	_, hasLove := reactions["love"]
	assert.False(t, hasLove)
	// reactionCount は変換しても総和は不変。
	assert.Equal(t, 6, e.ReactionCount)
}

// packReactions は合計を **正規化前** の各値から求める (統合前の sumReactions と
// 同じ)。集約後の値から求めると、小数を含むレコードで切り捨ての位置が変わる。
// 実データに小数は入らないが、統合で挙動を変えていないことをここで固定する。
func TestPackNote_ReactionCount_SummedBeforeNormalization(t *testing.T) {
	idGen := newTestIDGen(t)
	noteID := idGen.Generate(time.Now())

	// "like" は 👍 に正規化されるので、正規化後は 👍:3.0 の 1 キーになる。
	// 正規化前から足すと int(1.5)+int(1.5)=2、正規化後から足すと int(3.0)=3。
	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte(`{"like":1.5,"👍":1.5}`)),
	}

	e := PackNote(note, idGen)
	assert.Equal(t, 2, e.ReactionCount, "合計は正規化前の各値を int に落として足す")

	var reactions map[string]float64
	require.NoError(t, json.Unmarshal(e.Reactions, &reactions))
	assert.Equal(t, float64(3), reactions["👍"], "reactions 側は正規化後にマージされる")
}

// packReactions の戻り値 (正規化済み JSON, 合計) が、統合前の
// normalizeReactionKeys / sumReactions の組と全分岐で一致することを固定する。
func TestPackReactions(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantJSON  string
		wantTotal int
	}{
		// nil / 空は golden Note.reactions が object 必須なので {} に coalesce
		// する (#1312)。
		{"nil", "", "{}", 0},
		{"empty object", "{}", "{}", 0},
		// unmarshal に失敗したら raw をそのまま返して合計 0 (fail-soft)。
		{"invalid json", "not json", "not json", 0},
		// JSON null は unmarshal に成功して nil map になるので {} が出る。
		{"json null", "null", "{}", 0},
		{"canonical only", `{"❤":2}`, `{"❤":2}`, 2},
		{"legacy alias merged", `{"like":2,"👍":3}`, `{"👍":5}`, 5},
		{"legacy colon form", `{":smile:":1}`, `{":smile@.:":1}`, 1},
		{"negative value", `{"❤":-1}`, `{"❤":-1}`, -1},
		// **raw をそのまま返す最適化を入れると落ちるケース。** キーが 1 つも
		// 変わらないので raw passthrough が成立してしまうが、raw の順
		// ({"bb","a"}) と json.Marshal の順 (バイト列昇順) は違う。キーが
		// 1 個以下のケースだけでは順序の変化を観測できない。
		{"multi key reordered by marshal", `{"bb":1,"a":2}`, `{"a":2,"bb":1}`, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw datatypes.JSON
			if tc.raw != "" {
				raw = datatypes.JSON([]byte(tc.raw))
			}
			gotJSON, gotTotal := packReactions(raw)
			// bytes で比較する。JSONEq だと不正 JSON のケースを見られず、
			// キー順の変化 (multi key reordered by marshal) も見逃す。
			assert.Equal(t, tc.wantJSON, string(gotJSON))
			assert.Equal(t, tc.wantTotal, gotTotal)
		})
	}
}

func TestPackNote_WithRenoteAndReply(t *testing.T) {
	idGen := newTestIDGen(t)
	renoteID := idGen.Generate(time.Now())
	replyID := idGen.Generate(time.Now())
	noteID := idGen.Generate(time.Now())

	renoteText := "original"
	replyText := "parent reply"
	text := "quoting"

	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		Text:       &text,
		RenoteID:   &renoteID,
		ReplyID:    &replyID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
		Renote: &model.Note{
			ID:         renoteID,
			UserID:     "user2",
			Text:       &renoteText,
			Visibility: model.NoteVisibilityPublic,
			Reactions:  datatypes.JSON([]byte("{}")),
			User: &model.User{
				ID:                "user2",
				Username:          "author2",
				AvatarDecorations: datatypes.JSON([]byte("[]")),
			},
		},
		Reply: &model.Note{
			ID:         replyID,
			UserID:     "user3",
			Text:       &replyText,
			Visibility: model.NoteVisibilityPublic,
			Reactions:  datatypes.JSON([]byte("{}")),
		},
	}

	e := PackNote(note, idGen)

	require.NotNil(t, e.Renote)
	assert.Equal(t, renoteID, e.Renote.ID)
	assert.Equal(t, &renoteText, e.Renote.Text)
	assert.Equal(t, "author2", e.Renote.User.Username)

	require.NotNil(t, e.Reply)
	assert.Equal(t, replyID, e.Reply.ID)
	assert.Equal(t, &replyText, e.Reply.Text)
}

// 純粋リノート (boost) が引用投稿 (quote) を包む場合、引用先 (renote.renote) まで
// pack されること。これが無いと frontend は引用先を「削除されたノート」として
// 描画する (timeline bug)。併せて (1) reply embed は detail:false leaf、(2) renote
// embed 内の reply (renote.reply, depth-2) は展開しない (top-level reply のみ)、
// (3) renote chain は maxNoteEmbedDepth=2 で止まる、ことを検証する。
func TestPackNote_NestedRenoteEmbedPacksQuoteTarget(t *testing.T) {
	idGen := newTestIDGen(t)
	deepID := idGen.Generate(time.Now())       // LV3 (cap で出ないはず)
	targetID := idGen.Generate(time.Now())     // LV2 引用先
	quoteReplyID := idGen.Generate(time.Now()) // quote の reply (depth-2、出ないはず)
	replyChildID := idGen.Generate(time.Now()) // top.reply の renote (leaf、出ないはず)
	quoteID := idGen.Generate(time.Now())      // LV1 引用投稿
	replyID := idGen.Generate(time.Now())      // top の reply
	topID := idGen.Generate(time.Now())        // LV0 pure renote

	targetText := "quote target body"
	quoteText := "quoting"

	mk := func(id, uid string) *model.Note {
		return &model.Note{
			ID: id, UserID: uid, Visibility: model.NoteVisibilityPublic,
			Reactions: datatypes.JSON([]byte("{}")),
		}
	}

	// LV2 引用先。さらに自分の renote (LV3) を持たせて cap=2 の打ち切りを検証。
	target := mk(targetID, "u3")
	target.Text = &targetText
	target.User = &model.User{ID: "u3", Username: "target_author", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	target.RenoteID = &deepID
	target.Renote = mk(deepID, "u4")

	// LV1 引用投稿 (text 付き renote = quote)。reply も持たせ、renote.reply が depth-2 で
	// 展開されないこと (top-level reply のみ展開) を検証する。
	quote := mk(quoteID, "u2")
	quote.Text = &quoteText
	quote.RenoteID = &targetID
	quote.ReplyID = &quoteReplyID
	quote.Reply = mk(quoteReplyID, "u7")
	quote.User = &model.User{ID: "u2", Username: "quote_author", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	quote.Renote = target

	// top の reply target。自分の renote を持たせ、reply embed が leaf であることを検証。
	reply := mk(replyID, "u5")
	reply.RenoteID = &replyChildID
	reply.Renote = mk(replyChildID, "u6")

	// LV0 pure renote (text 無し) が quote を包む。
	top := mk(topID, "u1")
	top.RenoteID = &quoteID
	top.ReplyID = &replyID
	top.Renote = quote
	top.Reply = reply

	e := PackNote(top, idGen)

	// LV1: 引用投稿
	require.NotNil(t, e.Renote, "top.renote (quote) should pack")
	assert.Equal(t, quoteID, e.Renote.ID)
	// LV2: 引用先 — 本バグの修正点。depth 2 まで展開される。
	require.NotNil(t, e.Renote.Renote, "renote.renote (quote target) must pack, else frontend shows deleted note")
	assert.Equal(t, targetID, e.Renote.Renote.ID)
	assert.Equal(t, &targetText, e.Renote.Renote.Text)
	// LV3: maxNoteEmbedDepth=2 で打ち切られる。
	assert.Nil(t, e.Renote.Renote.Renote, "renote chain must stop at maxNoteEmbedDepth=2")
	// renote embed 内の reply (depth-2) も upstream と同じく展開する
	// (renote は detail:true で pack されるため)。
	require.NotNil(t, e.Renote.Reply, "renote.reply (depth-2) must pack like upstream")
	assert.Equal(t, quoteReplyID, e.Renote.Reply.ID)

	// reply embed は detail:false leaf なので自分の renote を展開しない (upstream 一致)。
	require.NotNil(t, e.Reply, "top.reply should pack")
	assert.Nil(t, e.Reply.Renote, "reply embed is detail:false leaf and must not expand its renote")
}

// #1816: reply embed は upstream で detail:false なので clippedCount / poll を
// 出力しない。renote embed は detail:true なので両方とも出す。
func TestPackNote_ReplyEmbedDetailFalse(t *testing.T) {
	idGen := newTestIDGen(t)
	renoteID := idGen.Generate(time.Now())
	replyID := idGen.Generate(time.Now())
	noteID := idGen.Generate(time.Now())
	text := "top"

	poll := &model.Poll{Choices: model.StringArray{"a", "b"}, Votes: model.Int64Array{1, 2}}
	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		Text:       &text,
		RenoteID:   &renoteID,
		ReplyID:    &replyID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
		Renote: &model.Note{
			ID:           renoteID,
			UserID:       "user2",
			Visibility:   model.NoteVisibilityPublic,
			Reactions:    datatypes.JSON([]byte("{}")),
			ClippedCount: 2,
			HasPoll:      true,
			Poll:         poll,
		},
		Reply: &model.Note{
			ID:           replyID,
			UserID:       "user3",
			Visibility:   model.NoteVisibilityPublic,
			Reactions:    datatypes.JSON([]byte("{}")),
			ClippedCount: 3,
			HasPoll:      true,
			Poll:         poll,
		},
	}

	e := PackNote(note, idGen)

	// top-level (detail:true): clippedCount / poll を出す。
	require.NotNil(t, e.ClippedCount)

	// renote embed (detail:true): clippedCount / poll を出す。
	require.NotNil(t, e.Renote)
	require.NotNil(t, e.Renote.ClippedCount)
	assert.Equal(t, 2, *e.Renote.ClippedCount)
	assert.NotNil(t, e.Renote.Poll)

	// reply embed (detail:false): clippedCount / poll を省く。hasPoll は detail 外
	// なので残る。
	require.NotNil(t, e.Reply)
	assert.Nil(t, e.Reply.ClippedCount, "reply embed は clippedCount を出さない")
	assert.Nil(t, e.Reply.Poll, "reply embed は poll を出さない")
	assert.True(t, e.Reply.HasPoll, "hasPoll は detail 外なので reply にも残る")

	// JSON でも reply embed から clippedCount / poll の key が消える。
	b, err := json.Marshal(e.Reply)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "clippedCount")
	assert.NotContains(t, string(b), "\"poll\"")
}

func TestPackNotes_EmbeddedRenoteHasInstanceAndEmoji(t *testing.T) {
	idGen := newTestIDGen(t)
	renoteID := idGen.Generate(time.Now())
	noteID := idGen.Generate(time.Now())
	remoteHost := "remote.example"

	renote := &model.Note{
		ID:         renoteID,
		UserID:     "user2",
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
		UserHost:   &remoteHost,
		Emojis:     []string{"wave"},
		User: &model.User{
			ID:                "user2",
			Username:          "remoteuser",
			Host:              &remoteHost,
			AvatarDecorations: datatypes.JSON([]byte("[]")),
			Emojis:            []string{"wave"},
		},
	}
	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		RenoteID:   &renoteID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
		User: &model.User{
			ID:                "user1",
			Username:          "localuser",
			AvatarDecorations: datatypes.JSON([]byte("[]")),
		},
		Renote: renote,
	}

	instLookup := &stubInstanceLookup{data: map[string]*model.Instance{
		remoteHost: {Host: remoteHost, Name: strPtr("Remote")},
	}}
	emojiLookup := &stubEmojiLookup{data: map[string][]*model.Emoji{
		remoteHost: {{Name: "wave", PublicURL: "https://remote.example/emoji/wave.png"}},
	}}

	out := PackNotes(context.Background(), []*model.Note{note}, idGen, instLookup, emojiLookup, nil)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Renote)
	require.NotNil(t, out[0].Renote.User.Instance)
	assert.Equal(t, strPtr("Remote"), out[0].Renote.User.Instance.Name)
	require.NotNil(t, out[0].Renote.Emojis)
	assert.Equal(t, "https://remote.example/emoji/wave.png", (*out[0].Renote.Emojis)["wave"])
	assert.Equal(t, "https://remote.example/emoji/wave.png", out[0].Renote.User.Emojis["wave"])
}

func TestPackNoteWithInstance_EmbeddedRenote(t *testing.T) {
	idGen := newTestIDGen(t)
	renoteID := idGen.Generate(time.Now())
	noteID := idGen.Generate(time.Now())
	remoteHost := "remote.example"

	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		RenoteID:   &renoteID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
		Renote: &model.Note{
			ID:         renoteID,
			UserID:     "user2",
			Visibility: model.NoteVisibilityPublic,
			Reactions:  datatypes.JSON([]byte("{}")),
			UserHost:   &remoteHost,
			User: &model.User{
				ID:                "user2",
				Username:          "remoteuser",
				Host:              &remoteHost,
				AvatarDecorations: datatypes.JSON([]byte("[]")),
			},
		},
	}

	instLookup := &stubInstanceLookup{data: map[string]*model.Instance{
		remoteHost: {Host: remoteHost, Name: strPtr("Remote")},
	}}

	out := PackNoteWithInstance(context.Background(), note, idGen, instLookup, nil, nil)
	require.NotNil(t, out.Renote)
	require.NotNil(t, out.Renote.User.Instance)
	assert.Equal(t, strPtr("Remote"), out.Renote.User.Instance.Name)
}

// stubBufferedReader is a test double for BufferedReactionsReader.
type stubBufferedReader struct {
	data map[string]map[string]int64
	err  error
}

func (s *stubBufferedReader) GetBufferedMany(_ context.Context, ids []string) (map[string]map[string]int64, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[string]map[string]int64)
	for _, id := range ids {
		if d, ok := s.data[id]; ok {
			out[id] = d
		}
	}
	return out, nil
}

// reader が non-nil なら DB の reactions と buffered deltas を merge して
// 返す (#647)。emoji も merged keys から resolve される。
func TestPackNotes_MergesBufferedReactions(t *testing.T) {
	idGen := newTestIDGen(t)
	noteID := idGen.Generate(time.Now())
	remoteHost := "remote.example"

	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte(`{":existing@.:": 1}`)),
		User:       &model.User{ID: "user1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))},
	}
	emojiLookup := &stubEmojiLookup{data: map[string][]*model.Emoji{
		remoteHost: {{Name: "yikes", PublicURL: "https://remote.example/emoji/yikes.png"}},
	}}
	reader := &stubBufferedReader{data: map[string]map[string]int64{
		noteID: {":yikes@" + remoteHost + ":": 1},
	}}

	out := PackNotes(context.Background(), []*model.Note{note}, idGen, nil, emojiLookup, reader)
	require.Len(t, out, 1)
	// merged reactions JSON は両方のキーを含む
	var got map[string]int
	require.NoError(t, json.Unmarshal(out[0].Reactions, &got))
	assert.Equal(t, 1, got[":existing@.:"])
	assert.Equal(t, 1, got[":yikes@"+remoteHost+":"])
	assert.Equal(t, 2, out[0].ReactionCount)
	// reactionEmojis に buffered 由来の emoji 解決結果が入る
	assert.Equal(t, "https://remote.example/emoji/yikes.png", out[0].ReactionEmojis["yikes@"+remoteHost])
}

// 2 件の note が同一 *Note の renote を共有する場合 (repository の batch hydration
// で起こりうる)、buffered delta が二重加算されないこと。mergeBufferedReactions は
// 加算的なので、flat list に同一ポインタが 2 回現れても適用は 1 回に限定する。
func TestPackNotes_SharedRenote_NoDoubleMerge(t *testing.T) {
	idGen := newTestIDGen(t)
	renoteID := idGen.Generate(time.Now().Add(-time.Hour))
	// 共有される renote 先 (base reaction 1)。
	shared := &model.Note{
		ID:         renoteID,
		UserID:     "ru",
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte(`{":base@.:": 1}`)),
		User:       &model.User{ID: "ru", Username: "renoter", AvatarDecorations: datatypes.JSON([]byte("[]"))},
	}
	mkRenoter := func(i int) *model.Note {
		return &model.Note{
			ID: idGen.Generate(time.Now().Add(time.Duration(i) * time.Second)), UserID: "u",
			Visibility: model.NoteVisibilityPublic, RenoteID: &renoteID,
			User:   &model.User{ID: "u", Username: "u", AvatarDecorations: datatypes.JSON([]byte("[]"))},
			Renote: shared, // 両者が同一ポインタを共有
		}
	}
	n1, n2 := mkRenoter(0), mkRenoter(1)
	// shared renote に buffered delta +2。
	reader := &stubBufferedReader{data: map[string]map[string]int64{
		renoteID: {":base@.:": 2},
	}}

	out := PackNotes(context.Background(), []*model.Note{n1, n2}, idGen, nil, nil, reader)
	require.Len(t, out, 2)
	// 二重加算なら 1+2+2=5 になる。正しくは 1+2=3 (1 回のみ適用)。
	for _, e := range out {
		require.NotNil(t, e.Renote)
		var got map[string]int
		require.NoError(t, json.Unmarshal(e.Renote.Reactions, &got))
		assert.Equal(t, 3, got[":base@.:"], "shared renote の buffered delta は二重加算されない")
	}
}

// 0 以下になった merged value は出力から除外される。
func TestPackNotes_BufferedNegativeDelta_RemovesKey(t *testing.T) {
	idGen := newTestIDGen(t)
	noteID := idGen.Generate(time.Now())

	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte(`{":foo@.:": 1}`)),
		User:       &model.User{ID: "user1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))},
	}
	reader := &stubBufferedReader{data: map[string]map[string]int64{
		noteID: {":foo@.:": -1},
	}}

	out := PackNotes(context.Background(), []*model.Note{note}, idGen, nil, nil, reader)
	require.Len(t, out, 1)
	var got map[string]int
	if len(out[0].Reactions) > 0 {
		_ = json.Unmarshal(out[0].Reactions, &got)
	}
	assert.NotContains(t, got, ":foo@.:")
	assert.Equal(t, 0, out[0].ReactionCount)
}

// reader == nil は旧挙動 (DB のみ)。
func TestPackNotes_NilReader_NoMerge(t *testing.T) {
	idGen := newTestIDGen(t)
	noteID := idGen.Generate(time.Now())

	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte(`{":foo@.:": 3}`)),
		User:       &model.User{ID: "user1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))},
	}
	out := PackNotes(context.Background(), []*model.Note{note}, idGen, nil, nil, nil)
	require.Len(t, out, 1)
	assert.Equal(t, 3, out[0].ReactionCount)
}

// reader が error を返した場合は stale な DB 値で fall back して continue。
func TestPackNotes_ReaderError_FallsBackToDB(t *testing.T) {
	idGen := newTestIDGen(t)
	noteID := idGen.Generate(time.Now())

	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte(`{":foo@.:": 2}`)),
		User:       &model.User{ID: "user1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))},
	}
	reader := &stubBufferedReader{err: errors.New("redis down")}

	out := PackNotes(context.Background(), []*model.Note{note}, idGen, nil, nil, reader)
	require.Len(t, out, 1)
	assert.Equal(t, 2, out[0].ReactionCount, "reader error 時は DB 値で続行")
}

// embed された Renote の reactions も merge 対象。
func TestPackNotes_MergesBufferedReactions_OnEmbeddedRenote(t *testing.T) {
	idGen := newTestIDGen(t)
	renoteID := idGen.Generate(time.Now())
	noteID := idGen.Generate(time.Now())

	renote := &model.Note{
		ID:         renoteID,
		UserID:     "user2",
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
		User:       &model.User{ID: "user2", Username: "bob", AvatarDecorations: datatypes.JSON([]byte("[]"))},
	}
	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		RenoteID:   &renoteID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
		User:       &model.User{ID: "user1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))},
		Renote:     renote,
	}
	reader := &stubBufferedReader{data: map[string]map[string]int64{
		renoteID: {":heart@.:": 5},
	}}

	out := PackNotes(context.Background(), []*model.Note{note}, idGen, nil, nil, reader)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Renote)
	assert.Equal(t, 5, out[0].Renote.ReactionCount)
}

// embed された Reply の reactions も merge 対象 (Renote 経路と対称)。
func TestPackNotes_MergesBufferedReactions_OnEmbeddedReply(t *testing.T) {
	idGen := newTestIDGen(t)
	replyID := idGen.Generate(time.Now())
	noteID := idGen.Generate(time.Now())

	reply := &model.Note{
		ID:         replyID,
		UserID:     "user2",
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte(`{":existing@.:": 1}`)),
		User:       &model.User{ID: "user2", Username: "bob", AvatarDecorations: datatypes.JSON([]byte("[]"))},
	}
	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		ReplyID:    &replyID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
		User:       &model.User{ID: "user1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))},
		Reply:      reply,
	}
	reader := &stubBufferedReader{data: map[string]map[string]int64{
		replyID: {":heart@.:": 3},
	}}

	out := PackNotes(context.Background(), []*model.Note{note}, idGen, nil, nil, reader)
	require.Len(t, out, 1)
	require.NotNil(t, out[0].Reply)
	assert.Equal(t, 4, out[0].Reply.ReactionCount, "embed reply の DB(1) + buffered(3) が merge される")
}

func TestFlattenNotesPlusRelations_NilSafe(t *testing.T) {
	out := flattenNotesPlusRelations([]*model.Note{nil, {ID: "a"}})
	require.Len(t, out, 1)
	assert.Equal(t, "a", out[0].ID)
}

func TestPackNote_CreatedAtParsing(t *testing.T) {
	idGen := newTestIDGen(t)
	noteID := idGen.Generate(time.Now())

	note := &model.Note{
		ID:         noteID,
		UserID:     "user1",
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}

	entity := PackNote(note, idGen)

	// createdAtはISO 8601形式
	assert.Contains(t, entity.CreatedAt, "T")
	assert.Contains(t, entity.CreatedAt, "Z")
}
