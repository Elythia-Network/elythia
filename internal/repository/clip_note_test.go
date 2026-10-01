package repository

import (
	"context"
	"sort"
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestClipNoteRepository_LifeCycle(t *testing.T) {
	clipRepo := NewClipRepository(testDB)
	repo := NewClipNoteRepository(testDB)
	noteRepo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_cn_1", "cnuser1")
	defer cleanupUser(t, user.ID)

	c := newTestClip("clp_cn_1", user.ID, "alpha")
	require.NoError(t, clipRepo.Create(c))
	defer cleanupClip(t, c.ID)

	n := &model.Note{
		ID:         "n_cn_1",
		UserID:     user.ID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, noteRepo.Create(n))
	defer cleanupNote(t, n.ID)

	cn := &model.ClipNote{ID: "cn_pair_1", ClipID: c.ID, NoteID: n.ID}
	require.NoError(t, repo.Create(cn))
	defer testDB.Exec(`DELETE FROM "clip_note" WHERE id = ?`, cn.ID)

	got, err := repo.FindByPair(c.ID, n.ID)
	require.NoError(t, err)
	assert.Equal(t, cn.ID, got.ID)

	rows, err := repo.ListByClip(c.ID, "", "", 10)
	require.NoError(t, err)
	assert.Len(t, rows, 1)

	count, err := repo.CountByClip(c.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)

	require.NoError(t, repo.Delete(cn))
	_, err = repo.FindByPair(c.ID, n.ID)
	assert.Error(t, err)
}

// DeleteByPair は実際に消えた行数を返す。clips/remove-note は 0 件を
// NO_SUCH_NOTE にし、1 件のときだけ clippedCount を減らす (本家 4682d44cae)。
func TestClipNoteRepository_DeleteByPairReportsRows(t *testing.T) {
	clipRepo := NewClipRepository(testDB)
	repo := NewClipNoteRepository(testDB)
	noteRepo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_cn_delpair", "cndelpair")
	defer cleanupUser(t, user.ID)

	c := newTestClip("clp_cn_delpair", user.ID, "alpha")
	require.NoError(t, clipRepo.Create(c))
	defer cleanupClip(t, c.ID)

	n := &model.Note{
		ID:         "n_cn_delpair",
		UserID:     user.ID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, noteRepo.Create(n))
	defer cleanupNote(t, n.ID)

	require.NoError(t, repo.Create(&model.ClipNote{ID: "cn_delpair", ClipID: c.ID, NoteID: n.ID}))
	defer testDB.Exec(`DELETE FROM "clip_note" WHERE id = ?`, "cn_delpair")

	deleted, err := repo.DeleteByPair(c.ID, n.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), deleted)
	_, err = repo.FindByPair(c.ID, n.ID)
	assert.True(t, IsNotFound(err))

	deleted, err = repo.DeleteByPair(c.ID, n.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), deleted, "2 回目は 0 件")
}

// #1554 ListClipIDsByNote は note を含む clip の distinct clipId を返す。
func TestClipNoteRepository_ListClipIDsByNote(t *testing.T) {
	clipRepo := NewClipRepository(testDB)
	repo := NewClipNoteRepository(testDB)
	noteRepo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_cnlist", "cnlistuser")
	defer cleanupUser(t, user.ID)

	c1 := newTestClip("clp_cnl_1", user.ID, "c1")
	c2 := newTestClip("clp_cnl_2", user.ID, "c2")
	require.NoError(t, clipRepo.Create(c1))
	require.NoError(t, clipRepo.Create(c2))
	defer cleanupClip(t, c1.ID)
	defer cleanupClip(t, c2.ID)

	n := &model.Note{ID: "n_cnl_1", UserID: user.ID, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))}
	other := &model.Note{ID: "n_cnl_2", UserID: user.ID, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))}
	require.NoError(t, noteRepo.Create(n))
	require.NoError(t, noteRepo.Create(other))
	defer cleanupNote(t, n.ID)
	defer cleanupNote(t, other.ID)

	// n は c1 と c2 の両方に、other は c1 のみに含める。
	for _, cn := range []*model.ClipNote{
		{ID: "cn_l1", ClipID: c1.ID, NoteID: n.ID},
		{ID: "cn_l2", ClipID: c2.ID, NoteID: n.ID},
		{ID: "cn_l3", ClipID: c1.ID, NoteID: other.ID},
	} {
		require.NoError(t, repo.Create(cn))
		defer testDB.Exec(`DELETE FROM "clip_note" WHERE id = ?`, cn.ID)
	}

	ids, err := repo.ListClipIDsByNote(n.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{c1.ID, c2.ID}, ids)

	ids, err = repo.ListClipIDsByNote("n_cnl_nonexistent")
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestClipNoteRepository_FindByPair_NotFound(t *testing.T) {
	repo := NewClipNoteRepository(testDB)
	_, err := repo.FindByPair("nope", "nope")
	assert.Error(t, err)
}

func TestClipNoteRepository_ListByClip_LimitClampAndPagination(t *testing.T) {
	clipRepo := NewClipRepository(testDB)
	repo := NewClipNoteRepository(testDB)
	noteRepo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_cn_2", "cnuser2")
	defer cleanupUser(t, user.ID)

	c := newTestClip("clp_cn_2", user.ID, "beta")
	require.NoError(t, clipRepo.Create(c))
	defer cleanupClip(t, c.ID)

	for _, id := range []string{"n_cn_2a", "n_cn_2b", "n_cn_2c"} {
		n := &model.Note{
			ID: id, UserID: user.ID, Visibility: model.NoteVisibilityPublic,
			Reactions: datatypes.JSON([]byte("{}")),
		}
		require.NoError(t, noteRepo.Create(n))
		defer cleanupNote(t, n.ID)
		cn := &model.ClipNote{ID: "cn_" + id, ClipID: c.ID, NoteID: n.ID}
		require.NoError(t, repo.Create(cn))
		defer testDB.Exec(`DELETE FROM "clip_note" WHERE id = ?`, cn.ID)
	}

	rows, err := repo.ListByClip(c.ID, "", "", 9999) // limit clamp
	require.NoError(t, err)
	assert.Len(t, rows, 3)

	rows, err = repo.ListByClip(c.ID, "", "", -1)
	require.NoError(t, err)
	assert.Len(t, rows, 3)

	// cursor は client が渡す note.id (= clip_note.noteId)。clip_note.id ではない (#1950)。
	rows, err = repo.ListByClip(c.ID, "n_cn_2c", "", 10)
	require.NoError(t, err)
	assert.Len(t, rows, 2)

	rows, err = repo.ListByClip(c.ID, "", "n_cn_2a", 10)
	require.NoError(t, err)
	assert.Len(t, rows, 2)
}

// TestClipNoteRepository_ListByClip_PaginatesOnNoteID は #1950 の回帰テスト。
// clip_note.id (AddNote 時に独立採番) の並びと note.id (作成順) の並びが食い違う
// ように、古い note を後から clip し新しい note を先に clip する。ページネーションが
// note.id でなされること (upstream clips/notes parity) を、(1) 並び順が newest-NOTE-first
// であること、(2) client が最後の note.id を untilId に渡すと正しく次ページに進むこと
// で検証する。旧実装 (clip_note.id 比較) では並び順が逆転し untilId も無関係 ULID と
// 比較されて壊れる。
func TestClipNoteRepository_ListByClip_PaginatesOnNoteID(t *testing.T) {
	clipRepo := NewClipRepository(testDB)
	repo := NewClipNoteRepository(testDB)
	noteRepo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_cn_3", "cnuser3")
	defer cleanupUser(t, user.ID)

	c := newTestClip("clp_cn_3", user.ID, "gamma")
	require.NoError(t, clipRepo.Create(c))
	defer cleanupClip(t, c.ID)

	mkNote := func(id string) {
		n := &model.Note{ID: id, UserID: user.ID, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))}
		require.NoError(t, noteRepo.Create(n))
		t.Cleanup(func() { cleanupNote(t, n.ID) })
	}
	mkClipNote := func(clipNoteID, noteID string) {
		cn := &model.ClipNote{ID: clipNoteID, ClipID: c.ID, NoteID: noteID}
		require.NoError(t, repo.Create(cn))
		t.Cleanup(func() { testDB.Exec(`DELETE FROM "clip_note" WHERE id = ?`, cn.ID) })
	}

	mkNote("n_p001") // 古い note (小さい note.id)
	mkNote("n_p999") // 新しい note (大きい note.id)
	// 新しい note を先に clip (小さい clip_note.id)、古い note を後で clip (大きい clip_note.id)。
	mkClipNote("cn_001_early", "n_p999")
	mkClipNote("cn_999_late", "n_p001")

	// 並び順は note.id DESC (newest-note-first)。clip_note.id 順なら逆になる。
	rows, err := repo.ListByClip(c.ID, "", "", 50)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "n_p999", rows[0].NoteID, "newest note must be first (note.id order, not clip order)")
	assert.Equal(t, "n_p001", rows[1].NoteID)

	// client は最後に返った note.id (n_p999) を untilId として渡す -> 次は n_p001 のみ。
	rows, err = repo.ListByClip(c.ID, "n_p999", "", 50)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "n_p001", rows[0].NoteID)

	// sinceId に古い note.id を渡すと新しい側 (n_p999) のみ。
	rows, err = repo.ListByClip(c.ID, "", "n_p001", 50)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "n_p999", rows[0].NoteID)
}

// TestClipNoteRepository_ListByClipVisible は clips/notes の visibility
// push-down (#1418 review) を検証する。clip に public / followers / specified
// の note を混在させ、viewer ごとに SQL 段階で見える clip_note が変わること、
// および ListByClip (export 経路) は全件返す (= フィルタしない) ことを確認する。
func TestClipNoteRepository_ListByClipVisible(t *testing.T) {
	clipRepo := NewClipRepository(testDB)
	repo := NewClipNoteRepository(testDB)
	noteRepo := NewNoteRepository(testDB)
	followingRepo := NewFollowingRepository(testDB)

	mkUser := func(id, username string) *model.User {
		u := insertTestUser(t, id, username)
		t.Cleanup(func() { cleanupUser(t, u.ID) })
		return u
	}
	author := mkUser("u_cnv_a", "cnvauthor")
	follower := mkUser("u_cnv_f", "cnvfollower")
	allowed := mkUser("u_cnv_al", "cnvallowed")

	c := newTestClip("clp_cnv", author.ID, "vis")
	require.NoError(t, clipRepo.Create(c))
	defer cleanupClip(t, c.ID)

	notes := []*model.Note{
		{ID: "n_cnv_pub", UserID: author.ID, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_cnv_fol", UserID: author.ID, Visibility: model.NoteVisibilityFollowers, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_cnv_spec", UserID: author.ID, Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: model.StringArray{allowed.ID}, Reactions: datatypes.JSON([]byte("{}"))},
	}
	for _, n := range notes {
		require.NoError(t, noteRepo.Create(n))
		defer cleanupNote(t, n.ID)
		cn := &model.ClipNote{ID: "cnv_" + n.ID, ClipID: c.ID, NoteID: n.ID}
		require.NoError(t, repo.Create(cn))
		defer testDB.Exec(`DELETE FROM "clip_note" WHERE id = ?`, cn.ID)
	}

	f := &model.Following{ID: "fl_cnv_1", FollowerID: follower.ID, FolloweeID: author.ID}
	require.NoError(t, followingRepo.Create(f))
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, f.ID)

	noteIDsOf := func(rows []*model.ClipNote) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.NoteID)
		}
		sort.Strings(out)
		return out
	}

	// ListByClip (export 経路) はフィルタせず全件返す。
	rows, err := repo.ListByClip(c.ID, "", "", 50)
	require.NoError(t, err)
	assert.Equal(t, []string{"n_cnv_fol", "n_cnv_pub", "n_cnv_spec"}, noteIDsOf(rows))

	// 匿名 viewer は public のみ。
	rows, err = repo.ListByClipVisible(c.ID, "", "", "", 50, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"n_cnv_pub"}, noteIDsOf(rows))

	// follower は public + followers。
	rows, err = repo.ListByClipVisible(c.ID, follower.ID, "", "", 50, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"n_cnv_fol", "n_cnv_pub"}, noteIDsOf(rows))

	// specified 対象 viewer は public + specified。
	rows, err = repo.ListByClipVisible(c.ID, allowed.ID, "", "", 50, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"n_cnv_pub", "n_cnv_spec"}, noteIDsOf(rows))

	// author 本人は全 visibility を閲覧可。
	rows, err = repo.ListByClipVisible(c.ID, author.ID, "", "", 50, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"n_cnv_fol", "n_cnv_pub", "n_cnv_spec"}, noteIDsOf(rows))
}

func TestClipNoteRepository_ListByClip_QueryError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	repo := NewClipNoteRepository(db)
	_, err := repo.ListByClip("any", "", "", 10)
	assert.Error(t, err)
	_, err = repo.CountByClip("any")
	assert.Error(t, err)
}
