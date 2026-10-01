package repository

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func cleanupNote(t *testing.T, id string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "poll" WHERE "noteId" = ?`, id)
	testDB.Exec(`DELETE FROM "note" WHERE id = ?`, id)
}

// TestNoteRepository_ListHomeTimeline_MutedChannelFilter verifies that
// model.TimelineDBFilter.MutedChannelIDs is applied at the SQL layer without
// short-circuiting other AND conditions. The OR clause
// `channelId IS NULL OR channelId NOT IN (...)` must be wrapped in
// parentheses; otherwise AND conditions (visibility / following) are bypassed.
// Regression guard from Devin #191 review.
func TestNoteRepository_ListHomeTimeline_MutedChannelFilter(t *testing.T) {
	repo := NewNoteRepository(testDB)
	chRepo := NewChannelRepository(testDB)

	viewer := insertTestUser(t, "u_mute_v", "muteviewer")
	defer cleanupUser(t, viewer.ID)
	author := insertTestUser(t, "u_mute_a", "muteauthor")
	defer cleanupUser(t, author.ID)

	// viewer は author をフォロー (home timeline に含める)
	require.NoError(t, testDB.Exec(
		`INSERT INTO "following" (id, "followerId", "followeeId") VALUES (?, ?, ?)`,
		"flw_mute", viewer.ID, author.ID,
	).Error)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "flw_mute")

	mutedCh := newTestChannel("ch_mute_m", "muted-ch", nil)
	require.NoError(t, chRepo.Create(mutedCh))
	defer cleanupChannel(t, mutedCh.ID)
	allowedCh := newTestChannel("ch_mute_a", "allowed-ch", nil)
	require.NoError(t, chRepo.Create(allowedCh))
	defer cleanupChannel(t, allowedCh.ID)

	mkNote := func(id, chID string) *model.Note {
		text := "hi"
		n := &model.Note{
			ID: id, UserID: author.ID, Text: &text,
			Visibility: model.NoteVisibilityPublic,
			Reactions:  datatypes.JSON([]byte("{}")),
		}
		if chID != "" {
			n.ChannelID = &chID
		}
		return n
	}
	plain := mkNote("n_mute_1", "")
	inMuted := mkNote("n_mute_2", mutedCh.ID)
	inAllowed := mkNote("n_mute_3", allowedCh.ID)
	require.NoError(t, repo.Create(plain))
	require.NoError(t, repo.Create(inMuted))
	require.NoError(t, repo.Create(inAllowed))
	defer cleanupNote(t, plain.ID)
	defer cleanupNote(t, inMuted.ID)
	defer cleanupNote(t, inAllowed.ID)

	// #1686: channel note は viewer が follow している channel のものだけ home
	// に含まれる (follow user の channel note は除外)。両 channel を follow した
	// 上で、muted channel の note は MutedChannelIDs 側で除外されることを検証する
	// (= mute が channel-follow より優先)。
	filter := model.TimelineDBFilter{
		ViewerID:           viewer.ID,
		MutedChannelIDs:    []string{mutedCh.ID},
		FollowedChannelIDs: []string{mutedCh.ID, allowedCh.ID},
	}
	rows, err := repo.ListHomeTimeline(viewer.ID, 50, "", "", filter)
	require.NoError(t, err)

	ids := make(map[string]bool, len(rows))
	for _, n := range rows {
		ids[n.ID] = true
	}
	assert.True(t, ids[plain.ID], "plain note must be included")
	assert.True(t, ids[inAllowed.ID], "note in followed allowed channel must be included")
	assert.False(t, ids[inMuted.ID], "note in muted channel must be excluded even when followed")
}

// #1681 applyTimelineFilter の被block / reply著者mute / instance-mute を
// ListHomeTimeline (SQL push-down) で検証する。
func TestNoteRepository_ListHomeTimeline_BaseFilters(t *testing.T) {
	repo := NewNoteRepository(testDB)
	viewer := insertTestUser(t, "u_bf_v", "bfviewer")
	author := insertTestUser(t, "u_bf_a", "bfauthor")
	blocker := insertTestUser(t, "u_bf_b", "bfblocker")
	muted := insertTestUser(t, "u_bf_m", "bfmuted")
	defer cleanupUser(t, viewer.ID)
	defer cleanupUser(t, author.ID)
	defer cleanupUser(t, blocker.ID)
	defer cleanupUser(t, muted.ID)

	// viewer は author / blocker / muted をフォロー (home timeline 対象)。
	for i, fid := range []string{author.ID, blocker.ID, muted.ID} {
		flid := "flw_bf_" + string(rune('a'+i))
		require.NoError(t, testDB.Exec(`INSERT INTO "following" (id, "followerId", "followeeId") VALUES (?, ?, ?)`, flid, viewer.ID, fid).Error)
		defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, flid)
	}

	txt := "hi"
	mkNote := func(n *model.Note) *model.Note {
		n.Text = &txt
		n.Visibility = model.NoteVisibilityPublic
		n.Reactions = datatypes.JSON([]byte("{}"))
		require.NoError(t, repo.Create(n))
		t.Cleanup(func() { cleanupNote(t, n.ID) })
		return n
	}
	plain := mkNote(&model.Note{ID: "n_bf_plain", UserID: author.ID})
	byBlocker := mkNote(&model.Note{ID: "n_bf_blk", UserID: blocker.ID})
	replyToMuted := mkNote(&model.Note{ID: "n_bf_rep", UserID: author.ID, ReplyID: strPtr2("n_bf_plain"), ReplyUserID: &muted.ID})
	remoteHost := "bad.example"
	fromBadInstance := mkNote(&model.Note{ID: "n_bf_inst", UserID: author.ID, UserHost: &remoteHost})
	// 大文字混じり host も LOWER() 突合で除外されること (#1681 review)。
	upperHost := "Bad.Example"
	fromUpperInstance := mkNote(&model.Note{ID: "n_bf_inst_up", UserID: author.ID, UserHost: &upperHost})

	filter := model.TimelineDBFilter{
		ViewerID:       viewer.ID,
		BlockerIDs:     []string{blocker.ID},
		MutedInstances: []string{"bad.example"},
		MutedUserIDs:   []string{muted.ID}, // literal path で reply著者mute を検証
	}
	rows, err := repo.ListHomeTimeline(viewer.ID, 50, "", "", filter)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, n := range rows {
		ids[n.ID] = true
	}
	assert.True(t, ids[plain.ID], "通常 note は含まれる")
	assert.False(t, ids[byBlocker.ID], "被block author の note は除外")
	assert.False(t, ids[replyToMuted.ID], "muted user 宛 reply は除外")
	assert.False(t, ids[fromBadInstance.ID], "muted instance の note は除外")
	assert.False(t, ids[fromUpperInstance.ID], "大文字 host も LOWER() で除外")
}

func strPtr2(s string) *string { return &s }

// TestNoteRepository_ListHomeTimeline_SuspendedFilter は #1686 の suspended-user
// filter を SQL 層で検証する。note/reply/renote のいずれかの author が
// suspended なら除外される (upstream generateSuspendedUserQueryForNote)。
func TestNoteRepository_ListHomeTimeline_SuspendedFilter(t *testing.T) {
	repo := NewNoteRepository(testDB)
	viewer := insertTestUser(t, "u_sus_v", "susviewer")
	normal := insertTestUser(t, "u_sus_n", "susnormal")
	susp := insertTestUser(t, "u_sus_s", "suspended")
	defer cleanupUser(t, viewer.ID)
	defer cleanupUser(t, normal.ID)
	defer cleanupUser(t, susp.ID)
	require.NoError(t, testDB.Exec(`UPDATE "user" SET "isSuspended" = true WHERE id = ?`, susp.ID).Error)

	// viewer は normal / susp をフォロー (home timeline 対象)。
	for i, fid := range []string{normal.ID, susp.ID} {
		flid := "flw_sus_" + string(rune('a'+i))
		require.NoError(t, testDB.Exec(`INSERT INTO "following" (id, "followerId", "followeeId") VALUES (?, ?, ?)`, flid, viewer.ID, fid).Error)
		defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, flid)
	}

	txt := "hi"
	mkNote := func(n *model.Note) *model.Note {
		n.Text = &txt
		n.Visibility = model.NoteVisibilityPublic
		n.Reactions = datatypes.JSON([]byte("{}"))
		require.NoError(t, repo.Create(n))
		t.Cleanup(func() { cleanupNote(t, n.ID) })
		return n
	}
	plain := mkNote(&model.Note{ID: "n_sus_plain", UserID: normal.ID})
	bySusp := mkNote(&model.Note{ID: "n_sus_own", UserID: susp.ID})
	replyToSusp := mkNote(&model.Note{ID: "n_sus_rep", UserID: normal.ID, ReplyID: strPtr2("n_sus_own"), ReplyUserID: &susp.ID})
	renoteOfSusp := mkNote(&model.Note{ID: "n_sus_rnt", UserID: normal.ID, RenoteID: strPtr2("n_sus_own"), RenoteUserID: &susp.ID})

	rows, err := repo.ListHomeTimeline(viewer.ID, 50, "", "", model.TimelineDBFilter{ViewerID: viewer.ID})
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, n := range rows {
		ids[n.ID] = true
	}
	assert.True(t, ids[plain.ID], "非 suspended author の note は含まれる")
	assert.False(t, ids[bySusp.ID], "suspended author の note は除外")
	assert.False(t, ids[replyToSusp.ID], "suspended user 宛 reply は除外")
	assert.False(t, ids[renoteOfSusp.ID], "suspended user の renote/quote は除外")
}

// TestNoteRepository_ListHomeTimeline_FollowedChannels は #1686 の home channel
// 包含を検証する。follow 中 channel の note は home に含め、follow 中 user が
// channel に投稿した note は (その channel を follow していない限り) home から
// 除外される (upstream timeline.ts の followingChannelIds 分岐)。
func TestNoteRepository_ListHomeTimeline_FollowedChannels(t *testing.T) {
	repo := NewNoteRepository(testDB)
	viewer := insertTestUser(t, "u_hc_v", "hcviewer")
	fu := insertTestUser(t, "u_hc_fu", "hcfollowed")
	x := insertTestUser(t, "u_hc_x", "hcother")
	defer cleanupUser(t, viewer.ID)
	defer cleanupUser(t, fu.ID)
	defer cleanupUser(t, x.ID)

	// viewer は fu のみ user-follow する。
	require.NoError(t, testDB.Exec(`INSERT INTO "following" (id, "followerId", "followeeId") VALUES (?, ?, ?)`, "flw_hc", viewer.ID, fu.ID).Error)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "flw_hc")

	txt := "hi"
	chFollowed := "ch_followed"
	chOther := "ch_other"
	mkNote := func(id, uid string, channelID *string) *model.Note {
		n := &model.Note{ID: id, UserID: uid, Text: &txt, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")), ChannelID: channelID}
		require.NoError(t, repo.Create(n))
		t.Cleanup(func() { cleanupNote(t, id) })
		return n
	}
	plain := mkNote("n_hc_plain", fu.ID, nil)                   // followed user の通常 note
	fuInOther := mkNote("n_hc_fuoth", fu.ID, &chOther)          // followed user の非follow channel note
	inFollowed := mkNote("n_hc_inf", x.ID, &chFollowed)         // follow 中 channel の note (投稿者は未follow)
	inOther := mkNote("n_hc_inoth", x.ID, &chOther)             // 非follow channel の note
	ownInFollowed := mkNote("n_hc_own", viewer.ID, &chFollowed) // 自分の follow 中 channel note

	collect := func(followed []string) map[string]bool {
		rows, err := repo.ListHomeTimeline(viewer.ID, 50, "", "", model.TimelineDBFilter{ViewerID: viewer.ID, FollowedChannelIDs: followed})
		require.NoError(t, err)
		ids := map[string]bool{}
		for _, n := range rows {
			ids[n.ID] = true
		}
		return ids
	}

	// follow 中 channel あり: follow channel note は含む、follow user の channel
	// note (非follow channel) は除外。
	withCh := collect([]string{chFollowed})
	assert.True(t, withCh[plain.ID], "通常 note は含まれる")
	assert.False(t, withCh[fuInOther.ID], "follow user の非follow channel note は除外")
	assert.True(t, withCh[inFollowed.ID], "follow 中 channel の note は含まれる")
	assert.False(t, withCh[inOther.ID], "非follow channel の note は除外")
	assert.True(t, withCh[ownInFollowed.ID], "自分の follow 中 channel note は含まれる")

	// follow 中 channel なし: 全 channel note 除外、通常 note のみ。
	noCh := collect(nil)
	assert.True(t, noCh[plain.ID], "通常 note は含まれる")
	assert.False(t, noCh[fuInOther.ID], "channel note は全除外")
	assert.False(t, noCh[inFollowed.ID], "channel note は全除外")
	assert.False(t, noCh[ownInFollowed.ID], "channel note は全除外")
}

// TestNoteRepository_ListHomeTimeline_WithRepliesFalse_SelfThreadOnly は #1047
// で導入された upstream 互換の reply filter semantics を SQL 層で直接検証する。
//
// 期待値:
//   - reply 無し note: 通過
//   - self-thread (replyUserId = note.userId): 通過
//   - 他人宛 reply (replyUserId != note.userId): 除外
//
// ViewerID 引数の有無に関わらず同 semantics となる (旧実装の
// `replyUserId = viewerID` 分岐は撤廃済)。
func TestNoteRepository_ListHomeTimeline_WithRepliesFalse_SelfThreadOnly(t *testing.T) {
	repo := NewNoteRepository(testDB)

	viewer := insertTestUser(t, "u_rep_v", "repviewer")
	defer cleanupUser(t, viewer.ID)
	author := insertTestUser(t, "u_rep_a", "repauthor")
	defer cleanupUser(t, author.ID)
	other := insertTestUser(t, "u_rep_o", "repother")
	defer cleanupUser(t, other.ID)

	// viewer は author をフォロー (home timeline に含める)
	require.NoError(t, testDB.Exec(
		`INSERT INTO "following" (id, "followerId", "followeeId") VALUES (?, ?, ?)`,
		"flw_rep", viewer.ID, author.ID,
	).Error)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "flw_rep")

	text := "hi"
	plain := &model.Note{
		ID: "n_rep_plain", UserID: author.ID, Text: &text,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	// self-thread: author 自身への reply
	selfReplyTargetID := "n_rep_plain"
	selfThread := &model.Note{
		ID: "n_rep_self", UserID: author.ID, Text: &text,
		Visibility:  model.NoteVisibilityPublic,
		Reactions:   datatypes.JSON([]byte("{}")),
		ReplyID:     &selfReplyTargetID,
		ReplyUserID: &author.ID,
	}
	// 他人宛 reply: author → other
	otherReplyTargetID := "n_rep_target_other"
	otherUserID := other.ID
	otherReplyTarget := &model.Note{
		ID: otherReplyTargetID, UserID: other.ID, Text: &text,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	otherReply := &model.Note{
		ID: "n_rep_other", UserID: author.ID, Text: &text,
		Visibility:  model.NoteVisibilityPublic,
		Reactions:   datatypes.JSON([]byte("{}")),
		ReplyID:     &otherReplyTargetID,
		ReplyUserID: &otherUserID,
	}
	require.NoError(t, repo.Create(plain))
	require.NoError(t, repo.Create(otherReplyTarget))
	require.NoError(t, repo.Create(selfThread))
	require.NoError(t, repo.Create(otherReply))
	defer cleanupNote(t, plain.ID)
	defer cleanupNote(t, otherReplyTarget.ID)
	defer cleanupNote(t, selfThread.ID)
	defer cleanupNote(t, otherReply.ID)

	withReplies := false
	filter := model.TimelineDBFilter{
		ViewerID:    viewer.ID,
		WithReplies: &withReplies,
	}
	rows, err := repo.ListHomeTimeline(viewer.ID, 50, "", "", filter)
	require.NoError(t, err)

	ids := make(map[string]bool, len(rows))
	for _, n := range rows {
		ids[n.ID] = true
	}
	assert.True(t, ids[plain.ID], "plain note (reply 無し) は通過すべき")
	assert.True(t, ids[selfThread.ID], "self-thread (replyUserId = userId) は通過すべき")
	assert.False(t, ids[otherReply.ID], "他人宛 reply (replyUserId != userId) は除外されるべき")
}

func TestNoteRepository_CreateAndFindByID(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_ncf_1", "noteuser1")
	defer cleanupUser(t, user.ID)

	text := "Hello, world!"
	note := &model.Note{
		ID:         "n_ncf_1",
		UserID:     user.ID,
		Text:       &text,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(note))
	defer cleanupNote(t, note.ID)

	found, err := repo.FindByID(note.ID)
	require.NoError(t, err)
	assert.Equal(t, note.ID, found.ID)
	assert.Equal(t, &text, found.Text)
}

func TestNoteRepository_FindByIDWithUser(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_nfu_1", "noteuser2")
	defer cleanupUser(t, user.ID)

	note := &model.Note{
		ID:         "n_nfu_1",
		UserID:     user.ID,
		Visibility: model.NoteVisibilityHome,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(note))
	defer cleanupNote(t, note.ID)

	found, err := repo.FindByIDWithUser(note.ID)
	require.NoError(t, err)
	assert.NotNil(t, found.User)
	assert.Equal(t, user.ID, found.User.ID)
	assert.Equal(t, "noteuser2", found.User.Username)
	// 軽量経路 (#425) では Renote/Reply は preload されない。
	assert.Nil(t, found.Renote)
	assert.Nil(t, found.Reply)
}

// FindByIDWithRelations は User に加えて Renote / Renote.User / Reply /
// Reply.User まで preload する full version (#425)。
func TestNoteRepository_FindByIDWithRelations(t *testing.T) {
	repo := NewNoteRepository(testDB)
	author := insertTestUser(t, "u_nfr_a", "nfra")
	defer cleanupUser(t, author.ID)
	rnAuthor := insertTestUser(t, "u_nfr_r", "nfrr")
	defer cleanupUser(t, rnAuthor.ID)

	renoteID := "n_nfr_rn"
	renoteTarget := &model.Note{
		ID:         renoteID,
		UserID:     rnAuthor.ID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(renoteTarget))
	defer cleanupNote(t, renoteTarget.ID)

	parent := &model.Note{
		ID:         "n_nfr_main",
		UserID:     author.ID,
		RenoteID:   &renoteID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(parent))
	defer cleanupNote(t, parent.ID)

	found, err := repo.FindByIDWithRelations(parent.ID)
	require.NoError(t, err)
	require.NotNil(t, found.User)
	assert.Equal(t, author.ID, found.User.ID)
	require.NotNil(t, found.Renote, "Renote must be preloaded")
	assert.Equal(t, renoteID, found.Renote.ID)
	require.NotNil(t, found.Renote.User, "Renote.User must be preloaded")
	assert.Equal(t, rnAuthor.ID, found.Renote.User.ID)
}

func TestNoteRepository_FindByIDWithRelations_NotFound(t *testing.T) {
	repo := NewNoteRepository(testDB)
	_, err := repo.FindByIDWithRelations("nonexistent_note_relations")
	assert.Error(t, err)
}

func TestNoteRepository_UpdateFields(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_nuf_1", "noteuser_uf")
	defer cleanupUser(t, user.ID)

	text := "original"
	note := &model.Note{
		ID:         "n_nuf_1",
		UserID:     user.ID,
		Text:       &text,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(note))
	defer cleanupNote(t, note.ID)

	updated := "edited"
	cw := "spoiler"
	require.NoError(t, repo.UpdateFields(note.ID, map[string]any{
		"text": &updated,
		"cw":   &cw,
	}))

	got, err := repo.FindByID(note.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Text)
	assert.Equal(t, "edited", *got.Text)
	require.NotNil(t, got.CW)
	assert.Equal(t, "spoiler", *got.CW)
}

func TestNoteRepository_UpdateFields_NoOp(t *testing.T) {
	repo := NewNoteRepository(testDB)
	require.NoError(t, repo.UpdateFields("any", nil))
}

func TestNoteRepository_ListByChannelID(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_nlc_1", "noteuser_lc")
	defer cleanupUser(t, user.ID)

	chRepo := NewChannelRepository(testDB)
	uid := user.ID
	ch := newTestChannel("ch_lc_1", "list-by-channel", &uid)
	require.NoError(t, chRepo.Create(ch))
	defer cleanupChannel(t, ch.ID)

	cid := ch.ID
	for _, id := range []string{"n_lc_1", "n_lc_2", "n_lc_3"} {
		note := &model.Note{
			ID:         id,
			UserID:     user.ID,
			Visibility: model.NoteVisibilityPublic,
			Reactions:  datatypes.JSON([]byte("{}")),
			ChannelID:  &cid,
		}
		require.NoError(t, repo.Create(note))
		defer cleanupNote(t, note.ID)
	}

	// すべて public note なので匿名 viewer (viewerID="") でも全件見える。
	// visibility push-down (#1440 / #1449) の matrix は別 test で確認する。
	rows, err := repo.ListByChannelID(ch.ID, "", "", "", 10)
	require.NoError(t, err)
	assert.Len(t, rows, 3)

	rows, err = repo.ListByChannelID(ch.ID, "", "n_lc_3", "", 10)
	require.NoError(t, err)
	assert.Len(t, rows, 2)

	rows, err = repo.ListByChannelID(ch.ID, "", "", "n_lc_1", 10)
	require.NoError(t, err)
	assert.Len(t, rows, 2)

	rows, err = repo.ListByChannelID(ch.ID, "", "", "", 0) // 0 → default 30
	require.NoError(t, err)
	assert.Len(t, rows, 3)
}

// TestNoteRepository_ListByChannelID_Visibility は channels/timeline の visibility
// push-down (#1440 IDOR) を SQL 段階で検証する。public channel に
// followers / specified visibility の note を混在させ、viewer ごとに見える
// note が変わることを matrix で確認する (#1449 review で signature 統合)。
func TestNoteRepository_ListByChannelID_Visibility(t *testing.T) {
	repo := NewNoteRepository(testDB)
	chRepo := NewChannelRepository(testDB)
	followingRepo := NewFollowingRepository(testDB)

	mkUser := func(id, username string) *model.User {
		u := insertTestUser(t, id, username)
		t.Cleanup(func() { cleanupUser(t, u.ID) })
		return u
	}
	author := mkUser("u_cv_a", "cvauthor")
	follower := mkUser("u_cv_f", "cvfollower")
	allowed := mkUser("u_cv_al", "cvallowed")

	uid := author.ID
	ch := newTestChannel("ch_cv_1", "channel-vis", &uid)
	require.NoError(t, chRepo.Create(ch))
	defer cleanupChannel(t, ch.ID)

	cid := ch.ID
	notes := []*model.Note{
		{ID: "n_cv_pub", UserID: author.ID, ChannelID: &cid, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_cv_fol", UserID: author.ID, ChannelID: &cid, Visibility: model.NoteVisibilityFollowers, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_cv_spec", UserID: author.ID, ChannelID: &cid, Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: model.StringArray{allowed.ID}, Reactions: datatypes.JSON([]byte("{}"))},
	}
	for _, n := range notes {
		require.NoError(t, repo.Create(n))
		defer cleanupNote(t, n.ID)
	}

	f := &model.Following{ID: "fl_cv_1", FollowerID: follower.ID, FolloweeID: author.ID}
	require.NoError(t, followingRepo.Create(f))
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, f.ID)

	idsOf := func(rows []*model.Note) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.ID)
		}
		sort.Strings(out)
		return out
	}

	// 匿名 viewer: public のみ。followers / specified が漏れない (#1440)。
	rows, err := repo.ListByChannelID(ch.ID, "", "", "", 50)
	require.NoError(t, err)
	assert.Equal(t, []string{"n_cv_pub"}, idsOf(rows))

	// 非 follower viewer に followers note は見えない。
	rows, err = repo.ListByChannelID(ch.ID, "u_cv_stranger", "", "", 50)
	require.NoError(t, err)
	assert.Equal(t, []string{"n_cv_pub"}, idsOf(rows))

	// follower viewer は public + followers。
	rows, err = repo.ListByChannelID(ch.ID, follower.ID, "", "", 50)
	require.NoError(t, err)
	assert.Equal(t, []string{"n_cv_fol", "n_cv_pub"}, idsOf(rows))

	// specified 対象 viewer は public + specified。
	rows, err = repo.ListByChannelID(ch.ID, allowed.ID, "", "", 50)
	require.NoError(t, err)
	assert.Equal(t, []string{"n_cv_pub", "n_cv_spec"}, idsOf(rows))

	// author 本人は全 visibility を閲覧可。
	rows, err = repo.ListByChannelID(ch.ID, author.ID, "", "", 50)
	require.NoError(t, err)
	assert.Equal(t, []string{"n_cv_fol", "n_cv_pub", "n_cv_spec"}, idsOf(rows))
}

func TestNoteRepository_FindByURI(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_nuri_1", "noteuser_uri")
	defer cleanupUser(t, user.ID)

	uri := "https://remote.example/notes/n_nuri_1"
	note := &model.Note{
		ID:         "n_nuri_1",
		UserID:     user.ID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
		URI:        &uri,
	}
	require.NoError(t, repo.Create(note))
	defer cleanupNote(t, note.ID)

	found, err := repo.FindByURI(uri)
	require.NoError(t, err)
	assert.Equal(t, note.ID, found.ID)

	// 存在しない URI なら error
	_, err = repo.FindByURI("https://remote.example/notes/missing")
	assert.Error(t, err)
}

func TestNoteRepository_Delete(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_nd_1", "noteuser3")
	defer cleanupUser(t, user.ID)

	note := &model.Note{
		ID:         "n_nd_1",
		UserID:     user.ID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(note))

	require.NoError(t, repo.Delete(note))

	_, err := repo.FindByID(note.ID)
	assert.Error(t, err)
}

func TestNoteRepository_Update(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_nu_1", "noteuser4")
	defer cleanupUser(t, user.ID)

	note := &model.Note{
		ID:         "n_nu_1",
		UserID:     user.ID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(note))
	defer cleanupNote(t, note.ID)

	require.NoError(t, repo.Update(note, "hasPoll", true))

	found, err := repo.FindByID(note.ID)
	require.NoError(t, err)
	assert.True(t, found.HasPoll)
}

func TestNoteRepository_FindByID_NotFound(t *testing.T) {
	repo := NewNoteRepository(testDB)

	_, err := repo.FindByID("nonexistent_note")
	assert.Error(t, err)
}

func TestNoteRepository_FindByIDWithUser_NotFound(t *testing.T) {
	repo := NewNoteRepository(testDB)

	_, err := repo.FindByIDWithUser("nonexistent_note")
	assert.Error(t, err)
}

func TestNoteRepository_ListByUserID(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_lst_1", "listuser")
	defer cleanupUser(t, user.ID)

	for _, id := range []string{"n_lst_1", "n_lst_2", "n_lst_3"} {
		note := &model.Note{
			ID:         id,
			UserID:     user.ID,
			Visibility: model.NoteVisibilityPublic,
			Reactions:  datatypes.JSON([]byte("{}")),
		}
		require.NoError(t, repo.Create(note))
		defer cleanupNote(t, id)
	}

	// 全件 (id降順)
	out, err := repo.ListByUserID(user.ID, "", "", 10)
	require.NoError(t, err)
	assert.Len(t, out, 3)
	assert.Equal(t, "n_lst_3", out[0].ID)

	// untilIDで絞り込み
	out, err = repo.ListByUserID(user.ID, "n_lst_3", "", 10)
	require.NoError(t, err)
	assert.Len(t, out, 2)

	// sinceIDで絞り込み
	out, err = repo.ListByUserID(user.ID, "", "n_lst_1", 10)
	require.NoError(t, err)
	assert.Len(t, out, 2)
}

// ListByUserIDFiltered は upstream users/notes 互換の 4 種 filter (#1021)。
func TestNoteRepository_ListByUserIDFiltered(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_lf_1", "lfuser")
	defer cleanupUser(t, user.ID)

	// 4 種類のノートを seed (file 添付 / reply / pure renote / 通常)。
	text := "plain"
	notes := []*model.Note{
		{ID: "n_lf_plain", UserID: user.ID, Text: &text, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")), FileIDs: model.StringArray{}},
		{ID: "n_lf_file", UserID: user.ID, Text: &text, FileIDs: model.StringArray{"f1"}, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))},
	}
	for _, n := range notes {
		require.NoError(t, repo.Create(n))
		defer cleanupNote(t, n.ID)
	}

	// withFiles=false の defaults (= withReplies=true / withRenotes=true /
	// withChannelNotes=false)。channel=false の filter は本ケースで影響なし。
	out, err := repo.ListByUserIDFiltered(user.ID, "", "", "", 10, false, true, true, false)
	require.NoError(t, err)
	assert.Len(t, out, 2)

	// withFiles=true で file 添付のみ
	out, err = repo.ListByUserIDFiltered(user.ID, "", "", "", 10, true, true, true, false)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "n_lf_file", out[0].ID)
}

// withRenotes=false は pure renote のみ除外し、cw/poll/reply を伴う quote renote は
// 残す (#1888、pureRenoteCondSQL が cw/poll/reply を考慮)。
func TestNoteRepository_ListByUserIDFiltered_WithRenotesFalseKeepsQuotes(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_wrq", "wrquser")
	defer cleanupUser(t, user.ID)

	mk := func(id string, mutate func(n *model.Note)) {
		n := &model.Note{ID: id, UserID: user.ID, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))}
		mutate(n)
		require.NoError(t, repo.Create(n))
		t.Cleanup(func() { cleanupNote(t, id) })
	}
	mk("n_wrq_target", func(n *model.Note) {})
	tid := "n_wrq_target"
	mk("n_wrq_pure", func(n *model.Note) { n.RenoteID = &tid })
	mk("n_wrq_cw", func(n *model.Note) { n.RenoteID = &tid; cw := "w"; n.CW = &cw })
	mk("n_wrq_poll", func(n *model.Note) { n.RenoteID = &tid; n.HasPoll = true })
	mk("n_wrq_reply", func(n *model.Note) { n.RenoteID = &tid; n.ReplyID = &tid })

	// withRenotes=false (3rd bool)。pure renote のみ除外される。
	out, err := repo.ListByUserIDFiltered(user.ID, "", "", "", 20, false, true, false, false)
	require.NoError(t, err)
	got := idSet(out)
	assert.True(t, got["n_wrq_target"])
	assert.True(t, got["n_wrq_cw"], "cw renote は quote なので残る")
	assert.True(t, got["n_wrq_poll"], "poll renote は quote なので残る")
	assert.True(t, got["n_wrq_reply"], "reply renote は quote なので残る")
	assert.NotContains(t, got, "n_wrq_pure", "pure renote は除外される")
}

// TestNoteRepository_ListByUserIDFiltered_VisibilityPushDown は users/notes の
// visibility push-down (#1418 review) を検証する。author が public / followers /
// specified の 3 note を持ち、viewer ごとに SQL 段階で見える note が変わること
// を確認する (LIMIT 前に絞られるためページネーションが途切れない)。
func TestNoteRepository_ListByUserIDFiltered_VisibilityPushDown(t *testing.T) {
	repo := NewNoteRepository(testDB)
	followingRepo := NewFollowingRepository(testDB)

	mkUser := func(id, username string) *model.User {
		u := insertTestUser(t, id, username)
		t.Cleanup(func() { cleanupUser(t, u.ID) })
		return u
	}
	author := mkUser("u_lfv_a", "lfvauthor")
	follower := mkUser("u_lfv_f", "lfvfollower")
	allowed := mkUser("u_lfv_al", "lfvallowed")
	stranger := mkUser("u_lfv_s", "lfvstranger")

	notes := []*model.Note{
		{ID: "n_lfv_pub", UserID: author.ID, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")), FileIDs: model.StringArray{}},
		{ID: "n_lfv_fol", UserID: author.ID, Visibility: model.NoteVisibilityFollowers, Reactions: datatypes.JSON([]byte("{}")), FileIDs: model.StringArray{}},
		{ID: "n_lfv_spec", UserID: author.ID, Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: model.StringArray{allowed.ID}, Reactions: datatypes.JSON([]byte("{}")), FileIDs: model.StringArray{}},
	}
	for _, n := range notes {
		require.NoError(t, repo.Create(n))
		defer cleanupNote(t, n.ID)
	}

	f := &model.Following{ID: "fl_lfv_1", FollowerID: follower.ID, FolloweeID: author.ID}
	require.NoError(t, followingRepo.Create(f))
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, f.ID)

	idsOf := func(rows []*model.Note) []string {
		out := make([]string, 0, len(rows))
		for _, n := range rows {
			out = append(out, n.ID)
		}
		sort.Strings(out)
		return out
	}

	// 匿名 viewer は public のみ。
	out, err := repo.ListByUserIDFiltered(author.ID, "", "", "", 50, false, true, true, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"n_lfv_pub"}, idsOf(out))

	// stranger (follow なし / specified 対象外) も public のみ。
	out, err = repo.ListByUserIDFiltered(author.ID, stranger.ID, "", "", 50, false, true, true, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"n_lfv_pub"}, idsOf(out))

	// follower は public + followers。
	out, err = repo.ListByUserIDFiltered(author.ID, follower.ID, "", "", 50, false, true, true, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"n_lfv_fol", "n_lfv_pub"}, idsOf(out))

	// specified の visibleUserIds に含まれる viewer は public + specified。
	out, err = repo.ListByUserIDFiltered(author.ID, allowed.ID, "", "", 50, false, true, true, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"n_lfv_pub", "n_lfv_spec"}, idsOf(out))

	// author 本人は全 visibility を閲覧可。
	out, err = repo.ListByUserIDFiltered(author.ID, author.ID, "", "", 50, false, true, true, false)
	require.NoError(t, err)
	assert.Equal(t, []string{"n_lfv_fol", "n_lfv_pub", "n_lfv_spec"}, idsOf(out))
}

func TestNoteRepository_ListByUserIDFiltered_QueryError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	repo := NewNoteRepository(db)
	_, err := repo.ListByUserIDFiltered("a", "", "", "", 10, true, true, true, false)
	assert.Error(t, err)
}

func TestNoteRepository_FindManyByIDsWithUser(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_lst_2", "listuser2")
	defer cleanupUser(t, user.ID)

	for _, id := range []string{"n_fm_1", "n_fm_2"} {
		note := &model.Note{
			ID:         id,
			UserID:     user.ID,
			Visibility: model.NoteVisibilityPublic,
			Reactions:  datatypes.JSON([]byte("{}")),
		}
		require.NoError(t, repo.Create(note))
		defer cleanupNote(t, id)
	}

	out, err := repo.FindManyByIDsWithUser([]string{"n_fm_2", "n_fm_1", "ghost"})
	require.NoError(t, err)
	assert.Len(t, out, 2)
	// 順序がidsの順序を保つ
	assert.Equal(t, "n_fm_2", out[0].ID)
	assert.Equal(t, "n_fm_1", out[1].ID)

	// 空配列
	out, err = repo.FindManyByIDsWithUser(nil)
	require.NoError(t, err)
	assert.Empty(t, out)
}

// TestNoteRepository_FindManyByIDsWithUser_HydratesRelations は hydrated shape
// (User / Renote{.User,.Poll} / Reply{.User,.Poll} / Poll) を全て検証する強い
// oracle。hydration の実装を入れ替える (preload → batch 等) 際の wiring 回帰を
// 捕まえるためのもの。
func TestNoteRepository_FindManyByIDsWithUser_HydratesRelations(t *testing.T) {
	repo := NewNoteRepository(testDB)
	pollRepo := NewPollRepository(testDB)

	ua := insertTestUser(t, "u_hy_a", "hy_author")
	ur := insertTestUser(t, "u_hy_r", "hy_renoter")
	up := insertTestUser(t, "u_hy_p", "hy_replier")
	defer cleanupUser(t, ua.ID)
	defer cleanupUser(t, ur.ID)
	defer cleanupUser(t, up.ID)

	mkNote := func(id, uid string, hasPoll bool) *model.Note {
		n := &model.Note{ID: id, UserID: uid, Visibility: model.NoteVisibilityPublic, HasPoll: hasPoll, Reactions: datatypes.JSON([]byte("{}"))}
		require.NoError(t, repo.Create(n))
		t.Cleanup(func() { cleanupNote(t, id) })
		return n
	}
	mkPoll := func(noteID, uid string) {
		require.NoError(t, pollRepo.Create(&model.Poll{
			NoteID: noteID, Choices: model.StringArray{"A", "B"}, Votes: model.Int64Array{0, 0},
			NoteVisibility: model.NoteVisibilityPublic, UserID: uid,
		}))
	}

	// renote 先 / reply 先はそれぞれ別 user + poll を持つ。
	nr := mkNote("n_hy_r", ur.ID, true)
	mkPoll(nr.ID, ur.ID)
	np := mkNote("n_hy_p", up.ID, true)
	mkPoll(np.ID, up.ID)
	// main note: author=ua, renote=nr, reply=np, 自身も poll を持つ。
	main := &model.Note{
		ID: "n_hy_m", UserID: ua.ID, Visibility: model.NoteVisibilityPublic,
		HasPoll: true, RenoteID: &nr.ID, ReplyID: &np.ID, Reactions: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(main))
	t.Cleanup(func() { cleanupNote(t, main.ID) })
	mkPoll(main.ID, ua.ID)

	out, err := repo.FindManyByIDsWithUser([]string{main.ID})
	require.NoError(t, err)
	require.Len(t, out, 1)
	got := out[0]

	// author + 自 poll
	require.NotNil(t, got.User, "author user must be hydrated")
	assert.Equal(t, ua.ID, got.User.ID)
	require.NotNil(t, got.Poll, "own poll must be hydrated")
	assert.Equal(t, main.ID, got.Poll.NoteID)

	// renote + その user + poll
	require.NotNil(t, got.Renote, "renote must be hydrated")
	assert.Equal(t, nr.ID, got.Renote.ID)
	require.NotNil(t, got.Renote.User, "renote.user must be hydrated")
	assert.Equal(t, ur.ID, got.Renote.User.ID)
	require.NotNil(t, got.Renote.Poll, "renote.poll must be hydrated")
	assert.Equal(t, nr.ID, got.Renote.Poll.NoteID)

	// reply + その user + poll
	require.NotNil(t, got.Reply, "reply must be hydrated")
	assert.Equal(t, np.ID, got.Reply.ID)
	require.NotNil(t, got.Reply.User, "reply.user must be hydrated")
	assert.Equal(t, up.ID, got.Reply.User.ID)
	require.NotNil(t, got.Reply.Poll, "reply.poll must be hydrated")
	assert.Equal(t, np.ID, got.Reply.Poll.NoteID)
}

// 純粋リノート (boost) が引用投稿 (quote) を包む場合、timeline batch loader が
// renote embed の renote (= 引用先) を 2 段 hydrate すること。これが無いと frontend
// で引用先が「削除されたノート」になる (timeline bug)。packer は renote.renote のみ
// depth-2 で出す (renote.reply は出さない) ので、reply は 2 段目を hydrate しない
// ことも併せて検証する。バッチに renote を持たない note (引用先自身) も混ぜ、
// level-2 の RenoteID==nil 分岐も網羅する。
func TestNoteRepository_FindManyByIDsWithUser_HydratesNestedRenote(t *testing.T) {
	repo := NewNoteRepository(testDB)

	ut := insertTestUser(t, "u_nr_t", "nr_target")
	uq := insertTestUser(t, "u_nr_q", "nr_quoter")
	ub := insertTestUser(t, "u_nr_b", "nr_booster")
	urp := insertTestUser(t, "u_nr_rp", "nr_replytgt")
	defer cleanupUser(t, ut.ID)
	defer cleanupUser(t, uq.ID)
	defer cleanupUser(t, ub.ID)
	defer cleanupUser(t, urp.ID)

	mkNote := func(id, uid string, renoteID, replyID, text *string) *model.Note {
		n := &model.Note{ID: id, UserID: uid, Text: text, Visibility: model.NoteVisibilityPublic, RenoteID: renoteID, ReplyID: replyID, Reactions: datatypes.JSON([]byte("{}"))}
		require.NoError(t, repo.Create(n))
		t.Cleanup(func() { cleanupNote(t, id) })
		return n
	}

	qtext := "quoting"
	target := mkNote("n_nr_t", ut.ID, nil, nil, nil) // LV2 引用先
	rtgt := mkNote("n_nr_rp", urp.ID, nil, nil, nil) // quote の reply 先 (depth-2)
	// LV1 引用投稿 = renote(target) + reply(rtgt) + text。
	quote := mkNote("n_nr_q", uq.ID, &target.ID, &rtgt.ID, &qtext)
	boost := mkNote("n_nr_b", ub.ID, &quote.ID, nil, nil) // LV0 pure renote

	// boost に加え、renote を持たない target も問い合わせて RenoteID==nil 経路を通す。
	out, err := repo.FindManyByIDsWithUser([]string{boost.ID, target.ID})
	require.NoError(t, err)
	require.Len(t, out, 2)
	byID := map[string]*model.Note{}
	for _, n := range out {
		byID[n.ID] = n
	}
	got := byID[boost.ID]
	require.NotNil(t, got)

	// LV1: 引用投稿
	require.NotNil(t, got.Renote, "renote (quote) must be hydrated")
	assert.Equal(t, quote.ID, got.Renote.ID)
	require.NotNil(t, got.Renote.User, "renote.user must be hydrated")
	assert.Equal(t, uq.ID, got.Renote.User.ID)
	// LV2 renote: 引用先 — 本バグの修正点
	require.NotNil(t, got.Renote.Renote, "renote.renote (quote target) must be hydrated")
	assert.Equal(t, target.ID, got.Renote.Renote.ID)
	require.NotNil(t, got.Renote.Renote.User, "quote target user must be hydrated")
	assert.Equal(t, ut.ID, got.Renote.Renote.User.ID)
	// LV2 reply: renote 先の返信先も packer が出すので hydrate する。
	require.NotNil(t, got.Renote.Reply, "renote.reply (depth-2) must be hydrated")
	assert.Equal(t, rtgt.ID, got.Renote.Reply.ID)
	require.NotNil(t, got.Renote.Reply.User, "renote.reply user must be hydrated")
	assert.Equal(t, urp.ID, got.Renote.Reply.User.ID)
}

// aliasing 回帰: 同ページの別ノートが depth-2 ターゲットを既に depth-1 sub として
// 読み込んでいる場合でも、quote の引用先 (renote.renote) が配線されること。
// level-2 の配線が lvl2IDSet の有無に依存していると、このケースで配線が skip され
// quote-of-quote が「削除されたノート」表示で再発する。
func TestNoteRepository_FindManyByIDsWithUser_NestedRenoteAliasing(t *testing.T) {
	repo := NewNoteRepository(testDB)

	ut := insertTestUser(t, "u_al_t", "al_target")
	uq := insertTestUser(t, "u_al_q", "al_quoter")
	ua := insertTestUser(t, "u_al_a", "al_booster")
	urb := insertTestUser(t, "u_al_b", "al_replier")
	defer cleanupUser(t, ut.ID)
	defer cleanupUser(t, uq.ID)
	defer cleanupUser(t, ua.ID)
	defer cleanupUser(t, urb.ID)

	mkNote := func(id, uid string, renoteID, replyID, text *string) *model.Note {
		n := &model.Note{ID: id, UserID: uid, Text: text, Visibility: model.NoteVisibilityPublic, RenoteID: renoteID, ReplyID: replyID, Reactions: datatypes.JSON([]byte("{}"))}
		require.NoError(t, repo.Create(n))
		t.Cleanup(func() { cleanupNote(t, id) })
		return n
	}

	qtext := "quoting"
	btext := "replying"
	target := mkNote("n_al_t", ut.ID, nil, nil, nil)           // T = 引用先
	quote := mkNote("n_al_q", uq.ID, &target.ID, nil, &qtext)  // Q = T の引用 (ページに直接は無い)
	boost := mkNote("n_al_a", ua.ID, &quote.ID, nil, nil)      // A = Q の pure renote
	reply := mkNote("n_al_b", urb.ID, nil, &target.ID, &btext) // B = T への reply

	// A と B を同ページで取得する。B の reply target として T が depth-1 sub に入るため、
	// Q の renote(T) は lvl2IDSet に乗らない (aliasing) が、配線は必須。
	out, err := repo.FindManyByIDsWithUser([]string{boost.ID, reply.ID})
	require.NoError(t, err)
	require.Len(t, out, 2)
	byID := map[string]*model.Note{}
	for _, n := range out {
		byID[n.ID] = n
	}
	got := byID[boost.ID]
	require.NotNil(t, got)

	require.NotNil(t, got.Renote, "renote (quote) must be hydrated")
	assert.Equal(t, quote.ID, got.Renote.ID)
	// aliasing でも引用先が配線されること (本回帰の核心)。
	require.NotNil(t, got.Renote.Renote, "renote.renote must be wired even when target was loaded as another note's depth-1 sub")
	assert.Equal(t, target.ID, got.Renote.Renote.ID)
}

func TestNoteRepository_QueryErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	repo := NewNoteRepository(db)

	_, err := repo.ListByUserID("a", "", "", 10)
	assert.Error(t, err)

	// 匿名 viewer (public/home only) と認証 viewer (full visibility 句) の
	// 双方で DB error が propagate することを確認 (#1449)。
	_, err = repo.ListByChannelID("a", "", "", "", 10)
	assert.Error(t, err)

	_, err = repo.ListByChannelID("a", "viewer", "", "", 10)
	assert.Error(t, err)

	_, err = repo.FindManyByIDsWithUser([]string{"a"})
	assert.Error(t, err)

	_, err = repo.ListRenotesOf("a", "", "", "", 10)
	assert.Error(t, err)

	_, err = repo.ListRepliesOf("a", "", "", "", 10)
	assert.Error(t, err)

	_, err = repo.ListChildrenOf("a", "", "", "", 10)
	assert.Error(t, err)

	_, err = repo.SearchByFilter(model.NoteSearchFilter{Query: "a", Limit: 10})
	assert.Error(t, err)

	err = repo.IncrementCount("a", "renoteCount", 1)
	assert.Error(t, err)

	err = repo.IncrementReaction("a", "x", 1)
	assert.Error(t, err)
}

func TestNoteRepository_IncrementReaction(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_irx_1", "irxuser")
	defer cleanupUser(t, user.ID)

	note := &model.Note{
		ID:         "n_irx_1",
		UserID:     user.ID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(note))
	defer cleanupNote(t, note.ID)

	require.NoError(t, repo.IncrementReaction(note.ID, "👍", 2))
	require.NoError(t, repo.IncrementReaction(note.ID, "❤", 1))

	found, err := repo.FindByID(note.ID)
	require.NoError(t, err)
	assert.Contains(t, string(found.Reactions), "👍")
	assert.Contains(t, string(found.Reactions), "❤")

	// 0以下になったキーは削除される
	require.NoError(t, repo.IncrementReaction(note.ID, "👍", -2))
	found, err = repo.FindByID(note.ID)
	require.NoError(t, err)
	assert.NotContains(t, string(found.Reactions), "👍")
}

func TestNoteRepository_IncrementCount(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_n_inc_1", "incuser1")
	defer cleanupUser(t, user.ID)

	note := &model.Note{
		ID:         "n_inc_1",
		UserID:     user.ID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(note))
	defer cleanupNote(t, note.ID)

	require.NoError(t, repo.IncrementCount(note.ID, "renoteCount", 2))
	require.NoError(t, repo.IncrementCount(note.ID, "repliesCount", 3))

	found, err := repo.FindByID(note.ID)
	require.NoError(t, err)
	assert.Equal(t, int16(2), found.RenoteCount)
	assert.Equal(t, int16(3), found.RepliesCount)
}

func TestNoteRepository_ListRenotesOf(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_lr_1", "lruser")
	defer cleanupUser(t, user.ID)

	// 元ノート
	parent := &model.Note{
		ID: "n_lr_parent", UserID: user.ID, Visibility: model.NoteVisibilityPublic,
		Reactions: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(parent))
	defer cleanupNote(t, parent.ID)

	// 3件のrenote
	parentID := parent.ID
	for _, id := range []string{"n_lr_r1", "n_lr_r2", "n_lr_r3"} {
		n := &model.Note{
			ID: id, UserID: user.ID, Visibility: model.NoteVisibilityPublic,
			RenoteID:  &parentID,
			Reactions: datatypes.JSON([]byte("{}")),
		}
		require.NoError(t, repo.Create(n))
		defer cleanupNote(t, id)
	}

	out, err := repo.ListRenotesOf(parent.ID, "", "", "", 10)
	require.NoError(t, err)
	assert.Len(t, out, 3)
	assert.Equal(t, "n_lr_r3", out[0].ID)

	out, err = repo.ListRenotesOf(parent.ID, "", "n_lr_r3", "", 10)
	require.NoError(t, err)
	assert.Len(t, out, 2)

	out, err = repo.ListRenotesOf(parent.ID, "", "", "n_lr_r1", 10)
	require.NoError(t, err)
	assert.Len(t, out, 2)
}

func TestNoteRepository_ListRepliesOf(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_lp_1", "lpuser")
	defer cleanupUser(t, user.ID)

	parent := &model.Note{
		ID: "n_lp_parent", UserID: user.ID, Visibility: model.NoteVisibilityPublic,
		Reactions: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(parent))
	defer cleanupNote(t, parent.ID)

	parentID := parent.ID
	for _, id := range []string{"n_lp_r1", "n_lp_r2"} {
		n := &model.Note{
			ID: id, UserID: user.ID, Visibility: model.NoteVisibilityPublic,
			ReplyID:   &parentID,
			Reactions: datatypes.JSON([]byte("{}")),
		}
		require.NoError(t, repo.Create(n))
		defer cleanupNote(t, id)
	}

	out, err := repo.ListRepliesOf(parent.ID, "", "", "", 10)
	require.NoError(t, err)
	assert.Len(t, out, 2)

	out, err = repo.ListRepliesOf(parent.ID, "", "n_lp_r2", "", 10)
	require.NoError(t, err)
	assert.Len(t, out, 1)

	out, err = repo.ListRepliesOf(parent.ID, "", "", "n_lp_r1", 10)
	require.NoError(t, err)
	assert.Len(t, out, 1)
}

func TestNoteRepository_ListChildrenOf(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_lc_1", "lcuser")
	defer cleanupUser(t, user.ID)

	parent := &model.Note{
		ID: "n_lc_parent", UserID: user.ID, Visibility: model.NoteVisibilityPublic,
		Reactions: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(parent))
	defer cleanupNote(t, parent.ID)

	parentID := parent.ID
	// 1件はreply, 1件はquote renote (IDの辞書順を昇順 c1 < c2 にしておく)
	reply := &model.Note{
		ID: "n_lc_c1", UserID: user.ID, Visibility: model.NoteVisibilityPublic,
		ReplyID:   &parentID,
		Reactions: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(reply))
	defer cleanupNote(t, reply.ID)

	// quote renote (text あり) は children に含める。pure renote 除外ロジック
	// (#1554) に引っかからないよう本文を持たせる。
	quoteText := "quote"
	quote := &model.Note{
		ID: "n_lc_c2", UserID: user.ID, Visibility: model.NoteVisibilityPublic,
		RenoteID:  &parentID,
		Text:      &quoteText,
		Reactions: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(quote))
	defer cleanupNote(t, quote.ID)

	out, err := repo.ListChildrenOf(parent.ID, "", "", "", 10)
	require.NoError(t, err)
	assert.Len(t, out, 2)

	out, err = repo.ListChildrenOf(parent.ID, "", "n_lc_c2", "", 10)
	require.NoError(t, err)
	assert.Len(t, out, 1)
	assert.Equal(t, "n_lc_c1", out[0].ID)

	out, err = repo.ListChildrenOf(parent.ID, "", "", "n_lc_c1", 10)
	require.NoError(t, err)
	assert.Len(t, out, 1)
	assert.Equal(t, "n_lc_c2", out[0].ID)
}

// TestNoteRepository_ListChildrenOf_PureRenoteExcluded pins upstream
// notes/children behavior (children.ts:53-67): a pure renote of the parent
// (no text / files / poll) must NOT appear as a child, while quote renotes
// (text or files or poll) and replies do. Regression guard for #1554.
func TestNoteRepository_ListChildrenOf_PureRenoteExcluded(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_lcpr_1", "lcpruser")
	defer cleanupUser(t, user.ID)

	parent := &model.Note{
		ID: "n_lcpr_parent", UserID: user.ID, Visibility: model.NoteVisibilityPublic,
		Reactions: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(parent))
	defer cleanupNote(t, parent.ID)
	parentID := parent.ID

	mk := func(id string, mutate func(n *model.Note)) {
		n := &model.Note{
			ID: id, UserID: user.ID, Visibility: model.NoteVisibilityPublic,
			Reactions: datatypes.JSON([]byte("{}")),
		}
		mutate(n)
		require.NoError(t, repo.Create(n))
		t.Cleanup(func() { cleanupNote(t, id) })
	}

	// reply (本文なしでも child として返す)
	mk("n_lcpr_c1_reply", func(n *model.Note) { n.ReplyID = &parentID })
	// pure renote (text/file/poll なし) → 除外される
	mk("n_lcpr_c2_pure", func(n *model.Note) { n.RenoteID = &parentID })
	// quote renote (text あり) → 含める
	mk("n_lcpr_c3_quote", func(n *model.Note) {
		n.RenoteID = &parentID
		txt := "quoted"
		n.Text = &txt
	})
	// renote + file → 含める (text なしでも fileIds 非空なら pure ではない)
	mk("n_lcpr_c4_file", func(n *model.Note) {
		n.RenoteID = &parentID
		n.FileIDs = model.StringArray{"file-x"}
	})
	// renote + poll → 含める (hasPoll=true なら pure ではない)
	mk("n_lcpr_c5_poll", func(n *model.Note) {
		n.RenoteID = &parentID
		n.HasPoll = true
	})
	// renote + cw → 含める (#1888: cw も quote 判定に含む)
	mk("n_lcpr_c6_cw", func(n *model.Note) {
		n.RenoteID = &parentID
		cw := "warn"
		n.CW = &cw
	})
	// 別 note への reply を伴う renote-of-parent → quote として含める (#1888: replyId も
	// quote 判定に含む)。reply target は parent ではないので reply branch では拾われない。
	mk("n_lcpr_other", func(n *model.Note) {})
	otherID := "n_lcpr_other"
	mk("n_lcpr_c7_replyquote", func(n *model.Note) {
		n.RenoteID = &parentID
		n.ReplyID = &otherID
	})

	out, err := repo.ListChildrenOf(parentID, "", "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{
		"n_lcpr_c1_reply":      true,
		"n_lcpr_c3_quote":      true,
		"n_lcpr_c4_file":       true,
		"n_lcpr_c5_poll":       true,
		"n_lcpr_c6_cw":         true,
		"n_lcpr_c7_replyquote": true,
	}, idSet(out))
	// pure renote が含まれていないことを明示
	assert.NotContains(t, idSet(out), "n_lcpr_c2_pure")
}

// --- #1500 visibility push-down (renotes / replies / children) ---
//
// スレッド系 3 メソッドが viewer の可視性を LIMIT 前に SQL push-down し、
// (1) anonymous / 非フォロワー / フォロワー / specified-target / author で正しく
// 絞り込むこと、(2) 非表示 note が LIMIT 枠を食わず under-fill しないこと、
// (3) specified note が宛先 viewer には見えること (timeline 型の specified 除外を
// 流用していないこと) を実 SQL で固定する。

// threadVizFixture は author A による mixed-visibility の子 note 群と、
// follower F / 非フォロワー viewer V を用意するヘルパ。childKind は "reply" /
// "renote" を選び、ListRepliesOf / ListRenotesOf に流用する。
func threadVizFixture(t *testing.T, repo NoteRepository, prefix, childKind string) (author, follower, viewer *model.User, parentID string, ids map[string]string) {
	t.Helper()
	followingRepo := NewFollowingRepository(testDB)
	mkUser := func(suffix, username string) *model.User {
		u := insertTestUser(t, prefix+"_"+suffix, username)
		t.Cleanup(func() { cleanupUser(t, u.ID) })
		return u
	}
	author = mkUser("a", prefix+"author")
	follower = mkUser("f", prefix+"follower")
	viewer = mkUser("v", prefix+"viewer")

	f := &model.Following{ID: prefix + "_fl", FollowerID: follower.ID, FolloweeID: author.ID}
	require.NoError(t, followingRepo.Create(f))
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "following" WHERE id = ?`, f.ID) })

	parent := &model.Note{ID: prefix + "_parent", UserID: author.ID, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))}
	require.NoError(t, repo.Create(parent))
	t.Cleanup(func() { cleanupNote(t, parent.ID) })
	parentID = parent.ID

	// ID は昇順 (1..5) で、非表示になりがちな specified/followers を「新しい側」に
	// 寄せることで、旧 fetch-then-filter 実装なら limit 枠を食って under-fill した
	// ケースを再現する。
	ids = map[string]string{
		"pub":    prefix + "_c1_pub",
		"home":   prefix + "_c2_home",
		"fol":    prefix + "_c3_fol",
		"spec_v": prefix + "_c4_specv",
		"spec_x": prefix + "_c5_specx",
	}
	mk := func(id string, vis model.NoteVisibility, visible []string) {
		n := &model.Note{ID: id, UserID: author.ID, Visibility: vis, Reactions: datatypes.JSON([]byte("{}"))}
		if len(visible) > 0 {
			n.VisibleUserIDs = model.StringArray(visible)
		}
		switch childKind {
		case "renote":
			n.RenoteID = &parentID
			// quote 扱いにして pure renote 除外ロジックと干渉しないよう text を持たせる
			txt := "q"
			n.Text = &txt
		default:
			n.ReplyID = &parentID
		}
		require.NoError(t, repo.Create(n))
		t.Cleanup(func() { cleanupNote(t, id) })
	}
	mk(ids["pub"], model.NoteVisibilityPublic, nil)
	mk(ids["home"], model.NoteVisibilityHome, nil)
	mk(ids["fol"], model.NoteVisibilityFollowers, nil)
	mk(ids["spec_v"], model.NoteVisibilitySpecified, []string{viewer.ID})
	mk(ids["spec_x"], model.NoteVisibilitySpecified, []string{"someone-else"})
	return author, follower, viewer, parentID, ids
}

func idSet(rows []*model.Note) map[string]bool {
	out := map[string]bool{}
	for _, n := range rows {
		out[n.ID] = true
	}
	return out
}

func TestNoteRepository_ListRepliesOf_VisibilityPushDown(t *testing.T) {
	repo := NewNoteRepository(testDB)
	author, follower, viewer, parentID, ids := threadVizFixture(t, repo, "rvp", "reply")

	// anonymous: public/home のみ。
	got, err := repo.ListRepliesOf(parentID, "", "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{ids["pub"]: true, ids["home"]: true}, idSet(got))

	// 非フォロワー viewer: public/home + 自分が宛先の specified。followers は見えない。
	got, err = repo.ListRepliesOf(parentID, viewer.ID, "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{ids["pub"]: true, ids["home"]: true, ids["spec_v"]: true}, idSet(got))

	// follower: public/home + followers。宛先でない specified は見えない。
	got, err = repo.ListRepliesOf(parentID, follower.ID, "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{ids["pub"]: true, ids["home"]: true, ids["fol"]: true}, idSet(got))

	// author: 自分の note は全 visibility 見える。
	got, err = repo.ListRepliesOf(parentID, author.ID, "", "", 10)
	require.NoError(t, err)
	assert.Len(t, got, 5)

	// under-fill しない: 非表示 (fol/spec_v/spec_x) が新しい側に 3 件あるが、
	// anonymous で limit=2 を要求すると visible 2 件 (pub/home) がきっちり返る。
	// 旧 fetch-then-filter なら新しい 2 行 (spec_x/spec_v) を取って filter で 0 件に
	// なっていた。
	got, err = repo.ListRepliesOf(parentID, "", "", "", 2)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{ids["pub"]: true, ids["home"]: true}, idSet(got))
}

func TestNoteRepository_ListRenotesOf_VisibilityPushDown(t *testing.T) {
	repo := NewNoteRepository(testDB)
	author, follower, viewer, parentID, ids := threadVizFixture(t, repo, "rnvp", "renote")

	got, err := repo.ListRenotesOf(parentID, "", "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{ids["pub"]: true, ids["home"]: true}, idSet(got))

	got, err = repo.ListRenotesOf(parentID, viewer.ID, "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{ids["pub"]: true, ids["home"]: true, ids["spec_v"]: true}, idSet(got))

	got, err = repo.ListRenotesOf(parentID, follower.ID, "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{ids["pub"]: true, ids["home"]: true, ids["fol"]: true}, idSet(got))

	got, err = repo.ListRenotesOf(parentID, author.ID, "", "", 10)
	require.NoError(t, err)
	assert.Len(t, got, 5)
}

func TestNoteRepository_ListChildrenOf_VisibilityPushDown(t *testing.T) {
	repo := NewNoteRepository(testDB)
	followingRepo := NewFollowingRepository(testDB)
	mkUser := func(id, username string) *model.User {
		u := insertTestUser(t, id, username)
		t.Cleanup(func() { cleanupUser(t, u.ID) })
		return u
	}
	author := mkUser("cvp_a", "cvpauthor")
	viewer := mkUser("cvp_v", "cvpviewer")
	follower := mkUser("cvp_f", "cvpfollower")
	f := &model.Following{ID: "cvp_fl", FollowerID: follower.ID, FolloweeID: author.ID}
	require.NoError(t, followingRepo.Create(f))
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "following" WHERE id = ?`, f.ID) })

	parent := &model.Note{ID: "cvp_parent", UserID: author.ID, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))}
	require.NoError(t, repo.Create(parent))
	t.Cleanup(func() { cleanupNote(t, parent.ID) })
	pid := parent.ID

	// children = reply + quote-renote の混在。followers を「reply 側」に置くことで、
	// もし base の OR が括弧で囲われず `replyId=? OR (renoteId=? AND visibility)` と
	// 解釈されていたら followers reply が漏れる。そのリーク回帰を固定する。
	txt := "q"
	rows := []*model.Note{
		{ID: "cvp_c1_pubreply", UserID: author.ID, Visibility: model.NoteVisibilityPublic, ReplyID: &pid, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "cvp_c2_folreply", UserID: author.ID, Visibility: model.NoteVisibilityFollowers, ReplyID: &pid, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "cvp_c3_pubquote", UserID: author.ID, Visibility: model.NoteVisibilityPublic, RenoteID: &pid, Text: &txt, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "cvp_c4_folquote", UserID: author.ID, Visibility: model.NoteVisibilityFollowers, RenoteID: &pid, Text: &txt, Reactions: datatypes.JSON([]byte("{}"))},
	}
	for _, n := range rows {
		require.NoError(t, repo.Create(n))
		id := n.ID
		t.Cleanup(func() { cleanupNote(t, id) })
	}

	// 非フォロワー viewer: followers の reply / quote は両方とも見えない (= 括弧で
	// OR 全体に visibility が AND されている証拠)。public の reply / quote のみ。
	got, err := repo.ListChildrenOf(pid, viewer.ID, "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"cvp_c1_pubreply": true, "cvp_c3_pubquote": true}, idSet(got))

	// follower: followers の reply / quote も見える。
	got, err = repo.ListChildrenOf(pid, follower.ID, "", "", 10)
	require.NoError(t, err)
	assert.Len(t, got, 4)

	// anonymous: public のみ。
	got, err = repo.ListChildrenOf(pid, "", "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"cvp_c1_pubreply": true, "cvp_c3_pubquote": true}, idSet(got))
}

func TestNoteRepository_SearchByFilter(t *testing.T) {
	repo := NewNoteRepository(testDB)
	localUser := insertTestUser(t, "u_se_1", "seuser")
	defer cleanupUser(t, localUser.ID)
	otherLocal := insertTestUser(t, "u_se_2", "seuser2")
	defer cleanupUser(t, otherLocal.ID)
	remoteHost := "remote.example"
	remoteUser := insertRemoteTestUser(t, "u_se_r", "seremote", remoteHost)
	defer cleanupUser(t, remoteUser.ID)

	channelID := "ch_se_1"
	hello := "Hello world this is searchable"
	other := "completely different"
	private := "Hello but private"
	helloChannel := "Hello in channel"
	helloRemote := "Hello from a remote instance"

	notes := []*model.Note{
		{ID: "n_se_1", UserID: localUser.ID, Text: &hello, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_se_2", UserID: localUser.ID, Text: &other, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_se_3", UserID: localUser.ID, Text: &private, Visibility: model.NoteVisibilityFollowers, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_se_4", UserID: otherLocal.ID, Text: &hello, Visibility: model.NoteVisibilityHome, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_se_5", UserID: localUser.ID, Text: &helloChannel, Visibility: model.NoteVisibilityPublic, ChannelID: &channelID, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_se_6", UserID: remoteUser.ID, Text: &helloRemote, Visibility: model.NoteVisibilityPublic, UserHost: &remoteHost, Reactions: datatypes.JSON([]byte("{}"))},
	}
	for _, n := range notes {
		require.NoError(t, repo.Create(n))
		defer cleanupNote(t, n.ID)
	}

	// 基本: 部分一致 + visibility フィルタ。匿名 (ViewerID 空) は public/home のみ、
	// followers (n_se_3) は除外。
	out, err := repo.SearchByFilter(model.NoteSearchFilter{Query: "hello", Limit: 10})
	require.NoError(t, err)
	got := idsOf(out)
	assert.ElementsMatch(t, []string{"n_se_1", "n_se_4", "n_se_5", "n_se_6"}, got)

	// #1554 ViewerID = 著者本人なら自分の followers note (n_se_3) も検索できる。
	out, err = repo.SearchByFilter(model.NoteSearchFilter{Query: "hello", ViewerID: localUser.ID, Limit: 10})
	require.NoError(t, err)
	assert.Contains(t, idsOf(out), "n_se_3", "viewer 自身の followers note は検索対象に含まれる")

	// userId フィルタ
	out, err = repo.SearchByFilter(model.NoteSearchFilter{Query: "hello", UserID: localUser.ID, Limit: 10})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"n_se_1", "n_se_5"}, idsOf(out))

	// channelId フィルタ
	out, err = repo.SearchByFilter(model.NoteSearchFilter{Query: "hello", ChannelID: channelID, Limit: 10})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"n_se_5"}, idsOf(out))

	// #1938: userId と channelId 両指定時は userId 優先 (channelId 無視)。旧実装は両
	// AND で n_se_5 のみだったが、upstream else-if では localUser の全 hello note (n_se_1 含む)。
	out, err = repo.SearchByFilter(model.NoteSearchFilter{Query: "hello", UserID: localUser.ID, ChannelID: channelID, Limit: 10})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"n_se_1", "n_se_5"}, idsOf(out), "userId 指定時は channelId を無視 (userId 優先排他)")

	// host フィルタ "." → ローカル限定
	out, err = repo.SearchByFilter(model.NoteSearchFilter{Query: "hello", Host: ".", Limit: 10})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"n_se_1", "n_se_4", "n_se_5"}, idsOf(out))

	// host フィルタ — 特定ホスト
	out, err = repo.SearchByFilter(model.NoteSearchFilter{Query: "hello", Host: remoteHost, Limit: 10})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"n_se_6"}, idsOf(out))

	// untilID / sinceID の分岐
	out, err = repo.SearchByFilter(model.NoteSearchFilter{Query: "hello", UntilID: "n_se_4", Limit: 10})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"n_se_1"}, idsOf(out))

	out, err = repo.SearchByFilter(model.NoteSearchFilter{Query: "hello", SinceID: "n_se_4", Limit: 10})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"n_se_5", "n_se_6"}, idsOf(out))

	// #1783 offset: id desc 順で先頭 1 件 (n_se_6) をスキップする。
	out, err = repo.SearchByFilter(model.NoteSearchFilter{Query: "hello", Limit: 10, Offset: 1})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"n_se_5", "n_se_4", "n_se_1"}, idsOf(out))

	// Limit デフォルト (0 → 10) を踏むケース
	out, err = repo.SearchByFilter(model.NoteSearchFilter{Query: "hello"})
	require.NoError(t, err)
	assert.Len(t, out, 4)
}

func TestNoteRepository_SearchByFilter_PgroongaOperator(t *testing.T) {
	repo := NewNoteRepository(testDB)
	// pgroonga 拡張は CI / testcontainers の素の Postgres には存在しない。
	// そこで `&@~` 演算子が解決されない (=エラーメッセージに `&@~` が
	// 含まれる) ことを以て「Pgroonga ブランチが選択されその SQL が
	// 実際に発行された」ことを検証する。
	_, err := repo.SearchByFilter(model.NoteSearchFilter{Query: "hello", Pgroonga: true, Limit: 10})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "&@~")
}

// idsOf is a tiny helper to extract note IDs for assertion comparisons.
func idsOf(notes []*model.Note) []string {
	out := make([]string, 0, len(notes))
	for _, n := range notes {
		out = append(out, n.ID)
	}
	return out
}

func TestNoteRepository_ListFeatured(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "feat_u", "featuser")
	defer cleanupUser(t, user.ID)

	n := &model.Note{ID: "feat_n1", UserID: user.ID, Visibility: "public", RenoteCount: 10}
	require.NoError(t, testDB.Create(n).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, n.ID)

	notes, err := repo.ListFeatured("", "", 10, 0)
	require.NoError(t, err)
	assert.NotEmpty(t, notes)

	// default limit
	notes, err = repo.ListFeatured("", "", 0, 0)
	require.NoError(t, err)
	assert.NotEmpty(t, notes)

	// offset
	notes, err = repo.ListFeatured("", "", 10, 1)
	require.NoError(t, err)
	_ = notes
}

// channelId 指定時は当該チャンネルに属するノートだけが返ること (#489)。
// 過去はこの絞り込みが無く、ハイライトに無関係ノートが混入していた。
func TestNoteRepository_ListFeatured_ChannelFilter(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "feat_chu", "featchu")
	defer cleanupUser(t, user.ID)

	chID := "feat_ch1"
	require.NoError(t, testDB.Exec(`INSERT INTO channel (id, name, "userId") VALUES (?, ?, ?)`, chID, "feat-ch", user.ID).Error)
	defer testDB.Exec(`DELETE FROM channel WHERE id = ?`, chID)

	chPtr := chID
	chNote := &model.Note{ID: "feat_chn1", UserID: user.ID, Visibility: "public", RenoteCount: 5, ChannelID: &chPtr}
	require.NoError(t, testDB.Create(chNote).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, chNote.ID)

	otherNote := &model.Note{ID: "feat_othn", UserID: user.ID, Visibility: "public", RenoteCount: 10}
	require.NoError(t, testDB.Create(otherNote).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, otherNote.ID)

	rows, err := repo.ListFeatured(chID, "", 10, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, chNote.ID, rows[0].ID)
}

func TestNoteRepository_ListFeatured_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewNoteRepository(testDB.WithContext(ctx))
	_, err := repo.ListFeatured("", "", 10, 0)
	assert.Error(t, err)
}

// #1487 Option B / #1491 review: users/featured-notes は selection 段で
// engagement DESC + visibility push-down + channel 除外、display 段で id DESC +
// untilID + limit という 2 段構成。selection は engagement 由来、display は
// id DESC + id cursor で一致するように分離されていることを覆う。
//
// id と engagement の並びが意図的にずれた fixture (z>m>a の id に対し engagement
// は a=10, m=5, z=1) を使い、display が id DESC で並ぶ (engagement DESC でない)
// ことを断定する。
func TestNoteRepository_ListFeaturedByUser(t *testing.T) {
	repo := NewNoteRepository(testDB)
	author := insertTestUser(t, "feat_by_u", "featbyu")
	defer cleanupUser(t, author.ID)
	follower := insertTestUser(t, "feat_by_f", "featbyf")
	defer cleanupUser(t, follower.ID)
	stranger := insertTestUser(t, "feat_by_s", "featbys")
	defer cleanupUser(t, stranger.ID)
	specified := insertTestUser(t, "feat_by_sp", "featbysp")
	defer cleanupUser(t, specified.ID)

	// follower → author の follow を seed。
	require.NoError(t, testDB.Create(&model.Following{
		ID: "f_feat_by_1", FollowerID: follower.ID, FolloweeID: author.ID,
	}).Error)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "f_feat_by_1")

	// channel 投稿はランキング対象外。
	chID := "feat_by_ch"
	require.NoError(t, testDB.Exec(`INSERT INTO channel (id, name, "userId") VALUES (?, ?, ?)`, chID, "feat-by-ch", author.ID).Error)
	defer testDB.Exec(`DELETE FROM channel WHERE id = ?`, chID)

	chPtr := chID
	// engagement と id の並びが逆になる fixture。display 段が id DESC である
	// ことを明示的に断定するため意図的にずらしている (= engagement DESC で
	// 返してしまうとこのテストが落ちる)。
	notes := []*model.Note{
		{ID: "feat_by_aaa", UserID: author.ID, Visibility: "public", RenoteCount: 10},                                                     // 最大 engagement / 最小 id
		{ID: "feat_by_mmm", UserID: author.ID, Visibility: "public", RenoteCount: 3, RepliesCount: 2},                                     //
		{ID: "feat_by_zzz", UserID: author.ID, Visibility: "public", RenoteCount: 1},                                                      // 最小 engagement / 最大 id
		{ID: "feat_by_fol", UserID: author.ID, Visibility: "followers", RenoteCount: 100},                                                 //
		{ID: "feat_by_spc", UserID: author.ID, Visibility: "specified", VisibleUserIDs: model.StringArray{specified.ID}, RenoteCount: 50}, //
		{ID: "feat_by_chn", UserID: author.ID, Visibility: "public", RenoteCount: 999, ChannelID: &chPtr},                                 //
	}
	for _, n := range notes {
		require.NoError(t, testDB.Create(n).Error)
		defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, n.ID)
	}

	// stranger: public 3 件、display は id DESC (zzz > mmm > aaa)。
	got, err := repo.ListFeaturedByUser(author.ID, stranger.ID, "", 10)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "feat_by_zzz", got[0].ID, "display は id DESC (engagement DESC ではない)")
	assert.Equal(t, "feat_by_mmm", got[1].ID)
	assert.Equal(t, "feat_by_aaa", got[2].ID)

	// anonymous も同じ shape (channel 除外を含む)。
	got, err = repo.ListFeaturedByUser(author.ID, "", "", 10)
	require.NoError(t, err)
	require.Len(t, got, 3)

	// follower は public 3 + followers 1 = 4 件。id DESC で zzz > mmm > fol > aaa。
	got, err = repo.ListFeaturedByUser(author.ID, follower.ID, "", 10)
	require.NoError(t, err)
	require.Len(t, got, 4)
	assert.Equal(t, "feat_by_zzz", got[0].ID)
	assert.Equal(t, "feat_by_mmm", got[1].ID)
	assert.Equal(t, "feat_by_fol", got[2].ID)
	assert.Equal(t, "feat_by_aaa", got[3].ID)

	// specified target は public 3 + specified 1 = 4 件。
	got, err = repo.ListFeaturedByUser(author.ID, specified.ID, "", 10)
	require.NoError(t, err)
	require.Len(t, got, 4)
	ids := map[string]bool{}
	for _, n := range got {
		ids[n.ID] = true
	}
	assert.True(t, ids["feat_by_spc"], "specified target に specified note が見える")
	assert.False(t, ids["feat_by_fol"], "specified target は follower ではないので followers note は出ない")

	// author 自身は channel 以外の全 visibility = 5 件。
	got, err = repo.ListFeaturedByUser(author.ID, author.ID, "", 10)
	require.NoError(t, err)
	require.Len(t, got, 5)
	for _, n := range got {
		assert.NotEqual(t, "feat_by_chn", n.ID, "channel 投稿は除外")
	}

	// limit <= 0 は 10 にデフォルト。
	got, err = repo.ListFeaturedByUser(author.ID, author.ID, "", 0)
	require.NoError(t, err)
	assert.Len(t, got, 5)

	// untilID="feat_by_mmm" → id DESC で id < mmm のもの (= feat_by_aaa) のみ
	// 残る。display 段で適用するため id DESC と一致したページングになる
	// (engagement DESC + id cursor の重複/欠落が起きない、#1491 review)。
	got, err = repo.ListFeaturedByUser(author.ID, stranger.ID, "feat_by_mmm", 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "feat_by_aaa", got[0].ID)
}

// #1491 re-review 指摘A: selection 段が「engagement DESC top-50 で絞る」核心
// 挙動の回帰検出。fixture を 51 件にして pool cap を踏ませ、low engagement の
// 1 件 (最大 id) が pool 外に出て display に現れないこと、選抜される 50 件が
// engagement=10 の note (id prefix `feat_cap_pool_`) であることを断定する。
//
// 6 件以下の小規模 fixture では pool = 全件となり、選抜順序が engagement 順
// でも id 順でも結果が変わらないため、`FeaturedNotesPerUserPoolSize` を 51 に
// すれば落ちる test がここまで存在しなかった。本 test は cap を取り除くと
// 必ず落ちる強い regression gate になる。
func TestNoteRepository_ListFeaturedByUser_EngagementPoolCap(t *testing.T) {
	repo := NewNoteRepository(testDB)
	author := insertTestUser(t, "feat_cap_u", "featcapu")
	defer cleanupUser(t, author.ID)
	defer testDB.Exec(`DELETE FROM "note" WHERE id LIKE 'feat_cap_%'`)

	// engagement=10 の note を 50 件 (id: feat_cap_pool_01 .. feat_cap_pool_50)。
	// すべて pool に入る想定。
	for i := 1; i <= 50; i++ {
		id := fmt.Sprintf("feat_cap_pool_%02d", i)
		require.NoError(t, testDB.Create(&model.Note{
			ID: id, UserID: author.ID, Visibility: "public", RenoteCount: 10,
		}).Error)
	}
	// engagement=0 の note を 1 件、最大 id (feat_cap_zzz_low) で。pool は
	// engagement DESC top-50 で絞られるので、これは pool 外になる想定。
	// id 順で並べたら一番上に来てしまうが、selection が engagement 順なので
	// 出てこないことが重要。
	const lowID = "feat_cap_zzz_low"
	require.NoError(t, testDB.Create(&model.Note{
		ID: lowID, UserID: author.ID, Visibility: "public", RenoteCount: 0,
	}).Error)

	got, err := repo.ListFeaturedByUser(author.ID, "", "", 100)
	require.NoError(t, err)
	// pool cap=50 で 50 件返る。limit=100 でも cap が優先。
	require.Len(t, got, 50, "engagement pool cap=50 で 50 件に絞られる")
	for _, n := range got {
		assert.NotEqual(t, lowID, n.ID, "engagement 最下位の note は pool 外に落ちる")
		assert.True(t, strings.HasPrefix(n.ID, "feat_cap_pool_"),
			"engagement DESC 上位 50 件 (feat_cap_pool_*) のみが選抜される")
	}
	// display は id DESC: feat_cap_pool_50, _49, ..., _01。
	assert.Equal(t, "feat_cap_pool_50", got[0].ID)
	assert.Equal(t, "feat_cap_pool_01", got[49].ID)
}

func TestNoteRepository_ListFeaturedByUser_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewNoteRepository(testDB.WithContext(ctx))
	_, err := repo.ListFeaturedByUser("u", "", "", 10)
	assert.Error(t, err)
}

// #1452: notes/user-list-timeline の visibility SQL push-down を覆う。
// public/home は常時、followers は viewer が author 本人か follow 済みのときだけ、
// 自分宛て (visibleUserIds / mentions) は visibility を問わず可視、channel 投稿は
// 除外、匿名は public/home のみ、を実 SQL に対して検証する。
func TestNoteRepository_ListByUserList_VisibilityPushdown(t *testing.T) {
	repo := NewNoteRepository(testDB)
	viewer := insertTestUser(t, "ult_v", "ultV")
	defer cleanupUser(t, viewer.ID)
	m1 := insertTestUser(t, "ult_m1", "ultM1")
	defer cleanupUser(t, m1.ID)
	m2 := insertTestUser(t, "ult_m2", "ultM2")
	defer cleanupUser(t, m2.ID)

	listID := "ult_list_1"
	require.NoError(t, testDB.Create(&model.UserList{ID: listID, UserID: viewer.ID, Name: "l"}).Error)
	defer testDB.Exec(`DELETE FROM "user_list" WHERE id = ?`, listID)
	// m1 / m2 / viewer 自身を list メンバーに (自分の followers note の確認用)。
	for i, mem := range []string{m1.ID, m2.ID, viewer.ID} {
		mid := fmt.Sprintf("ult_mem_%d", i)
		require.NoError(t, testDB.Create(&model.UserListMembership{ID: mid, UserListID: listID, UserID: mem}).Error)
		defer testDB.Exec(`DELETE FROM "user_list_membership" WHERE id = ?`, mid)
	}

	// channel for exclusion test.
	chID := "ult_ch"
	require.NoError(t, testDB.Exec(`INSERT INTO channel (id, name, "userId") VALUES (?, ?, ?)`, chID, "ult-ch", m1.ID).Error)
	defer testDB.Exec(`DELETE FROM channel WHERE id = ?`, chID)
	chPtr := chID

	// id 昇順 (a < b < ... < g)。結果は id DESC で並ぶ。
	notes := []*model.Note{
		{ID: "ult_n_a_pub", UserID: m1.ID, Visibility: "public"},
		{ID: "ult_n_b_home", UserID: m1.ID, Visibility: "home"},
		{ID: "ult_n_c_fol", UserID: m1.ID, Visibility: "followers"},
		{ID: "ult_n_d_spec", UserID: m1.ID, Visibility: "specified", VisibleUserIDs: model.StringArray{viewer.ID}},
		{ID: "ult_n_e_pub2", UserID: m2.ID, Visibility: "public"},
		{ID: "ult_n_f_self_fol", UserID: viewer.ID, Visibility: "followers"},
		{ID: "ult_n_g_ch", UserID: m1.ID, Visibility: "public", ChannelID: &chPtr},
	}
	for _, n := range notes {
		require.NoError(t, testDB.Create(n).Error)
		defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, n.ID)
	}

	collect := func(viewerID string) map[string]bool {
		got, err := repo.ListByUserList(listID, 50, "", "", model.TimelineDBFilter{ViewerID: viewerID})
		require.NoError(t, err)
		ids := map[string]bool{}
		for _, n := range got {
			ids[n.ID] = true
		}
		return ids
	}

	// viewer が m1 を follow していない状態。
	ids := collect(viewer.ID)
	assert.True(t, ids["ult_n_a_pub"], "public は見える")
	assert.True(t, ids["ult_n_b_home"], "home は見える")
	assert.False(t, ids["ult_n_c_fol"], "未フォローの followers は見えない")
	assert.True(t, ids["ult_n_d_spec"], "自分宛ての specified は list timeline にも出る (upstream generateVisibilityQuery)")
	assert.True(t, ids["ult_n_e_pub2"], "別メンバーの public も見える")
	assert.True(t, ids["ult_n_f_self_fol"], "自分の followers note は見える")
	assert.False(t, ids["ult_n_g_ch"], "channel 投稿は除外")

	// viewer が m1 を follow すると followers が見える。specified は依然出ない。
	require.NoError(t, testDB.Create(&model.Following{ID: "ult_f1", FollowerID: viewer.ID, FolloweeID: m1.ID}).Error)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "ult_f1")
	ids = collect(viewer.ID)
	assert.True(t, ids["ult_n_c_fol"], "follow 済みなら followers が見える")
	assert.True(t, ids["ult_n_d_spec"], "follow 済みでも自分宛ての specified は出る")

	// 匿名 (viewerID="") は public/home のみ。
	ids = collect("")
	assert.True(t, ids["ult_n_a_pub"] && ids["ult_n_b_home"], "匿名は public/home が見える")
	assert.False(t, ids["ult_n_c_fol"], "匿名は followers 不可")
	assert.False(t, ids["ult_n_f_self_fol"], "匿名は誰の followers も不可")

	// 並び順は id DESC (no cursor)。
	got, err := repo.ListByUserList(listID, 50, "", "", model.TimelineDBFilter{ViewerID: viewer.ID})
	require.NoError(t, err)
	for i := 1; i < len(got); i++ {
		assert.Greater(t, got[i-1].ID, got[i].ID, "id DESC order")
	}

	// untilID cursor: id < untilID のみ返る。
	got, err = repo.ListByUserList(listID, 50, "", "ult_n_c_fol", model.TimelineDBFilter{ViewerID: viewer.ID})
	require.NoError(t, err)
	for _, n := range got {
		assert.Less(t, n.ID, "ult_n_c_fol", "untilID 未満のみ")
	}
}

// #1452: push-down で visibility を LIMIT 前に絞ることで under-fill しないこと
// (旧 post-fetch FilterVisible は limit を非表示 note で食い potential 過少充填) を
// 固定する回帰テスト。
func TestNoteRepository_ListByUserList_NoUnderfill(t *testing.T) {
	repo := NewNoteRepository(testDB)
	viewer := insertTestUser(t, "ulu_v", "uluV")
	defer cleanupUser(t, viewer.ID)
	member := insertTestUser(t, "ulu_m", "uluM")
	defer cleanupUser(t, member.ID)

	listID := "ulu_list"
	require.NoError(t, testDB.Create(&model.UserList{ID: listID, UserID: viewer.ID, Name: "l"}).Error)
	defer testDB.Exec(`DELETE FROM "user_list" WHERE id = ?`, listID)
	require.NoError(t, testDB.Create(&model.UserListMembership{ID: "ulu_mem", UserListID: listID, UserID: member.ID}).Error)
	defer testDB.Exec(`DELETE FROM "user_list_membership" WHERE id = ?`, "ulu_mem")

	// public 5 件 + followers 5 件 (viewer は member を follow していない)。
	for i := 0; i < 5; i++ {
		pid := fmt.Sprintf("ulu_pub_%02d", i)
		require.NoError(t, testDB.Create(&model.Note{ID: pid, UserID: member.ID, Visibility: "public"}).Error)
		defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, pid)
		fid := fmt.Sprintf("ulu_fol_%02d", i)
		require.NoError(t, testDB.Create(&model.Note{ID: fid, UserID: member.ID, Visibility: "followers"}).Error)
		defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, fid)
	}

	// limit=5: push-down で public のみが対象 → 5 件埋まる。followers が混ざって
	// 後段 filter で削られ under-fill する旧挙動の回帰防止。
	got, err := repo.ListByUserList(listID, 5, "", "", model.TimelineDBFilter{ViewerID: viewer.ID})
	require.NoError(t, err)
	require.Len(t, got, 5, "push-down で limit ぶん public で埋まる")
	for _, n := range got {
		assert.True(t, strings.HasPrefix(n.ID, "ulu_pub_"), "followers は混ざらない")
	}
}

func TestNoteRepository_ListByUserList_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewNoteRepository(testDB.WithContext(ctx))
	_, err := repo.ListByUserList("l", 10, "", "", model.TimelineDBFilter{ViewerID: "v"})
	assert.Error(t, err)
}

// #1496: user_list_membership.withReplies を尊重した返信フィルタを実 SQL で覆う。
// 返信でない / 自己への返信 / viewer 宛ての返信は常に出る。第三者宛ての返信は
// メンバーが withReplies=ON のときだけ出る (upstream user-list-timeline getFromDb
// と一致)。可視性条件と切り分けるため全 note を public にしている。
func TestNoteRepository_ListByUserList_WithReplies(t *testing.T) {
	repo := NewNoteRepository(testDB)
	viewer := insertTestUser(t, "ulw_v", "ulwV")
	defer cleanupUser(t, viewer.ID)
	m := insertTestUser(t, "ulw_m", "ulwM") // withReplies=false
	defer cleanupUser(t, m.ID)
	m2 := insertTestUser(t, "ulw_m2", "ulwM2") // withReplies=true
	defer cleanupUser(t, m2.ID)
	third := insertTestUser(t, "ulw_x", "ulwX") // 第三者 (reply 先、list 非メンバー)
	defer cleanupUser(t, third.ID)

	listID := "ulw_list"
	require.NoError(t, testDB.Create(&model.UserList{ID: listID, UserID: viewer.ID, Name: "l"}).Error)
	defer testDB.Exec(`DELETE FROM "user_list" WHERE id = ?`, listID)
	require.NoError(t, testDB.Create(&model.UserListMembership{ID: "ulw_mem_m", UserListID: listID, UserID: m.ID, WithReplies: false}).Error)
	defer testDB.Exec(`DELETE FROM "user_list_membership" WHERE id = ?`, "ulw_mem_m")
	require.NoError(t, testDB.Create(&model.UserListMembership{ID: "ulw_mem_m2", UserListID: listID, UserID: m2.ID, WithReplies: true}).Error)
	defer testDB.Exec(`DELETE FROM "user_list_membership" WHERE id = ?`, "ulw_mem_m2")

	// reply 先の source note (FK 充足用、list 非メンバー third 投稿なので結果には出ない)。
	srcID := "ulw_src"
	require.NoError(t, testDB.Create(&model.Note{ID: srcID, UserID: third.ID, Visibility: "public"}).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, srcID)

	mkReply := func(id, author, replyUser string) *model.Note {
		ruid := replyUser
		return &model.Note{ID: id, UserID: author, Visibility: "public", ReplyID: &srcID, ReplyUserID: &ruid}
	}
	notes := []*model.Note{
		{ID: "ulw_n_plain", UserID: m.ID, Visibility: "public"}, // 返信でない
		mkReply("ulw_n_self", m.ID, m.ID),                       // 自己への返信
		mkReply("ulw_n_toviewer", m.ID, viewer.ID),              // viewer 宛て返信
		mkReply("ulw_n_tothird", m.ID, third.ID),                // 第三者宛て (withReplies=false)
		mkReply("ulw_n2_tothird", m2.ID, third.ID),              // 第三者宛て (withReplies=true)
	}
	for _, n := range notes {
		require.NoError(t, testDB.Create(n).Error)
		defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, n.ID)
	}

	got, err := repo.ListByUserList(listID, 50, "", "", model.TimelineDBFilter{ViewerID: viewer.ID})
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, n := range got {
		ids[n.ID] = true
	}
	assert.True(t, ids["ulw_n_plain"], "返信でない note は出る")
	assert.True(t, ids["ulw_n_self"], "自己への返信は出る")
	assert.True(t, ids["ulw_n_toviewer"], "viewer 宛ての返信は出る")
	assert.False(t, ids["ulw_n_tothird"], "withReplies=false メンバーの第三者宛て返信は出ない")
	assert.True(t, ids["ulw_n2_tothird"], "withReplies=true メンバーの第三者宛て返信は出る")
}

// #1498: TimelineDBFilter 経由の renote/file 系 param (withRenotes /
// includeMyRenotes / includeRenotedMyNotes / includeLocalRenotes / withFiles) が
// applyTimelineFilter で適用されることを実 SQL で覆う。pure renote と quote renote
// (text あり) を区別し、quote は常に残ることも確認する。
// #1681 user-list-timeline にも base-filter (被block / instance-mute) が効く。
func TestNoteRepository_ListByUserList_BaseFilters(t *testing.T) {
	repo := NewNoteRepository(testDB)
	viewer := insertTestUser(t, "ulbf_v", "ulbfV")
	member := insertTestUser(t, "ulbf_m", "ulbfM")
	blocker := insertTestUser(t, "ulbf_b", "ulbfB")
	defer cleanupUser(t, viewer.ID)
	defer cleanupUser(t, member.ID)
	defer cleanupUser(t, blocker.ID)

	listID := "ulbf_list"
	require.NoError(t, testDB.Create(&model.UserList{ID: listID, UserID: viewer.ID, Name: "l"}).Error)
	defer testDB.Exec(`DELETE FROM "user_list" WHERE id = ?`, listID)
	for i, mem := range []string{member.ID, blocker.ID} {
		mid := fmt.Sprintf("ulbf_mem_%d", i)
		require.NoError(t, testDB.Create(&model.UserListMembership{ID: mid, UserListID: listID, UserID: mem}).Error)
		defer testDB.Exec(`DELETE FROM "user_list_membership" WHERE id = ?`, mid)
	}

	plain := &model.Note{ID: "ulbf_plain", UserID: member.ID, Visibility: "public"}
	byBlocker := &model.Note{ID: "ulbf_blk", UserID: blocker.ID, Visibility: "public"}
	badHost := "bad.example"
	fromBad := &model.Note{ID: "ulbf_inst", UserID: member.ID, Visibility: "public", UserHost: &badHost}
	for _, n := range []*model.Note{plain, byBlocker, fromBad} {
		require.NoError(t, testDB.Create(n).Error)
		defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, n.ID)
	}

	got, err := repo.ListByUserList(listID, 50, "", "", model.TimelineDBFilter{
		ViewerID:       viewer.ID,
		BlockerIDs:     []string{blocker.ID},
		MutedInstances: []string{"bad.example"},
	})
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, n := range got {
		ids[n.ID] = true
	}
	assert.True(t, ids["ulbf_plain"], "通常 member の note は含まれる")
	assert.False(t, ids["ulbf_blk"], "被block member の note は除外")
	assert.False(t, ids["ulbf_inst"], "muted instance の note は除外")
}

func TestNoteRepository_ListByUserList_RenoteFilters(t *testing.T) {
	repo := NewNoteRepository(testDB)
	viewer := insertTestUser(t, "ulr_v", "ulrV") // list owner かつ member
	defer cleanupUser(t, viewer.ID)
	member := insertTestUser(t, "ulr_m", "ulrM")
	defer cleanupUser(t, member.ID)

	listID := "ulr_list"
	require.NoError(t, testDB.Create(&model.UserList{ID: listID, UserID: viewer.ID, Name: "l"}).Error)
	defer testDB.Exec(`DELETE FROM "user_list" WHERE id = ?`, listID)
	for i, mem := range []string{viewer.ID, member.ID} {
		mid := fmt.Sprintf("ulr_mem_%d", i)
		require.NoError(t, testDB.Create(&model.UserListMembership{ID: mid, UserListID: listID, UserID: mem}).Error)
		defer testDB.Exec(`DELETE FROM "user_list_membership" WHERE id = ?`, mid)
	}

	// renote 先 note (FK 充足用)。
	target := "ulr_target"
	require.NoError(t, testDB.Create(&model.Note{ID: target, UserID: member.ID, Visibility: "public"}).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, target)

	emptyFiles := model.StringArray{}
	remoteHost := "remote.example"
	// plain (通常投稿), pure renote (member, ローカル), quote renote (member, text
	// あり), file 付き plain, viewer の pure renote, remote pure renote
	// (renoteUserHost あり)。ulr_pure は renoteUserHost NULL = ローカル renote。
	plain := &model.Note{ID: "ulr_plain", UserID: member.ID, Visibility: "public", FileIDs: emptyFiles}
	pureRenote := &model.Note{ID: "ulr_pure", UserID: member.ID, Visibility: "public", RenoteID: &target, RenoteUserID: &member.ID, FileIDs: emptyFiles}
	quoteText := "quote"
	quoteRenote := &model.Note{ID: "ulr_quote", UserID: member.ID, Visibility: "public", RenoteID: &target, RenoteUserID: &member.ID, Text: &quoteText, FileIDs: emptyFiles}
	withFile := &model.Note{ID: "ulr_file", UserID: member.ID, Visibility: "public", FileIDs: model.StringArray{"f1"}}
	myPureRenote := &model.Note{ID: "ulr_my_pure", UserID: viewer.ID, Visibility: "public", RenoteID: &target, RenoteUserID: &member.ID, FileIDs: emptyFiles}
	remotePureRenote := &model.Note{ID: "ulr_remote", UserID: member.ID, Visibility: "public", RenoteID: &target, RenoteUserID: &member.ID, RenoteUserHost: &remoteHost, FileIDs: emptyFiles}
	for _, n := range []*model.Note{plain, pureRenote, quoteRenote, withFile, myPureRenote, remotePureRenote} {
		require.NoError(t, testDB.Create(n).Error)
		defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, n.ID)
	}

	collect := func(f model.TimelineDBFilter) map[string]bool {
		f.ViewerID = viewer.ID
		got, err := repo.ListByUserList(listID, 50, "", "", f)
		require.NoError(t, err)
		ids := map[string]bool{}
		for _, n := range got {
			ids[n.ID] = true
		}
		return ids
	}

	no := func() *bool { b := false; return &b }

	// 既定 (filter 空, renote 系 nil = true) は全件 (target は member 投稿だが
	// renote 先として list member なので出る点に注意 → target も含む)。
	ids := collect(model.TimelineDBFilter{})
	assert.True(t, ids["ulr_plain"] && ids["ulr_pure"] && ids["ulr_quote"] && ids["ulr_file"] && ids["ulr_my_pure"], "既定は renote も全部出る")

	// withRenotes=false: pure renote を除外、quote renote は残す。
	ids = collect(model.TimelineDBFilter{WithRenotes: no()})
	assert.False(t, ids["ulr_pure"], "withRenotes=false で pure renote は除外")
	assert.False(t, ids["ulr_my_pure"], "withRenotes=false で他の pure renote も除外")
	assert.True(t, ids["ulr_quote"], "quote renote (text あり) は残る")
	assert.True(t, ids["ulr_plain"], "通常投稿は残る")

	// includeMyRenotes=false: viewer 自身の pure renote のみ除外。member の pure
	// renote は残る。
	ids = collect(model.TimelineDBFilter{IncludeMyRenotes: no()})
	assert.False(t, ids["ulr_my_pure"], "自分の pure renote は除外")
	assert.True(t, ids["ulr_pure"], "他人の pure renote は残る")

	// includeRenotedMyNotes=false: 自分の note を renote した pure renote を除外。
	// fixture では renote 先 target は member 投稿なので除外されない (= 全 renote
	// 残る)。viewer 自身の note を renote した fixture を別途用意する。
	myTarget := "ulr_mytarget"
	require.NoError(t, testDB.Create(&model.Note{ID: myTarget, UserID: viewer.ID, Visibility: "public", FileIDs: emptyFiles}).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, myTarget)
	renoteOfMine := &model.Note{ID: "ulr_renote_mine", UserID: member.ID, Visibility: "public", RenoteID: &myTarget, RenoteUserID: &viewer.ID, FileIDs: emptyFiles}
	require.NoError(t, testDB.Create(renoteOfMine).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, renoteOfMine.ID)
	ids = collect(model.TimelineDBFilter{IncludeRenotedMyNotes: no()})
	assert.False(t, ids["ulr_renote_mine"], "自分の note の pure renote は除外")
	assert.True(t, ids["ulr_pure"], "他人の note の pure renote は残る")

	// includeLocalRenotes=false: renoteUserHost NULL (ローカル) の pure renote を
	// 除外し、remote (renoteUserHost あり) の pure renote は残す。
	ids = collect(model.TimelineDBFilter{IncludeLocalRenotes: no()})
	assert.False(t, ids["ulr_pure"], "ローカル投稿の pure renote は除外")
	assert.True(t, ids["ulr_remote"], "remote 投稿の pure renote は残る")
	assert.True(t, ids["ulr_quote"], "quote renote は残る")

	// withFiles=true: ファイル付き note のみ。
	ids = collect(model.TimelineDBFilter{WithFiles: true})
	assert.True(t, ids["ulr_file"], "ファイル付きは残る")
	assert.False(t, ids["ulr_plain"], "ファイル無しは除外")
	assert.False(t, ids["ulr_pure"], "ファイル無し renote も除外")
}

func TestNoteRepository_ListRenotesByUser(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "unrn_u", "unrnuser")
	defer cleanupUser(t, user.ID)
	otherUser := insertTestUser(t, "unrn_other_u", "unrnother")
	defer cleanupUser(t, otherUser.ID)

	orig := &model.Note{ID: "unrn_orig", UserID: user.ID, Visibility: "public"}
	require.NoError(t, testDB.Create(orig).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, orig.ID)
	otherOrig := &model.Note{ID: "unrn_other_orig", UserID: user.ID, Visibility: "public"}
	require.NoError(t, testDB.Create(otherOrig).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, otherOrig.ID)

	renoteID := orig.ID
	rn := &model.Note{ID: "unrn_rn", UserID: user.ID, RenoteID: &renoteID, Visibility: "public"}
	require.NoError(t, testDB.Create(rn).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, rn.ID)
	quoteText := "quote"
	quote := &model.Note{ID: "unrn_quote", UserID: user.ID, RenoteID: &renoteID, Text: &quoteText, Visibility: "public"}
	require.NoError(t, testDB.Create(quote).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, quote.ID)
	otherUsersRenote := &model.Note{ID: "unrn_other_user_rn", UserID: otherUser.ID, RenoteID: &renoteID, Visibility: "public"}
	require.NoError(t, testDB.Create(otherUsersRenote).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, otherUsersRenote.ID)
	otherRenoteID := otherOrig.ID
	otherTargetsRenote := &model.Note{ID: "unrn_other_target_rn", UserID: user.ID, RenoteID: &otherRenoteID, Visibility: "public"}
	require.NoError(t, testDB.Create(otherTargetsRenote).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, otherTargetsRenote.ID)

	found, err := repo.ListRenotesByUser(user.ID, orig.ID)
	require.NoError(t, err)
	require.Len(t, found, 2)
	assert.Equal(t, quote.ID, found[0].ID)
	assert.Equal(t, rn.ID, found[1].ID)
	assert.NotContains(t, []string{found[0].ID, found[1].ID}, otherUsersRenote.ID)
	assert.NotContains(t, []string{found[0].ID, found[1].ID}, otherTargetsRenote.ID)
}

func TestNoteRepository_ListRenotesByUser_NotFound(t *testing.T) {
	repo := NewNoteRepository(testDB)
	found, err := repo.ListRenotesByUser("ghost", "ghost")
	require.NoError(t, err)
	assert.Empty(t, found)
}

func TestNoteRepository_ListMentions(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "ment_u", "mentuser")
	mentionee := insertTestUser(t, "ment_m", "mentionee")
	defer cleanupUser(t, user.ID)
	defer cleanupUser(t, mentionee.ID)

	n := &model.Note{ID: "ment_n1", UserID: user.ID, Visibility: "public", Mentions: []string{mentionee.ID}}
	require.NoError(t, testDB.Create(n).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, n.ID)

	notes, err := repo.ListMentions(mentionee.ID, "", false, 10, "", "")
	require.NoError(t, err)
	assert.NotEmpty(t, notes)

	// with cursors
	notes, err = repo.ListMentions(mentionee.ID, "", false, 10, "", "zzz")
	require.NoError(t, err)
	assert.NotEmpty(t, notes)

	notes, err = repo.ListMentions(mentionee.ID, "", false, 10, "000", "")
	require.NoError(t, err)
	assert.NotEmpty(t, notes)
}

// #1554 following=true は note.userId が viewer の followee または viewer 自身に
// 限定する (upstream mentions.ts following param)。
func TestNoteRepository_ListMentions_Following(t *testing.T) {
	repo := NewNoteRepository(testDB)
	viewer := insertTestUser(t, "lmf_v", "lmfviewer")
	followed := insertTestUser(t, "lmf_f", "lmffollowed")
	stranger := insertTestUser(t, "lmf_s", "lmfstranger")
	defer cleanupUser(t, viewer.ID)
	defer cleanupUser(t, followed.ID)
	defer cleanupUser(t, stranger.ID)
	// viewer は followed を follow する。
	require.NoError(t, testDB.Create(&model.Following{ID: "lmf_fl1", FollowerID: viewer.ID, FolloweeID: followed.ID}).Error)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "lmf_fl1")

	// followed / stranger / viewer 自身が viewer を mention する 3 note。
	for _, n := range []*model.Note{
		{ID: "lmf_nf", UserID: followed.ID, Visibility: "public", Mentions: []string{viewer.ID}},
		{ID: "lmf_ns", UserID: stranger.ID, Visibility: "public", Mentions: []string{viewer.ID}},
		{ID: "lmf_nself", UserID: viewer.ID, Visibility: "public", Mentions: []string{viewer.ID}},
	} {
		require.NoError(t, testDB.Create(n).Error)
		defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, n.ID)
	}

	// following=false は全件 (3)。
	all, err := repo.ListMentions(viewer.ID, "", false, 50, "", "")
	require.NoError(t, err)
	assert.Len(t, all, 3)

	// following=true は followed + 自分の note のみ (stranger 除外)。
	followingOnly, err := repo.ListMentions(viewer.ID, "", true, 50, "", "")
	require.NoError(t, err)
	ids := make(map[string]bool)
	for _, n := range followingOnly {
		ids[n.ID] = true
	}
	assert.True(t, ids["lmf_nf"], "followee の note は含まれる")
	assert.True(t, ids["lmf_nself"], "自分の note は含まれる")
	assert.False(t, ids["lmf_ns"], "非フォローの stranger の note は除外される")
}

// TestNoteRepository_ListMentions_VisibilityPushDown は #2106 N27 を検証する。
// upstream QueryService.generateVisibilityQuery と同じく、mention 対象 (= viewer) は
// followers / specified note でも mention されていれば read できる (旧 #1441 の
// 「mention だけでは見えない」挙動を upstream に揃えて revert した)。follow も
// visibleUserIds も不要で、mention だけで read 経路では可視になる。
// (main-stream realtime push の #1472 strict gate は core/note.canSeeNoteForStream で
// 別途維持しており、本 read 緩和とは独立。)
func TestNoteRepository_ListMentions_VisibilityPushDown(t *testing.T) {
	repo := NewNoteRepository(testDB)
	followingRepo := NewFollowingRepository(testDB)

	mkUser := func(id, username string) *model.User {
		u := insertTestUser(t, id, username)
		t.Cleanup(func() { cleanupUser(t, u.ID) })
		return u
	}
	author := mkUser("u_lm_a", "lmauthor")
	victim := mkUser("u_lm_v", "lmvictim")

	notes := []*model.Note{
		{ID: "n_lm_pub", UserID: author.ID, Visibility: model.NoteVisibilityPublic, Mentions: model.StringArray{victim.ID}, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_lm_fol", UserID: author.ID, Visibility: model.NoteVisibilityFollowers, Mentions: model.StringArray{victim.ID}, Reactions: datatypes.JSON([]byte("{}"))},
		// specified だが visibleUserIds に victim を含まない (mentions のみ)。
		{ID: "n_lm_spec", UserID: author.ID, Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: model.StringArray{"someone-else"}, Mentions: model.StringArray{victim.ID}, Reactions: datatypes.JSON([]byte("{}"))},
	}
	for _, n := range notes {
		require.NoError(t, repo.Create(n))
		defer cleanupNote(t, n.ID)
	}

	idsOf := func(rows []*model.Note) []string {
		out := make([]string, 0, len(rows))
		for _, n := range rows {
			out = append(out, n.ID)
		}
		sort.Strings(out)
		return out
	}

	// #2106 N27: follow なし / visibleUserIds 非対象でも、mention されていれば
	// followers / specified note を read できる (upstream の mentions cross-visibility)。
	out, err := repo.ListMentions(victim.ID, "", false, 50, "", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"n_lm_fol", "n_lm_pub", "n_lm_spec"}, idsOf(out),
		"mention されていれば followers/specified も read 経路では可視")

	// follow しても visibleUserIds に追加しても結果は不変 (既に mention で可視)。
	f := &model.Following{ID: "fl_lm_1", FollowerID: victim.ID, FolloweeID: author.ID}
	require.NoError(t, followingRepo.Create(f))
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, f.ID)
	require.NoError(t, repo.UpdateFields("n_lm_spec", map[string]any{"visibleUserIds": model.StringArray{victim.ID}}))
	out, err = repo.ListMentions(victim.ID, "", false, 50, "", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"n_lm_fol", "n_lm_pub", "n_lm_spec"}, idsOf(out))
}

func TestNoteRepository_ListMentions_DefaultLimit(t *testing.T) {
	repo := NewNoteRepository(testDB)
	notes, err := repo.ListMentions("nobody", "", false, 0, "", "")
	require.NoError(t, err)
	assert.Empty(t, notes)
}

func TestNoteRepository_ListMentions_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewNoteRepository(testDB.WithContext(ctx))
	_, err := repo.ListMentions("x", "", false, 10, "", "")
	assert.Error(t, err)
}

// #1451: visibility 絞り込みを LIMIT 前に SQL push-down するので、別種別の mention
// でページが埋まって under-fill することがない。未指定は upstream TS 同様に全種別を
// 返す。
func TestNoteRepository_ListMentions_VisibilityFilter(t *testing.T) {
	repo := NewNoteRepository(testDB)
	author := insertTestUser(t, "mvf_a", "mvfauthor")
	me := insertTestUser(t, "mvf_m", "mvfme")
	defer cleanupUser(t, author.ID)
	defer cleanupUser(t, me.ID)

	// public mention と specified(me を visibleUserIds に含む) mention を交互に
	// 10 件作る。id 連番で混在させ、片種別だけ要求したときに別種別がページ枠を
	// 食わないことを確認する。
	ids := []string{"mvf_00", "mvf_01", "mvf_02", "mvf_03", "mvf_04", "mvf_05", "mvf_06", "mvf_07", "mvf_08", "mvf_09"}
	for i, nid := range ids {
		n := &model.Note{
			ID: nid, UserID: author.ID,
			Mentions:  model.StringArray{me.ID},
			Reactions: datatypes.JSON([]byte("{}")),
		}
		if i%2 == 0 {
			n.Visibility = model.NoteVisibilityPublic
		} else {
			n.Visibility = model.NoteVisibilitySpecified
			n.VisibleUserIDs = model.StringArray{me.ID}
		}
		require.NoError(t, repo.Create(n))
		defer cleanupNote(t, n.ID)
	}

	// public のみ limit=5 -> public が 5 件きっちり (specified がスロットを食わない)。
	pub, err := repo.ListMentions(me.ID, "public", false, 5, "", "")
	require.NoError(t, err)
	require.Len(t, pub, 5, "public 指定で under-fill しない")
	for _, n := range pub {
		assert.Equal(t, model.NoteVisibilityPublic, n.Visibility)
	}

	// specified のみ limit=5 -> specified が 5 件。
	spec, err := repo.ListMentions(me.ID, "specified", false, 5, "", "")
	require.NoError(t, err)
	require.Len(t, spec, 5, "specified 指定で under-fill しない")
	for _, n := range spec {
		assert.Equal(t, model.NoteVisibilitySpecified, n.Visibility)
	}

	// 未指定 (default) -> 全種別 (TS 一致): public + specified の両方が出る。
	all, err := repo.ListMentions(me.ID, "", false, 100, "", "")
	require.NoError(t, err)
	assert.Len(t, all, 10, "default は全種別を返す")
}

// TestNoteRepository_ListMentions_VisibleUserIDsOnly は #1484 を検証する。
// 本文 @mention の無い specified DM (viewer ∈ visibleUserIds のみ) が
// notes/mentions に出ること、mentions と visibleUserIds の両方に入っても重複行が
// 出ないこと、宛先でも mention 対象でもない viewer には依然出ないこと (#1441 gate
// 整合) を固定する。
func TestNoteRepository_ListMentions_VisibleUserIDsOnly(t *testing.T) {
	repo := NewNoteRepository(testDB)

	mkUser := func(id, username string) *model.User {
		u := insertTestUser(t, id, username)
		t.Cleanup(func() { cleanupUser(t, u.ID) })
		return u
	}
	author := mkUser("u_vu_a", "vuauthor")
	recipient := mkUser("u_vu_r", "vurecipient")
	stranger := mkUser("u_vu_s", "vustranger")

	notes := []*model.Note{
		// 本文 @mention 無し / visibleUserIds に recipient のみ (= 宛先指定だけの DM)。
		{ID: "n_vu_dm", UserID: author.ID, Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: model.StringArray{recipient.ID}, Reactions: datatypes.JSON([]byte("{}"))},
		// mentions と visibleUserIds の両方に recipient (重複行が出ないことの確認用)。
		{ID: "n_vu_both", UserID: author.ID, Visibility: model.NoteVisibilitySpecified, Mentions: model.StringArray{recipient.ID}, VisibleUserIDs: model.StringArray{recipient.ID}, Reactions: datatypes.JSON([]byte("{}"))},
	}
	for _, n := range notes {
		require.NoError(t, repo.Create(n))
		defer cleanupNote(t, n.ID)
	}

	countOf := func(rows []*model.Note, id string) int {
		c := 0
		for _, n := range rows {
			if n.ID == id {
				c++
			}
		}
		return c
	}

	// recipient (default = 全種別): visibleUserIds 由来 / mentions+visibleUserIds
	// 両方とも 1 行ずつ。OR は単一行 boolean なので重複しない。
	out, err := repo.ListMentions(recipient.ID, "", false, 50, "", "")
	require.NoError(t, err)
	assert.Equal(t, 1, countOf(out, "n_vu_dm"), "@mention の無い specified DM が visibleUserIds 経由で出る")
	assert.Equal(t, 1, countOf(out, "n_vu_both"), "mentions と visibleUserIds 両方に入っても重複行は出ない")

	// recipient (Direct タブ = visibility=specified): 同じく両方出る。
	out, err = repo.ListMentions(recipient.ID, "specified", false, 50, "", "")
	require.NoError(t, err)
	assert.Equal(t, 1, countOf(out, "n_vu_dm"), "Direct タブでも visibleUserIds-only DM が出る")
	assert.Equal(t, 1, countOf(out, "n_vu_both"))

	// stranger: 宛先でも mention 対象でもないので何も出ない (#1441 gate 整合)。
	out, err = repo.ListMentions(stranger.ID, "", false, 50, "", "")
	require.NoError(t, err)
	assert.Equal(t, 0, countOf(out, "n_vu_dm"))
	assert.Equal(t, 0, countOf(out, "n_vu_both"))
}

func TestNoteRepository_SearchByTag(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "tag_u", "taguser")
	defer cleanupUser(t, user.ID)

	n := &model.Note{ID: "tag_n1", UserID: user.ID, Visibility: "public", Tags: []string{"golang", "misskey"}}
	require.NoError(t, testDB.Create(n).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, n.ID)

	notes, err := repo.SearchByTag([][]string{{"golang"}}, "", 10, "", "", model.NoteSearchTagFilter{})
	require.NoError(t, err)
	assert.NotEmpty(t, notes)

	notes, err = repo.SearchByTag([][]string{{"nonexistent"}}, "", 10, "", "", model.NoteSearchTagFilter{})
	require.NoError(t, err)
	assert.Empty(t, notes)

	// default limit
	notes, err = repo.SearchByTag([][]string{{"golang"}}, "", 0, "", "", model.NoteSearchTagFilter{})
	require.NoError(t, err)
	assert.NotEmpty(t, notes)

	// with cursors
	notes, err = repo.SearchByTag([][]string{{"golang"}}, "", 10, "000", "zzz", model.NoteSearchTagFilter{})
	require.NoError(t, err)
	assert.NotEmpty(t, notes)
}

// #1948-18: query は normalizeForSearch (NFKC+lowercase) してから突合するので、
// 正規化済み note.tags ("misskey") を 'Misskey' や全角 query で検索できる。
func TestNoteRepository_SearchByTag_QueryNormalized(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "tagn_u", "tagnuser")
	defer cleanupUser(t, user.ID)
	// note.tags は正規化済み (lowercase) を格納する前提。
	n := &model.Note{ID: "tagn_n1", UserID: user.ID, Visibility: "public", Tags: []string{"misskey"}}
	require.NoError(t, testDB.Create(n).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, n.ID)

	// 大文字混在 query → normalize で 'misskey' に。
	notes, err := repo.SearchByTag([][]string{{"Misskey"}}, "", 10, "", "", model.NoteSearchTagFilter{})
	require.NoError(t, err)
	assert.NotEmpty(t, notes, "大文字 query が normalize されて match (#1948-18)")

	// 全角 query → NFKC で 'misskey' に。
	notes, err = repo.SearchByTag([][]string{{"Ｍｉｓｓｋｅｙ"}}, "", 10, "", "", model.NoteSearchTagFilter{})
	require.NoError(t, err)
	assert.NotEmpty(t, notes, "全角 query が NFKC normalize されて match (#1948-18)")
}

// #1948-18: mentions は sinceId があれば untilId の有無に関わらず ASC で order する
// (upstream mentions.ts:69)。汎用 paginationOrder (sinceID && !untilID で ASC) と異なる。
func TestNoteRepository_ListMentions_SinceIDWithUntilID_ASC(t *testing.T) {
	repo := NewNoteRepository(testDB)
	author := insertTestUser(t, "mord_a", "mordauthor")
	target := insertTestUser(t, "mord_t", "mordtarget")
	defer cleanupUser(t, author.ID)
	defer cleanupUser(t, target.ID)
	for _, nid := range []string{"mord1", "mord2", "mord3"} {
		n := &model.Note{ID: nid, UserID: author.ID, Visibility: "public", Mentions: []string{target.ID}}
		require.NoError(t, testDB.Create(n).Error)
		defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, nid)
	}
	// sinceID + untilID 両方指定: upstream は ASC。
	out, err := repo.ListMentions(target.ID, "", false, 50, "mord0", "mord9")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(out), 2)
	assert.True(t, out[0].ID < out[1].ID, "sinceId 指定時は untilId があっても ASC (#1948-18)")
}

// #1683 query (外側OR・内側AND) の複合タグ検索を実 SQL で検証する。
func TestNoteRepository_SearchByTag_OrOfAnd(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "tagq_u", "tagquser")
	defer cleanupUser(t, user.ID)

	goRust := &model.Note{ID: "tagq_gr", UserID: user.ID, Visibility: "public", Tags: []string{"go", "rust"}}
	webOnly := &model.Note{ID: "tagq_web", UserID: user.ID, Visibility: "public", Tags: []string{"web"}}
	goOnly := &model.Note{ID: "tagq_go", UserID: user.ID, Visibility: "public", Tags: []string{"go"}}
	for _, n := range []*model.Note{goRust, webOnly, goOnly} {
		require.NoError(t, testDB.Create(n).Error)
		defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, n.ID)
	}

	// (go AND rust) OR (web)。
	out, err := repo.SearchByTag([][]string{{"go", "rust"}, {"web"}}, "", 50, "", "", model.NoteSearchTagFilter{})
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, n := range out {
		ids[n.ID] = true
	}
	assert.True(t, ids["tagq_gr"], "go AND rust はヒット")
	assert.True(t, ids["tagq_web"], "web group はヒット")
	assert.False(t, ids["tagq_go"], "go だけは (go AND rust) を満たさず外れる")

	// 有効 group が無い (空) は空結果。
	out, err = repo.SearchByTag([][]string{{""}}, "", 50, "", "", model.NoteSearchTagFilter{})
	require.NoError(t, err)
	assert.Empty(t, out)
}

// #1554 SearchByTag の reply/renote/poll/withFiles filter。
func TestNoteRepository_SearchByTag_Filters(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "sbtf_u", "sbtfuser")
	defer cleanupUser(t, user.ID)
	// reply の ReplyID は実在する note (plain) を指す。000080 で自己参照 FK は
	// 落としたので制約上の必要は無いが、参照先がある通常のケースを見たいので維持する。
	parent := "sbtf_plain"
	bt := func(v bool) *bool { return &v }

	plain := &model.Note{ID: "sbtf_plain", UserID: user.ID, Visibility: "public", Tags: []string{"sbtf"}}
	reply := &model.Note{ID: "sbtf_reply", UserID: user.ID, Visibility: "public", Tags: []string{"sbtf"}, ReplyID: &parent}
	pollFile := &model.Note{ID: "sbtf_pf", UserID: user.ID, Visibility: "public", Tags: []string{"sbtf"}, HasPoll: true, FileIDs: []string{"f1"}}
	for _, n := range []*model.Note{plain, reply, pollFile} {
		require.NoError(t, testDB.Create(n).Error)
		defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, n.ID)
	}

	collect := func(f model.NoteSearchTagFilter) map[string]bool {
		out, err := repo.SearchByTag([][]string{{"sbtf"}}, "", 50, "", "", f)
		require.NoError(t, err)
		ids := map[string]bool{}
		for _, n := range out {
			ids[n.ID] = true
		}
		return ids
	}

	ids := collect(model.NoteSearchTagFilter{Reply: bt(true)})
	assert.True(t, ids["sbtf_reply"])
	assert.False(t, ids["sbtf_plain"])

	ids = collect(model.NoteSearchTagFilter{Reply: bt(false)})
	assert.False(t, ids["sbtf_reply"])
	assert.True(t, ids["sbtf_plain"])

	ids = collect(model.NoteSearchTagFilter{Poll: bt(true)})
	assert.True(t, ids["sbtf_pf"])
	assert.False(t, ids["sbtf_plain"])

	ids = collect(model.NoteSearchTagFilter{WithFiles: true})
	assert.True(t, ids["sbtf_pf"])
	assert.False(t, ids["sbtf_plain"])
}

func TestNoteRepository_SearchByTag_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewNoteRepository(testDB.WithContext(ctx))
	_, err := repo.SearchByTag([][]string{{"x"}}, "", 10, "", "", model.NoteSearchTagFilter{})
	assert.Error(t, err)
}

// TestNoteRepository_SearchByTag_VisibilityPushDown は #1439 の visibility
// push-down を検証する。同一 tag に public / followers / specified を付け、
// viewer ごとに SQL 段で見える note が変わることを確認する。
func TestNoteRepository_SearchByTag_VisibilityPushDown(t *testing.T) {
	repo := NewNoteRepository(testDB)
	followingRepo := NewFollowingRepository(testDB)

	mkUser := func(id, username string) *model.User {
		u := insertTestUser(t, id, username)
		t.Cleanup(func() { cleanupUser(t, u.ID) })
		return u
	}
	author := mkUser("u_sbt_a", "sbtauthor")
	follower := mkUser("u_sbt_f", "sbtfollower")
	allowed := mkUser("u_sbt_al", "sbtallowed")
	stranger := mkUser("u_sbt_s", "sbtstranger")

	notes := []*model.Note{
		{ID: "n_sbt_pub", UserID: author.ID, Visibility: model.NoteVisibilityPublic, Tags: model.StringArray{"sbttag"}, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_sbt_fol", UserID: author.ID, Visibility: model.NoteVisibilityFollowers, Tags: model.StringArray{"sbttag"}, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_sbt_spec", UserID: author.ID, Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: model.StringArray{allowed.ID}, Tags: model.StringArray{"sbttag"}, Reactions: datatypes.JSON([]byte("{}"))},
	}
	for _, n := range notes {
		require.NoError(t, repo.Create(n))
		defer cleanupNote(t, n.ID)
	}

	f := &model.Following{ID: "fl_sbt_1", FollowerID: follower.ID, FolloweeID: author.ID}
	require.NoError(t, followingRepo.Create(f))
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, f.ID)

	idsOf := func(rows []*model.Note) []string {
		out := make([]string, 0, len(rows))
		for _, n := range rows {
			out = append(out, n.ID)
		}
		sort.Strings(out)
		return out
	}

	// 匿名 / stranger は public のみ。
	out, err := repo.SearchByTag([][]string{{"sbttag"}}, "", 50, "", "", model.NoteSearchTagFilter{})
	require.NoError(t, err)
	assert.Equal(t, []string{"n_sbt_pub"}, idsOf(out))

	out, err = repo.SearchByTag([][]string{{"sbttag"}}, stranger.ID, 50, "", "", model.NoteSearchTagFilter{})
	require.NoError(t, err)
	assert.Equal(t, []string{"n_sbt_pub"}, idsOf(out))

	// follower は public + followers。
	out, err = repo.SearchByTag([][]string{{"sbttag"}}, follower.ID, 50, "", "", model.NoteSearchTagFilter{})
	require.NoError(t, err)
	assert.Equal(t, []string{"n_sbt_fol", "n_sbt_pub"}, idsOf(out))

	// visibleUserIds 対象は public + specified。
	out, err = repo.SearchByTag([][]string{{"sbttag"}}, allowed.ID, 50, "", "", model.NoteSearchTagFilter{})
	require.NoError(t, err)
	assert.Equal(t, []string{"n_sbt_pub", "n_sbt_spec"}, idsOf(out))

	// author 本人は全 visibility。
	out, err = repo.SearchByTag([][]string{{"sbttag"}}, author.ID, 50, "", "", model.NoteSearchTagFilter{})
	require.NoError(t, err)
	assert.Equal(t, []string{"n_sbt_fol", "n_sbt_pub", "n_sbt_spec"}, idsOf(out))
}

// TestNoteRepository_DeleteByUserBatch verifies that one call deletes up to
// batchSize rows and callers are expected to loop.
func TestNoteRepository_DeleteByUserBatch(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_dbu_v", "dbuviewer")
	defer cleanupUser(t, user.ID)

	ids := []string{"n_dbu_1", "n_dbu_2", "n_dbu_3"}
	text := "hi"
	for _, id := range ids {
		n := &model.Note{
			ID: id, UserID: user.ID, Text: &text,
			Visibility: model.NoteVisibilityPublic,
			Reactions:  datatypes.JSON([]byte("{}")),
		}
		require.NoError(t, repo.Create(n))
		defer cleanupNote(t, id)
	}

	// 1 バッチで 2 件だけ消える。
	n, err := repo.DeleteByUserBatch(user.ID, 2)
	require.NoError(t, err)
	assert.EqualValues(t, 2, n)

	// 残りの 1 件は次のバッチで消える。
	n, err = repo.DeleteByUserBatch(user.ID, 100)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)

	// 以降はすべて 0 件 (呼び出し側の break 条件)。
	n, err = repo.DeleteByUserBatch(user.ID, 100)
	require.NoError(t, err)
	assert.EqualValues(t, 0, n)

	// userId 空は no-op
	n, err = repo.DeleteByUserBatch("", 10)
	require.NoError(t, err)
	assert.EqualValues(t, 0, n)
}

// TestNoteRepository_ListHomeTimeline_MutedChannelsRespectedWithPrecedence
// guards against the SQL precedence bug where MutedChannelIDs would short
// circuit other AND conditions via an unparenthesized OR. Places two notes
// by the viewer (one in a muted channel) and two notes by an unfollowed
// user (one in an open channel). The home timeline must return only the
// viewer's plain note. With missing parentheses the follow filter would be
// bypassed and the unfollowed user's notes would leak through.
func TestNoteRepository_ListHomeTimeline_MutedChannelsRespectedWithPrecedence(t *testing.T) {
	repo := NewNoteRepository(testDB)
	viewer := insertTestUser(t, "u_mc_v", "mcviewer")
	defer cleanupUser(t, viewer.ID)
	other := insertTestUser(t, "u_mc_o", "mcother")
	defer cleanupUser(t, other.ID)

	mutedChannel := "ch_mc_muted"
	openChannel := "ch_mc_open"
	selfText := "self note"
	selfInMuted := "self in muted channel"
	otherText := "other note (not followed)"
	otherInOpen := "other in open channel (not followed)"

	notes := []*model.Note{
		{ID: "n_mc_1", UserID: viewer.ID, Text: &selfText, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_mc_2", UserID: viewer.ID, Text: &selfInMuted, Visibility: model.NoteVisibilityPublic, ChannelID: &mutedChannel, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_mc_3", UserID: other.ID, Text: &otherText, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_mc_4", UserID: other.ID, Text: &otherInOpen, Visibility: model.NoteVisibilityPublic, ChannelID: &openChannel, Reactions: datatypes.JSON([]byte("{}"))},
	}
	for _, n := range notes {
		require.NoError(t, repo.Create(n))
		defer cleanupNote(t, n.ID)
	}

	rows, err := repo.ListHomeTimeline(viewer.ID, 100, "", "", model.TimelineDBFilter{
		ViewerID:        viewer.ID,
		MutedChannelIDs: []string{mutedChannel},
	})
	require.NoError(t, err)

	ids := idsOf(rows)
	assert.ElementsMatch(t, []string{"n_mc_1"}, ids,
		"follow + mute フィルタが OR で短絡されていない (SQL precedence バグのリグレッション)")
}

// TestNoteRepository_ListLocalTimeline_MutedUsersFilter verifies that
// model.TimelineDBFilter.MutedUserIDs is applied at SQL level (#892),
// excluding both notes authored by muted users AND renotes whose original
// author is muted. heavy-mute viewer でも limit ぶん fill されることも
// 同時に確認する (= post-fetch filter 時の UX regression 回避)。
func TestNoteRepository_ListLocalTimeline_MutedUsersFilter(t *testing.T) {
	repo := NewNoteRepository(testDB)
	mutedAuthor := insertTestUser(t, "u_mu_m", "mumuted")
	defer cleanupUser(t, mutedAuthor.ID)
	okAuthor := insertTestUser(t, "u_mu_o", "muok")
	defer cleanupUser(t, okAuthor.ID)
	mutedRenoteSrc := insertTestUser(t, "u_mu_rs", "murenotesrc")
	defer cleanupUser(t, mutedRenoteSrc.ID)

	mkPlain := func(id, uid string) *model.Note {
		text := "x"
		return &model.Note{
			ID: id, UserID: uid, Text: &text,
			Visibility: model.NoteVisibilityPublic,
			Reactions:  datatypes.JSON([]byte("{}")),
		}
	}

	// muted author の note (除外されるべき)
	muted1 := mkPlain("n_mu_m1", mutedAuthor.ID)
	muted2 := mkPlain("n_mu_m2", mutedAuthor.ID)
	require.NoError(t, repo.Create(muted1))
	require.NoError(t, repo.Create(muted2))
	defer cleanupNote(t, muted1.ID)
	defer cleanupNote(t, muted2.ID)

	// muted renote source (= ok author が muted user の note を renote。
	// renoteUserId が muted なので除外されるべき)
	renoteSrc := mkPlain("n_mu_src", mutedRenoteSrc.ID)
	require.NoError(t, repo.Create(renoteSrc))
	defer cleanupNote(t, renoteSrc.ID)
	renote := &model.Note{
		ID: "n_mu_renote", UserID: okAuthor.ID,
		RenoteID: &renoteSrc.ID, RenoteUserID: &mutedRenoteSrc.ID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(renote))
	defer cleanupNote(t, renote.ID)

	// non-muted note (= ok author の plain note、含まれるべき)
	ok1 := mkPlain("n_mu_ok1", okAuthor.ID)
	ok2 := mkPlain("n_mu_ok2", okAuthor.ID)
	ok3 := mkPlain("n_mu_ok3", okAuthor.ID)
	require.NoError(t, repo.Create(ok1))
	require.NoError(t, repo.Create(ok2))
	require.NoError(t, repo.Create(ok3))
	defer cleanupNote(t, ok1.ID)
	defer cleanupNote(t, ok2.ID)
	defer cleanupNote(t, ok3.ID)

	rows, err := repo.ListLocalTimeline(100, "", "", model.TimelineDBFilter{
		MutedUserIDs: []string{mutedAuthor.ID, mutedRenoteSrc.ID},
	})
	require.NoError(t, err)

	ids := idsOf(rows)
	// muted author の note と muted renote source は除外
	assert.NotContains(t, ids, muted1.ID)
	assert.NotContains(t, ids, muted2.ID)
	assert.NotContains(t, ids, renote.ID)
	// renoteSrc 自体は muted author の plain note として除外される
	assert.NotContains(t, ids, renoteSrc.ID)
	// ok author の plain note 3 件が残る
	assert.Contains(t, ids, ok1.ID)
	assert.Contains(t, ids, ok2.ID)
	assert.Contains(t, ids, ok3.ID)

	// heavy-mute fill: limit=2 を要求した時、SQL push-down なら non-muted note
	// が limit 件返ることを確認する (post-fetch filter だと最大 2 → muted 除外
	// で 0 件になり得たが、SQL 段階で除外しているので必ず 2 件返る)。
	rows, err = repo.ListLocalTimeline(2, "", "", model.TimelineDBFilter{
		MutedUserIDs: []string{mutedAuthor.ID, mutedRenoteSrc.ID},
	})
	require.NoError(t, err)
	assert.Len(t, rows, 2, "SQL push-down で limit ぶん non-muted note が fill されること (#892)")
}

// TestNoteRepository_ListLocalTimeline_MutedUsersSubquery は production
// 経路 (UseMutingSubquery=true + ViewerID) で muting テーブルへ subquery
// する filter が正しく動作することを確認する (#894)。muting row の
// expiresAt が NULL / 未来 / 過去の 3 ケースで active mute だけ filter
// に効くこと、renote 元 user の mute も同時に弾かれることを担保する。
func TestNoteRepository_ListLocalTimeline_MutedUsersSubquery(t *testing.T) {
	repo := NewNoteRepository(testDB)
	mutingRepo := NewMutingRepository(testDB)
	viewer := insertTestUser(t, "u_mus_v", "musv")
	defer cleanupUser(t, viewer.ID)
	mutedAuthor := insertTestUser(t, "u_mus_m", "musm")
	defer cleanupUser(t, mutedAuthor.ID)
	expiredMutedAuthor := insertTestUser(t, "u_mus_e", "muse")
	defer cleanupUser(t, expiredMutedAuthor.ID)
	mutedRenoteSrc := insertTestUser(t, "u_mus_rs", "musrs")
	defer cleanupUser(t, mutedRenoteSrc.ID)
	okAuthor := insertTestUser(t, "u_mus_o", "muso")
	defer cleanupUser(t, okAuthor.ID)

	// active mute (expiresAt nil)
	activeMute := &model.Muting{ID: "mute_us_a", MuterID: viewer.ID, MuteeID: mutedAuthor.ID}
	require.NoError(t, mutingRepo.Create(activeMute))
	defer cleanupMuting(t, activeMute.ID)

	// active mute (renote 元)
	renoteMute := &model.Muting{ID: "mute_us_r", MuterID: viewer.ID, MuteeID: mutedRenoteSrc.ID}
	require.NoError(t, mutingRepo.Create(renoteMute))
	defer cleanupMuting(t, renoteMute.ID)

	// expired mute (subquery で除外されるべき = 該当 user の note は filter 通過する)
	pastTime := time.Now().Add(-1 * time.Hour)
	expiredMute := &model.Muting{ID: "mute_us_e", MuterID: viewer.ID, MuteeID: expiredMutedAuthor.ID, ExpiresAt: &pastTime}
	require.NoError(t, mutingRepo.Create(expiredMute))
	defer cleanupMuting(t, expiredMute.ID)

	mkPlain := func(id, uid string) *model.Note {
		text := "x"
		return &model.Note{
			ID: id, UserID: uid, Text: &text,
			Visibility: model.NoteVisibilityPublic,
			Reactions:  datatypes.JSON([]byte("{}")),
		}
	}

	// muted active author の note (除外)
	mutedNote := mkPlain("n_mus_m", mutedAuthor.ID)
	require.NoError(t, repo.Create(mutedNote))
	defer cleanupNote(t, mutedNote.ID)

	// expired mute author の note (含まれる = subquery が NOW() フィルタを尊重)
	expiredAuthorNote := mkPlain("n_mus_e", expiredMutedAuthor.ID)
	require.NoError(t, repo.Create(expiredAuthorNote))
	defer cleanupNote(t, expiredAuthorNote.ID)

	// renote (= ok author が mutedRenoteSrc を renote。renoteUserId が active
	// mute なので除外)
	renoteSrc := mkPlain("n_mus_src", mutedRenoteSrc.ID)
	require.NoError(t, repo.Create(renoteSrc))
	defer cleanupNote(t, renoteSrc.ID)
	renote := &model.Note{
		ID: "n_mus_renote", UserID: okAuthor.ID,
		RenoteID: &renoteSrc.ID, RenoteUserID: &mutedRenoteSrc.ID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(renote))
	defer cleanupNote(t, renote.ID)

	// ok plain note (含まれる)
	okNote := mkPlain("n_mus_ok", okAuthor.ID)
	require.NoError(t, repo.Create(okNote))
	defer cleanupNote(t, okNote.ID)

	// production 経路: UseMutingSubquery=true / MutedUserIDs 空
	rows, err := repo.ListLocalTimeline(100, "", "", model.TimelineDBFilter{
		ViewerID:          viewer.ID,
		UseMutingSubquery: true,
	})
	require.NoError(t, err)
	ids := idsOf(rows)
	assert.NotContains(t, ids, mutedNote.ID, "active mute author の note は除外")
	assert.NotContains(t, ids, renoteSrc.ID, "active mute author の plain note (renote 元) も除外")
	assert.NotContains(t, ids, renote.ID, "renoteUserId が active mute の renote も除外")
	assert.Contains(t, ids, expiredAuthorNote.ID, "expired mute は subquery で除外され filter 通過")
	assert.Contains(t, ids, okNote.ID, "ok author の note は含まれる")
}

// TestNoteRepository_ListLocalTimeline_RenoteMutedSubquery は production
// 経路 (UseRenoteMutingSubquery=true + ViewerID) で renote_muting への
// NOT EXISTS subquery が pure renote のみ除外し、plain note / quote
// renote は通すことを直接 verify する (#903)。
func TestNoteRepository_ListLocalTimeline_RenoteMutedSubquery(t *testing.T) {
	repo := NewNoteRepository(testDB)
	renoteMutingRepo := NewRenoteMutingRepository(testDB)
	viewer := insertTestUser(t, "u_rms_v", "rmsv")
	defer cleanupUser(t, viewer.ID)
	mutedRenoter := insertTestUser(t, "u_rms_m", "rmsm")
	defer cleanupUser(t, mutedRenoter.ID)
	srcAuthor := insertTestUser(t, "u_rms_s", "rmss")
	defer cleanupUser(t, srcAuthor.ID)

	// viewer renote-mutes mutedRenoter
	rmRec := &model.RenoteMuting{ID: "rms_mute", MuterID: viewer.ID, MuteeID: mutedRenoter.ID}
	require.NoError(t, renoteMutingRepo.Create(rmRec))
	defer cleanupRenoteMuting(t, rmRec.ID)

	// mutedRenoter の plain note (= renote ではない、含まれるべき)
	plainText := "plain"
	plainNote := &model.Note{
		ID: "n_rms_plain", UserID: mutedRenoter.ID, Text: &plainText,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(plainNote))
	defer cleanupNote(t, plainNote.ID)

	// srcAuthor の original note (renote 元)
	srcText := "src"
	srcNote := &model.Note{
		ID: "n_rms_src", UserID: srcAuthor.ID, Text: &srcText,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(srcNote))
	defer cleanupNote(t, srcNote.ID)

	// mutedRenoter の pure renote (除外されるべき)
	pureRenote := &model.Note{
		ID: "n_rms_pure", UserID: mutedRenoter.ID,
		RenoteID: &srcNote.ID, RenoteUserID: &srcAuthor.ID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(pureRenote))
	defer cleanupNote(t, pureRenote.ID)

	// mutedRenoter の quote renote (text 付き、含まれるべき)
	quoteText := "quote!"
	quoteRenote := &model.Note{
		ID: "n_rms_quote", UserID: mutedRenoter.ID, Text: &quoteText,
		RenoteID: &srcNote.ID, RenoteUserID: &srcAuthor.ID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(quoteRenote))
	defer cleanupNote(t, quoteRenote.ID)

	rows, err := repo.ListLocalTimeline(100, "", "", model.TimelineDBFilter{
		ViewerID:                viewer.ID,
		UseRenoteMutingSubquery: true,
	})
	require.NoError(t, err)
	ids := idsOf(rows)

	assert.Contains(t, ids, plainNote.ID, "plain note は renote-mute 対象外")
	assert.Contains(t, ids, srcNote.ID, "src author の note は対象外 (= viewer は src を mute していない)")
	assert.Contains(t, ids, quoteRenote.ID, "quote renote (text 付き) は通る")
	assert.NotContains(t, ids, pureRenote.ID, "muted renoter の pure renote のみ除外")
}

// TestNoteRepository_ListHomeTimeline_MutedUsersWithFollowFilter は HomeTimeline
// で MutedUserIDs と follow 制約が両立することを確認する (#892)。muted user は
// follow していても home から除外されるべき、かつ MutedUserIDs の OR 節バグで
// 非 follower の note が漏れない (= MutedChannel と同 SQL precedence ガード)。
func TestNoteRepository_ListHomeTimeline_MutedUsersWithFollowFilter(t *testing.T) {
	repo := NewNoteRepository(testDB)
	viewer := insertTestUser(t, "u_mhfu_v", "mhfuviewer")
	defer cleanupUser(t, viewer.ID)
	followedMuted := insertTestUser(t, "u_mhfu_fm", "mhfufollowedmuted")
	defer cleanupUser(t, followedMuted.ID)
	followedOK := insertTestUser(t, "u_mhfu_fo", "mhfufollowedok")
	defer cleanupUser(t, followedOK.ID)
	stranger := insertTestUser(t, "u_mhfu_s", "mhfustranger")
	defer cleanupUser(t, stranger.ID)

	// viewer は followedMuted と followedOK を follow するが、followedMuted は
	// mute 済み。stranger は未 follow なので home に含まれてはいけない。
	require.NoError(t, testDB.Exec(
		`INSERT INTO "following" (id, "followerId", "followeeId") VALUES (?, ?, ?)`,
		"flw_mhfu_1", viewer.ID, followedMuted.ID,
	).Error)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "flw_mhfu_1")
	require.NoError(t, testDB.Exec(
		`INSERT INTO "following" (id, "followerId", "followeeId") VALUES (?, ?, ?)`,
		"flw_mhfu_2", viewer.ID, followedOK.ID,
	).Error)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "flw_mhfu_2")

	mk := func(id, uid string) *model.Note {
		text := "hi"
		return &model.Note{
			ID: id, UserID: uid, Text: &text,
			Visibility: model.NoteVisibilityPublic,
			Reactions:  datatypes.JSON([]byte("{}")),
		}
	}
	muted := mk("n_mhfu_m", followedMuted.ID)
	ok := mk("n_mhfu_o", followedOK.ID)
	leaked := mk("n_mhfu_s", stranger.ID)
	require.NoError(t, repo.Create(muted))
	require.NoError(t, repo.Create(ok))
	require.NoError(t, repo.Create(leaked))
	defer cleanupNote(t, muted.ID)
	defer cleanupNote(t, ok.ID)
	defer cleanupNote(t, leaked.ID)

	rows, err := repo.ListHomeTimeline(viewer.ID, 100, "", "", model.TimelineDBFilter{
		ViewerID:     viewer.ID,
		MutedUserIDs: []string{followedMuted.ID},
	})
	require.NoError(t, err)

	ids := idsOf(rows)
	assert.ElementsMatch(t, []string{ok.ID}, ids,
		"follow + mute フィルタが両立し、stranger が漏れず muted user も除外されること (#892)")
}

func TestNoteRepository_CountReplyTargets(t *testing.T) {
	repo := NewNoteRepository(testDB)
	author := insertTestUser(t, "u_crt_a", "crtA")
	defer cleanupUser(t, author.ID)
	t1 := insertTestUser(t, "u_crt_t1", "crtT1")
	defer cleanupUser(t, t1.ID)
	t2 := insertTestUser(t, "u_crt_t2", "crtT2")
	defer cleanupUser(t, t2.ID)

	// author から t1 に 3 件、t2 に 1 件返信。author 自身への返信は除外される。
	replyID := "n_crt_src"
	require.NoError(t, testDB.Create(&model.Note{ID: replyID, UserID: t1.ID, Visibility: model.NoteVisibilityPublic}).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, replyID)
	mk := func(id, target string) *model.Note {
		return &model.Note{ID: id, UserID: author.ID, ReplyID: &replyID, ReplyUserID: &target, Visibility: model.NoteVisibilityPublic}
	}
	for _, n := range []*model.Note{
		mk("n_crt_1", t1.ID),
		mk("n_crt_2", t1.ID),
		mk("n_crt_3", t1.ID),
		mk("n_crt_4", t2.ID),
		mk("n_crt_5", author.ID), // 自己返信は無視
	} {
		require.NoError(t, testDB.Create(n).Error)
		defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, n.ID)
	}

	// viewer == author 自身は全 visibility 見える (= 既存挙動)
	rows, err := repo.CountReplyTargets(author.ID, author.ID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, t1.ID, rows[0].UserID)
	assert.EqualValues(t, 3, rows[0].Count)
	assert.Equal(t, t2.ID, rows[1].UserID)
	assert.EqualValues(t, 1, rows[1].Count)

	// limit <= 0 は 10 にデフォルト。
	rows2, err := repo.CountReplyTargets(author.ID, author.ID, 0)
	require.NoError(t, err)
	assert.Len(t, rows2, 2)
}

func TestNoteRepository_CountReplyTargets_VisibilityPushdown(t *testing.T) {
	repo := NewNoteRepository(testDB)
	author := insertTestUser(t, "u_crt_vp_a", "crtVpA")
	defer cleanupUser(t, author.ID)
	t1 := insertTestUser(t, "u_crt_vp_t1", "crtVpT1")
	defer cleanupUser(t, t1.ID)
	t2 := insertTestUser(t, "u_crt_vp_t2", "crtVpT2")
	defer cleanupUser(t, t2.ID)
	stranger := insertTestUser(t, "u_crt_vp_s", "crtVpS")
	defer cleanupUser(t, stranger.ID)
	follower := insertTestUser(t, "u_crt_vp_f", "crtVpF")
	defer cleanupUser(t, follower.ID)
	specified := insertTestUser(t, "u_crt_vp_sp", "crtVpSp")
	defer cleanupUser(t, specified.ID)

	// follower → author の follow を seed。
	require.NoError(t, testDB.Create(&model.Following{
		ID:           "f_crt_vp_1",
		FollowerID:   follower.ID,
		FolloweeID:   author.ID,
		FollowerHost: nil,
		FolloweeHost: nil,
	}).Error)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "f_crt_vp_1")

	replyID := "n_crt_vp_src"
	require.NoError(t, testDB.Create(&model.Note{ID: replyID, UserID: t1.ID, Visibility: model.NoteVisibilityPublic}).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, replyID)

	// author → t1: public 1 件, followers 1 件
	// author → t2: specified (visibleUserIds=[specified]) 1 件
	pubNote := &model.Note{ID: "n_crt_vp_pub", UserID: author.ID, ReplyID: &replyID, ReplyUserID: &t1.ID, Visibility: model.NoteVisibilityPublic}
	folNote := &model.Note{ID: "n_crt_vp_fol", UserID: author.ID, ReplyID: &replyID, ReplyUserID: &t1.ID, Visibility: model.NoteVisibilityFollowers}
	spNote := &model.Note{ID: "n_crt_vp_sp", UserID: author.ID, ReplyID: &replyID, ReplyUserID: &t2.ID, Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: model.StringArray{specified.ID}}
	for _, n := range []*model.Note{pubNote, folNote, spNote} {
		require.NoError(t, testDB.Create(n).Error)
		defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, n.ID)
	}

	// stranger (非フォロワー、非 specified) は public のみ集計。t1=1, t2 は出ない。
	rows, err := repo.CountReplyTargets(author.ID, stranger.ID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, t1.ID, rows[0].UserID)
	assert.EqualValues(t, 1, rows[0].Count)

	// anonymous (viewerID="") も同じく public のみ。
	rows, err = repo.CountReplyTargets(author.ID, "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, t1.ID, rows[0].UserID)
	assert.EqualValues(t, 1, rows[0].Count)

	// follower は public + followers が見える。t1=2, t2 は specified 対象外で出ない。
	rows, err = repo.CountReplyTargets(author.ID, follower.ID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, t1.ID, rows[0].UserID)
	assert.EqualValues(t, 2, rows[0].Count)

	// specified は visibleUserIds 経由で t2 への specified 1 件が見える + public 1 件。
	rows, err = repo.CountReplyTargets(author.ID, specified.ID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	// 順序は count DESC で同数なら不定 (どちらも 1 件)
	gotIDs := map[string]int64{}
	for _, r := range rows {
		gotIDs[r.UserID] = r.Count
	}
	assert.Equal(t, int64(1), gotIDs[t1.ID])
	assert.Equal(t, int64(1), gotIDs[t2.ID])

	// author 自身は全 visibility 集計可能。
	rows, err = repo.CountReplyTargets(author.ID, author.ID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
}

func TestNoteRepository_CountReplyTargets_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewNoteRepository(testDB.WithContext(ctx))
	_, err := repo.CountReplyTargets("me", "", 10)
	assert.Error(t, err)
}

// --- 追加テスト (#260 repository coverage: 0%関数) ---

func TestNoteRepository_ListByFileID(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_lbfi_1", "lbfiuser")
	defer cleanupUser(t, user.ID)

	text := "with file"
	n := &model.Note{
		ID:         "n_lbfi_1",
		UserID:     user.ID,
		Text:       &text,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
		FileIDs:    model.StringArray{"file_abc"},
	}
	require.NoError(t, repo.Create(n))
	defer cleanupNote(t, n.ID)

	got, err := repo.ListByFileID("file_abc", "", "", 10)
	require.NoError(t, err)
	assert.NotEmpty(t, got)

	empty, err := repo.ListByFileID("file_nonexistent", "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// frontend Paginator (cursor mode) との互換性確認: untilID 指定で
// id < untilID DESC, sinceID 指定で id > sinceID ASC を返すこと (#488)。
func TestNoteRepository_ListByFileID_Cursor(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_lbfi_2", "lbfiuser2")
	defer cleanupUser(t, user.ID)

	text := "with file cursor"
	for _, id := range []string{"n_lbfi_a", "n_lbfi_b", "n_lbfi_c", "n_lbfi_d"} {
		n := &model.Note{
			ID:         id,
			UserID:     user.ID,
			Text:       &text,
			Visibility: model.NoteVisibilityPublic,
			Reactions:  datatypes.JSON([]byte("{}")),
			FileIDs:    model.StringArray{"file_pg"},
		}
		require.NoError(t, repo.Create(n))
		defer cleanupNote(t, n.ID)
	}

	// untilID="n_lbfi_c" → id < "n_lbfi_c" を DESC 2 件 = [b, a]
	rows, err := repo.ListByFileID("file_pg", "", "n_lbfi_c", 2)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "n_lbfi_b", rows[0].ID)
	assert.Equal(t, "n_lbfi_a", rows[1].ID)

	// sinceID="n_lbfi_b" → id > "n_lbfi_b" を ASC 2 件 = [c, d]
	rows, err = repo.ListByFileID("file_pg", "n_lbfi_b", "", 2)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "n_lbfi_c", rows[0].ID)
	assert.Equal(t, "n_lbfi_d", rows[1].ID)
}

// TestNoteRepository_MentionsFileIdsGINIndexes verifies that the #1427 GIN
// indexes (migration 000054 / 000055) are created and actually usable for the
// array-containment (@>) queries behind ListMentions / ListByFileID. これらが
// 落ちると seq scan に戻り、note テーブル肥大に比例して静かに性能退行するため
// regression guard として固定する。
func TestNoteRepository_MentionsFileIdsGINIndexes(t *testing.T) {
	// 1. index が存在し GIN 型であること (CONCURRENTLY migration が適用された
	//    ことの確認も兼ねる)。
	for _, idx := range []string{"IDX_note_mentions", "IDX_note_fileIds"} {
		indexdef := indexDef(t, "note", idx)
		require.NotEmpty(t, indexdef, "GIN index %s must exist (migration applied)", idx)
		assert.Contains(t, indexdef, "USING gin", "%s must be a GIN index", idx)
	}

	// 2. planner が @> containment に GIN index を選べること。小さな test
	//    テーブルでは planner が seq scan を選ぶため、同一接続上で
	//    enable_seqscan=off を効かせて index 経路を強制し、plan に index 名が
	//    現れることを確認する (index が無ければ強制しても seq scan のまま)。
	//    SET LOCAL は transaction スコープなので Begin/Rollback で閉じる。
	checkPlanUsesIndex := func(query, arg, indexName string) {
		tx := testDB.Begin()
		defer tx.Rollback()
		require.NoError(t, tx.Exec(`SET LOCAL enable_seqscan = off`).Error)
		var lines []string
		require.NoError(t, tx.Raw(`EXPLAIN `+query, arg).Scan(&lines).Error)
		plan := strings.Join(lines, "\n")
		assert.Contains(t, plan, indexName,
			"@> query should use %s under enable_seqscan=off; plan was:\n%s", indexName, plan)
	}
	checkPlanUsesIndex(
		`SELECT * FROM "note" WHERE "mentions" @> ARRAY[?]::varchar[]`, "u1", "IDX_note_mentions")
	checkPlanUsesIndex(
		`SELECT * FROM "note" WHERE "fileIds" @> ARRAY[?]::varchar[]`, "f1", "IDX_note_fileIds")
}

func TestNoteRepository_IncrementUserNotesCount(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_n_inc_2", "incuser2")
	defer cleanupUser(t, user.ID)

	require.NoError(t, repo.IncrementUserNotesCount(user.ID, 3))
	require.NoError(t, repo.IncrementUserNotesCount(user.ID, -1))

	// 値はuserrepoから確認
	ur := NewUserRepository(testDB)
	got, err := ur.FindByID(user.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, got.NotesCount)
}

func TestNoteRepository_ListLocalTimeline(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_llt_1", "lltuser")
	defer cleanupUser(t, user.ID)

	text := "local"
	n := &model.Note{
		ID:         "n_llt_1",
		UserID:     user.ID,
		Text:       &text,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(n))
	defer cleanupNote(t, n.ID)

	filter := model.TimelineDBFilter{}
	rows, err := repo.ListLocalTimeline(50, "", "", filter)
	require.NoError(t, err)
	ids := make(map[string]bool, len(rows))
	for _, r := range rows {
		ids[r.ID] = true
	}
	assert.True(t, ids[n.ID])

	// since/until分岐を通す
	_, err = repo.ListLocalTimeline(50, "nonexistent", "", filter)
	require.NoError(t, err)
	_, err = repo.ListLocalTimeline(50, "", "nonexistent", filter)
	require.NoError(t, err)
}

func TestNoteRepository_ListGlobalTimeline(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_lgt_1", "lgtuser")
	defer cleanupUser(t, user.ID)

	text := "global"
	n := &model.Note{
		ID:         "n_lgt_1",
		UserID:     user.ID,
		Text:       &text,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, repo.Create(n))
	defer cleanupNote(t, n.ID)

	filter := model.TimelineDBFilter{}
	rows, err := repo.ListGlobalTimeline(50, "", "", filter)
	require.NoError(t, err)
	ids := make(map[string]bool, len(rows))
	for _, r := range rows {
		ids[r.ID] = true
	}
	assert.True(t, ids[n.ID])

	// since/until分岐
	_, err = repo.ListGlobalTimeline(50, "nonexistent", "", filter)
	require.NoError(t, err)
	_, err = repo.ListGlobalTimeline(50, "", "nonexistent", filter)
	require.NoError(t, err)
}

// TestNoteRepository_SinceIDFlipsOrderASC verifies that sinceID-only
// pagination queries return rows in ASC order, matching upstream Misskey's
// QueryService.makePaginationQuery. Two representative note functions are
// exercised; the helper itself has unit coverage in pagination_test.go.
// Regression guard for #384.
func TestNoteRepository_SinceIDFlipsOrderASC(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "asc_u", "ascuser")
	defer cleanupUser(t, user.ID)

	// IDの lexicographic 順に作成: a < b < c
	for _, id := range []string{"asc_n_a", "asc_n_b", "asc_n_c"} {
		n := &model.Note{ID: id, UserID: user.ID, Visibility: "public", Tags: []string{"ascflip"}, Reactions: datatypes.JSON([]byte("{}"))}
		require.NoError(t, testDB.Create(n).Error)
		defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, id)
	}

	// sinceID単独指定: ASC順 (古い→新しい)。asc_n_a より大なので b, c が返る想定。
	notes, err := repo.SearchByTag([][]string{{"ascflip"}}, "", 10, "asc_n_a", "", model.NoteSearchTagFilter{})
	require.NoError(t, err)
	require.Len(t, notes, 2)
	assert.Equal(t, "asc_n_b", notes[0].ID)
	assert.Equal(t, "asc_n_c", notes[1].ID)

	// untilID単独指定: DESC順 (既存挙動)。asc_n_c より小なので b, a が返る。
	notes, err = repo.SearchByTag([][]string{{"ascflip"}}, "", 10, "", "asc_n_c", model.NoteSearchTagFilter{})
	require.NoError(t, err)
	require.Len(t, notes, 2)
	assert.Equal(t, "asc_n_b", notes[0].ID)
	assert.Equal(t, "asc_n_a", notes[1].ID)

	// ListLocalTimeline も同様 (timelineFilter経路)。
	filter := model.TimelineDBFilter{ViewerID: user.ID}
	notes, err = repo.ListLocalTimeline(10, "asc_n_a", "", filter)
	require.NoError(t, err)
	// 関係する3件の中で b, c が order ASC で返ること (他ユーザーのpublicノートも混ざる可能性あり)
	ownOnly := make([]*model.Note, 0, 2)
	for _, n := range notes {
		if strings.HasPrefix(n.ID, "asc_n_") {
			ownOnly = append(ownOnly, n)
		}
	}
	require.Len(t, ownOnly, 2)
	assert.Equal(t, "asc_n_b", ownOnly[0].ID)
	assert.Equal(t, "asc_n_c", ownOnly[1].ID)
}

// #403: nodeinfo usage 用 count clauseを実 PostgreSQL で検証する。
func TestNoteRepository_CountLocalNotes_CountLocalComments(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "n_cnt_u", "cntuser")
	defer cleanupUser(t, user.ID)

	// local post 1本
	post := &model.Note{ID: "cnt_post1", UserID: user.ID, Visibility: "public"}
	require.NoError(t, testDB.Create(post).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, post.ID)

	// local reply 1本
	replyID := "cnt_post1"
	reply := &model.Note{ID: "cnt_reply1", UserID: user.ID, Visibility: "public", ReplyID: &replyID, ReplyUserID: &user.ID}
	require.NoError(t, testDB.Create(reply).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, reply.ID)

	// remote note (count 対象外)
	remoteHost := "remote.example"
	remote := &model.Note{ID: "cnt_remote1", UserID: user.ID, Visibility: "public", UserHost: &remoteHost}
	require.NoError(t, testDB.Create(remote).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, remote.ID)

	// fixture 追加前の数との delta を確認する (他 test fixture 残存があり得る)。
	posts, err := repo.CountLocalNotes()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, posts, int64(2)) // local post + reply

	comments, err := repo.CountLocalComments()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, comments, int64(1)) // reply だけ
}

func TestNoteRepository_CountLocal_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewNoteRepository(testDB.WithContext(ctx))
	_, err := repo.CountLocalNotes()
	assert.Error(t, err)
	_, err = repo.CountLocalComments()
	assert.Error(t, err)
}

// ListPublicNotesForFeed は RSS/Atom/JSON フィードに載せる note を返す。
// upstream FeedService と同じく renote を除き、visibility ∈ {public, home} を
// 新しい順に取る。フォロワー限定・ダイレクトは公開フィードに出してはいけない。
func TestNoteRepository_ListPublicNotesForFeed(t *testing.T) {
	repo := NewNoteRepository(testDB)
	u := insertTestUser(t, "u_feed", "feeduser")
	defer cleanupUser(t, u.ID)
	other := insertTestUser(t, "u_feed2", "feeduser2")
	defer cleanupUser(t, other.ID)
	defer testDB.Exec(`DELETE FROM "note" WHERE "userId" IN (?, ?)`, u.ID, other.ID)

	mk := func(id, userID string, vis model.NoteVisibility) {
		require.NoError(t, repo.Create(&model.Note{ID: id, UserID: userID, Visibility: vis}))
	}
	mk("feed_pub1", u.ID, model.NoteVisibilityPublic)
	mk("feed_home", u.ID, model.NoteVisibilityHome)
	mk("feed_fol", u.ID, model.NoteVisibilityFollowers)
	mk("feed_spec", u.ID, model.NoteVisibilitySpecified)
	mk("feed_other", other.ID, model.NoteVisibilityPublic)
	// renote は除外される。
	src := "feed_pub1"
	require.NoError(t, repo.Create(&model.Note{
		ID: "feed_rn", UserID: u.ID, Visibility: model.NoteVisibilityPublic, RenoteID: &src,
	}))

	got, err := repo.ListPublicNotesForFeed(u.ID, 50)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, n := range got {
		ids[n.ID] = true
	}
	assert.True(t, ids["feed_pub1"], "public は含む")
	assert.True(t, ids["feed_home"], "home は含む")
	assert.False(t, ids["feed_fol"], "followers 限定は出さない")
	assert.False(t, ids["feed_spec"], "ダイレクトは出さない")
	assert.False(t, ids["feed_other"], "他人の note は含まない")
	assert.False(t, ids["feed_rn"], "renote は含まない")

	// 新しい順。
	require.GreaterOrEqual(t, len(got), 2)
	assert.Greater(t, got[0].ID, got[1].ID)

	// limit が効く。
	limited, err := repo.ListPublicNotesForFeed(u.ID, 1)
	require.NoError(t, err)
	assert.Len(t, limited, 1)
}

// 自分宛てではない specified (DM) はリスト TL に漏らさない。
// 自分宛てを出す変更 (upstream generateVisibilityQuery 準拠) で、他人宛ての DM
// まで見えるようになっていないことを確かめる。
func TestNoteRepository_ListByUserList_OtherSpecifiedHidden(t *testing.T) {
	repo := NewNoteRepository(testDB)
	viewer := insertTestUser(t, "u_osp_v", "ospV")
	defer cleanupUser(t, viewer.ID)
	member := insertTestUser(t, "u_osp_m", "ospM")
	defer cleanupUser(t, member.ID)
	third := insertTestUser(t, "u_osp_t", "ospT")
	defer cleanupUser(t, third.ID)
	defer testDB.Exec(`DELETE FROM "note" WHERE "userId" IN (?, ?, ?)`, viewer.ID, member.ID, third.ID)

	listID := "osp_list"
	require.NoError(t, testDB.Create(&model.UserList{ID: listID, UserID: viewer.ID, Name: "l"}).Error)
	defer testDB.Exec(`DELETE FROM "user_list" WHERE id = ?`, listID)
	require.NoError(t, testDB.Create(&model.UserListMembership{ID: "osp_mem", UserListID: listID, UserID: member.ID}).Error)
	defer testDB.Exec(`DELETE FROM "user_list_membership" WHERE id = ?`, "osp_mem")

	require.NoError(t, repo.Create(&model.Note{
		ID: "osp_mine", UserID: member.ID, Visibility: "specified",
		VisibleUserIDs: model.StringArray{viewer.ID},
	}))
	require.NoError(t, repo.Create(&model.Note{
		ID: "osp_other", UserID: member.ID, Visibility: "specified",
		VisibleUserIDs: model.StringArray{third.ID},
	}))

	got, err := repo.ListByUserList(listID, 50, "", "", model.TimelineDBFilter{ViewerID: viewer.ID})
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, n := range got {
		ids[n.ID] = true
	}
	assert.True(t, ids["osp_mine"], "自分宛ての DM は出る")
	assert.False(t, ids["osp_other"], "他人宛ての DM は出さない")
}

// ListByUserIDFiltered / ListPublicNotesForFeed のカーソル分岐を覆う。
func TestNoteRepository_ListByUserIDFiltered_Cursor(t *testing.T) {
	repo := NewNoteRepository(testDB)
	u := insertTestUser(t, "u_cur", "curuser")
	defer cleanupUser(t, u.ID)
	defer testDB.Exec(`DELETE FROM "note" WHERE "userId" = ?`, u.ID)

	for _, id := range []string{"cur_1", "cur_2", "cur_3"} {
		require.NoError(t, repo.Create(&model.Note{ID: id, UserID: u.ID, Visibility: model.NoteVisibilityPublic}))
	}

	got, err := repo.ListByUserIDFiltered(u.ID, "", "cur_3", "", 50, false, false, true, false)
	require.NoError(t, err)
	for _, n := range got {
		assert.Less(t, n.ID, "cur_3", "untilID より前だけ返る")
	}

	got, err = repo.ListByUserIDFiltered(u.ID, "", "", "cur_1", 50, false, false, true, false)
	require.NoError(t, err)
	for _, n := range got {
		assert.Greater(t, n.ID, "cur_1", "sinceID より後だけ返る")
	}
}

// HideFollowersOnlyReplyFromNonFollowee: 返信先が followers 限定の投稿で、
// viewer がその投稿者をフォローしておらず自分でもないノートを SQL 段階で弾く。
// post-fetch だけだと件数が limit を割って DB fallback がすり抜ける。
func TestApplyTimelineFilter_HideFollowersOnlyReply(t *testing.T) {
	repo := NewNoteRepository(testDB)
	viewer := insertTestUser(t, "u_hfr_v", "hfrV")
	defer cleanupUser(t, viewer.ID)
	author := insertTestUser(t, "u_hfr_a", "hfrA")
	defer cleanupUser(t, author.ID)
	target := insertTestUser(t, "u_hfr_t", "hfrT")
	defer cleanupUser(t, target.ID)
	defer testDB.Exec(`DELETE FROM "note" WHERE "userId" IN (?, ?, ?)`, viewer.ID, author.ID, target.ID)

	// target の followers 限定投稿への author の返信。
	require.NoError(t, repo.Create(&model.Note{ID: "hfr_src", UserID: target.ID, Visibility: model.NoteVisibilityFollowers}))
	src, targetID := "hfr_src", target.ID
	require.NoError(t, repo.Create(&model.Note{
		ID: "hfr_reply", UserID: author.ID, Visibility: model.NoteVisibilityPublic,
		ReplyID: &src, ReplyUserID: &targetID,
	}))
	require.NoError(t, repo.Create(&model.Note{ID: "hfr_plain", UserID: author.ID, Visibility: model.NoteVisibilityPublic}))

	f := model.TimelineDBFilter{ViewerID: viewer.ID, HideFollowersOnlyReplyFromNonFollowee: true}
	got, err := repo.ListGlobalTimeline(50, "", "", f)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, n := range got {
		ids[n.ID] = true
	}
	assert.True(t, ids["hfr_plain"], "通常の投稿は残る")
	assert.False(t, ids["hfr_reply"], "未フォローの followers 投稿への返信は弾く")

	// viewer が target をフォローしていれば出る。
	require.NoError(t, testDB.Create(&model.Following{
		ID: "hfr_fw", FollowerID: viewer.ID, FolloweeID: target.ID,
	}).Error)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "hfr_fw")

	got, err = repo.ListGlobalTimeline(50, "", "", f)
	require.NoError(t, err)
	ids = map[string]bool{}
	for _, n := range got {
		ids[n.ID] = true
	}
	assert.True(t, ids["hfr_reply"], "フォローしていれば出る")
}

// withReplies=false でも自己スレッド (自分の投稿への自分の返信) は残す。
// upstream の userTimeline は fanout 側で `replyUserId !== userId` を条件に
// 振り分けており、自己スレッドは userTimeline 側に入るため出る。
func TestNoteRepository_ListByUserIDFiltered_SelfThreadKept(t *testing.T) {
	repo := NewNoteRepository(testDB)
	u := insertTestUser(t, "u_stf", "stfuser")
	defer cleanupUser(t, u.ID)
	other := insertTestUser(t, "u_stf2", "stfuser2")
	defer cleanupUser(t, other.ID)
	defer testDB.Exec(`DELETE FROM "note" WHERE "userId" IN (?, ?)`, u.ID, other.ID)

	require.NoError(t, repo.Create(&model.Note{ID: "stf_root", UserID: u.ID, Visibility: model.NoteVisibilityPublic}))
	require.NoError(t, repo.Create(&model.Note{ID: "stf_src", UserID: other.ID, Visibility: model.NoteVisibilityPublic}))
	root, otherID, selfID := "stf_root", other.ID, u.ID
	// 自己スレッド。
	require.NoError(t, repo.Create(&model.Note{
		ID: "stf_self", UserID: u.ID, Visibility: model.NoteVisibilityPublic,
		ReplyID: &root, ReplyUserID: &selfID,
	}))
	// 他人宛ての返信。
	src := "stf_src"
	require.NoError(t, repo.Create(&model.Note{
		ID: "stf_other", UserID: u.ID, Visibility: model.NoteVisibilityPublic,
		ReplyID: &src, ReplyUserID: &otherID,
	}))

	got, err := repo.ListByUserIDFiltered(u.ID, "", "", "", 50, false, false, true, false)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, n := range got {
		ids[n.ID] = true
	}
	assert.True(t, ids["stf_root"])
	assert.True(t, ids["stf_self"], "自己スレッドは withReplies=false でも出る")
	assert.False(t, ids["stf_other"], "他人宛ての返信は出さない")

	// withReplies=true なら両方出る。
	got, err = repo.ListByUserIDFiltered(u.ID, "", "", "", 50, false, true, true, false)
	require.NoError(t, err)
	ids = map[string]bool{}
	for _, n := range got {
		ids[n.ID] = true
	}
	assert.True(t, ids["stf_other"])
}

// withChannelNotes=true でも本人以外にはセンシティブチャンネルの投稿を出さない
// (upstream users/notes.ts の `channel.isSensitive = false`)。
func TestNoteRepository_ListByUserIDFiltered_SensitiveChannel(t *testing.T) {
	repo := NewNoteRepository(testDB)
	u := insertTestUser(t, "u_sch", "schuser")
	defer cleanupUser(t, u.ID)
	defer testDB.Exec(`DELETE FROM "note" WHERE "userId" = ?`, u.ID)

	chOwner := u.ID
	require.NoError(t, testDB.Create(&model.Channel{ID: "sch_ns", UserID: &chOwner, Name: "ns", IsSensitive: false}).Error)
	require.NoError(t, testDB.Create(&model.Channel{ID: "sch_s", UserID: &chOwner, Name: "s", IsSensitive: true}).Error)
	defer testDB.Exec(`DELETE FROM "channel" WHERE id IN ('sch_ns','sch_s')`)

	ns, sensitive := "sch_ns", "sch_s"
	require.NoError(t, repo.Create(&model.Note{ID: "sch_plain", UserID: u.ID, Visibility: model.NoteVisibilityPublic}))
	require.NoError(t, repo.Create(&model.Note{ID: "sch_n1", UserID: u.ID, Visibility: model.NoteVisibilityPublic, ChannelID: &ns}))
	require.NoError(t, repo.Create(&model.Note{ID: "sch_n2", UserID: u.ID, Visibility: model.NoteVisibilityPublic, ChannelID: &sensitive}))

	// 他人が見る -> センシティブチャンネルの投稿は出ない。
	got, err := repo.ListByUserIDFiltered(u.ID, "viewer_other", "", "", 50, false, false, true, true)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, n := range got {
		ids[n.ID] = true
	}
	assert.True(t, ids["sch_plain"])
	assert.True(t, ids["sch_n1"])
	assert.False(t, ids["sch_n2"], "他人にはセンシティブチャンネルの投稿を出さない")

	// 本人が見る -> 出る。
	got, err = repo.ListByUserIDFiltered(u.ID, u.ID, "", "", 50, false, false, true, true)
	require.NoError(t, err)
	ids = map[string]bool{}
	for _, n := range got {
		ids[n.ID] = true
	}
	assert.True(t, ids["sch_n2"], "本人には出る")
}

// ListPublicByUserID は visibility ∈ {public, home} かつ !localOnly の note のみを
// id DESC / cursor で返す (AP outbox 用, #1878)。
func TestNoteRepository_ListPublicByUserID(t *testing.T) {
	repo := NewNoteRepository(testDB)
	u := insertTestUser(t, "u_obx", "obxuser")
	defer cleanupUser(t, u.ID)
	defer testDB.Exec(`DELETE FROM "note" WHERE "userId" = ?`, u.ID)

	mk := func(id string, vis model.NoteVisibility, localOnly bool) {
		require.NoError(t, repo.Create(&model.Note{ID: id, UserID: u.ID, Visibility: vis, LocalOnly: localOnly}))
	}
	mk("obx_pub1", model.NoteVisibilityPublic, false)
	mk("obx_pub2", model.NoteVisibilityPublic, false)
	mk("obx_home", model.NoteVisibilityHome, false)
	mk("obx_fol", model.NoteVisibilityFollowers, false)
	mk("obx_spec", model.NoteVisibilitySpecified, false)
	mk("obx_lo", model.NoteVisibilityPublic, true)

	all, err := repo.ListPublicByUserID(u.ID, "", "", 50)
	require.NoError(t, err)
	got := map[string]bool{}
	for _, n := range all {
		got[n.ID] = true
	}
	assert.Len(t, all, 3, "public x2 + home only")
	assert.True(t, got["obx_pub1"])
	assert.True(t, got["obx_home"])
	assert.False(t, got["obx_fol"], "followers excluded")
	assert.False(t, got["obx_spec"], "specified excluded")
	assert.False(t, got["obx_lo"], "localOnly excluded")

	// untilID cursor: id < "obx_pub2"。
	page, err := repo.ListPublicByUserID(u.ID, "obx_pub2", "", 50)
	require.NoError(t, err)
	for _, n := range page {
		assert.Less(t, n.ID, "obx_pub2", "cursor excludes id >= until")
	}

	// sinceID cursor: id > "obx_home" → obx_pub1, obx_pub2 を ASC で返す
	// (paginationOrder が since-only で ASC に倒す。handler 側で reverse される)。
	sinceRows, err := repo.ListPublicByUserID(u.ID, "", "obx_home", 50)
	require.NoError(t, err)
	require.Len(t, sinceRows, 2)
	assert.Equal(t, "obx_pub1", sinceRows[0].ID, "since-only is ASC")
	assert.Equal(t, "obx_pub2", sinceRows[1].ID)
}

// #2069 (upstream #16119): SearchByFilter は RangeStartID/RangeEndID (投稿日時範囲を
// aidx prefix に変換した境界) で cursor とは独立に絞り込む。
func TestNoteRepository_SearchByFilter_DateRange(t *testing.T) {
	repo := NewNoteRepository(testDB)
	u := insertTestUser(t, "u_dr_1", "drangeuser")
	defer cleanupUser(t, u.ID)
	idGen, _ := id.NewGenerator("aidx")
	txt := "drangesearchabletoken"
	mk := func(ts time.Time) string {
		nid := idGen.Generate(ts)
		require.NoError(t, repo.Create(&model.Note{
			ID: nid, UserID: u.ID, Text: &txt,
			Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
		}))
		t.Cleanup(func() { cleanupNote(t, nid) })
		return nid
	}
	id2021 := mk(time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC))
	id2022 := mk(time.Date(2022, 6, 1, 0, 0, 0, 0, time.UTC))
	id2023 := mk(time.Date(2023, 6, 1, 0, 0, 0, 0, time.UTC))

	// 2022 年のみを範囲指定 (start 2022-01-01 / end 2022-12-31)。
	rangeStart := id.AidxCutoffPrefix(time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC))
	rangeEnd := id.AidxCutoffPrefix(time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC))
	out, err := repo.SearchByFilter(model.NoteSearchFilter{
		Query: "drangesearchabletoken", RangeStartID: rangeStart, RangeEndID: rangeEnd, Limit: 50,
	})
	require.NoError(t, err)
	got := idsOf(out)
	assert.Contains(t, got, id2022, "2022 の note は範囲内 (#2069)")
	assert.NotContains(t, got, id2021, "2021 は範囲外")
	assert.NotContains(t, got, id2023, "2023 は範囲外")
}

// #2106 L4 / #2215: ListPublicNotes は public/非 localOnly note を返し、
// local/reply/renote/withFiles/poll の各 filter を適用する。
func TestNoteRepository_ListPublicNotes(t *testing.T) {
	repo := NewNoteRepository(testDB)
	user := insertTestUser(t, "u_lpn_1", "lpnuser")
	defer cleanupUser(t, user.ID)

	text := "x"
	host := "remote.example"
	mk := func(id string, mut func(*model.Note)) *model.Note {
		n := &model.Note{ID: id, UserID: user.ID, Text: &text, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))}
		mut(n)
		require.NoError(t, repo.Create(n))
		return n
	}
	// reply/renote の FK 制約を満たすため参照先 note を先に作る。
	refNote := mk("n_lpn_ref", func(n *model.Note) {})
	ref := refNote.ID
	plain := mk("n_lpn_plain", func(n *model.Note) {})
	remote := mk("n_lpn_remote", func(n *model.Note) { n.UserHost = &host })
	reply := mk("n_lpn_reply", func(n *model.Note) { n.ReplyID = &ref })
	renote := mk("n_lpn_renote", func(n *model.Note) { n.RenoteID = &ref })
	files := mk("n_lpn_files", func(n *model.Note) { n.FileIDs = model.StringArray{"f1"} })
	poll := mk("n_lpn_poll", func(n *model.Note) { n.HasPoll = true })
	localonly := mk("n_lpn_localonly", func(n *model.Note) { n.LocalOnly = true })
	followers := mk("n_lpn_followers", func(n *model.Note) { n.Visibility = model.NoteVisibilityFollowers })
	for _, n := range []*model.Note{refNote, plain, remote, reply, renote, files, poll, localonly, followers} {
		defer cleanupNote(t, n.ID)
	}

	has := func(f model.PublicNotesFilter, id string) bool {
		rows, err := repo.ListPublicNotes(f, 50, "", "")
		require.NoError(t, err)
		for _, r := range rows {
			if r.ID == id {
				return true
			}
		}
		return false
	}
	tr, fa := true, false

	// no filter: public/非 localOnly のみ。localOnly / followers は除外。
	assert.True(t, has(model.PublicNotesFilter{}, plain.ID))
	assert.True(t, has(model.PublicNotesFilter{}, remote.ID))
	assert.False(t, has(model.PublicNotesFilter{}, localonly.ID))
	assert.False(t, has(model.PublicNotesFilter{}, followers.ID))
	// local: userHost IS NULL のみ。
	assert.True(t, has(model.PublicNotesFilter{Local: true}, plain.ID))
	assert.False(t, has(model.PublicNotesFilter{Local: true}, remote.ID))
	// reply true/false。
	assert.True(t, has(model.PublicNotesFilter{Reply: &tr}, reply.ID))
	assert.False(t, has(model.PublicNotesFilter{Reply: &tr}, plain.ID))
	assert.True(t, has(model.PublicNotesFilter{Reply: &fa}, plain.ID))
	assert.False(t, has(model.PublicNotesFilter{Reply: &fa}, reply.ID))
	// renote true/false。
	assert.True(t, has(model.PublicNotesFilter{Renote: &tr}, renote.ID))
	assert.True(t, has(model.PublicNotesFilter{Renote: &fa}, plain.ID))
	// withFiles true/false。
	assert.True(t, has(model.PublicNotesFilter{WithFiles: &tr}, files.ID))
	assert.True(t, has(model.PublicNotesFilter{WithFiles: &fa}, plain.ID))
	// poll true/false。
	assert.True(t, has(model.PublicNotesFilter{Poll: &tr}, poll.ID))
	assert.True(t, has(model.PublicNotesFilter{Poll: &fa}, plain.ID))

	// since/until 分岐。
	_, err := repo.ListPublicNotes(model.PublicNotesFilter{}, 50, "nonexistent", "")
	require.NoError(t, err)
	_, err = repo.ListPublicNotes(model.PublicNotesFilter{}, 50, "", "nonexistent")
	require.NoError(t, err)
}

// LIKE パターンのメタ文字はリテラル照合になる (#2514、upstream の
// sql-like-escape.ts 相当)。素通しだと利用者の入力した % / _ がワイルド
// カードとして解釈され、無関係な note が誤ヒットする。
func TestNoteRepository_SearchByFilter_EscapesLikePattern(t *testing.T) {
	repo := NewNoteRepository(testDB)
	u := insertTestUser(t, "u_selike_1", "selikeuser")
	defer cleanupUser(t, u.ID)

	pct := "progress is 100% done"
	plain := "progress is 100 percent done"
	usc := "snake_case word here"
	uscTrap := "snakeycase word here"
	bsl := `path c:\temp noted`
	notes := []*model.Note{
		{ID: "n_sel_1", UserID: u.ID, Text: &pct, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_sel_2", UserID: u.ID, Text: &plain, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_sel_3", UserID: u.ID, Text: &usc, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_sel_4", UserID: u.ID, Text: &uscTrap, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: "n_sel_5", UserID: u.ID, Text: &bsl, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))},
	}
	for _, n := range notes {
		require.NoError(t, repo.Create(n))
		defer cleanupNote(t, n.ID)
	}

	// "%" はリテラル。無エスケープだと '%100% done%' が「100 の後に任意列 +
	// " done"」になり n_sel_2 も誤ヒットする。
	out, err := repo.SearchByFilter(model.NoteSearchFilter{Query: "100% done", Limit: 10})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"n_sel_1"}, idsOf(out))

	// "_" もリテラル。無エスケープだと任意 1 文字に化けて snakeycase (n_sel_4)
	// も誤ヒットする。
	out, err = repo.SearchByFilter(model.NoteSearchFilter{Query: "snake_case", Limit: 10})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"n_sel_3"}, idsOf(out))

	// backslash を含むクエリもリテラル照合できる。
	out, err = repo.SearchByFilter(model.NoteSearchFilter{Query: `c:\temp`, Limit: 10})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"n_sel_5"}, idsOf(out))

	// lower() + LIKE 化後も case-insensitive を維持する (大文字クエリで当たる)。
	out, err = repo.SearchByFilter(model.NoteSearchFilter{Query: "SNAKE_CASE", Limit: 10})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"n_sel_3"}, idsOf(out))
}

// **返信先の note が見えないなら集計に出さない。** 集計は非正規化列
// `"replyUserId"` を読むだけなので、返信元 (author の reply) だけを gate すると
// 「返信元は public だが返信先は followers」の組で相手の身元が集計値に漏れる。
// upstream も generateVisibilityQuery を 2 つのクエリ両方に足している。
func TestNoteRepository_CountReplyTargets_ReplyTargetVisibility(t *testing.T) {
	repo := NewNoteRepository(testDB)
	author := insertTestUser(t, "u_crt_rt_a", "crtRtA")
	defer cleanupUser(t, author.ID)
	hidden := insertTestUser(t, "u_crt_rt_h", "crtRtH")
	defer cleanupUser(t, hidden.ID)
	shown := insertTestUser(t, "u_crt_rt_v", "crtRtV")
	defer cleanupUser(t, shown.ID)
	stranger := insertTestUser(t, "u_crt_rt_s", "crtRtS")
	defer cleanupUser(t, stranger.ID)

	// 返信先 2 件: hidden の followers note (stranger には見えない) と
	// shown の public note。
	hiddenTarget := "n_crt_rt_hidden"
	shownTarget := "n_crt_rt_shown"
	require.NoError(t, testDB.Create(&model.Note{ID: hiddenTarget, UserID: hidden.ID, Visibility: model.NoteVisibilityFollowers}).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, hiddenTarget)
	require.NoError(t, testDB.Create(&model.Note{ID: shownTarget, UserID: shown.ID, Visibility: model.NoteVisibilityPublic}).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, shownTarget)

	// author の返信はどちらも public = **返信元だけ見ると両方とも集計対象**。
	for _, n := range []*model.Note{
		{ID: "n_crt_rt_r1", UserID: author.ID, ReplyID: &hiddenTarget, ReplyUserID: &hidden.ID, Visibility: model.NoteVisibilityPublic},
		{ID: "n_crt_rt_r2", UserID: author.ID, ReplyID: &shownTarget, ReplyUserID: &shown.ID, Visibility: model.NoteVisibilityPublic},
	} {
		require.NoError(t, testDB.Create(n).Error)
		defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, n.ID)
	}

	// stranger からは shown だけが見える。hidden は返信先が followers なので出ない。
	rows, err := repo.CountReplyTargets(author.ID, stranger.ID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1, "返信先が見えない相手が集計に出ている")
	assert.Equal(t, shown.ID, rows[0].UserID)

	// anonymous も同じ。
	rows, err = repo.CountReplyTargets(author.ID, "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, shown.ID, rows[0].UserID)

	// 返信先の author 自身からは両方見える (自分の followers note は見える)。
	rows, err = repo.CountReplyTargets(author.ID, hidden.ID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2, "返信先の当人からは見えるはず")
}

// **直近 1000 件を超える返信は集計に入らない。** upstream
// get-frequently-replied-users.ts の `ORDER BY note.id DESC LIMIT 1000` と同じ。
// この endpoint は未認証で任意の userId に対して叩けてレート制限も無いので、
// 上限が無いと投稿数に比例して重くなる (相関 EXISTS が 1 行ごとの PK 探索になる)。
func TestNoteRepository_CountReplyTargets_ScanLimit(t *testing.T) {
	// **窓のサイズはリテラルで書く。** `recentReplyScanLimit` を fixture に使うと、
	// 定数を変える変異にテスト側が追随して境界がずれても落ちない (実測で 7 / 2000 の
	// どちらでも PASS した)。同じブランチの emoji 側で踏んだのと同じ型。
	const window = 1000
	assert.Equal(t, window, recentReplyScanLimit, "upstream の limit(1000) と揃える")

	repo := NewNoteRepository(testDB)
	author := insertTestUser(t, "u_crt_lim_a", "crtLimA")
	defer cleanupUser(t, author.ID)
	recent := insertTestUser(t, "u_crt_lim_r", "crtLimR")
	defer cleanupUser(t, recent.ID)
	old := insertTestUser(t, "u_crt_lim_o", "crtLimO")
	defer cleanupUser(t, old.ID)
	viewer := insertTestUser(t, "u_crt_lim_v", "crtLimV")
	defer cleanupUser(t, viewer.ID)

	// **返信先は 1 件ずつ別のノートにする。** 同じノートへ 1000 回にすると、
	// upstream は `note.id IN (replyIds)` で畳んで 1、mk-go の COUNT(*) は 1000 と
	// なり、件数を assert すると**既知の乖離が最大に開いた形で固定される**。
	// 別々にすれば両者とも 1000 で一致するので、乖離に依存せず件数を見られる。
	oldTarget := "n_crt_lim_to"
	require.NoError(t, testDB.Create(&model.Note{ID: oldTarget, UserID: old.ID, Visibility: model.NoteVisibilityPublic}).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, oldTarget)
	defer testDB.Exec(`DELETE FROM "note" WHERE "userId" = ?`, recent.ID)
	recentTargets := make([]*model.Note, 0, window)
	for i := 0; i < window; i++ {
		recentTargets = append(recentTargets, &model.Note{
			ID: fmt.Sprintf("n_crt_lim_t%04d", i), UserID: recent.ID, Visibility: model.NoteVisibilityPublic,
		})
	}
	require.NoError(t, testDB.CreateInBatches(recentTargets, 500).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE "userId" = ?`, author.ID)

	// id は昇順で新しくなる。old 宛を先に (= 古い側) 5 件、recent 宛を後に 1000 件。
	notes := make([]*model.Note, 0, window+5)
	for i := 0; i < 5; i++ {
		notes = append(notes, &model.Note{
			ID: fmt.Sprintf("n_crt_lim_a%04d", i), UserID: author.ID,
			ReplyID: &oldTarget, ReplyUserID: &old.ID, Visibility: model.NoteVisibilityPublic,
		})
	}
	for i := 0; i < window; i++ {
		tid := recentTargets[i].ID
		notes = append(notes, &model.Note{
			ID: fmt.Sprintf("n_crt_lim_b%04d", i), UserID: author.ID,
			ReplyID: &tid, ReplyUserID: &recent.ID, Visibility: model.NoteVisibilityPublic,
		})
	}
	require.NoError(t, testDB.CreateInBatches(notes, 500).Error)

	rows, err := repo.CountReplyTargets(author.ID, viewer.ID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1, "1000 件の窓の外にいる相手が集計に出ている")
	assert.Equal(t, recent.ID, rows[0].UserID)
	// 返信先が全部別ノートなので upstream / mk-go とも 1000。窓のサイズを潰す変異
	// (`.Limit(10)` など) はここで落ちる。
	assert.EqualValues(t, window, rows[0].Count)
}

// 自己返信は窓を取った**後**に除外する。窓の内側に置くと、自己返信だけを大量に
// 持つ利用者で LIMIT が満たされず全表走査に倒れる。
func TestNoteRepository_CountReplyTargets_SelfRepliesConsumeWindow(t *testing.T) {
	repo := NewNoteRepository(testDB)
	author := insertTestUser(t, "u_crt_self_a", "crtSelfA")
	defer cleanupUser(t, author.ID)
	other := insertTestUser(t, "u_crt_self_o", "crtSelfO")
	defer cleanupUser(t, other.ID)
	viewer := insertTestUser(t, "u_crt_self_v", "crtSelfV")
	defer cleanupUser(t, viewer.ID)

	selfTarget, otherTarget := "n_crt_self_ts", "n_crt_self_to"
	require.NoError(t, testDB.Create(&model.Note{ID: selfTarget, UserID: author.ID, Visibility: model.NoteVisibilityPublic}).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, selfTarget)
	require.NoError(t, testDB.Create(&model.Note{ID: otherTarget, UserID: other.ID, Visibility: model.NoteVisibilityPublic}).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, otherTarget)
	defer testDB.Exec(`DELETE FROM "note" WHERE "userId" = ?`, author.ID)

	// 他者宛が古く (a)、自己返信が新しい (b)。窓は自己返信込みで取るので、
	// 自己返信が窓を埋めれば他者宛は集計に出ない = upstream と同じ窓。
	notes := []*model.Note{{
		ID: "n_crt_self_a000", UserID: author.ID,
		ReplyID: &otherTarget, ReplyUserID: &other.ID, Visibility: model.NoteVisibilityPublic,
	}}
	for i := 0; i < 1000; i++ {
		notes = append(notes, &model.Note{
			ID: fmt.Sprintf("n_crt_self_b%04d", i), UserID: author.ID,
			ReplyID: &selfTarget, ReplyUserID: &author.ID, Visibility: model.NoteVisibilityPublic,
		})
	}
	require.NoError(t, testDB.CreateInBatches(notes, 500).Error)

	rows, err := repo.CountReplyTargets(author.ID, viewer.ID, 10)
	require.NoError(t, err)
	assert.Empty(t, rows, "自己返信が窓を埋めたら他者宛は出ない (upstream と同じ窓)")
}

// #2995: この note を renote / reply したリモート user を引く。
//
// **ローカルの renote / reply は返さない** (連合配送の宛先を組み立てるための
// クエリで、ローカル user には inbox が無い)。**重複も返さない** (同じ user が
// renote と reply の両方をしている、複数回 reply している)。
func TestNote_ListRenoteOrReplyRemoteUserIDs(t *testing.T) {
	repo := NewNoteRepository(testDB)
	local := insertTestUser(t, "u_rr_local", "rrlocal")
	remoteA := insertTestUser(t, "u_rr_a", "rra")
	remoteB := insertTestUser(t, "u_rr_b", "rrb")
	host := "remote.example"
	for _, id := range []string{remoteA.ID, remoteB.ID} {
		require.NoError(t, testDB.Exec(`UPDATE "user" SET host = ? WHERE id = ?`, host, id).Error)
	}
	t.Cleanup(func() {
		testDB.Exec(`DELETE FROM "note" WHERE id LIKE 'rr_%'`)
		cleanupUser(t, local.ID)
		cleanupUser(t, remoteA.ID)
		cleanupUser(t, remoteB.ID)
	})

	target := &model.Note{ID: "rr_target", UserID: local.ID, Visibility: model.NoteVisibilityFollowers}
	require.NoError(t, repo.Create(target))

	seed := func(id, userID string, userHost *string, renote, reply bool) {
		n := &model.Note{ID: id, UserID: userID, UserHost: userHost, Visibility: model.NoteVisibilityPublic}
		if renote {
			n.RenoteID = &target.ID
		}
		if reply {
			n.ReplyID = &target.ID
		}
		require.NoError(t, repo.Create(n))
	}
	seed("rr_remote_renote", remoteA.ID, &host, true, false)
	// 同じ user が reply もしている (重複しないこと)。
	seed("rr_remote_reply", remoteA.ID, &host, false, true)
	seed("rr_remote_b_reply", remoteB.ID, &host, false, true)
	// ローカルの renote は対象外。
	seed("rr_local_renote", local.ID, nil, true, false)
	// 無関係な note も対象外。
	seed("rr_unrelated", remoteB.ID, &host, false, false)

	ids, err := repo.ListRenoteOrReplyRemoteUserIDs(target.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{remoteA.ID, remoteB.ID}, ids)

	// 引き当てが無ければ空。
	none, err := repo.ListRenoteOrReplyRemoteUserIDs("rr_unrelated")
	require.NoError(t, err)
	assert.Empty(t, none)
	empty, err := repo.ListRenoteOrReplyRemoteUserIDs("")
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// **凍結した利用者の featured ノートを返さないこと。**
//
// `users/show` が凍結ユーザーを `NO_SUCH_USER` で隠すのに、同じ利用者のノートが
// 本文つきで返るのは経路ごとに結果が食い違う形。upstream
// `users/featured-notes.ts` は `generateSuspendedUserQueryForNote` を掛けている。
func TestNoteRepository_ListFeaturedByUser_ExcludesSuspendedAuthor(t *testing.T) {
	repo := NewNoteRepository(testDB)
	author := insertTestUser(t, "feat_susp_u", "featsuspu")
	defer cleanupUser(t, author.ID)

	note := &model.Note{ID: "feat_susp_n1", UserID: author.ID, Visibility: "public", RenoteCount: 5}
	require.NoError(t, testDB.Create(note).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, note.ID)

	// 凍結前は返る (対照)。
	got, err := repo.ListFeaturedByUser(author.ID, "", "", 10)
	require.NoError(t, err)
	require.Len(t, got, 1, "凍結前は返ること")

	require.NoError(t, testDB.Exec(`UPDATE "user" SET "isSuspended" = true WHERE id = ?`, author.ID).Error)

	got, err = repo.ListFeaturedByUser(author.ID, "", "", 10)
	require.NoError(t, err)
	require.Empty(t, got, "凍結後は返さないこと")
}
