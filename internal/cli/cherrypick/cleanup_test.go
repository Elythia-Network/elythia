package cherrypick

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/maintenance"
)

// convertedDB is convertDB after a convert: nothing needs action and the
// marker is set.
func convertedDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := convertDB(t)
	for _, sql := range []string{
		`DELETE FROM note`, `DELETE FROM abuse_report_resolver`, `UPDATE instance SET "quarantineLimited" = false`,
		`UPDATE meta SET "repositoryUrl" = NULL, "feedbackUrl" = NULL`, `UPDATE "user" SET "isSuspended" = true WHERE id = 'p1'`,
	} {
		require.NoError(t, db.Exec(sql).Error)
	}
	require.NoError(t, maintenance.MarkCherryPickConverted(t.Context(), db))
	return db
}

func leftovers(t *testing.T, db *gorm.DB) int {
	t.Helper()
	left, err := maintenance.CherryPickLeftovers(t.Context(), db)
	require.NoError(t, err)
	return len(left)
}

func TestCleanup_DryRunAndApply(t *testing.T) {
	db := convertedDB(t)
	h := newConvertHarness(t, db)
	require.Equal(t, 0, cleanup(h.e, nil), h.stderr.String())
	for _, want := range []string{"user.approved", "instance.quarantineLimited", "abuse_report_resolver", "note.deleteAt", "nothing was dropped"} {
		assert.Contains(t, h.stdout.String(), want)
	}
	assert.Equal(t, 4, leftovers(t, db), "既定では消さない")

	h = newConvertHarness(t, db)
	assert.Equal(t, 2, cleanup(h.e, []string{"-apply"}))
	assert.Contains(t, h.stderr.String(), "pass -backup-taken")
	assert.Equal(t, 4, leftovers(t, db))

	h = newConvertHarness(t, db)
	require.Equal(t, 0, cleanup(h.e, []string{"-apply", "-backup-taken"}), h.stderr.String())
	assert.Contains(t, h.stdout.String(), "dropped 4 column(s) and table(s)")
	assert.Zero(t, leftovers(t, db))

	h = newConvertHarness(t, db)
	assert.Equal(t, 0, cleanup(h.e, []string{"-apply", "-backup-taken"}))
	assert.Contains(t, h.stdout.String(), "nothing left to clean up")

	// 消した後も check は通る。
	r, err := maintenance.InspectCherryPick(t.Context(), db, convertNow)
	require.NoError(t, err)
	assert.Zero(t, r.Unprocessed())
}

func TestCleanup_Refuses(t *testing.T) {
	t.Run("not converted", func(t *testing.T) {
		db := convertDB(t)
		h := newConvertHarness(t, db)
		assert.Equal(t, 1, cleanup(h.e, []string{"-apply", "-backup-taken"}))
		assert.Contains(t, h.stderr.String(), "run `elythia cherrypick convert` first")
		assert.Equal(t, 4, leftovers(t, db))
	})
	t.Run("still needs action", func(t *testing.T) {
		db := convertDB(t)
		require.NoError(t, maintenance.MarkCherryPickConverted(t.Context(), db))
		h := newConvertHarness(t, db)
		assert.Equal(t, ExitNeedsAction, cleanup(h.e, []string{"-apply", "-backup-taken"}))
		assert.Contains(t, h.stderr.String(), "some items still need action")
		assert.Equal(t, 4, leftovers(t, db))
	})
	t.Run("not CherryPick", func(t *testing.T) {
		db := convertDB(t)
		require.NoError(t, db.Exec(`DROP TABLE migrations`).Error)
		h := newConvertHarness(t, db)
		assert.Equal(t, 0, cleanup(h.e, nil))
		assert.Contains(t, h.stdout.String(), "nothing to clean up")
	})
	t.Run("unsupported version", func(t *testing.T) {
		db := convertDB(t)
		require.NoError(t, db.Exec(`DELETE FROM migrations WHERE name = 'ApprovalSignup1768893580518'`).Error)
		h := newConvertHarness(t, db)
		assert.Equal(t, ExitNeedsAction, cleanup(h.e, nil))
	})
}

func TestCleanup_ArgumentsAndSetup(t *testing.T) {
	h := newConvertHarness(t, nil)
	assert.Equal(t, 2, cleanup(h.e, []string{"extra"}))
	assert.Contains(t, h.stderr.String(), `unexpected argument "extra"`)
	h = newConvertHarness(t, nil)
	assert.Equal(t, 2, cleanup(h.e, []string{"-nope"}))

	h = newConvertHarness(t, nil)
	h.e.loadConfig = func(string) (*config.Config, error) { return nil, errors.New("no file") }
	assert.Equal(t, 1, cleanup(h.e, nil))
	assert.Contains(t, h.stderr.String(), "no file")

	h = newConvertHarness(t, nil)
	h.e.openDB = func(*config.Config) (*gorm.DB, func(), error) { return nil, nil, errors.New("refused") }
	assert.Equal(t, 1, cleanup(h.e, nil))
	assert.Contains(t, h.stderr.String(), "refused")
	assert.Equal(t, 2, Cleanup([]string{"-nope"}))
}
