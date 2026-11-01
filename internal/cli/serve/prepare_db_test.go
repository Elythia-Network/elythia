package serve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/db/pgstat"
)

// The Supervisor of `elythia serve` runs the pg_stat_statements check on every
// database it opens (#3909).
func TestSupervisorDeps_PreparesTheDatabase(t *testing.T) {
	var got *gorm.DB
	orig := ensurePgStatStatements
	ensurePgStatStatements = func(_ context.Context, db *gorm.DB) (pgstat.Outcome, error) {
		got = db
		return pgstat.OutcomeInstalled, nil
	}
	t.Cleanup(func() { ensurePgStatStatements = orig })

	deps := supervisorDeps(&config.Config{}, &cache.RedisClients{})
	require.NotNil(t, deps.PrepareDB)
	db := &gorm.DB{}
	deps.PrepareDB(db)
	assert.Same(t, db, got, "PrepareDB checks pg_stat_statements on the database it is given")
}

func TestPrepareDatabase(t *testing.T) {
	run := func(outcome pgstat.Outcome, ensureErr error) (string, *gorm.DB) {
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, nil))
		db := &gorm.DB{}
		var got *gorm.DB
		prepareDatabase(db, log, func(_ context.Context, d *gorm.DB) (pgstat.Outcome, error) {
			got = d
			return outcome, ensureErr
		})
		assert.Same(t, db, got, "the opened database is checked")
		return buf.String(), got
	}

	t.Run("cannot create: warns with the SQL to run", func(t *testing.T) {
		out, _ := run("", fmt.Errorf("%w: permission denied", pgstat.ErrCreate))
		assert.Contains(t, out, "level=WARN")
		assert.Contains(t, out, pgstat.CreateSQL)
	})
	t.Run("other failure: warns", func(t *testing.T) {
		out, _ := run("", errors.New("timeout"))
		assert.Contains(t, out, "level=WARN")
		assert.NotContains(t, out, pgstat.CreateSQL)
	})
	t.Run("created: info", func(t *testing.T) {
		out, _ := run(pgstat.OutcomeCreated, nil)
		assert.Contains(t, out, "level=INFO")
		assert.Contains(t, out, "created the pg_stat_statements extension")
	})
	// 既定の運営者 (読み込んでいない) と、もう作ってある場合は何も出さない。
	for _, o := range []pgstat.Outcome{pgstat.OutcomeNotPreloaded, pgstat.OutcomeInstalled} {
		t.Run(string(o)+": silent", func(t *testing.T) {
			out, _ := run(o, nil)
			assert.Empty(t, out)
		})
	}
}
