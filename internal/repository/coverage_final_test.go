package repository

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// 本ファイルは#260 repository coverage 100%到達のための最終追加。
// 主に各関数のsuccess path / limit-clamp / filter-branch を埋める。

// --- chat: FindMessageByURI success path ---

func TestChatRepository_FindMessageByURI_Success(t *testing.T) {
	r := NewChatRepository(testDB)
	u1 := insertTestUser(t, "fmu_u1", "fmu1")
	defer cleanupUser(t, u1.ID)
	u2 := insertTestUser(t, "fmu_u2", "fmu2")
	defer cleanupUser(t, u2.ID)

	uri := "https://remote.example/chat/fmu_1"
	msg := &model.ChatMessage{
		ID:         "fmu_m1",
		FromUserID: u1.ID,
		ToUserID:   &u2.ID,
		URI:        &uri,
		Text:       strPtrCC("hi"),
	}
	require.NoError(t, r.CreateMessage(msg))
	defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, msg.ID)

	found, err := r.FindMessageByURI(uri)
	require.NoError(t, err)
	assert.Equal(t, msg.ID, found.ID)
}

// --- chat: ListHistory success path ---

func TestChatRepository_ListHistory_Extras(t *testing.T) {
	r := NewChatRepository(testDB)
	// limit clamp: 0 -> default, >100 -> 100
	_, err := r.ListHistory("x", 0)
	require.NoError(t, err)
	_, err = r.ListHistory("x", 999)
	require.NoError(t, err)
}

// --- reversi: ListByUser limit clamp + ListActive ---
// FindByFederationIDのsuccess pathは現状の migration では reversi_game テーブルに
// "federationId" カラムが存在しないため (migration 未追加。関数・service側の
// コードは TS 互換のため準備済みだが schema が追従していない) テスト不能。
// error path は coverage_complete_test.go の cancelled context テストで踏む。

func TestReversiRepository_Extras(t *testing.T) {
	r := NewReversiRepository(testDB)
	u := insertTestUser(t, "rv_ex_u1", "rvex")
	defer cleanupUser(t, u.ID)

	// ListByUser: limit clamp (0 -> default)
	_, err := r.ListByUser(u.ID, 0)
	require.NoError(t, err)
	_, err = r.ListByUser(u.ID, 999)
	require.NoError(t, err)

	// ListActive
	_, err = r.ListActive()
	require.NoError(t, err)
}

// --- user_list: UpdateMembership RowsAffected==0 ---

func TestUserListRepository_UpdateMembership_NotFound(t *testing.T) {
	r := NewUserListRepository(testDB)
	wrTrue := true
	err := r.UpdateMembership("nonexistent_list", "nonexistent_user", &wrTrue)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

// #1948-15: withReplies=nil (= request 省略) は既存値を維持し、membership 存在のみ確認する。
func TestUserListRepository_UpdateMembership_NilPreservesAndExistence(t *testing.T) {
	r := NewUserListRepository(testDB)
	owner := insertTestUser(t, "ulnowner", "ulnilowner")
	defer cleanupUser(t, owner.ID)
	member := insertTestUser(t, "ulnmember", "ulnilmember")
	defer cleanupUser(t, member.ID)
	list := &model.UserList{ID: "ul_nil_l1", UserID: owner.ID, Name: "nil-test"}
	require.NoError(t, r.Create(list))
	defer testDB.Exec(`DELETE FROM "user_list" WHERE id = ?`, list.ID)
	mem := &model.UserListMembership{ID: "ul_nil_m1", UserListID: list.ID, UserID: member.ID, WithReplies: true}
	require.NoError(t, r.AddMember(mem))
	defer testDB.Exec(`DELETE FROM "user_list_membership" WHERE id = ?`, mem.ID)

	// nil + 存在: 既存 withReplies=true を維持 (no-op)。
	require.NoError(t, r.UpdateMembership(list.ID, member.ID, nil))
	var got model.UserListMembership
	require.NoError(t, testDB.Where("id = ?", mem.ID).First(&got).Error)
	assert.True(t, got.WithReplies, "nil 渡しは既存値を維持 (#1948-15)")

	// nil + 不在: NO_SUCH_USER 判定のため ErrRecordNotFound を返す。
	err := r.UpdateMembership(list.ID, "ghost_member", nil)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound, "nil + membership 不在は ErrRecordNotFound (#1948-15)")
}

// --- page: ListPublicByUser limit defaults ---

func TestPageRepository_ListPublicByUser_Clamp(t *testing.T) {
	r := NewPageRepository(testDB)
	_, err := r.ListPublicByUser("x", "", "", 0, 0)
	require.NoError(t, err)
	_, err = r.ListPublicByUser("x", "", "", 999, 0)
	require.NoError(t, err)
}

// --- page_like: ListByUser limit defaults ---

func TestPageLikeRepository_ListByUser_Clamp(t *testing.T) {
	r := NewPageLikeRepository(testDB)
	_, err := r.ListByUser("x", "", "", 0, 0)
	require.NoError(t, err)
	_, err = r.ListByUser("x", "", "", 999, 0)
	require.NoError(t, err)
}

// --- drive_file: ListForAdmin limit/filter branches ---

func TestDriveFileRepository_ListForAdmin_Branches(t *testing.T) {
	r := NewDriveFileRepository(testDB)

	// limit default + remote + host指定 + fileType + since/until
	_, err := r.ListForAdmin("", "remote", "host.example", "image/", "since_x", "until_x", 0)
	require.NoError(t, err)
	_, err = r.ListForAdmin("", "remote", "host.example", "image/", "since_x", "until_x", 999)
	require.NoError(t, err)
	// origin="" (no userHost filter)
	_, err = r.ListForAdmin("", "", "", "", "", "", 10)
	require.NoError(t, err)
	// userId 指定 (origin / host が無視されるルートを通す)
	_, err = r.ListForAdmin("nonexistent_user", "remote", "host.example", "", "", "", 10)
	require.NoError(t, err)
}

// --- emoji.ListRemoteWithFilter: limit defaults ---

func TestEmojiRepository_ListRemoteWithFilter_Clamp(t *testing.T) {
	r := NewEmojiRepository(testDB)
	_, err := r.ListRemoteWithFilter("", "remote.example", "", "", 0, 0)
	require.NoError(t, err)
	_, err = r.ListRemoteWithFilter("", "remote.example", "", "", 999, 0)
	require.NoError(t, err)
}

// --- instance.List: filter branches ---

func TestInstanceRepository_List_Branches(t *testing.T) {
	r := NewInstanceRepository(testDB)
	tr := true
	fa := false

	for _, f := range []model.InstanceListFilter{
		{Limit: 10},
		{Limit: 10, SortBy: "+pubAt"},
		{Limit: 10, SortBy: "-pubAt"},
		{Limit: 10, SortBy: "+notes", Host: "example"},
		{Limit: 10, SortBy: "-notes"},
		{Limit: 10, SortBy: "+users"},
		{Limit: 10, SortBy: "-users"},
		{Limit: 10, SortBy: "+following"},
		{Limit: 10, SortBy: "-following"},
		{Limit: 10, SortBy: "+followers"},
		{Limit: 10, SortBy: "-followers"},
		{Limit: 10, SortBy: "unknown_sort"},
		{Limit: 10, Suspended: &tr},
		{Limit: 10, Suspended: &fa},
		{Limit: 10, NotResponding: &tr},
		{Limit: 10, Federating: &tr},
		{Limit: 10, Federating: &fa},
		{Limit: 10, Subscribing: &tr},
		{Limit: 10, Subscribing: &fa},
		{Limit: 10, Publishing: &tr},
		{Limit: 10, Publishing: &fa},
	} {
		_, err := r.List(f)
		require.NoError(t, err)
	}
}

// --- meta.EnsureInitial ---

func TestMetaRepository_EnsureInitial_ExistingRow(t *testing.T) {
	r := NewMetaRepository(testDB)
	// 既にメタ行がある状態でEnsureInitialを呼ぶと no-op
	// (初期化時に重複INSERT回避パスを踏む)
	_, err := r.Fetch()
	if err != nil {
		// 既存メタなしなら追加
		require.NoError(t, testDB.Exec(
			`INSERT INTO "meta" (id) VALUES ('x') ON CONFLICT (id) DO NOTHING`,
		).Error)
	}
	require.NoError(t, r.EnsureInitial("x"))
}

// --- promo.Exists success path ---

func TestPromoReadRepository_IsRead_SuccessAndFalse(t *testing.T) {
	r := NewPromoReadRepository(testDB)
	u := insertTestUser(t, "pr_ir_u1", "prir")
	defer cleanupUser(t, u.ID)

	// noteを作成 (FK用)
	nr := NewNoteRepository(testDB)
	txt := "promo"
	n := &model.Note{
		ID:         "pr_ir_n1",
		UserID:     u.ID,
		Text:       &txt,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, nr.Create(n))
	defer cleanupNote(t, n.ID)

	// 未readは false
	assert.False(t, r.IsRead(u.ID, n.ID))

	// MarkReadしてtrueに
	require.NoError(t, r.MarkRead(&model.PromoRead{ID: "pr_ir_p1", UserID: u.ID, NoteID: n.ID}))
	defer testDB.Exec(`DELETE FROM "promo_read" WHERE id = ?`, "pr_ir_p1")
	assert.True(t, r.IsRead(u.ID, n.ID))
}

// --- registration_ticket.List: filter分岐 ---

func TestRegistrationTicketRepository_List_Filters(t *testing.T) {
	r := NewRegistrationTicketRepository(testDB)
	now := time.Now()
	_, err := r.List("all", 10, 0, now)
	require.NoError(t, err)
	_, err = r.List("unused", 10, 0, now)
	require.NoError(t, err)
	_, err = r.List("used", 10, 0, now)
	require.NoError(t, err)
	_, err = r.List("expired", 10, 0, now)
	require.NoError(t, err)
	_, err = r.List("unknown", 10, 0, now)
	require.NoError(t, err)
}

// --- note_reaction.ListByNoteID: reactions フィルタ付き ---
// ListByNoteIDシグネチャが複雑なためリフレクションを使わない形で踏む。
// 関数が内部で reactions スライスのlenを見て分岐するパスは既に cancelled
// context testで踏めているので、ここでは追加テストしない。

// --- role_notes_query.ListByRole: sinceのみ / untilのみ / 両方 ---

func TestRoleNotesQuery_ListByRole_Extras(t *testing.T) {
	q := NewRoleNotesQuery(testDB)
	_, err := q.ListByRole("x", 10, "since_x", "")
	require.NoError(t, err)
	_, err = q.ListByRole("x", 10, "", "until_x")
	require.NoError(t, err)
}

// --- user.ListUsers branch (admin paging / filter) ---

func TestUserRepository_ListUsers_Branches(t *testing.T) {
	r := NewUserRepository(testDB)
	for _, f := range []model.UserListFilter{
		{Limit: 10, Origin: "local", Sort: "+createdAt"},
		{Limit: 10, Origin: "remote", Sort: "-createdAt"},
		{Limit: 10, Origin: "combined", State: "suspended", Hostname: "host.example", Sort: "+updatedAt"},
		{Limit: 10, State: "admin", Sort: "-updatedAt"},
		{Limit: 10, State: "moderator", Sort: "+lastActiveDate"},
		{Limit: 10, State: "alive", Sort: "-lastActiveDate"},
		{Limit: 10, State: "silenced", Sort: "+followerCount"},
		{Limit: 10, Sort: "-followerCount"},
		{Limit: 10, Sort: "+followingCount"},
		{Limit: 10, Sort: "-followingCount"},
		{Limit: 10, Sort: "unknown"},
	} {
		_, err := r.ListUsers(f)
		require.NoError(t, err)
	}
}

// --- user.ListRemoteInboxes branch ---

func TestUserRepository_ListRemoteInboxes_Branches(t *testing.T) {
	r := NewUserRepository(testDB)
	_, err := r.ListRemoteInboxes()
	require.NoError(t, err)
}

// note.DeleteExpiredRemoteNotes のテスト: aidx ID cutoff でリモートノートを
// 削除する挙動を確認する。mk-go は note."createdAt" カラムを持たないため、
// id 文字列の lexicographic 比較で時刻境界を判定する。
func TestNoteRepository_DeleteExpiredRemoteNotes(t *testing.T) {
	nr := NewNoteRepository(testDB)

	// リモートユーザー作成
	host := "remote.example"
	u := &model.User{
		ID:                "den_u1",
		Username:          "remote_note_owner",
		UsernameLower:     "remote_note_owner",
		Host:              &host,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(u).Error)
	defer cleanupUser(t, u.ID)

	// 古いid (time prefix が小さい) / 新しいid (大きい) のリモートノートを投入。
	// aidx: 先頭8文字が base36(ms-since-2000)。"00000000..." は 2000-01-01。
	oldHost := host
	oldNote := &model.Note{
		ID: "00000000_old_note", UserID: u.ID, UserHost: &oldHost,
		Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, nr.Create(oldNote))
	defer cleanupNote(t, oldNote.ID)

	// 新しい (将来) noteは削除されない
	futureNote := &model.Note{
		ID: "zzzzzzzzfuture_z", UserID: u.ID, UserHost: &oldHost,
		Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, nr.Create(futureNote))
	defer cleanupNote(t, futureNote.ID)

	// ローカルノート (userHost IS NULL) は削除されない
	localNote := &model.Note{
		ID: "00000000_local_z", UserID: u.ID, // UserHost nil
		Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, nr.Create(localNote))
	defer cleanupNote(t, localNote.ID)

	// expiryDays=1 (1日以前) を設定すると古いnoteは消える
	n, err := nr.DeleteExpiredRemoteNotes(1, 10)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(1))

	// 新しいnoteは残っている
	_, err = nr.FindByID(futureNote.ID)
	require.NoError(t, err)
	// ローカルnoteも残る
	_, err = nr.FindByID(localNote.ID)
	require.NoError(t, err)
	// 古いリモートnoteは消えた
	_, err = nr.FindByID(oldNote.ID)
	assert.Error(t, err)
}

// aidxCutoffID: 境界条件のunit test (時刻境界 / 長さ調整)。
func TestAidxCutoffID(t *testing.T) {
	// 2000-01-01 直後 (ms=0) → "0000000000000000"
	zero := aidxCutoffID(time.UnixMilli(aidxTime2000Ms))
	assert.Equal(t, "0000000000000000", zero)

	// 2000年以前 (ms<0) は 0 に clamp
	negative := aidxCutoffID(time.UnixMilli(aidxTime2000Ms - 1000))
	assert.Equal(t, "0000000000000000", negative)

	// 現在時刻は16文字, 先頭は0以外 (大半のケース)
	now := aidxCutoffID(time.Now())
	assert.Len(t, now, 16)
	assert.Equal(t, "00000000", now[8:])

	// 遠い未来 (>2089年): truncate 経路を踏む
	far := aidxCutoffID(time.UnixMilli(aidxTime2000Ms + 36_000_000_000_000_000))
	assert.Len(t, far, 16)
}

// note.DeleteByUserBatch: empty userID / batchSize=0 分岐
func TestNoteRepository_DeleteByUserBatch_EarlyReturns(t *testing.T) {
	nr := NewNoteRepository(testDB)

	// userID空はno-op
	n, err := nr.DeleteByUserBatch("", 10)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	// batchSize <= 0 のときdefault 100適用 (存在しないuserで呼ぶ)
	_, err = nr.DeleteByUserBatch("nonexistent_user", 0)
	require.NoError(t, err)
}

// page_like.ListByUser: offset >0 の分岐
func TestPageLikeRepository_ListByUser_Offset(t *testing.T) {
	r := NewPageLikeRepository(testDB)
	_, err := r.ListByUser("x", "", "", 10, 5)
	require.NoError(t, err)
}

// TestPageLikeRepository_ListByUser_Cursor covers the cursor branches added
// for /api/i/page-likes fetchOlder (#1136 follow-up). sinceID 単独で ASC、
// untilID 単独で DESC、両方指定で AND-DESC をそれぞれ exercise する。
func TestPageLikeRepository_ListByUser_Cursor(t *testing.T) {
	r := NewPageLikeRepository(testDB)
	_, err := r.ListByUser("x", "since_x", "", 10, 0)
	require.NoError(t, err)
	_, err = r.ListByUser("x", "", "until_x", 10, 0)
	require.NoError(t, err)
	_, err = r.ListByUser("x", "since_x", "until_x", 10, 0)
	require.NoError(t, err)
}

// note_reaction.ListByNoteID: since/untilIDあり分岐
func TestNoteReactionRepository_ListByNoteID_SinceUntil(t *testing.T) {
	r := NewNoteReactionRepository(testDB)
	_, err := r.ListByNoteID("x", "until_x", "", 10, nil)
	require.NoError(t, err)
	_, err = r.ListByNoteID("x", "", "since_x", 10, nil)
	require.NoError(t, err)
}

// chat.ListHistory: DBに会話データがあるsuccess path。
func TestChatRepository_ListHistory_WithData(t *testing.T) {
	r := NewChatRepository(testDB)
	u1 := insertTestUser(t, "clh_u1", "clh1")
	defer cleanupUser(t, u1.ID)
	u2 := insertTestUser(t, "clh_u2", "clh2")
	defer cleanupUser(t, u2.ID)

	msg := &model.ChatMessage{
		ID: "clh_m1", FromUserID: u1.ID, ToUserID: &u2.ID, Text: strPtrCC("hi"),
	}
	require.NoError(t, r.CreateMessage(msg))
	defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, msg.ID)

	history, err := r.ListHistory(u1.ID, 10)
	require.NoError(t, err)
	assert.NotEmpty(t, history)
}

// emoji.ListRemoteWithFilter: querystring指定 (substring filter) 分岐
func TestEmojiRepository_ListRemoteWithFilter_Query(t *testing.T) {
	r := NewEmojiRepository(testDB)
	_, err := r.ListRemoteWithFilter("cat", "remote.example", "", "", 10, 0)
	require.NoError(t, err)
}

// registration_ticket.List: createdByあり / usedByあり な行を混ぜて全filter分岐を踏む。
func TestRegistrationTicketRepository_List_WithData(t *testing.T) {
	r := NewRegistrationTicketRepository(testDB)
	u := insertTestUser(t, "rt_wd_u1", "rtwd")
	defer cleanupUser(t, u.ID)

	// 使われていないtickets
	require.NoError(t, testDB.Exec(
		`INSERT INTO "registration_ticket" (id, code, "createdById") VALUES (?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		"rt_wd_1", "c_wd_1", u.ID,
	).Error)
	// 使われたticket
	require.NoError(t, testDB.Exec(
		`INSERT INTO "registration_ticket" (id, code, "createdById", "usedAt", "usedById") VALUES (?, ?, ?, NOW(), ?) ON CONFLICT (id) DO NOTHING`,
		"rt_wd_2", "c_wd_2", u.ID, u.ID,
	).Error)
	defer testDB.Exec(`DELETE FROM "registration_ticket" WHERE id IN (?, ?)`, "rt_wd_1", "rt_wd_2")

	now := time.Now()
	// 全filterパスを再度 (データ有りパス)
	_, err := r.List("all", 10, 0, now)
	require.NoError(t, err)
	_, err = r.List("unused", 10, 0, now)
	require.NoError(t, err)
	_, err = r.List("used", 10, 0, now)
	require.NoError(t, err)
}

// role_notes_query.ListByRole: roleに所属するuserを持つsuccess path
func TestRoleNotesQuery_ListByRole_Success(t *testing.T) {
	q := NewRoleNotesQuery(testDB)
	// 実データ有無に関わらずJOIN分岐はerror無しで抜ける (既存テストで十分)
	_, err := q.ListByRole("x", 10, "s", "u")
	require.NoError(t, err)
}

// user_list: ListsContainingMember owner無しパスとerror paths
func TestUserListRepository_ListsContainingMember_NoResult(t *testing.T) {
	r := NewUserListRepository(testDB)
	lists, err := r.ListsContainingMember("none_owner", "none_member")
	require.NoError(t, err)
	assert.Empty(t, lists)
}

// meta.EnsureInitial: すでに行がある状態 (no-op path) と 新規挿入path
func TestMetaRepository_EnsureInitial_BothPaths(t *testing.T) {
	// 一時DBにメタが無い状態から始めるのは不可能なので、現状を保存して新規挿入を実行。
	// 既にメタ行があるなら早期returnパスを踏む (expected)。
	r := NewMetaRepository(testDB)
	// no-op / create のどちらかを踏むだけでOK
	require.NoError(t, r.EnsureInitial("test_meta_id"))
	// 2回目は必ずno-op
	require.NoError(t, r.EnsureInitial("test_meta_id"))
}

// meta.EnsureInitial: cancelled contextで `if err != gorm.ErrRecordNotFound` 経路を踏む。
func TestMetaRepository_EnsureInitial_DBError(t *testing.T) {
	r := NewMetaRepository(cancelledDB(t))
	err := r.EnsureInitial("whatever")
	assert.Error(t, err)
}

// emoji.ListRemoteWithFilter: offset > 0 分岐
func TestEmojiRepository_ListRemoteWithFilter_Offset(t *testing.T) {
	r := NewEmojiRepository(testDB)
	_, err := r.ListRemoteWithFilter("", "remote.example", "", "", 10, 5)
	require.NoError(t, err)
}

// note.DeleteExpiredRemoteNotes: batchSize<=0 default + error path
func TestNoteRepository_DeleteExpiredRemoteNotes_Edges(t *testing.T) {
	nr := NewNoteRepository(testDB)
	// batchSize=0 → default 100適用
	_, err := nr.DeleteExpiredRemoteNotes(365*100, 0) // 100年以上前は何も削除しない
	require.NoError(t, err)

	// cancelled contextでerror path
	cn := NewNoteRepository(cancelledDB(t))
	_, err = cn.DeleteExpiredRemoteNotes(1, 10)
	assert.Error(t, err)
}

// note_reaction.ListByNoteID: len(reactions) > 1 分岐
func TestNoteReactionRepository_ListByNoteID_MultipleReactions(t *testing.T) {
	r := NewNoteReactionRepository(testDB)
	_, err := r.ListByNoteID("x", "", "", 10, []string{"👍", "🎉", "❤"})
	require.NoError(t, err)
}

// promo.Exists (PromoNoteRepository): cancelled contextでerror path
func TestPromoNoteRepository_Exists_DBError(t *testing.T) {
	r := NewPromoNoteRepository(cancelledDB(t))
	_, err := r.Exists("x")
	assert.Error(t, err)
}

// registration_ticket.List: cancelled contextでerror path
func TestRegistrationTicketRepository_List_DBError(t *testing.T) {
	r := NewRegistrationTicketRepository(cancelledDB(t))
	_, err := r.List("all", 10, 0, time.Now())
	assert.Error(t, err)
}

// role_notes_query.ListByRole: cancelled contextでerror path
func TestRoleNotesQuery_ListByRole_DBError(t *testing.T) {
	q := NewRoleNotesQuery(cancelledDB(t))
	_, err := q.ListByRole("x", 10, "", "")
	assert.Error(t, err)
}

// emoji.buildV2Query: UpdatedAtFrom / UpdatedAtTo / RoleIDs 分岐を踏む (rebase後にdevelopに追加された新規機能)
func TestEmojiRepository_ListV2_UpdatedAtRange(t *testing.T) {
	repo := NewEmojiRepository(testDB)
	// UpdatedAt分岐
	_, err := repo.ListV2(model.EmojiV2Filter{
		Limit: 10,
		Query: &model.EmojiV2Query{
			UpdatedAtFrom: "2000-01-01",
			UpdatedAtTo:   "2099-12-31",
		},
	})
	require.NoError(t, err)
	// RoleIDs分岐は別呼び出し (pq.Array wrapが必要)
	_, err = repo.ListV2(model.EmojiV2Filter{
		Limit: 10,
		Query: &model.EmojiV2Query{
			RoleIDs: []string{"role_x"},
		},
	})
	require.NoError(t, err)
}
