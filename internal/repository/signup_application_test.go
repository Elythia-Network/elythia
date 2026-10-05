package repository

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// newTestApplication builds a live application with the given claim code hash.
func newTestApplication(id, hash string) *model.SignupApplication {
	now := time.Now().UTC().Truncate(time.Second)
	return &model.SignupApplication{
		ID:            id,
		ClaimCodeHash: hash,
		Status:        model.SignupApplicationPending,
		CreatedAt:     now,
		UpdatedAt:     now,
		ExpiresAt:     now.Add(7 * 24 * time.Hour),
	}
}

func cleanupApplications(t *testing.T) {
	t.Helper()
	require.NoError(t, testDB.Exec(`DELETE FROM "signup_application"`).Error)
}

func TestSignupApplicationRepository_CreateAndFind(t *testing.T) {
	repo := NewSignupApplicationRepository(testDB)
	cleanupApplications(t)
	defer cleanupApplications(t)

	app := newTestApplication("sa_find_1", "hash-find-1")
	require.NoError(t, repo.Create(app))

	found, err := repo.FindByID(app.ID)
	require.NoError(t, err)
	assert.Equal(t, "hash-find-1", found.ClaimCodeHash)
	assert.Equal(t, model.SignupApplicationPending, found.Status)

	byCode, err := repo.FindByClaimCodeHash("hash-find-1")
	require.NoError(t, err)
	assert.Equal(t, app.ID, byCode.ID)
}

// クレームコードの hash は一意。**衝突を黙って通すと、別の申請を上書きするか
// 取り違えることになる。**
func TestSignupApplicationRepository_Create_RejectsDuplicateHash(t *testing.T) {
	repo := NewSignupApplicationRepository(testDB)
	cleanupApplications(t)
	defer cleanupApplications(t)

	require.NoError(t, repo.Create(newTestApplication("sa_dup_1", "same-hash")))

	err := repo.Create(newTestApplication("sa_dup_2", "same-hash"))
	assert.ErrorIs(t, err, ErrSignupApplicationCodeCollision)
}

// 連絡先という自然キーが無くなったので、同じ人が何度でも申請できる。
// **抑止は captcha とレート制限が担う。** DB は妨げない。
func TestSignupApplicationRepository_Create_AllowsMultipleLiveApplications(t *testing.T) {
	repo := NewSignupApplicationRepository(testDB)
	cleanupApplications(t)
	defer cleanupApplications(t)

	require.NoError(t, repo.Create(newTestApplication("sa_multi_1", "hash-1")))
	assert.NoError(t, repo.Create(newTestApplication("sa_multi_2", "hash-2")))
	assert.NoError(t, repo.Create(newTestApplication("sa_multi_3", "hash-3")))
}

func TestSignupApplicationRepository_FindByClaimCodeHash_NotFound(t *testing.T) {
	repo := NewSignupApplicationRepository(testDB)
	cleanupApplications(t)
	defer cleanupApplications(t)

	_, err := repo.FindByClaimCodeHash("no-such-hash")
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestSignupApplicationRepository_ListAndCount(t *testing.T) {
	repo := NewSignupApplicationRepository(testDB)
	cleanupApplications(t)
	defer cleanupApplications(t)

	pending := newTestApplication("sa_ls_p", "h1")
	require.NoError(t, repo.Create(pending))

	approved := newTestApplication("sa_ls_a", "h2")
	approved.Status = model.SignupApplicationApproved
	require.NoError(t, repo.Create(approved))

	rejected := newTestApplication("sa_ls_r", "h3")
	rejected.Status = model.SignupApplicationRejected
	require.NoError(t, repo.Create(rejected))

	tests := []struct {
		filter string
		want   []string
	}{
		{filter: SignupApplicationFilterPending, want: []string{"sa_ls_p"}},
		{filter: SignupApplicationFilterApproved, want: []string{"sa_ls_a"}},
		{filter: SignupApplicationFilterProcessed, want: []string{"sa_ls_r"}},
	}
	for _, tt := range tests {
		t.Run(tt.filter, func(t *testing.T) {
			rows, err := repo.List(tt.filter, 50, 0)
			require.NoError(t, err)
			ids := make([]string, 0, len(rows))
			for _, r := range rows {
				ids = append(ids, r.ID)
			}
			assert.ElementsMatch(t, tt.want, ids)

			count, err := repo.Count(tt.filter)
			require.NoError(t, err)
			assert.Equal(t, len(tt.want), count)
		})
	}

	// 未知のフィルタは "all" 扱い。**古いクライアントが管理画面を壊せないように。**
	rows, err := repo.List("no-such-filter", 50, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 3)

	count, err := repo.Count(SignupApplicationFilterAll)
	require.NoError(t, err)
	assert.Equal(t, 3, count)
}

func TestSignupApplicationRepository_List_Pagination(t *testing.T) {
	repo := NewSignupApplicationRepository(testDB)
	cleanupApplications(t)
	defer cleanupApplications(t)

	base := time.Now().UTC().Truncate(time.Second)
	for i, id := range []string{"sa_pg_1", "sa_pg_2", "sa_pg_3"} {
		app := newTestApplication(id, "hash-"+id)
		app.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		require.NoError(t, repo.Create(app))
	}

	// 新しい順。
	rows, err := repo.List(SignupApplicationFilterAll, 2, 0)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "sa_pg_3", rows[0].ID)
	assert.Equal(t, "sa_pg_2", rows[1].ID)

	rows, err = repo.List(SignupApplicationFilterAll, 2, 2)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "sa_pg_1", rows[0].ID)
}

func TestSignupApplicationRepository_ExpireStale(t *testing.T) {
	repo := NewSignupApplicationRepository(testDB)
	cleanupApplications(t)
	defer cleanupApplications(t)

	now := time.Now().UTC().Truncate(time.Second)

	stalePending := newTestApplication("sa_ex_p", "h_sa_ex_p_r1")
	stalePending.ExpiresAt = now.Add(-time.Hour)
	require.NoError(t, repo.Create(stalePending))

	staleApproved := newTestApplication("sa_ex_a", "h_sa_ex_a_r2")
	staleApproved.Status = model.SignupApplicationApproved
	staleApproved.ExpiresAt = now.Add(-time.Hour)
	require.NoError(t, repo.Create(staleApproved))

	fresh := newTestApplication("sa_ex_f", "h_sa_ex_f_r3")
	require.NoError(t, repo.Create(fresh))

	// 終端状態は期限を過ぎていても触らない (履歴を書き換えない)。
	rejected := newTestApplication("sa_ex_r", "h_sa_ex_r_r4")
	rejected.Status = model.SignupApplicationRejected
	rejected.ExpiresAt = now.Add(-time.Hour)
	require.NoError(t, repo.Create(rejected))

	changed, err := repo.ExpireStale(now)
	require.NoError(t, err)
	assert.Equal(t, 2, changed)

	for _, tc := range []struct{ id, want string }{
		{"sa_ex_p", model.SignupApplicationExpired},
		{"sa_ex_a", model.SignupApplicationExpired},
		{"sa_ex_f", model.SignupApplicationPending},
		{"sa_ex_r", model.SignupApplicationRejected},
	} {
		found, err := repo.FindByID(tc.id)
		require.NoError(t, err)
		assert.Equal(t, tc.want, found.Status, tc.id)
	}
}

func TestSignupApplicationRepository_UpdateFieldsTx(t *testing.T) {
	repo := NewSignupApplicationRepository(testDB)
	cleanupApplications(t)
	defer cleanupApplications(t)

	app := newTestApplication("sa_up_1", "h_sa_up_1_r1")
	require.NoError(t, repo.Create(app))

	admin := "admin_user_id"
	now := time.Now().UTC().Truncate(time.Second)
	require.NoError(t, repo.DB().Transaction(func(tx *gorm.DB) error {
		locked, err := repo.FindByIDForUpdateTx(tx, app.ID)
		if err != nil {
			return err
		}
		if locked.Status != model.SignupApplicationPending {
			t.Fatalf("unexpected status %q", locked.Status)
		}
		return repo.UpdateFieldsTx(tx, app.ID, map[string]any{
			"status":        model.SignupApplicationApproved,
			"processedById": admin,
			"processedAt":   now,
			"updatedAt":     now,
		})
	}))

	found, err := repo.FindByID(app.ID)
	require.NoError(t, err)
	assert.Equal(t, model.SignupApplicationApproved, found.Status)
	require.NotNil(t, found.ProcessedByID)
	assert.Equal(t, admin, *found.ProcessedByID)
	require.NotNil(t, found.ProcessedAt)
}

func TestSignupApplicationRepository_UpdateFieldsTx_EmptyIsNoop(t *testing.T) {
	repo := NewSignupApplicationRepository(testDB)
	cleanupApplications(t)
	defer cleanupApplications(t)

	app := newTestApplication("sa_up_2", "h_sa_up_2_r1")
	require.NoError(t, repo.Create(app))

	require.NoError(t, repo.DB().Transaction(func(tx *gorm.DB) error {
		return repo.UpdateFieldsTx(tx, app.ID, nil)
	}))

	found, err := repo.FindByID(app.ID)
	require.NoError(t, err)
	assert.Equal(t, model.SignupApplicationPending, found.Status)
}

func TestSignupApplicationRepository_FindByIDForUpdateTx_NotFound(t *testing.T) {
	repo := NewSignupApplicationRepository(testDB)
	cleanupApplications(t)
	defer cleanupApplications(t)

	require.NoError(t, repo.DB().Transaction(func(tx *gorm.DB) error {
		_, err := repo.FindByIDForUpdateTx(tx, "no-such-id")
		assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
		return nil
	}))
}

func TestSignupApplicationRepository_FindByID_NotFound(t *testing.T) {
	repo := NewSignupApplicationRepository(testDB)
	_, err := repo.FindByID("no-such-id")
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

// abortedTxRepo returns a repository bound to a transaction that PostgreSQL has
// already aborted, so every subsequent statement fails.
//
// DB 障害時の分岐を踏むための道具。**接続を壊す代わりに transaction を abort
// させる**ので、他のテストが使っている接続に影響しない。
func abortedTxRepo(t *testing.T) SignupApplicationRepository {
	t.Helper()
	tx := testDB.Begin()
	require.NoError(t, tx.Error)
	t.Cleanup(func() { tx.Rollback() })
	// わざと失敗させて abort 状態にする。以降の文はすべてエラーになる。
	require.Error(t, tx.Exec(`SELECT 1 FROM "no_such_table_for_error_path"`).Error)
	return NewSignupApplicationRepository(tx)
}

func TestSignupApplicationRepository_ErrorPaths(t *testing.T) {
	t.Run("Create surfaces non-unique errors as-is", func(t *testing.T) {
		repo := abortedTxRepo(t)
		err := repo.Create(newTestApplication("sa_err_1", "h_sa_err_1_r1"))
		require.Error(t, err)
		// 一意制約違反ではないので、専用エラーに化けないこと。
		assert.NotErrorIs(t, err, ErrSignupApplicationCodeCollision)
	})

	t.Run("FindByClaimCodeHash", func(t *testing.T) {
		repo := abortedTxRepo(t)
		_, err := repo.FindByClaimCodeHash("hash-1")
		assert.Error(t, err)
	})

	t.Run("List", func(t *testing.T) {
		repo := abortedTxRepo(t)
		_, err := repo.List(SignupApplicationFilterAll, 10, 0)
		assert.Error(t, err)
	})

	t.Run("Count", func(t *testing.T) {
		repo := abortedTxRepo(t)
		_, err := repo.Count(SignupApplicationFilterAll)
		assert.Error(t, err)
	})

	t.Run("ExpireStale", func(t *testing.T) {
		repo := abortedTxRepo(t)
		_, err := repo.ExpireStale(time.Now())
		assert.Error(t, err)
	})

	t.Run("FindByIDForUpdateTx", func(t *testing.T) {
		repo := abortedTxRepo(t)
		_, err := repo.FindByIDForUpdateTx(repo.DB(), "sa_err_1")
		assert.Error(t, err)
	})
}
