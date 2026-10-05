package repository

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func cleanupReaction(t *testing.T, id string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "note_reaction" WHERE id = ?`, id)
}

func TestNoteReactionRepository_CreateAndFind(t *testing.T) {
	repo := NewNoteReactionRepository(testDB)
	user := insertTestUser(t, "u_nr_1", "rxuser1")
	defer cleanupUser(t, user.ID)

	noteRepo := NewNoteRepository(testDB)
	note := &model.Note{
		ID:         "n_nr_1",
		UserID:     user.ID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, noteRepo.Create(note))
	defer cleanupNote(t, note.ID)

	rec := &model.NoteReaction{ID: "rx_1", UserID: user.ID, NoteID: note.ID, Reaction: "👍"}
	require.NoError(t, repo.Create(rec))
	defer cleanupReaction(t, rec.ID)

	found, err := repo.FindByPair(user.ID, note.ID)
	require.NoError(t, err)
	assert.Equal(t, "👍", found.Reaction)
}

func TestNoteReactionRepository_DuplicateError(t *testing.T) {
	repo := NewNoteReactionRepository(testDB)
	user := insertTestUser(t, "u_nr_2", "rxuser2")
	defer cleanupUser(t, user.ID)

	noteRepo := NewNoteRepository(testDB)
	note := &model.Note{
		ID:         "n_nr_2",
		UserID:     user.ID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, noteRepo.Create(note))
	defer cleanupNote(t, note.ID)

	rec1 := &model.NoteReaction{ID: "rx_d1", UserID: user.ID, NoteID: note.ID, Reaction: "👍"}
	require.NoError(t, repo.Create(rec1))
	defer cleanupReaction(t, rec1.ID)

	// 同じ (userId, noteId) で再度作成すると一意制約違反
	rec2 := &model.NoteReaction{ID: "rx_d2", UserID: user.ID, NoteID: note.ID, Reaction: "❤"}
	err := repo.Create(rec2)
	assert.ErrorIs(t, err, ErrDuplicateReaction)
}

func TestNoteReactionRepository_Delete(t *testing.T) {
	repo := NewNoteReactionRepository(testDB)
	user := insertTestUser(t, "u_nr_3", "rxuser3")
	defer cleanupUser(t, user.ID)

	noteRepo := NewNoteRepository(testDB)
	note := &model.Note{
		ID:         "n_nr_3",
		UserID:     user.ID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, noteRepo.Create(note))
	defer cleanupNote(t, note.ID)

	rec := &model.NoteReaction{ID: "rx_del_1", UserID: user.ID, NoteID: note.ID, Reaction: "👍"}
	require.NoError(t, repo.Create(rec))
	affected, derr := repo.Delete(rec)
	require.NoError(t, derr)
	assert.Equal(t, int64(1), affected) // #2106 L40: 削除した行数を返す

	_, err := repo.FindByPair(user.ID, note.ID)
	assert.Error(t, err)
}

func TestNoteReactionRepository_ListByNoteID(t *testing.T) {
	repo := NewNoteReactionRepository(testDB)
	user1 := insertTestUser(t, "u_nr_4", "rxuser4")
	user2 := insertTestUser(t, "u_nr_5", "rxuser5")
	defer cleanupUser(t, user1.ID)
	defer cleanupUser(t, user2.ID)

	noteRepo := NewNoteRepository(testDB)
	note := &model.Note{
		ID:         "n_nr_4",
		UserID:     user1.ID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, noteRepo.Create(note))
	defer cleanupNote(t, note.ID)

	for _, r := range []struct {
		id, uid, rx string
	}{
		{"rx_l1", user1.ID, "👍"},
		{"rx_l2", user2.ID, "❤"},
	} {
		rec := &model.NoteReaction{ID: r.id, UserID: r.uid, NoteID: note.ID, Reaction: r.rx}
		require.NoError(t, repo.Create(rec))
		defer cleanupReaction(t, rec.ID)
	}

	out, err := repo.ListByNoteID(note.ID, "", "", 10, nil)
	require.NoError(t, err)
	assert.Len(t, out, 2)

	// 種類絞り込み
	out, err = repo.ListByNoteID(note.ID, "", "", 10, []string{"👍"})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "👍", out[0].Reaction)

	// untilID で絞り込み
	out, err = repo.ListByNoteID(note.ID, "rx_l2", "", 10, nil)
	require.NoError(t, err)
	assert.Len(t, out, 1)

	// sinceID で絞り込み
	out, err = repo.ListByNoteID(note.ID, "", "rx_l1", 10, nil)
	require.NoError(t, err)
	assert.Len(t, out, 1)
}

// TestNoteReactionRepository_ListByUserID covers the #821 PR-D path used by
// users/reactions endpoint: reactor 視点の reaction list を取得し、User /
// Note を Preload して pack 時の N+1 を avoid する。
func TestNoteReactionRepository_ListByUserID(t *testing.T) {
	repo := NewNoteReactionRepository(testDB)
	user1 := insertTestUser(t, "u_nr_6", "rxuser6")
	user2 := insertTestUser(t, "u_nr_7", "rxuser7")
	defer cleanupUser(t, user1.ID)
	defer cleanupUser(t, user2.ID)

	noteRepo := NewNoteRepository(testDB)
	note1 := &model.Note{
		ID:         "n_nr_5",
		UserID:     user1.ID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	note2 := &model.Note{
		ID:         "n_nr_6",
		UserID:     user1.ID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, noteRepo.Create(note1))
	require.NoError(t, noteRepo.Create(note2))
	defer cleanupNote(t, note1.ID)
	defer cleanupNote(t, note2.ID)

	// user2 が note1 / note2 に reaction、user1 は note2 のみに reaction。
	// ListByUserID(user2) で 2 件、ListByUserID(user1) で 1 件返ることを check。
	for _, r := range []struct {
		id, uid, nid, rx string
	}{
		{"rx_u1", user2.ID, note1.ID, "👍"},
		{"rx_u2", user2.ID, note2.ID, "❤"},
		{"rx_u3", user1.ID, note2.ID, "🎉"},
	} {
		rec := &model.NoteReaction{ID: r.id, UserID: r.uid, NoteID: r.nid, Reaction: r.rx}
		require.NoError(t, repo.Create(rec))
		defer cleanupReaction(t, rec.ID)
	}

	// user2 視点で 2 件、Preload("User") / Preload("Note") が効いていることも
	// 確認する。
	// note は全て public なので匿名 viewer (viewerID="") でも全件見える。
	out, err := repo.ListByUserID(user2.ID, "", "", "", 10)
	require.NoError(t, err)
	require.Len(t, out, 2)
	for _, rec := range out {
		assert.Equal(t, user2.ID, rec.UserID)
		require.NotNil(t, rec.User, "User should be preloaded")
		assert.Equal(t, user2.ID, rec.User.ID)
		require.NotNil(t, rec.Note, "Note should be preloaded")
	}

	// user1 視点で 1 件 (= note2 のみ)。
	out, err = repo.ListByUserID(user1.ID, "", "", "", 10)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "rx_u3", out[0].ID)

	// untilID で絞り込み (rx_u2 より前 → rx_u1 の 1 件)。
	out, err = repo.ListByUserID(user2.ID, "", "rx_u2", "", 10)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "rx_u1", out[0].ID)

	// sinceID で絞り込み (rx_u1 より後 → rx_u2 の 1 件)。
	out, err = repo.ListByUserID(user2.ID, "", "", "rx_u1", 10)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "rx_u2", out[0].ID)
}

// TestNoteReactionRepository_ListByUserID_VisibilityPushDown は
// generateVisibilityQuery 相当の SQL push down を検証する。reactor が
// public / followers-only / specified の 3 note に reaction し、viewer ごとに
// 見える reaction が変わることを確認する (LIMIT 前に SQL で絞られるため
// ページネーションが途切れない)。
func TestNoteReactionRepository_ListByUserID_VisibilityPushDown(t *testing.T) {
	repo := NewNoteReactionRepository(testDB)
	followingRepo := NewFollowingRepository(testDB)
	noteRepo := NewNoteRepository(testDB)

	// 各 insert 直後に cleanup を登録する。途中の insert が失敗しても先行分が
	// leak しないようにする (defer をまとめて後段で登録すると、途中失敗時に
	// 先行ユーザーが共有 testDB に残る)。
	mkUser := func(id, username string) *model.User {
		u := insertTestUser(t, id, username)
		t.Cleanup(func() { cleanupUser(t, u.ID) })
		return u
	}
	reactor := mkUser("u_nrv_r", "nrvreact")
	author := mkUser("u_nrv_a", "nrvauthor")
	follower := mkUser("u_nrv_f", "nrvfollower")
	allowed := mkUser("u_nrv_al", "nrvallowed")
	stranger := mkUser("u_nrv_s", "nrvstranger")

	notes := []*model.Note{
		{ID: "n_nrv_pub", UserID: author.ID, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_nrv_fol", UserID: author.ID, Visibility: model.NoteVisibilityFollowers, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_nrv_spec", UserID: author.ID, Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: model.StringArray{allowed.ID}, Reactions: datatypes.JSON([]byte("{}"))},
	}
	for _, n := range notes {
		require.NoError(t, noteRepo.Create(n))
		defer cleanupNote(t, n.ID)
	}

	for _, r := range []struct{ id, nid, rx string }{
		{"rxv_pub", "n_nrv_pub", "👍"},
		{"rxv_fol", "n_nrv_fol", "❤"},
		{"rxv_spec", "n_nrv_spec", "👀"},
	} {
		rec := &model.NoteReaction{ID: r.id, UserID: reactor.ID, NoteID: r.nid, Reaction: r.rx}
		require.NoError(t, repo.Create(rec))
		defer cleanupReaction(t, rec.ID)
	}

	// follower が author を follow している。
	f := &model.Following{ID: "fl_nrv_1", FollowerID: follower.ID, FolloweeID: author.ID}
	require.NoError(t, followingRepo.Create(f))
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, f.ID)

	idsOf := func(rows []*model.NoteReaction) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.ID)
		}
		sort.Strings(out)
		return out
	}

	// 匿名 viewer は public のみ。
	out, err := repo.ListByUserID(reactor.ID, "", "", "", 50)
	require.NoError(t, err)
	assert.Equal(t, []string{"rxv_pub"}, idsOf(out))

	// stranger (follow なし / specified 対象外) も public のみ。
	out, err = repo.ListByUserID(reactor.ID, stranger.ID, "", "", 50)
	require.NoError(t, err)
	assert.Equal(t, []string{"rxv_pub"}, idsOf(out))

	// follower は public + followers。
	out, err = repo.ListByUserID(reactor.ID, follower.ID, "", "", 50)
	require.NoError(t, err)
	assert.Equal(t, []string{"rxv_fol", "rxv_pub"}, idsOf(out))

	// specified の visibleUserIds に含まれる viewer は public + specified。
	out, err = repo.ListByUserID(reactor.ID, allowed.ID, "", "", 50)
	require.NoError(t, err)
	assert.Equal(t, []string{"rxv_pub", "rxv_spec"}, idsOf(out))

	// author 本人は自分の followers / specified note を全て見られる。
	out, err = repo.ListByUserID(reactor.ID, author.ID, "", "", 50)
	require.NoError(t, err)
	assert.Equal(t, []string{"rxv_fol", "rxv_pub", "rxv_spec"}, idsOf(out))
}

func TestNoteReactionRepository_QueryErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	repo := NewNoteReactionRepository(db)

	err := repo.Create(&model.NoteReaction{ID: "x", UserID: "u", NoteID: "n", Reaction: "a"})
	assert.Error(t, err)
	// キャンセル済みコンテキストはErrDuplicateReactionではなく素のエラー
	assert.False(t, errors.Is(err, ErrDuplicateReaction))

	_, err = repo.FindByPair("u", "n")
	assert.Error(t, err)

	_, err = repo.ListByNoteID("n", "", "", 10, nil)
	assert.Error(t, err)
}

// #2067: ListReactionPairs は "<userId>/<reaction>" を id 昇順 (oldest first) で
// 最大 limit 件返す。note_reaction 行が即 persist されるため buffered count とは独立。
func TestNoteReactionRepository_ListReactionPairs(t *testing.T) {
	repo := NewNoteReactionRepository(testDB)
	u1 := insertTestUser(t, "u_lrp_1", "lrpuser1")
	defer cleanupUser(t, u1.ID)
	u2 := insertTestUser(t, "u_lrp_2", "lrpuser2")
	defer cleanupUser(t, u2.ID)

	noteRepo := NewNoteRepository(testDB)
	note := &model.Note{ID: "n_lrp_1", UserID: u1.ID, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))}
	require.NoError(t, noteRepo.Create(note))
	defer cleanupNote(t, note.ID)

	// id 昇順で rx_lrp_1 (u1/👍) → rx_lrp_2 (u2/❤)。
	for _, rec := range []*model.NoteReaction{
		{ID: "rx_lrp_1", UserID: u1.ID, NoteID: note.ID, Reaction: "👍"},
		{ID: "rx_lrp_2", UserID: u2.ID, NoteID: note.ID, Reaction: "❤"},
	} {
		require.NoError(t, repo.Create(rec))
		defer cleanupReaction(t, rec.ID)
	}

	pairs, err := repo.ListReactionPairs(note.ID, 16)
	require.NoError(t, err)
	assert.Equal(t, []string{u1.ID + "/👍", u2.ID + "/❤"}, pairs, "oldest first")

	// limit でキャップされる (oldest 1 件のみ)。
	capped, err := repo.ListReactionPairs(note.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, []string{u1.ID + "/👍"}, capped)

	// limit <= 0 は nil。
	none, err := repo.ListReactionPairs(note.ID, 0)
	require.NoError(t, err)
	assert.Nil(t, none)

	// reaction の無い note は空。
	empty, err := repo.ListReactionPairs("n_lrp_nonexistent", 16)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// **凍結した利用者のノートへのリアクションを返さないこと。**
//
// `users/reactions` はリアクション先のノートを完全な shape で返すので、その
// 著者が凍結されていれば出さない。upstream `users/reactions.ts` は
// `generateSuspendedUserQueryForNote` を掛けている。
func TestNoteReactionRepository_ListByUserID_ExcludesSuspendedNoteAuthor(t *testing.T) {
	repo := NewNoteReactionRepository(testDB)
	author := insertTestUser(t, "u_nr_susp_a", "rxsuspa")
	reactor := insertTestUser(t, "u_nr_susp_r", "rxsuspr")
	defer cleanupUser(t, author.ID)
	defer cleanupUser(t, reactor.ID)

	noteRepo := NewNoteRepository(testDB)
	note := &model.Note{
		ID:         "n_nr_susp",
		UserID:     author.ID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, noteRepo.Create(note))
	defer cleanupNote(t, note.ID)

	rec := &model.NoteReaction{ID: "rx_susp_1", UserID: reactor.ID, NoteID: note.ID, Reaction: "👍"}
	require.NoError(t, repo.Create(rec))
	defer cleanupReaction(t, rec.ID)

	// 凍結前は返る (対照)。
	out, err := repo.ListByUserID(reactor.ID, "", "", "", 10)
	require.NoError(t, err)
	require.Len(t, out, 1, "凍結前は返ること")

	require.NoError(t, testDB.Exec(`UPDATE "user" SET "isSuspended" = true WHERE id = ?`, author.ID).Error)

	out, err = repo.ListByUserID(reactor.ID, "", "", "", 10)
	require.NoError(t, err)
	require.Empty(t, out, "ノートの著者が凍結されていたら返さないこと")
}

// **返信先 / リノート元の著者が凍結されていても返さないこと。**
//
// upstream の `generateSuspendedUserQueryForNote` は `user` / `replyUser` /
// `renoteUser` の 3 つを見る。著者しか見ないと、**凍結した利用者のノートが
// 「誰かの返信」として本文ごと出る**。`note` 側の
// `applySuspendedAuthorExclusion` と同じ 3 列を見ること。
func TestNoteReactionRepository_ListByUserID_ExcludesSuspendedReplyAndRenoteAuthor(t *testing.T) {
	// **ID はリテラルで書く。** CI の「重複フィクスチャ ID」検査は
	// `insertTestUser(t, "..."` の第 1 引数を文字列として読むので、連結で
	// 組むと前半だけを拾って誤検知する (実際に lint が落ちた)。
	for _, tc := range []struct {
		kind       string
		suspended  string
		author     string
		reactor    string
		baseNote   string
		targetNote string
		reaction   string
	}{
		{"reply", "u_nrrp_s", "u_nrrp_a", "u_nrrp_r", "n_nrrp_base", "n_nrrp_tgt", "rx_nrrp_1"},
		{"renote", "u_nrrn_s", "u_nrrn_a", "u_nrrn_r", "n_nrrn_base", "n_nrrn_tgt", "rx_nrrn_1"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			repo := NewNoteReactionRepository(testDB)
			noteRepo := NewNoteRepository(testDB)
			suspended := insertTestUser(t, tc.suspended, tc.suspended)
			author := insertTestUser(t, tc.author, tc.author)
			reactor := insertTestUser(t, tc.reactor, tc.reactor)
			defer cleanupUser(t, suspended.ID)
			defer cleanupUser(t, author.ID)
			defer cleanupUser(t, reactor.ID)

			// 凍結される側のノート。
			base := &model.Note{
				ID: tc.baseNote, UserID: suspended.ID,
				Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
			}
			require.NoError(t, noteRepo.Create(base))
			defer cleanupNote(t, base.ID)

			// それへの返信 / 引用。著者は凍結されていない。
			target := &model.Note{
				ID: tc.targetNote, UserID: author.ID,
				Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
			}
			if tc.kind == "reply" {
				target.ReplyID = &base.ID
				target.ReplyUserID = &suspended.ID
			} else {
				target.RenoteID = &base.ID
				target.RenoteUserID = &suspended.ID
			}
			require.NoError(t, noteRepo.Create(target))
			defer cleanupNote(t, target.ID)

			rec := &model.NoteReaction{
				ID: tc.reaction, UserID: reactor.ID, NoteID: target.ID, Reaction: "👍",
			}
			require.NoError(t, repo.Create(rec))
			defer cleanupReaction(t, rec.ID)

			// 凍結前は返る (対照)。
			out, err := repo.ListByUserID(reactor.ID, "", "", "", 10)
			require.NoError(t, err)
			require.Len(t, out, 1, "凍結前は返ること")

			require.NoError(t, testDB.Exec(
				`UPDATE "user" SET "isSuspended" = true WHERE id = ?`, suspended.ID).Error)

			out, err = repo.ListByUserID(reactor.ID, "", "", "", 10)
			require.NoError(t, err)
			require.Empty(t, out, "%s 先の著者が凍結されていたら返さないこと", tc.kind)
		})
	}
}
