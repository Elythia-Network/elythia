package db

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// fakePool is a gorm.ConnPool that is neither *sql.DB nor a GetDBConnector.
type fakePool struct{}

func (fakePool) PrepareContext(context.Context, string) (*sql.Stmt, error) { return nil, nil }
func (fakePool) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, nil
}
func (fakePool) QueryContext(context.Context, string, ...any) (*sql.Rows, error) { return nil, nil }
func (fakePool) QueryRowContext(context.Context, string, ...any) *sql.Row        { return nil }

// TestCloseConnPool covers the pool shapes dbresolver hands to Close: a bare
// *sql.DB, one wrapped by PrepareStmt, and shapes it cannot close.
func TestCloseConnPool(t *testing.T) {
	open := func(t *testing.T) *sql.DB {
		t.Helper()
		// sql.Open は接続しないので、到達できない宛先でよい。
		d, err := sql.Open("pgx", "postgres://x:x@127.0.0.1:1/x")
		require.NoError(t, err)
		return d
	}

	bare := open(t)
	require.NoError(t, closeConnPool(bare))
	assert.ErrorContains(t, bare.Ping(), "closed")

	wrapped := open(t)
	require.NoError(t, closeConnPool(&gorm.PreparedStmtDB{ConnPool: wrapped}))
	assert.ErrorContains(t, wrapped.Ping(), "closed")

	assert.ErrorContains(t, closeConnPool(&gorm.PreparedStmtDB{ConnPool: fakePool{}}), "get replica sql.DB")
	assert.ErrorContains(t, closeConnPool(fakePool{}), "unsupported type")
}
