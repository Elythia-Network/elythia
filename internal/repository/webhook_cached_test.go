package repository

import (
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func webhookIDs(rows []*model.Webhook) []string {
	out := make([]string, 0, len(rows))
	for _, w := range rows {
		out = append(out, w.ID)
	}
	return out
}

func insertWebhookRow(t *testing.T, id, userID string, active bool) {
	t.Helper()
	require.NoError(t, testDB.Exec(
		`INSERT INTO "webhook" (id, "userId", name, "on", url, secret, active) VALUES (?, ?, 'h', '{note}', 'https://example.com/hook', 's', ?)`,
		id, userID, active).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "webhook" WHERE id = ?`, id) })
}

// 有効な webhook を全件メモリに持ち、ListActiveByUserID をそこから返す (#4218)。
// 書き込みで捨て、他のプロセスへの知らせ (onChange) を呼ぶ。送信の状態の更新では
// 捨てない。
func TestCachedWebhookRepository_ServesActiveFromCache(t *testing.T) {
	user := insertTestUser(t, "u_whc_1", "whcacheuser")
	defer cleanupUser(t, user.ID)
	repo := NewCachedWebhookRepositoryFromDB(testDB)
	changes := 0
	repo.SetInvalidationHook(func() { changes++ })

	insertWebhookRow(t, "whc_active", user.ID, true)
	insertWebhookRow(t, "whc_inactive", user.ID, false)
	got, err := repo.ListActiveByUserID(user.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"whc_active"}, webhookIDs(got))

	// wrapper を通らない書き込みは、キャッシュに残る間は見えない。
	insertWebhookRow(t, "whc_direct", user.ID, true)
	got, err = repo.ListActiveByUserID(user.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"whc_active"}, webhookIDs(got))

	// 送信の状態の更新では捨てない。
	require.NoError(t, repo.UpdateLatestStatus("whc_active", time.Now(), 200))
	got, err = repo.ListActiveByUserID(user.ID)
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.Zero(t, changes)

	// 作成で捨てる。
	created := &model.Webhook{ID: "whc_created", UserID: user.ID, Name: "c", On: model.StringArray{"note"}, URL: "https://example.com/c", Secret: "s", Active: true}
	require.NoError(t, repo.Create(created))
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "webhook" WHERE id = ?`, created.ID) })
	got, err = repo.ListActiveByUserID(user.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"whc_active", "whc_direct", "whc_created"}, webhookIDs(got))
	assert.Equal(t, 1, changes)

	// 更新 (無効にする) で捨てる。
	created.Active = false
	require.NoError(t, repo.Update(created))
	got, err = repo.ListActiveByUserID(user.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"whc_active", "whc_direct"}, webhookIDs(got))
	assert.Equal(t, 2, changes)

	// 削除で捨てる。
	require.NoError(t, repo.Delete("whc_direct", user.ID))
	got, err = repo.ListActiveByUserID(user.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"whc_active"}, webhookIDs(got))
	assert.Equal(t, 3, changes)

	// webhook を持たない利用者は空。
	none, err := repo.ListActiveByUserID("nobody")
	require.NoError(t, err)
	assert.Empty(t, none)
}

// TTL が切れたら読み直す。DropLocal でも捨てる。
func TestCachedWebhookRepository_TTLAndDropLocal(t *testing.T) {
	user := insertTestUser(t, "u_whc_2", "whcacheuser2")
	defer cleanupUser(t, user.ID)
	inner := &webhookRepository{db: testDB}
	repo := NewCachedWebhookRepositoryWithTTL(inner, inner, time.Hour)

	got, err := repo.ListActiveByUserID(user.ID)
	require.NoError(t, err)
	assert.Empty(t, got)
	insertWebhookRow(t, "whc_ttl", user.ID, true)
	got, err = repo.ListActiveByUserID(user.ID)
	require.NoError(t, err)
	assert.Empty(t, got, "served from the cache until the TTL")
	// 時計を待たずに、積んだ時刻を TTL より前へずらす。
	repo.mu.Lock()
	repo.at = repo.at.Add(-2 * time.Hour)
	repo.mu.Unlock()
	got, err = repo.ListActiveByUserID(user.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"whc_ttl"}, webhookIDs(got))

	insertWebhookRow(t, "whc_drop", user.ID, true)
	repo2 := NewCachedWebhookRepository(inner, inner)
	_, err = repo2.ListActiveByUserID(user.ID)
	require.NoError(t, err)
	testDB.Exec(`DELETE FROM "webhook" WHERE id = ?`, "whc_drop")
	repo2.DropLocal()
	got, err = repo2.ListActiveByUserID(user.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"whc_ttl"}, webhookIDs(got))
}

// NotifyChanged は自分のキャッシュを捨て、他のプロセスへ知らせる (退会のジョブが呼ぶ)。
func TestCachedWebhookRepository_NotifyChanged(t *testing.T) {
	user := insertTestUser(t, "u_whc_5", "whcacheuser5")
	defer cleanupUser(t, user.ID)
	repo := NewCachedWebhookRepositoryFromDB(testDB)
	changes := 0
	repo.SetInvalidationHook(func() { changes++ })
	insertWebhookRow(t, "whc_notify", user.ID, true)
	_, err := repo.ListActiveByUserID(user.ID)
	require.NoError(t, err)
	testDB.Exec(`DELETE FROM "webhook" WHERE id = ?`, "whc_notify")

	repo.NotifyChanged()
	got, err := repo.ListActiveByUserID(user.ID)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Equal(t, 1, changes)
}

// 返す値は複製で、呼び出し側が書き換えてもキャッシュは変わらない。
func TestCachedWebhookRepository_ReturnsCopies(t *testing.T) {
	user := insertTestUser(t, "u_whc_3", "whcacheuser3")
	defer cleanupUser(t, user.ID)
	insertWebhookRow(t, "whc_copy", user.ID, true)
	repo := NewCachedWebhookRepositoryFromDB(testDB)

	got, err := repo.ListActiveByUserID(user.ID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	got[0].On[0] = "changed"
	got[0].Name = "changed"
	again, err := repo.ListActiveByUserID(user.ID)
	require.NoError(t, err)
	assert.Equal(t, model.StringArray{"note"}, again[0].On)
	assert.Equal(t, "h", again[0].Name)
}

type failingWebhookLister struct{ calls int }

func (f *failingWebhookLister) ListAllActive() ([]*model.Webhook, error) {
	f.calls++
	return nil, errors.New("db down")
}

// 読み込みの失敗はキャッシュしない。
func TestCachedWebhookRepository_DoesNotCacheErrors(t *testing.T) {
	lister := &failingWebhookLister{}
	repo := NewCachedWebhookRepository(&webhookRepository{db: testDB}, lister)
	_, err := repo.ListActiveByUserID("u")
	require.Error(t, err)
	_, err = repo.ListActiveByUserID("u")
	require.Error(t, err)
	assert.Equal(t, 2, lister.calls)
}

// 読むだけのメソッドは内側へそのまま渡す。
func TestCachedWebhookRepository_PassesReadsThrough(t *testing.T) {
	user := insertTestUser(t, "u_whc_4", "whcacheuser4")
	defer cleanupUser(t, user.ID)
	insertWebhookRow(t, "whc_read", user.ID, false)
	repo := NewCachedWebhookRepositoryFromDB(testDB)

	w, err := repo.FindByID("whc_read")
	require.NoError(t, err)
	assert.Equal(t, user.ID, w.UserID)
	w, err = repo.FindByIDAndUserID("whc_read", user.ID)
	require.NoError(t, err)
	assert.Equal(t, "whc_read", w.ID)
	rows, err := repo.ListByUserID(user.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"whc_read"}, webhookIDs(rows))
	n, err := repo.CountByUserID(user.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)
}

// 列に入らない値は DB へ渡さずに not found にする (#3025)。
func TestCachedWebhookRepository_RejectsUnstorableIDs(t *testing.T) {
	repo := NewCachedWebhookRepositoryFromDB(testDB)
	_, err := repo.FindByID("a\x00b")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = repo.FindByIDAndUserID("a\x00b", "u")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = repo.FindByIDAndUserID("w", "u\x00")
	assert.ErrorIs(t, err, ErrNotFound)
}
