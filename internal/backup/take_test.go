package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/testutil"
)

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// TestTake_MinIO takes a backup of a database with the real migrations into
// MinIO and checks every field of meta.json against what is in the bucket
// and in the dump.
func TestTake_MinIO(t *testing.T) {
	p := newDatabase(t)
	gdb, err := gorm.Open(postgres.Open(p.url), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	require.NoError(t, err)
	testutil.ApplyMigrations(gdb)
	if sqlDB, err := gdb.DB(); err == nil {
		_ = sqlDB.Close()
	}
	p.exec(t,
		// 本体の migration にも管理表の定義がある。中身は golang-migrate と同じ 1 行にする。
		`CREATE TABLE IF NOT EXISTS schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`,
		`DELETE FROM schema_migrations`,
		`INSERT INTO schema_migrations VALUES (121, false)`,
		`CREATE TABLE schema_migrations_local (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`,
		`INSERT INTO schema_migrations_local VALUES (900001, true)`,
		`INSERT INTO "user" (id, username, "usernameLower") SELECT 'u' || g, 'user' || g, 'user' || g FROM generate_series(1, 25) g`,
		`ALTER DATABASE `+p.db+` SET work_mem = '8MB'`,
	)
	st := newS3Storage(t)
	at := time.Date(2026, 10, 10, 4, 0, 0, 0, time.UTC)
	opts := p.takeOptions(st)
	opts.Now = func() time.Time { return at }

	meta, err := Take(context.Background(), opts)
	require.NoError(t, err)

	id := "20261010T040000Z"
	assert.Equal(t, id, meta.ID)
	assert.Equal(t, MetaFormatVersion, meta.FormatVersion)
	assert.True(t, meta.CreatedAt.Equal(at))
	assert.Equal(t, config.MkGoVersion, meta.ElythiaVersion)
	assert.Equal(t, p.db, meta.Database)
	assert.Regexp(t, `^18\.`, meta.PostgresVersion)
	assert.GreaterOrEqual(t, meta.PostgresVersionNum, 180000)
	assert.Equal(t, []MigrationState{
		{Table: "schema_migrations", Version: 121},
		{Table: "schema_migrations_local", Version: 900001, Dirty: true},
	}, meta.Migrations)
	assert.Equal(t, []string{"work_mem=8MB"}, meta.DatabaseSettings)
	assert.Equal(t, int64(25), meta.RowCounts["public.user"])
	assert.False(t, meta.Encrypted)
	assert.Empty(t, meta.Encryption)
	assert.Equal(t, DumpFile, meta.DumpFile)

	// 保存先には dump と meta.json の 2 つだけが置かれる。
	objs, err := st.List(context.Background(), GenerationPrefix())
	require.NoError(t, err)
	require.Len(t, objs, 2)
	assert.Equal(t, Key(id, DumpFile), objs[0].Key)
	assert.Equal(t, Key(id, MetaFile), objs[1].Key)

	// sha256 と大きさは、バケットから直接読んだ bytes と一致する。
	dump := rawS3Get(t, st, Key(id, DumpFile))
	assert.Equal(t, int64(len(dump)), meta.DumpSize)
	assert.Equal(t, sha256Hex(dump), meta.DumpSHA256)
	assert.Equal(t, meta.DumpSHA256, meta.PlainSHA256, "unencrypted: stored bytes are the pg_dump output")
	assert.Equal(t, "PGDMP", string(dump[:5]))

	// meta.json はバケットの中身と同じ内容で読める。
	var stored Meta
	require.NoError(t, json.Unmarshal(rawS3Get(t, st, Key(id, MetaFile)), &stored))
	assert.Equal(t, meta.DumpSHA256, stored.DumpSHA256)
	assert.Equal(t, meta.RowCounts, stored.RowCounts)

	// 行数は dump の中身と一致する: 戻した DB を同じ方法で数える。
	restored := p.restore(t, dump)
	assert.Equal(t, meta.RowCounts, restored.countRows(t))
	assert.Greater(t, len(meta.RowCounts), 50, "the real schema has many tables")

	// 同じ秒にもう一度取ると、既存の世代を上書きせずに失敗する。
	_, err = Take(context.Background(), opts)
	require.ErrorContains(t, err, "already exists")
	again := rawS3Get(t, st, Key(id, DumpFile))
	assert.Equal(t, meta.DumpSHA256, sha256Hex(again))
}

// TestTake_CountsMatchDumpUnderWrites keeps inserting from another connection
// while the backup runs: the counts in meta.json must still equal the rows in
// the dump, because both come from the same snapshot.
func TestTake_CountsMatchDumpUnderWrites(t *testing.T) {
	p := newDatabase(t)
	p.seedSimple(t)
	// dump に時間がかかるよう、行を増やしておく。
	p.exec(t, `INSERT INTO note (body) SELECT repeat(md5(g::text), 20) FROM generate_series(1, 200000) g`)
	st := newS3Storage(t)

	writer := p.connect(t)
	var inserted atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// 3 つの表に書く。schema を跨いで同じ snapshot で数えているかを見る。
			if _, err := writer.Exec(context.Background(),
				`WITH a AS (INSERT INTO note (body) VALUES ('w') RETURNING 1),
				      b AS (INSERT INTO other."Weird Name" VALUES (9) RETURNING 1)
				 INSERT INTO ev VALUES (0, 15)`); err != nil {
				return
			}
			inserted.Add(1)
		}
	}()
	// 書き込みが始まってから取る。
	require.Eventually(t, func() bool { return inserted.Load() > 20 }, 10*time.Second, 10*time.Millisecond)
	before := inserted.Load()
	meta, err := Take(context.Background(), p.takeOptions(st))
	after := inserted.Load()
	close(stop)
	wg.Wait()
	require.NoError(t, err)
	require.Greater(t, after, before, "writes must continue while the backup runs")

	dump := rawS3Get(t, st, Key(meta.ID, DumpFile))
	restored := p.restore(t, dump)
	assert.Equal(t, meta.RowCounts, restored.countRows(t))
	// 取った後の書き込みは dump に入っていない (= snapshot の時点で数えている)。
	final := p.countRows(t)
	assert.Greater(t, final["public.note"], meta.RowCounts["public.note"])
}

// TestTake_Encrypted checks that an encrypted dump is not plaintext in the
// storage, and that the identity decrypts it into a dump pg_restore reads.
func TestTake_Encrypted(t *testing.T) {
	p := newDatabase(t)
	p.seedSimple(t)
	st := newS3Storage(t)
	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	recipients, err := ParseRecipients([]string{id.Recipient().String()})
	require.NoError(t, err)
	opts := p.takeOptions(st)
	opts.Recipients = recipients

	meta, err := Take(context.Background(), opts)
	require.NoError(t, err)
	assert.True(t, meta.Encrypted)
	assert.Equal(t, EncryptionAge, meta.Encryption)
	assert.Equal(t, DumpFileAge, meta.DumpFile)
	_, err = st.Stat(context.Background(), Key(meta.ID, DumpFile))
	assert.ErrorIs(t, err, ErrNotFound, "no plaintext dump next to the encrypted one")

	stored := rawS3Get(t, st, Key(meta.ID, DumpFileAge))
	assert.Equal(t, sha256Hex(stored), meta.DumpSHA256)
	assert.Equal(t, int64(len(stored)), meta.DumpSize)
	assert.NotEqual(t, meta.DumpSHA256, meta.PlainSHA256)
	assert.True(t, bytes.HasPrefix(stored, []byte("age-encryption.org/v1\n")))
	assert.NotContains(t, string(stored), "PGDMP")
	assert.NotContains(t, string(stored), "Weird Name")

	// 別の鍵では戻せない。
	other, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	_, err = Decrypt(bytes.NewReader(stored), other)
	require.Error(t, err)

	plain, err := Decrypt(bytes.NewReader(stored), id)
	require.NoError(t, err)
	dump, err := io.ReadAll(plain)
	require.NoError(t, err)
	assert.Equal(t, meta.PlainSHA256, sha256Hex(dump))
	list := p.pgRestoreList(t, dump)
	assert.Contains(t, list, "TABLE DATA public note")
	assert.Contains(t, list, "TABLE DATA other Weird Name")
	assert.Equal(t, meta.RowCounts, p.restore(t, dump).countRows(t))
}

// TestTake_Directory takes a backup into a directory storage.
func TestTake_Directory(t *testing.T) {
	p := newDatabase(t)
	p.seedSimple(t)
	st, err := NewDirStorage(markedDir(t))
	require.NoError(t, err)
	meta, err := Take(context.Background(), p.takeOptions(st))
	require.NoError(t, err)

	assert.Equal(t, map[string]int64{
		"other.Weird Name":         3,
		"public.ev_p1":             10,
		"public.ev_p2":             5,
		"public.note":              1000,
		"public.schema_migrations": 1,
	}, meta.RowCounts)
	assert.Equal(t, []MigrationState{
		{Table: "schema_migrations", Version: 121},
		{Table: "schema_migrations_local", Missing: true},
	}, meta.Migrations)
	assert.Equal(t, []string{"statement_timeout=5min", "work_mem=8MB"}, meta.DatabaseSettings)
	assert.Regexp(t, `^pg_dump \(PostgreSQL\) 18\.`, meta.PgDumpVersion)
	assert.Equal(t, "UTF8", meta.DatabaseLocale.Encoding)
	assert.Equal(t, "libc", meta.DatabaseLocale.Provider)
	assert.NotEmpty(t, meta.DatabaseLocale.Collate)
	assert.NotEmpty(t, meta.DatabaseLocale.Ctype)
	require.Len(t, meta.Extensions, 2)
	assert.Equal(t, "pg_trgm", meta.Extensions[0].Name)
	assert.Equal(t, "public", meta.Extensions[0].Schema)
	assert.NotEmpty(t, meta.Extensions[0].Version)
	assert.Equal(t, "plpgsql", meta.Extensions[1].Name)
	assert.Equal(t, "pg_catalog", meta.Extensions[1].Schema)

	gens, err := ListGenerations(context.Background(), st)
	require.NoError(t, err)
	require.Len(t, gens, 1)
	require.True(t, gens[0].Complete())
	assert.Equal(t, meta.DumpSHA256, gens[0].Meta.DumpSHA256)

	size, sum, err := HashObject(context.Background(), st, Key(meta.ID, DumpFile))
	require.NoError(t, err)
	assert.Equal(t, meta.DumpSize, size)
	assert.Equal(t, meta.DumpSHA256, sum)
	r, err := st.Get(context.Background(), Key(meta.ID, DumpFile))
	require.NoError(t, err)
	dump, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	assert.Equal(t, meta.RowCounts, p.restore(t, dump).countRows(t))
}

// TestTake_PgDumpFailureLeavesNothing: when pg_dump fails partway, neither
// storage keeps a dump or a meta.json.
func TestTake_PgDumpFailureLeavesNothing(t *testing.T) {
	p := newDatabase(t)
	p.seedSimple(t)
	dir, err := NewDirStorage(markedDir(t))
	require.NoError(t, err)
	for name, st := range map[string]Storage{"dir": dir, "s3": newS3Storage(t)} {
		t.Run(name, func(t *testing.T) {
			opts := p.takeOptions(st)
			// 存在しない DB へ繋がせて、pg_dump だけを失敗させる。
			opts.Dump.URI = "postgresql://" + pgUser + "@localhost:5432/no_such_db?sslmode=disable"
			_, err := Take(context.Background(), opts)
			require.Error(t, err)
			assert.ErrorContains(t, err, "no_such_db")
			objs, err := st.List(context.Background(), "")
			require.NoError(t, err)
			assert.Empty(t, objs)
		})
	}
}

// TestTake_PgDumpTruncatedOutputLeavesNothing: pg_dump writes part of the dump
// and then exits non-zero. The partial dump must not become an object.
func TestTake_PgDumpTruncatedOutputLeavesNothing(t *testing.T) {
	p := newDatabase(t)
	p.seedSimple(t)
	dir, err := NewDirStorage(markedDir(t))
	require.NoError(t, err)
	for name, st := range map[string]Storage{"dir": dir, "s3": newS3Storage(t)} {
		t.Run(name, func(t *testing.T) {
			// Delete を効かなくして、Take の後始末ではなく Put そのものが途中までの
			// dump を確定させないことを見る。
			opts := p.takeOptions(noDeleteStorage{st})
			opts.Runner = truncatingRunner{inner: p.runner}
			_, err := Take(context.Background(), opts)
			require.Error(t, err)
			objs, err := st.List(context.Background(), "")
			require.NoError(t, err)
			assert.Empty(t, objs)
		})
	}
}

// truncatingRunner runs the program through a shell that keeps only the first
// 2000 bytes of its output and then exits non-zero, like a pg_dump that dies
// partway.
type truncatingRunner struct{ inner dockerExecRunner }

func (r truncatingRunner) Command(ctx context.Context, env []string, name string, args ...string) *exec.Cmd {
	if len(args) == 1 && args[0] == "--version" {
		return r.inner.Command(ctx, env, name, args...)
	}
	return r.inner.Command(ctx, env, "sh", append([]string{"-c", `"$0" "$@" | head -c 2000; exit 3`, name}, args...)...)
}

// noDeleteStorage ignores Delete.
type noDeleteStorage struct{ Storage }

func (noDeleteStorage) Delete(context.Context, string) error { return nil }

// TestTake_SameSecondDoesNotMix starts two backups with the same generation ID
// at once. Exactly one may succeed, and its generation must be consistent:
// the other must neither overwrite nor delete its files.
func TestTake_SameSecondDoesNotMix(t *testing.T) {
	p := newDatabase(t)
	p.seedSimple(t)
	dir, err := NewDirStorage(markedDir(t))
	require.NoError(t, err)
	for name, st := range map[string]Storage{"dir": dir, "s3": newS3Storage(t)} {
		t.Run(name, func(t *testing.T) {
			at := time.Now()
			// List で同じ ID が無いことを確かめる段を、2 つとも通り抜けさせる。
			gate := &listGate{Storage: st, ready: make(chan struct{}), n: 2}
			var wg sync.WaitGroup
			errs := make([]error, 2)
			for i := range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					opts := p.takeOptions(gate)
					opts.Now = func() time.Time { return at }
					_, errs[i] = Take(context.Background(), opts)
				}()
			}
			wg.Wait()
			ok := 0
			for _, err := range errs {
				if err == nil {
					ok++
				} else {
					assert.ErrorIs(t, err, ErrExists)
				}
			}
			require.Equal(t, 1, ok, "%v", errs)
			gens, err := ListGenerations(context.Background(), st)
			require.NoError(t, err)
			require.Len(t, gens, 1)
			require.True(t, gens[0].Complete())
			size, sum, err := HashObject(context.Background(), st, Key(gens[0].ID, DumpFile))
			require.NoError(t, err)
			assert.Equal(t, gens[0].Meta.DumpSize, size)
			assert.Equal(t, gens[0].Meta.DumpSHA256, sum)
		})
	}
}

// listGate holds List until n callers have reached it, so concurrent Takes
// all pass the "does the generation exist" check.
type listGate struct {
	Storage
	mu    sync.Mutex
	n     int
	ready chan struct{}
}

func (g *listGate) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	g.mu.Lock()
	if g.n > 0 {
		g.n--
		if g.n == 0 {
			close(g.ready)
		}
	}
	g.mu.Unlock()
	<-g.ready
	return g.Storage.List(ctx, prefix)
}

func (g *listGate) PutNew(ctx context.Context, key string, r io.Reader) error {
	return g.Storage.(NewPutter).PutNew(ctx, key, r)
}

// TestTake_IgnoresDatabaseTimeouts: a statement_timeout set on the database
// must not cut the row counts.
func TestTake_IgnoresDatabaseTimeouts(t *testing.T) {
	p := newDatabase(t)
	p.exec(t,
		`CREATE TABLE big (id int)`,
		`INSERT INTO big SELECT generate_series(1, 2000000)`,
		`ALTER DATABASE `+p.db+` SET statement_timeout = '10ms'`,
		`ALTER DATABASE `+p.db+` SET idle_in_transaction_session_timeout = '10ms'`,
	)
	st, err := NewDirStorage(markedDir(t))
	require.NoError(t, err)
	meta, err := Take(context.Background(), p.takeOptions(st))
	require.NoError(t, err)
	assert.Equal(t, int64(2000000), meta.RowCounts["public.big"])
}
