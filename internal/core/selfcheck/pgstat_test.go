package selfcheck

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/db/pgstat"
)

// #3909: pg_stat_statements の設定と拡張が食い違うときだけ警告する。
func TestCheckPgStatStatements(t *testing.T) {
	ctx := context.Background()
	assert.Equal(t, StatusSkip, CheckPgStatStatements(ctx, LocalDeps{}).Status)

	for name, tc := range map[string]struct {
		state  pgstat.State
		err    error
		status Status
	}{
		"not used":                {state: pgstat.State{}, status: StatusOK},
		"enabled":                 {state: pgstat.State{Preloaded: true, Installed: true}, status: StatusOK},
		"preloaded, no extension": {state: pgstat.State{Preloaded: true}, status: StatusWarn},
		"extension, no preload":   {state: pgstat.State{Installed: true}, status: StatusWarn},
		"state cannot be read":    {err: errors.New("connection refused"), status: StatusWarn},
	} {
		t.Run(name, func(t *testing.T) {
			res := CheckPgStatStatements(ctx, LocalDeps{PgStatStatements: func(context.Context) (pgstat.State, error) {
				return tc.state, tc.err
			}})
			assert.Equal(t, tc.status, res.Status, res.Detail)
			if tc.status == StatusWarn {
				assert.NotEmpty(t, res.Hint, "a warning says how to fix it")
			}
		})
	}

	// 拡張が無いときの直し方には、実行する SQL を書く。
	res := CheckPgStatStatements(ctx, LocalDeps{PgStatStatements: func(context.Context) (pgstat.State, error) {
		return pgstat.State{Preloaded: true}, nil
	}})
	assert.Contains(t, res.Hint, pgstat.CreateSQL)
}
