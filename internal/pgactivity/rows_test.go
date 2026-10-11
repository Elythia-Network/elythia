package pgactivity

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

// fakeRows is a pgx.Rows with one row whose Scan fails (scanErr) or that
// ends with err.
type fakeRows struct {
	scanErr, err error
	done         bool
}

func (r *fakeRows) Close()                                       {}
func (r *fakeRows) Err() error                                   { return r.err }
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Next() bool {
	if r.done || r.scanErr == nil {
		return false
	}
	r.done = true
	return true
}
func (r *fakeRows) Scan(...any) error      { return r.scanErr }
func (r *fakeRows) Values() ([]any, error) { return nil, nil }
func (r *fakeRows) RawValues() [][]byte    { return nil }
func (r *fakeRows) Conn() *pgx.Conn        { return nil }

type fakeQuerier struct{ rows pgx.Rows }

func (q fakeQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) { return q.rows, nil }
func (q fakeQuerier) QueryRow(context.Context, string, ...any) pgx.Row        { return nil }

// TestList_ReadFailures checks that a row that cannot be read, or a result
// that ends with an error, fails the listing instead of returning a partial
// (possibly empty) one: an empty list would read as "no session".
func TestList_ReadFailures(t *testing.T) {
	boom := errors.New("boom")
	_, err := List(context.Background(), fakeQuerier{&fakeRows{scanErr: boom}}, "db")
	assert.ErrorIs(t, err, boom)
	_, err = List(context.Background(), fakeQuerier{&fakeRows{err: boom}}, "db")
	assert.ErrorIs(t, err, boom)
}
