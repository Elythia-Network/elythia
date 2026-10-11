// Package pgactivity lists and terminates the sessions connected to a
// PostgreSQL database, as seen in pg_stat_activity. Both the restore of
// backups (#3461) and the maintenance mode (#3463) use it to decide that a
// database has no client left.
package pgactivity

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Querier is the part of *pgx.Conn the package uses.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Session is one backend connected to the database.
type Session struct {
	PID         int    `json:"pid"`
	User        string `json:"user"`
	Application string `json:"application"`
	// Addr is the client address, "local" for a UNIX socket, or "" when the
	// row belongs to a role the querying user cannot fully see.
	Addr string `json:"addr"`
	// SameRole reports whether the session belongs to the querying role, so
	// that it may be terminated without pg_signal_backend.
	SameRole bool `json:"sameRole"`
}

// String formats the session for logs and error messages.
func (s Session) String() string {
	addr := s.Addr
	if addr == "" {
		addr = "unknown"
	}
	return fmt.Sprintf("pid=%d user=%s application=%q from=%s", s.PID, s.User, s.Application, addr)
}

// listSQL lists every session on $1 except the querying one and the
// autovacuum workers.
//
// backend_type では絞らない。別の role の行では backend_type が null になり
// (client_addr と state も null)、'client backend' で絞ると運営者の psql などを
// 見落とす (#3463 の設計 §7 で実測)。autovacuum worker は usesysid が null なので、
// それで外す。名前の変更 (ALTER DATABASE RENAME) も autovacuum は止めて通る。
const listSQL = `
	SELECT pid,
		coalesce(usename, ''),
		coalesce(application_name, ''),
		CASE WHEN backend_type IS NULL THEN '' ELSE coalesce(host(client_addr), 'local') END,
		coalesce(usename = current_user, false)
	FROM pg_stat_activity
	WHERE datname = $1 AND pid <> pg_backend_pid() AND usesysid IS NOT NULL
	ORDER BY pid`

// List returns the sessions connected to database, except the caller's own.
func List(ctx context.Context, q Querier, database string) ([]Session, error) {
	rows, err := q.Query(ctx, listSQL, database)
	if err != nil {
		return nil, fmt.Errorf("pgactivity: read pg_stat_activity: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.PID, &s.User, &s.Application, &s.Addr, &s.SameRole); err != nil {
			return nil, fmt.Errorf("pgactivity: read pg_stat_activity: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgactivity: read pg_stat_activity: %w", err)
	}
	return out, nil
}

// terminateSQL ends $1 only while it is still a backend on $2 of the current
// role whose application_name is $3.
//
// pid だけで切ると、数えてから切るまでの間に pid が別の接続に使い回されたとき、
// 関係の無い接続を切る。確かめるのと切るのを 1 文にする。名前は数えたときの
// 完全な名前で比べ、同じ接頭辞を持つ別のプロセスの接続を切らない
// (#3463 のレビュー2周目 4)。
const terminateSQL = `
	SELECT coalesce(bool_or(pg_terminate_backend(pid)), false)
	FROM pg_stat_activity
	WHERE pid = $1 AND datname = $2 AND usename = current_user
		AND application_name = $3`

// Terminate ends the backend pid with pg_terminate_backend, but only while it
// is still connected to database as the current role with application_name
// application (as List returned it). It reports whether it ended one (false
// when that backend is gone or no longer matches).
func Terminate(ctx context.Context, q Querier, pid int, database, application string) (bool, error) {
	var ok bool
	if err := q.QueryRow(ctx, terminateSQL, pid, database, application).Scan(&ok); err != nil {
		return false, fmt.Errorf("pgactivity: terminate %d: %w", pid, err)
	}
	return ok, nil
}

// Join formats sessions for an error message.
func Join(sessions []Session) string {
	parts := make([]string, len(sessions))
	for i, s := range sessions {
		parts[i] = s.String()
	}
	return strings.Join(parts, "; ")
}
