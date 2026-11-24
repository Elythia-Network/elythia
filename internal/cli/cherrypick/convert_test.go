package cherrypick

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/maintenance"
	"github.com/elythia-network/elythia/internal/server"
	"github.com/elythia-network/elythia/internal/testutil"
)

var convertNow = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

// fakeDeleter deletes the way the server's paths end up in the database and
// records the calls.
type fakeDeleter struct {
	db         *gorm.DB
	accounts   []string
	notes      []string
	failOn     string
	closeErr   error
	closed     bool
	doNothings bool
	waitErr    error
	waits      int
	// cleanupFailOn fails like an account flagged deleted whose cleanup was
	// not queued.
	cleanupFailOn string
}

func (d *fakeDeleter) Wait(context.Context) error { d.waits++; return d.waitErr }

func (d *fakeDeleter) DeleteMigratedAccount(id string) error {
	if id == d.failOn {
		return errors.New("boom")
	}
	if id == d.cleanupFailOn {
		return fmt.Errorf("delete: %w", server.ErrAccountCleanupNotQueued)
	}
	d.accounts = append(d.accounts, id)
	if d.doNothings {
		return nil
	}
	return d.db.Exec(`UPDATE "user" SET "isDeleted" = true, "isSuspended" = true WHERE id = ?`, id).Error
}

func (d *fakeDeleter) DeleteMigratedNote(id string) error {
	if id == d.failOn {
		return errors.New("boom")
	}
	d.notes = append(d.notes, id)
	if d.doNothings {
		return nil
	}
	return d.db.Exec(`DELETE FROM note WHERE id = ?`, id).Error
}

func (d *fakeDeleter) Close() error { d.closed = true; return d.closeErr }

// convertDB recreates a CherryPick 4.18.0-beta.1 schema that Elythia has
// migrated (schema_migrations has a row) and seeds one of each item.
func convertDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := testutil.OpenTestDBSchema("convert")
	require.NoError(t, err)
	var schema string
	require.NoError(t, db.Raw("SELECT current_schema()").Scan(&schema).Error)
	require.Equal(t, "internal_cli_cherrypick_convert", schema)
	require.NoError(t, db.Exec(`DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`).Error)
	require.NoError(t, db.Exec(`CREATE SCHEMA "`+schema+`"`).Error)
	require.NoError(t, testutil.LoadCherryPickSchema(db))
	for _, sql := range []string{
		`CREATE TABLE schema_migrations (version bigint PRIMARY KEY, dirty boolean NOT NULL)`,
		`INSERT INTO schema_migrations VALUES (147, false)`,
		`INSERT INTO meta (id, "approvalRequiredForSignup", "repositoryUrl") VALUES ('x', true, 'https://github.com/kokonect-link/cherrypick')`,
		`INSERT INTO "user" (id, username, "usernameLower", approved) VALUES ('a1', 'alice', 'alice', true)`,
		`INSERT INTO "user" (id, username, "usernameLower", approved, "signupReason") VALUES ('p1', 'carol', 'carol', false, 'hi')`,
		`INSERT INTO abuse_report_resolver (id, "updatedAt", name, "reportContentPattern", "expiresAt", forward)
			VALUES ('r1', now(), 'spam', 'buy now', 'indefinitely', false)`,
		`INSERT INTO instance (id, "firstRetrievedAt", host, "quarantineLimited") VALUES ('i1', now(), 'q.example', true)`,
	} {
		require.NoError(t, db.Exec(sql).Error, sql)
	}
	past, future := convertNow.Add(-time.Hour), convertNow.Add(time.Hour)
	require.NoError(t, db.Exec(`INSERT INTO note (id, "userId", visibility, "deleteAt") VALUES
		('n1', 'a1', 'public', ?), ('n2', 'a1', 'public', ?)`, past, future).Error)
	return db
}

type convertHarness struct {
	e       convertEnv
	stdout  *bytes.Buffer
	stderr  *bytes.Buffer
	deleter *fakeDeleter
	built   int
}

func newConvertHarness(t *testing.T, db *gorm.DB) *convertHarness {
	h := &convertHarness{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, deleter: &fakeDeleter{db: db}}
	h.e = convertEnv{
		stdout:     h.stdout,
		stderr:     h.stderr,
		loadConfig: func(string) (*config.Config, error) { return &config.Config{}, nil },
		openDB:     func(*config.Config) (*gorm.DB, func(), error) { return db, func() {}, nil },
		newDeleter: func(*config.Config) (deleter, error) { h.built++; return h.deleter, nil },
		createFile: defaultConvertEnv().createFile,
		now:        func() time.Time { return convertNow },
	}
	return h
}

func unprocessed(t *testing.T, db *gorm.DB) int {
	t.Helper()
	r, err := maintenance.InspectCherryPick(t.Context(), db, convertNow)
	require.NoError(t, err)
	return r.Unprocessed()
}

func converted(t *testing.T, db *gorm.DB) bool {
	t.Helper()
	ok, err := maintenance.CherryPickConverted(t.Context(), db)
	require.NoError(t, err)
	return ok
}

func TestConvert_MissingChoices(t *testing.T) {
	db := convertDB(t)
	h := newConvertHarness(t, db)
	assert.Equal(t, 2, convert(h.e, []string{"-apply"}))
	assert.Contains(t, h.stdout.String(), "quarantined instances       1  -> (choose)")
	for _, want := range []string{"-quarantined block|lift", "-later-deletes delete|keep", "-resolvers-out <file> and -accept-manual-reports"} {
		assert.Contains(t, h.stderr.String(), want)
	}
	assert.Equal(t, 5, unprocessed(t, db), "選ばないうちは何も変えない")
	assert.Zero(t, h.built)
}

func TestConvert_DryRun(t *testing.T) {
	db := convertDB(t)
	h := newConvertHarness(t, db)
	out := filepath.Join(t.TempDir(), "resolvers.json")
	code := convert(h.e, []string{"-quarantined", "block", "-later-deletes", "keep", "-resolvers-out", out, "-accept-manual-reports"})
	assert.Equal(t, 0, code, h.stderr.String())
	assert.Contains(t, h.stdout.String(), "users waiting for approval  1  -> suspend")
	assert.Contains(t, h.stdout.String(), `@carol  p1  "hi"`, "申請の理由も出す")
	assert.Contains(t, h.stdout.String(), "scheduled deletes, due      1  -> delete")
	assert.Contains(t, h.stdout.String(), "nothing was changed; pass -apply")
	assert.Equal(t, 5, unprocessed(t, db))
	assert.NoFileExists(t, out)
	assert.Zero(t, h.built)
	assert.False(t, converted(t, db))
}

func TestConvert_Apply(t *testing.T) {
	db := convertDB(t)
	h := newConvertHarness(t, db)
	out := filepath.Join(t.TempDir(), "resolvers.json")
	code := convert(h.e, []string{"-apply", "-quarantined", "block", "-later-deletes", "keep", "-resolvers-out", out, "-accept-manual-reports"})
	require.Equal(t, 0, code, h.stderr.String())
	assert.Contains(t, h.stdout.String(), "converted; Elythia can now serve this database")

	assert.Zero(t, unprocessed(t, db))
	assert.True(t, converted(t, db))
	assert.Empty(t, h.deleter.accounts, "凍結はSQLで済ませる")
	assert.Equal(t, []string{"n1"}, h.deleter.notes, "期限切れは本体の削除の処理で消す")
	assert.Equal(t, 1, h.deleter.waits, "止める前に配送を積み終わるのを待つ")
	assert.True(t, h.deleter.closed)

	raw, err := os.ReadFile(out)
	require.NoError(t, err)
	var rows []maintenance.CherryPickResolver
	require.NoError(t, json.Unmarshal(raw, &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "spam", rows[0].Name)

	// 2 回目は済んでいると答えて何もしない。
	h2 := newConvertHarness(t, db)
	assert.Equal(t, 0, convert(h2.e, []string{"-apply"}))
	assert.Contains(t, h2.stdout.String(), "already been converted")
	assert.Zero(t, h2.built)
}

func TestConvert_ApplyDeletes(t *testing.T) {
	db := convertDB(t)
	h := newConvertHarness(t, db)
	code := convert(h.e, []string{"-apply", "-pending-users", "delete", "-quarantined", "lift", "-later-deletes", "delete",
		"-resolvers-out", filepath.Join(t.TempDir(), "r.json"), "-accept-manual-reports"})
	require.Equal(t, 0, code, h.stderr.String())
	assert.Equal(t, []string{"p1"}, h.deleter.accounts)
	assert.Equal(t, []string{"n1", "n2"}, h.deleter.notes)
	assert.Equal(t, 1, h.deleter.waits, "待つのは止める前の1回")
	assert.Equal(t, 1, h.built)
	assert.True(t, converted(t, db))
	assert.Contains(t, h.stdout.String(), "deleted 1 account(s) and 2 note(s)")
}

func TestConvert_NothingForTheServer(t *testing.T) {
	db := convertDB(t)
	require.NoError(t, db.Exec(`DELETE FROM note`).Error)
	h := newConvertHarness(t, db)
	code := convert(h.e, []string{"-apply", "-pending-users", "approve", "-quarantined", "lift",
		"-resolvers-out", filepath.Join(t.TempDir(), "r.json"), "-accept-manual-reports"})
	require.Equal(t, 0, code, h.stderr.String())
	assert.Zero(t, h.built, "消すものが無ければ本体を組み立てない")
	assert.True(t, converted(t, db))
}

// 理由の無い承認されていない利用者は、選ぶまで変換しない。選べばその通りに処理し、
// 理由のある承認待ちとは別に扱う。
func TestConvert_UnapprovedMembers(t *testing.T) {
	base := func(t *testing.T) []string {
		return []string{"-apply", "-quarantined", "lift", "-later-deletes", "keep",
			"-resolvers-out", filepath.Join(t.TempDir(), "r.json"), "-accept-manual-reports"}
	}
	seed := func(t *testing.T) *gorm.DB {
		db := convertDB(t)
		require.NoError(t, db.Exec(`INSERT INTO "user" (id, username, "usernameLower", approved) VALUES ('m1', 'erin', 'erin', false)`).Error)
		return db
	}
	t.Run("required", func(t *testing.T) {
		db := seed(t)
		h := newConvertHarness(t, db)
		assert.Equal(t, 2, convert(h.e, base(t)))
		assert.Contains(t, h.stdout.String(), "users without approval      1  -> (choose)")
		assert.Contains(t, h.stderr.String(), "-unapproved-members suspend|delete|approve")
		assert.False(t, converted(t, db))
	})
	t.Run("suspend", func(t *testing.T) {
		db := seed(t)
		h := newConvertHarness(t, db)
		require.Equal(t, 0, convert(h.e, append(base(t), "-unapproved-members", "suspend")), h.stderr.String())
		var suspended bool
		require.NoError(t, db.Raw(`SELECT "isSuspended" FROM "user" WHERE id = 'm1'`).Scan(&suspended).Error)
		assert.True(t, suspended)
		assert.True(t, converted(t, db))
	})
	t.Run("delete", func(t *testing.T) {
		db := seed(t)
		h := newConvertHarness(t, db)
		require.Equal(t, 0, convert(h.e, append(base(t), "-unapproved-members", "delete")), h.stderr.String())
		assert.Equal(t, []string{"m1"}, h.deleter.accounts, "理由のある p1 は -pending-users の既定 (凍結) に従う")
		assert.True(t, converted(t, db))
	})
	// 初期ユーザーと管理者は数えず、凍結もしない。誰が対象かは名前で出す。
	t.Run("staff are left alone", func(t *testing.T) {
		db := seed(t)
		for _, sql := range []string{
			`INSERT INTO "user" (id, username, "usernameLower", approved) VALUES ('root1', 'root', 'root', false), ('adm1', 'adm', 'adm', false)`,
			`UPDATE meta SET "rootUserId" = 'root1'`,
			`INSERT INTO role (id, "updatedAt", "lastUsedAt", name, description, "isAdministrator") VALUES ('ra', now(), now(), 'admins', '', true)`,
			`INSERT INTO role_assignment (id, "userId", "roleId") VALUES ('as1', 'adm1', 'ra')`,
		} {
			require.NoError(t, db.Exec(sql).Error, sql)
		}
		h := newConvertHarness(t, db)
		require.Equal(t, 0, convert(h.e, append(base(t), "-unapproved-members", "suspend")), h.stderr.String())
		assert.Contains(t, h.stdout.String(), "users without approval      1  -> suspend\n    @erin\n")
		assert.Contains(t, h.stdout.String(), "left as they are: the root user, administrators and moderators without approval  2\n    @adm\n    @root\n")
		var suspended []string
		require.NoError(t, db.Raw(`SELECT id FROM "user" WHERE "isSuspended" ORDER BY id`).Scan(&suspended).Error)
		assert.Equal(t, []string{"m1", "p1"}, suspended, "p1 は -pending-users の既定 (凍結)")
		assert.True(t, converted(t, db))
	})
	t.Run("approve", func(t *testing.T) {
		db := seed(t)
		h := newConvertHarness(t, db)
		require.Equal(t, 0, convert(h.e, append(base(t), "-unapproved-members", "approve")), h.stderr.String())
		var approved bool
		require.NoError(t, db.Raw(`SELECT approved FROM "user" WHERE id = 'm1'`).Scan(&approved).Error)
		assert.True(t, approved)
		assert.Empty(t, h.deleter.accounts)
		assert.True(t, converted(t, db))
	})
}

func TestConvert_Failures(t *testing.T) {
	choices := func(t *testing.T) []string {
		return []string{"-apply", "-quarantined", "lift", "-later-deletes", "delete",
			"-resolvers-out", filepath.Join(t.TempDir(), "r.json"), "-accept-manual-reports"}
	}
	t.Run("delete fails", func(t *testing.T) {
		db := convertDB(t)
		h := newConvertHarness(t, db)
		h.deleter.failOn = "n2"
		assert.Equal(t, 1, convert(h.e, choices(t)))
		assert.Contains(t, h.stderr.String(), "delete note n2: boom")
		assert.True(t, h.deleter.closed, "失敗しても本体を止める")
		assert.False(t, converted(t, db))

		// もう一度流すと、残りだけを処理する。
		h2 := newConvertHarness(t, db)
		require.Equal(t, 0, convert(h2.e, []string{"-apply", "-later-deletes", "delete"}), h2.stderr.String())
		assert.Equal(t, []string{"n2"}, h2.deleter.notes)
		assert.True(t, converted(t, db))
	})
	t.Run("account delete fails", func(t *testing.T) {
		db := convertDB(t)
		h := newConvertHarness(t, db)
		h.deleter.failOn = "p1"
		args := append(choices(t), "-pending-users", "delete")
		assert.Equal(t, 1, convert(h.e, args))
		assert.Empty(t, h.deleter.notes, "アカウントで失敗したら投稿へ進まない")
		assert.NotContains(t, h.stderr.String(), "from the admin page", "flag が立つ前の失敗は流し直せばよい")
	})
	t.Run("account cleanup not queued", func(t *testing.T) {
		db := convertDB(t)
		h := newConvertHarness(t, db)
		h.deleter.cleanupFailOn = "p1"
		args := append(choices(t), "-pending-users", "delete")
		assert.Equal(t, 1, convert(h.e, args))
		assert.Contains(t, h.stderr.String(), "delete these accounts again from the admin page: p1")
	})
	t.Run("wait fails", func(t *testing.T) {
		db := convertDB(t)
		h := newConvertHarness(t, db)
		h.deleter.waitErr = errors.New("still running")
		assert.Equal(t, 1, convert(h.e, choices(t)))
		assert.Contains(t, h.stderr.String(), "wait for the deliveries of the deleted notes: still running")
		assert.Contains(t, h.stderr.String(), "does not queue their deliveries again")
		assert.False(t, converted(t, db))
	})
	t.Run("close fails", func(t *testing.T) {
		db := convertDB(t)
		h := newConvertHarness(t, db)
		h.deleter.closeErr = errors.New("stuck")
		assert.Equal(t, 1, convert(h.e, choices(t)))
		assert.Contains(t, h.stderr.String(), "stop the server: stuck")
		assert.Contains(t, h.stderr.String(), "does not queue their deliveries again")
		assert.False(t, converted(t, db))
	})
	t.Run("server cannot be built", func(t *testing.T) {
		db := convertDB(t)
		h := newConvertHarness(t, db)
		h.e.newDeleter = func(*config.Config) (deleter, error) { return nil, errors.New("no redis") }
		assert.Equal(t, 1, convert(h.e, choices(t)))
		assert.Contains(t, h.stderr.String(), "no redis")
	})
	t.Run("deletes do not take", func(t *testing.T) {
		db := convertDB(t)
		h := newConvertHarness(t, db)
		h.deleter.doNothings = true
		assert.Equal(t, ExitNeedsAction, convert(h.e, choices(t)))
		assert.Contains(t, h.stderr.String(), "some items still need action")
		assert.False(t, converted(t, db))
	})
	t.Run("resolvers file exists", func(t *testing.T) {
		db := convertDB(t)
		h := newConvertHarness(t, db)
		out := filepath.Join(t.TempDir(), "r.json")
		require.NoError(t, os.WriteFile(out, []byte("keep me"), 0o600))
		args := []string{"-apply", "-quarantined", "lift", "-later-deletes", "delete", "-resolvers-out", out, "-accept-manual-reports"}
		assert.Equal(t, 1, convert(h.e, args))
		raw, err := os.ReadFile(out)
		require.NoError(t, err)
		assert.Equal(t, "keep me", string(raw), "前のファイルを上書きしない")
		assert.Contains(t, h.stderr.String(), "pass another path")
		assert.Equal(t, 5, unprocessed(t, db), "書き出せなければ何も変えない")
	})
	t.Run("resolvers file cannot be written", func(t *testing.T) {
		db := convertDB(t)
		h := newConvertHarness(t, db)
		h.e.createFile = func(string) (io.WriteCloser, error) { return failingWriter{}, nil }
		assert.Equal(t, 1, convert(h.e, choices(t)))
		assert.Equal(t, 5, unprocessed(t, db))
	})
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }
func (failingWriter) Close() error              { return nil }

func TestConvert_NotReady(t *testing.T) {
	t.Run("not migrated by Elythia", func(t *testing.T) {
		db := convertDB(t)
		require.NoError(t, db.Exec(`DELETE FROM schema_migrations`).Error)
		h := newConvertHarness(t, db)
		assert.Equal(t, 1, convert(h.e, nil))
		assert.Contains(t, h.stderr.String(), "run `elythia migrate -allow-cherrypick` first")
	})
	t.Run("not CherryPick", func(t *testing.T) {
		db := convertDB(t)
		require.NoError(t, db.Exec(`DROP TABLE migrations`).Error)
		h := newConvertHarness(t, db)
		assert.Equal(t, 0, convert(h.e, nil))
		assert.Contains(t, h.stdout.String(), "did not come from CherryPick")
	})
	t.Run("unsupported version", func(t *testing.T) {
		db := convertDB(t)
		require.NoError(t, db.Exec(`DELETE FROM migrations WHERE name = 'ApprovalSignup1768893580518'`).Error)
		h := newConvertHarness(t, db)
		assert.Equal(t, ExitNeedsAction, convert(h.e, nil))
		assert.Contains(t, h.stdout.String(), "other than 4.18.0-beta.1")
	})
	t.Run("nothing to do", func(t *testing.T) {
		db := convertDB(t)
		for _, sql := range []string{
			`DELETE FROM note`, `DELETE FROM abuse_report_resolver`, `UPDATE instance SET "quarantineLimited" = false`,
			`UPDATE meta SET "repositoryUrl" = NULL, "feedbackUrl" = NULL`, `UPDATE "user" SET approved = true`,
		} {
			require.NoError(t, db.Exec(sql).Error)
		}
		h := newConvertHarness(t, db)
		require.Equal(t, 0, convert(h.e, []string{"-apply"}), h.stderr.String())
		assert.Contains(t, h.stdout.String(), "nothing needs action")
		assert.True(t, converted(t, db), "要対応が無くても印を付ける")
	})
}

func TestConvert_ArgumentsAndSetup(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-pending-users", "ban"}, `invalid -pending-users "ban"`},
		{[]string{"-unapproved-members", "ban"}, `invalid -unapproved-members "ban"`},
		{[]string{"-quarantined", "ignore"}, `invalid -quarantined "ignore"`},
		{[]string{"-later-deletes", "later"}, `invalid -later-deletes "later"`},
		{[]string{"extra"}, `unexpected argument "extra"`},
	} {
		h := newConvertHarness(t, nil)
		assert.Equal(t, 2, convert(h.e, tc.args), tc.args)
		assert.Contains(t, h.stderr.String(), tc.want)
	}
	h := newConvertHarness(t, nil)
	assert.Equal(t, 2, convert(h.e, []string{"-nope"}))

	h = newConvertHarness(t, nil)
	h.e.loadConfig = func(string) (*config.Config, error) { return nil, errors.New("no file") }
	assert.Equal(t, 1, convert(h.e, nil))
	assert.Contains(t, h.stderr.String(), "no file")

	h = newConvertHarness(t, nil)
	h.e.openDB = func(*config.Config) (*gorm.DB, func(), error) { return nil, nil, errors.New("refused") }
	assert.Equal(t, 1, convert(h.e, nil))
	assert.Contains(t, h.stderr.String(), "refused")
}

// 本物の組み立ては、繋げなければエラーを返す。
func TestNewServerDeleter_Unreachable(t *testing.T) {
	cfg := &config.Config{}
	cfg.DB.Host = "127.0.0.1"
	cfg.DB.Port = 1
	cfg.DB.User = "x"
	cfg.DB.DB = "x"
	_, err := newServerDeleter(cfg)
	assert.Error(t, err)
	_, _, err = openDB(cfg)
	assert.Error(t, err)
	assert.Equal(t, 2, Convert([]string{"-nope"}))
}

// DB が途中で使えなくなったら、何も変えずに 1 で終わる。
func TestConvert_DatabaseGone(t *testing.T) {
	db := convertDB(t)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	gone, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{})
	require.NoError(t, err)
	goneSQL, err := gone.DB()
	require.NoError(t, err)
	h := newConvertHarness(t, gone)
	require.NoError(t, goneSQL.Close())
	assert.Equal(t, 1, convert(h.e, nil))
}

// 止めるときに捨てた処理があれば、Close は失敗する (印を付けない)。
type fakeMigrationServer struct {
	dropped int64
	waited  bool
}

func (f *fakeMigrationServer) DeleteMigratedAccount(string) error { return nil }
func (f *fakeMigrationServer) DeleteMigratedNote(string) error    { return nil }
func (f *fakeMigrationServer) WaitNoteDeletes(context.Context) error {
	f.waited = true
	return nil
}
func (f *fakeMigrationServer) Shutdown(context.Context) error { return nil }
func (f *fakeMigrationServer) BackgroundDropped() int64       { return f.dropped }

func TestServerDeleter_DroppedWork(t *testing.T) {
	srv := &fakeMigrationServer{dropped: 2}
	d := &serverDeleter{srv: srv, closeDB: func() {}, closeRedis: func() error { return nil }, stopTimeout: time.Second}
	require.NoError(t, d.Wait(t.Context()))
	assert.True(t, srv.waited)
	err := d.Close()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "2 piece(s) of background work were given up while stopping")

	srv.dropped = 0
	assert.NoError(t, d.Close())
}

// 組み立てた本体を包む serverDeleter は、本体の入口へそのまま渡し、Close で止める。
func TestServerDeleter(t *testing.T) {
	db, err := testutil.OpenTestDB()
	require.NoError(t, err)
	testutil.ApplyMigrations(db)

	mr := miniredis.RunT(t)
	port, err := strconv.Atoi(mr.Port())
	require.NoError(t, err)
	opts := config.RedisOptions{Host: mr.Host(), Port: port}
	cfg := &config.Config{
		URL: "http://example.test", Host: "example.test", Hostname: "example.test",
		Scheme: "http", WsScheme: "ws", ID: "aidx", TestMode: true,
		Redis: opts, RedisForPubsub: opts, RedisForJobQueue: opts, RedisForTimelines: opts, RedisForReactions: opts,
	}
	closedDB := 0
	open := func(*config.Config) (*gorm.DB, func(), error) { return db, func() { closedDB++ }, nil }

	d, err := buildServerDeleter(cfg, open, cache.NewRedisClients, server.New)
	require.NoError(t, err)
	assert.Error(t, d.DeleteMigratedAccount("cherrypick-nobody"))
	assert.Error(t, d.DeleteMigratedNote("cherrypick-nonote"))
	assert.NoError(t, d.Wait(t.Context()))
	assert.NoError(t, d.Close())
	assert.Equal(t, 1, closedDB)

	t.Run("redis fails", func(t *testing.T) {
		closedDB = 0
		_, err := buildServerDeleter(cfg, open, func(*config.Config) (*cache.RedisClients, error) {
			return nil, errors.New("no redis")
		}, server.New)
		assert.ErrorContains(t, err, "no redis")
		assert.Equal(t, 1, closedDB, "DB を閉じる")
	})
	t.Run("construction fails", func(t *testing.T) {
		closedDB = 0
		_, err := buildServerDeleter(cfg, open, cache.NewRedisClients,
			func(*config.Config, *gorm.DB, *cache.RedisClients) (*server.Server, error) {
				return nil, errors.New("bad wiring")
			})
		assert.ErrorContains(t, err, "bad wiring")
		assert.Equal(t, 1, closedDB)
	})
	t.Run("database fails", func(t *testing.T) {
		_, err := buildServerDeleter(cfg, func(*config.Config) (*gorm.DB, func(), error) {
			return nil, nil, errors.New("refused")
		}, cache.NewRedisClients, server.New)
		assert.ErrorContains(t, err, "refused")
	})
}

func TestMissingChoices(t *testing.T) {
	r := maintenance.CherryPickReport{AbuseReportResolvers: 1}
	assert.Len(t, missingChoices(r, convertChoices{resolversOut: "r.json"}), 1, "同意のフラグが要る")
	assert.Len(t, missingChoices(r, convertChoices{acceptReports: true}), 1, "書き出し先が要る")
	assert.Empty(t, missingChoices(r, convertChoices{resolversOut: "r.json", acceptReports: true}))
	assert.Empty(t, missingChoices(maintenance.CherryPickReport{}, convertChoices{}))
	assert.Len(t, missingChoices(maintenance.CherryPickReport{UnapprovedMembers: 1}, convertChoices{}), 1)
	assert.Empty(t, missingChoices(maintenance.CherryPickReport{UnapprovedMembers: 1}, convertChoices{unapproved: "approve"}))
}
