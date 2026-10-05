package repository

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

func insertGoneTestInstance(t *testing.T, id, host string, state model.SuspensionState) {
	t.Helper()
	require.NoError(t, testDB.Create(&model.Instance{ID: id, Host: host, FirstRetrievedAt: time.Now(), SuspensionState: state}).Error)
	t.Cleanup(func() {
		testDB.Exec(`DELETE FROM instance WHERE id = ?`, id)
		testDB.Exec(`DELETE FROM instance_gone_suspension WHERE host = ?`, host)
	})
}

func insertGoneTestFollowing(t *testing.T, id, follower, followee string, followerHost, followeeHost *string) {
	t.Helper()
	require.NoError(t, testDB.Create(&model.Following{ID: id, FollowerID: follower, FolloweeID: followee, FollowerHost: followerHost, FolloweeHost: followeeHost}).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM following WHERE id = ?`, id) })
}

func insertGoneTestRequest(t *testing.T, id, follower, followee string, followerHost, followeeHost *string) {
	t.Helper()
	require.NoError(t, testDB.Create(&model.FollowRequest{ID: id, FollowerID: follower, FolloweeID: followee, FollowerHost: followerHost, FolloweeHost: followeeHost}).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM follow_request WHERE id = ?`, id) })
}

func hostp(s string) *string { return &s }

// goneFixture: gone1 (記録あり) / gone2 (記録なし = TS が立てた停止) / 手動停止 / 通常。
func goneFixture(t *testing.T) *GoneInstanceRepository {
	t.Helper()
	repo := NewGoneInstanceRepository(testDB)
	insertGoneTestInstance(t, "gi_1", "gone1.example", model.SuspensionStateGoneSuspended)
	insertGoneTestInstance(t, "gi_2", "gone2.example", model.SuspensionStateGoneSuspended)
	insertGoneTestInstance(t, "gi_3", "manual.example", model.SuspensionStateManuallySuspended)
	insertGoneTestInstance(t, "gi_4", "live.example", model.SuspensionStateNone)

	for _, u := range []struct{ id, name string }{{"gu_l1", "gl1"}, {"gu_l2", "gl2"}} {
		insertTestUser(t, u.id, u.name)
		id := u.id
		t.Cleanup(func() { cleanupUser(t, id) })
	}
	insertRemoteSampleUser(t, "gu_r1", "r1", "gone1.example", nil, false, false)
	insertRemoteSampleUser(t, "gu_r2", "r2", "gone1.example", nil, false, false)
	insertRemoteSampleUser(t, "gu_r3", "r3", "gone2.example", nil, false, false)
	insertRemoteSampleUser(t, "gu_r4", "r4", "manual.example", nil, false, false)
	insertRemoteSampleUser(t, "gu_r5", "r5", "live.example", nil, false, false)

	g1, g2 := hostp("gone1.example"), hostp("gone2.example")
	insertGoneTestFollowing(t, "gf_1", "gu_r1", "gu_l1", g1, nil)
	insertGoneTestFollowing(t, "gf_2", "gu_r2", "gu_l1", g1, nil)
	insertGoneTestFollowing(t, "gf_3", "gu_l1", "gu_r1", nil, g1)
	insertGoneTestFollowing(t, "gf_4", "gu_l2", "gu_r3", nil, g2)
	insertGoneTestFollowing(t, "gf_5", "gu_r4", "gu_l1", hostp("manual.example"), nil)
	insertGoneTestFollowing(t, "gf_6", "gu_r5", "gu_l2", hostp("live.example"), nil)
	// リモート同士の行は数えない (片付けの対象外)。
	insertGoneTestFollowing(t, "gf_7", "gu_r1", "gu_r3", g1, g2)
	insertGoneTestRequest(t, "gr_1", "gu_r2", "gu_l2", g1, nil)
	insertGoneTestRequest(t, "gr_2", "gu_l2", "gu_r1", nil, g1)
	// 別ホスト (手動停止 / 通常) とのリクエストは片付けの対象外。
	insertGoneTestRequest(t, "gr_3", "gu_r4", "gu_l2", hostp("manual.example"), nil)
	insertGoneTestRequest(t, "gr_4", "gu_l1", "gu_r5", nil, hostp("live.example"))
	return repo
}

func TestGoneInstanceRepository_ListGone(t *testing.T) {
	repo := goneFixture(t)
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, repo.RecordGone("gone1.example", at))

	list, err := repo.ListGone()
	require.NoError(t, err)
	require.Len(t, list, 2, "only goneSuspended hosts (manual / live are excluded)")
	assert.Equal(t, "gone2.example", list[0].Host, "hosts without a record come first")
	assert.Nil(t, list[0].SuspendedAt)
	assert.Equal(t, GoneInstanceSummary{Host: "gone2.example", Following: 1}, list[0])
	assert.Equal(t, "gone1.example", list[1].Host)
	require.NotNil(t, list[1].SuspendedAt)
	assert.True(t, at.Equal(*list[1].SuspendedAt))
	assert.Equal(t, int64(2), list[1].Followers)
	assert.Equal(t, int64(1), list[1].Following)
	assert.Equal(t, int64(2), list[1].FollowRequests)
}

// 戻した後に再び消えたら、新しい時刻で記録し直す。
func TestGoneInstanceRepository_RecordGoneOverwrites(t *testing.T) {
	repo := goneFixture(t)
	require.NoError(t, repo.RecordGone("gone1.example", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	later := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	require.NoError(t, repo.RecordGone("gone1.example", later))
	var row model.InstanceGoneSuspension
	require.NoError(t, testDB.Where("host = ?", "gone1.example").First(&row).Error)
	assert.True(t, later.Equal(row.SuspendedAt))
	assert.NoError(t, repo.RecordGone("", later))
}

func TestGoneInstanceRepository_Relations(t *testing.T) {
	repo := goneFixture(t)
	rows, err := repo.ListRelations("gone1.example", 10)
	require.NoError(t, err)
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	assert.ElementsMatch(t, []string{"gf_1", "gf_2", "gf_3"}, ids, "both directions, only with local users")

	rows, err = repo.ListRelations("gone1.example", 2)
	require.NoError(t, err)
	assert.Len(t, rows, 2)

	n, err := repo.CountRelations("gone1.example")
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)

	rows, err = repo.ListRelations("bad\x00host", 10)
	assert.NoError(t, err)
	assert.Empty(t, rows)
}

func TestGoneInstanceRepository_DeleteFollowRequests(t *testing.T) {
	repo := goneFixture(t)
	n, err := repo.DeleteFollowRequests("gone1.example")
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
	n, err = repo.DeleteFollowRequests("gone1.example")
	require.NoError(t, err)
	assert.Zero(t, n)
	var left []string
	require.NoError(t, testDB.Model(&model.FollowRequest{}).Where("id IN ?", []string{"gr_1", "gr_2", "gr_3", "gr_4"}).Pluck("id", &left).Error)
	assert.ElementsMatch(t, []string{"gr_3", "gr_4"}, left, "requests with other hosts are kept")
}
