package repository

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestPollRepository_Create(t *testing.T) {
	repo := NewPollRepository(testDB)
	user := insertTestUser(t, "u_pc_1", "polluser")
	defer cleanupUser(t, user.ID)

	note := &model.Note{
		ID:         "n_pc_1",
		UserID:     user.ID,
		Visibility: model.NoteVisibilityPublic,
		HasPoll:    true,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, testDB.Create(note).Error)
	defer cleanupNote(t, note.ID)

	poll := &model.Poll{
		NoteID:         note.ID,
		Multiple:       false,
		Choices:        model.StringArray{"A", "B", "C"},
		Votes:          model.Int64Array{0, 0, 0},
		NoteVisibility: model.NoteVisibilityPublic,
		UserID:         user.ID,
	}
	require.NoError(t, repo.Create(poll))

	// 作成されたことを確認
	var found model.Poll
	err := testDB.First(&found, "\"noteId\" = ?", note.ID).Error
	require.NoError(t, err)
	assert.Equal(t, note.ID, found.NoteID)
	assert.Len(t, found.Choices, 3)
	assert.False(t, found.Multiple)
}

// #3443: core/note が受け付ける期限の上限 (note.MaxPollExpiresAtUnixMilli、
// 9999-12-30T23:59:59.999Z) が timestamptz に往復できる。セッションの時刻帯で
// 5 桁の年になると、simple protocol の読み戻しで parse に失敗する。repository
// から core/note は import できないので値を直に書く。
// note.MaxPollExpiresAtUnixMilli と同じ値であることは core/note の
// TestMaxPollExpiresAtUnixMilli_MatchesRepositoryTest が見る。
func TestPollRepository_StoresLatestAcceptedExpiry(t *testing.T) {
	repo := NewPollRepository(testDB)
	user := insertTestUser(t, "u_pmax_1", "pollmaxuser")
	defer cleanupUser(t, user.ID)

	note := &model.Note{ID: "n_pmax_1", UserID: user.ID, Visibility: model.NoteVisibilityPublic, HasPoll: true, Reactions: datatypes.JSON([]byte("{}"))}
	require.NoError(t, testDB.Create(note).Error)
	defer cleanupNote(t, note.ID)

	expires := time.UnixMilli(253_402_214_399_999)
	require.NoError(t, repo.Create(&model.Poll{
		NoteID: note.ID, Choices: model.StringArray{"a", "b"}, Votes: model.Int64Array{0, 0},
		NoteVisibility: model.NoteVisibilityPublic, UserID: user.ID, ExpiresAt: &expires,
	}))

	var found model.Poll
	require.NoError(t, testDB.First(&found, "\"noteId\" = ?", note.ID).Error)
	require.NotNil(t, found.ExpiresAt)
	assert.True(t, found.ExpiresAt.Equal(expires), "got %v, want %v", found.ExpiresAt, expires)
}

// TestPollRepository_ListExpiredUnnotified covers the ticker scan path
// added in #690 (ExpiryWorker)。partial index 経由のクエリ条件
// (expiresAt < now AND notifiedAt IS NULL) が正しく適用され、limit が効く
// ことを guard する。
func TestPollRepository_ListExpiredUnnotified(t *testing.T) {
	repo := NewPollRepository(testDB)
	user := insertTestUser(t, "u_lex_1", "lexuser")
	defer cleanupUser(t, user.ID)

	now := time.Now()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	// 3 種類の poll を seed: 期限切れ未通知 / 期限切れ通知済 / 未満了
	mk := func(id string, expires time.Time, notified *time.Time) {
		note := &model.Note{ID: id, UserID: user.ID, Visibility: model.NoteVisibilityPublic, HasPoll: true, Reactions: datatypes.JSON([]byte("{}"))}
		require.NoError(t, testDB.Create(note).Error)
		t.Cleanup(func() { cleanupNote(t, note.ID) })
		p := &model.Poll{
			NoteID: id, Multiple: false,
			Choices: model.StringArray{"a"}, Votes: model.Int64Array{0},
			NoteVisibility: model.NoteVisibilityPublic, UserID: user.ID,
			ExpiresAt:  &expires,
			NotifiedAt: notified,
		}
		require.NoError(t, repo.Create(p))
	}
	mk("p_lex_expired1", past, nil)
	mk("p_lex_expired2", past.Add(-time.Minute), nil)
	notified := past.Add(time.Minute)
	mk("p_lex_already", past, &notified)
	mk("p_lex_future", future, nil)

	rows, err := repo.ListExpiredUnnotified(now, 10)
	require.NoError(t, err)
	ids := []string{}
	for _, r := range rows {
		ids = append(ids, r.NoteID)
	}
	// 期限切れ未通知の 2 件のみ、ASC で並ぶ (古い方が先)
	assert.Equal(t, []string{"p_lex_expired2", "p_lex_expired1"}, ids)
}

func TestPollRepository_ListExpiredUnnotified_LimitCap(t *testing.T) {
	repo := NewPollRepository(testDB)
	user := insertTestUser(t, "u_lim_1", "limuser")
	defer cleanupUser(t, user.ID)

	now := time.Now()
	past := now.Add(-time.Hour)

	// limit=0 / 負数のとき内部で 100 にクランプされる
	for i := 0; i < 3; i++ {
		nid := "p_lim_" + string(rune('a'+i))
		note := &model.Note{ID: nid, UserID: user.ID, Visibility: model.NoteVisibilityPublic, HasPoll: true, Reactions: datatypes.JSON([]byte("{}"))}
		require.NoError(t, testDB.Create(note).Error)
		t.Cleanup(func() { cleanupNote(t, nid) })
		p := &model.Poll{NoteID: nid, Choices: model.StringArray{"x"}, Votes: model.Int64Array{0}, NoteVisibility: model.NoteVisibilityPublic, UserID: user.ID, ExpiresAt: &past}
		require.NoError(t, repo.Create(p))
	}

	rows, err := repo.ListExpiredUnnotified(now, 1)
	require.NoError(t, err)
	assert.Len(t, rows, 1)

	rows, err = repo.ListExpiredUnnotified(now, 0)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(rows), 3)
}

// TestPollRepository_BackfillNotifiedAt は migration 053 が含む backfill SQL
// (#1415) の挙動を guard する。Misskey TS から mk-go へ移行した際、000044 が
// notifiedAt = NULL でカラムを追加するため、既に expiresAt 経過後の poll は
// 軒並み「未通知」となり ExpiryWorker 初回 tick で一斉発火する不具合の修正。
//
// 053 は startup 時点で既に testDB に適用済みなので、ここでは SQL 自体を
// 再実行する形で「期限切れ未通知 → 通知済みに backfill される / 未満了は
// 触らない / 既に通知済みは触らない」契約を確認する。
func TestPollRepository_BackfillNotifiedAt(t *testing.T) {
	repo := NewPollRepository(testDB)
	user := insertTestUser(t, "u_bf_1", "bfuser")
	defer cleanupUser(t, user.ID)

	now := time.Now()
	past := now.Add(-2 * time.Hour)
	future := now.Add(time.Hour)
	already := past.Add(time.Minute)

	mk := func(id string, expires *time.Time, notified *time.Time) {
		note := &model.Note{ID: id, UserID: user.ID, Visibility: model.NoteVisibilityPublic, HasPoll: true, Reactions: datatypes.JSON([]byte("{}"))}
		require.NoError(t, testDB.Create(note).Error)
		t.Cleanup(func() { cleanupNote(t, note.ID) })
		require.NoError(t, repo.Create(&model.Poll{
			NoteID: id, Choices: model.StringArray{"a"}, Votes: model.Int64Array{0},
			NoteVisibility: model.NoteVisibilityPublic, UserID: user.ID,
			ExpiresAt:  expires,
			NotifiedAt: notified,
		}))
	}
	// 1) 期限切れ未通知 → backfill 対象
	mk("p_bf_target", &past, nil)
	// 2) 未満了 → 触らない
	mk("p_bf_future", &future, nil)
	// 3) 既に通知済み → 触らない
	mk("p_bf_already", &past, &already)
	// 4) expiresAt なし → 触らない
	mk("p_bf_noexpire", nil, nil)

	// 053 と同じ SQL を再実行する。冪等なので既に適用済みの DB でも結果は変わらない。
	require.NoError(t, testDB.Exec(
		`UPDATE "poll" SET "notifiedAt" = "expiresAt"
		  WHERE "expiresAt" IS NOT NULL AND "expiresAt" < NOW() AND "notifiedAt" IS NULL`,
	).Error)

	target, err := repo.FindByNoteID("p_bf_target")
	require.NoError(t, err)
	require.NotNil(t, target.NotifiedAt, "backfill 対象は notifiedAt が埋まる")
	assert.WithinDuration(t, past, *target.NotifiedAt, time.Second, "notifiedAt = expiresAt にコピーされる")

	future2, err := repo.FindByNoteID("p_bf_future")
	require.NoError(t, err)
	assert.Nil(t, future2.NotifiedAt, "未満了は触らない")

	already2, err := repo.FindByNoteID("p_bf_already")
	require.NoError(t, err)
	require.NotNil(t, already2.NotifiedAt)
	assert.WithinDuration(t, already, *already2.NotifiedAt, time.Second, "既に通知済みの timestamp は維持される")

	noexp, err := repo.FindByNoteID("p_bf_noexpire")
	require.NoError(t, err)
	assert.Nil(t, noexp.NotifiedAt, "expiresAt=NULL は触らない")
}

func TestPollRepository_MarkNotified(t *testing.T) {
	repo := NewPollRepository(testDB)
	user := insertTestUser(t, "u_mn_1", "mnuser")
	defer cleanupUser(t, user.ID)

	noteID := "n_mn_1"
	note := &model.Note{ID: noteID, UserID: user.ID, Visibility: model.NoteVisibilityPublic, HasPoll: true, Reactions: datatypes.JSON([]byte("{}"))}
	require.NoError(t, testDB.Create(note).Error)
	defer cleanupNote(t, noteID)

	expires := time.Now().Add(-time.Hour)
	require.NoError(t, repo.Create(&model.Poll{
		NoteID: noteID, Choices: model.StringArray{"a"}, Votes: model.Int64Array{0},
		NoteVisibility: model.NoteVisibilityPublic, UserID: user.ID,
		ExpiresAt: &expires,
	}))

	// 未通知状態
	pre, err := repo.FindByNoteID(noteID)
	require.NoError(t, err)
	assert.Nil(t, pre.NotifiedAt)

	now := time.Now()
	require.NoError(t, repo.MarkNotified(noteID, now))

	post, err := repo.FindByNoteID(noteID)
	require.NoError(t, err)
	require.NotNil(t, post.NotifiedAt)
	assert.WithinDuration(t, now, *post.NotifiedAt, time.Second)

	// 不在 noteID でも no-op (= no error)
	require.NoError(t, repo.MarkNotified("nonexistent", now))
}

func TestPollRepository_ListRecommendation(t *testing.T) {
	repo := NewPollRepository(testDB)
	viewer := insertTestUser(t, "u_lr_viewer", "lrviewer")
	defer cleanupUser(t, viewer.ID)
	author := insertTestUser(t, "u_lr_author", "lrauthor")
	defer cleanupUser(t, author.ID)
	muted := insertTestUser(t, "u_lr_muted", "lrmuted")
	defer cleanupUser(t, muted.ID)

	now := time.Now()
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	remoteHost := "remote.example"
	ch := "chan_lr_1"

	mk := func(id, userID string, vis model.NoteVisibility, host *string, expires *time.Time, channelID *string) {
		note := &model.Note{ID: id, UserID: userID, Visibility: vis, HasPoll: true, Reactions: datatypes.JSON([]byte("{}"))}
		require.NoError(t, testDB.Create(note).Error)
		t.Cleanup(func() { cleanupNote(t, id) })
		p := &model.Poll{
			NoteID: id, Multiple: false, Choices: model.StringArray{"a"}, Votes: model.Int64Array{0},
			NoteVisibility: vis, UserID: userID, UserHost: host, ExpiresAt: expires, ChannelID: channelID,
		}
		require.NoError(t, repo.Create(p))
	}
	mk("p_lr_good2", author.ID, model.NoteVisibilityPublic, nil, &future, nil)
	mk("p_lr_good1", author.ID, model.NoteVisibilityPublic, nil, nil, nil) // no expiry
	mk("p_lr_self", viewer.ID, model.NoteVisibilityPublic, nil, &future, nil)
	mk("p_lr_followers", author.ID, model.NoteVisibilityFollowers, nil, &future, nil)
	mk("p_lr_expired", author.ID, model.NoteVisibilityPublic, nil, &past, nil)
	mk("p_lr_remote", author.ID, model.NoteVisibilityPublic, &remoteHost, &future, nil)
	mk("p_lr_muted", muted.ID, model.NoteVisibilityPublic, nil, &future, nil)
	mk("p_lr_chan", author.ID, model.NoteVisibilityPublic, nil, &future, &ch)
	mk("p_lr_voted", author.ID, model.NoteVisibilityPublic, nil, &future, nil)
	require.NoError(t, testDB.Create(&model.PollVote{ID: "pv_lr_1", UserID: viewer.ID, NoteID: "p_lr_voted", Choice: 0}).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "poll_vote" WHERE id = ?`, "pv_lr_1") })

	ids, err := repo.ListRecommendation(viewer.ID, []string{muted.ID}, true, 10, 0)
	require.NoError(t, err)
	// good1/good2 のみ (self/followers/expired/remote/muted/channel/voted は除外)、noteId DESC。
	assert.Equal(t, []string{"p_lr_good2", "p_lr_good1"}, ids)

	// excludeChannels=false なら channel poll も含まれる。
	ids2, err := repo.ListRecommendation(viewer.ID, []string{muted.ID}, false, 10, 0)
	require.NoError(t, err)
	assert.Contains(t, ids2, "p_lr_chan")

	// limit/offset。
	page, err := repo.ListRecommendation(viewer.ID, []string{muted.ID}, true, 1, 1)
	require.NoError(t, err)
	assert.Equal(t, []string{"p_lr_good1"}, page)
}
