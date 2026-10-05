package entity

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestPackNotification_NilInput(t *testing.T) {
	assert.Nil(t, PackNotification(nil, nil, nil, nil, nil, nil))
}

func TestPackNotification_FollowType_WithUser(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	nid := idGen.Generate(time.Now())
	n := &notification.Notification{
		ID:         nid,
		CreatedAt:  time.Date(2026, 4, 19, 12, 0, 0, 0, time.UTC),
		Type:       notification.TypeFollow,
		NotifierID: "u_bob",
	}
	user := &model.User{ID: "u_bob", Username: "bob", UsernameLower: "bob"}
	out := PackNotification(n, user, nil, idGen, nil, nil)
	assert.Equal(t, nid, out["id"])
	assert.Equal(t, "2026-04-19T12:00:00.000Z", out["createdAt"])
	assert.Equal(t, "follow", out["type"])
	// userId はnotifierIdから取る (TS互換)
	assert.Equal(t, "u_bob", out["userId"])
	// user は packed UserLite
	u, ok := out["user"].(UserLite)
	assert.True(t, ok)
	assert.Equal(t, "bob", u.Username)
	// note はfollowタイプでは無い
	_, hasNote := out["note"]
	assert.False(t, hasNote)
}

func TestPackNotification_ReactionType_WithNote(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	nid := idGen.Generate(time.Now())
	noteID := idGen.Generate(time.Now())
	n := &notification.Notification{
		ID:         nid,
		CreatedAt:  time.Now(),
		Type:       notification.TypeReaction,
		NotifierID: "u_alice",
		NoteID:     noteID,
		Reaction:   ":smile:",
	}
	user := &model.User{ID: "u_alice", Username: "alice"}
	note := &model.Note{ID: noteID, UserID: "u_me", Text: ptrStr("hi")}
	out := PackNotification(n, user, note, idGen, nil, nil)
	assert.Equal(t, "reaction", out["type"])
	assert.Equal(t, "u_alice", out["userId"])
	assert.Equal(t, ":smile:", out["reaction"])
	// note は packed で返る
	_, hasNote := out["note"]
	assert.True(t, hasNote)
}

func TestPackNotification_PollVote_WithChoice(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	choice := 2
	n := &notification.Notification{
		ID:         "x",
		CreatedAt:  time.Now(),
		Type:       notification.TypePollVote,
		NotifierID: "u_voter",
		NoteID:     "n1",
		Choice:     &choice,
	}
	out := PackNotification(n, nil, nil, idGen, nil, nil)
	assert.Equal(t, 2, out["choice"])
	// user / note は nil なので含まれない
	_, hasUser := out["user"]
	assert.False(t, hasUser)
	_, hasNote := out["note"]
	assert.False(t, hasNote)
}

func TestPackNotification_PopulatesInstanceOnUser(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	remoteHost := "remote.example"
	themeColor := "#cafe00"
	instLookup := &stubInstanceLookup{data: map[string]*model.Instance{
		remoteHost: {Host: remoteHost, ThemeColor: &themeColor},
	}}

	n := &notification.Notification{
		ID:         "x",
		CreatedAt:  time.Now(),
		Type:       notification.TypeRenote,
		NotifierID: "u_remote",
	}
	user := &model.User{ID: "u_remote", Username: "remote", Host: &remoteHost}

	out := PackNotification(n, user, nil, idGen, instLookup, nil)

	u, ok := out["user"].(UserLite)
	require.True(t, ok)
	require.NotNil(t, u.Instance, "user.instance must be set for remote notifier")
	assert.Equal(t, themeColor, *u.Instance.ThemeColor)
}

func TestPackNotification_ExtraFields_Merged(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	n := &notification.Notification{
		ID:        "x",
		CreatedAt: time.Now(),
		Type:      notification.TypeExportCompleted,
		Extra:     map[string]any{"fileId": "df_1"},
	}
	out := PackNotification(n, nil, nil, idGen, nil, nil)
	assert.Equal(t, "df_1", out["fileId"])
}

// #1249 以前に永続化された exportCompleted 通知は exportedEntity に mk-go 内部値
// (plural / kebab) を持つ。misskey_dart の enum decode を落とさないよう read 時に
// singular / camelCase へ正規化する (#1391)。
func TestPackNotification_NormalizesExportedEntity(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	cases := map[string]string{
		"notes":         "note",
		"favorites":     "favorite",
		"user-lists":    "userList",
		"antennas":      "antenna",
		"clips":         "clip",
		"mute":          "muting",
		"custom-emojis": "customEmoji",
		// already-canonical / unknown はそのまま。
		"note":      "note",
		"following": "following",
		"blocking":  "blocking",
		"weird":     "weird",
	}
	for stored, want := range cases {
		n := &notification.Notification{
			ID:        "x",
			CreatedAt: time.Now(),
			Type:      notification.TypeExportCompleted,
			Extra:     map[string]any{"exportedEntity": stored, "fileId": "df_1"},
		}
		out := PackNotification(n, nil, nil, idGen, nil, nil)
		assert.Equal(t, want, out["exportedEntity"], "stored=%q", stored)
		assert.Equal(t, "df_1", out["fileId"], "stored=%q: other extra fields preserved", stored)
	}
}

func ptrStr(s string) *string { return &s }

// PackNotifications は N 件の通知を pack しても InstanceLookup を 1 回 /
// EmojiLookup は host 数分しか叩かないことを確認する (#428 N+1 解消)。
// 単品 PackNotification を loop で呼ぶと 1 件あたり 2 + 2 (note 用 +
// user 用) クエリ発生するため、20 件で 80 クエリにふくらむ前提を防ぐ。
func TestPackNotifications_BatchesResolvers(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	hostA := "a.example"
	hostB := "b.example"
	themeColor := "#cafe00"
	instLookup := &stubInstanceLookup{data: map[string]*model.Instance{
		hostA: {Host: hostA, ThemeColor: &themeColor},
		hostB: {Host: hostB, ThemeColor: &themeColor},
	}}
	emojiLookup := &stubEmojiLookup{data: map[string][]*model.Emoji{}}

	// 3 件の通知: 同じ host を含む / 別 host / local の混在で host 集約も確認。
	mkItem := func(id string, host *string) NotificationItem {
		u := &model.User{ID: "u" + id, Username: "u" + id, UsernameLower: "u" + id, Host: host}
		return NotificationItem{
			N: &notification.Notification{
				ID: id, CreatedAt: time.Now(), Type: notification.TypeFollow, NotifierID: "u" + id,
			},
			User: u,
		}
	}
	items := []NotificationItem{
		mkItem("n1", &hostA),
		mkItem("n2", &hostA),
		mkItem("n3", &hostB),
	}

	out := PackNotifications(items, idGen, instLookup, emojiLookup)
	require.Len(t, out, 3)

	assert.Len(t, instLookup.calls, 1, "InstanceResolver は 1 batch fetch のみ")
	// host は a/b の 2 つ uniq 化されている。
	assert.ElementsMatch(t, []string{hostA, hostB}, instLookup.calls[0])

	// remote user の Instance が乗っていることだけ抜き取り検証。
	for _, item := range out {
		u, ok := item["user"].(UserLite)
		require.True(t, ok)
		require.NotNil(t, u.Instance)
		assert.Equal(t, themeColor, *u.Instance.ThemeColor)
	}
}

// item 内で N == nil な要素は skip され、残りは入力順序を保つことを確認。
func TestPackNotifications_SkipsNilNotifications(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	items := []NotificationItem{
		{N: &notification.Notification{ID: "x1", CreatedAt: time.Now(), Type: notification.TypeFollow}},
		{N: nil},
		{N: &notification.Notification{ID: "x2", CreatedAt: time.Now(), Type: notification.TypeFollow}},
	}
	out := PackNotifications(items, idGen, nil, nil)
	require.Len(t, out, 2)
	assert.Equal(t, "x1", out[0]["id"])
	assert.Equal(t, "x2", out[1]["id"])
}

// note を持つ通知も 1 batch で resolve されることを確認 (note.user の Instance
// と top-level notifier の Instance が同じ host でも fetch は 1 回)。
func TestPackNotifications_NoteAndUserShareInstanceFetch(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	host := "remote.example"
	themeColor := "#abcdef"
	instLookup := &stubInstanceLookup{data: map[string]*model.Instance{
		host: {Host: host, ThemeColor: &themeColor},
	}}

	notifier := &model.User{ID: "u_a", Username: "a", UsernameLower: "a", Host: &host}
	noteAuthor := &model.User{ID: "u_b", Username: "b", UsernameLower: "b", Host: &host}
	note := &model.Note{ID: "n_1", UserID: "u_b", User: noteAuthor, Text: ptrStr("hi")}

	n := &notification.Notification{
		ID: "x", CreatedAt: time.Now(), Type: notification.TypeReply,
		NotifierID: "u_a", NoteID: "n_1",
	}

	out := PackNotifications([]NotificationItem{{N: n, User: notifier, Note: note}}, idGen, instLookup, nil)
	require.Len(t, out, 1)
	assert.Len(t, instLookup.calls, 1, "host 重複は uniq 化されて 1 回の fetch")
	assert.ElementsMatch(t, []string{host}, instLookup.calls[0])
}

// #1559 roleAssigned: lookup が role を返せば packed role を embed し roleId は出さない。
func TestPackNotification_RoleAssigned_PacksRole(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	n := &notification.Notification{
		ID:        idGen.Generate(time.Now()),
		CreatedAt: time.Now(),
		Type:      notification.TypeRoleAssigned,
		Extra:     map[string]any{"roleId": "r1"},
	}
	lookup := func(roleID string) (map[string]any, bool) {
		assert.Equal(t, "r1", roleID)
		return map[string]any{"id": "r1", "name": "VIP"}, true
	}
	out := PackNotification(n, nil, nil, idGen, nil, nil, WithRoleLookup(lookup))
	require.NotNil(t, out)
	assert.Equal(t, "roleAssigned", out["type"])
	role, ok := out["role"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "VIP", role["name"])
	_, hasRoleID := out["roleId"]
	assert.False(t, hasRoleID, "roleId は packed role に解決済なので surface しない")
}

// role 削除済 (lookup が false) なら通知ごと drop する。
func TestPackNotification_RoleAssigned_DroppedWhenRoleDeleted(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	n := &notification.Notification{
		ID:        idGen.Generate(time.Now()),
		CreatedAt: time.Now(),
		Type:      notification.TypeRoleAssigned,
		Extra:     map[string]any{"roleId": "gone"},
	}
	lookup := func(string) (map[string]any, bool) { return nil, false }
	assert.Nil(t, PackNotification(n, nil, nil, idGen, nil, nil, WithRoleLookup(lookup)))
}

// role lookup 未配線でも安全側に倒して通知を drop する。
func TestPackNotification_RoleAssigned_DroppedWhenLookupUnset(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	n := &notification.Notification{
		ID:        idGen.Generate(time.Now()),
		CreatedAt: time.Now(),
		Type:      notification.TypeRoleAssigned,
		Extra:     map[string]any{"roleId": "r1"},
	}
	assert.Nil(t, PackNotification(n, nil, nil, idGen, nil, nil))
}

// #1559 chatRoomInvitationReceived: lookup が invitation を返せば packed
// invitation を embed し invitationId は出さない。
func TestPackNotification_ChatRoomInvitation_PacksInvitation(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	n := &notification.Notification{
		ID:         idGen.Generate(time.Now()),
		CreatedAt:  time.Now(),
		Type:       notification.TypeChatRoomInvitationReceived,
		NotifierID: "u_inviter",
		Extra:      map[string]any{"invitationId": "inv1"},
	}
	inviter := &model.User{ID: "u_inviter", Username: "inviter"}
	lookup := func(invID, viewerID string) (map[string]any, bool) {
		assert.Equal(t, "inv1", invID)
		assert.Equal(t, "u_me", viewerID)
		return map[string]any{"id": invID, "roomId": "room1"}, true
	}
	out := PackNotification(n, inviter, nil, idGen, nil, nil, WithChatInvitationLookup(lookup), WithViewer("u_me"))
	require.NotNil(t, out)
	assert.Equal(t, "chatRoomInvitationReceived", out["type"])
	assert.Equal(t, "u_inviter", out["userId"], "notifier(招待者) は userId として出る")
	inv, ok := out["invitation"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "room1", inv["roomId"])
	_, hasInvID := out["invitationId"]
	assert.False(t, hasInvID, "invitationId は packed invitation に解決済なので surface しない")
}

// invitation 削除済 (lookup が false) なら通知ごと drop する。
func TestPackNotification_ChatRoomInvitation_DroppedWhenDeleted(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	n := &notification.Notification{
		ID:        idGen.Generate(time.Now()),
		CreatedAt: time.Now(),
		Type:      notification.TypeChatRoomInvitationReceived,
		Extra:     map[string]any{"invitationId": "gone"},
	}
	lookup := func(string, string) (map[string]any, bool) { return nil, false }
	assert.Nil(t, PackNotification(n, nil, nil, idGen, nil, nil, WithChatInvitationLookup(lookup)))
}

// lookup 未配線でも安全側に倒して通知を drop する。
func TestPackNotification_ChatRoomInvitation_DroppedWhenLookupUnset(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	n := &notification.Notification{
		ID:        idGen.Generate(time.Now()),
		CreatedAt: time.Now(),
		Type:      notification.TypeChatRoomInvitationReceived,
		Extra:     map[string]any{"invitationId": "inv1"},
	}
	assert.Nil(t, PackNotification(n, nil, nil, idGen, nil, nil))
}

// #2735: 通知の note embed に files が入ること。packNoteAtDepth は Files を空
// スライスで初期化するだけなので、resolver を渡さない限り添付メディアは返らない。
func TestPackNotification_ResolvesNoteFiles(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	files := &stubDriveFileLookup{files: map[string]*model.DriveFile{
		"f1": {ID: "f1", Name: "a.png", Type: "image/png", URL: "https://example.com/f1"},
	}}
	r := NewNoteFieldResolver(files, nil, nil, nil, nil, idGen)

	n := &notification.Notification{
		ID: idGen.Generate(time.Now()), CreatedAt: time.Now(),
		Type: notification.TypeMention, NotifierID: "u_bob", NoteID: "n1",
	}
	note := &model.Note{ID: "n1", UserID: "u_bob", FileIDs: model.StringArray{"f1"}}

	out := PackNotification(n, nil, note, idGen, nil, nil, WithNoteFieldResolver(r))
	packed, ok := out["note"].(NoteEntity)
	require.True(t, ok)
	require.Len(t, packed.Files, 1)
}

// resolver 未配線 (nil) でも panic せず、従来どおり files が空配列で返ること。
// pin しているのは外形的な契約で、guard は resolveNoteFields と
// NoteFieldResolver.Apply の nil receiver の 2 段どちらでも成立する。
func TestPackNotification_NilNoteFieldResolverIsNoOp(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	n := &notification.Notification{
		ID: idGen.Generate(time.Now()), CreatedAt: time.Now(),
		Type: notification.TypeMention, NotifierID: "u_bob", NoteID: "n1",
	}
	note := &model.Note{ID: "n1", UserID: "u_bob", FileIDs: model.StringArray{"f1"}}

	out := PackNotification(n, nil, note, idGen, nil, nil, WithNoteFieldResolver(nil))
	packed, ok := out["note"].(NoteEntity)
	require.True(t, ok)
	assert.Empty(t, packed.Files)
}

// ページ全体を 1 バッチで解決すること。1 通知ごとに Apply を呼ぶと drive_file
// lookup が件数分に増えるので、既存の Instance / Emoji resolver と同じく
// lookup 呼び出しは 1 回に畳む。
func TestPackNotifications_BatchesNoteFileLookup(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	files := &stubDriveFileLookup{files: map[string]*model.DriveFile{
		"f1": {ID: "f1", Name: "a.png", Type: "image/png", URL: "https://example.com/f1"},
		"f2": {ID: "f2", Name: "b.png", Type: "image/png", URL: "https://example.com/f2"},
	}}
	r := NewNoteFieldResolver(files, nil, nil, nil, nil, idGen)

	items := make([]NotificationItem, 0, 2)
	for i, spec := range []struct{ noteID, fileID string }{{"n1", "f1"}, {"n2", "f2"}} {
		items = append(items, NotificationItem{
			N: &notification.Notification{
				ID: idGen.Generate(time.Now().Add(time.Duration(i) * time.Second)), CreatedAt: time.Now(),
				Type: notification.TypeMention, NotifierID: "u_bob", NoteID: spec.noteID,
			},
			Note: &model.Note{ID: spec.noteID, UserID: "u_bob", FileIDs: model.StringArray{spec.fileID}},
		})
	}

	out := PackNotifications(items, idGen, nil, nil, WithNoteFieldResolver(r))
	require.Len(t, out, 2)
	// **どの通知行にどの note が戻ったか**まで見る。resolveNoteFields は値型 slice に
	// 抜き出して書き戻すので、対応付けを取り違えると通知 A の行に通知 B の note が
	// 載る。件数だけ見ると (どちらも 1 件なので) 取りこぼす。
	for i, want := range []string{"f1", "f2"} {
		packed, ok := out[i]["note"].(NoteEntity)
		require.True(t, ok)
		require.Len(t, packed.Files, 1)
		df, ok := packed.Files[0].(DriveFileEntity)
		require.True(t, ok)
		assert.Equal(t, want, df.ID, "out[%d] には対応する note が戻ること", i)
	}
	assert.Equal(t, 1, files.calls)
}

// note を持つ通知と持たない通知が混ざったページで、note が正しい行に戻ること。
// resolveNoteFields は note を持つ行だけを抜き出して idx で戻すので、その間接参照を
// 落とすと follow 通知の行に mention の note が生え、mention 側は未解決のままになる
// (全件 note ありのバッチだけを見ていると取りこぼす失敗モード)。
func TestPackNotifications_MixedNoteAndNoteLessRows(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	files := &stubDriveFileLookup{files: map[string]*model.DriveFile{
		"f1": {ID: "f1", Name: "a.png", Type: "image/png", URL: "https://example.com/f1"},
	}}
	r := NewNoteFieldResolver(files, nil, nil, nil, nil, idGen)

	base := time.Now()
	items := []NotificationItem{
		{N: &notification.Notification{
			ID: idGen.Generate(base), CreatedAt: base,
			Type: notification.TypeFollow, NotifierID: "u_bob",
		}},
		{
			N: &notification.Notification{
				ID: idGen.Generate(base.Add(time.Second)), CreatedAt: base,
				Type: notification.TypeMention, NotifierID: "u_bob", NoteID: "n1",
			},
			Note: &model.Note{ID: "n1", UserID: "u_bob", FileIDs: model.StringArray{"f1"}},
		},
	}

	out := PackNotifications(items, idGen, nil, nil, WithNoteFieldResolver(r))
	require.Len(t, out, 2)
	_, hasNote := out[0]["note"]
	assert.False(t, hasNote, "note を持たない通知の行に note が生えないこと")
	packed, ok := out[1]["note"].(NoteEntity)
	require.True(t, ok)
	require.Len(t, packed.Files, 1, "note を持つ側の files が解決されること")
}

// note を伴わない通知 (follow 等) しかない場合は drive_file lookup を呼ばない。
// pin しているのは外形的な契約で、guard は resolveNoteFields の `len(notes) == 0` と
// ResolveFiles の `len(allIDs) == 0` の 2 段どちらでも成立する。
func TestPackNotifications_NoNoteSkipsFileLookup(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	files := &stubDriveFileLookup{files: map[string]*model.DriveFile{}}
	r := NewNoteFieldResolver(files, nil, nil, nil, nil, idGen)

	out := PackNotifications([]NotificationItem{{
		N: &notification.Notification{
			ID: idGen.Generate(time.Now()), CreatedAt: time.Now(),
			Type: notification.TypeFollow, NotifierID: "u_bob",
		},
	}}, idGen, nil, nil, WithNoteFieldResolver(r))
	require.Len(t, out, 1)
	assert.Equal(t, 0, files.calls)
}

// #2735: viewer 依存 field も notifiee 視点で解決されること。viewer は
// WithViewer で渡した notifiee ID から組み立てる。
func TestPackNotification_ResolvesViewerFieldsForNotifiee(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	reactions := &stubNoteReactionLookup{rows: map[string]*model.NoteReaction{
		"n1": {NoteID: "n1", Reaction: "👍"},
	}}
	r := NewNoteFieldResolver(nil, nil, nil, reactions, nil, idGen)

	n := &notification.Notification{
		ID: idGen.Generate(time.Now()), CreatedAt: time.Now(),
		Type: notification.TypeReaction, NotifierID: "u_bob", NoteID: "n1", Reaction: "👍",
	}
	note := &model.Note{
		ID: "n1", UserID: "u_alice",
		Reactions: datatypes.JSON([]byte(`{"👍":1}`)),
	}

	out := PackNotification(n, nil, note, idGen, nil, nil,
		WithViewer("u_alice"), WithNoteFieldResolver(r))
	packed, ok := out["note"].(NoteEntity)
	require.True(t, ok)
	require.NotNil(t, packed.MyReaction)
	assert.Equal(t, "👍", *packed.MyReaction)
}

// WithViewer が空 (= notifiee 不明) なら viewer 依存 field は解決しない。
// 空 ID で note_reaction を引きに行かないことの guard。
func TestPackNotification_EmptyViewerSkipsViewerFields(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	reactions := &countingReactionLookup{rows: map[string]*model.NoteReaction{
		"n1": {NoteID: "n1", Reaction: "👍"},
	}}
	r := NewNoteFieldResolver(nil, nil, nil, reactions, nil, idGen)

	n := &notification.Notification{
		ID: idGen.Generate(time.Now()), CreatedAt: time.Now(),
		Type: notification.TypeReaction, NoteID: "n1",
	}
	note := &model.Note{ID: "n1", UserID: "u_alice", Reactions: datatypes.JSON([]byte(`{"👍":1}`))}

	out := PackNotification(n, nil, note, idGen, nil, nil, WithNoteFieldResolver(r))
	packed, ok := out["note"].(NoteEntity)
	require.True(t, ok)
	assert.Nil(t, packed.MyReaction)
	assert.Empty(t, reactions.queriedID, "viewer 不明のまま note_reaction を引かないこと")
}
