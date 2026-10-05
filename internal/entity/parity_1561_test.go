package entity

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// #1561 [MEDIUM] clippedCount は model 実値を出す (旧: 0 ハードコード)。
// #1816: top-level note は detail:true なので clippedCount を *int で出力する。
func TestPackNote_ClippedCountRealValue(t *testing.T) {
	idGen := newTestIDGen(t)
	n := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "u1",
		Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
		ClippedCount: 5,
	}
	packed := PackNote(n, idGen)
	require.NotNil(t, packed.ClippedCount)
	assert.Equal(t, 5, *packed.ClippedCount)
}

// #1561 [MEDIUM] visibleUserIds は specified のときだけ出力する。
func TestPackNote_VisibleUserIDsSpecifiedOnly(t *testing.T) {
	idGen := newTestIDGen(t)
	base := func(vis model.NoteVisibility) *model.Note {
		return &model.Note{
			ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: vis,
			Reactions: datatypes.JSON([]byte("{}")), VisibleUserIDs: []string{"a", "b"},
		}
	}
	// specified: 出力される
	spec := PackNote(base(model.NoteVisibilitySpecified), idGen)
	assert.Equal(t, []string{"a", "b"}, spec.VisibleUserIDs)
	bs, _ := json.Marshal(spec)
	assert.Contains(t, string(bs), `"visibleUserIds":["a","b"]`)
	// public: VisibleUserIDs が DB にあっても省略
	pub := PackNote(base(model.NoteVisibilityPublic), idGen)
	assert.Nil(t, pub.VisibleUserIDs)
	bp, _ := json.Marshal(pub)
	assert.NotContains(t, string(bp), `"visibleUserIds"`)
}

// #1561 [MEDIUM] name 付き note は text を 【name】prefix 整形する。
func TestPackNote_NamePrefixedText(t *testing.T) {
	idGen := newTestIDGen(t)
	name := "Page Title"
	body := "  hello body  "
	url := "https://remote.example/pages/x"
	n := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic,
		Reactions: datatypes.JSON([]byte("{}")), Name: &name, Text: &body, URL: &url,
	}
	got := PackNote(n, idGen)
	require.NotNil(t, got.Text)
	assert.Equal(t, "【Page Title】\nhello body\n\nhttps://remote.example/pages/x", *got.Text)

	// url が無く uri があれば uri を使う。text が nil でも空本文で整形。
	uri := "https://remote.example/notes/y"
	n2 := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic,
		Reactions: datatypes.JSON([]byte("{}")), Name: &name, URI: &uri,
	}
	got2 := PackNote(n2, idGen)
	require.NotNil(t, got2.Text)
	assert.Equal(t, "【Page Title】\n\n\nhttps://remote.example/notes/y", *got2.Text)

	// name はあるが url/uri が無ければ整形しない (text そのまま)。
	n3 := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic,
		Reactions: datatypes.JSON([]byte("{}")), Name: &name, Text: &body,
	}
	assert.Equal(t, body, *PackNote(n3, idGen).Text)
}

// #1561 [MEDIUM] channel.userId が出力される (nil なら null)。
func TestApplyChannel_UserID(t *testing.T) {
	owner := "channel-owner"
	chMap := map[string]*model.Channel{
		"ch1": {ID: "ch1", Name: "general", UserID: &owner, Color: "#fff"},
	}
	chID := "ch1"
	n := &NoteEntity{ID: "n1", ChannelID: &chID}
	applyChannel(n, chMap)
	require.NotNil(t, n.Channel)
	require.NotNil(t, n.Channel.UserID)
	assert.Equal(t, "channel-owner", *n.Channel.UserID)
	b, _ := json.Marshal(n.Channel)
	assert.Contains(t, string(b), `"userId":"channel-owner"`)
}

// #1561 [LOW] hasPoll は true のときだけ出力 / mentions は空なら省略。
func TestPackNote_HasPollAndMentionsOmit(t *testing.T) {
	idGen := newTestIDGen(t)
	// hasPoll=true + mentions あり → 出力
	withPoll := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic,
		Reactions: datatypes.JSON([]byte("{}")), HasPoll: true, Mentions: []string{"m1"},
	}
	bp, _ := json.Marshal(PackNote(withPoll, idGen))
	assert.Contains(t, string(bp), `"hasPoll":true`)
	assert.Contains(t, string(bp), `"mentions":["m1"]`)
	// hasPoll=false + mentions 空 → 省略
	plain := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic,
		Reactions: datatypes.JSON([]byte("{}")),
	}
	bn, _ := json.Marshal(PackNote(plain, idGen))
	assert.NotContains(t, string(bn), `"hasPoll"`)
	assert.NotContains(t, string(bn), `"mentions"`)
}

// #1639 [LOW] emojis は remote note (host!=nil) のみ出力 (custom emoji 無くても
// {})、local note (host==nil) は省略する (upstream NoteEntityService.ts:412)。
func TestPackNote_EmojisRemoteOnly(t *testing.T) {
	idGen := newTestIDGen(t)
	remote := "remote.example"

	// local note: emojis 省略 (nil)
	local := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic,
		Reactions: datatypes.JSON([]byte("{}")),
	}
	lp := PackNote(local, idGen)
	assert.Nil(t, lp.Emojis, "local note omits emojis")
	// top-level の emojis key が無いことを確認 (user.emojis の null と混同しない
	// よう unmarshal して top-level key の有無で判定)。
	var lm map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(mustJSON(t, lp), &lm))
	_, hasLocal := lm["emojis"]
	assert.False(t, hasLocal, "local note must omit top-level emojis")

	// remote note: custom emoji 無くても emojis={} を出力
	rmt := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "u2", UserHost: &remote,
		Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
	}
	rp := PackNote(rmt, idGen)
	require.NotNil(t, rp.Emojis, "remote note always carries emojis object")
	assert.Empty(t, *rp.Emojis)
	var rm map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(mustJSON(t, rp), &rm))
	assert.Equal(t, "{}", string(rm["emojis"]), "remote note outputs emojis:{}")
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// #1640 [LOW] REST pack (PackNote) は reactionAndUserPairCache を出さない
// (upstream は withReactionAndUserPairCache=true の streaming/AP 経路のみ出力)。
func TestPackNote_ReactionAndUserPairCacheOmittedInREST(t *testing.T) {
	idGen := newTestIDGen(t)
	n := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic,
		Reactions:                datatypes.JSON([]byte("{}")),
		ReactionAndUserPairCache: []string{"userA/👍"},
	}
	got := PackNote(n, idGen)
	assert.Nil(t, got.ReactionAndUserPairCache, "REST pack omits reactionAndUserPairCache")
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(mustJSON(t, got), &m))
	_, has := m["reactionAndUserPairCache"]
	assert.False(t, has, "REST JSON must not contain reactionAndUserPairCache")
}
