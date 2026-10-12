package backup

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Kinds of a kept database (KeptDatabase.Kind).
const (
	// KeptBeforeRestore is the database a restore swapped out:
	// <db>_before_restore_<time>.
	KeptBeforeRestore = "before_restore"
	// KeptRolledBack is the database a rollback (or the revert of a failed
	// restore) swapped out: <db>_rolled_back_<time>.
	KeptRolledBack = "rolled_back"
)

// keptTimeLayout is the time in the name of a kept database (UTC).
const keptTimeLayout = "20060102150405"

// ParseKeptName reports whether name is a database kept by a restore or a
// rollback of database, and returns its kind and the time in its name.
//
// 前方一致と時刻の形を Go の側で確かめる。SQL の LIKE では _ が任意の 1 文字に
// なるので、<db>Xbefore_restore_... のような無関係な DB まで拾ってしまう。
func ParseKeptName(database, name string) (kind string, at time.Time, ok bool) {
	for _, k := range []string{KeptBeforeRestore, KeptRolledBack} {
		suffix, found := strings.CutPrefix(name, database+"_"+k+"_")
		if !found || len(suffix) != len(keptTimeLayout) {
			continue
		}
		for _, c := range suffix {
			if c < '0' || c > '9' {
				return "", time.Time{}, false
			}
		}
		t, err := time.ParseInLocation(keptTimeLayout, suffix, time.UTC)
		if err != nil {
			return "", time.Time{}, false
		}
		return k, t, true
	}
	return "", time.Time{}, false
}

// RollbackName returns the name a rollback at t keeps the current database
// under: <database>_rolled_back_<time>, the name `elythia backup restore
// -rollback` uses.
func RollbackName(database string, t time.Time) (string, error) {
	name := database + "_" + KeptRolledBack + "_" + t.UTC().Format(keptTimeLayout)
	if len(name) > maxIdentifierLen {
		return "", fmt.Errorf("backup: %q is longer than %d bytes; the database name is too long for a rollback", name, maxIdentifierLen)
	}
	return name, nil
}

// KeptDatabase is a database kept by a restore or a rollback, which a
// rollback can swap back in.
type KeptDatabase struct {
	Name string `json:"name"`
	// Kind is KeptBeforeRestore or KeptRolledBack.
	Kind string `json:"kind"`
	// At is the time in the name: when it was swapped out.
	At time.Time `json:"at"`
	// Size is the size in bytes, or nil when the database user cannot
	// read it.
	Size *int64 `json:"size"`
	// Migrations is the state of its migration tables (nil when they could
	// not be read).
	Migrations []MigrationState `json:"migrations"`
	// NeedsMigration is true when it is behind the bundled migrations; a
	// rollback migrates it before swapping it in.
	NeedsMigration bool `json:"needsMigration"`
	// Problem is why a rollback refuses it, or "": one of the Kept*
	// problem values.
	Problem string `json:"problem,omitempty"`
}

// Problems of a kept database (KeptDatabase.Problem).
const (
	KeptProblemTooNew     = "migrationTooNew"
	KeptProblemDirty      = "migrationDirty"
	KeptProblemNotElythia = "notElythia"
	KeptProblemUnreadable = "unreadable"
)

// KeptProblem returns the KeptDatabase.Problem for an error of
// CheckDatabaseMigrations ("" for nil).
func KeptProblem(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrRestoreMigrationTooNew):
		return KeptProblemTooNew
	case errors.Is(err, ErrRestoreMigrationDirty):
		return KeptProblemDirty
	case errors.Is(err, ErrRestoreNotElythia):
		return KeptProblemNotElythia
	}
	return KeptProblemUnreadable
}

// KeptDatabases lists the databases kept by restores and rollbacks of
// database, newest first, with their migration state. It connects to
// maintenance (empty means "postgres") and briefly to each kept database.
func (r *Restorer) KeptDatabases(ctx context.Context, maintenance, database string) ([]KeptDatabase, error) {
	admin, err := r.connect(ctx, maintenanceDB(maintenance))
	if err != nil {
		return nil, err
	}
	defer admin.Close(context.Background())
	rows, err := admin.Query(ctx, `SELECT datname FROM pg_database`)
	if err != nil {
		return nil, fmt.Errorf("backup: read pg_database: %w", err)
	}
	all, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("backup: read pg_database: %w", err)
	}
	var out []KeptDatabase
	var names []string
	for _, n := range all {
		kind, at, ok := ParseKeptName(database, n)
		if !ok {
			continue
		}
		out = append(out, KeptDatabase{Name: n, Kind: kind, At: at})
		names = append(names, n)
	}
	if len(out) == 0 {
		return []KeptDatabase{}, nil
	}
	// 大きさは、繋げない DB では pg_database_size がエラーになるので、権限のある
	// ものだけ測る (1 つのために一覧ごと落とさない)。
	rows, err = admin.Query(ctx, `
		SELECT datname, CASE WHEN has_database_privilege(oid, 'CONNECT') THEN pg_database_size(oid) END
		FROM pg_database WHERE datname = ANY($1)`, names)
	if err != nil {
		return nil, fmt.Errorf("backup: read the database sizes: %w", err)
	}
	sizes := map[string]*int64{}
	var name string
	var size *int64
	if _, err := pgx.ForEachRow(rows, []any{&name, &size}, func() error {
		sizes[name] = size
		return nil
	}); err != nil {
		return nil, fmt.Errorf("backup: read the database sizes: %w", err)
	}
	for i := range out {
		out[i].Size = sizes[out[i].Name]
		dm, err := r.CheckDatabaseMigrations(ctx, out[i].Name)
		if dm != nil {
			out[i].Migrations = dm.States
			out[i].NeedsMigration = dm.NeedsMigration
		}
		out[i].Problem = KeptProblem(err)
	}
	slices.SortStableFunc(out, func(a, b KeptDatabase) int {
		if c := b.At.Compare(a.At); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// CheckRollback runs the stop conditions of a rollback that swaps kept back
// in as database and keeps database as keepAs: kept exists and nobody is
// connected to it (waiting a few seconds for sessions that are closing),
// keepAs is free, and the database user may rename both
// (ErrRestoreNoPrivilege). It connects to maintenance (empty means
// "postgres"). The connections on database are checked again by
// ReplaceDatabase, in the maintenance.
func (r *Restorer) CheckRollback(ctx context.Context, maintenance, database, kept, keepAs string) error {
	admin, err := r.connect(ctx, maintenanceDB(maintenance))
	if err != nil {
		return err
	}
	defer admin.Close(context.Background())
	for _, name := range []string{database, kept} {
		db, err := databaseInfo(ctx, admin, name)
		if err != nil {
			return err
		}
		if err := checkSwapPrivilege(ctx, admin, name, db); err != nil {
			return err
		}
	}
	if _, err := databaseInfo(ctx, admin, keepAs); err == nil {
		return fmt.Errorf("backup: database %q already exists", keepAs)
	}
	// 一覧 (KeptDatabases) は退避した DB へ短く繋いで版を読むので、一覧の直後の準備では
	// その閉じた接続が少しの間残る。名前の入れ替えと同じだけ待つ。
	return waitNoConnections(ctx, admin, kept, replacementSettle)
}
