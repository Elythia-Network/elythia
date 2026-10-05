package channels

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNotifications_AuthenticatedSubscribesPerUser(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewNotifications(ctx)
	require.NoError(t, ch.Init(nil))
	assert.Equal(t, []string{"notifications:alice"}, ctx.subs)

	ch.OnRedisEvent([]byte(`{"id":"n1","type":"follow"}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "notification", ctx.sentType[0])

	ch.OnClientMessage("ignored", json.RawMessage(`{}`))
	ch.Dispose()
	assert.Equal(t, []string{"notifications:alice"}, ctx.unsubs)
}

func TestNotifications_AnonymousIsNoOp(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewNotifications(ctx)
	err := ch.Init(nil)
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
	ch.Dispose()
	assert.Empty(t, ctx.unsubs)
}

func TestNotifications_NilUserPointer(t *testing.T) {
	ctx := newCtx((*model.User)(nil))
	ch := NewNotifications(ctx)
	err := ch.Init(nil)
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
}

func TestMain_AuthenticatedSubscribesBoth(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewMain(ctx)
	require.NoError(t, ch.Init(nil))
	assert.ElementsMatch(t, []string{"notifications:alice", "main:alice"}, ctx.subs)

	// envelope なし → "notification" として転送される
	ch.OnRedisEvent([]byte(`{"id":"n1"}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "notification", ctx.sentType[0])

	// envelope あり → そのタイプとして転送される
	ch.OnRedisEvent([]byte(`{"type":"follow","body":{"userId":"u2"}}`))
	require.Len(t, ctx.sentType, 2)
	assert.Equal(t, "follow", ctx.sentType[1])

	// 通知 timeline の bare body は `type` に "mention" 等を含むが、`body`
	// が無いので envelope ではなく "notification" として転送する (#420
	// follow-up: bare 通知 body の routing 修正)。これが無いと frontend
	// `main.on('notification', ...)` が発火せず timeline がライブ更新しない。
	ctx2 := newCtx(&model.User{ID: "alice"})
	ch2 := NewMain(ctx2)
	require.NoError(t, ch2.Init(nil))
	ch2.OnRedisEvent([]byte(`{"id":"n1","type":"mention","createdAt":"x","notifierId":"bob"}`))
	require.Len(t, ctx2.sentType, 1)
	assert.Equal(t, "notification", ctx2.sentType[0],
		"bare notification body must forward as `notification`, not its embedded type")

	ch.OnClientMessage("ignored", json.RawMessage(`{}`))
	ch.Dispose()
	assert.ElementsMatch(t, []string{"notifications:alice", "main:alice"}, ctx.unsubs)
}

func TestMain_AnonymousIsNoOp(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewMain(ctx)
	err := ch.Init(nil)
	assert.ErrorIs(t, err, stream.ErrInvalidParams)
	assert.Empty(t, ctx.subs)
	ch.Dispose()
	assert.Empty(t, ctx.unsubs)
}

func TestMain_BadJSONEnvelopeFallsBack(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewMain(ctx)
	require.NoError(t, ch.Init(nil))
	// invalid JSON は notification にフォールバックして送信される
	ch.OnRedisEvent([]byte(`{not json`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "notification", ctx.sentType[0])
}

func TestMain_FollowEventRefreshesSnapshot(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewMain(ctx)
	require.NoError(t, ch.Init(nil))
	ch.OnRedisEvent([]byte(`{"type":"follow","body":{"id":"followee1"}}`))
	// snapshot が follow で更新される (followee1 を following=true で追加)。
	require.Len(t, ctx.followingUpd, 1)
	assert.Equal(t, followingUpdate{"followee1", true}, ctx.followingUpd[0])
	// client へも従来どおり転送される。
	assert.Equal(t, []string{"follow"}, ctx.sentType)
}

func TestMain_UnfollowEventRefreshesSnapshot(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewMain(ctx)
	require.NoError(t, ch.Init(nil))
	ch.OnRedisEvent([]byte(`{"type":"unfollow","body":{"id":"followee1"}}`))
	require.Len(t, ctx.followingUpd, 1)
	assert.Equal(t, followingUpdate{"followee1", false}, ctx.followingUpd[0])
}

func TestMain_FollowedEventDoesNotRefreshSnapshot(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewMain(ctx)
	require.NoError(t, ch.Init(nil))
	// `followed` (誰かが自分を follow) は自分の follow 一覧を変えないので
	// snapshot を触らない。client へは転送される。
	ch.OnRedisEvent([]byte(`{"type":"followed","body":{"id":"follower1"}}`))
	assert.Empty(t, ctx.followingUpd)
	assert.Equal(t, []string{"followed"}, ctx.sentType)
}

func TestMain_FollowEventMissingIDIgnored(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewMain(ctx)
	require.NoError(t, ch.Init(nil))
	// id が無い follow body は snapshot 更新できないので no-op (転送は継続)。
	ch.OnRedisEvent([]byte(`{"type":"follow","body":{"username":"x"}}`))
	assert.Empty(t, ctx.followingUpd)
	assert.Equal(t, []string{"follow"}, ctx.sentType)
}

// --- #1568: per-viewer notification embed hide ---

func sentBytes(t *testing.T, ctx *stubContext, i int) []byte {
	t.Helper()
	b, ok := ctx.sentBody[i].(json.RawMessage)
	require.True(t, ok, "sent body must be json.RawMessage")
	return []byte(b)
}

// notifWithRenote builds a bare notification ({id,type,noteId,note}) whose note
// renotes a followers-only note by renoteAuthor.
func notifWithRenote(t *testing.T, renoteAuthor string) []byte {
	t.Helper()
	emb := embed("followers", renoteAuthor, nil)
	note := tlStream("base", "noteauthor", "public", "2026-01-02T03:04:05.000Z", entity.UserLite{})
	note.Renote = &emb
	return mustJSON(t, map[string]any{"id": "n1", "type": "renote", "noteId": "base", "note": note})
}

func notifNoteRenote(t *testing.T, body []byte) *entity.NoteEntity {
	t.Helper()
	var m struct {
		Note entity.NoteEntity `json:"note"`
	}
	require.NoError(t, json.Unmarshal(body, &m))
	return m.Note.Renote
}

func TestNotifications_EmbedRenoteHiddenForNonFollower(t *testing.T) {
	ctx := newCtx(&model.User{ID: "viewer"})
	ctx.followingSnap = map[string]bool{} // follows nobody
	ch := NewNotifications(ctx)
	require.NoError(t, ch.Init(nil))
	ch.OnRedisEvent(notifWithRenote(t, "renoteauthor"))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "notification", ctx.sentType[0])
	r := notifNoteRenote(t, sentBytes(t, ctx, 0))
	require.NotNil(t, r)
	assert.True(t, r.IsHidden, "depth-2 followers renote must be blanked for non-follower")
	assert.Nil(t, r.Text)
}

func TestNotifications_EmbedVerbatimForFollower(t *testing.T) {
	ctx := newCtx(&model.User{ID: "viewer"})
	ctx.followingSnap = map[string]bool{"renoteauthor": true}
	ch := NewNotifications(ctx)
	require.NoError(t, ch.Init(nil))
	payload := notifWithRenote(t, "renoteauthor")
	ch.OnRedisEvent(payload)
	assert.True(t, bytes.Equal(sentBytes(t, ctx, 0), payload),
		"follower must receive the notification verbatim (no re-marshal)")
}

func TestNotifications_NoNoteVerbatim(t *testing.T) {
	ctx := newCtx(&model.User{ID: "viewer"})
	ch := NewNotifications(ctx)
	require.NoError(t, ch.Init(nil))
	payload := []byte(`{"id":"n1","type":"follow","userId":"bob"}`)
	ch.OnRedisEvent(payload)
	assert.True(t, bytes.Equal(sentBytes(t, ctx, 0), payload), "note-less notification must be verbatim")
}

func TestMain_BareNotificationEmbedHidden(t *testing.T) {
	ctx := newCtx(&model.User{ID: "viewer"})
	ctx.followingSnap = map[string]bool{}
	ch := NewMain(ctx)
	require.NoError(t, ch.Init(nil))
	ch.OnRedisEvent(notifWithRenote(t, "renoteauthor"))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "notification", ctx.sentType[0])
	r := notifNoteRenote(t, sentBytes(t, ctx, 0))
	require.NotNil(t, r)
	assert.True(t, r.IsHidden, "main bare-notification embed must be blanked for non-follower")
}

func TestMain_EnvelopeReplyNoteEmbedHidden(t *testing.T) {
	ctx := newCtx(&model.User{ID: "viewer"})
	ctx.followingSnap = map[string]bool{}
	ch := NewMain(ctx)
	require.NoError(t, ch.Init(nil))
	emb := embed("followers", "renoteauthor", nil)
	note := tlStream("base", "noteauthor", "public", "2026-01-02T03:04:05.000Z", entity.UserLite{})
	note.Renote = &emb
	payload := mustJSON(t, map[string]any{"type": "reply", "body": note})
	ch.OnRedisEvent(payload)
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "reply", ctx.sentType[0])
	var got entity.NoteEntity
	require.NoError(t, json.Unmarshal(sentBytes(t, ctx, 0), &got))
	require.NotNil(t, got.Renote)
	assert.True(t, got.Renote.IsHidden, "reply-envelope note's depth-2 embed must be blanked")
}

// --- #1711: main channel mention / notification mute-block gate ---

// mentionEnvelope builds a {type:mention, body:<public note>} payload addressed
// to "alice" (so the visibility gate passes via the mentions branch).
func mentionEnvelope(authorID, authorHost string) []byte {
	host := any(nil)
	if authorHost != "" {
		host = authorHost
	}
	return []byte(`{"type":"mention","body":{"id":"n1","userId":"` + authorID +
		`","visibility":"public","mentions":["alice"],"user":{"id":"` + authorID +
		`","host":` + jsonStr(host) + `}}}`)
}

func jsonStr(v any) string {
	if v == nil {
		return "null"
	}
	return `"` + v.(string) + `"`
}

func TestMain_MentionDroppedByInstanceMute(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ctx.muteBlockSnap = &stream.MuteBlockSnapshot{MutedInstances: set("evil.example")}
	ch := NewMain(ctx)
	require.NoError(t, ch.Init(nil))
	ch.OnRedisEvent(mentionEnvelope("bob", "evil.example"))
	assert.Empty(t, ctx.sentType, "mention from muted instance must be dropped")
}

func TestMain_MentionDroppedByUserMute(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ctx.muteBlockSnap = &stream.MuteBlockSnapshot{Muting: set("bob")}
	ch := NewMain(ctx)
	require.NoError(t, ch.Init(nil))
	ch.OnRedisEvent(mentionEnvelope("bob", ""))
	assert.Empty(t, ctx.sentType, "mention from muted user must be dropped")
}

func TestMain_MentionDroppedByBlock(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ctx.muteBlockSnap = &stream.MuteBlockSnapshot{BlockingMe: set("bob")}
	ch := NewMain(ctx)
	require.NoError(t, ch.Init(nil))
	ch.OnRedisEvent(mentionEnvelope("bob", ""))
	assert.Empty(t, ctx.sentType, "mention from a user who blocks me must be dropped")
}

func TestMain_MentionPassesClean(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ctx.muteBlockSnap = &stream.MuteBlockSnapshot{Muting: set("someoneelse"), MutedInstances: set("other.example")}
	ch := NewMain(ctx)
	require.NoError(t, ch.Init(nil))
	ch.OnRedisEvent(mentionEnvelope("bob", "good.example"))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "mention", ctx.sentType[0])
}

func TestMain_MentionNilSnapshotPasses(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"}) // muteBlockSnap nil → fail-open
	ch := NewMain(ctx)
	require.NoError(t, ch.Init(nil))
	ch.OnRedisEvent(mentionEnvelope("bob", "evil.example"))
	require.Len(t, ctx.sentType, 1, "nil snapshot must not drop anything")
	assert.Equal(t, "mention", ctx.sentType[0])
}

func TestMain_MentionDroppedByVisibility(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewMain(ctx)
	require.NoError(t, ch.Init(nil))
	// specified note where alice is not in visibleUserIds → isNoteVisibleForMe false。
	ch.OnRedisEvent([]byte(`{"type":"mention","body":{"id":"n1","userId":"bob","visibility":"specified","visibleUserIds":["carol"]}}`))
	assert.Empty(t, ctx.sentType, "mention of a specified note not visible to me must be dropped")
}

func TestMain_ReplyNotGatedByMute(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ctx.muteBlockSnap = &stream.MuteBlockSnapshot{Muting: set("bob")}
	ch := NewMain(ctx)
	require.NoError(t, ch.Init(nil))
	// upstream main.ts は reply を mute/block で gate しない (notification/mention のみ)。
	ch.OnRedisEvent([]byte(`{"type":"reply","body":{"id":"n1","userId":"bob","visibility":"public"}}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "reply", ctx.sentType[0])
}

func TestNotifications_DroppedByInstanceMute(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ctx.muteBlockSnap = &stream.MuteBlockSnapshot{MutedInstances: set("evil.example")}
	ch := NewNotifications(ctx)
	require.NoError(t, ch.Init(nil))
	ch.OnRedisEvent([]byte(`{"id":"x1","type":"mention","user":{"id":"bob","host":"evil.example"}}`))
	assert.Empty(t, ctx.sentType, "notification from a muted instance must be dropped")
}

func TestMain_BareNotificationDroppedByInstanceMute(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ctx.muteBlockSnap = &stream.MuteBlockSnapshot{MutedInstances: set("evil.example")}
	ch := NewMain(ctx)
	require.NoError(t, ch.Init(nil))
	ch.OnRedisEvent([]byte(`{"id":"x1","type":"reaction","user":{"id":"bob","host":"evil.example"}}`))
	assert.Empty(t, ctx.sentType, "bare notification from a muted instance must be dropped")
}

// unreadNotification envelope の body は packed notification そのものなので、
// bare notification topic と同じ hide ゲートを通す必要がある。#1568 はこの
// envelope を覆っておらず、非可視 embed の内容が「未読バッジ用」イベント経由で
// そのまま届いていた。
func TestMain_UnreadNotificationEnvelopeEmbedHidden(t *testing.T) {
	ctx := newCtx(&model.User{ID: "viewer"})
	ctx.followingSnap = map[string]bool{} // follows nobody
	ch := NewMain(ctx)
	require.NoError(t, ch.Init(nil))

	emb := embed("followers", "renoteauthor", nil)
	emb.FileIDs = []string{"f2"}
	emb.Files = []any{map[string]any{"id": "f2", "url": "https://example.com/secret.png"}}
	note := tlStream("base", "noteauthor", "public", "2026-01-02T03:04:05.000Z", entity.UserLite{})
	note.Renote = &emb
	ch.OnRedisEvent(mustJSON(t, map[string]any{
		"type": "unreadNotification",
		"body": map[string]any{"id": "n1", "type": "renote", "noteId": "base", "note": note},
	}))

	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "unreadNotification", ctx.sentType[0])
	r := notifNoteRenote(t, sentBytes(t, ctx, 0))
	require.NotNil(t, r)
	assert.True(t, r.IsHidden, "unreadNotification の非可視 embed は blank されること")
	assert.Nil(t, r.Text)
	assert.Empty(t, r.Files, "非可視 embed の添付ファイル URL を送らないこと")
}

// positive control: 見える embed なら unreadNotification でも添付ごとそのまま届く
// (ゲートが「常に空にする」で通っているだけではないことを固定する)。
func TestMain_UnreadNotificationEnvelopeVisibleEmbedKeepsFiles(t *testing.T) {
	ctx := newCtx(&model.User{ID: "viewer"})
	ctx.followingSnap = map[string]bool{"renoteauthor": true}
	ch := NewMain(ctx)
	require.NoError(t, ch.Init(nil))

	emb := embed("followers", "renoteauthor", nil)
	emb.FileIDs = []string{"f2"}
	emb.Files = []any{map[string]any{"id": "f2", "url": "https://example.com/ok.png"}}
	note := tlStream("base", "noteauthor", "public", "2026-01-02T03:04:05.000Z", entity.UserLite{})
	note.Renote = &emb
	payload := mustJSON(t, map[string]any{
		"type": "unreadNotification",
		"body": map[string]any{"id": "n1", "type": "renote", "noteId": "base", "note": note},
	})
	ch.OnRedisEvent(payload)

	require.Len(t, ctx.sentType, 1)
	r := notifNoteRenote(t, sentBytes(t, ctx, 0))
	require.NotNil(t, r)
	assert.False(t, r.IsHidden)
	require.Len(t, r.Files, 1)
}

// note を持たない main envelope はゲートに掛けても **byte 単位で** verbatim で
// 通ること。ゲートを type 列挙ではなく全 envelope に掛けているので、無関係な
// イベントを壊していないことを固定する。
//
// **JSONEq ではなく byte 比較にしてある。** ゲートが body を map[string]any 経由で
// re-marshal してしまうと、key 順が並び替わり、2^53 超の整数が float64 で丸まり、
// `<` `&` が \u003c \u0026 にエスケープされる。JSONEq はこれを全て「同値」と
// 見なすので、劣化を素通りさせる。fixture にその 3 つを仕込んである。
//
// **note を持つ envelope は verbatim にならない。** hide が実際に何かを blank した
// ときは hideNotificationNote が map[string]any 経由で組み直すので、上記の変質が
// 起きる。bare notification topic では #1568 以来の挙動で、unreadNotification に
// 及ぶのは新しい。packed notification の top-level に 2^53 超の整数を置く field は
// 現状無い (id 系は文字列、choice は選択肢 index)。
//
// 一覧は `main:<id>` topic に流れる実在の envelope 全 20 種のうち、この else 分岐に
// 来て**かつ note を持たない** 16 種。数え方: `grep -rn "PublishMainEvent(" internal/
// --include='*.go'` から interface 宣言・メソッド定義本体 (stream/main_publisher.go)・
// _test.go を除き、第 2 引数のリテラルを uniq
// した 20 種から、note envelope (reply / renote / mention = isNoteEnvelope 側) と
// unreadNotification (note を**持ちうる**ので verbatim とは限らない。gate の効きは
// TestMain_UnreadNotificationEnvelopeEmbedHidden で見る) を引いた残り。
//
// **これは else 分岐に来るものの全量。** MainChannel は `notifications:<id>` topic も
// 購読しているが、bare 通知は mainStreamEnvelope が packed notification の署名
// (id / createdAt) を見て弾くので envelope 側に来ない。以前は `app` 型が Extra の `body` の top-level merge で誤検出され
// else 分岐に来ていた (#2738、修正済。ガードは
// TestMain_AppNotificationForwardedAsNotification)。
//
// **固定できるのは「この 16 種の body を gate が壊さない」ことまで。** gate を type
// 列挙にしなかった判断は「この 16 種はどれも top-level に `note` を持たない」という
// 事実に寄りかかっているが、payload は手書きなので producer 側と連動しない。将来
// どれかの body に `note` が生えても、この一覧は黙って古びるだけで落ちない。
func TestMain_NonNoteEnvelopeUnaffectedByNotificationGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
	}{
		{"readAllNotifications", `{"type":"readAllNotifications","body":null}`},
		{"notificationFlushed", `{"type":"notificationFlushed","body":null}`},
		{"myTokenRegenerated", `{"type":"myTokenRegenerated","body":null}`},
		// registryUpdated の value は任意のユーザー JSON。2^53 超の整数と HTML 文字を
		// 含めて、re-marshal による丸めとエスケープを検出させる。value の中に "note" を
		// 置いても top-level probe は見ないので影響しないことも同時に固定する。
		{"registryUpdated", `{"type":"registryUpdated","body":{"scope":["client"],"key":"k","value":{"bigint":9007199254740993,"html":"a<b>&c","note":{"id":"not-a-note"}}}}`},
		{"meUpdated", `{"type":"meUpdated","body":{"id":"viewer","name":"me","avatarDecorations":[{"angle":0.25}]}}`},
		{"driveFileCreated", `{"type":"driveFileCreated","body":{"id":"f1","url":"https://example.com/f1"}}`},
		{"announcementCreated", `{"type":"announcementCreated","body":{"announcement":{"id":"a1","text":"x & y"}}}`},
		{"newChatMessage", `{"type":"newChatMessage","body":{"id":"m1","text":"hi","fromUserId":"bob"}}`},
		{"signin", `{"type":"signin","body":{"id":"s1","ip":"127.0.0.1","success":true}}`},
		{"urlUploadFinished", `{"type":"urlUploadFinished","body":{"marker":"m","file":{"id":"f1"}}}`},
		{"pageEvent", `{"type":"pageEvent","body":{"pageId":"p1","event":"e","var":{"n":1}}}`},
		{"readAllAnnouncements", `{"type":"readAllAnnouncements","body":null}`},
		{"follow", `{"type":"follow","body":{"id":"bob","username":"bob"}}`},
		{"followed", `{"type":"followed","body":{"id":"bob","username":"bob"}}`},
		{"unfollow", `{"type":"unfollow","body":{"id":"bob","username":"bob"}}`},
		{"receiveFollowRequest", `{"type":"receiveFollowRequest","body":{"id":"bob","username":"bob"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newCtx(&model.User{ID: "viewer"})
			ch := NewMain(ctx)
			require.NoError(t, ch.Init(nil))
			ch.OnRedisEvent([]byte(tc.payload))
			require.Len(t, ctx.sentType, 1)
			assert.Equal(t, tc.name, ctx.sentType[0], "envelope の type がそのまま転送されること")
			var env struct {
				Body json.RawMessage `json:"body"`
			}
			require.NoError(t, json.Unmarshal([]byte(tc.payload), &env))
			assert.Equal(t, string(env.Body), string(sentBytes(t, ctx, 0)),
				"note を持たない body は byte 単位で verbatim であること")
		})
	}
}

// #2738: `app` 通知 (notifications/create) は Extra の body / header / icon が
// packer で top-level に merge されるので、packed body が
// `{"id":..,"type":"app","createdAt":..,"body":"hello",..}` になる。`type` と
// `body` が揃っていることだけを見ていた旧判定では、これを main envelope と
// 誤検出してイベント名 `app` / body は文字列 `"hello"` として送っていた。
// 期待は `notification` イベント + 通知オブジェクト全体。
//
// body が JSON object のケースも並べてある。**body の型で切る判定を採らない**
// ことを固定するため — envelope 側にも `body:null` があり、bare 側にも文字列と
// object の両方があるので、body の形では両方向に誤る。
func TestMain_AppNotificationForwardedAsNotification(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
	}{
		{
			"string body",
			`{"id":"n1","type":"app","createdAt":"2026-01-02T03:04:05.000Z","body":"hello","header":"h","icon":"i"}`,
		},
		{
			// body が object でも routing は変わらないこと。
			"object body",
			`{"id":"n2","type":"app","createdAt":"2026-01-02T03:04:05.000Z","body":{"text":"hello"},"header":null,"icon":null}`,
		},
		{
			// header / icon が無く top-level が id / createdAt / type / body の
			// 4 キーだけでも bare 側であること。
			"body only",
			`{"id":"n3","type":"app","createdAt":"2026-01-02T03:04:05.000Z","body":"hello"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newCtx(&model.User{ID: "viewer"})
			ch := NewMain(ctx)
			require.NoError(t, ch.Init(nil))
			ch.OnRedisEvent([]byte(tc.payload))
			require.Len(t, ctx.sentType, 1)
			assert.Equal(t, "notification", ctx.sentType[0],
				"app notification must forward as `notification`, not as its `type` field")
			assert.Equal(t, tc.payload, string(sentBytes(t, ctx, 0)),
				"body は通知オブジェクト全体 (verbatim) であること")
		})
	}
}

// bare 通知の `type` が envelope に実在する type と一致していても envelope 扱い
// しないこと (#420 の回帰ガード)。`body` を持つ通知が現れても成立し続けるよう、
// `body` 付きのケースも並べる (#2738)。
func TestMain_BareNotificationWithEnvelopeTypeIsNotEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
	}{
		{"mention", `{"id":"n1","type":"mention","createdAt":"x","notifierId":"bob"}`},
		{"reply", `{"id":"n1","type":"reply","createdAt":"x","noteId":"a1"}`},
		{"renote", `{"id":"n1","type":"renote","createdAt":"x","noteId":"a1"}`},
		{"follow", `{"id":"n1","type":"follow","createdAt":"x","userId":"bob"}`},
		// bare 側に `body` が生えても envelope 扱いしないこと。
		{"mention with body", `{"id":"n1","type":"mention","createdAt":"x","body":{"id":"spoofed"}}`},
		{"follow with body", `{"id":"n1","type":"follow","createdAt":"x","body":{"id":"spoofed"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newCtx(&model.User{ID: "viewer"})
			ch := NewMain(ctx)
			require.NoError(t, ch.Init(nil))
			ch.OnRedisEvent([]byte(tc.payload))
			require.Len(t, ctx.sentType, 1)
			assert.Equal(t, "notification", ctx.sentType[0],
				"bare notification must forward as `notification`, not its embedded type")
			assert.Equal(t, tc.payload, string(sentBytes(t, ctx, 0)))
		})
	}
}

// mainStreamEnvelope の判定境界を直接固定する。OnRedisEvent 経由では
// hide gate を挟むので、判定そのものの入出力はここで見る。
func TestMainStreamEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name     string
		payload  string
		wantOK   bool
		wantType string
		wantBody string
	}{
		{"envelope", `{"type":"follow","body":{"id":"bob"}}`, true, "follow", `{"id":"bob"}`},
		{"envelope with null body", `{"type":"readAllNotifications","body":null}`, true, "readAllNotifications", "null"},
		{"envelope with string body", `{"type":"pageEvent","body":"x"}`, true, "pageEvent", `"x"`},
		// 逆順でもキー集合が同じなら envelope。
		{"envelope key order", `{"body":{"id":"bob"},"type":"follow"}`, true, "follow", `{"id":"bob"}`},
		// envelope にキーが増えても envelope のまま (判定は packed notification の
		// 署名を見ており、キー数を数えていない)。
		{"envelope with extra key", `{"type":"follow","body":{"id":"bob"},"seq":1}`, true, "follow", `{"id":"bob"}`},
		{"app notification", `{"id":"n1","type":"app","createdAt":"x","body":"hello"}`, false, "", ""},
		{"bare notification", `{"id":"n1","type":"mention","createdAt":"x"}`, false, "", ""},
		// id / createdAt はどちらか一方でも packed notification の署名として扱う。
		{"id only", `{"id":"n1","type":"app","body":"hello"}`, false, "", ""},
		{"createdAt only", `{"createdAt":"x","type":"app","body":"hello"}`, false, "", ""},
		// null でもキーが在れば署名 (packer は必ず値を入れるので実在しないが、
		// 「存在」で判定していることを固定する)。
		{"null id", `{"id":null,"type":"app","body":"hello"}`, false, "", ""},
		{"type only", `{"type":"follow"}`, false, "", ""},
		{"body only", `{"body":{"id":"bob"}}`, false, "", ""},
		{"empty type", `{"type":"","body":null}`, false, "", ""},
		{"non-string type", `{"type":1,"body":null}`, false, "", ""},
		{"two keys but not type/body", `{"id":"n1","createdAt":"x"}`, false, "", ""},
		{"not an object", `["type","body"]`, false, "", ""},
		{"invalid json", `{`, false, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotType, gotBody, ok := mainStreamEnvelope([]byte(tc.payload))
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantType, gotType)
			assert.Equal(t, tc.wantBody, string(gotBody))
		})
	}
}
