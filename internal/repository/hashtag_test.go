package repository

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// cleanupHashtag removes a hashtag row by name. テスト同士の干渉を防ぐために
// テスト末尾で必ず呼ぶこと。
func cleanupHashtag(t *testing.T, name string) {
	t.Helper()
	t.Cleanup(func() {
		testDB.Exec(`DELETE FROM "hashtag" WHERE name = ?`, name)
	})
}

func TestHashtagRepository_FindByName_NotFound(t *testing.T) {
	repo := NewHashtagRepository(testDB)
	_, err := repo.FindByName("#absolutely_no_such_tag_in_db")
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestHashtagRepository_RecordMention_LocalUser_FreshRow(t *testing.T) {
	repo := NewHashtagRepository(testDB)
	user := insertTestUser(t, "u_ht_l1", "ht_local1")
	defer cleanupUser(t, user.ID)
	cleanupHashtag(t, "#htlocal1")

	require.NoError(t, repo.RecordMention("h_ht_l1", "#htlocal1", user.ID, true))

	got, err := repo.FindByName("#htlocal1")
	require.NoError(t, err)
	assert.Equal(t, "#htlocal1", got.Name)
	assert.Equal(t, 1, got.MentionedUsersCount)
	assert.Equal(t, 1, got.MentionedLocalUsersCount)
	assert.Equal(t, 0, got.MentionedRemoteUsersCount)
	assert.Equal(t, model.StringArray{user.ID}, got.MentionedUserIds)
	assert.Equal(t, model.StringArray{user.ID}, got.MentionedLocalUserIds)
	assert.Empty(t, []string(got.MentionedRemoteUserIds))
}

func TestHashtagRepository_RecordMention_RemoteUser_FreshRow(t *testing.T) {
	repo := NewHashtagRepository(testDB)
	user := insertRemoteTestUser(t, "u_ht_r1", "ht_remote1", "remote.example")
	defer cleanupUser(t, user.ID)
	cleanupHashtag(t, "#htremote1")

	require.NoError(t, repo.RecordMention("h_ht_r1", "#htremote1", user.ID, false))

	got, err := repo.FindByName("#htremote1")
	require.NoError(t, err)
	assert.Equal(t, 1, got.MentionedUsersCount)
	assert.Equal(t, 0, got.MentionedLocalUsersCount)
	assert.Equal(t, 1, got.MentionedRemoteUsersCount)
	assert.Equal(t, model.StringArray{user.ID}, got.MentionedRemoteUserIds)
}

func TestHashtagRepository_RecordMention_Idempotent(t *testing.T) {
	// 同じ user で 2 回 RecordMention しても count は 1 のまま (NOT ANY ガード)。
	repo := NewHashtagRepository(testDB)
	user := insertTestUser(t, "u_ht_idem", "ht_idem")
	defer cleanupUser(t, user.ID)
	cleanupHashtag(t, "#htidem")

	require.NoError(t, repo.RecordMention("h_ht_idem1", "#htidem", user.ID, true))
	require.NoError(t, repo.RecordMention("h_ht_idem2", "#htidem", user.ID, true))

	got, err := repo.FindByName("#htidem")
	require.NoError(t, err)
	assert.Equal(t, 1, got.MentionedUsersCount, "duplicate mention should not double-count")
	assert.Equal(t, 1, got.MentionedLocalUsersCount)
	assert.Equal(t, model.StringArray{user.ID}, got.MentionedUserIds)
}

func TestHashtagRepository_RecordMention_MultipleUsers(t *testing.T) {
	// 異なる local / remote user を混ぜると total / local / remote が独立に増える。
	repo := NewHashtagRepository(testDB)
	u1 := insertTestUser(t, "u_ht_m1", "ht_m1")
	defer cleanupUser(t, u1.ID)
	u2 := insertTestUser(t, "u_ht_m2", "ht_m2")
	defer cleanupUser(t, u2.ID)
	u3 := insertRemoteTestUser(t, "u_ht_m3", "ht_m3", "remote.example")
	defer cleanupUser(t, u3.ID)
	cleanupHashtag(t, "#htmix")

	require.NoError(t, repo.RecordMention("h_ht_m1", "#htmix", u1.ID, true))
	require.NoError(t, repo.RecordMention("h_ht_m2", "#htmix", u2.ID, true))
	require.NoError(t, repo.RecordMention("h_ht_m3", "#htmix", u3.ID, false))

	got, err := repo.FindByName("#htmix")
	require.NoError(t, err)
	assert.Equal(t, 3, got.MentionedUsersCount)
	assert.Equal(t, 2, got.MentionedLocalUsersCount)
	assert.Equal(t, 1, got.MentionedRemoteUsersCount)
	assert.ElementsMatch(t, []string{u1.ID, u2.ID, u3.ID}, []string(got.MentionedUserIds))
	assert.ElementsMatch(t, []string{u1.ID, u2.ID}, []string(got.MentionedLocalUserIds))
	assert.ElementsMatch(t, []string{u3.ID}, []string(got.MentionedRemoteUserIds))
}

func TestHashtagRepository_RecordAttach_FreshRow(t *testing.T) {
	repo := NewHashtagRepository(testDB)
	user := insertTestUser(t, "u_ht_a1", "ht_a1")
	defer cleanupUser(t, user.ID)
	cleanupHashtag(t, "#htattach1")

	require.NoError(t, repo.RecordAttach("h_ht_a1", "#htattach1", user.ID, true))

	got, err := repo.FindByName("#htattach1")
	require.NoError(t, err)
	assert.Equal(t, 1, got.AttachedUsersCount)
	assert.Equal(t, 1, got.AttachedLocalUsersCount)
	assert.Equal(t, model.StringArray{user.ID}, got.AttachedLocalUserIds)
	// Mention 系は触らないこと
	assert.Equal(t, 0, got.MentionedUsersCount)
	assert.Empty(t, []string(got.MentionedUserIds))
}

func TestHashtagRepository_RecordAttach_Idempotent(t *testing.T) {
	repo := NewHashtagRepository(testDB)
	user := insertTestUser(t, "u_ht_aid", "ht_aid")
	defer cleanupUser(t, user.ID)
	cleanupHashtag(t, "#htattachidem")

	require.NoError(t, repo.RecordAttach("h_ht_aid1", "#htattachidem", user.ID, true))
	require.NoError(t, repo.RecordAttach("h_ht_aid2", "#htattachidem", user.ID, true))

	got, err := repo.FindByName("#htattachidem")
	require.NoError(t, err)
	assert.Equal(t, 1, got.AttachedUsersCount)
}

// RecordDetach は attached* から user を除去し count を減算する。
func TestHashtagRepository_RecordDetach(t *testing.T) {
	repo := NewHashtagRepository(testDB)
	u1 := insertTestUser(t, "u_ht_d1", "ht_d1")
	u2 := insertTestUser(t, "u_ht_d2", "ht_d2")
	defer cleanupUser(t, u1.ID)
	defer cleanupUser(t, u2.ID)
	cleanupHashtag(t, "#htdetach")

	require.NoError(t, repo.RecordAttach("h_ht_d1", "#htdetach", u1.ID, true))
	require.NoError(t, repo.RecordAttach("h_ht_d2", "#htdetach", u2.ID, true))

	// u1 を detach → count 2->1、u1 だけ配列から消える。
	require.NoError(t, repo.RecordDetach("#htdetach", u1.ID, true))
	got, err := repo.FindByName("#htdetach")
	require.NoError(t, err)
	assert.Equal(t, 1, got.AttachedUsersCount)
	assert.Equal(t, 1, got.AttachedLocalUsersCount)
	assert.Equal(t, model.StringArray{u2.ID}, got.AttachedLocalUserIds)
}

// 存在しない user / hashtag の detach は no-op (冪等、count を負にしない)。
func TestHashtagRepository_RecordDetach_Idempotent(t *testing.T) {
	repo := NewHashtagRepository(testDB)
	u := insertTestUser(t, "u_ht_di", "ht_di")
	defer cleanupUser(t, u.ID)
	cleanupHashtag(t, "#htdetachidem")

	require.NoError(t, repo.RecordAttach("h_ht_di1", "#htdetachidem", u.ID, true))
	// 二重 detach: 1 回目で 0、2 回目は list に居ないので no-op。
	require.NoError(t, repo.RecordDetach("#htdetachidem", u.ID, true))
	require.NoError(t, repo.RecordDetach("#htdetachidem", u.ID, true))
	got, err := repo.FindByName("#htdetachidem")
	require.NoError(t, err)
	assert.Equal(t, 0, got.AttachedUsersCount)
	assert.Empty(t, []string(got.AttachedLocalUserIds))

	// 存在しない hashtag への detach も error にならない。
	require.NoError(t, repo.RecordDetach("#htnope", u.ID, true))
}

func TestHashtagRepository_RecordMention_ConcurrentInsert(t *testing.T) {
	// 段階 1 INSERT が ON CONFLICT DO NOTHING で skip された経路でも段階 2 で
	// 正しく count が 1 になることを確認 (既存 row 上書きシナリオ)。
	repo := NewHashtagRepository(testDB)
	cleanupHashtag(t, "#htrace")
	// 事前に row だけ確保 (count=0)
	require.NoError(t, testDB.Exec(
		`INSERT INTO "hashtag" (id, name) VALUES (?, ?)`, "h_ht_pre", "#htrace",
	).Error)

	user := insertTestUser(t, "u_ht_race", "ht_race")
	defer cleanupUser(t, user.ID)

	// 違う id を渡しても既存行が選ばれて count++ される
	require.NoError(t, repo.RecordMention("h_ht_late", "#htrace", user.ID, true))

	got, err := repo.FindByName("#htrace")
	require.NoError(t, err)
	assert.Equal(t, "h_ht_pre", got.ID, "existing row id is preserved")
	assert.Equal(t, 1, got.MentionedUsersCount)
}
