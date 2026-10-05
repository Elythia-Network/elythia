package repository

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// seedRelayUser creates a remote user optionally marked as relay-observed.
func seedRelayUser(t *testing.T, id string, viaRelay bool, lastFetched *time.Time) *model.User {
	t.Helper()
	host := "orphan.example"
	u := &model.User{
		ID: id, Username: id, UsernameLower: id, Host: &host,
		LastFetchedAt: lastFetched, AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(u).Error)
	t.Cleanup(func() { cleanupUser(t, id) })
	if viaRelay {
		require.NoError(t, NewRelayObservedUserRepository(testDB).MarkObserved(id))
	}
	return u
}

func longAgo() *time.Time  { t := time.Now().Add(-90 * 24 * time.Hour); return &t }
func recently() *time.Time { t := time.Now().Add(-1 * time.Hour); return &t }

// TestDeleteOrphanRemoteUsers_OnlyRelayDerived は本機能の核心。
// リレー由来でない孤児は、条件を満たしていても消さない。
func TestDeleteOrphanRemoteUsers_OnlyRelayDerived(t *testing.T) {
	r := NewUserRepository(testDB)
	seedRelayUser(t, "orph_relay", true, longAgo())
	seedRelayUser(t, "orph_direct", false, longAgo())

	_, err := r.DeleteOrphanRemoteUsers(30, 100)
	require.NoError(t, err)

	_, err = r.FindByID("orph_relay")
	assert.Error(t, err, "リレー由来の孤児は消える")
	_, err = r.FindByID("orph_direct")
	assert.NoError(t, err, "リレー由来でない孤児は残る (意図して観測したもの)")
}

// 猶予期間内は消さない。ResolveActor は actorTTL で lastFetchedAt を更新する
// ため、短い猶予だと活動中の著者が削除・再フェッチを繰り返す。
func TestDeleteOrphanRemoteUsers_RespectsGrace(t *testing.T) {
	r := NewUserRepository(testDB)
	seedRelayUser(t, "orph_recent", true, recently())

	_, err := r.DeleteOrphanRemoteUsers(30, 100)
	require.NoError(t, err)

	_, err = r.FindByID("orph_recent")
	assert.NoError(t, err, "猶予期間内は残る")
}

// **必須ガード**: ノートが残っているユーザーは消さない。
// note.userId が CASCADE なので、消すとそのノートも道連れになる。
func TestDeleteOrphanRemoteUsers_KeepsUsersWithNotes(t *testing.T) {
	r := NewUserRepository(testDB)
	nr := NewNoteRepository(testDB)
	u := seedRelayUser(t, "orph_hasnote", true, longAgo())
	host := "orphan.example"
	n := &model.Note{
		ID: "orph_note_1", UserID: u.ID, UserHost: &host,
		Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, nr.Create(n))
	defer cleanupNote(t, n.ID)

	_, err := r.DeleteOrphanRemoteUsers(30, 100)
	require.NoError(t, err)

	_, err = r.FindByID(u.ID)
	assert.NoError(t, err, "ノートが残っているユーザーは消さない")
	_, err = nr.FindByID(n.ID)
	assert.NoError(t, err, "ノートも道連れにならない")
}

// 関係を持つユーザーは残る。
func TestDeleteOrphanRemoteUsers_KeepsReferencedUsers(t *testing.T) {
	r := NewUserRepository(testDB)
	local := insertTestUser(t, "orph_lu", "orphlu")
	defer cleanupUser(t, local.ID)

	for _, tc := range []struct {
		name   string
		id     string
		attach func(t *testing.T, uid string)
	}{
		{"フォローされている", "orph_followed", func(t *testing.T, uid string) {
			f := &model.Following{ID: "orphf_" + uid, FollowerID: local.ID, FolloweeID: uid}
			require.NoError(t, testDB.Create(f).Error)
			t.Cleanup(func() { testDB.Delete(&model.Following{}, "id = ?", f.ID) })
		}},
		{"ブロックされている", "orph_blocked", func(t *testing.T, uid string) {
			b := &model.Blocking{ID: "orphb_" + uid, BlockerID: local.ID, BlockeeID: uid}
			require.NoError(t, testDB.Create(b).Error)
			t.Cleanup(func() { testDB.Delete(&model.Blocking{}, "id = ?", b.ID) })
		}},
		{"ミュートされている", "orph_muted", func(t *testing.T, uid string) {
			m := &model.Muting{ID: "orphm_" + uid, MuterID: local.ID, MuteeID: uid}
			require.NoError(t, testDB.Create(m).Error)
			t.Cleanup(func() { testDB.Delete(&model.Muting{}, "id = ?", m.ID) })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seedRelayUser(t, tc.id, true, longAgo())
			tc.attach(t, tc.id)

			_, err := r.DeleteOrphanRemoteUsers(30, 100)
			require.NoError(t, err)

			_, err = r.FindByID(tc.id)
			assert.NoError(t, err, "関係を持つユーザーは残る")
		})
	}
}

// system_account (リレー actor / instance actor / proxy) は消さない。
func TestDeleteOrphanRemoteUsers_KeepsSystemAccounts(t *testing.T) {
	r := NewUserRepository(testDB)
	seedRelayUser(t, "orph_system", true, longAgo())
	sa := &model.SystemAccount{ID: "orph_sa", UserID: "orph_system", Type: "orphan-test"}
	require.NoError(t, testDB.Create(sa).Error)
	defer testDB.Delete(&model.SystemAccount{}, "id = ?", sa.ID)

	_, err := r.DeleteOrphanRemoteUsers(30, 100)
	require.NoError(t, err)

	_, err = r.FindByID("orph_system")
	assert.NoError(t, err, "system_account は消さない")
}

// ローカルユーザーは対象外。
func TestDeleteOrphanRemoteUsers_SkipsLocalUsers(t *testing.T) {
	r := NewUserRepository(testDB)
	local := insertTestUser(t, "orph_loc2", "orphloc2")
	defer cleanupUser(t, local.ID)
	require.NoError(t, NewRelayObservedUserRepository(testDB).MarkObserved(local.ID))

	_, err := r.DeleteOrphanRemoteUsers(30, 100)
	require.NoError(t, err)

	_, err = r.FindByID(local.ID)
	assert.NoError(t, err, "ローカルユーザーは消さない")
}

// 削除で relay_observed_user 側も CASCADE で落ちること。
func TestDeleteOrphanRemoteUsers_CascadesMarker(t *testing.T) {
	r := NewUserRepository(testDB)
	seedRelayUser(t, "orph_cascade", true, longAgo())

	_, err := r.DeleteOrphanRemoteUsers(30, 100)
	require.NoError(t, err)

	var n int64
	require.NoError(t, testDB.Model(&model.RelayObservedUser{}).
		Where(`"userId" = ?`, "orph_cascade").Count(&n).Error)
	assert.Zero(t, n, "印も CASCADE で落ちること")
}

// MarkObserved は重複しても落ちない (並行 inbox job で起こりうる)。
func TestMarkObserved_Idempotent(t *testing.T) {
	seedRelayUser(t, "orph_dup", false, longAgo())
	repo := NewRelayObservedUserRepository(testDB)
	require.NoError(t, repo.MarkObserved("orph_dup"))
	require.NoError(t, repo.MarkObserved("orph_dup"))
	assert.NoError(t, repo.MarkObserved(""))
}
