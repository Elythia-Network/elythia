package repository

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// #3449: 複数の list を混ぜる timeline (social / ログイン中の local) の DB
// fallback は、fanout が list に積む返信を返す。返信を絞る条件
// (ExcludeRepliesToOthers) に、KeepRepliesToViewer / KeepHomeFanoutReplies の
// 例外を OR で足す。可視性・mute などの他の条件は AND のまま効くこと。
func TestNoteRepository_ReplyExclusionKeepsMultiListFanout(t *testing.T) {
	repo := NewNoteRepository(testDB)
	viewer := insertTestUser(t, "u_mlr_v", "mlrV")
	followee := insertTestUser(t, "u_mlr_f", "mlrF")
	followeeWR := insertTestUser(t, "u_mlr_w", "mlrW")
	stranger := insertTestUser(t, "u_mlr_s", "mlrS")
	muted := insertTestUser(t, "u_mlr_m", "mlrM")
	users := []string{viewer.ID, followee.ID, followeeWR.ID, stranger.ID, muted.ID}
	for _, id := range users {
		defer cleanupUser(t, id)
	}
	defer testDB.Exec(`DELETE FROM "note" WHERE "userId" IN ?`, users)
	defer testDB.Exec(`DELETE FROM "following" WHERE "followerId" = ?`, viewer.ID)
	require.NoError(t, testDB.Create(&model.Following{ID: "mlr_fw1", FollowerID: viewer.ID, FolloweeID: followee.ID}).Error)
	require.NoError(t, testDB.Create(&model.Following{ID: "mlr_fw2", FollowerID: viewer.ID, FolloweeID: followeeWR.ID, WithReplies: true}).Error)

	// 返信先の投稿 (それぞれ公開)。
	parents := map[string]string{viewer.ID: "mlr_pv", followee.ID: "mlr_pf", stranger.ID: "mlr_ps"}
	for uid, nid := range parents {
		require.NoError(t, repo.Create(&model.Note{ID: nid, UserID: uid, Visibility: model.NoteVisibilityPublic}))
	}
	reply := func(nid, author, to string, vis model.NoteVisibility, mentions ...string) {
		t.Helper()
		parent, target := parents[to], to
		require.NoError(t, repo.Create(&model.Note{
			ID: nid, UserID: author, Visibility: vis,
			ReplyID: &parent, ReplyUserID: &target, Mentions: model.StringArray(mentions),
		}))
	}
	pub := model.NoteVisibilityPublic
	reply("mlr_s2v", stranger.ID, viewer.ID, pub)                                // 自分宛て (localTimelineWithReplyTo)
	reply("mlr_s2v_home", stranger.ID, viewer.ID, model.NoteVisibilityHome)      // 自分宛てだが LTL に出ない公開範囲
	reply("mlr_m2v", muted.ID, viewer.ID, pub)                                   // 自分宛てだが mute 済み
	reply("mlr_f2v", followee.ID, viewer.ID, pub)                                // フォロー中の人から自分宛て
	reply("mlr_f2s", followee.ID, stranger.ID, pub)                              // フォロー中 (withReplies 無し) の人の他人宛て
	reply("mlr_f2s_mention", followee.ID, stranger.ID, pub, viewer.ID)           // 同上だが自分への mention 付き
	reply("mlr_w2s", followeeWR.ID, stranger.ID, pub)                            // withReplies 付きでフォロー中の人の他人宛て
	reply("mlr_v2s", viewer.ID, stranger.ID, pub)                                // 自分の他人宛て
	reply("mlr_s2f", stranger.ID, followee.ID, pub)                              // フォローしていない人の他人宛て
	reply("mlr_s2s_mention", stranger.ID, followee.ID, pub, viewer.ID)           // フォローしていない人の、自分への mention 付きの他人宛て
	reply("mlr_f2v_spec", followee.ID, viewer.ID, model.NoteVisibilitySpecified) // 宛先に自分が入っていない specified

	ids := func(notes []*model.Note) map[string]bool {
		m := map[string]bool{}
		for _, n := range notes {
			m[n.ID] = true
		}
		return m
	}
	base := model.TimelineDBFilter{ViewerID: viewer.ID, ExcludeRepliesToOthers: true, MutedUserIDs: []string{muted.ID}}

	t.Run("local keeps replies to the viewer", func(t *testing.T) {
		f := base
		f.KeepRepliesToViewer = true
		got, err := repo.ListLocalTimeline(100, "", "", f)
		require.NoError(t, err)
		m := ids(got)
		assert.True(t, m["mlr_s2v"], "自分宛ての公開の返信は出る")
		for _, id := range []string{"mlr_s2v_home", "mlr_m2v", "mlr_f2s", "mlr_f2s_mention", "mlr_w2s", "mlr_v2s", "mlr_s2f", "mlr_s2s_mention", "mlr_f2v_spec"} {
			assert.False(t, m[id], id)
		}
		assert.True(t, m["mlr_f2v"], "フォロー中の人からでも、ローカルの公開の自分宛ては localTimelineWithReplyTo に積まれる")
	})

	t.Run("local without the flag stays as before", func(t *testing.T) {
		got, err := repo.ListLocalTimeline(100, "", "", base)
		require.NoError(t, err)
		assert.False(t, ids(got)["mlr_s2v"])
	})

	t.Run("home keeps what the home fan-out delivers", func(t *testing.T) {
		f := base
		f.KeepRepliesToViewer = true
		f.KeepHomeFanoutReplies = true
		got, err := repo.ListHomeTimeline(viewer.ID, 100, "", "", f)
		require.NoError(t, err)
		m := ids(got)
		for _, id := range []string{"mlr_f2v", "mlr_f2s_mention", "mlr_w2s", "mlr_v2s"} {
			assert.True(t, m[id], id)
		}
		for _, id := range []string{"mlr_f2s", "mlr_s2v", "mlr_s2f", "mlr_s2s_mention", "mlr_m2v", "mlr_f2v_spec"} {
			assert.False(t, m[id], id)
		}
	})

	t.Run("home without the flags stays as before", func(t *testing.T) {
		got, err := repo.ListHomeTimeline(viewer.ID, 100, "", "", base)
		require.NoError(t, err)
		m := ids(got)
		for _, id := range []string{"mlr_f2v", "mlr_f2s_mention", "mlr_w2s", "mlr_v2s"} {
			assert.False(t, m[id], id)
		}
		assert.True(t, m["mlr_pf"], "返信でない投稿は出る")
	})

	// チャンネルの投稿は、返信も含めてフォロワー全員のホームの list に積まれる
	// (fanoutToChannelFollowers)。フォロー中・mute していないチャンネルのものだけ返す。
	chFollowed, chOther, chMuted := "mlr_ch_f", "mlr_ch_o", "mlr_ch_m"
	for nid, ch := range map[string]string{"mlr_chf": chFollowed, "mlr_cho": chOther, "mlr_chm": chMuted} {
		parent, target, ch := parents[stranger.ID], followee.ID, ch
		require.NoError(t, repo.Create(&model.Note{
			ID: nid, UserID: muted.ID, Visibility: pub, ChannelID: &ch,
			ReplyID: &parent, ReplyUserID: &target,
		}))
	}
	t.Run("home keeps replies in followed channels", func(t *testing.T) {
		f := base
		f.MutedUserIDs = nil // 投稿者 (muted) を mute から外す。チャンネルの条件だけを見る。
		f.KeepHomeFanoutReplies = true
		f.FollowedChannelIDs = []string{chFollowed}
		f.MutedChannelIDs = []string{chMuted}
		got, err := repo.ListHomeTimeline(viewer.ID, 100, "", "", f)
		require.NoError(t, err)
		m := ids(got)
		assert.True(t, m["mlr_chf"], "フォロー中のチャンネルの、フォローしていない人の他人宛ての返信は出る")
		assert.False(t, m["mlr_cho"], "フォローしていないチャンネルの返信は出ない")
		assert.False(t, m["mlr_chm"], "mute したチャンネルの返信は出ない")

		// mute したチャンネルは handler が FollowedChannelIDs から外すが、
		// 外し損ねても MutedChannelIDs で落ちる。
		f.FollowedChannelIDs = []string{chFollowed, chMuted}
		got, err = repo.ListHomeTimeline(viewer.ID, 100, "", "", f)
		require.NoError(t, err)
		assert.False(t, ids(got)["mlr_chm"])

		// 例外が無ければ、フォロー中のチャンネルでも他人宛ての返信は出ない (従来どおり)。
		f.KeepHomeFanoutReplies = false
		got, err = repo.ListHomeTimeline(viewer.ID, 100, "", "", f)
		require.NoError(t, err)
		assert.False(t, ids(got)["mlr_chf"])
	})

	// フォローしていない人の followers 限定の投稿への返信は、withReplies 付きで
	// フォローしている人のものでも fanout が配らない。SQL では
	// HideFollowersOnlyReplyFromNonFollowee が落とすので、それと一緒に使う
	// (hybridDBFallback は両方そろったときだけ KeepHomeFanoutReplies を付ける)。
	require.NoError(t, repo.Create(&model.Note{ID: "mlr_ps_fo", UserID: stranger.ID, Visibility: model.NoteVisibilityFollowers}))
	foParent, strangerID := "mlr_ps_fo", stranger.ID
	require.NoError(t, repo.Create(&model.Note{
		ID: "mlr_w2s_fo", UserID: followeeWR.ID, Visibility: pub,
		ReplyID: &foParent, ReplyUserID: &strangerID,
	}))
	t.Run("followers-only gate still applies", func(t *testing.T) {
		f := base
		f.KeepHomeFanoutReplies = true
		f.HideFollowersOnlyReplyFromNonFollowee = true
		got, err := repo.ListHomeTimeline(viewer.ID, 100, "", "", f)
		require.NoError(t, err)
		m := ids(got)
		assert.True(t, m["mlr_w2s"])
		assert.False(t, m["mlr_w2s_fo"], "フォローしていない人の followers 限定の投稿への返信は出ない")

		// gate が無いと出てしまう (だから hybridDBFallback は gate と組にする)。
		f.HideFollowersOnlyReplyFromNonFollowee = false
		got, err = repo.ListHomeTimeline(viewer.ID, 100, "", "", f)
		require.NoError(t, err)
		assert.True(t, ids(got)["mlr_w2s_fo"])
	})

	t.Run("flags need a viewer", func(t *testing.T) {
		f := base
		f.ViewerID = ""
		f.MutedUserIDs = nil
		f.KeepRepliesToViewer = true
		f.KeepHomeFanoutReplies = true
		got, err := repo.ListLocalTimeline(100, "", "", f)
		require.NoError(t, err)
		assert.False(t, ids(got)["mlr_s2v"])
	})
}
