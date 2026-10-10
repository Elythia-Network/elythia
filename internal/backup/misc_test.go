package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRecipients(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	rs, err := ParseRecipients([]string{" " + id.Recipient().String() + " ", ""})
	require.NoError(t, err)
	assert.Len(t, rs, 1)

	_, err = ParseRecipients(nil)
	require.ErrorContains(t, err, "recipients is empty")
	_, err = ParseRecipients([]string{" ", ""})
	require.ErrorContains(t, err, "recipients is empty")
	_, err = ParseRecipients([]string{"age1notakey"})
	require.ErrorContains(t, err, "encryption.recipients")
	// 秘密鍵を recipients に書き間違えたら受け付けない。
	_, err = ParseRecipients([]string{id.String()})
	require.Error(t, err)
}

func TestLoadIdentitiesAndDecrypt(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "key.txt")
	require.NoError(t, os.WriteFile(path, []byte("# created: now\n"+id.String()+"\n"), 0o600))
	ids, err := LoadIdentities(path)
	require.NoError(t, err)
	require.Len(t, ids, 1)

	var enc bytes.Buffer
	w, err := age.Encrypt(&enc, id.Recipient())
	require.NoError(t, err)
	_, err = io.WriteString(w, "plain")
	require.NoError(t, err)
	require.NoError(t, w.Close())
	r, err := Decrypt(&enc, ids...)
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "plain", string(got))

	_, err = LoadIdentities("")
	require.ErrorContains(t, err, "identityFile is empty")
	_, err = LoadIdentities(filepath.Join(t.TempDir(), "missing"))
	require.ErrorContains(t, err, "open identity file")
	bad := filepath.Join(t.TempDir(), "bad.txt")
	require.NoError(t, os.WriteFile(bad, []byte("not a key\n"), 0o600))
	_, err = LoadIdentities(bad)
	require.ErrorContains(t, err, "parse identity file")
}

func TestDumpConnFromURL(t *testing.T) {
	dc, err := DumpConnFromURL("postgres://mk:p%40ss@db:5432/mk?sslmode=disable")
	require.NoError(t, err)
	assert.Equal(t, "postgres://mk@db:5432/mk?sslmode=disable", dc.URI)
	assert.Equal(t, "p@ss", dc.Password)
	assert.NotContains(t, dc.URI, "p%40ss", "the password must not be on the command line")

	// UDS は host をクエリで渡す形 (config.DatabaseURL)。
	dc, err = DumpConnFromURL("postgres://mk:pw@/mk?host=%2Fvar%2Frun%2Fpostgresql&port=5432&sslmode=disable")
	require.NoError(t, err)
	assert.Equal(t, "pw", dc.Password)
	assert.Contains(t, dc.URI, "host=%2Fvar%2Frun%2Fpostgresql")

	dc, err = DumpConnFromURL("postgres:///mk")
	require.NoError(t, err)
	assert.Empty(t, dc.Password)

	_, err = DumpConnFromURL("postgres://a b@%zz")
	require.Error(t, err)
}

func TestTailBuffer(t *testing.T) {
	tb := &tailBuffer{max: 5}
	_, _ = tb.Write([]byte("abc"))
	_, _ = tb.Write([]byte("defg"))
	assert.Equal(t, "cdefg", tb.String())
}

func TestTake_RequiresStorageAndChecksBeforeConnecting(t *testing.T) {
	_, err := Take(context.Background(), TakeOptions{})
	require.ErrorContains(t, err, "no storage")

	st, err := NewDirStorage(t.TempDir())
	require.NoError(t, err)
	_, err = Take(context.Background(), TakeOptions{Storage: st, DatabaseURL: "postgres://a b@%zz"})
	require.Error(t, err)

	// 接続できない DB では、保存先に何も置かない。
	_, err = Take(context.Background(), TakeOptions{Storage: st, DatabaseURL: "postgres://x:y@127.0.0.1:1/none?sslmode=disable&connect_timeout=2"})
	require.ErrorContains(t, err, "connect")
	objs, err := st.List(context.Background(), "")
	require.NoError(t, err)
	assert.Empty(t, objs)
}

// brokenStorage wraps a Storage and makes chosen operations fail or lie.
type brokenStorage struct {
	Storage
	failList  bool
	failPutOn string
	// corruptGet flips the first byte of what Get returns for keys ending
	// with it.
	corruptGet string
	deleted    []string
}

func (b *brokenStorage) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	if b.failList {
		return nil, errors.New("list failed")
	}
	return b.Storage.List(ctx, prefix)
}

func (b *brokenStorage) Put(ctx context.Context, key string, r io.Reader) error {
	if b.failPutOn != "" && strings.HasSuffix(key, b.failPutOn) {
		_, _ = io.Copy(io.Discard, r)
		return errors.New("put failed")
	}
	return b.Storage.Put(ctx, key, r)
}

func (b *brokenStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := b.Storage.Get(ctx, key)
	if err != nil || b.corruptGet == "" || !strings.HasSuffix(key, b.corruptGet) {
		return rc, err
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return nil, err
	}
	data[0] ^= 0xff
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (b *brokenStorage) Delete(ctx context.Context, key string) error {
	b.deleted = append(b.deleted, key)
	return b.Storage.Delete(ctx, key)
}

func TestTake_StorageFailures(t *testing.T) {
	p := newDatabase(t)
	p.seedSimple(t)
	ctx := context.Background()
	newDir := func() Storage {
		st, err := NewDirStorage(t.TempDir())
		require.NoError(t, err)
		return st
	}

	t.Run("list fails", func(t *testing.T) {
		_, err := Take(ctx, p.takeOptions(&brokenStorage{Storage: newDir(), failList: true}))
		require.ErrorContains(t, err, "list failed")
	})
	t.Run("dump upload fails", func(t *testing.T) {
		inner := newDir()
		_, err := Take(ctx, p.takeOptions(&brokenStorage{Storage: inner, failPutOn: DumpFile}))
		require.ErrorContains(t, err, "put failed")
		objs, err := inner.List(ctx, "")
		require.NoError(t, err)
		assert.Empty(t, objs)
	})
	t.Run("read back differs", func(t *testing.T) {
		inner := newDir()
		bs := &brokenStorage{Storage: inner, corruptGet: DumpFile}
		_, err := Take(ctx, p.takeOptions(bs))
		require.ErrorContains(t, err, "read back as")
		objs, err := inner.List(ctx, "")
		require.NoError(t, err)
		assert.Empty(t, objs, "the dump that failed the check is deleted and no meta.json is written")
		require.Len(t, bs.deleted, 1)
	})
	t.Run("meta upload fails", func(t *testing.T) {
		inner := newDir()
		_, err := Take(ctx, p.takeOptions(&brokenStorage{Storage: inner, failPutOn: MetaFile}))
		require.ErrorContains(t, err, "put failed")
		objs, err := inner.List(ctx, "")
		require.NoError(t, err)
		assert.Empty(t, objs)
	})
	t.Run("pg_dump missing", func(t *testing.T) {
		opts := p.takeOptions(newDir())
		opts.Runner = LocalRunner{}
		opts.PgDump = filepath.Join(t.TempDir(), "no-pg_dump")
		_, err := Take(ctx, opts)
		require.ErrorContains(t, err, "start")
	})
	t.Run("cancelled", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := Take(cctx, p.takeOptions(newDir()))
		require.Error(t, err)
	})
}

func TestListGenerations(t *testing.T) {
	st, err := NewDirStorage(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()
	put := func(key, body string) { require.NoError(t, st.Put(ctx, key, strings.NewReader(body))) }

	complete, incomplete, unknown, verified := "20261001T000000Z", "20261002T000000Z", "20261003T000000Z", "20261004T000000Z"
	put(Key(complete, DumpFile), "dump")
	put(Key(complete, MetaFile), `{"formatVersion":1,"id":"`+complete+`","encrypted":false}`)
	put(Key(incomplete, DumpFile), "partial")
	put(Key(unknown, MetaFile), `{"formatVersion":2,"id":"`+unknown+`"}`)
	put(Key(verified, MetaFile), `{"formatVersion":1,"id":"`+verified+`"}`)
	put(Key(verified, VerifyFile), `{"id":"`+verified+`","ok":true}`)
	put("generations/latest/meta.json", "{}")
	put("unrelated", "x")

	gens, err := ListGenerations(ctx, st)
	require.NoError(t, err)
	require.Len(t, gens, 4)
	assert.Equal(t, []string{complete, incomplete, unknown, verified}, []string{gens[0].ID, gens[1].ID, gens[2].ID, gens[3].ID})

	assert.True(t, gens[0].Complete())
	assert.Equal(t, int64(len("dump")+len(`{"formatVersion":1,"id":"`+complete+`","encrypted":false}`)), gens[0].Size)
	assert.Len(t, gens[0].Objects, 2)
	assert.Nil(t, gens[0].Verify)

	assert.False(t, gens[1].Complete())
	assert.NoError(t, gens[1].MetaError)

	assert.False(t, gens[2].Complete())
	require.Error(t, gens[2].MetaError)
	assert.Contains(t, gens[2].MetaError.Error(), "formatVersion 2")

	require.NotNil(t, gens[3].Verify)
	assert.True(t, gens[3].Verify.OK)

	_, err = (&brokenStorage{Storage: st, failList: true}).List(ctx, "")
	require.Error(t, err)
	_, err = ListGenerations(ctx, &brokenStorage{Storage: st, failList: true})
	require.Error(t, err)
}

func TestReadMeta_Rejects(t *testing.T) {
	st, err := NewDirStorage(t.TempDir())
	require.NoError(t, err)
	ctx := context.Background()
	id := "20261001T000000Z"

	_, err = ReadMeta(ctx, st, "../x")
	require.ErrorContains(t, err, "invalid generation id")
	_, err = ReadMeta(ctx, st, id)
	require.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, st.Put(ctx, Key(id, MetaFile), strings.NewReader("{not json")))
	_, err = ReadMeta(ctx, st, id)
	require.ErrorContains(t, err, "decode")

	require.NoError(t, st.Put(ctx, Key(id, MetaFile), strings.NewReader(`{"formatVersion":1,"id":"20261002T000000Z"}`)))
	_, err = ReadMeta(ctx, st, id)
	require.ErrorContains(t, err, "is for generation")

	require.NoError(t, st.Put(ctx, Key(id, MetaFile), io.MultiReader(strings.NewReader(`{"a":"`), io.LimitReader(zeroReader{}, maxMetaBytes))))
	_, err = ReadMeta(ctx, st, id)
	require.ErrorContains(t, err, "too large")

	_, err = ReadVerify(ctx, st, id)
	require.ErrorIs(t, err, ErrNotFound)
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func TestDBInfo_ErrorsOnClosedConnection(t *testing.T) {
	p := newDatabase(t)
	conn, err := pgx.Connect(context.Background(), p.url)
	require.NoError(t, err)
	require.NoError(t, conn.Close(context.Background()))
	ctx := context.Background()
	_, err = CountRows(ctx, conn)
	require.Error(t, err)
	_, err = ReadMigrations(ctx, conn)
	require.Error(t, err)
	_, err = ReadDatabaseSettings(ctx, conn)
	require.Error(t, err)
	_, err = ReadServerInfo(ctx, conn)
	require.Error(t, err)
}

func TestReadMigrations_EmptyTable(t *testing.T) {
	p := newDatabase(t)
	p.exec(t, `CREATE TABLE schema_migrations_local (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`)
	got, err := ReadMigrations(context.Background(), p.connect(t))
	require.NoError(t, err)
	assert.Equal(t, []MigrationState{
		{Table: "schema_migrations", Missing: true},
		{Table: "schema_migrations_local", Version: NilMigrationVersion},
	}, got)
	settings, err := ReadDatabaseSettings(context.Background(), p.connect(t))
	require.NoError(t, err)
	assert.Equal(t, []string{}, settings, "no settings is an empty list, not null")
}
