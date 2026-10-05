package notifications

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// strPtr is a tiny helper for *string fields on model rows.
func strPtr(s string) *string { return &s }

// groupedHandler wires a handler with user / note repos + query service so the
// grouped endpoint can resolve notifier users and viewer-visible notes.
func groupedHandler(t *testing.T) (*Handler, *notification.Service, *testutil.MockUserRepository, *testutil.MockNoteRepository) {
	t.Helper()
	h, svc := newTestHandler(t)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	h.SetRepos(userRepo, noteRepo)
	h.SetQueryService(corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository()))
	return h, svc, userRepo, noteRepo
}

func decodeGrouped(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(body, &resp))
	return resp
}

func TestGrouped_Empty(t *testing.T) {
	h, _, _, _ := groupedHandler(t)
	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, decodeGrouped(t, rec.Body.Bytes()))
}

func TestGrouped_InvalidParam(t *testing.T) {
	h, _, _, _ := groupedHandler(t)
	// malformed JSON body → bind error → INVALID_PARAM.
	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 同一 note への連続 reaction が reaction:grouped に畳み込まれることを確認する。
func TestGrouped_ReactionsGroupOnSameNote(t *testing.T) {
	h, svc, userRepo, noteRepo := groupedHandler(t)
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol"}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice", Visibility: "public", User: &model.User{ID: "alice", Username: "alice"}}

	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeReaction, NoteID: "n1", Reaction: "👍"})
	require.NoError(t, err)
	_, err = svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: "carol", Type: notification.TypeReaction, NoteID: "n1", Reaction: "🎉"})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{"limit":50}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)

	resp := decodeGrouped(t, rec.Body.Bytes())
	require.Len(t, resp, 1)
	assert.Equal(t, "reaction:grouped", resp[0]["type"])
	assert.NotNil(t, resp[0]["note"])
	reactions, ok := resp[0]["reactions"].([]any)
	require.True(t, ok)
	require.Len(t, reactions, 2)
	// 各 entry は {user, reaction}。
	for _, r := range reactions {
		entry := r.(map[string]any)
		assert.NotNil(t, entry["user"])
		assert.NotEmpty(t, entry["reaction"])
	}
}

// TestGrouped_NonVisibleEmbedHidden locks in the #1546/#1570 fix: a followers-only
// renote embedded in a notifications-grouped note must be blanked for a viewer who
// cannot see it. notehide.HideNotificationNotes fail-closes on the unset
// package-level following repo, so a non-follower viewer never sees the embed body.
func TestGrouped_NonVisibleEmbedHidden(t *testing.T) {
	h, svc, userRepo, noteRepo := groupedHandler(t)
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	secret := "secret"
	renote := &model.Note{ID: "emb", UserID: "stranger", Visibility: "followers", User: &model.User{ID: "stranger", Username: "stranger"}, Text: &secret}
	noteRepo.Notes["emb"] = renote
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Visibility: "public", User: &model.User{ID: "bob", Username: "bob"}, RenoteID: strPtr("emb"), Renote: renote}

	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeReaction, NoteID: "n1", Reaction: "👍"})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{"limit":50}`)
	setAuth(c, &model.User{ID: "alice"}) // alice does not follow stranger
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)

	resp := decodeGrouped(t, rec.Body.Bytes())
	require.Len(t, resp, 1)
	note, ok := resp[0]["note"].(map[string]any)
	require.True(t, ok, "notification must carry a note")
	renoteMap, ok := note["renote"].(map[string]any)
	require.True(t, ok, "embedded renote must be present")
	assert.Equal(t, true, renoteMap["isHidden"], "followers renote embed must be hidden from a non-follower viewer (#1546 IDOR guard)")
	assert.Nil(t, renoteMap["text"], "hidden embed text must be blanked")
}

// 異なる note への reaction は別グループ (= 単体 notification) のまま残る。
func TestGrouped_ReactionsDifferentNotesStaySeparate(t *testing.T) {
	h, svc, userRepo, noteRepo := groupedHandler(t)
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice", Visibility: "public", User: &model.User{ID: "alice"}}
	noteRepo.Notes["n2"] = &model.Note{ID: "n2", UserID: "alice", Visibility: "public", User: &model.User{ID: "alice"}}

	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeReaction, NoteID: "n1", Reaction: "👍"})
	require.NoError(t, err)
	_, err = svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeReaction, NoteID: "n2", Reaction: "👍"})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{"limit":50}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))

	resp := decodeGrouped(t, rec.Body.Bytes())
	require.Len(t, resp, 2)
	assert.Equal(t, "reaction", resp[0]["type"])
	assert.Equal(t, "reaction", resp[1]["type"])
}

// 同一 target note への連続 renote が renote:grouped (users[]) に畳み込まれる。
func TestGrouped_RenotesGroupOnSameTarget(t *testing.T) {
	h, svc, userRepo, noteRepo := groupedHandler(t)
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol"}
	// 2 つの renote note は別 ID だが同じ target ("target") を renote する。
	noteRepo.Notes["rn1"] = &model.Note{ID: "rn1", UserID: "bob", Visibility: "public", RenoteID: strPtr("target"), User: &model.User{ID: "bob"}}
	noteRepo.Notes["rn2"] = &model.Note{ID: "rn2", UserID: "carol", Visibility: "public", RenoteID: strPtr("target"), User: &model.User{ID: "carol"}}

	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeRenote, NoteID: "rn1"})
	require.NoError(t, err)
	_, err = svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: "carol", Type: notification.TypeRenote, NoteID: "rn2"})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{"limit":50}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))

	resp := decodeGrouped(t, rec.Body.Bytes())
	require.Len(t, resp, 1)
	assert.Equal(t, "renote:grouped", resp[0]["type"])
	// users[] は **順序も** 見る。upstream notifications-grouped.ts:141 が
	// `userIds: [prev.notifierId]` で初期化し :145 が cur を push するので
	// [prev, cur] の順。降順ページでは新しい notifier が先頭になる。
	// 件数だけ見ると順序反転を取りこぼす (reaction:grouped 側と対称、#2736)。
	assert.Equal(t, []string{"carol", "bob"}, groupedUserIDs(t, resp[0]),
		"users[] の順序が upstream と一致すること")
}

// reaction group の createdAt は seed に使う prev の時刻を採る (upstream
// notifications-grouped.ts:118 の `createdAt: prev.createdAt`)。prev は一覧の並び順で
// 1 つ前なので、既定の降順ページでは新しい方 (sinceId 単独ページは昇順になるため
// 逆転する。fixture は降順で組んである)。
//
// endpoint 経由だと 2 件の通知が同一ミリ秒になりうるので、groupNotifications を
// 直接叩いて createdAt を別値に固定する。#2736 で prev/cur の引き方を index から
// id 突き合わせに変えたので、**createdAt の**取り違えを検出できるのはこのアサート
// だけ (user / reaction の対の取り違えは
// TestGrouped_GroupingSurvivesDroppedNotification 側が拾う)。
func TestGroupNotifications_ReactionGroupTakesPrevCreatedAt(t *testing.T) {
	newer := &notification.Notification{ID: "n2", Type: notification.TypeReaction, NotifierID: "carol", NoteID: "n1", Reaction: "🎉"}
	older := &notification.Notification{ID: "n1", Type: notification.TypeReaction, NotifierID: "bob", NoteID: "n1", Reaction: "👍"}
	packed := []map[string]any{
		{"id": "n2", "type": "reaction", "noteId": "n1", "reaction": "🎉", "createdAt": "2026-01-02T03:04:06.000Z", "user": map[string]any{"id": "carol"}},
		{"id": "n1", "type": "reaction", "noteId": "n1", "reaction": "👍", "createdAt": "2026-01-02T03:04:05.000Z", "user": map[string]any{"id": "bob"}},
	}

	out := groupNotifications([]*notification.Notification{newer, older}, packed, nil)
	require.Len(t, out, 1)
	assert.Equal(t, "reaction:grouped", out[0]["type"])
	assert.Equal(t, "2026-01-02T03:04:06.000Z", out[0]["createdAt"],
		"group の createdAt は prev を採ること (降順 fixture なので新しい側)")
}

// renote group は逆に cur の createdAt を採る (降順ページでは古い方。#2106 L23、
// upstream notifications-grouped.ts:139 の `createdAt: notification.createdAt`)。
// **型ごとに逆**なのが罠で、upstream も :118 が `prev.createdAt`、:139 が
// `notification.createdAt` と書き分けている。
func TestGroupNotifications_RenoteGroupTakesCurCreatedAt(t *testing.T) {
	newer := &notification.Notification{ID: "n2", Type: notification.TypeRenote, NotifierID: "carol", NoteID: "rn2"}
	older := &notification.Notification{ID: "n1", Type: notification.TypeRenote, NotifierID: "bob", NoteID: "rn1"}
	packed := []map[string]any{
		{"id": "n2", "type": "renote", "noteId": "rn2", "userId": "carol", "createdAt": "2026-01-02T03:04:06.000Z", "user": map[string]any{"id": "carol"}},
		{"id": "n1", "type": "renote", "noteId": "rn1", "userId": "bob", "createdAt": "2026-01-02T03:04:05.000Z", "user": map[string]any{"id": "bob"}},
	}
	noteByID := map[string]*model.Note{
		"rn2": {ID: "rn2", UserID: "carol", RenoteID: strPtr("target")},
		"rn1": {ID: "rn1", UserID: "bob", RenoteID: strPtr("target")},
	}

	out := groupNotifications([]*notification.Notification{newer, older}, packed, noteByID)
	require.Len(t, out, 1)
	assert.Equal(t, "renote:grouped", out[0]["type"])
	assert.Equal(t, "2026-01-02T03:04:05.000Z", out[0]["createdAt"],
		"group の createdAt は cur を採ること (降順 fixture なので古い側)")
}

// groupedUserIDs extracts the user ids of a renote:grouped row **in order**.
func groupedUserIDs(t *testing.T, row map[string]any) []string {
	t.Helper()
	users, ok := row["users"].([]any)
	require.True(t, ok, "users must be present on a renote:grouped row")
	out := make([]string, 0, len(users))
	for _, u := range users {
		m, ok := u.(map[string]any)
		require.True(t, ok)
		out = append(out, m["id"].(string))
	}
	return out
}

// 異なる target を renote する notification は別グループのまま残る。
func TestGrouped_RenotesDifferentTargetsStaySeparate(t *testing.T) {
	h, svc, userRepo, noteRepo := groupedHandler(t)
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	noteRepo.Notes["rn1"] = &model.Note{ID: "rn1", UserID: "bob", Visibility: "public", RenoteID: strPtr("t1"), User: &model.User{ID: "bob"}}
	noteRepo.Notes["rn2"] = &model.Note{ID: "rn2", UserID: "bob", Visibility: "public", RenoteID: strPtr("t2"), User: &model.User{ID: "bob"}}

	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeRenote, NoteID: "rn1"})
	require.NoError(t, err)
	_, err = svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeRenote, NoteID: "rn2"})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{"limit":50}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))

	resp := decodeGrouped(t, rec.Body.Bytes())
	require.Len(t, resp, 2)
	assert.Equal(t, "renote", resp[0]["type"])
	assert.Equal(t, "renote", resp[1]["type"])
}

// #2106 N6: renote 通知の notifier user が解決できない (= hard-delete 等) 場合、その通知は
// 丸ごと drop される (upstream #packInternal の needsUser && !userIfNeed)。bob が解決不能なら
// bob の renote 通知は落ち、解決できた carol の renote 1 件のみが残る (単独なので grouped で
// なく単発の "renote")。
func TestGrouped_RenoteUnresolvedNotifierDropped(t *testing.T) {
	h, svc, userRepo, noteRepo := groupedHandler(t)
	// notifier bob は userRepo に登録しない → notifier 解決不能 → 通知ごと drop。
	userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol"}
	noteRepo.Notes["rn1"] = &model.Note{ID: "rn1", UserID: "bob", Visibility: "public", RenoteID: strPtr("target"), User: &model.User{ID: "bob"}}
	noteRepo.Notes["rn2"] = &model.Note{ID: "rn2", UserID: "carol", Visibility: "public", RenoteID: strPtr("target"), User: &model.User{ID: "carol"}}

	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: "carol", Type: notification.TypeRenote, NoteID: "rn2"})
	require.NoError(t, err)
	_, err = svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeRenote, NoteID: "rn1"})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{"limit":50}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))

	resp := decodeGrouped(t, rec.Body.Bytes())
	require.Len(t, resp, 1, "bob (解決不能 notifier) の通知は drop され carol の 1 件のみ")
	assert.Equal(t, "renote", resp[0]["type"], "残り 1 件は単発の renote")
}

// reaction group の後ろに非グループ通知 (follow) が並ぶと grouping が途切れる。
func TestGrouped_MixedTypesBreakGroup(t *testing.T) {
	h, svc, userRepo, noteRepo := groupedHandler(t)
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol"}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice", Visibility: "public", User: &model.User{ID: "alice"}}

	ctx := context.Background()
	// 作成順 (古い→新しい): follow, reaction, reaction。
	// list は newest-first なので [react2, react1, follow]。
	_, err := svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow})
	require.NoError(t, err)
	_, err = svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeReaction, NoteID: "n1", Reaction: "👍"})
	require.NoError(t, err)
	_, err = svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: "carol", Type: notification.TypeReaction, NoteID: "n1", Reaction: "🎉"})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{"limit":50}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))

	resp := decodeGrouped(t, rec.Body.Bytes())
	require.Len(t, resp, 2)
	assert.Equal(t, "reaction:grouped", resp[0]["type"])
	assert.Equal(t, "follow", resp[1]["type"])
}

// limit が endpoint 全体として効くことを確認する。3 つの別 note への reaction を
// limit=2 で取ると 2 件になる。
//
// **切り詰めているのは grouping 後の slice ではなく `svc.List`。** List は
// `len(out) >= limit` で打ち切るので、grouping が件数を増やさない以上あの slice は
// 到達しない (grouped.go 側のコメント参照)。ここが固定しているのは endpoint の
// 外形であって slice の実装ではない。
func TestGrouped_LimitAppliesToResult(t *testing.T) {
	h, svc, userRepo, noteRepo := groupedHandler(t)
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	for _, id := range []string{"n1", "n2", "n3"} {
		noteRepo.Notes[id] = &model.Note{ID: id, UserID: "alice", Visibility: "public", User: &model.User{ID: "alice"}}
	}

	ctx := context.Background()
	for _, id := range []string{"n1", "n2", "n3"} {
		_, err := svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeReaction, NoteID: id, Reaction: "👍"})
		require.NoError(t, err)
	}

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{"limit":2}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))

	resp := decodeGrouped(t, rec.Body.Bytes())
	require.Len(t, resp, 2)
}

// renote 通知の **renote note 自体** (= 通知の NoteID が指す note) が削除済 / 非可視で
// pack できない場合、通知行ごと drop する (#1953)。grouped 経路でも note-required
// 通知が落ちて空になることを確認する (以前は noteId だけの行を残していた)。
//
// renoteTargetID が "target note" と呼ぶのは その note の RenoteID のほうなので
// 取り違えないこと。ここで落としているのは renote note 側。
//
// 削除済 note の drop は upstream packMany と同じだが、**不可視 note まで落とすのは
// mk-go 独自** (upstream は hideNote で blank して行を残す)。
func TestGrouped_RenoteWithUnresolvableTargetDropped(t *testing.T) {
	h, svc, userRepo, noteRepo := groupedHandler(t)
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	// renote note 自体を repo に登録しない → noteByID に乗らず note pack 不能。
	_ = noteRepo

	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeRenote, NoteID: "rn1"})
	require.NoError(t, err)
	_, err = svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeRenote, NoteID: "rn2"})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{"limit":50}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))

	resp := decodeGrouped(t, rec.Body.Bytes())
	require.Len(t, resp, 0, "note を pack できない note-required 通知は drop される (#1953)")
}

// grouped endpoint も markAsRead の暗黙副作用を持つ (#420 と同挙動)。
func TestGrouped_MarksAsRead(t *testing.T) {
	h, svc, userRepo, _ := groupedHandler(t)
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)

	assert.Contains(t, pub.types("alice"), "readAllNotifications",
		"Grouped should publish readAllNotifications by default")
}

// markAsRead:false を明示した grouped 要求は既読化副作用を発火させない。
func TestGrouped_MarkAsReadFalseSkipsRead(t *testing.T) {
	h, svc, userRepo, _ := groupedHandler(t)
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{"markAsRead":false}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)

	assert.NotContains(t, pub.types("alice"), "readAllNotifications",
		"Grouped with markAsRead:false must not publish readAllNotifications")
}

// #2062: includeTypes/excludeTypes の enum 外は 400、enum 内 (obsolete 含む) は通る。
func TestGrouped_NotificationTypeEnum(t *testing.T) {
	h, _, _, _ := groupedHandler(t)
	// enum 外 type → 400。
	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{"includeTypes":["bogus"]}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "enum 外 includeTypes は 400 (#2062)")

	// excludeTypes enum 外 → 400。
	c, rec = newJSONRequest(t, "/api/i/notifications-grouped", `{"excludeTypes":["nope"]}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "enum 外 excludeTypes は 400 (#2062)")

	// enum 内 (obsolete pollVote 含む) → 200。
	c, rec = newJSONRequest(t, "/api/i/notifications-grouped", `{"excludeTypes":["reaction","pollVote"]}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	assert.Equal(t, http.StatusOK, rec.Code, "enum 内 (obsolete 含む) は通る")
}

// #2736: packer が通知を drop すると filtered と packed の長さがずれ、
// grouped が index out of range で panic して 500 を返していた。
// role 削除済の roleAssigned は packer が通知ごと落とす。
func TestGrouped_DroppedRoleAssignedDoesNotPanic(t *testing.T) {
	h, svc := newTestHandler(t)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bobuser"}
	h.SetRepos(userRepo, noteRepo)
	// role 削除済 = lookup が false を返す
	h.SetRoleLookup(func(string) (map[string]any, bool) { return nil, false })

	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", Type: notification.TypeRoleAssigned,
		Extra: map[string]any{"roleId": "deleted-role"},
	})
	require.NoError(t, err)
	_, err = svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1, "drop された roleAssigned は結果に出ない")
	assert.Equal(t, "follow", resp[0]["type"])
}

// invitation 削除済の chatRoomInvitationReceived も同じく drop される。
// accept / reject で日常的に起きるので、こちらも固定する。
func TestGrouped_DroppedChatInvitationDoesNotPanic(t *testing.T) {
	h, svc := newTestHandler(t)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bobuser"}
	h.SetRepos(userRepo, noteRepo)
	h.SetChatInvitationLookup(func(string, string) (map[string]any, bool) { return nil, false })

	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob",
		Type:  notification.TypeChatRoomInvitationReceived,
		Extra: map[string]any{"invitationId": "gone"},
	})
	require.NoError(t, err)
	_, err = svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	assert.Equal(t, "follow", resp[0]["type"])
}

// grouped 側も同じ resolver を通ること (Show とは別の pack 呼び出し)。
func TestGrouped_ResolvesNoteFiles(t *testing.T) {
	h, svc := newTestHandler(t)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	driveRepo := testutil.NewMockDriveFileRepository()
	driveRepo.Files["f1"] = &model.DriveFile{
		ID: "f1", UserID: strPtr("bob"), Name: "a.png", Type: "image/png",
		URL: "https://example.com/f1",
	}
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bobuser"}
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "bob", Visibility: "public",
		FileIDs: model.StringArray{"f1"},
		User:    &model.User{ID: "bob", Username: "bobuser"},
	}
	h.SetRepos(userRepo, noteRepo)
	h.SetQueryService(corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository()))
	h.SetNoteFieldResolver(entity.NewNoteFieldResolver(driveRepo, nil, nil, nil, nil, idGen))

	_, err := svc.Create(context.Background(), notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeMention, NoteID: "n1",
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	note := resp[0]["note"].(map[string]any)
	require.Len(t, note["files"].([]any), 1)
}

// drop が grouping の対応付けを壊していないこと。#2736 の修正は filtered の index を
// 使わなくなったので、group に別の通知が混ざらないことを user/reaction の対で見る
// (件数だけ見ると off-by-one を取りこぼす)。
func TestGrouped_GroupingSurvivesDroppedNotification(t *testing.T) {
	h, svc := newTestHandler(t)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bobuser"}
	userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol"}
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "alice", Visibility: "public",
		User: &model.User{ID: "alice", Username: "alice"},
	}
	h.SetRepos(userRepo, noteRepo)
	h.SetQueryService(corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository()))
	h.SetRoleLookup(func(string) (map[string]any, bool) { return nil, false })

	ctx := context.Background()
	// 古い順に作る (一覧は newest first)。drop される roleAssigned は末尾 (= 最古)。
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", Type: notification.TypeRoleAssigned,
		Extra: map[string]any{"roleId": "deleted-role"},
	})
	require.NoError(t, err)
	for _, spec := range []struct{ notifier, reaction string }{{"bob", "\U0001F44D"}, {"carol", "\U0001F389"}} {
		_, err = svc.Create(ctx, notification.CreateInput{
			NotifieeID: "alice", NotifierID: spec.notifier, Type: notification.TypeReaction,
			NoteID: "n1", Reaction: spec.reaction,
		})
		require.NoError(t, err)
	}

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1, "2 件の reaction が 1 グループに畳まれること")
	assert.Equal(t, "reaction:grouped", resp[0]["type"])
	// 対応 (user↔reaction) だけでなく **順序** も見る。upstream は [prev, cur] の順に
	// push するので newest first。map に畳むと順序反転を取りこぼす。
	assert.Equal(t,
		[]groupedReaction{{User: "carol", Reaction: "\U0001F389"}, {User: "bob", Reaction: "\U0001F44D"}},
		reactionEntries(t, resp[0]),
		"group の各 entry で user と reaction の対応と順序が保たれること")
}

// #2736: **列の先頭 (= 最新) が drop されるケース。** ここが id 突き合わせでないと
// 壊れる唯一の形で、「panic だけ bounds-guard で塞ぐ」素朴な修正はこの fixture が
// 無いと素通りする (drop が末尾や中間にあるうちは index 版と出力が一致してしまう)。
//
// 先頭が落ちると後続の index が 1 つずつずれるので、bounds-guard 版は 2 件の
// reaction を別グループとして出す。
func TestGrouped_DroppedHeadKeepsGrouping(t *testing.T) {
	h, svc := newTestHandler(t)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bobuser"}
	userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol"}
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "alice", Visibility: "public",
		User: &model.User{ID: "alice", Username: "alice"},
	}
	h.SetRepos(userRepo, noteRepo)
	h.SetQueryService(corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository()))
	h.SetRoleLookup(func(string) (map[string]any, bool) { return nil, false })

	ctx := context.Background()
	// 古い順に作る。一覧は newest first なので drop される roleAssigned が先頭に来る。
	for _, spec := range []struct{ notifier, reaction string }{{"bob", "\U0001F44D"}, {"carol", "\U0001F389"}} {
		_, err := svc.Create(ctx, notification.CreateInput{
			NotifieeID: "alice", NotifierID: spec.notifier, Type: notification.TypeReaction,
			NoteID: "n1", Reaction: spec.reaction,
		})
		require.NoError(t, err)
	}
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", Type: notification.TypeRoleAssigned,
		Extra: map[string]any{"roleId": "deleted-role"},
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1, "先頭 drop の後ろの 2 件は 1 グループに畳まれること")
	assert.Equal(t, "reaction:grouped", resp[0]["type"])
	assert.Equal(t,
		[]groupedReaction{{User: "carol", Reaction: "\U0001F389"}, {User: "bob", Reaction: "\U0001F44D"}},
		reactionEntries(t, resp[0]))
}

// #2736: pack で drop された通知は出力に出ないが、**グループの区切りとしては残る**。
// upstream i/notifications-grouped は raw 列で grouping してから packGroupedMany で
// null を落とす順なので、間に drop を挟んだ 2 件の同一 note reaction は畳まれない。
func TestGrouped_DroppedNotificationBreaksGrouping(t *testing.T) {
	h, svc := newTestHandler(t)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bobuser"}
	userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol"}
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "alice", Visibility: "public",
		User: &model.User{ID: "alice", Username: "alice"},
	}
	h.SetRepos(userRepo, noteRepo)
	h.SetQueryService(corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository()))
	h.SetRoleLookup(func(string) (map[string]any, bool) { return nil, false })

	ctx := context.Background()
	// 古い順: reaction(bob) → drop される roleAssigned → reaction(carol)。
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeReaction,
		NoteID: "n1", Reaction: "\U0001F44D",
	})
	require.NoError(t, err)
	_, err = svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", Type: notification.TypeRoleAssigned,
		Extra: map[string]any{"roleId": "deleted-role"},
	})
	require.NoError(t, err)
	_, err = svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "carol", Type: notification.TypeReaction,
		NoteID: "n1", Reaction: "\U0001F389",
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 2, "drop を挟んだ reaction は畳まれず 2 行のまま")
	for _, row := range resp {
		assert.Equal(t, "reaction", row["type"], "grouped に畳まれていないこと")
	}
}

// groupedReaction is one {user, reaction} pair of a reaction:grouped row.
type groupedReaction struct {
	User     string
	Reaction string
}

// reactionEntries extracts the {user.id, reaction} pairs of a reaction:grouped
// row **in order**, so both the pairing and the ordering can be asserted.
func reactionEntries(t *testing.T, row map[string]any) []groupedReaction {
	t.Helper()
	entries, ok := row["reactions"].([]any)
	require.True(t, ok, "reactions must be present on a grouped row")
	out := make([]groupedReaction, 0, len(entries))
	for _, e := range entries {
		m, ok := e.(map[string]any)
		require.True(t, ok)
		u, ok := m["user"].(map[string]any)
		require.True(t, ok, "each entry must carry its own user")
		out = append(out, groupedReaction{User: u["id"].(string), Reaction: m["reaction"].(string)})
	}
	return out
}

// mutedGroupedHandler wires the grouped handler with a muting repo so a
// notification from `mutee` is dropped at collectNotifications.
func mutedGroupedHandler(t *testing.T, mutee string) (*Handler, *notification.Service, *testutil.MockUserRepository, *testutil.MockNoteRepository) {
	t.Helper()
	h, svc, userRepo, noteRepo := groupedHandler(t)
	mutingRepo := testutil.NewMockMutingRepository()
	require.NoError(t, mutingRepo.Create(&model.Muting{ID: "mu1", MuterID: "alice", MuteeID: mutee}))
	h.SetMutingRepo(mutingRepo)
	return h, svc, userRepo, noteRepo
}

// #2739: **grouping 条件に当たらない**通知が drop されても、区切りとして働くこと。
//
// carol の reaction → bob (mute 済) の follow → dave の reaction、の順で届いた
// 場合、upstream は follow を区切りとして 2 行返す。mk-go は drop を grouping の
// 前に行っていたため 1 グループに畳んでいた。
func TestGrouped_DroppedNonGroupableActsAsSeparator(t *testing.T) {
	h, svc, userRepo, noteRepo := mutedGroupedHandler(t, "bob")
	for _, id := range []string{"bob", "carol", "dave"} {
		userRepo.Users[id] = &model.User{ID: id, Username: id}
	}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice", Visibility: "public", User: &model.User{ID: "alice", Username: "alice"}}

	ctx := context.Background()
	mk := func(in notification.CreateInput) {
		t.Helper()
		_, err := svc.Create(ctx, in)
		require.NoError(t, err)
	}
	mk(notification.CreateInput{NotifieeID: "alice", NotifierID: "carol", Type: notification.TypeReaction, NoteID: "n1", Reaction: "👍"})
	mk(notification.CreateInput{NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow})
	mk(notification.CreateInput{NotifieeID: "alice", NotifierID: "dave", Type: notification.TypeReaction, NoteID: "n1", Reaction: "🎉"})

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{"limit":50}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)

	resp := decodeGrouped(t, rec.Body.Bytes())
	require.Len(t, resp, 2, "drop された follow が区切りとして働く")
	for _, r := range resp {
		assert.Equal(t, "reaction", r["type"], "1 件ずつなので単体 shape")
	}
	assert.NotContains(t, []any{resp[0]["userId"], resp[1]["userId"]}, "bob", "mute 済の行は出さない")
}

// #2739: 同型でも **noteId が違えば** grouping 条件に当たらないので区切りになる。
// 「reaction 型だから当たる」と読んで除外だけに倒すとこの形を 1 行に畳む。
func TestGrouped_DroppedReactionOnOtherNoteActsAsSeparator(t *testing.T) {
	h, svc, userRepo, noteRepo := mutedGroupedHandler(t, "bob")
	for _, id := range []string{"bob", "carol", "dave"} {
		userRepo.Users[id] = &model.User{ID: id, Username: id}
	}
	for _, nid := range []string{"n1", "n2"} {
		noteRepo.Notes[nid] = &model.Note{ID: nid, UserID: "alice", Visibility: "public", User: &model.User{ID: "alice", Username: "alice"}}
	}

	ctx := context.Background()
	mk := func(notifier, noteID, reaction string) {
		t.Helper()
		_, err := svc.Create(ctx, notification.CreateInput{
			NotifieeID: "alice", NotifierID: notifier, Type: notification.TypeReaction,
			NoteID: noteID, Reaction: reaction,
		})
		require.NoError(t, err)
	}
	mk("carol", "n1", "👍")
	mk("bob", "n2", "😭") // mute 済 + 別 note
	mk("dave", "n1", "🎉")

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{"limit":50}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)

	resp := decodeGrouped(t, rec.Body.Bytes())
	require.Len(t, resp, 2, "別 note への reaction は区切りになる")
}

// #2739: **grouping 条件に当たる**通知が drop された場合は、区切りにせず
// グループから除外するだけにすること。
//
// upstream はここで畳んだうえで pack を通すが、`reaction:grouped` が notifierId を
// 持たないので mute した相手の reaction が reactions[] に残る。mk-go はグループを
// 作ったあとで中身から外すので漏らさない。**upstream に揃えてはいけない側。**
func TestGrouped_DroppedGroupableIsExcludedNotSeparator(t *testing.T) {
	h, svc, userRepo, noteRepo := mutedGroupedHandler(t, "bob")
	for _, id := range []string{"bob", "carol", "dave"} {
		userRepo.Users[id] = &model.User{ID: id, Username: id}
	}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice", Visibility: "public", User: &model.User{ID: "alice", Username: "alice"}}

	ctx := context.Background()
	for _, tc := range []struct{ notifier, reaction string }{
		{"carol", "👍"}, {"bob", "😭"}, {"dave", "🎉"},
	} {
		_, err := svc.Create(ctx, notification.CreateInput{
			NotifieeID: "alice", NotifierID: tc.notifier, Type: notification.TypeReaction,
			NoteID: "n1", Reaction: tc.reaction,
		})
		require.NoError(t, err)
	}

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{"limit":50}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)

	resp := decodeGrouped(t, rec.Body.Bytes())
	require.Len(t, resp, 1, "同一 note への reaction は区切りにしない")
	assert.Equal(t, "reaction:grouped", resp[0]["type"])
	reactions, ok := resp[0]["reactions"].([]any)
	require.True(t, ok)
	require.Len(t, reactions, 2, "mute 済の reaction はグループから外す (upstream は残す)")
	for _, r := range reactions {
		entry := r.(map[string]any)
		user, _ := entry["user"].(map[string]any)
		require.NotNil(t, user)
		assert.NotEqual(t, "bob", user["id"], "mute 済 notifier が reactions[] に残らない")
	}
}

// #2739: グループの生き残りが 1 件だけになったら単体 shape で返す。
//
// **これは upstream との意図的な差。** upstream の #packInternal が grouped 行を
// null にするのは要素 0 件のときだけなので、pack 段でメンバーが落ちて 1 件に
// なったグループはそのまま `reaction:grouped` で返る。
func TestGrouped_GroupWithSingleSurvivorIsUngrouped(t *testing.T) {
	h, svc, userRepo, noteRepo := mutedGroupedHandler(t, "bob")
	for _, id := range []string{"bob", "carol"} {
		userRepo.Users[id] = &model.User{ID: id, Username: id}
	}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice", Visibility: "public", User: &model.User{ID: "alice", Username: "alice"}}

	ctx := context.Background()
	for _, tc := range []struct{ notifier, reaction string }{{"carol", "👍"}, {"bob", "😭"}} {
		_, err := svc.Create(ctx, notification.CreateInput{
			NotifieeID: "alice", NotifierID: tc.notifier, Type: notification.TypeReaction,
			NoteID: "n1", Reaction: tc.reaction,
		})
		require.NoError(t, err)
	}

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{"limit":50}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)

	resp := decodeGrouped(t, rec.Body.Bytes())
	require.Len(t, resp, 1)
	assert.Equal(t, "reaction", resp[0]["type"], "1 件だけ残ったら単体 shape")
	assert.Equal(t, "carol", resp[0]["userId"])
}

// #2739: drop の理由ごとに、区切りとして働く / グループから外れるの両方を固定する。
// 理由によって drop する位置が違うと乖離が残るため、mute 以外も同じ形で見る。
func TestGrouped_DroppedByEachReasonActsAsSeparator(t *testing.T) {
	// setup は「carol の reaction → 対象の通知 → dave の reaction (同一 note)」を
	// 作り、対象が drop されることを前提に 2 行になることを見る。
	cases := []struct {
		name  string
		setup func(t *testing.T, h *Handler, svc *notification.Service, userRepo *testutil.MockUserRepository)
	}{
		{
			name: "suspended notifier",
			setup: func(t *testing.T, h *Handler, svc *notification.Service, userRepo *testutil.MockUserRepository) {
				userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", IsSuspended: true}
				_, err := svc.Create(context.Background(), notification.CreateInput{
					NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
				})
				require.NoError(t, err)
			},
		},
		{
			name: "unresolved notifier",
			setup: func(t *testing.T, h *Handler, svc *notification.Service, userRepo *testutil.MockUserRepository) {
				// userRepo に bob を入れない = notifier が解決できない。
				_, err := svc.Create(context.Background(), notification.CreateInput{
					NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
				})
				require.NoError(t, err)
			},
		},
		{
			name: "resolved follow request",
			setup: func(t *testing.T, h *Handler, svc *notification.Service, userRepo *testutil.MockUserRepository) {
				userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
				// followReqRepo は空 = 対応する follow_request が既に無い。
				h.SetFollowRequestRepo(testutil.NewMockFollowRequestRepository())
				_, err := svc.Create(context.Background(), notification.CreateInput{
					NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeReceiveFollowReq,
				})
				require.NoError(t, err)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, svc, userRepo, noteRepo := groupedHandler(t)
			userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol"}
			userRepo.Users["dave"] = &model.User{ID: "dave", Username: "dave"}
			noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice", Visibility: "public", User: &model.User{ID: "alice", Username: "alice"}}

			ctx := context.Background()
			_, err := svc.Create(ctx, notification.CreateInput{
				NotifieeID: "alice", NotifierID: "carol", Type: notification.TypeReaction, NoteID: "n1", Reaction: "👍",
			})
			require.NoError(t, err)
			tc.setup(t, h, svc, userRepo)
			_, err = svc.Create(ctx, notification.CreateInput{
				NotifieeID: "alice", NotifierID: "dave", Type: notification.TypeReaction, NoteID: "n1", Reaction: "🎉",
			})
			require.NoError(t, err)

			c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{"limit":50}`)
			setAuth(c, &model.User{ID: "alice"})
			require.NoError(t, h.Grouped(c))
			require.Equal(t, http.StatusOK, rec.Code)

			resp := decodeGrouped(t, rec.Body.Bytes())
			require.Len(t, resp, 2, "drop された通知が区切りとして働く")
		})
	}
}

// #2739: 同じ理由で drop された通知が grouping 条件に**当たる**場合は、区切りに
// せずグループから外すだけ。
func TestGrouped_DroppedByEachReasonIsExcludedWhenGroupable(t *testing.T) {
	cases := []struct {
		name     string
		notifier string
		setup    func(t *testing.T, h *Handler, userRepo *testutil.MockUserRepository)
	}{
		{
			name:     "suspended notifier",
			notifier: "bob",
			setup: func(t *testing.T, h *Handler, userRepo *testutil.MockUserRepository) {
				userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", IsSuspended: true}
			},
		},
		{
			name:     "unresolved notifier",
			notifier: "ghost",
			setup:    func(t *testing.T, h *Handler, userRepo *testutil.MockUserRepository) {},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, svc, userRepo, noteRepo := groupedHandler(t)
			userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol"}
			userRepo.Users["dave"] = &model.User{ID: "dave", Username: "dave"}
			noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice", Visibility: "public", User: &model.User{ID: "alice", Username: "alice"}}
			tc.setup(t, h, userRepo)

			ctx := context.Background()
			for _, n := range []struct{ notifier, reaction string }{
				{"carol", "👍"}, {tc.notifier, "😭"}, {"dave", "🎉"},
			} {
				_, err := svc.Create(ctx, notification.CreateInput{
					NotifieeID: "alice", NotifierID: n.notifier, Type: notification.TypeReaction,
					NoteID: "n1", Reaction: n.reaction,
				})
				require.NoError(t, err)
			}

			c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{"limit":50}`)
			setAuth(c, &model.User{ID: "alice"})
			require.NoError(t, h.Grouped(c))
			require.Equal(t, http.StatusOK, rec.Code)

			resp := decodeGrouped(t, rec.Body.Bytes())
			require.Len(t, resp, 1, "同一 note への reaction は区切りにしない")
			reactions, _ := resp[0]["reactions"].([]any)
			require.Len(t, reactions, 2, "drop された reaction はグループから外す")
		})
	}
}

// #2739: Show は drop 済みの行を返さないまま (grouped 側の変更が漏れないこと)。
func TestShow_StillDropsFilteredRows(t *testing.T) {
	h, svc, userRepo, noteRepo := mutedGroupedHandler(t, "bob")
	for _, id := range []string{"bob", "carol"} {
		userRepo.Users[id] = &model.User{ID: id, Username: id}
	}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice", Visibility: "public", User: &model.User{ID: "alice", Username: "alice"}}

	ctx := context.Background()
	for _, n := range []struct{ notifier, reaction string }{{"carol", "👍"}, {"bob", "😭"}} {
		_, err := svc.Create(ctx, notification.CreateInput{
			NotifieeID: "alice", NotifierID: n.notifier, Type: notification.TypeReaction,
			NoteID: "n1", Reaction: n.reaction,
		})
		require.NoError(t, err)
	}

	c, rec := newJSONRequest(t, "/api/i/notifications", `{"limit":50}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1, "mute 済の行は Show では出さない")
	assert.Equal(t, "carol", resp[0]["userId"])
}

// #2739: グループの id は**生き残ったメンバー**から採ること。
// (createdAt 側は TestGroupNotifications_GroupCreatedAtSkipsDropped が固定する —
// endpoint 経由だと 2 件の通知が同一ミリ秒になりうるので判別できない。)
// upstream は raw 列基準 (id は最後のメンバー、reaction の createdAt は先頭) で
// drop の有無を見ないが、mk-go は drop 済みを中身から外すので、そのまま使うと
// **落とした通知の id / 時刻が漏れる** (id は aidx で生成時刻を含む)。
func TestGrouped_GroupIDComesFromSurvivors(t *testing.T) {
	h, svc, userRepo, noteRepo := mutedGroupedHandler(t, "bob")
	for _, id := range []string{"bob", "carol", "dave"} {
		userRepo.Users[id] = &model.User{ID: id, Username: id}
	}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice", Visibility: "public", User: &model.User{ID: "alice", Username: "alice"}}

	ctx := context.Background()
	created := map[string]*notification.Notification{}
	for _, n := range []struct{ notifier, reaction string }{
		{"carol", "👍"}, {"dave", "🎉"}, {"bob", "😭"},
	} {
		got, err := svc.Create(ctx, notification.CreateInput{
			NotifieeID: "alice", NotifierID: n.notifier, Type: notification.TypeReaction,
			NoteID: "n1", Reaction: n.reaction,
		})
		require.NoError(t, err)
		created[n.notifier] = got
	}

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{"limit":50}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)

	resp := decodeGrouped(t, rec.Body.Bytes())
	require.Len(t, resp, 1)
	assert.Equal(t, "reaction:grouped", resp[0]["type"])
	// 一覧は既定で降順なので、並びは bob (drop) → dave → carol。生き残りの
	// 最後は carol。
	assert.Equal(t, created["carol"].ID, resp[0]["id"], "id は最後の生存メンバー")
	assert.NotEqual(t, created["bob"].ID, resp[0]["id"], "drop された行の id を返さない")
}

// #2739: renote も reaction と同じ規則で扱われること。
//
// **renote だけが非対称に壊れやすい。** reaction grouping は通知が持つ NoteID
// だけで判定できるが、renote grouping は target note を noteByID から引くので、
// drop された行の note を引いていないと `renoteTargetID` が "" を返して
// 「grouping 条件に当たるのに区切りになる」壊れ方をする。
func TestGrouped_DroppedRenoteIsExcludedNotSeparator(t *testing.T) {
	h, svc, userRepo, noteRepo := mutedGroupedHandler(t, "bob")
	for _, id := range []string{"bob", "carol", "dave"} {
		userRepo.Users[id] = &model.User{ID: id, Username: id}
	}
	// 3 つの renote note は別 ID だが同じ target を renote する。
	for _, rn := range []struct{ id, user string }{{"rn1", "carol"}, {"rn2", "bob"}, {"rn3", "dave"}} {
		noteRepo.Notes[rn.id] = &model.Note{
			ID: rn.id, UserID: rn.user, Visibility: "public",
			RenoteID: strPtr("target"), User: &model.User{ID: rn.user, Username: rn.user},
		}
	}

	ctx := context.Background()
	created := map[string]*notification.Notification{}
	for _, rn := range []struct{ id, user string }{{"rn1", "carol"}, {"rn2", "bob"}, {"rn3", "dave"}} {
		got, err := svc.Create(ctx, notification.CreateInput{
			NotifieeID: "alice", NotifierID: rn.user, Type: notification.TypeRenote, NoteID: rn.id,
		})
		require.NoError(t, err)
		created[rn.user] = got
	}

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{"limit":50}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)

	resp := decodeGrouped(t, rec.Body.Bytes())
	require.Len(t, resp, 1, "同一 target への renote は区切りにしない")
	assert.Equal(t, "renote:grouped", resp[0]["type"])
	assert.Equal(t, []string{"dave", "carol"}, groupedUserIDs(t, resp[0]),
		"mute 済の bob はグループから外れる (upstream は残す)")
	// id は最後の生存メンバー。降順なので dave → carol の順で、最後は carol。
	assert.Equal(t, created["carol"].ID, resp[0]["id"], "id は最後の生存メンバー")
	assert.NotEqual(t, created["bob"].ID, resp[0]["id"], "drop された行の id を返さない")
}

// #2739: renote でも、grouping 条件に当たらない通知 (別 target) は区切りになる。
func TestGrouped_DroppedRenoteOnOtherTargetActsAsSeparator(t *testing.T) {
	h, svc, userRepo, noteRepo := mutedGroupedHandler(t, "bob")
	for _, id := range []string{"bob", "carol", "dave"} {
		userRepo.Users[id] = &model.User{ID: id, Username: id}
	}
	for _, rn := range []struct{ id, user, target string }{
		{"rn1", "carol", "target"}, {"rn2", "bob", "other"}, {"rn3", "dave", "target"},
	} {
		noteRepo.Notes[rn.id] = &model.Note{
			ID: rn.id, UserID: rn.user, Visibility: "public",
			RenoteID: strPtr(rn.target), User: &model.User{ID: rn.user, Username: rn.user},
		}
	}

	ctx := context.Background()
	for _, rn := range []struct{ id, user string }{{"rn1", "carol"}, {"rn2", "bob"}, {"rn3", "dave"}} {
		_, err := svc.Create(ctx, notification.CreateInput{
			NotifieeID: "alice", NotifierID: rn.user, Type: notification.TypeRenote, NoteID: rn.id,
		})
		require.NoError(t, err)
	}

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{"limit":50}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)

	resp := decodeGrouped(t, rec.Body.Bytes())
	require.Len(t, resp, 2, "別 target への renote は区切りになる")
}

// #2739: グループの createdAt も生存メンバー基準であること。
//
// endpoint 経由だと通知の createdAt が同一ミリ秒になりうるので、
// groupNotifications を直接叩いて別値に固定する。
func TestGroupNotifications_GroupCreatedAtSkipsDropped(t *testing.T) {
	// 降順ページ: n3 (drop) → n2 → n1。生存の先頭は n2。
	rows := []*notification.Notification{
		{ID: "n3", Type: notification.TypeReaction, NotifierID: "bob", NoteID: "note1", Reaction: "😭"},
		{ID: "n2", Type: notification.TypeReaction, NotifierID: "dave", NoteID: "note1", Reaction: "🎉"},
		{ID: "n1", Type: notification.TypeReaction, NotifierID: "carol", NoteID: "note1", Reaction: "👍"},
	}
	// packed に n3 が無い = drop 済み。
	packed := []map[string]any{
		{"id": "n2", "type": "reaction", "noteId": "note1", "reaction": "🎉", "createdAt": "2026-01-02T03:04:06.000Z", "user": map[string]any{"id": "dave"}},
		{"id": "n1", "type": "reaction", "noteId": "note1", "reaction": "👍", "createdAt": "2026-01-02T03:04:05.000Z", "user": map[string]any{"id": "carol"}},
	}

	out := groupNotifications(rows, packed, nil)
	require.Len(t, out, 1)
	assert.Equal(t, "reaction:grouped", out[0]["type"])
	assert.Equal(t, "2026-01-02T03:04:06.000Z", out[0]["createdAt"],
		"createdAt は先頭の生存メンバー (drop された n3 の時刻を出さない)")
	assert.Equal(t, "n1", out[0]["id"], "id は最後の生存メンバー")
}

// 取得が 0 件の fetch では既読化しない (#2833)。upstream
// i/notifications-grouped.ts の `notifications.length === 0` 早期 return と同じ。
//
// **既読位置が飛ぶのが問題。** MarkAllAsRead が進める先は fetch が返した行では
// なく**ストリームの最新エントリ**なので、0 件の fetch で呼ぶと、ユーザーが一度も
// 受け取っていない通知まで既読になる。
//
// こちらは `includeTypes:[]` = Grouped の emptyByTypeFilter が
// collectNotificationsWithDropped を呼ぶ**前**に早期 return する経路。
// **svc.List に到達する経路は別に要る** —
// この 1 本だけだと「includeTypes が空かどうか」だけを見る実装で全テストが通り、
// untilId 末尾ページングや excludeTypes 全指定の回帰を検出できない
// (TestGrouped_EmptyPageDoesNotMarkAsRead)。
func TestGrouped_IncludeTypesEmptyDoesNotMarkAsRead(t *testing.T) {
	h, svc, _, _ := groupedHandler(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	// includeTypes: [] は「何も含めない」なので取得は 0 件になる。ストリームには
	// 通知が積まれたままなので、既読化すると位置が最新まで飛ぶ。
	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{"includeTypes":[]}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, decodeGrouped(t, rec.Body.Bytes()))

	assert.NotContains(t, pub.types("alice"), "readAllNotifications",
		"empty fetch must not publish readAllNotifications")
	readID, err := svc.LatestReadID(ctx, "alice")
	require.NoError(t, err)
	assert.Empty(t, readID, "empty fetch must not advance the read marker")
}

// 非空の fetch では従来どおり既読化する。上の早期 return が広すぎないことの
// 裏返しで、これが無いと「常に既読化しない」変異が素通りする。
func TestGrouped_NonEmptyResultMarksAsRead(t *testing.T) {
	h, svc, userRepo, _ := groupedHandler(t)
	// notifier を解決できないと collectNotificationsWithDropped の
	// unresolved-notifier drop (#2106 N6) で行が落ち、結果が空になって
	// 「非空の fetch」を試したことにならない。
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotEmpty(t, decodeGrouped(t, rec.Body.Bytes()))

	assert.Contains(t, pub.types("alice"), "readAllNotifications",
		"non-empty fetch keeps the implicit mark-as-read")
	readID, err := svc.LatestReadID(ctx, "alice")
	require.NoError(t, err)
	assert.NotEmpty(t, readID, "non-empty fetch advances the read marker")
}

// 判定は `all` (svc.List の生の結果) であって `grouped` ではない (#2833)。
//
// notifier を解決できない通知は collectNotificationsWithDropped の
// unresolved-notifier drop (#2106 N6) で落ちるので、取得は 1 件でも応答は空になる
// (落とすのは pack ではなくその前段)。upstream の早期 return は `getNotifications`
// の直後にあり、この drop よりさらに前なので、**この場合は既読化する**。
// `grouped` で判定すると upstream より後ろの位置になり、逆向きの乖離を作る。
func TestGrouped_MarksAsReadWhenRowsDropButFetchWasNonEmpty(t *testing.T) {
	h, svc, _, _ := groupedHandler(t)
	ctx := context.Background()
	// notifier "bob" を userRepo に入れない → unresolved-notifier drop で行が落ちる。
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", `{}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, decodeGrouped(t, rec.Body.Bytes()), "pack drops the unresolved notifier row")

	assert.Contains(t, pub.types("alice"), "readAllNotifications",
		"upstream marks as read here: the early return is before pack, not after")
}

// **svc.List に到達する 0 件 fetch** でも既読化しない (#2833)。
//
// これが本命のケースで、実運用で最も踏むのは末尾までページングした
// `untilId` (cursor は exclusive なので最古通知の id を渡すと 0 行になる)。
// ストリームには通知が残っているので、既読化すると位置が最新まで飛ぶ。
//
// **includeTypes:[] の 1 本だけでは守れない。** あちらは svc.List を呼ぶ前に
// 早期 return する経路なので、guard を「includeTypes が空か」で書いた実装でも
// 全テストが通ってしまう (実測)。
func TestGrouped_EmptyPageDoesNotMarkAsRead(t *testing.T) {
	h, svc, userRepo, _ := groupedHandler(t)
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	ctx := context.Background()
	oldest, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	// untilId は exclusive。最古の通知自身を渡すと、それより古い行は無いので 0 件。
	c, rec := newJSONRequest(t, "/api/i/notifications-grouped",
		`{"untilId":"`+oldest.ID+`"}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, decodeGrouped(t, rec.Body.Bytes()))

	assert.NotContains(t, pub.types("alice"), "readAllNotifications",
		"an empty page must not publish readAllNotifications")
	readID, err := svc.LatestReadID(ctx, "alice")
	require.NoError(t, err)
	assert.Empty(t, readID, "an empty page must not advance the read marker")
}

// excludeTypes が実在する type を全て覆っても同じ (#2833)。こちらも svc.List に
// 到達する経路で、in-app の type filter で全行が落ちて 0 件になる形
// (mk-go の svc.List に refetch loop は無く、MaxPerUser=300 を 1 回取って絞る)。
func TestGrouped_ExcludeAllTypesDoesNotMarkAsRead(t *testing.T) {
	h, svc, userRepo, _ := groupedHandler(t)
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped",
		`{"excludeTypes":["follow"]}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, decodeGrouped(t, rec.Body.Bytes()))

	assert.NotContains(t, pub.types("alice"), "readAllNotifications",
		"excluding every present type must not publish readAllNotifications")
	readID, err := svc.LatestReadID(ctx, "alice")
	require.NoError(t, err)
	assert.Empty(t, readID, "excluding every present type must not advance the read marker")
}

// excludeTypes 全指定でも、**notificationTypeList に無い type の行は生き残る**
// (#2835)。svc.List の exclude filter は `excludeSet[n.Type]` の一致しか見ない
// ので、pollVote (notificationTypeList の外にある唯一の type。producer は #690 で
// 無効化済みだが、それ以前の行は残りうる) が 1 件あるだけで `len(all) > 0` が
// 成立し、#2833 の guard をすり抜けて既読化まで走る。upstream は早期 return
// するので `[]` が正。
//
// **この 1 本が Grouped 側の早期 return を守る唯一のテスト。** これが無いと
// 早期 return を丸ごと消す変異が全テストを素通りする (実測)。
func TestGrouped_ExcludeAllTypesStopsNonEnumRows(t *testing.T) {
	h, svc, userRepo, noteRepo := groupedHandler(t)
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice", Visibility: "public",
		User: &model.User{ID: "alice", Username: "alice"}}
	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob",
		Type: notification.TypePollVote, NoteID: "n1",
	})
	require.NoError(t, err)

	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	body, err := json.Marshal(map[string]any{"excludeTypes": notificationTypeList})
	require.NoError(t, err)
	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", string(body))
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)
	// wire 上 `[]` であること。null は misskey-js / misskey_dart が non-null で
	// 受けるので通知ページごと落ちる。
	assert.Equal(t, "[]", strings.TrimSpace(rec.Body.String()))

	assert.NotContains(t, pub.types("alice"), "readAllNotifications")
	readID, err := svc.LatestReadID(ctx, "alice")
	require.NoError(t, err)
	assert.Empty(t, readID, "non-enum rows must not sneak past the early return")
}

// Grouped も Show と同じく obsolete を除去する (#2837)。除去後に空になった
// includeTypes は filter として効かないので全件が返る。
func TestGrouped_IncludeTypesObsoleteOnlyReturnsAll(t *testing.T) {
	h, svc, userRepo, _ := groupedHandler(t)
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)

	c, rec := newJSONRequest(t, "/api/i/notifications-grouped",
		`{"includeTypes":["pollVote"]}`)
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)
	resp := decodeGrouped(t, rec.Body.Bytes())
	require.Len(t, resp, 1, "obsolete-only includeTypes means no filter")
	assert.Equal(t, "follow", resp[0]["type"])
}
