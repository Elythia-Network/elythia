package backup

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
)

// Queryer is satisfied by *pgx.Conn and pgx.Tx.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// MigrationTables are the migration bookkeeping tables recorded in Meta, in
// the order they are recorded: the core track and the fork's local track
// (#3428).
var MigrationTables = []string{"schema_migrations", "schema_migrations_local"}

// NilMigrationVersion is MigrationState.Version for a bookkeeping table that
// exists but has no row (golang-migrate's NilVersion).
const NilMigrationVersion = -1

// rowCountTablesSQL lists the tables whose rows pg_dump writes: ordinary
// tables outside the system schemas that do not belong to an extension.
//
// 分割表の親 (relkind 'p') は数えない。親の count(*) は子の行を含むので、親と子の
// 両方を数えると同じ行を 2 回数える。行は子 (relkind 'r') の側で数える。
// 拡張の持ち物の表は pg_dump がデータを書かない (CREATE EXTENSION で作り直す)
// ので外す。DB 全体の schema を数えるのは pg_dump が DB 全体を取るためで、
// current_schema() には絞らない。
const rowCountTablesSQL = `SELECT n.nspname, c.relname
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind = 'r'
  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
  AND n.nspname NOT LIKE 'pg\_toast%'
  AND n.nspname NOT LIKE 'pg\_temp\_%'
  AND NOT EXISTS (
    SELECT 1 FROM pg_depend d
    WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e'
  )
ORDER BY n.nspname, c.relname`

// CountRows counts the rows of every table pg_dump dumps, keyed by
// "schema.table". Run it in the snapshot the dump uses so the counts match
// the dump exactly; verify (#3459) runs it on the restored database.
func CountRows(ctx context.Context, q Queryer) (map[string]int64, error) {
	rows, err := q.Query(ctx, rowCountTablesSQL)
	if err != nil {
		return nil, fmt.Errorf("backup: list tables: %w", err)
	}
	type table struct{ schema, name string }
	var tables []table
	for rows.Next() {
		var t table
		if err := rows.Scan(&t.schema, &t.name); err != nil {
			rows.Close()
			return nil, fmt.Errorf("backup: list tables: %w", err)
		}
		tables = append(tables, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("backup: list tables: %w", err)
	}
	counts := make(map[string]int64, len(tables))
	for _, t := range tables {
		var n int64
		ident := pgx.Identifier{t.schema, t.name}.Sanitize()
		if err := q.QueryRow(ctx, "SELECT count(*) FROM "+ident).Scan(&n); err != nil {
			return nil, fmt.Errorf("backup: count %s.%s: %w", t.schema, t.name, err)
		}
		counts[t.schema+"."+t.name] = n
	}
	return counts, nil
}

// ReadMigrations reads each table in MigrationTables, resolved through the
// search_path like golang-migrate does. A missing table is recorded with
// Missing, an empty one with NilMigrationVersion.
func ReadMigrations(ctx context.Context, q Queryer) ([]MigrationState, error) {
	out := make([]MigrationState, 0, len(MigrationTables))
	for _, table := range MigrationTables {
		st := MigrationState{Table: table}
		var exists bool
		if err := q.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil {
			return nil, fmt.Errorf("backup: look up %s: %w", table, err)
		}
		if !exists {
			st.Missing = true
			out = append(out, st)
			continue
		}
		// golang-migrate は 1 行だけを持つ。複数行あっても最大の番号を記録する。
		err := q.QueryRow(ctx, "SELECT version, dirty FROM "+pgx.Identifier{table}.Sanitize()+" ORDER BY version DESC LIMIT 1").
			Scan(&st.Version, &st.Dirty)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			st.Version = NilMigrationVersion
		case err != nil:
			return nil, fmt.Errorf("backup: read %s: %w", table, err)
		}
		out = append(out, st)
	}
	return out, nil
}

// ReadDatabaseSettings returns the settings set with ALTER DATABASE ... SET
// on the current database (not per role), as sorted "name=value" entries.
// pg_dump -Fc does not include them.
func ReadDatabaseSettings(ctx context.Context, q Queryer) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT unnest(s.setconfig)
FROM pg_db_role_setting s
JOIN pg_database d ON d.oid = s.setdatabase
WHERE d.datname = current_database() AND s.setrole = 0`)
	if err != nil {
		return nil, fmt.Errorf("backup: read database settings: %w", err)
	}
	settings, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("backup: read database settings: %w", err)
	}
	sort.Strings(settings)
	if settings == nil {
		settings = []string{}
	}
	return settings, nil
}

// ServerInfo is what Meta records about the server.
type ServerInfo struct {
	Version    string
	VersionNum int
	Database   string
}

// ReadServerInfo reads server_version, server_version_num and the database
// name.
func ReadServerInfo(ctx context.Context, q Queryer) (ServerInfo, error) {
	var si ServerInfo
	err := q.QueryRow(ctx, "SELECT current_setting('server_version'), current_setting('server_version_num')::int, current_database()").
		Scan(&si.Version, &si.VersionNum, &si.Database)
	if err != nil {
		return ServerInfo{}, fmt.Errorf("backup: read server version: %w", err)
	}
	return si, nil
}
