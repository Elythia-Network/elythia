package maintenance

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedVisibilityNote(t *testing.T, id, userID string, visibility model.NoteVisibility, tags []string) {
	t.Helper()
	require.NoError(t, testDB.Create(&model.Note{
		ID: id, UserID: userID, Visibility: visibility, Tags: model.StringArray(tags),
	}).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "note" WHERE id = ?`, id) })
}

func seedHashtag(t *testing.T, h *model.Hashtag) {
	t.Helper()
	h.MentionedUsersCount = len(h.MentionedUserIds)
	h.MentionedLocalUsersCount = len(h.MentionedLocalUserIds)
	h.MentionedRemoteUsersCount = len(h.MentionedRemoteUserIds)
	h.AttachedUsersCount = len(h.AttachedUserIds)
	for _, a := range []*model.StringArray{&h.MentionedUserIds, &h.MentionedLocalUserIds, &h.MentionedRemoteUserIds,
		&h.AttachedUserIds, &h.AttachedLocalUserIds, &h.AttachedRemoteUserIds} {
		if *a == nil {
			*a = model.StringArray{}
		}
	}
	require.NoError(t, testDB.Create(h).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "hashtag" WHERE id = ?`, h.ID) })
}

func hashtagByName(t *testing.T, name string) (model.Hashtag, bool) {
	t.Helper()
	var rows []model.Hashtag
	require.NoError(t, testDB.Where("name = ?", name).Find(&rows).Error)
	if len(rows) == 0 {
		return model.Hashtag{}, false
	}
	return rows[0], true
}

// 公開範囲を限ったノートでだけタグを使った利用者を、hashtag の mentioned から外す。
// public / home のノートでも使っている利用者と、ノートの無い (消した) 利用者は残す。
// 誰も残らない行は消す。
func TestBackfillHashtagMentionsBatch(t *testing.T) {
	for _, u := range []string{"hm_priv", "hm_pub", "hm_home", "hm_remote", "hm_gone"} {
		seedBackfillUser(t, u)
	}
	// hm_priv: followers のノートだけ。外す (local)。
	seedVisibilityNote(t, "hm_n1", "hm_priv", model.NoteVisibilityFollowers, []string{"mixed", "privonly"})
	// hm_pub: followers と public の両方。残す。
	seedVisibilityNote(t, "hm_n2", "hm_pub", model.NoteVisibilityFollowers, []string{"mixed"})
	seedVisibilityNote(t, "hm_n3", "hm_pub", model.NoteVisibilityPublic, []string{"mixed"})
	// hm_home: specified と home の両方。残す。
	seedVisibilityNote(t, "hm_n4", "hm_home", model.NoteVisibilitySpecified, []string{"mixed"})
	seedVisibilityNote(t, "hm_n5", "hm_home", model.NoteVisibilityHome, []string{"mixed"})
	// hm_remote: specified のノートだけ。外す (remote)。
	seedVisibilityNote(t, "hm_n6", "hm_remote", model.NoteVisibilitySpecified, []string{"mixed"})
	// hm_gone はノートが無い (消した)。どの公開範囲だったか分からないので残す。

	seedHashtag(t, &model.Hashtag{ID: "hm_h1", Name: "mixed",
		MentionedUserIds:       model.StringArray{"hm_priv", "hm_pub", "hm_home", "hm_remote", "hm_gone"},
		MentionedLocalUserIds:  model.StringArray{"hm_priv", "hm_pub", "hm_home", "hm_gone"},
		MentionedRemoteUserIds: model.StringArray{"hm_remote"},
	})
	seedHashtag(t, &model.Hashtag{ID: "hm_h2", Name: "privonly",
		MentionedUserIds:      model.StringArray{"hm_priv"},
		MentionedLocalUserIds: model.StringArray{"hm_priv"},
	})

	// dry-run は数えるだけ。
	res, err := BackfillHashtagMentionsBatch(testDB, "hm_", 100, true, nil)
	require.NoError(t, err)
	assert.Equal(t, 4, res.Scanned)
	assert.Equal(t, 3, res.Removed)
	assert.Zero(t, res.Deleted)
	h, ok := hashtagByName(t, "privonly")
	require.True(t, ok)
	assert.Equal(t, 1, h.MentionedUsersCount)

	res, err = BackfillHashtagMentionsBatch(testDB, "hm_", 100, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 4, res.Scanned)
	assert.Equal(t, "hm_n6", res.LastID)
	assert.Equal(t, 3, res.Removed)
	assert.Equal(t, 1, res.Deleted)

	h, ok = hashtagByName(t, "mixed")
	require.True(t, ok)
	assert.ElementsMatch(t, []string{"hm_pub", "hm_home", "hm_gone"}, []string(h.MentionedUserIds))
	assert.Equal(t, 3, h.MentionedUsersCount)
	assert.ElementsMatch(t, []string{"hm_pub", "hm_home", "hm_gone"}, []string(h.MentionedLocalUserIds))
	assert.Equal(t, 3, h.MentionedLocalUsersCount)
	assert.Empty(t, h.MentionedRemoteUserIds)
	assert.Equal(t, 0, h.MentionedRemoteUsersCount)

	_, ok = hashtagByName(t, "privonly")
	assert.False(t, ok, "a row nobody uses any more is deleted")

	// 冪等。2 回目は何も変えない。
	res, err = BackfillHashtagMentionsBatch(testDB, "hm_", 100, false, nil)
	require.NoError(t, err)
	assert.Zero(t, res.Removed)
	assert.Zero(t, res.Deleted)
}

// 誰も mentioned に残らなくても、プロフィールのタグ (attached) の利用者が
// いる行は消さない。
func TestBackfillHashtagMentionsBatch_KeepsAttachedRow(t *testing.T) {
	seedBackfillUser(t, "hma_u")
	seedVisibilityNote(t, "hma_n1", "hma_u", model.NoteVisibilityFollowers, []string{"attachedtoo"})
	seedHashtag(t, &model.Hashtag{ID: "hma_h", Name: "attachedtoo",
		MentionedUserIds:      model.StringArray{"hma_u"},
		MentionedLocalUserIds: model.StringArray{"hma_u"},
		AttachedUserIds:       model.StringArray{"hma_u"},
		AttachedLocalUserIds:  model.StringArray{"hma_u"},
	})
	res, err := BackfillHashtagMentionsBatch(testDB, "hma_", 100, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Removed)
	assert.Zero(t, res.Deleted)
	h, ok := hashtagByName(t, "attachedtoo")
	require.True(t, ok)
	assert.Empty(t, h.MentionedUserIds)
	assert.Equal(t, 0, h.MentionedUsersCount)
	assert.Equal(t, 1, h.AttachedUsersCount)
}

func TestBackfillHashtagMentionsBatch_EmptyAndDefaults(t *testing.T) {
	res, err := BackfillHashtagMentionsBatch(testDB, "zzzz", 0, false, nil)
	require.NoError(t, err)
	assert.Zero(t, res.Scanned)
	assert.Empty(t, res.LastID)
}

// 同じ (作者, タグ) の組が複数のバッチに出ても、実行全体で 1 つの seen を渡せば
// dry-run で 1 回だけ数える。
func TestBackfillHashtagMentionsBatch_DryRunCountsPairOnceAcrossBatches(t *testing.T) {
	seedBackfillUser(t, "hmd_u")
	seedVisibilityNote(t, "hmd_n1", "hmd_u", model.NoteVisibilityFollowers, []string{"hmdtag"})
	seedVisibilityNote(t, "hmd_n2", "hmd_u", model.NoteVisibilityFollowers, []string{"hmdtag"})
	seedHashtag(t, &model.Hashtag{ID: "hmd_h", Name: "hmdtag",
		MentionedUserIds:      model.StringArray{"hmd_u"},
		MentionedLocalUserIds: model.StringArray{"hmd_u"},
	})
	seen := HashtagMentionsSeen{}
	removed := 0
	cursor := "hmd_"
	for {
		res, err := BackfillHashtagMentionsBatch(testDB, cursor, 1, true, seen)
		require.NoError(t, err)
		if res.Scanned == 0 || res.LastID > "hmd_n2" {
			break
		}
		removed += res.Removed
		cursor = res.LastID
	}
	assert.Equal(t, 1, removed)
}

// 数が配列とずれていて 0 の行でも、外した後の数を負にしない。
func TestBackfillHashtagMentionsBatch_ClampsDriftedCounts(t *testing.T) {
	seedBackfillUser(t, "hmc_u")
	seedVisibilityNote(t, "hmc_n1", "hmc_u", model.NoteVisibilityFollowers, []string{"hmctag"})
	h := &model.Hashtag{ID: "hmc_h", Name: "hmctag",
		MentionedUserIds:       model.StringArray{"hmc_u", "hmc_other"},
		MentionedLocalUserIds:  model.StringArray{"hmc_u"},
		MentionedRemoteUserIds: model.StringArray{"hmc_other"},
	}
	seedHashtag(t, h)
	require.NoError(t, testDB.Exec(`UPDATE "hashtag" SET "mentionedUsersCount" = 0, "mentionedLocalUsersCount" = 0, "mentionedRemoteUsersCount" = 0 WHERE id = ?`, h.ID).Error)

	res, err := BackfillHashtagMentionsBatch(testDB, "hmc_", 100, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Removed)
	got, ok := hashtagByName(t, "hmctag")
	require.True(t, ok)
	assert.Equal(t, []string{"hmc_other"}, []string(got.MentionedUserIds))
	assert.Equal(t, 0, got.MentionedUsersCount)
	assert.Equal(t, 0, got.MentionedLocalUsersCount)
	assert.Equal(t, 0, got.MentionedRemoteUsersCount)

	// リモートの利用者の数も同じく 0 で止める。
	seedBackfillUser(t, "hmc_r")
	seedVisibilityNote(t, "hmc_n2", "hmc_r", model.NoteVisibilitySpecified, []string{"hmctag2"})
	r := &model.Hashtag{ID: "hmc_h2", Name: "hmctag2",
		MentionedUserIds:       model.StringArray{"hmc_r"},
		MentionedRemoteUserIds: model.StringArray{"hmc_r"},
	}
	seedHashtag(t, r)
	require.NoError(t, testDB.Exec(`UPDATE "hashtag" SET "mentionedRemoteUsersCount" = 0, "attachedUsersCount" = 0 WHERE id = ?`, r.ID).Error)
	require.NoError(t, testDB.Exec(`UPDATE "hashtag" SET "attachedUserIds" = '{x}' WHERE id = ?`, r.ID).Error)
	_, err = BackfillHashtagMentionsBatch(testDB, "hmc_n1", 100, false, nil)
	require.NoError(t, err)
	got, ok = hashtagByName(t, "hmctag2")
	require.True(t, ok)
	assert.Equal(t, 0, got.MentionedRemoteUsersCount)
}
