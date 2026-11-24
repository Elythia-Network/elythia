package serve

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
)

// The Supervisor of `elythia serve` checks every database it opens: a
// CherryPick database that convert has not finished on is refused, and so is
// one that cannot be checked. Other databases pass.
func TestSupervisorDeps_ChecksCherryPick(t *testing.T) {
	boom := errors.New("boom")
	for _, tt := range []struct {
		name    string
		needs   bool
		err     error
		wantErr error
	}{
		{name: "needs convert", needs: true, wantErr: errCherryPickNotConverted},
		{name: "check failed", err: boom, wantErr: boom},
		{name: "converted or not CherryPick", needs: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := &gorm.DB{}
			var got *gorm.DB
			orig := cherryPickNeedsConvert
			cherryPickNeedsConvert = func(_ context.Context, d *gorm.DB) (bool, error) {
				got = d
				return tt.needs, tt.err
			}
			t.Cleanup(func() { cherryPickNeedsConvert = orig })

			deps := supervisorDeps(&config.Config{}, &cache.RedisClients{})
			require.NotNil(t, deps.CheckDB)
			err := deps.CheckDB(db)
			assert.Same(t, db, got, "the opened database is checked")
			if tt.wantErr == nil {
				assert.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, tt.wantErr)
		})
	}
}

// The refusal names both ways out: running migrate again (nothing needs action
// but the first migrate up could not mark the database) and convert.
// tests/cherrypick/run.sh matches "refusing to start".
func TestErrCherryPickNotConverted_NamesBothFixes(t *testing.T) {
	msg := errCherryPickNotConverted.Error()
	assert.Contains(t, msg, "refusing to start")
	assert.Contains(t, msg, "run `elythia migrate` again")
	assert.Contains(t, msg, "`elythia cherrypick convert`")
}
