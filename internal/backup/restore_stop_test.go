package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rsDatabasesLike counts databases whose name matches pattern (LIKE).
func rsDatabasesLike(t *testing.T, pg *rsPG, pattern string) int64 {
	t.Helper()
	conn := pg.connect(t, rsSuperUser, rsSuperPass, "postgres")
	defer conn.Close(context.Background())
	return rsCount(t, conn, `SELECT count(*) FROM pg_database WHERE datname LIKE $1`, pattern)
}

// TestRestoreStopConditions checks the four stop conditions of #3461 and
// the failures after the restore started, against a real PostgreSQL. 止まった
// ときに、戻す先の DB が作られていない (作ったものは消えている) ことも見る。
func TestRestoreStopConditions(t *testing.T) {
	pg := rsStartPG(t, "postgres:18-alpine")
	rsSetupInstance(t, pg, false)
	storage := newRSStorage()
	meta := rsTakeBackup(t, pg, storage, rsAppDB, rsGenID, rsBackupOptions{verified: true})
	ctx := context.Background()
	swap := RestoreOptions{ID: rsGenID, Mode: RestoreSwap, Database: rsAppDB, Confirm: rsAppDB}
	super := pg.restorer(storage, rsSuperUser, rsSuperPass)

	noLeftovers := func(t *testing.T) {
		t.Helper()
		assert.Equal(t, int64(0), rsDatabasesLike(t, pg, rsAppDB+"\\_%restore\\_%"))
		app := pg.connect(t, rsAppUser, rsAppPass, rsAppDB)
		assert.Equal(t, int64(1), rsCount(t, app, `SELECT count(*) FROM note`))
		_ = app.Close(ctx)
	}

	t.Run("target not confirmed", func(t *testing.T) {
		for _, confirm := range []string{"", "misskey", rsAppDB + " "} {
			o := swap
			o.Confirm = confirm
			_, err := super.Restore(ctx, o)
			require.ErrorIs(t, err, ErrRestoreNotConfirmed, confirm)
			assert.Contains(t, err.Error(), "-confirm "+rsAppDB)
			require.ErrorIs(t, super.CheckTarget(ctx, o), ErrRestoreNotConfirmed)
		}
		noLeftovers(t)
	})

	t.Run("connections remain on the target", func(t *testing.T) {
		held := pg.connect(t, rsAppUser, rsAppPass, rsAppDB)
		var pid int
		require.NoError(t, held.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
		// 別の DB への接続は数えない。
		other := pg.connect(t, rsAppUser, rsAppPass, "postgres")
		_, err := super.Restore(ctx, swap)
		require.ErrorIs(t, err, ErrRestoreConnections)
		assert.Contains(t, err.Error(), "1 sessions")
		assert.Contains(t, err.Error(), "pid="+strconv.Itoa(pid))
		require.ErrorIs(t, super.CheckNoConnections(ctx, "", rsAppDB), ErrRestoreConnections)

		empty := RestoreOptions{ID: rsGenID, Mode: RestoreEmpty, Database: rsAppDB, Confirm: rsAppDB}
		require.ErrorIs(t, super.CheckTarget(ctx, empty), ErrRestoreConnections)
		// 名前の入れ替えも、接続が残っていれば通さない。
		require.ErrorIs(t, super.ReplaceDatabase(ctx, "", rsAppDB, "x_before", "x_restore"), ErrRestoreConnections)
		// メンテナンスに入る前に準備する呼び出し方 (#3463) では、接続を見ない。
		o := swap
		o.SkipConnectionCheck = true
		require.NoError(t, super.CheckTarget(ctx, o))
		_ = held.Close(ctx)
		_ = other.Close(ctx)
		require.NoError(t, super.CheckNoConnections(ctx, "", rsAppDB))
		noLeftovers(t)
	})

	t.Run("backup has migrations newer than the binary", func(t *testing.T) {
		bundled, _, err := latestMigration(rsCoreMigrations)
		require.NoError(t, err)
		cases := map[string]func(*Meta){
			"core": func(m *Meta) { m.Migrations[0].Version = int64(bundled) + 1 },
			"local": func(m *Meta) {
				m.Migrations[1] = MigrationState{Table: LocalMigrationsTable, Version: 900001}
			},
		}
		ids := map[string]string{"core": "20261010T040000Z", "local": "20261010T040001Z"}
		for name, mutate := range cases {
			rsTakeBackup(t, pg, storage, rsAppDB, ids[name], rsBackupOptions{mutateMeta: mutate})
			o := swap
			o.ID = ids[name]
			_, err := super.Restore(ctx, o)
			require.ErrorIs(t, err, ErrRestoreMigrationTooNew, name)
		}
		// 同梱と同じ番号までは通す (境界)。
		atBundled := "20261010T040002Z"
		rsTakeBackup(t, pg, storage, rsAppDB, atBundled, rsBackupOptions{mutateMeta: func(m *Meta) { m.Migrations[0].Version = int64(bundled) }})
		_, err = super.Load(ctx, atBundled)
		require.NoError(t, err)
		noLeftovers(t)
	})

	t.Run("no privilege to create databases", func(t *testing.T) {
		app := pg.restorer(storage, rsAppUser, rsAppPass)
		_, err := app.Restore(ctx, swap)
		require.ErrorIs(t, err, ErrRestoreNoPrivilege)
		assert.Contains(t, err.Error(), "CREATEDB")
		assert.Contains(t, err.Error(), "docs/deployment.md")
		assert.NotContains(t, err.Error(), "ownership")

		// CREATEDB があっても、今の DB の持ち主でなければ名前を変えられない。
		pg.exec(t, "postgres", "CREATE ROLE other LOGIN PASSWORD 'other' CREATEDB")
		_, err = pg.restorer(storage, "other", "other").Restore(ctx, swap)
		require.ErrorIs(t, err, ErrRestoreNoPrivilege)
		assert.Contains(t, err.Error(), "ownership")
		assert.NotContains(t, err.Error(), "CREATEDB and")
		noLeftovers(t)

		// 持ち主に CREATEDB を足すと通る。
		pg.exec(t, "postgres", "ALTER ROLE "+rsAppUser+" CREATEDB")
		t.Cleanup(func() { pg.exec(t, "postgres", "ALTER ROLE "+rsAppUser+" NOCREATEDB") })
		require.NoError(t, app.CheckTarget(ctx, swap))
	})

	t.Run("empty mode into a database with tables", func(t *testing.T) {
		_, err := super.Restore(ctx, RestoreOptions{ID: rsGenID, Mode: RestoreEmpty, Database: rsAppDB, Confirm: rsAppDB})
		require.ErrorIs(t, err, ErrRestoreNotEmpty)
		pg.exec(t, "postgres", "CREATE DATABASE missing_tables_ok")
		require.NoError(t, super.CheckTarget(ctx, RestoreOptions{Mode: RestoreEmpty, Database: "missing_tables_ok", Confirm: "missing_tables_ok"}))
		// 管理者の持ち物の DB へ、アプリのユーザーでは戻せない (public に CREATE できない)。
		err = pg.restorer(storage, rsAppUser, rsAppPass).CheckTarget(ctx, RestoreOptions{Mode: RestoreEmpty, Database: "missing_tables_ok", Confirm: "missing_tables_ok"})
		require.ErrorIs(t, err, ErrRestoreNoPrivilege)
		assert.Contains(t, err.Error(), "OWNER")
		err = super.CheckTarget(ctx, RestoreOptions{Mode: RestoreEmpty, Database: "nope", Confirm: "nope"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "create the empty database first")
		noLeftovers(t)
	})

	t.Run("extension missing on the target server", func(t *testing.T) {
		id := "20261010T045000Z"
		rsTakeBackup(t, pg, storage, rsAppDB, id, rsBackupOptions{mutateMeta: func(m *Meta) {
			m.Extensions = append(m.Extensions, ExtensionInfo{Name: "no_such_ext", Version: "1.0", Schema: "public"})
		}})
		o := swap
		o.ID = id
		_, err := super.Restore(ctx, o)
		require.ErrorIs(t, err, ErrRestoreExtension)
		assert.Contains(t, err.Error(), "no_such_ext")
		assert.NotContains(t, err.Error(), "plpgsql")
		noLeftovers(t)
		pg.exec(t, "postgres", "CREATE DATABASE ext_empty")
		t.Cleanup(func() { pg.exec(t, "postgres", "DROP DATABASE ext_empty") })
		plan, err := super.Load(ctx, id)
		require.NoError(t, err)
		err = super.CheckCompatibility(ctx, plan, RestoreOptions{Mode: RestoreEmpty, Database: "ext_empty", Confirm: "ext_empty"})
		require.ErrorIs(t, err, ErrRestoreExtension)
	})

	t.Run("locale of the backup", func(t *testing.T) {
		plan, err := super.Load(ctx, rsGenID)
		require.NoError(t, err)
		require.NotEmpty(t, plan.Meta.DatabaseLocale.Encoding)
		// 空の DB へ戻す形: 戻す先の照合順序がバックアップと違えば止まる。
		pg.exec(t, "postgres",
			"CREATE DATABASE locale_c TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C'",
			"CREATE DATABASE locale_same TEMPLATE template0 ENCODING "+literal(plan.Meta.DatabaseLocale.Encoding)+
				" LC_COLLATE "+literal(plan.Meta.DatabaseLocale.Collate)+" LC_CTYPE "+literal(plan.Meta.DatabaseLocale.Ctype))
		t.Cleanup(func() { pg.exec(t, "postgres", "DROP DATABASE locale_c", "DROP DATABASE locale_same") })
		err = super.CheckCompatibility(ctx, plan, RestoreOptions{Mode: RestoreEmpty, Database: "locale_c", Confirm: "locale_c"})
		require.ErrorIs(t, err, ErrRestoreLocale)
		assert.Contains(t, err.Error(), `CREATE DATABASE "locale_c" WITH TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE `+literal(plan.Meta.DatabaseLocale.Collate))
		require.NoError(t, super.CheckCompatibility(ctx, plan, RestoreOptions{Mode: RestoreEmpty, Database: "locale_same", Confirm: "locale_same"}))
		_, err = super.Restore(ctx, RestoreOptions{ID: rsGenID, Mode: RestoreEmpty, Database: "locale_c", Confirm: "locale_c"})
		require.ErrorIs(t, err, ErrRestoreLocale)
		// 記録の無い古いメタ情報では比べない。
		noLocale := *plan
		noLocale.Meta.DatabaseLocale = DatabaseLocale{}
		require.NoError(t, super.CheckCompatibility(ctx, &noLocale, RestoreOptions{Mode: RestoreEmpty, Database: "locale_c", Confirm: "locale_c"}))

		// 入れ替える形: 今の DB ではなく、バックアップに記録した値で DB を作る。
		id := "20261010T045500Z"
		rsTakeBackup(t, pg, storage, rsAppDB, id, rsBackupOptions{mutateMeta: func(m *Meta) {
			m.DatabaseLocale = DatabaseLocale{Encoding: "UTF8", Collate: "C", Ctype: "C", Provider: "libc"}
		}})
		var out bytes.Buffer
		r := pg.restorer(storage, rsSuperUser, rsSuperPass)
		r.Out = &out
		o := swap
		o.ID = id
		res, err := r.Restore(ctx, o)
		require.NoError(t, err, out.String())
		assert.Contains(t, out.String(), "the restored database uses the backup's")
		conn := pg.connect(t, rsSuperUser, rsSuperPass, "postgres")
		var collate, ctype string
		require.NoError(t, conn.QueryRow(ctx, `SELECT datcollate, datctype FROM pg_database WHERE datname = $1`, rsAppDB).Scan(&collate, &ctype))
		assert.Equal(t, "C", collate)
		assert.Equal(t, "C", ctype)
		var keptCollate string
		require.NoError(t, conn.QueryRow(ctx, `SELECT datcollate FROM pg_database WHERE datname = $1`, res.BeforeRestore).Scan(&keptCollate))
		assert.Equal(t, plan.Meta.DatabaseLocale.Collate, keptCollate)
		_ = conn.Close(ctx)
		// 後の subtest のために、元の DB へ戻す。
		require.NoError(t, r.ReplaceDatabase(ctx, "", rsAppDB, rsAppDB+"_locale_c", res.BeforeRestore))
		require.NoError(t, r.DropDatabase(ctx, "", rsAppDB+"_locale_c"))
		noLeftovers(t)
	})

	t.Run("row counts differ", func(t *testing.T) {
		id := "20261010T050000Z"
		rsTakeBackup(t, pg, storage, rsAppDB, id, rsBackupOptions{mutateMeta: func(m *Meta) { m.RowCounts["public.note"]++ }})
		o := swap
		o.ID = id
		_, err := super.Restore(ctx, o)
		require.ErrorIs(t, err, ErrRestoreRowMismatch)
		assert.Contains(t, err.Error(), "public.note: 1 rows, backup has 2 rows")
		noLeftovers(t)
	})

	t.Run("dump does not match its metadata", func(t *testing.T) {
		id := "20261010T060000Z"
		m := rsTakeBackup(t, pg, storage, rsAppDB, id, rsBackupOptions{})
		storage.objs[Key(id, m.DumpFile)][100] ^= 0xff
		o := swap
		o.ID = id
		_, err := super.Restore(ctx, o)
		require.ErrorIs(t, err, ErrRestoreChecksum)
		noLeftovers(t)
	})

	t.Run("pg_restore fails", func(t *testing.T) {
		// メタ情報とは一致するが、途中で切れた dump。
		id := "20261010T070000Z"
		full := storage.objs[Key(rsGenID, meta.DumpFile)]
		rsTakeBackup(t, pg, storage, rsAppDB, id, rsBackupOptions{})
		storage.objs[Key(id, DumpFile)] = full[:len(full)/2]
		rsRehash(t, storage, id)
		o := swap
		o.ID = id
		_, err := super.Restore(ctx, o)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "pg_restore")
		noLeftovers(t)
	})

	t.Run("replace and undo", func(t *testing.T) {
		pg.exec(t, "postgres", "CREATE DATABASE swap_a", "CREATE DATABASE swap_b")
		require.NoError(t, super.ReplaceDatabase(ctx, "", "swap_a", "swap_a_old", "swap_b"))
		assert.Equal(t, int64(1), rsDatabasesLike(t, pg, "swap_a_old"))
		assert.Equal(t, int64(0), rsDatabasesLike(t, pg, "swap_b"))
		// 入れ替え先が無ければ、どちらの名前も変わらない (1 つのトランザクション)。
		require.Error(t, super.ReplaceDatabase(ctx, "", "swap_a", "swap_a_x", "no_such_db"))
		assert.Equal(t, int64(1), rsDatabasesLike(t, pg, "swap_a"))
		assert.Equal(t, int64(0), rsDatabasesLike(t, pg, "swap_a_x"))
		require.NoError(t, super.DropDatabase(ctx, "", "swap_a_old"))
		require.NoError(t, super.DropDatabase(ctx, "", "swap_a_old"))
		assert.Equal(t, int64(0), rsDatabasesLike(t, pg, "swap_a_old"))
	})

	t.Run("swap outcome after a failure", func(t *testing.T) {
		// 名前の入れ替えの後でエラーが返ったときは、pg_database で状態を確かめる。
		pg.exec(t, "postgres", "CREATE DATABASE oc_before", "CREATE DATABASE oc_restore_only")
		t.Cleanup(func() { pg.exec(t, "postgres", "DROP DATABASE oc_before", "DROP DATABASE oc_restore_only") })
		assert.Equal(t, swapDone, super.swapOutcome("", "oc_restore", "oc_before"))
		assert.Equal(t, swapNotDone, super.swapOutcome("", "oc_restore_only", "oc_before_x"))
		assert.Equal(t, swapUnknown, super.swapOutcome("", "oc_restore_only", "oc_before"))
		assert.Equal(t, swapUnknown, super.swapOutcome("", "oc_none", "oc_none_before"))
	})

	t.Run("swap reported as failed after the names changed", func(t *testing.T) {
		var out bytes.Buffer
		r := pg.restorer(storage, rsSuperUser, rsSuperPass)
		r.Out = &out
		r.afterReplace = func() error { return errors.New("conn closed after commit") }
		res, err := r.Restore(ctx, swap)
		require.NoError(t, err, out.String())
		assert.Equal(t, rsAppDB+"_before_restore_20261010120000", res.BeforeRestore)
		assert.Contains(t, out.String(), "was already replaced")
		assert.NotContains(t, out.String(), "was not changed")
		// 戻した DB が今の名前で使われている。後の subtest のために元へ戻す。
		require.NoError(t, r.ReplaceDatabase(ctx, "", rsAppDB, rsAppDB+"_oc_x", res.BeforeRestore))
		require.NoError(t, r.DropDatabase(ctx, "", rsAppDB+"_oc_x"))
		noLeftovers(t)
	})

	t.Run("swap whose outcome cannot be told", func(t *testing.T) {
		// 入れ替えの後、作った名前の DB がまた現れる (両方の名前がある) と、どちらとも
		// 言えない。そのときは作った名前の DB を消さずに止まる。
		restoreName, beforeName, err := RestoreNames(rsAppDB, super.now())
		require.NoError(t, err)
		r := pg.restorer(storage, rsSuperUser, rsSuperPass)
		r.afterReplace = func() error {
			pg.exec(t, "postgres", "CREATE DATABASE "+restoreName)
			return errors.New("conn closed")
		}
		_, err = r.Restore(ctx, swap)
		require.ErrorIs(t, err, ErrRestoreSwitchUnknown)
		assert.Contains(t, err.Error(), "check pg_database for "+restoreName+" and "+beforeName)
		assert.Equal(t, int64(1), rsDatabasesLike(t, pg, restoreName))
		assert.Equal(t, int64(1), rsDatabasesLike(t, pg, beforeName))
		// 後の subtest のために元へ戻す。
		require.NoError(t, r.DropDatabase(ctx, "", restoreName))
		require.NoError(t, super.ReplaceDatabase(ctx, "", rsAppDB, rsAppDB+"_oc_y", beforeName))
		require.NoError(t, super.DropDatabase(ctx, "", rsAppDB+"_oc_y"))
		noLeftovers(t)
	})

	t.Run("rollback switch reported as failed after the names changed", func(t *testing.T) {
		pg.exec(t, "postgres", "CREATE DATABASE rb_cur", "CREATE DATABASE rb_kept")
		t.Cleanup(func() { pg.exec(t, "postgres", "DROP DATABASE IF EXISTS rb_cur", "DROP DATABASE IF EXISTS rb_aside") })
		var out bytes.Buffer
		r := pg.restorer(storage, rsSuperUser, rsSuperPass)
		r.Out = &out
		r.afterReplace = func() error { return errors.New("conn closed after commit") }
		require.NoError(t, r.SwitchDatabase(ctx, "", "rb_cur", "rb_aside", "rb_kept"))
		assert.Contains(t, out.String(), "was already replaced")
		assert.Equal(t, int64(1), rsDatabasesLike(t, pg, "rb_aside"))
		assert.Equal(t, int64(0), rsDatabasesLike(t, pg, "rb_kept"))
		// 入れ替わっていなければ (元の名前のまま)、元のエラーを返す。
		pg.exec(t, "postgres", "CREATE DATABASE rb_kept2")
		t.Cleanup(func() { pg.exec(t, "postgres", "DROP DATABASE IF EXISTS rb_kept2") })
		r.afterReplace = nil
		held := pg.connect(t, rsSuperUser, rsSuperPass, "rb_cur")
		err := r.SwitchDatabase(ctx, "", "rb_cur", "rb_aside2", "rb_kept2")
		require.ErrorIs(t, err, ErrRestoreConnections)
		assert.NotErrorIs(t, err, ErrRestoreSwitchUnknown)
		_ = held.Close(ctx)
	})

	t.Run("existing restore names", func(t *testing.T) {
		restored, _, err := RestoreNames(rsAppDB, super.now())
		require.NoError(t, err)
		pg.exec(t, "postgres", "CREATE DATABASE "+restored)
		t.Cleanup(func() { pg.exec(t, "postgres", "DROP DATABASE "+restored) })
		err = super.CheckTarget(ctx, swap)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already exists")

		// Restore は時刻を 1 回だけ取り、CheckTarget が確かめる名前と作る名前を揃える。
		// 時計が進んでも、確かめる段で止まる (作る段の CREATE DATABASE で落ちるのではない)。
		r := pg.restorer(storage, rsSuperUser, rsSuperPass)
		at := super.now()
		calls := 0
		r.Now = func() time.Time {
			calls++
			return at.Add(time.Duration(calls-1) * time.Second)
		}
		_, err = r.Restore(ctx, swap)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `backup: database "`+restored+`" already exists`)
		assert.Equal(t, 1, calls)
		o := swap
		o.At = at.Add(time.Hour)
		require.NoError(t, super.CheckTarget(ctx, o))

		// 通るときも、退避した名前は最初に取った時刻から作る。
		base := at.Add(2 * time.Hour)
		calls = 0
		r.Now = func() time.Time {
			calls++
			return base.Add(time.Duration(calls-1) * time.Second)
		}
		res, err := r.Restore(ctx, swap)
		require.NoError(t, err)
		_, wantBefore, err := RestoreNames(rsAppDB, base)
		require.NoError(t, err)
		assert.Equal(t, wantBefore, res.BeforeRestore)
		assert.Equal(t, 1, calls)
		require.NoError(t, super.ReplaceDatabase(ctx, "", rsAppDB, rsAppDB+"_oc_z", res.BeforeRestore))
		require.NoError(t, super.DropDatabase(ctx, "", rsAppDB+"_oc_z"))
		// 最初に作った restored はこの subtest の Cleanup で消す。それ以外は残っていない。
		assert.Equal(t, int64(1), rsDatabasesLike(t, pg, rsAppDB+"\\_%restore\\_%"))
	})

	t.Run("steps on a scratch database", func(t *testing.T) {
		pg.exec(t, "postgres", "CREATE DATABASE scratch")
		// 設定の名前が不正、または PostgreSQL が受けない設定。
		_, err := super.ApplyDatabaseSettings(ctx, "scratch", Meta{DatabaseSettings: []string{"bad name=1"}})
		assert.ErrorContains(t, err, "invalid database setting")
		applied, err := super.ApplyDatabaseSettings(ctx, "scratch", Meta{DatabaseSettings: []string{"work_mem=4MB", "no_such_setting=1"}})
		assert.ErrorContains(t, err, "no_such_setting")
		assert.Equal(t, []string{"work_mem=4MB"}, applied)

		// 行数の鍵の形が不正、表が無い。
		// 戻した DB に無い表、メタ情報に無い表のどちらも食い違いにする (verify と同じ比べ方)。
		err = super.VerifyRestored(ctx, "scratch", Meta{RowCounts: map[string]int64{"public.missing": 0}})
		assert.ErrorIs(t, err, ErrRestoreRowMismatch)
		assert.ErrorContains(t, err, "public.missing: no table, backup has 0 rows")
		pg.exec(t, "scratch", "CREATE TABLE extra (x int)")
		err = super.VerifyRestored(ctx, "scratch", Meta{RowCounts: map[string]int64{}})
		assert.ErrorContains(t, err, "public.extra: 0 rows, backup has no table")
		pg.exec(t, "scratch", "DROP TABLE extra")

		// fork の系列 (#3428) も当てる。管理表は schema_migrations_local。
		local := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(local, "900001_fork.up.sql"), []byte("CREATE TABLE fork_t (id int);"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(local, "900001_fork.down.sql"), []byte("DROP TABLE fork_t;"), 0o600))
		r := pg.restorer(storage, rsSuperUser, rsSuperPass)
		r.LocalMigrationsDir = local
		states, err := r.Migrate("scratch")
		require.NoError(t, err)
		require.Len(t, states, 2)
		assert.Equal(t, LocalMigrationsTable, states[1].Table)
		assert.Equal(t, int64(900001), states[1].Version)
		conn := pg.connect(t, rsSuperUser, rsSuperPass, "scratch")
		assert.Equal(t, int64(900001), rsCount(t, conn, `SELECT version FROM schema_migrations_local`))

		// 落ちる migration は、その系列の名前と一緒に返す。
		_, err = conn.Exec(ctx, `UPDATE schema_migrations SET dirty = true`)
		require.NoError(t, err)
		_ = conn.Close(ctx)
		_, err = r.Migrate("scratch")
		assert.ErrorContains(t, err, "migrate "+CoreMigrationsTable)

		// dirty な管理表は、検査で落ちる。
		g, closeDB := rsGormAs(t, pg, rsSuperUser, rsSuperPass, "scratch")
		defer closeDB()
		report, err := CheckRestored(ctx, RestoreCheckDeps{DB: g, CoreMigrationsDir: rsCoreMigrations})
		require.NoError(t, err)
		assert.False(t, report.OK)
	})

	t.Run("pg_restore version", func(t *testing.T) {
		o := RestoreOptions{Mode: RestoreSwap, Database: rsAppDB, Confirm: rsAppDB}
		r := pg.restorer(storage, rsSuperUser, rsSuperPass)
		r.Exec = func(context.Context, string, []string, []string, io.Reader, io.Writer, io.Writer) error {
			return errors.New("not found")
		}
		assert.ErrorContains(t, r.CheckTarget(ctx, o), "--version")
		r.Exec = func(_ context.Context, _ string, _ []string, _ []string, _ io.Reader, out io.Writer, _ io.Writer) error {
			_, err := io.WriteString(out, "garbage")
			return err
		}
		assert.ErrorContains(t, r.CheckTarget(ctx, o), "cannot read the pg_restore version")
		r.Exec = func(_ context.Context, _ string, _ []string, _ []string, _ io.Reader, out io.Writer, _ io.Writer) error {
			_, err := io.WriteString(out, "pg_restore (PostgreSQL) 19.1\n")
			return err
		}
		assert.ErrorIs(t, r.CheckTarget(ctx, o), ErrRestoreVersion)
	})
}
