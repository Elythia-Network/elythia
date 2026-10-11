package repository

import (
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newCheckedMetaRow(t *testing.T, id string) {
	t.Helper()
	host, pass := "smtp.example.com", "pw"
	require.NoError(t, testDB.Create(&model.Meta{ID: id, SMTPHost: &host, SMTPPass: &pass}).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "meta" WHERE id = ?`, id) })
}

func TestMetaRepository_UpdateChecked(t *testing.T) {
	newCheckedMetaRow(t, "m_chk_1")
	repo := NewMetaRepository(testDB).(CheckedMetaUpdater)

	var seen string
	require.NoError(t, repo.UpdateChecked(map[string]any{"name": "checked"}, func(cur *model.Meta) error {
		seen = *cur.SMTPHost
		return nil
	}))
	assert.Equal(t, "smtp.example.com", seen, "check sees the current row")
	var got model.Meta
	require.NoError(t, testDB.First(&got).Error)
	require.NotNil(t, got.Name)
	assert.Equal(t, "checked", *got.Name)

	refused := errors.New("refused")
	err := repo.UpdateChecked(map[string]any{"name": "not written"}, func(*model.Meta) error { return refused })
	assert.ErrorIs(t, err, refused)
	require.NoError(t, testDB.First(&got).Error)
	assert.Equal(t, "checked", *got.Name, "nothing is written when check refuses")
}

// 判定から書き込みまでの間、行はロックされていて、別の更新は待たされる。
// ロックしないと、判定の後に入った更新の上に、判定した前提で書いてしまう。
func TestMetaRepository_UpdateCheckedLocksTheRow(t *testing.T) {
	newCheckedMetaRow(t, "m_chk_2")
	repo := NewMetaRepository(testDB)
	checked := repo.(CheckedMetaUpdater)

	done := make(chan error, 1)
	require.NoError(t, checked.UpdateChecked(map[string]any{"smtpHost": "smtp.locked.example"}, func(*model.Meta) error {
		go func() { done <- repo.Update(map[string]any{"smtpHost": "smtp.concurrent.example"}) }()
		select {
		case err := <-done:
			t.Errorf("a concurrent update was not blocked by the check (err=%v)", err)
		case <-time.After(300 * time.Millisecond):
		}
		return nil
	}))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the concurrent update did not finish after the checked update committed")
	}
	var got model.Meta
	require.NoError(t, testDB.First(&got).Error)
	assert.Equal(t, "smtp.concurrent.example", *got.SMTPHost, "the blocked update runs after the commit")
}

type plainMetaRepo struct{ MetaRepository }

func TestCachedMetaRepository_UpdateChecked(t *testing.T) {
	newCheckedMetaRow(t, "m_chk_3")
	cached := NewCachedMetaRepositoryWithTTL(NewMetaRepository(testDB), time.Hour)
	notified := 0
	cached.SetInvalidationHook(func() { notified++ })
	before, err := cached.Fetch()
	require.NoError(t, err)
	require.Nil(t, before.Name)

	require.NoError(t, cached.UpdateChecked(map[string]any{"name": "via cache"}, func(*model.Meta) error { return nil }))
	after, err := cached.Fetch()
	require.NoError(t, err)
	require.NotNil(t, after.Name)
	assert.Equal(t, "via cache", *after.Name, "the cache is invalidated")
	assert.Equal(t, 1, notified)

	refused := errors.New("refused")
	assert.ErrorIs(t, cached.UpdateChecked(map[string]any{"name": "x"}, func(*model.Meta) error { return refused }), refused)
	assert.Equal(t, 1, notified, "a refused update does not notify")

	plain := NewCachedMetaRepositoryWithTTL(plainMetaRepo{NewMetaRepository(testDB)}, time.Hour)
	assert.ErrorIs(t, plain.UpdateChecked(map[string]any{"name": "x"}, func(*model.Meta) error { return nil }), ErrCheckedUpdateUnsupported)
}
