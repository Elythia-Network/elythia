package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAlterDatabaseSetSQL(t *testing.T) {
	cases := map[string]string{
		"work_mem=8MB":                      `ALTER DATABASE "db" SET work_mem TO '8MB'`,
		"statement_timeout=0":               `ALTER DATABASE "db" SET statement_timeout TO '0'`,
		"default_text_search_config=it's":   `ALTER DATABASE "db" SET default_text_search_config TO 'it''s'`,
		`search_path="$user", public`:       `ALTER DATABASE "db" SET search_path TO '$user', 'public'`,
		`search_path="a,b", "q""t", public`: `ALTER DATABASE "db" SET search_path TO 'a,b', 'q"t', 'public'`,
		"my.custom_1=x=y":                   `ALTER DATABASE "db" SET my.custom_1 TO 'x=y'`,
		"Search_Path=a":                     `ALTER DATABASE "db" SET Search_Path TO 'a'`,
	}
	for in, want := range cases {
		got, err := alterDatabaseSetSQL("db", in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"", "noequals", "=v", "1abc=v", "a;DROP=v", "a b=v", ".a=v", "a=\x00"} {
		_, err := alterDatabaseSetSQL("db", bad)
		assert.Error(t, err, bad)
	}
}

func TestReadMetaRejectsBadMetadata(t *testing.T) {
	ctx := context.Background()
	id := rsGenID
	good := Meta{FormatVersion: MetaFormatVersion, ID: id, DumpFile: DumpFile}
	cases := map[string]func(*Meta){
		"format version": func(m *Meta) { m.FormatVersion = 2 },
		"other id":       func(m *Meta) { m.ID = "20261010T000000Z" },
		"dump path":      func(m *Meta) { m.DumpFile = "../../etc/passwd" },
	}
	for name, mutate := range cases {
		s := newRSStorage()
		m := good
		mutate(&m)
		s.putJSON(t, Key(id, MetaFile), m)
		_, err := (&Restorer{Storage: s}).readMeta(ctx, id)
		assert.Error(t, err, name)
	}
	s := newRSStorage()
	_, err := (&Restorer{Storage: s}).readMeta(ctx, id)
	assert.ErrorContains(t, err, "incomplete or missing")
	s.getErr[Key(id, MetaFile)] = errors.New("boom")
	_, err = (&Restorer{Storage: s}).readMeta(ctx, id)
	assert.ErrorContains(t, err, "boom")
	s = newRSStorage()
	s.objs[Key(id, MetaFile)] = []byte("{")
	_, err = (&Restorer{Storage: s}).readMeta(ctx, id)
	assert.Error(t, err)
	s.putJSON(t, Key(id, MetaFile), good)
	_, err = (&Restorer{Storage: s}).readMeta(ctx, id)
	assert.NoError(t, err)
}

func TestResolveLatestPicksNewestVerified(t *testing.T) {
	ctx := context.Background()
	s := newRSStorage()
	r := &Restorer{Storage: s}
	put := func(id string, verify *VerifyResult) {
		s.putJSON(t, Key(id, MetaFile), Meta{FormatVersion: MetaFormatVersion, ID: id, DumpFile: DumpFile})
		s.objs[Key(id, DumpFile)] = []byte("dump")
		if verify != nil {
			s.putJSON(t, Key(id, VerifyFile), verify)
		}
	}
	_, _, err := r.resolveID(ctx, LatestGeneration)
	assert.ErrorContains(t, err, "no verified generation")

	put("20261001T000000Z", &VerifyResult{ID: "20261001T000000Z", OK: true})
	put("20261002T000000Z", &VerifyResult{ID: "20261002T000000Z", OK: true})
	put("20261003T000000Z", &VerifyResult{ID: "20261003T000000Z", OK: false})
	put("20261004T000000Z", nil)
	// 別の世代の結果を置き間違えたものは、検証済みとして扱わない。
	put("20261005T000000Z", &VerifyResult{ID: "20261002T000000Z", OK: true})
	// meta.json の無い世代 (取っている途中) は候補にしない。
	s.putJSON(t, Key("20261006T000000Z", VerifyFile), VerifyResult{ID: "20261006T000000Z", OK: true})
	// dump の無い世代 (Generation.Complete が false) も候補にしない。
	put("20261007T000000Z", &VerifyResult{ID: "20261007T000000Z", OK: true})
	delete(s.objs, Key("20261007T000000Z", DumpFile))

	id, verified, err := r.resolveID(ctx, LatestGeneration)
	require.NoError(t, err)
	assert.Equal(t, "20261002T000000Z", id)
	assert.True(t, verified)

	id, verified, err = r.resolveID(ctx, "20261004T000000Z")
	require.NoError(t, err)
	assert.Equal(t, "20261004T000000Z", id)
	assert.False(t, verified)
	_, verified, err = r.resolveID(ctx, "20261005T000000Z")
	require.NoError(t, err)
	assert.False(t, verified)

	_, _, err = r.resolveID(ctx, "../x")
	assert.ErrorContains(t, err, "invalid generation id")

	// verify.json が読めなければ、古い世代へ進まずに止まる。
	s.getErr[Key("20261002T000000Z", VerifyFile)] = errors.New("boom")
	_, _, err = r.resolveID(ctx, "20261002T000000Z")
	assert.ErrorContains(t, err, "boom")
	_, _, err = r.resolveID(ctx, LatestGeneration)
	assert.ErrorContains(t, err, "cannot read "+Key("20261002T000000Z", VerifyFile))
	delete(s.getErr, Key("20261002T000000Z", VerifyFile))

	// meta.json が読めない世代 (知らない形式など) があっても、止まる。
	s.putJSON(t, Key("20261008T000000Z", MetaFile), Meta{FormatVersion: MetaFormatVersion + 1, ID: "20261008T000000Z"})
	_, _, err = r.resolveID(ctx, LatestGeneration)
	assert.ErrorContains(t, err, "cannot read generation 20261008T000000Z")
}

type rsListErrStorage struct{ *rsStorage }

func (rsListErrStorage) List(context.Context, string) ([]ObjectInfo, error) {
	return nil, errors.New("list failed")
}

func TestNewestGeneration(t *testing.T) {
	ctx := context.Background()
	s := newRSStorage()
	got, err := NewestGeneration(ctx, s)
	require.NoError(t, err)
	assert.Empty(t, got)
	put := func(id string) {
		s.putJSON(t, Key(id, MetaFile), Meta{FormatVersion: MetaFormatVersion, ID: id, DumpFile: DumpFile})
		s.objs[Key(id, DumpFile)] = []byte("dump")
	}
	put("20261001T000000Z")
	put("20261002T000000Z")
	// meta.json の無い世代 (途中で止まった) は数えない。
	s.objs[Key("20261003T000000Z", DumpFile)] = []byte("dump")
	got, err = NewestGeneration(ctx, s)
	require.NoError(t, err)
	assert.Equal(t, "20261002T000000Z", got)
	// dump が無い世代は数えず、読めない meta.json の世代は数える。
	s.putJSON(t, Key("20261004T000000Z", MetaFile), Meta{FormatVersion: MetaFormatVersion, ID: "20261004T000000Z", DumpFile: DumpFile})
	got, _ = NewestGeneration(ctx, s)
	assert.Equal(t, "20261002T000000Z", got)
	s.putJSON(t, Key("20261005T000000Z", MetaFile), Meta{FormatVersion: MetaFormatVersion + 1, ID: "20261005T000000Z"})
	got, _ = NewestGeneration(ctx, s)
	assert.Equal(t, "20261005T000000Z", got)

	_, err = NewestGeneration(ctx, rsListErrStorage{newRSStorage()})
	assert.ErrorContains(t, err, "list failed")
	id, verified, err := (&Restorer{Storage: s}).ResolveID(ctx, "20261001T000000Z")
	require.NoError(t, err)
	assert.Equal(t, "20261001T000000Z", id)
	assert.False(t, verified)
}

func TestResolveLatestListError(t *testing.T) {
	_, _, err := (&Restorer{Storage: rsListErrStorage{newRSStorage()}}).resolveID(context.Background(), LatestGeneration)
	assert.ErrorContains(t, err, "list failed")
}

func TestCheckMigrations(t *testing.T) {
	local := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(local, "900002_x.up.sql"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(local, "900002_x.down.sql"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(local, "README"), nil, 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(local, "sub"), 0o700))
	r := &Restorer{CoreMigrationsDir: rsCoreMigrations, LocalMigrationsDir: local}
	core, _, err := latestMigration(rsCoreMigrations)
	require.NoError(t, err)

	ok := []MigrationState{
		{Table: CoreMigrationsTable, Version: int64(core)},
		{Table: LocalMigrationsTable, Version: 900002},
	}
	assert.NoError(t, r.checkMigrations(Meta{Migrations: ok}))
	assert.NoError(t, r.checkMigrations(Meta{Migrations: []MigrationState{{Table: LocalMigrationsTable, Missing: true, Version: 999999}}}))

	err = r.checkMigrations(Meta{Migrations: []MigrationState{{Table: LocalMigrationsTable, Version: 900003}}})
	assert.ErrorIs(t, err, ErrRestoreMigrationTooNew)
	// 管理表が空 (行が無い) のバックアップは、戻した後に全部を当てるので止めない。
	assert.NoError(t, r.checkMigrations(Meta{Migrations: []MigrationState{{Table: CoreMigrationsTable, Version: NilMigrationVersion}}}))
	err = r.checkMigrations(Meta{Migrations: []MigrationState{{Table: CoreMigrationsTable, Version: -2}}})
	assert.ErrorContains(t, err, "invalid version -2")
	assert.NotErrorIs(t, err, ErrRestoreMigrationTooNew)
	err = r.checkMigrations(Meta{Migrations: []MigrationState{{Table: CoreMigrationsTable, Version: 3, Dirty: true}}})
	assert.ErrorContains(t, err, "dirty")
	err = r.checkMigrations(Meta{Migrations: []MigrationState{{Table: "other", Version: 1}}})
	assert.ErrorContains(t, err, "unknown migration table")

	err = (&Restorer{CoreMigrationsDir: t.TempDir()}).checkMigrations(Meta{})
	assert.ErrorContains(t, err, "no migrations found")
	file := filepath.Join(local, "README")
	_, _, err = latestMigration(file)
	assert.Error(t, err)
	assert.Error(t, (&Restorer{CoreMigrationsDir: file}).checkMigrations(Meta{}))
	assert.Error(t, (&Restorer{CoreMigrationsDir: rsCoreMigrations, LocalMigrationsDir: file}).checkMigrations(Meta{}))
}

func TestRestoreNames(t *testing.T) {
	at := time.Date(2026, 10, 10, 21, 4, 5, 0, time.FixedZone("JST", 9*3600))
	restored, before, err := RestoreNames("misskey", at)
	require.NoError(t, err)
	assert.Equal(t, "misskey_restore_20261010120405", restored)
	assert.Equal(t, "misskey_before_restore_20261010120405", before)
	_, _, err = RestoreNames(strings.Repeat("a", 63-len("_before_restore_20261010120405")), at)
	assert.NoError(t, err)
	_, _, err = RestoreNames(strings.Repeat("a", 64-len("_before_restore_20261010120405")), at)
	assert.ErrorContains(t, err, "too long")
}

func TestCreateDatabaseSQL(t *testing.T) {
	base := DatabaseLocale{Encoding: "UTF8", Collate: "en_US.utf8", Ctype: "en_US.utf8"}
	cases := map[string]string{
		"":        ``,
		"libc":    ` LOCALE_PROVIDER libc`,
		"icu":     ` LOCALE_PROVIDER icu ICU_LOCALE 'und'`,
		"builtin": ` LOCALE_PROVIDER builtin BUILTIN_LOCALE 'C.UTF-8'`,
	}
	locales := map[string]string{"icu": "und", "builtin": "C.UTF-8"}
	for p, want := range cases {
		l := base
		l.Provider, l.Locale = p, locales[p]
		assert.Equal(t, `CREATE DATABASE "x" WITH TEMPLATE template0 ENCODING 'UTF8' LC_COLLATE 'en_US.utf8' LC_CTYPE 'en_US.utf8'`+want+` OWNER "app"`,
			createDatabaseSQL("x", l, "app"), p)
	}
	l := base
	l.Provider = "icu"
	assert.NotContains(t, createDatabaseSQL("x", l, "app"), "ICU_LOCALE")
	l.Provider = "builtin"
	assert.NotContains(t, createDatabaseSQL("x", l, "app"), "BUILTIN_LOCALE")
	assert.NotContains(t, createDatabaseSQL("x", base, ""), "OWNER")
}

func TestSwapLocale(t *testing.T) {
	var out bytes.Buffer
	r := &Restorer{Out: &out}
	current := restoreDatabase{locale: DatabaseLocale{Encoding: "UTF8", Collate: "C", Ctype: "C", Provider: "libc"}}
	// バックアップに記録が無ければ、今の DB の値で作る。
	assert.Equal(t, current.locale, r.swapLocale(Meta{}, "db", current))
	assert.Empty(t, out.String())
	// 16 より前の記録 (provider が空) は libc として比べ、同じなら何も出さない。
	old := DatabaseLocale{Encoding: "UTF8", Collate: "C", Ctype: "C"}
	assert.Equal(t, old, r.swapLocale(Meta{DatabaseLocale: old}, "db", current))
	assert.Empty(t, out.String())
	// 違えばバックアップの値で作り、そのことを出力に残す。
	want := DatabaseLocale{Encoding: "UTF8", Collate: "en_US.utf8", Ctype: "en_US.utf8", Provider: "libc"}
	assert.Equal(t, want, r.swapLocale(Meta{DatabaseLocale: want}, "db", current))
	assert.Contains(t, out.String(), "LC_COLLATE en_US.utf8")
	assert.Contains(t, out.String(), "the current database db has ENCODING UTF8 LC_COLLATE C")
	assert.Equal(t, "ENCODING UTF8 LC_COLLATE C LC_CTYPE C LOCALE_PROVIDER icu LOCALE und",
		describeLocale(DatabaseLocale{Encoding: "UTF8", Collate: "C", Ctype: "C", Provider: "icu", Locale: "und"}))
}

func TestRestoreRejectsBadOptions(t *testing.T) {
	r := &Restorer{Storage: newRSStorage()}
	ctx := context.Background()
	_, err := r.Restore(ctx, RestoreOptions{Mode: "copy", Database: "a", Confirm: "a"})
	assert.ErrorContains(t, err, "unknown restore mode")
	_, err = r.Restore(ctx, RestoreOptions{Mode: RestoreSwap})
	assert.ErrorContains(t, err, "no database")
	_, err = r.Restore(ctx, RestoreOptions{Mode: RestoreSwap, Database: "a", Confirm: "a", ID: "nope"})
	assert.ErrorContains(t, err, "invalid generation id")
	err = r.RestoreInto(ctx, &RestorePlan{}, RestoreOptions{Mode: RestoreEmpty, Database: "a"}, "b")
	assert.ErrorContains(t, err, "empty mode restores into")
}

// TestFetchDump covers the checks on the downloaded dump: size, both
// hashes, decryption and the cleanup of the temporary file.
func TestFetchDump(t *testing.T) {
	ctx := context.Background()
	plain := bytes.Repeat([]byte("PGDMP-data "), 1000)
	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	other, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	var enc bytes.Buffer
	w, err := age.Encrypt(&enc, id.Recipient())
	require.NoError(t, err)
	_, err = w.Write(plain)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	setup := func(stored []byte, encrypted bool) (*Restorer, Meta, string) {
		s := newRSStorage()
		tmp := t.TempDir()
		m := Meta{ID: rsGenID, DumpFile: DumpFile, PlainSHA256: rsSHA(plain), DumpSHA256: rsSHA(stored), DumpSize: int64(len(stored))}
		if encrypted {
			m.DumpFile, m.Encrypted, m.Encryption = DumpFileAge, true, "age"
		}
		s.objs[Key(rsGenID, m.DumpFile)] = stored
		return &Restorer{Storage: s, TempDir: tmp, Identities: []age.Identity{id}}, m, tmp
	}
	assertNoTemp := func(dir string) {
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		assert.Empty(t, entries)
	}

	for name, encrypted := range map[string]bool{"plain": false, "age": true} {
		stored := plain
		if encrypted {
			stored = enc.Bytes()
		}
		r, m, tmp := setup(stored, encrypted)
		path, cleanup, err := r.fetchDump(ctx, m)
		require.NoError(t, err, name)
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, plain, got, name)
		cleanup()
		assertNoTemp(tmp)
	}

	fails := map[string]func(*Restorer, *Meta){
		"size":           func(_ *Restorer, m *Meta) { m.DumpSize++ },
		"stored hash":    func(_ *Restorer, m *Meta) { m.DumpSHA256 = rsSHA([]byte("x")) },
		"plain hash":     func(_ *Restorer, m *Meta) { m.PlainSHA256 = rsSHA([]byte("x")) },
		"wrong identity": func(r *Restorer, _ *Meta) { r.Identities = []age.Identity{other} },
		"no identity":    func(r *Restorer, _ *Meta) { r.Identities = nil },
		"scheme":         func(_ *Restorer, m *Meta) { m.Encryption = "gpg" },
		"missing":        func(r *Restorer, _ *Meta) { r.Storage = newRSStorage() },
	}
	for name, mutate := range fails {
		r, m, tmp := setup(enc.Bytes(), true)
		mutate(r, &m)
		_, _, err := r.fetchDump(ctx, m)
		assert.Error(t, err, name)
		switch name {
		case "size", "stored hash", "plain hash":
			assert.ErrorIs(t, err, ErrRestoreChecksum, name)
		}
		assertNoTemp(tmp)
	}

	// 途中で切れた暗号文は、復号の段で落ちる。
	r, m, tmp := setup(enc.Bytes()[:enc.Len()-10], true)
	_, _, err = r.fetchDump(ctx, m)
	assert.Error(t, err)
	assertNoTemp(tmp)
	// 暗号文の後ろに余計なバイトがあっても、通さない。
	r, m, tmp = setup(enc.Bytes(), true)
	r.Storage.(*rsStorage).objs[Key(rsGenID, DumpFileAge)] = append(append([]byte{}, enc.Bytes()...), 'x')
	_, _, err = r.fetchDump(ctx, m)
	assert.Error(t, err)
	// 平文の後ろに余計なバイトがあれば、大きさとハッシュで落ちる。
	r, m, tmp2 := setup(plain, false)
	r.Storage.(*rsStorage).objs[Key(rsGenID, DumpFile)] = append(append([]byte{}, plain...), 'x')
	_, _, err = r.fetchDump(ctx, m)
	assert.ErrorIs(t, err, ErrRestoreChecksum)
	assertNoTemp(tmp2)
	assertNoTemp(tmp)
	// 一時ファイルを作れない。
	r, m, _ = setup(plain, false)
	r.TempDir = filepath.Join(t.TempDir(), "missing")
	_, _, err = r.fetchDump(ctx, m)
	assert.ErrorContains(t, err, "temporary file")
}

func TestPgRestoreReportsStderr(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "d")
	require.NoError(t, os.WriteFile(dump, []byte("x"), 0o600))
	var gotArgs, gotEnv []string
	var gotProgram string
	r := &Restorer{
		Conn: RestoreConn{Tool: func(db string) (string, []string) { return "dbname=" + db, []string{"PGPASSWORD=p"} }},
		Exec: func(_ context.Context, program string, args, env []string, stdin io.Reader, _, stderr io.Writer) error {
			gotProgram, gotArgs, gotEnv = program, args, env
			b, _ := io.ReadAll(stdin)
			assert.Equal(t, "x", string(b))
			_, _ = stderr.Write(bytes.Repeat([]byte("e"), 70<<10))
			return errors.New("exit status 1")
		},
	}
	err := r.pgRestore(context.Background(), "target", dump)
	require.Error(t, err)
	assert.Equal(t, "pg_restore", gotProgram)
	assert.Equal(t, []string{"--exit-on-error", "--single-transaction", "--no-owner", "--no-privileges", "--dbname", "dbname=target"}, gotArgs)
	assert.Equal(t, []string{"PGPASSWORD=p"}, gotEnv)
	// stderr は先頭の 64 KiB だけを残す。
	assert.Len(t, strings.TrimPrefix(err.Error(), "backup: pg_restore: exit status 1: "), 64<<10)

	r.PgRestore = "/usr/local/bin/pg_restore"
	_ = r.pgRestore(context.Background(), "target", dump)
	assert.Equal(t, "/usr/local/bin/pg_restore", gotProgram)
	assert.ErrorContains(t, r.pgRestore(context.Background(), "target", dump+".missing"), "open dump")
}

func TestExecLocal(t *testing.T) {
	var out bytes.Buffer
	err := ExecLocal(context.Background(), "sh", []string{"-c", `cat; printf "$X"`}, []string{"X=env"}, strings.NewReader("in-"), &out, io.Discard)
	require.NoError(t, err)
	assert.Equal(t, "in-env", out.String())
}

func TestRedisGlobAndProtection(t *testing.T) {
	assert.Equal(t, `a\*b\?c\[d\]e\\f:`, escapeGlob(`a*b?c[d]e\f:`))
	for _, k := range []string{
		"passwordguard:failures:u1", "passwordguard:failures:u1:10.0.0.0/24",
		"mk:2fa:totp:used:u1:123456", "mk:ap:inbox:seen:abc", "mk:signupform:nonce:n",
		"host:elythia:maintenance", "host:elythia:maintenance:proc:1", "elythia:maintenance",
	} {
		assert.True(t, protectedKey(k, "host:"), k)
	}
	for _, k := range []string{"host:list:homeTimeline:u1", "antennaTimeline:a", "other:elythia:maintenance"} {
		assert.False(t, protectedKey(k, "host:"), k)
	}
}

// TestCleanRedisAfterRestore checks which keys go and which stay, on
// separate default / timelines / job queue Redis databases.
func TestCleanRedisAfterRestore(t *testing.T) {
	base := rsStartRedis(t)
	addr := base.Options().Addr
	client := func(db int) *redis.Client {
		c := redis.NewClient(&redis.Options{Addr: addr, DB: db})
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	def, tl, jq := client(0), client(1), client(2)
	ctx := context.Background()
	const p = "elythia.example:"
	set := func(c *redis.Client, keys ...string) {
		for _, k := range keys {
			require.NoError(t, c.Set(ctx, k, "1", 0).Err())
		}
	}
	gone := map[*redis.Client][]string{
		tl: {p + "list:homeTimeline:u1", p + "list:globalTimeline", p + "ephNote:n", p + "ephNoteURI:x",
			p + "ephUser:u", p + "ephUserURI:x", p + "ephFile:f"},
		def: {p + "cleanRemoteNotes:cursor", "antennaTimeline:a", "featuredGlobalNotesRanking:1",
			"featuredInChannelNotesRanking:c:1", "featuredPerUserNotesRanking:u:1", "featuredGalleryPostsRanking:1",
			"reaction-buffer:n", "userSwSubscriptions:u", "reversi:fed:session:s", "reversi:game:turnTimer:g:1",
			"reversi:matchAny", "bubbleVersus:match:m", "apFederationRule:hits:1", "oauth:txn:t"},
		jq: {"bull:deliver:wait", "bull:deliver:1", "bull:deliver:delayed", "bull:deliver:id"},
	}
	kept := map[*redis.Client][]string{
		// timelines の DB では、default の接頭辞の key も timelines の接頭辞の外の key も触らない。
		tl: {"other.example:list:homeTimeline:u1", p + "notificationTimeline:u1"},
		def: {p + "notificationTimeline:u1", p + "latestReadNotification:u1", p + "url-preview:x",
			p + "elythia:maintenance", p + "elythia:maintenance:proc:1",
			"passwordguard:failures:u1", "mk:2fa:totp:used:u1:1", "mk:ap:inbox:seen:a", "mk:signupform:nonce:n",
			"limit:u1:notes/create", "apDeliveryBreaker:remote.example", "reversi:federation:version:remote.example",
			// 接頭辞の付く key は、default の DB でも list:* を消さない (timelines の DB の担当)。
			p + "list:homeTimeline:u1"},
		jq: {"bull:deliver:meta", "bull:deliver:repeat", "bull:inbox:wait", "bull:inbox:1", "bull:queues"},
	}
	for c, keys := range gone {
		set(c, keys...)
	}
	for c, keys := range kept {
		set(c, keys...)
	}

	res, err := CleanRedisAfterRestore(ctx, RedisCleanupTargets{
		Default:   RedisCleanupTarget{Client: def, Prefix: p},
		Timelines: RedisCleanupTarget{Client: tl, Prefix: p},
		JobQueue:  RedisCleanupTarget{Client: jq},
	})
	require.NoError(t, err)
	var total int64
	for c, keys := range gone {
		for _, k := range keys {
			assert.Equal(t, int64(0), c.Exists(ctx, k).Val(), k)
		}
		total += int64(len(keys))
	}
	for c, keys := range kept {
		for _, k := range keys {
			assert.Equal(t, int64(1), c.Exists(ctx, k).Val(), k)
		}
	}
	assert.Equal(t, total, res.Total())

	// 未配線の client は飛ばす。
	_, err = CleanRedisAfterRestore(ctx, RedisCleanupTargets{})
	require.NoError(t, err)
	// SCAN に失敗したら止まる。
	require.NoError(t, def.Close())
	_, err = CleanRedisAfterRestore(ctx, RedisCleanupTargets{Default: RedisCleanupTarget{Client: def}})
	assert.Error(t, err)
}

// TestCleanRedisManyKeys crosses SCAN pages.
func TestCleanRedisManyKeys(t *testing.T) {
	rdb := rsStartRedis(t)
	ctx := context.Background()
	pipe := rdb.Pipeline()
	for i := range 3000 {
		pipe.Set(ctx, "list:homeTimeline:"+strconv.Itoa(i), "1", 0)
	}
	_, err := pipe.Exec(ctx)
	require.NoError(t, err)
	res, err := CleanRedisAfterRestore(ctx, RedisCleanupTargets{Timelines: RedisCleanupTarget{Client: rdb}})
	require.NoError(t, err)
	assert.Equal(t, int64(3000), res.Total())
	assert.Equal(t, int64(0), rdb.DBSize(ctx).Val())
}

// TestRestoreStepsReportConnectionFailures runs every step against a server
// that does not answer.
func TestRestoreStepsReportConnectionFailures(t *testing.T) {
	const secret = "s3cret-pass"
	dead := RestoreConn{
		SQL:     func(db string) string { return "postgres://u:" + secret + "@127.0.0.1:1/" + db + "?connect_timeout=2" },
		Migrate: func(db string) string { return "pgx5://u:" + secret + "@127.0.0.1:1/" + db + "?connect_timeout=2" },
	}
	plain := []byte("dump")
	s := newRSStorage()
	s.objs[Key(rsGenID, DumpFile)] = plain
	meta := Meta{ID: rsGenID, DumpFile: DumpFile, DumpSHA256: rsSHA(plain), DumpSize: int64(len(plain))}
	r := &Restorer{Storage: s, Conn: dead, CoreMigrationsDir: rsCoreMigrations, TempDir: t.TempDir()}
	ctx := context.Background()
	swap := RestoreOptions{Mode: RestoreSwap, Database: "db", Confirm: "db"}
	empty := RestoreOptions{Mode: RestoreEmpty, Database: "db", Confirm: "db"}
	errs := map[string]error{
		"CheckTarget swap":   r.CheckTarget(ctx, swap),
		"CheckTarget empty":  r.CheckTarget(ctx, empty),
		"CheckNoConnections": r.CheckNoConnections(ctx, "", "db"),
		"CheckCompatibility": r.CheckCompatibility(ctx, &RestorePlan{Meta: meta}, swap),
		"RestoreInto swap":   r.RestoreInto(ctx, &RestorePlan{Meta: meta}, swap, "db_restore"),
		"VerifyRestored":     r.VerifyRestored(ctx, "db", meta),
		"ReplaceDatabase":    r.ReplaceDatabase(ctx, "", "db", "a", "b"),
		"DropDatabase":       r.DropDatabase(ctx, "", "db"),
	}
	// 繋がらなければ、入れ替わったかどうかは分からない。
	assert.Equal(t, swapUnknown, r.swapOutcome("", "db_restore", "db_before"))
	_, errs["ApplyDatabaseSettings"] = r.ApplyDatabaseSettings(ctx, "db", meta)
	_, errs["Migrate"] = r.Migrate("db")
	for name, err := range errs {
		require.Error(t, err, name)
		assert.NotContains(t, err.Error(), secret, name)
	}
	assert.Contains(t, errs["CheckTarget empty"].Error(), "create the empty database first")
}

func TestRedactURL(t *testing.T) {
	err := redactURL(errors.New("open pgx5://u:p@h/db failed"), "pgx5://u:p@h/db")
	assert.Equal(t, "open <database url> failed", err.Error())
}

func TestCheckRestoredMigrationDirErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	_, err := CheckRestored(context.Background(), RestoreCheckDeps{CoreMigrationsDir: file})
	assert.Error(t, err)
	_, err = CheckRestored(context.Background(), RestoreCheckDeps{CoreMigrationsDir: rsCoreMigrations, LocalMigrationsDir: file})
	assert.Error(t, err)
	// DB も Redis も無ければ、skip だけで失敗にはならない。
	report, err := CheckRestored(context.Background(), RestoreCheckDeps{CoreMigrationsDir: rsCoreMigrations})
	require.NoError(t, err)
	assert.True(t, report.OK)
}

func TestParseToolMajor(t *testing.T) {
	for in, want := range map[string]int{
		"pg_restore (PostgreSQL) 18.0\n":                 18,
		"pg_restore (PostgreSQL) 16.10":                  16,
		"pg_restore (PostgreSQL) 19devel":                19,
		"pg_restore (PostgreSQL) 9.6.24":                 9,
		"pg_restore (PostgreSQL) 18.1 (Debian 18.1-1)\n": 18,
		"pg_restore (PostgreSQL) 0.1":                    0,
	} {
		got, err := parseToolMajor(in)
		if want == 0 {
			assert.Error(t, err, in)
			continue
		}
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	_, err := parseToolMajor("")
	assert.Error(t, err)
}
