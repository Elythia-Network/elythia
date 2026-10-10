package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"
	gomigrate "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // golang-migrate の pgx5 driver
	"github.com/golang-migrate/migrate/v4/source"
	_ "github.com/golang-migrate/migrate/v4/source/file" // file:// source
	"github.com/jackc/pgx/v5"
)

// RestoreMode selects where `backup restore` writes (#3461).
type RestoreMode string

const (
	// RestoreSwap restores into a new database on the same server, checks
	// it, and then swaps names with the current database, which is kept as
	// <db>_before_restore_<time>.
	RestoreSwap RestoreMode = "swap"
	// RestoreEmpty restores into an existing database that has no tables
	// (a new server for a PostgreSQL major upgrade, or a move to another
	// host).
	RestoreEmpty RestoreMode = "empty"
)

// LatestGeneration is accepted as RestoreOptions.ID for the newest
// generation whose verification passed.
const LatestGeneration = "latest"

// Reasons a restore stops before touching any database. They wrap the
// returned error so that callers and tests can tell them apart.
var (
	ErrRestoreNotConfirmed    = errors.New("backup: restore target not confirmed")
	ErrRestoreConnections     = errors.New("backup: the restore target has open connections")
	ErrRestoreMigrationTooNew = errors.New("backup: the backup has migrations this binary does not know")
	ErrRestoreNoPrivilege     = errors.New("backup: insufficient privilege for a same-server restore")
	ErrRestoreNotEmpty        = errors.New("backup: the restore target is not empty")
	ErrRestoreChecksum        = errors.New("backup: the dump does not match its metadata")
	ErrRestoreRowMismatch     = errors.New("backup: restored row counts differ from the backup")
	ErrRestoreVersion         = errors.New("backup: pg_restore is newer than the target server")
	ErrRestoreLocale          = errors.New("backup: the restore target has a different encoding or locale from the backup")
	ErrRestoreExtension       = errors.New("backup: the target server lacks extensions the backup uses")
	// ErrRestoreSwitchUnknown is returned when renaming the databases failed
	// and pg_database does not tell whether the rename happened.
	ErrRestoreSwitchUnknown = errors.New("backup: could not tell whether the database names were switched")
)

// maxIdentifierLen is NAMEDATALEN-1 of a default PostgreSQL build.
const maxIdentifierLen = 63

// RestoreExecFunc runs a PostgreSQL client program. env is added to the
// process environment.
type RestoreExecFunc func(ctx context.Context, program string, args, env []string,
	stdin io.Reader, stdout, stderr io.Writer) error

// ExecLocal runs program on this host. It is the RestoreExecFunc outside
// tests.
func ExecLocal(ctx context.Context, program string, args, env []string,
	stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

// RestoreConn tells the restorer how to reach the PostgreSQL server for a
// given database name.
type RestoreConn struct {
	// SQL returns a pgx connection string.
	SQL func(dbname string) string
	// Migrate returns a golang-migrate database URL (pgx5 scheme).
	Migrate func(dbname string) string
	// Tool returns the --dbname argument of pg_restore and extra
	// environment variables (PGPASSWORD). The password must not be in the
	// returned conninfo, because process arguments are visible to others.
	Tool func(dbname string) (conninfo string, env []string)
}

// Restorer restores a generation from Storage (#3461).
type Restorer struct {
	Storage Storage
	Conn    RestoreConn
	// Exec runs pg_restore. Nil means ExecLocal.
	Exec RestoreExecFunc
	// PgRestore is the pg_restore program. Empty means "pg_restore".
	PgRestore string
	// Identities decrypt age-encrypted dumps.
	Identities []age.Identity
	// CoreMigrationsDir and LocalMigrationsDir are the bundled migrations.
	CoreMigrationsDir  string
	LocalMigrationsDir string
	// TempDir holds the downloaded dump. Empty means os.TempDir().
	TempDir string
	// Out receives progress messages. Nil discards them.
	Out io.Writer
	// Now is time.Now unless replaced by tests.
	Now func() time.Time

	// afterReplace runs after the name swap and its error is added to the
	// result (tests only).
	afterReplace func() error
}

// RestoreOptions are the inputs of one restore.
type RestoreOptions struct {
	// ID is a generation ID or LatestGeneration.
	ID   string
	Mode RestoreMode
	// Database is the name the server uses (db.db of the config).
	Database string
	// Confirm must equal Database.
	Confirm string
	// MaintenanceDB is the database the swap mode connects to while it
	// creates and renames databases. Empty means "postgres".
	MaintenanceDB string
	// SkipConnectionCheck makes CheckTarget accept open connections on the
	// target. Only for callers that restore into the new database while the
	// server is still running and check again before ReplaceDatabase.
	SkipConnectionCheck bool
	// At is the time the swap mode names its databases after (RestoreNames).
	// Zero means now; Restore fixes it once so that CheckTarget checks the
	// same names it then creates.
	At time.Time
}

// RestoreResult describes a finished restore.
type RestoreResult struct {
	ID       string
	Mode     RestoreMode
	Database string
	// BeforeRestore is the name the previous database now has (swap mode).
	BeforeRestore string
	// Verified is true when the generation has a passing verify.json.
	Verified bool
	Meta     Meta
	// Settings are the per-database settings that were applied again.
	Settings []string
	// Migrations is the state of the bookkeeping tables after migrating.
	Migrations []MigrationState
}

func (r *Restorer) logf(format string, args ...any) {
	if r.Out != nil {
		fmt.Fprintf(r.Out, format+"\n", args...)
	}
}

func (r *Restorer) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// RestorePlan is a generation chosen for a restore and checked against
// this binary (Load).
type RestorePlan struct {
	ID string
	// Verified is true when the generation has a passing verify.json.
	Verified bool
	Meta     Meta
}

// Restore runs every step of a restore in order: Load, CheckTarget,
// CheckCompatibility, RestoreInto, VerifyRestored, ApplyDatabaseSettings, Migrate and, in the
// swap mode, ReplaceDatabase. The steps are exported so that a caller can
// run the first ones while the server is still up and only the name swap
// while it is stopped (#3463).
//
// On failure nothing that existed before is changed: the swap mode drops
// the database it created, and the empty mode restores in a single
// transaction (a failure after pg_restore leaves the restored data in the
// target database, which was empty).
func (r *Restorer) Restore(ctx context.Context, opts RestoreOptions) (*RestoreResult, error) {
	if opts.Mode != RestoreSwap && opts.Mode != RestoreEmpty {
		return nil, fmt.Errorf("backup: unknown restore mode %q (want swap or empty)", opts.Mode)
	}
	// 何かを読む前に、名前の確認だけは済ませる。
	if err := checkConfirm(opts); err != nil {
		return nil, err
	}
	plan, err := r.Load(ctx, opts.ID)
	if err != nil {
		return nil, err
	}
	// 名前の元になる時刻は 1 回だけ取る。CheckTarget が「まだ無い」と確かめた名前と、
	// 実際に作る名前を揃えるため (秒の境目をまたぐと別の名前になる)。
	if opts.At.IsZero() {
		opts.At = r.now()
	}
	if err := r.CheckTarget(ctx, opts); err != nil {
		return nil, err
	}
	if err := r.CheckCompatibility(ctx, plan, opts); err != nil {
		return nil, err
	}
	res := &RestoreResult{ID: plan.ID, Mode: opts.Mode, Database: opts.Database, Verified: plan.Verified, Meta: plan.Meta}
	target := opts.Database
	var before string
	if opts.Mode == RestoreSwap {
		target, before, err = RestoreNames(opts.Database, opts.At)
		if err != nil {
			return nil, err
		}
	}
	if err := r.RestoreInto(ctx, plan, opts, target); err != nil {
		return nil, err
	}
	err = r.finish(ctx, plan, opts, target, before, res)
	if err != nil && opts.Mode == RestoreSwap {
		if errors.Is(err, ErrRestoreSwitchUnknown) {
			// 入れ替わったかどうか分からないので、作った DB は消さない。消すと、
			// 入れ替わっていた場合に戻したばかりの DB を消してしまう。
			return nil, err
		}
		// 作った DB だけを消す。今の DB には触っていない。
		if derr := r.DropDatabase(context.Background(), opts.MaintenanceDB, target); derr != nil {
			r.logf("warning: could not drop %s: %v", target, derr)
		} else {
			r.logf("dropped %s; the current database %s was not changed", target, opts.Database)
		}
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

type swapState int

const (
	swapUnknown swapState = iota
	swapDone
	swapNotDone
)

// swapOutcome tells from pg_database whether a failed name swap happened:
// restored (the replacement) is gone and before (the kept name) exists
// (done), or restored exists and before does not (not done).
func (r *Restorer) swapOutcome(maintenance, restored, before string) swapState {
	// 呼ばれるのは失敗の後で、元の ctx は取り消されていることがある。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := r.connect(ctx, maintenanceDB(maintenance))
	if err != nil {
		return swapUnknown
	}
	defer admin.Close(context.Background())
	rows, err := admin.Query(ctx, `SELECT datname FROM pg_database WHERE datname = ANY($1)`, []string{restored, before})
	if err != nil {
		return swapUnknown
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return swapUnknown
	}
	has := map[string]bool{}
	for _, n := range names {
		has[n] = true
	}
	switch {
	case has[before] && !has[restored]:
		return swapDone
	case has[restored] && !has[before]:
		return swapNotDone
	}
	return swapUnknown
}

func (r *Restorer) finish(ctx context.Context, plan *RestorePlan, opts RestoreOptions, target, before string, res *RestoreResult) error {
	if err := r.VerifyRestored(ctx, target, plan.Meta); err != nil {
		return err
	}
	settings, err := r.ApplyDatabaseSettings(ctx, target, plan.Meta)
	if err != nil {
		return err
	}
	res.Settings = settings
	if res.Migrations, err = r.Migrate(target); err != nil {
		return err
	}
	if opts.Mode != RestoreSwap {
		return nil
	}
	if err := r.SwitchDatabase(ctx, opts.MaintenanceDB, opts.Database, before, target); err != nil {
		return err
	}
	res.BeforeRestore = before
	return nil
}

// SwitchDatabase is ReplaceDatabase for the swap mode and for a rollback:
// when the rename returns an error, it reads pg_database again. If the
// rename happened anyway, it logs a warning and returns nil; if it cannot
// tell, it returns ErrRestoreSwitchUnknown.
//
// 名前の入れ替えは、サーバーの側で COMMIT が通った後に、取り消しや接続断で
// エラーとして返ることがある。そのまま「変わっていない」と扱うと、実際には
// 入れ替わっているのに Redis の後始末を飛ばしてしまう。
func (r *Restorer) SwitchDatabase(ctx context.Context, maintenance, database, keepAs, replacement string) error {
	err := r.ReplaceDatabase(ctx, maintenance, database, keepAs, replacement)
	if r.afterReplace != nil {
		err = errors.Join(err, r.afterReplace())
	}
	if err == nil {
		return nil
	}
	switch r.swapOutcome(maintenance, replacement, keepAs) {
	case swapDone:
		r.logf("warning: %v; but %s was already replaced (the previous database is %s)", err, database, keepAs)
		return nil
	case swapNotDone:
		return err
	}
	return fmt.Errorf("%w: %v; check pg_database for %s and %s before starting the server",
		ErrRestoreSwitchUnknown, err, replacement, keepAs)
}

func checkConfirm(opts RestoreOptions) error {
	if opts.Database == "" {
		return errors.New("backup: no database to restore into")
	}
	// **戻す先の名前を、打った人に明示させる。** 設定ファイルを取り違えたまま
	// 叩くと、別のインスタンスの DB を入れ替えてしまう。
	if opts.Confirm != opts.Database {
		return fmt.Errorf("%w: pass -confirm %s to restore into %q", ErrRestoreNotConfirmed, opts.Database, opts.Database)
	}
	return nil
}

// Load resolves id (a generation ID or LatestGeneration), reads its
// meta.json and stops when the backup has migrations this binary does not
// know (ErrRestoreMigrationTooNew).
func (r *Restorer) Load(ctx context.Context, id string) (*RestorePlan, error) {
	id, verified, err := r.resolveID(ctx, id)
	if err != nil {
		return nil, err
	}
	meta, err := r.readMeta(ctx, id)
	if err != nil {
		return nil, err
	}
	if !verified {
		r.logf("warning: generation %s has no passing verification (run `elythia backup verify` first)", id)
	}
	if err := r.checkMigrations(meta); err != nil {
		return nil, err
	}
	return &RestorePlan{ID: id, Verified: verified, Meta: meta}, nil
}

// RestoreNames returns the database names a same-server restore at t uses:
// restored is where the backup goes first, and before is the name the
// current database is kept under.
func RestoreNames(database string, t time.Time) (restored, before string, err error) {
	suffix := t.UTC().Format("20060102150405")
	restored = database + "_restore_" + suffix
	before = database + "_before_restore_" + suffix
	if len(before) > maxIdentifierLen {
		return "", "", fmt.Errorf("backup: %q is longer than %d bytes; the database name is too long for a same-server restore", before, maxIdentifierLen)
	}
	return restored, before, nil
}

func (r *Restorer) connect(ctx context.Context, dbname string) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, r.Conn.SQL(dbname))
	if err != nil {
		return nil, fmt.Errorf("backup: connect to %q: %w", dbname, err)
	}
	return conn, nil
}

func maintenanceDB(name string) string {
	if name == "" {
		return "postgres"
	}
	return name
}

// CheckTarget runs the stop conditions that do not need the dump: the
// confirmation, open connections on the target (unless
// SkipConnectionCheck), and for the swap mode the privileges and free
// names, for the empty mode that the target has no tables.
func (r *Restorer) CheckTarget(ctx context.Context, opts RestoreOptions) error {
	if err := checkConfirm(opts); err != nil {
		return err
	}
	if opts.Mode == RestoreEmpty {
		return r.checkEmptyTarget(ctx, opts)
	}
	admin, err := r.connect(ctx, maintenanceDB(opts.MaintenanceDB))
	if err != nil {
		return err
	}
	defer admin.Close(context.Background())
	if err := r.checkToolVersion(ctx, admin); err != nil {
		return err
	}
	db, err := databaseInfo(ctx, admin, opts.Database)
	if err != nil {
		return err
	}
	if err := checkSwapPrivilege(ctx, admin, opts.Database, db); err != nil {
		return err
	}
	if !opts.SkipConnectionCheck {
		if err := checkNoConnections(ctx, admin, opts.Database); err != nil {
			return err
		}
	}
	at := opts.At
	if at.IsZero() {
		at = r.now()
	}
	restored, before, err := RestoreNames(opts.Database, at)
	if err != nil {
		return err
	}
	for _, name := range []string{restored, before} {
		if _, err := databaseInfo(ctx, admin, name); err == nil {
			return fmt.Errorf("backup: database %q already exists", name)
		}
	}
	return nil
}

func (r *Restorer) checkEmptyTarget(ctx context.Context, opts RestoreOptions) error {
	conn, err := r.connect(ctx, opts.Database)
	if err != nil {
		return fmt.Errorf("%w (create the empty database first)", err)
	}
	defer conn.Close(context.Background())
	if err := r.checkToolVersion(ctx, conn); err != nil {
		return err
	}
	if !opts.SkipConnectionCheck {
		if err := checkNoConnections(ctx, conn, opts.Database); err != nil {
			return err
		}
	}
	// PostgreSQL 15 からは public schema の持ち主が pg_database_owner になり、
	// PUBLIC は CREATE できない。管理者が自分の持ち物として作った DB へ、アプリの
	// ユーザーで pg_restore を流すと権限エラーで落ちるので、先に止める。
	var canCreate bool
	err = conn.QueryRow(ctx, `
		SELECT NOT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'public')
		    OR has_schema_privilege(current_user, 'public', 'CREATE')`).Scan(&canCreate)
	if err != nil {
		return fmt.Errorf("backup: inspect %q: %w", opts.Database, err)
	}
	if !canCreate {
		return fmt.Errorf("%w: the database user cannot create tables in %q; create it with CREATE DATABASE %s OWNER <database user>",
			ErrRestoreNoPrivilege, opts.Database, ident(opts.Database))
	}
	var n int
	err = conn.QueryRow(ctx, `
		SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f')
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp%'`).Scan(&n)
	if err != nil {
		return fmt.Errorf("backup: inspect %q: %w", opts.Database, err)
	}
	if n > 0 {
		return fmt.Errorf("%w: %q has %d relations; restore into an empty database, or use -mode swap", ErrRestoreNotEmpty, opts.Database, n)
	}
	return nil
}

// CheckCompatibility runs the stop conditions that need the backup's
// metadata: every extension the backup uses (Meta.Extensions) must be
// available on the target server (ErrRestoreExtension), and in the empty
// mode the target database must have the encoding and locale recorded in
// the backup (ErrRestoreLocale). The swap mode creates its database with the
// recorded values, so it only checks the extensions.
//
// 拡張が無いと、pg_restore は CREATE EXTENSION で落ちる。入れ替える形では DB を
// 作って dump を流した後になるので、何かを作る前に止める。
func (r *Restorer) CheckCompatibility(ctx context.Context, plan *RestorePlan, opts RestoreOptions) error {
	dbname := maintenanceDB(opts.MaintenanceDB)
	if opts.Mode == RestoreEmpty {
		dbname = opts.Database
	}
	conn, err := r.connect(ctx, dbname)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if err := checkTargetExtensions(ctx, conn, plan.Meta.Extensions); err != nil {
		return err
	}
	if opts.Mode != RestoreEmpty || plan.Meta.DatabaseLocale.Encoding == "" {
		return nil
	}
	target, err := databaseInfo(ctx, conn, opts.Database)
	if err != nil {
		return err
	}
	want := normalizeLocale(plan.Meta.DatabaseLocale)
	if got := normalizeLocale(target.locale); got != want {
		// 照合順序が違うと、戻した後の並び順や一意制約の判定が変わる。encoding が
		// 違うと、文字が化けるか pg_restore が落ちる。どちらも黙って進めない。
		return fmt.Errorf("%w: %q has %s, the backup was taken from %s; create the database again with: %s OWNER <database user>",
			ErrRestoreLocale, opts.Database, describeLocale(got), describeLocale(want), createDatabaseSQL(opts.Database, want, ""))
	}
	return nil
}

// swapLocale returns the encoding and locale the swap mode creates the
// restored database with: the ones recorded in the backup, or the current
// database's when the backup has none.
//
// 戻すのはバックアップを取った時点の DB なので、作り直すときもその時点の値を使う。
// 今の DB と違うとき (取った後に作り直した、など) は、気付けるよう出力に残す。
func (r *Restorer) swapLocale(meta Meta, database string, current restoreDatabase) DatabaseLocale {
	if meta.DatabaseLocale.Encoding == "" {
		return current.locale
	}
	want := meta.DatabaseLocale
	if normalizeLocale(want) != normalizeLocale(current.locale) {
		r.logf("note: the backup has %s, the current database %s has %s; the restored database uses the backup's",
			describeLocale(normalizeLocale(want)), database, describeLocale(normalizeLocale(current.locale)))
	}
	return want
}

// normalizeLocale fills in the provider of servers older than PostgreSQL 15,
// which only had libc.
func normalizeLocale(l DatabaseLocale) DatabaseLocale {
	if l.Provider == "" {
		l.Provider = "libc"
	}
	return l
}

func describeLocale(l DatabaseLocale) string {
	s := fmt.Sprintf("ENCODING %s LC_COLLATE %s LC_CTYPE %s LOCALE_PROVIDER %s", l.Encoding, l.Collate, l.Ctype, l.Provider)
	if l.Locale != "" {
		s += " LOCALE " + l.Locale
	}
	return s
}

// checkTargetExtensions stops (ErrRestoreExtension) when an extension in
// exts is not available on the server q is connected to.
func checkTargetExtensions(ctx context.Context, q Queryer, exts []ExtensionInfo) error {
	missing, err := missingExtensions(ctx, q, exts)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s; install them on the target server first (pg_bigm: deploy/postgres-bigm)",
			ErrRestoreExtension, strings.Join(missing, ", "))
	}
	return nil
}

// checkToolVersion stops (ErrRestoreVersion) when pg_restore is a newer
// major version than the server it restores into.
//
// 新しい pg_restore は、古いサーバーが知らない設定を流す (18 の pg_restore は 16 の
// サーバーへ `SET transaction_timeout` を送って落ちる)。古い pg_restore は新しい
// pg_dump の形式を読めない。バックアップ用の image の pg_restore は 1 つの版
// なので、戻す先のサーバーがそれより古ければ、何かを作る前に止める。
func (r *Restorer) checkToolVersion(ctx context.Context, conn *pgx.Conn) error {
	var serverNum int
	if err := conn.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverNum); err != nil {
		return fmt.Errorf("backup: read server_version_num: %w", err)
	}
	var out bytes.Buffer
	if err := r.exec()(ctx, r.pgRestoreProgram(), []string{"--version"}, nil, nil, &out, io.Discard); err != nil {
		return fmt.Errorf("backup: run %s --version: %w", r.pgRestoreProgram(), err)
	}
	tool, err := parseToolMajor(out.String())
	if err != nil {
		return err
	}
	if server := serverNum / 10000; tool > server {
		return fmt.Errorf("%w: pg_restore is %d but the server is %d; restore into a PostgreSQL %d or newer server (see docs/deployment.md \"PostgreSQL 16 → 18 への移行\")",
			ErrRestoreVersion, tool, server, tool)
	}
	return nil
}

// parseToolMajor reads the major version from "pg_restore (PostgreSQL) 18.0"
// (also "18.1 (Debian 18.1-1)" and "19devel"): the leading digits of the
// first field that starts with a digit.
func parseToolMajor(s string) (int, error) {
	for _, f := range strings.Fields(s) {
		end := 0
		for end < len(f) && f[end] >= '0' && f[end] <= '9' {
			end++
		}
		if end == 0 {
			continue
		}
		if n, err := strconv.Atoi(f[:end]); err == nil && n > 0 {
			return n, nil
		}
		break
	}
	return 0, fmt.Errorf("backup: cannot read the pg_restore version from %q", strings.TrimSpace(s))
}

func (r *Restorer) exec() RestoreExecFunc {
	if r.Exec == nil {
		return ExecLocal
	}
	return r.Exec
}

func (r *Restorer) pgRestoreProgram() string {
	if r.PgRestore == "" {
		return "pg_restore"
	}
	return r.PgRestore
}

// CheckNoConnections stops (ErrRestoreConnections) when a session other
// than the caller's is connected to database. It connects to maintenance
// (empty means "postgres").
func (r *Restorer) CheckNoConnections(ctx context.Context, maintenance, database string) error {
	admin, err := r.connect(ctx, maintenanceDB(maintenance))
	if err != nil {
		return err
	}
	defer admin.Close(context.Background())
	return checkNoConnections(ctx, admin, database)
}

// RestoreInto downloads and checks the dump, then runs pg_restore into
// dbname. In the swap mode it first creates dbname with the encoding and
// locale recorded in the backup (Meta.DatabaseLocale) and the owner of
// opts.Database, and drops it again when pg_restore fails. In the empty mode dbname must be opts.Database.
func (r *Restorer) RestoreInto(ctx context.Context, plan *RestorePlan, opts RestoreOptions, dbname string) error {
	if opts.Mode == RestoreEmpty && dbname != opts.Database {
		return fmt.Errorf("backup: the empty mode restores into %q, not %q", opts.Database, dbname)
	}
	// 作る前に dump を確かめる。食い違っていたら、DB を作らずに止まる。
	dump, cleanup, err := r.fetchDump(ctx, plan.Meta)
	if err != nil {
		return err
	}
	defer cleanup()
	if opts.Mode == RestoreEmpty {
		return r.pgRestore(ctx, dbname, dump)
	}
	admin, err := r.connect(ctx, maintenanceDB(opts.MaintenanceDB))
	if err != nil {
		return err
	}
	defer admin.Close(context.Background())
	db, err := databaseInfo(ctx, admin, opts.Database)
	if err != nil {
		return err
	}
	loc := r.swapLocale(plan.Meta, opts.Database, db)
	r.logf("creating database %s", dbname)
	if _, err := admin.Exec(ctx, createDatabaseSQL(dbname, loc, db.owner)); err != nil {
		return fmt.Errorf("backup: create database %q: %w", dbname, err)
	}
	if err := r.pgRestore(ctx, dbname, dump); err != nil {
		if _, derr := admin.Exec(context.Background(), dropDatabaseSQL(dbname)); derr != nil {
			r.logf("warning: could not drop %s: %v", dbname, derr)
		}
		return err
	}
	return nil
}

// VerifyRestored compares the row counts of dbname with the backup
// (ErrRestoreRowMismatch). Run it before Migrate, which may add or remove
// rows.
func (r *Restorer) VerifyRestored(ctx context.Context, dbname string, meta Meta) error {
	conn, err := r.connect(ctx, dbname)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	if err := checkRowCounts(ctx, conn, meta.RowCounts); err != nil {
		return err
	}
	r.logf("row counts match (%d tables)", len(meta.RowCounts))
	return nil
}

// ApplyDatabaseSettings applies the per-database settings recorded in the
// backup (ALTER DATABASE ... SET, which pg_dump does not include) to
// dbname, and returns the applied ones.
//
// 入れ替える形では、名前を変える前の DB に当てる。設定は DB の OID に付くので、
// 名前を変えても付いてくる。
func (r *Restorer) ApplyDatabaseSettings(ctx context.Context, dbname string, meta Meta) ([]string, error) {
	conn, err := r.connect(ctx, dbname)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	var applied []string
	for _, s := range meta.DatabaseSettings {
		stmt, err := alterDatabaseSetSQL(dbname, s)
		if err != nil {
			return applied, err
		}
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return applied, fmt.Errorf("backup: apply database setting %q: %w", s, err)
		}
		applied = append(applied, s)
	}
	return applied, nil
}

// ReplaceDatabase renames database to keepAs and replacement to database,
// in one transaction, after checking that nobody is connected to database.
// It connects to maintenance (empty means "postgres").
//
// Swapping in a restored database is ReplaceDatabase(db, before, restored);
// undoing it is ReplaceDatabase(db, <some new name>, before).
func (r *Restorer) ReplaceDatabase(ctx context.Context, maintenance, database, keepAs, replacement string) error {
	admin, err := r.connect(ctx, maintenanceDB(maintenance))
	if err != nil {
		return err
	}
	defer admin.Close(context.Background())
	if err := checkNoConnections(ctx, admin, database); err != nil {
		return err
	}
	// 名前の入れ替えは 1 つのトランザクションで行う。どちらかが落ちたら (今の DB に
	// 接続が戻ってきたなど)、両方とも元の名前のまま残る。
	r.logf("renaming %s to %s and %s to %s", database, keepAs, replacement, database)
	err = pgx.BeginFunc(ctx, admin, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "ALTER DATABASE "+ident(database)+" RENAME TO "+ident(keepAs)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "ALTER DATABASE "+ident(replacement)+" RENAME TO "+ident(database))
		return err
	})
	if err != nil {
		return fmt.Errorf("backup: swap database names: %w", err)
	}
	return nil
}

// DropDatabase drops name, disconnecting its sessions. It connects to
// maintenance (empty means "postgres"). A missing database is not an error.
func (r *Restorer) DropDatabase(ctx context.Context, maintenance, name string) error {
	admin, err := r.connect(ctx, maintenanceDB(maintenance))
	if err != nil {
		return err
	}
	defer admin.Close(context.Background())
	if _, err := admin.Exec(ctx, dropDatabaseSQL(name)); err != nil {
		return fmt.Errorf("backup: drop database %q: %w", name, err)
	}
	return nil
}

func dropDatabaseSQL(name string) string {
	return "DROP DATABASE IF EXISTS " + ident(name) + " WITH (FORCE)"
}

// ResolveID returns the generation id (a generation ID or LatestGeneration)
// selects and whether it was verified, the way Load does.
func (r *Restorer) ResolveID(ctx context.Context, id string) (string, bool, error) {
	return r.resolveID(ctx, id)
}

// NewestGeneration returns the newest generation in the storage that has a
// meta.json, whether or not it can be read ("" when there is none).
// Generations without meta.json (interrupted takes) are ignored.
//
// 読めない meta.json の世代も数える。読めないだけで中身が新しいかもしれず、
// 「戻す世代が最新」と言い切れないため。
func NewestGeneration(ctx context.Context, st Storage) (string, error) {
	gens, err := ListGenerations(ctx, st)
	if err != nil {
		return "", fmt.Errorf("backup: list generations: %w", err)
	}
	for i := len(gens) - 1; i >= 0; i-- {
		if g := gens[i]; g.Complete() || g.MetaError != nil {
			return g.ID, nil
		}
	}
	return "", nil
}

// resolveID returns the generation to restore and whether it was verified.
func (r *Restorer) resolveID(ctx context.Context, id string) (string, bool, error) {
	if id == LatestGeneration {
		return r.latestVerified(ctx)
	}
	if !ValidID(id) {
		return "", false, fmt.Errorf("backup: invalid generation id %q", id)
	}
	vr, err := ReadVerify(ctx, r.Storage, id)
	if errors.Is(err, ErrNotFound) {
		return id, false, nil
	}
	if err != nil {
		return "", false, err
	}
	return id, verifyPassed(id, vr), nil
}

// verifyPassed reports whether vr is a passing verification of id.
//
// 別の世代の結果を置き間違えたものを、検証済みとして扱わない。
func verifyPassed(id string, vr *VerifyResult) bool {
	return vr != nil && vr.OK && vr.ID == id
}

// latestVerified returns the newest complete generation (Generation.Complete)
// with a passing verify.json.
//
// 「最新」に検証していない世代を選ばない。取っている途中で落ちた世代や、検証で
// 食い違った世代へ黙って戻すと、戻した後で初めて壊れていると分かる。
func (r *Restorer) latestVerified(ctx context.Context) (string, bool, error) {
	gens, err := ListGenerations(ctx, r.Storage)
	if err != nil {
		return "", false, fmt.Errorf("backup: list generations: %w", err)
	}
	for i := len(gens) - 1; i >= 0; i-- {
		g := gens[i]
		// 読めなかった世代を飛ばして古い世代へ進むと、一時的な読み込みの失敗で
		// 意図より古い時点へ戻してしまう。読めないものがあれば止めて、ID の明示を求める。
		if g.MetaError != nil {
			return "", false, fmt.Errorf("backup: cannot read generation %s (%w); pass a generation id explicitly", g.ID, g.MetaError)
		}
		if !g.Complete() {
			continue
		}
		if g.Verify == nil && hasObject(g, Key(g.ID, VerifyFile)) {
			return "", false, fmt.Errorf("backup: cannot read %s; pass a generation id explicitly", Key(g.ID, VerifyFile))
		}
		if verifyPassed(g.ID, g.Verify) {
			return g.ID, true, nil
		}
	}
	return "", false, errors.New("backup: no verified generation; run `elythia backup verify` or pass a generation id explicitly")
}

func hasObject(g Generation, key string) bool {
	for _, o := range g.Objects {
		if o.Key == key {
			return true
		}
	}
	return false
}

// readMeta reads meta.json of id (ReadMeta) and accepts only the dump names
// Take writes.
func (r *Restorer) readMeta(ctx context.Context, id string) (Meta, error) {
	m, err := ReadMeta(ctx, r.Storage, id)
	if errors.Is(err, ErrNotFound) {
		return Meta{}, fmt.Errorf("backup: generation %s has no %s (incomplete or missing)", id, MetaFile)
	}
	if err != nil {
		return Meta{}, err
	}
	// 保存先の中身から鍵を組み立てるので、決まった名前以外は受けない。
	if m.DumpFile != DumpFile && m.DumpFile != DumpFileAge {
		return Meta{}, fmt.Errorf("backup: %s has unexpected dumpFile %q", Key(id, MetaFile), m.DumpFile)
	}
	return *m, nil
}

// checkMigrations stops when the backup has migrations the bundled ones do
// not reach.
//
// **バイナリより新しい版で取ったバックアップは戻さない。** 管理表の番号が同梱の
// 最新より進んでいると、戻した後に `elythia migrate` も本体の起動も通らない
// (golang-migrate は知らない番号で止まる)。戻す前に、取った版以上のバイナリへ
// 上げてもらう。dirty の管理表も、migration の途中で取ったものなので戻さない。
func (r *Restorer) checkMigrations(m Meta) error {
	core, _, err := latestMigration(r.CoreMigrationsDir)
	if err != nil {
		return err
	}
	if core == 0 {
		return fmt.Errorf("backup: no migrations found in %q (run from the directory that has migration/)", r.CoreMigrationsDir)
	}
	local, _, err := latestMigration(r.LocalMigrationsDir)
	if err != nil {
		return err
	}
	for _, s := range m.Migrations {
		if s.Missing {
			continue
		}
		var bundled uint
		switch s.Table {
		case CoreMigrationsTable:
			bundled = core
		case LocalMigrationsTable:
			bundled = local
		default:
			return fmt.Errorf("backup: meta.json has an unknown migration table %q", s.Table)
		}
		if s.Dirty {
			return fmt.Errorf("backup: %s was dirty (version %d) when the backup was taken", s.Table, s.Version)
		}
		// 管理表はあるが行が無い (NilMigrationVersion) のは、migration を 1 つも当てて
		// いない DB。戻した後の Migrate で全部を当てるので、止めない。
		if s.Version == NilMigrationVersion {
			continue
		}
		if s.Version < 0 {
			return fmt.Errorf("backup: meta.json has an invalid version %d for %s", s.Version, s.Table)
		}
		if uint(s.Version) > bundled {
			return fmt.Errorf("%w: %s is at %d in the backup (taken by Elythia %s) but this binary ships up to %d; use a binary at least as new as the backup",
				ErrRestoreMigrationTooNew, s.Table, s.Version, m.ElythiaVersion, bundled)
		}
	}
	return nil
}

// latestMigration returns the highest up migration version in dir and the
// number of up migrations. A missing directory has none.
//
// internal/cli/migrate.LatestVersion と同じ数え方 (golang-migrate の名前の規則)。
// backup から cli を import しないために持つ。
func latestMigration(dir string) (latest uint, count int, err error) {
	if dir == "" {
		return 0, 0, nil
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("backup: read %s: %w", dir, err)
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		m, perr := source.Parse(ent.Name())
		if perr != nil || m.Direction != source.Up {
			continue
		}
		latest = max(latest, m.Version)
		count++
	}
	return latest, count, nil
}

func (r *Restorer) pgRestore(ctx context.Context, dbname, dump string) error {
	f, err := os.Open(dump)
	if err != nil {
		return fmt.Errorf("backup: open dump: %w", err)
	}
	defer f.Close()
	conninfo, env := r.Conn.Tool(dbname)
	program, run := r.pgRestoreProgram(), r.exec()
	// --single-transaction と --exit-on-error で、途中で落ちたら何も残さない。
	// --no-owner / --no-privileges は、取った側と戻す側で role の名前が違っても
	// 戻せるようにするため (Elythia は 1 つの role で動き、GRANT を使わない)。
	// dump は標準入力で渡す。pg_restore をコンテナの中で動かす構成でも、ファイルを
	// コンテナへ見せずに済む。
	args := []string{"--exit-on-error", "--single-transaction", "--no-owner", "--no-privileges", "--dbname", conninfo}
	var stderr bytes.Buffer
	r.logf("running pg_restore into %s", dbname)
	if err := run(ctx, program, args, env, f, io.Discard, &limitedWriter{w: &stderr, n: 64 << 10}); err != nil {
		return fmt.Errorf("backup: pg_restore: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// limitedWriter keeps the first n bytes and discards the rest.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}

// fetchDump downloads the dump to a temporary file, decrypting it when
// needed, and checks the size and hashes in meta. The caller must call
// cleanup.
//
// **書き込む前に全部を確かめる。** pg_restore へ流しながら確かめると、途中で
// 切れた・書き換わった dump を、食い違いに気付く前に半分戻してしまう。
func (r *Restorer) fetchDump(ctx context.Context, meta Meta) (path string, cleanup func(), err error) {
	if meta.Encrypted && meta.Encryption != "age" {
		return "", nil, fmt.Errorf("backup: unknown encryption %q", meta.Encryption)
	}
	if meta.Encrypted && len(r.Identities) == 0 {
		return "", nil, errors.New("backup: the dump is encrypted; set backup.encryption.identityFile")
	}
	rc, err := r.Storage.Get(ctx, Key(meta.ID, meta.DumpFile))
	if err != nil {
		return "", nil, fmt.Errorf("backup: open %s: %w", Key(meta.ID, meta.DumpFile), err)
	}
	defer rc.Close()
	f, err := os.CreateTemp(r.TempDir, "elythia-restore-*.pgc")
	if err != nil {
		return "", nil, fmt.Errorf("backup: create temporary file: %w", err)
	}
	cleanup = func() { _ = os.Remove(f.Name()) }
	fail := func(err error) (string, func(), error) {
		_ = f.Close()
		cleanup()
		return "", nil, err
	}

	stored := sha256.New()
	counter := &countingWriter{}
	raw := io.TeeReader(rc, io.MultiWriter(stored, counter))
	plainSrc := raw
	if meta.Encrypted {
		plainSrc, err = age.Decrypt(raw, r.Identities...)
		if err != nil {
			return fail(fmt.Errorf("backup: decrypt the dump: %w", err))
		}
	}
	plain := sha256.New()
	r.logf("downloading %s", Key(meta.ID, meta.DumpFile))
	if _, err := io.Copy(io.MultiWriter(f, plain), plainSrc); err != nil {
		return fail(fmt.Errorf("backup: read the dump: %w", err))
	}
	// 復号が終わった後ろに余計なバイトがあれば、それも数えて食い違いにする。
	if _, err := io.Copy(io.Discard, raw); err != nil {
		return fail(fmt.Errorf("backup: read the dump: %w", err))
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("backup: write temporary file: %w", err)
	}
	if counter.n != meta.DumpSize {
		cleanup()
		return "", nil, fmt.Errorf("%w: size %d, meta.json says %d", ErrRestoreChecksum, counter.n, meta.DumpSize)
	}
	if got := hex.EncodeToString(stored.Sum(nil)); got != meta.DumpSHA256 {
		cleanup()
		return "", nil, fmt.Errorf("%w: sha256 %s, meta.json says %s", ErrRestoreChecksum, got, meta.DumpSHA256)
	}
	if meta.PlainSHA256 != "" {
		if got := hex.EncodeToString(plain.Sum(nil)); got != meta.PlainSHA256 {
			cleanup()
			return "", nil, fmt.Errorf("%w: plain sha256 %s, meta.json says %s", ErrRestoreChecksum, got, meta.PlainSHA256)
		}
	}
	return f.Name(), cleanup, nil
}

type countingWriter struct{ n int64 }

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// Migrate applies the bundled migrations to dbname, core first, and
// returns the state of the bookkeeping tables.
//
// 戻したバックアップがバイナリより古い版で取ったものなら、ここで追いつく。
// 本体の起動を待たずに当てるのは、落ちたときに今の DB と入れ替える前に止めるため。
func (r *Restorer) Migrate(dbname string) ([]MigrationState, error) {
	type track struct{ table, dir string }
	tracks := []track{{CoreMigrationsTable, r.CoreMigrationsDir}}
	if _, n, err := latestMigration(r.LocalMigrationsDir); err != nil {
		return nil, err
	} else if n > 0 {
		tracks = append(tracks, track{LocalMigrationsTable, r.LocalMigrationsDir})
	}
	var states []MigrationState
	for _, t := range tracks {
		dbURL := r.Conn.Migrate(dbname)
		if t.table != CoreMigrationsTable {
			sep := "?"
			if strings.Contains(dbURL, "?") {
				sep = "&"
			}
			dbURL += sep + "x-migrations-table=" + t.table
		}
		m, err := gomigrate.New("file://"+t.dir, dbURL)
		if err != nil {
			// URL にはパスワードが入るので、err の文面に URL が含まれないものだけを返す。
			return nil, fmt.Errorf("backup: open migrations for %s: %w", t.table, redactURL(err, dbURL))
		}
		err = m.Up()
		v, dirty, verr := m.Version()
		_, _ = m.Close()
		if err != nil && !errors.Is(err, gomigrate.ErrNoChange) {
			return nil, fmt.Errorf("backup: migrate %s: %w", t.table, err)
		}
		if verr != nil {
			return nil, fmt.Errorf("backup: read %s: %w", t.table, verr)
		}
		r.logf("%s is at version %d", t.table, v)
		states = append(states, MigrationState{Table: t.table, Version: int64(v), Dirty: dirty})
	}
	return states, nil
}

func redactURL(err error, dbURL string) error {
	return errors.New(strings.ReplaceAll(err.Error(), dbURL, "<database url>"))
}

// restoreDatabase is what the restore reads about an existing database.
type restoreDatabase struct {
	locale             DatabaseLocale
	owner              string
	ownerIsCurrentUser bool
}

func databaseInfo(ctx context.Context, conn *pgx.Conn, name string) (restoreDatabase, error) {
	var d restoreDatabase
	var provider string
	// datlocprovider は 15 から、datlocale は 17 から (16 までは daticulocale)。
	// 版ごとに列を選ばずに済むよう、行を jsonb にして名前で引く。
	err := conn.QueryRow(ctx, `
		SELECT pg_encoding_to_char(d.encoding), d.datcollate, d.datctype,
		       coalesce(to_jsonb(d) ->> 'datlocprovider', ''),
		       coalesce(to_jsonb(d) ->> 'datlocale', to_jsonb(d) ->> 'daticulocale', ''),
		       r.rolname, r.rolname = current_user
		FROM pg_database d JOIN pg_roles r ON r.oid = d.datdba
		WHERE d.datname = $1`, name).
		Scan(&d.locale.Encoding, &d.locale.Collate, &d.locale.Ctype, &provider, &d.locale.Locale, &d.owner, &d.ownerIsCurrentUser)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, fmt.Errorf("backup: database %q does not exist", name)
	}
	if err != nil {
		return d, fmt.Errorf("backup: inspect database %q: %w", name, err)
	}
	d.locale.Provider = localeProviders[provider]
	return d, nil
}

// checkSwapPrivilege stops when the current user cannot create the new
// database or rename the current one.
//
// 名前を変えるには、その DB の持ち主であることと CREATEDB の両方が要る
// (superuser はどちらも要らない)。途中で権限が足りずに止まると、作った DB を
// 片付ける手間だけが残るので、何かを作る前に確かめる。
func checkSwapPrivilege(ctx context.Context, conn *pgx.Conn, name string, db restoreDatabase) error {
	var super, createdb bool
	if err := conn.QueryRow(ctx, `SELECT rolsuper, rolcreatedb FROM pg_roles WHERE rolname = current_user`).
		Scan(&super, &createdb); err != nil {
		return fmt.Errorf("backup: read role attributes: %w", err)
	}
	if super || (createdb && db.ownerIsCurrentUser) {
		return nil
	}
	var missing []string
	if !createdb {
		missing = append(missing, "CREATEDB")
	}
	if !db.ownerIsCurrentUser {
		missing = append(missing, fmt.Sprintf("ownership of %q (owned by %s)", name, db.owner))
	}
	return fmt.Errorf("%w: the database user lacks %s; grant them (see docs/deployment.md \"バックアップから戻す\"), or restore with -mode empty into a database an administrator created with OWNER <database user>",
		ErrRestoreNoPrivilege, strings.Join(missing, " and "))
}

// checkNoConnections stops when another session is connected to name.
//
// **本体や worker が繋いだまま戻すと、戻した後も古い接続のまま書き込みが続く。**
// 入れ替える形では名前の変更が接続のある DB を拒否するが、それは pg_restore と
// 突き合わせを終えた後になる。空の DB へ戻す形では、何も止めない。
func checkNoConnections(ctx context.Context, conn *pgx.Conn, name string) error {
	rows, err := conn.Query(ctx, `
		SELECT pid, coalesce(usename, ''), coalesce(application_name, ''), coalesce(host(client_addr), 'local')
		FROM pg_stat_activity
		WHERE datname = $1 AND pid <> pg_backend_pid() AND backend_type = 'client backend'
		ORDER BY pid`, name)
	if err != nil {
		return fmt.Errorf("backup: read pg_stat_activity: %w", err)
	}
	var sessions []string
	for rows.Next() {
		var pid int
		var user, app, addr string
		if err := rows.Scan(&pid, &user, &app, &addr); err != nil {
			rows.Close()
			return fmt.Errorf("backup: read pg_stat_activity: %w", err)
		}
		sessions = append(sessions, fmt.Sprintf("pid=%d user=%s application=%q from=%s", pid, user, app, addr))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("backup: read pg_stat_activity: %w", err)
	}
	if len(sessions) > 0 {
		return fmt.Errorf("%w: stop the server and workers first (%d sessions on %q: %s)",
			ErrRestoreConnections, len(sessions), name, strings.Join(sessions, "; "))
	}
	return nil
}

func createDatabaseSQL(name string, loc DatabaseLocale, owner string) string {
	var b strings.Builder
	b.WriteString("CREATE DATABASE " + ident(name) + " WITH TEMPLATE template0")
	b.WriteString(" ENCODING " + literal(loc.Encoding))
	b.WriteString(" LC_COLLATE " + literal(loc.Collate) + " LC_CTYPE " + literal(loc.Ctype))
	switch loc.Provider {
	case "icu":
		b.WriteString(" LOCALE_PROVIDER icu")
		if loc.Locale != "" {
			b.WriteString(" ICU_LOCALE " + literal(loc.Locale))
		}
	case "builtin":
		b.WriteString(" LOCALE_PROVIDER builtin")
		if loc.Locale != "" {
			b.WriteString(" BUILTIN_LOCALE " + literal(loc.Locale))
		}
	case "libc":
		b.WriteString(" LOCALE_PROVIDER libc")
	}
	if owner != "" {
		b.WriteString(" OWNER " + ident(owner))
	}
	return b.String()
}

// checkRowCounts counts the rows of the restored database the same way Take
// does (CountRows) and compares them with want (compareRowCounts, shared with
// verify).
func checkRowCounts(ctx context.Context, conn *pgx.Conn, want map[string]int64) error {
	got, err := CountRows(ctx, conn)
	if err != nil {
		return err
	}
	var mismatches []string
	for _, m := range compareRowCounts(want, got) {
		mismatches = append(mismatches, fmt.Sprintf("%s: %s, backup has %s", m.Table, rowsLabel(m.Actual), rowsLabel(m.Expected)))
	}
	if len(mismatches) > 0 {
		return fmt.Errorf("%w: %s", ErrRestoreRowMismatch, strings.Join(mismatches, "; "))
	}
	return nil
}

func rowsLabel(n int64) string {
	if n < 0 {
		return "no table"
	}
	return fmt.Sprintf("%d rows", n)
}

// listQuotedSettings are the settings whose value is a list of quoted
// elements (GUC_LIST_QUOTE in PostgreSQL). pg_dumpall quotes each element
// separately for them; quoting the whole value would make one element.
var listQuotedSettings = map[string]bool{
	"search_path":               true,
	"temp_tablespaces":          true,
	"session_preload_libraries": true,
	"local_preload_libraries":   true,
	"shared_preload_libraries":  true,
	"unix_socket_directories":   true,
}

// alterDatabaseSetSQL renders "name=value" (pg_db_role_setting.setconfig)
// as ALTER DATABASE ... SET.
func alterDatabaseSetSQL(dbname, setting string) (string, error) {
	name, value, ok := strings.Cut(setting, "=")
	if !ok || !validSettingName(name) {
		return "", fmt.Errorf("backup: invalid database setting %q", setting)
	}
	if strings.ContainsRune(value, 0) {
		return "", fmt.Errorf("backup: invalid database setting %q", setting)
	}
	values := []string{value}
	if listQuotedSettings[strings.ToLower(name)] {
		values = splitGUCList(value)
	}
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = literal(v)
	}
	return "ALTER DATABASE " + ident(dbname) + " SET " + name + " TO " + strings.Join(quoted, ", "), nil
}

func validSettingName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		switch {
		case c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		case i > 0 && (c == '.' || (c >= '0' && c <= '9')):
		default:
			return false
		}
	}
	return true
}

// splitGUCList splits a list setting the way PostgreSQL's SplitGUCList
// does: elements are separated by commas, and double-quoted elements may
// contain commas and doubled quotes.
func splitGUCList(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inQuote && c == '"' && i+1 < len(s) && s[i+1] == '"':
			cur.WriteByte('"')
			i++
		case c == '"':
			inQuote = !inQuote
		case !inQuote && c == ',':
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		case !inQuote && (c == ' ' || c == '\t') && cur.Len() == 0:
		default:
			cur.WriteByte(c)
		}
	}
	return append(out, strings.TrimSpace(cur.String()))
}

func ident(s string) string { return pgx.Identifier{s}.Sanitize() }

// literal quotes s as a standard SQL string literal
// (standard_conforming_strings is on by default since PostgreSQL 9.1).
func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
