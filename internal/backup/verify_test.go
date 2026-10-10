package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
)

const testGenID = "20261010T040000Z"

// putGeneration stores dump and meta under testGenID.
func putGeneration(t *testing.T, s *memStorage, meta Meta, dump []byte) {
	t.Helper()
	body, err := json.Marshal(meta)
	require.NoError(t, err)
	s.objects[Key(meta.ID, meta.DumpFile)] = dump
	s.objects[Key(meta.ID, MetaFile)] = body
}

func (e *verifyEnv) opts() VerifyOptions {
	return VerifyOptions{
		Sandbox:        e.sandbox,
		Bundled:        e.bundled,
		ElythiaVersion: "9.9.9",
		Now:            func() time.Time { return time.Date(2026, 10, 10, 5, 0, 0, 0, time.UTC) },
	}
}

// stageOf returns the result of stage.
func stageOf(t *testing.T, res VerifyResult, stage VerifyStage) StageResult {
	t.Helper()
	require.Len(t, res.Stages, 3)
	for _, s := range res.Stages {
		if s.Stage == stage {
			return s
		}
	}
	t.Fatalf("no stage %s", stage)
	return StageResult{}
}

// readVerifyJSON reads back verify.json.
func readVerifyJSON(t *testing.T, s *memStorage, id string) VerifyResult {
	t.Helper()
	body, ok := s.objects[Key(id, VerifyFile)]
	require.True(t, ok, "verify.json was not written")
	var got VerifyResult
	require.NoError(t, json.Unmarshal(body, &got))
	return got
}

// requireFailedAt checks that stage failed with msg and later stages were skipped.
func requireFailedAt(t *testing.T, res VerifyResult, stage VerifyStage, msg string) {
	t.Helper()
	assert.False(t, res.OK)
	order := []VerifyStage{StageReadable, StageRestorable, StageUsable}
	seen := false
	for i, st := range order {
		r := res.Stages[i]
		require.Equal(t, st, r.Stage)
		switch {
		case st == stage:
			seen = true
			assert.False(t, r.OK, st)
			assert.False(t, r.Skipped, st)
			assert.Contains(t, r.Error, msg)
		case !seen:
			assert.True(t, r.OK, "%s should pass: %s", st, r.Error)
		default:
			assert.True(t, r.Skipped, "%s should be skipped", st)
			assert.False(t, r.OK, st)
		}
	}
}

func TestVerifyHealthyBackupPassesAllStages(t *testing.T) {
	e := getVerifyEnv(t)
	dump := e.dump(t)
	s := newMemStorage()
	meta := e.buildMeta(t, testGenID, dump, e.bundled.Core, false)
	putGeneration(t, s, meta, dump)
	metaBefore := append([]byte(nil), s.objects[Key(testGenID, MetaFile)]...)

	res, err := Verify(context.Background(), s, testGenID, e.opts())
	require.NoError(t, err)
	for _, st := range res.Stages {
		assert.True(t, st.OK, "%s: %s", st.Stage, st.Error)
	}
	assert.True(t, res.OK)
	assert.Empty(t, res.Mismatches)
	assert.Equal(t, "9.9.9", res.ElythiaVersion)
	assert.Equal(t, meta.DumpSHA256, res.DumpSHA256)
	assert.Equal(t, time.Date(2026, 10, 10, 5, 0, 0, 0, time.UTC), res.VerifiedAt)
	assert.Equal(t, res, readVerifyJSON(t, s, testGenID))

	// 保存先は verify.json を足すだけで、dump と meta.json は書き換えない。
	assert.Equal(t, []string{Key(testGenID, VerifyFile)}, s.puts)
	assert.Equal(t, dump, s.objects[Key(testGenID, DumpFile)])
	assert.Equal(t, metaBefore, s.objects[Key(testGenID, MetaFile)])
	e.sandbox.assertNoLeftovers(t)
}

func TestVerifyEncryptedBackup(t *testing.T) {
	e := getVerifyEnv(t)
	dump := e.dump(t)
	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	var enc bytes.Buffer
	w, err := age.Encrypt(&enc, id.Recipient())
	require.NoError(t, err)
	_, err = w.Write(dump)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	meta := e.buildMeta(t, testGenID, dump, e.bundled.Core, false)
	meta.Encrypted = true
	meta.Encryption = "age"
	meta.DumpFile = DumpFileAge
	meta.PlainSHA256 = meta.DumpSHA256
	meta.DumpSHA256 = sha256Hex(enc.Bytes())
	meta.DumpSize = int64(enc.Len())

	t.Run("right identity", func(t *testing.T) {
		s := newMemStorage()
		putGeneration(t, s, meta, enc.Bytes())
		opts := e.opts()
		opts.Identities = []age.Identity{id}
		res, err := Verify(context.Background(), s, testGenID, opts)
		require.NoError(t, err)
		assert.True(t, res.OK, "%+v", res.Stages)
		e.sandbox.assertNoLeftovers(t)
	})
	t.Run("wrong identity", func(t *testing.T) {
		// 鍵の取り違えは設定の誤りで、世代の欠陥ではない。前回の結果を上書きしない。
		other, err := age.GenerateX25519Identity()
		require.NoError(t, err)
		s := newMemStorage()
		putGeneration(t, s, meta, enc.Bytes())
		opts := e.opts()
		opts.Identities = []age.Identity{other}
		_, err = Verify(context.Background(), s, testGenID, opts)
		require.ErrorContains(t, err, "identityFile does not match")
		assert.NotContains(t, s.objects, Key(testGenID, VerifyFile))
	})
	t.Run("plain hash mismatch", func(t *testing.T) {
		bad := meta
		bad.PlainSHA256 = sha256Hex([]byte("other"))
		s := newMemStorage()
		putGeneration(t, s, bad, enc.Bytes())
		opts := e.opts()
		opts.Identities = []age.Identity{id}
		res, err := Verify(context.Background(), s, testGenID, opts)
		require.NoError(t, err)
		requireFailedAt(t, res, StageReadable, "decrypted dump sha256")
	})
	t.Run("no identity configured", func(t *testing.T) {
		s := newMemStorage()
		putGeneration(t, s, meta, enc.Bytes())
		_, err := Verify(context.Background(), s, testGenID, e.opts())
		require.ErrorContains(t, err, "identityFile")
		assert.NotContains(t, s.objects, Key(testGenID, VerifyFile))
	})
}

func TestVerifyDetectsTruncatedDump(t *testing.T) {
	e := getVerifyEnv(t)
	dump := e.dump(t)
	cut := dump[:len(dump)-1]

	t.Run("meta of the whole dump", func(t *testing.T) {
		s := newMemStorage()
		putGeneration(t, s, e.buildMeta(t, testGenID, dump, e.bundled.Core, false), cut)
		res, err := Verify(context.Background(), s, testGenID, e.opts())
		require.NoError(t, err)
		requireFailedAt(t, res, StageReadable, "dump size")
		assert.Equal(t, res, readVerifyJSON(t, s, testGenID))
	})
	t.Run("meta of the cut dump", func(t *testing.T) {
		// pg_dump 自体が途中で切れ、その切れたファイルで meta.json を作った場合。
		// hash は一致するので、中身を読んで初めて分かる。
		//
		// 末尾を少し落としただけでは pg_restore は気付かない。PostgreSQL 18 の
		// pg_dump は (seek できる出力では) 末尾に TOC をもう 1 度書き、pg_restore は
		// 先頭の TOC だけを使うため (手元で 0.6 以降の位置で切ったものは全部戻った)。
		// その形は「meta of the whole dump」の大きさと hash の比較で捕まえる。
		s := newMemStorage()
		cutEarly := dump[:len(dump)/3]
		putGeneration(t, s, e.buildMeta(t, testGenID, cutEarly, e.bundled.Core, false), cutEarly)
		res, err := Verify(context.Background(), s, testGenID, e.opts())
		require.NoError(t, err)
		requireFailedAt(t, res, StageReadable, "pg_restore --list failed")
	})
}

func TestVerifyDetectsSHA256Mismatch(t *testing.T) {
	e := getVerifyEnv(t)
	dump := e.dump(t)
	meta := e.buildMeta(t, testGenID, dump, e.bundled.Core, false)
	flipped := append([]byte(nil), dump...)
	flipped[len(flipped)/2] ^= 0xff
	s := newMemStorage()
	putGeneration(t, s, meta, flipped)

	res, err := Verify(context.Background(), s, testGenID, e.opts())
	require.NoError(t, err)
	requireFailedAt(t, res, StageReadable, "dump sha256")
}

func TestVerifyDetectsRowCountMismatch(t *testing.T) {
	e := getVerifyEnv(t)
	dump := e.dump(t)
	meta := e.buildMeta(t, testGenID, dump, e.bundled.Core, false)
	meta.RowCounts["public.verify_fixture"] = fixtureRows + 1
	meta.RowCounts["public.no_such_table"] = 7
	meta.RowCounts["no_schema"] = 1
	// meta.json に無い表が戻した DB にあるのも、dump と meta.json の食い違い。
	require.Contains(t, meta.RowCounts, "public.note")
	delete(meta.RowCounts, "public.note")
	s := newMemStorage()
	putGeneration(t, s, meta, dump)

	res, err := Verify(context.Background(), s, testGenID, e.opts())
	require.NoError(t, err)
	requireFailedAt(t, res, StageRestorable, "row counts of 4 table(s)")
	assert.Equal(t, []RowMismatch{
		{Table: "no_schema", Expected: 1, Actual: -1},
		{Table: "public.no_such_table", Expected: 7, Actual: -1},
		{Table: "public.note", Expected: -1, Actual: 0},
		{Table: "public.verify_fixture", Expected: fixtureRows + 1, Actual: fixtureRows},
	}, res.Mismatches)
	assert.Equal(t, res.Mismatches, readVerifyJSON(t, s, testGenID).Mismatches)
	e.sandbox.assertNoLeftovers(t)
}

func TestVerifyDetectsDirtyTrackingTable(t *testing.T) {
	e := getVerifyEnv(t)
	e.setMigration(t, e.bundled.Core, true)
	dump := e.dump(t)
	s := newMemStorage()
	putGeneration(t, s, e.buildMeta(t, testGenID, dump, e.bundled.Core, true), dump)

	res, err := Verify(context.Background(), s, testGenID, e.opts())
	require.NoError(t, err)
	requireFailedAt(t, res, StageUsable, "schema_migrations is dirty")
	e.sandbox.assertNoLeftovers(t)
}

func TestVerifyDetectsTrackingTableAheadOfBinary(t *testing.T) {
	e := getVerifyEnv(t)
	e.setMigration(t, e.bundled.Core+1, false)
	dump := e.dump(t)
	s := newMemStorage()
	putGeneration(t, s, e.buildMeta(t, testGenID, dump, e.bundled.Core+1, false), dump)

	res, err := Verify(context.Background(), s, testGenID, e.opts())
	require.NoError(t, err)
	requireFailedAt(t, res, StageUsable, "newer than this binary")
}

func TestVerifyDetectsMetaMigrationDisagreement(t *testing.T) {
	e := getVerifyEnv(t)
	dump := e.dump(t)
	meta := e.buildMeta(t, testGenID, dump, e.bundled.Core-1, false)
	s := newMemStorage()
	putGeneration(t, s, meta, dump)

	res, err := Verify(context.Background(), s, testGenID, e.opts())
	require.NoError(t, err)
	requireFailedAt(t, res, StageUsable, "meta.json records")
}

func TestVerifyRejectsMetaWithoutRowCounts(t *testing.T) {
	e := getVerifyEnv(t)
	dump := e.dump(t)
	meta := e.buildMeta(t, testGenID, dump, e.bundled.Core, false)
	meta.RowCounts = nil
	s := newMemStorage()
	putGeneration(t, s, meta, dump)

	res, err := Verify(context.Background(), s, testGenID, e.opts())
	require.NoError(t, err)
	requireFailedAt(t, res, StageRestorable, "no row counts")
}

func TestVerifyRejectsGarbageDump(t *testing.T) {
	e := getVerifyEnv(t)
	garbage := []byte("this is not a pg_dump archive")
	s := newMemStorage()
	putGeneration(t, s, e.buildMeta(t, testGenID, garbage, e.bundled.Core, false), garbage)

	res, err := Verify(context.Background(), s, testGenID, e.opts())
	require.NoError(t, err)
	requireFailedAt(t, res, StageReadable, "pg_restore --list failed")
}

// failingSandbox wraps a Sandbox and fails one program.
type failingSandbox struct {
	Sandbox
	program   string
	mkdirErr  error
	removeErr error
	// failStop really stops the server and then reports a failure.
	failStop bool
	removed  []string
	initdb   []string
}

func (f *failingSandbox) Run(ctx context.Context, cmd SandboxCmd) error {
	if cmd.Program == f.program {
		return errors.New("injected failure")
	}
	if cmd.Program == "initdb" {
		f.initdb = cmd.Args
	}
	err := f.Sandbox.Run(ctx, cmd)
	if err == nil && f.failStop && cmd.Program == "pg_ctl" && len(cmd.Args) > 0 && cmd.Args[0] == "stop" {
		return errors.New("injected stop failure")
	}
	return err
}

func (f *failingSandbox) MkdirTemp(ctx context.Context) (string, error) {
	if f.mkdirErr != nil {
		return "", f.mkdirErr
	}
	return f.Sandbox.MkdirTemp(ctx)
}

func (f *failingSandbox) RemoveAll(ctx context.Context, dir string) error {
	f.removed = append(f.removed, dir)
	if err := f.Sandbox.RemoveAll(ctx, dir); err != nil {
		return err
	}
	return f.removeErr
}

func TestVerifySandboxFailuresAreErrorsAndCleanUp(t *testing.T) {
	e := getVerifyEnv(t)
	dump := e.dump(t)
	meta := e.buildMeta(t, testGenID, dump, e.bundled.Core, false)

	for _, tc := range []struct {
		name    string
		sandbox *failingSandbox
		want    string
	}{
		{"initdb fails", &failingSandbox{program: "initdb"}, "initdb"},
		{"start fails", &failingSandbox{program: "pg_ctl"}, "start the throwaway server"},
		{"mkdir fails", &failingSandbox{mkdirErr: errors.New("disk full")}, "disk full"},
		{"remove fails", &failingSandbox{removeErr: errors.New("busy")}, "remove the sandbox directory"},
		{"stop fails after the verdict", &failingSandbox{failStop: true}, "stop the throwaway server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.sandbox.Sandbox = e.sandbox
			s := newMemStorage()
			putGeneration(t, s, meta, dump)
			opts := e.opts()
			opts.Sandbox = tc.sandbox
			res, err := Verify(context.Background(), s, testGenID, opts)
			require.ErrorContains(t, err, tc.want)
			if tc.sandbox.removeErr != nil || tc.sandbox.failStop {
				// 判定が出た後の後始末の失敗では、判定を verify.json に残す。
				assert.True(t, res.OK, "%+v", res.Stages)
				assert.Equal(t, res, readVerifyJSON(t, s, testGenID))
			} else {
				assert.NotContains(t, s.objects, Key(testGenID, VerifyFile))
			}
			if tc.sandbox.mkdirErr == nil {
				assert.Len(t, tc.sandbox.removed, 1)
			}
			e.sandbox.assertNoLeftovers(t)
		})
	}
}

func TestVerifyRemovesStagedDump(t *testing.T) {
	e := getVerifyEnv(t)
	dump := e.dump(t)
	s := newMemStorage()
	putGeneration(t, s, e.buildMeta(t, testGenID, dump, e.bundled.Core, false), dump)
	opts := e.opts()
	opts.TempDir = t.TempDir()

	_, err := Verify(context.Background(), s, testGenID, opts)
	require.NoError(t, err)
	entries, err := os.ReadDir(opts.TempDir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// noRunSandbox fails the test if Verify gets as far as the sandbox.
type noRunSandbox struct{ t *testing.T }

func (n noRunSandbox) Run(context.Context, SandboxCmd) error {
	n.t.Fatal("sandbox used")
	return nil
}
func (n noRunSandbox) MkdirTemp(context.Context) (string, error) {
	n.t.Fatal("sandbox used")
	return "", nil
}
func (n noRunSandbox) RemoveAll(context.Context, string) error { return nil }
func (n noRunSandbox) Server(string) SandboxServer             { return SandboxServer{} }

func plainMeta(dump []byte) Meta {
	return Meta{
		FormatVersion: MetaFormatVersion, ID: testGenID, DumpFile: DumpFile,
		DumpSize: int64(len(dump)), DumpSHA256: sha256Hex(dump), RowCounts: map[string]int64{"public.x": 1},
	}
}

func TestVerifyInputErrors(t *testing.T) {
	dump := []byte("dump")
	good := VerifyOptions{Sandbox: noRunSandbox{t}, Bundled: BundledMigrations{Core: 1}}

	t.Run("invalid id", func(t *testing.T) {
		_, err := Verify(context.Background(), newMemStorage(), "latest", good)
		require.ErrorContains(t, err, "invalid generation id")
	})
	t.Run("no sandbox", func(t *testing.T) {
		_, err := Verify(context.Background(), newMemStorage(), testGenID, VerifyOptions{Bundled: good.Bundled})
		require.ErrorContains(t, err, "no sandbox")
	})
	t.Run("unknown bundled version", func(t *testing.T) {
		_, err := Verify(context.Background(), newMemStorage(), testGenID, VerifyOptions{Sandbox: good.Sandbox})
		require.ErrorContains(t, err, "bundled core migration")
	})
	t.Run("no meta", func(t *testing.T) {
		_, err := Verify(context.Background(), newMemStorage(), testGenID, good)
		require.ErrorContains(t, err, "incomplete")
	})
	t.Run("meta read error", func(t *testing.T) {
		s := newMemStorage()
		s.getErr = map[string]error{MetaFile: errors.New("network down")}
		_, err := Verify(context.Background(), s, testGenID, good)
		require.ErrorContains(t, err, "network down")
	})
	t.Run("broken meta", func(t *testing.T) {
		s := newMemStorage()
		s.objects[Key(testGenID, MetaFile)] = []byte("{")
		_, err := Verify(context.Background(), s, testGenID, good)
		require.ErrorContains(t, err, "decode")
	})
	t.Run("unknown meta version", func(t *testing.T) {
		s := newMemStorage()
		m := plainMeta(dump)
		m.FormatVersion = MetaFormatVersion + 1
		putGeneration(t, s, m, dump)
		_, err := Verify(context.Background(), s, testGenID, good)
		require.ErrorContains(t, err, "formatVersion")
	})
	t.Run("meta of another generation", func(t *testing.T) {
		s := newMemStorage()
		m := plainMeta(dump)
		m.ID = "20261010T030000Z"
		body, _ := json.Marshal(m)
		s.objects[Key(testGenID, MetaFile)] = body
		_, err := Verify(context.Background(), s, testGenID, good)
		require.ErrorContains(t, err, "is for generation")
	})
	t.Run("dump read error", func(t *testing.T) {
		s := newMemStorage()
		putGeneration(t, s, plainMeta(dump), dump)
		s.getErr = map[string]error{DumpFile: errors.New("network down")}
		_, err := Verify(context.Background(), s, testGenID, good)
		require.ErrorContains(t, err, "network down")
	})
	t.Run("temp dir unusable", func(t *testing.T) {
		s := newMemStorage()
		putGeneration(t, s, plainMeta(dump), dump)
		opts := good
		opts.TempDir = filepath.Join(t.TempDir(), "missing")
		_, err := Verify(context.Background(), s, testGenID, opts)
		require.Error(t, err)
	})
}

func TestVerifyReadableStageFailuresWithoutSandbox(t *testing.T) {
	dump := []byte("dump")
	opts := VerifyOptions{Sandbox: noRunSandbox{t}, Bundled: BundledMigrations{Core: 1}}
	for _, tc := range []struct {
		name   string
		mutate func(*Meta, *memStorage)
		want   string
	}{
		{"dump missing", func(_ *Meta, s *memStorage) { delete(s.objects, Key(testGenID, DumpFile)) }, "is missing"},
		{"dump file name outside the generation", func(m *Meta, _ *memStorage) { m.DumpFile = "../../etc/passwd" }, "names dump file"},
		{"encrypted under the plain name", func(m *Meta, _ *memStorage) { m.Encrypted, m.Encryption = true, "age" }, "names dump file"},
		{"unknown encryption", func(m *Meta, _ *memStorage) { m.Encrypted, m.Encryption = true, "gpg" }, "unknown encryption"},
		{"plain hash disagrees", func(m *Meta, _ *memStorage) { m.PlainSHA256 = sha256Hex([]byte("x")) }, "decrypted dump sha256"},
		{"not age", func(m *Meta, s *memStorage) {
			m.Encrypted, m.Encryption, m.DumpFile = true, "age", DumpFileAge
			s.objects[Key(testGenID, DumpFileAge)] = dump
		}, "cannot decrypt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newMemStorage()
			m := plainMeta(dump)
			putGeneration(t, s, m, dump)
			tc.mutate(&m, s)
			body, _ := json.Marshal(m)
			s.objects[Key(testGenID, MetaFile)] = body
			o := opts
			if m.Encrypted {
				id, err := age.GenerateX25519Identity()
				require.NoError(t, err)
				o.Identities = []age.Identity{id}
			}
			res, err := Verify(context.Background(), s, testGenID, o)
			require.NoError(t, err)
			requireFailedAt(t, res, StageReadable, tc.want)
			assert.Equal(t, config.MkGoVersion, res.ElythiaVersion)
			assert.Equal(t, res, readVerifyJSON(t, s, testGenID))
		})
	}
}

func TestVerifyResultWriteError(t *testing.T) {
	dump := []byte("dump")
	s := newMemStorage()
	m := plainMeta(dump)
	m.DumpSize++
	putGeneration(t, s, m, dump)
	s.putErr = map[string]error{VerifyFile: errors.New("read-only")}
	res, err := Verify(context.Background(), s, testGenID, VerifyOptions{Sandbox: noRunSandbox{t}, Bundled: BundledMigrations{Core: 1}})
	require.ErrorContains(t, err, "read-only")
	assert.False(t, res.OK)
}

func TestResolveGenerationID(t *testing.T) {
	ctx := context.Background()
	s := newMemStorage()
	_, err := ResolveGenerationID(ctx, s, "latest")
	require.ErrorContains(t, err, "no complete generation")

	putMeta := func(id string) {
		m := plainMeta([]byte("x"))
		m.ID = id
		body, err := json.Marshal(m)
		require.NoError(t, err)
		s.objects[Key(id, MetaFile)] = body
	}
	putMeta("20261008T000000Z")
	putMeta("20261009T000000Z")
	// meta.json の無い新しい世代 (取っている途中) は選ばない。
	s.objects[Key("20261010T000000Z", DumpFile)] = []byte("x")
	s.objects["generations/junk/meta.json"] = []byte("{}")

	got, err := ResolveGenerationID(ctx, s, "latest")
	require.NoError(t, err)
	assert.Equal(t, "20261009T000000Z", got)

	// 最新の meta.json が読めないときは、古い世代へ黙って下がらずに止める。
	s.objects[Key("20261011T000000Z", MetaFile)] = []byte("{")
	_, err = ResolveGenerationID(ctx, s, "latest")
	require.ErrorContains(t, err, "the newest generation 20261011T000000Z has an unreadable meta.json")

	got, err = ResolveGenerationID(ctx, s, "20261001T000000Z")
	require.NoError(t, err)
	assert.Equal(t, "20261001T000000Z", got)

	_, err = ResolveGenerationID(ctx, s, "../x")
	require.ErrorContains(t, err, "invalid generation id")

	s.getErr = map[string]error{"<list>": errors.New("denied")}
	_, err = ResolveGenerationID(ctx, s, "latest")
	require.ErrorContains(t, err, "denied")
}

func TestLocalSandbox(t *testing.T) {
	ctx := context.Background()
	sb := LocalSandbox{TempDir: t.TempDir(), Tools: config.BackupToolsOptions{PgRestore: "cat", PgCtl: "false"}}

	var out bytes.Buffer
	require.NoError(t, sb.Run(ctx, SandboxCmd{Program: "pg_restore", Stdin: bytes.NewReader([]byte("hello")), Stdout: &out}))
	assert.Equal(t, "hello", out.String())

	err := sb.Run(ctx, SandboxCmd{Program: "pg_ctl"})
	require.ErrorContains(t, err, "pg_ctl")
	var pe *ProgramError
	require.ErrorAs(t, err, &pe, "false ran and exited 1")
	assert.Equal(t, 1, pe.ExitCode)

	// 上書きしていないプログラムは PATH から名前で探す。
	err = sb.Run(ctx, SandboxCmd{Program: "sh", Args: []string{"-c", "echo oops >&2; exit 3"}})
	require.ErrorContains(t, err, "oops")
	require.ErrorAs(t, err, &pe)
	assert.Equal(t, ProgramError{Program: "sh", ExitCode: 3, Stderr: "oops"}, *pe)

	// 動かせなかったもの (見つからない、126 / 127、シグナル) は ProgramError にしない。
	for _, c := range []SandboxCmd{
		{Program: "elythia-no-such-program"},
		{Program: "sh", Args: []string{"-c", "exit 127"}},
		{Program: "sh", Args: []string{"-c", "exit 126"}},
		{Program: "sh", Args: []string{"-c", "kill -9 $$"}},
	} {
		err := sb.Run(ctx, c)
		require.Error(t, err, "%v", c.Args)
		assert.False(t, errors.As(err, &pe), "%v: %v", c.Args, err)
	}

	free, err := sb.FreeBytes(ctx, sb.TempDir)
	require.NoError(t, err)
	assert.Positive(t, free)
	_, err = sb.FreeBytes(ctx, filepath.Join(sb.TempDir, "missing"))
	require.Error(t, err)

	dir, err := sb.MkdirTemp(ctx)
	require.NoError(t, err)
	info, err := os.Stat(filepath.Join(dir, "sock"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())

	srv := sb.Server(dir)
	assert.Equal(t, filepath.Join(dir, "sock"), srv.Host)
	assert.Equal(t, srv.Host, srv.ToolHost)
	assert.Equal(t, srv.Port, srv.ToolPort)
	// 待ち受けは unix socket だけにする (TCP を開かない)。
	assert.Contains(t, srv.Options, "listen_addresses=")

	require.NoError(t, sb.RemoveAll(ctx, dir))
	_, err = os.Stat(dir)
	assert.True(t, os.IsNotExist(err))

	_, err = LocalSandbox{TempDir: filepath.Join(dir, "missing")}.MkdirTemp(ctx)
	require.Error(t, err)
}

func TestVerifyReportsFsckDriftAsWarning(t *testing.T) {
	e := getVerifyEnv(t)
	// notesCount を実際の件数 (0) とずらしたユーザーを置く。元の DB にあるずれで、
	// バックアップの欠陥ではないので、段は通して警告に残す。
	require.NoError(t, e.db.Exec(`INSERT INTO "user" (id, username, "usernameLower", "avatarDecorations", "notesCount") VALUES ('verifydrift1', 'drift', 'drift', '[]', 5)`).Error)
	t.Cleanup(func() { require.NoError(t, e.db.Exec(`DELETE FROM "user" WHERE id = 'verifydrift1'`).Error) })
	dump := e.dump(t)
	s := newMemStorage()
	putGeneration(t, s, e.buildMeta(t, testGenID, dump, e.bundled.Core, false), dump)

	res, err := Verify(context.Background(), s, testGenID, e.opts())
	require.NoError(t, err)
	assert.True(t, res.OK, "%+v", res.Stages)
	usable := stageOf(t, res, StageUsable)
	require.Len(t, usable.Warnings, 1)
	assert.Contains(t, usable.Warnings[0], "1 counter drift")
	assert.Equal(t, usable.Warnings, stageOf(t, readVerifyJSON(t, s, testGenID), StageUsable).Warnings)
}

func TestVerifyTrackingTableShapes(t *testing.T) {
	e := getVerifyEnv(t)
	for _, tc := range []struct {
		name  string
		setup []string
		undo  []string
		want  string // "" means the usable stage passes
	}{
		{
			name:  "missing core table",
			setup: []string{`ALTER TABLE schema_migrations RENAME TO schema_migrations_moved`},
			undo:  []string{`ALTER TABLE schema_migrations_moved RENAME TO schema_migrations`},
			want:  "schema_migrations does not exist",
		},
		{
			name:  "local table ahead of the binary",
			setup: []string{`CREATE TABLE schema_migrations_local (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`, `INSERT INTO schema_migrations_local VALUES (900001, false)`},
			undo:  []string{`DROP TABLE schema_migrations_local`},
			want:  "schema_migrations_local is at version 900001",
		},
		{
			// 全段を down した後の fork の管理表 (行が無い) は NilMigrationVersion (-1) として通す。
			name:  "empty local table",
			setup: []string{`CREATE TABLE schema_migrations_local (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`},
			undo:  []string{`DROP TABLE schema_migrations_local`},
		},
		{
			// fsck の問い合わせが通らない DB (本体の表が欠けている) は使えない。
			name:  "fsck cannot run",
			setup: []string{`ALTER TABLE note RENAME TO note_moved`},
			undo:  []string{`ALTER TABLE note_moved RENAME TO note`},
			want:  "fsck cannot run on the restored database",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, q := range tc.setup {
				require.NoError(t, e.db.Exec(q).Error)
			}
			t.Cleanup(func() {
				for _, q := range tc.undo {
					require.NoError(t, e.db.Exec(q).Error)
				}
			})
			dump := e.dump(t)
			meta := e.buildMeta(t, testGenID, dump, e.bundled.Core, false)
			// 管理表の形そのものを試すので、meta.json との突き合わせは外す。
			meta.Migrations = nil
			s := newMemStorage()
			putGeneration(t, s, meta, dump)

			res, err := Verify(context.Background(), s, testGenID, e.opts())
			require.NoError(t, err)
			if tc.want == "" {
				assert.True(t, res.OK, "%+v", res.Stages)
				return
			}
			requireFailedAt(t, res, StageUsable, tc.want)
		})
	}
}

// TestVerifyGenerationsTakenByTake takes generations with Take into MinIO
// (plain and encrypted) and into a directory, and verifies each through the
// same storage: all three stages pass and verify.json is stored next to
// meta.json.
func TestVerifyGenerationsTakenByTake(t *testing.T) {
	e := getVerifyEnv(t)
	p := newDatabase(t)
	gdb, err := migrateDatabase(p.url, e.bundled)
	require.NoError(t, err)
	closeGorm(gdb)
	// CountRows が扱う形 (拡張、別の schema と引用の要る名前、分割表) を、
	// Elythia の schema に足す。
	p.exec(t,
		`CREATE EXTENSION pg_trgm`,
		`CREATE SCHEMA other`,
		`CREATE TABLE other."Weird Name" (x int)`,
		`INSERT INTO other."Weird Name" VALUES (1), (2), (3)`,
		`CREATE TABLE ev (id int, k int) PARTITION BY RANGE (k)`,
		`CREATE TABLE ev_p1 PARTITION OF ev FOR VALUES FROM (0) TO (10)`,
		`CREATE TABLE ev_p2 PARTITION OF ev FOR VALUES FROM (10) TO (20)`,
		`INSERT INTO ev SELECT g, g FROM generate_series(0, 14) g`,
		`INSERT INTO "user" (id, username, "usernameLower") SELECT 'u' || g, 'user' || g, 'user' || g FROM generate_series(1, 25) g`,
	)
	identity, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	recipients, err := ParseRecipients([]string{identity.Recipient().String()})
	require.NoError(t, err)

	for _, tc := range []struct {
		name    string
		storage func(t *testing.T) Storage
		encrypt bool
	}{
		{"minio", func(t *testing.T) Storage { return newS3Storage(t) }, false},
		{"minio encrypted", func(t *testing.T) Storage { return newS3Storage(t) }, true},
		{"directory", func(t *testing.T) Storage {
			st, err := NewDirStorage(markedDir(t))
			require.NoError(t, err)
			return st
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.storage(t)
			takeOpts := p.takeOptions(st)
			if tc.encrypt {
				takeOpts.Recipients = recipients
			}
			meta, err := Take(ctx, takeOpts)
			require.NoError(t, err)
			// 前提: 拡張と locale が記録され、verify がそれを使う経路を通る。
			require.True(t, slices.ContainsFunc(meta.Extensions, func(x ExtensionInfo) bool { return x.Name == "pg_trgm" }), "%+v", meta.Extensions)
			require.NotEmpty(t, meta.DatabaseLocale.Collate)
			require.Equal(t, int64(3), meta.RowCounts[`other.Weird Name`])
			require.Equal(t, tc.encrypt, meta.Encrypted)

			opts := e.opts()
			if tc.encrypt {
				opts.Identities = []age.Identity{identity}
			}
			rec := &failingSandbox{Sandbox: e.sandbox}
			opts.Sandbox = rec
			res, err := Verify(ctx, st, meta.ID, opts)
			// 使い捨てのサーバーは、元の DB と同じ locale で作る。
			assert.Contains(t, rec.initdb, "--lc-collate="+meta.DatabaseLocale.Collate)
			require.NoError(t, err)
			require.Len(t, res.Stages, 3)
			for _, s := range res.Stages {
				assert.True(t, s.OK, "%s: %s", s.Stage, s.Error)
			}
			assert.True(t, res.OK)
			assert.Empty(t, res.Mismatches)

			// verify.json は Verify が nil を返す前に保存先へ置かれている。
			stored, err := ReadVerify(ctx, st, meta.ID)
			require.NoError(t, err)
			assert.Equal(t, res, *stored)
			gens, err := ListGenerations(ctx, st)
			require.NoError(t, err)
			require.Len(t, gens, 1)
			assert.True(t, gens[0].Complete())
			require.NotNil(t, gens[0].Verify)
			assert.True(t, gens[0].Verify.OK)
			e.sandbox.assertNoLeftovers(t)
		})
	}
}

// TestVerifyStopsOnMissingExtension: an extension the throwaway server cannot
// install is an error of the environment, reported before pg_restore and
// without writing verify.json.
func TestVerifyStopsOnMissingExtension(t *testing.T) {
	e := getVerifyEnv(t)
	dump := e.dump(t)
	meta := e.buildMeta(t, testGenID, dump, e.bundled.Core, false)
	meta.Extensions = []ExtensionInfo{
		{Name: "pg_bigm", Version: "1.2", Schema: "public"},
		{Name: "plpgsql", Version: "1.0", Schema: "pg_catalog"},
	}
	s := newMemStorage()
	putGeneration(t, s, meta, dump)

	res, err := Verify(context.Background(), s, testGenID, e.opts())
	require.ErrorContains(t, err, "extension(s) pg_bigm that the throwaway PostgreSQL server cannot install")
	assert.NotContains(t, err.Error(), "plpgsql")
	assert.False(t, res.OK)
	assert.NotContains(t, s.objects, Key(testGenID, VerifyFile))
	e.sandbox.assertNoLeftovers(t)
}

func TestInitdbLocaleArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   DatabaseLocale
		want []string
	}{
		{"recorded before #3458", DatabaseLocale{}, []string{"--encoding=UTF8", "--locale=C"}},
		{"libc", DatabaseLocale{Encoding: "UTF8", Collate: "en_US.utf8", Ctype: "en_US.utf8", Provider: "libc"},
			[]string{"--encoding=UTF8", "--locale=C", "--lc-collate=en_US.utf8", "--lc-ctype=en_US.utf8"}},
		{"icu", DatabaseLocale{Encoding: "UTF8", Collate: "C", Ctype: "C", Provider: "icu", Locale: "ja-JP"},
			[]string{"--encoding=UTF8", "--locale=C", "--lc-collate=C", "--lc-ctype=C", "--locale-provider=icu", "--icu-locale=ja-JP"}},
		{"builtin", DatabaseLocale{Encoding: "UTF8", Collate: "C", Ctype: "C", Provider: "builtin", Locale: "C.UTF-8"},
			[]string{"--encoding=UTF8", "--locale=C", "--lc-collate=C", "--lc-ctype=C", "--locale-provider=builtin", "--builtin-locale=C.UTF-8"}},
		{"other encoding", DatabaseLocale{Encoding: "EUC_JP", Collate: "C", Ctype: "C"},
			[]string{"--encoding=EUC_JP", "--locale=C", "--lc-collate=C", "--lc-ctype=C"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, initdbLocaleArgs(tc.in))
		})
	}
}

// TestVerifyEnvironmentFailuresKeepTheEarlierResult: failures of the
// environment (not of the generation) return an error and leave verify.json
// as it was.
func TestVerifyEnvironmentFailuresKeepTheEarlierResult(t *testing.T) {
	dump := []byte("dump bytes of some length")
	earlier := []byte(`{"ok":true}`)
	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	var enc bytes.Buffer
	w, err := age.Encrypt(&enc, id.Recipient())
	require.NoError(t, err)
	_, err = w.Write(dump)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	encMeta := plainMeta(enc.Bytes())
	encMeta.Encrypted, encMeta.Encryption, encMeta.DumpFile = true, EncryptionAge, DumpFileAge
	encMeta.PlainSHA256 = sha256Hex(dump)

	for _, tc := range []struct {
		name  string
		meta  Meta
		data  []byte
		setup func(*memStorage, *VerifyOptions)
		want  string
	}{
		{"pg_restore cannot be run", plainMeta(dump), dump, func(_ *memStorage, o *VerifyOptions) {
			o.Sandbox = LocalSandbox{TempDir: t.TempDir(), Tools: config.BackupToolsOptions{PgRestore: "/nonexistent/pg_restore"}}
		}, "run pg_restore --list"},
		{"download cut", plainMeta(dump), dump, func(s *memStorage, _ *VerifyOptions) {
			s.failAfter = map[string]int{DumpFile: 5}
		}, "connection reset by peer"},
		{"download cut in the age header", encMeta, enc.Bytes(), func(s *memStorage, _ *VerifyOptions) {
			s.failAfter = map[string]int{DumpFileAge: 10}
		}, "connection reset by peer"},
		{"download cut in the age payload", encMeta, enc.Bytes(), func(s *memStorage, _ *VerifyOptions) {
			s.failAfter = map[string]int{DumpFileAge: len(enc.Bytes()) - 5}
		}, "connection reset by peer"},
		{"temporary directory too small", func() Meta { m := plainMeta(dump); m.DumpSize = 1 << 62; return m }(), dump, nil,
			"less than the 4611686018427387904 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newMemStorage()
			putGeneration(t, s, tc.meta, tc.data)
			s.objects[Key(testGenID, VerifyFile)] = earlier
			o := VerifyOptions{Sandbox: noRunSandbox{t}, Bundled: BundledMigrations{Core: 1}, Identities: []age.Identity{id}}
			if tc.setup != nil {
				tc.setup(s, &o)
			}
			_, err := Verify(context.Background(), s, testGenID, o)
			require.ErrorContains(t, err, tc.want)
			assert.Equal(t, earlier, s.objects[Key(testGenID, VerifyFile)])
		})
	}
}

// failingWriter fails every write, like a full disk.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, syscall.ENOSPC }
func (failingWriter) Close() error              { return nil }

// closeFailingWriter accepts writes but fails on Close (delayed write error).
type closeFailingWriter struct{}

func (closeFailingWriter) Write(p []byte) (int, error) { return len(p), nil }
func (closeFailingWriter) Close() error                { return syscall.ENOSPC }

func TestVerifyStagingWriteErrorsAreEnvironmentErrors(t *testing.T) {
	dump := []byte("dump")
	for name, w := range map[string]io.WriteCloser{"write": failingWriter{}, "close": closeFailingWriter{}} {
		t.Run(name, func(t *testing.T) {
			s := newMemStorage()
			putGeneration(t, s, plainMeta(dump), dump)
			v := &verifier{storage: s, id: testGenID, meta: plainMeta(dump), opts: VerifyOptions{Sandbox: noRunSandbox{t}},
				dumpPath: filepath.Join(t.TempDir(), "dump.pgc"),
				create:   func(string) (io.WriteCloser, error) { return w, nil }}
			err := v.readable(context.Background())
			require.ErrorIs(t, err, syscall.ENOSPC)
			var se *stageError
			assert.False(t, errors.As(err, &se))
		})
	}
}

func TestProgramVerdict(t *testing.T) {
	ctx := context.Background()
	var se *stageError

	err := programVerdict(ctx, "pg_restore", &ProgramError{Program: "pg_restore", ExitCode: 1, Stderr: `invalid input syntax for type integer`})
	require.ErrorAs(t, err, &se, "a dump that pg_restore rejects is a defect of the generation")
	assert.Contains(t, se.msg, "pg_restore failed")

	for _, stderr := range envMarkers {
		err := programVerdict(ctx, "pg_restore", &ProgramError{Program: "pg_restore", ExitCode: 1, Stderr: "ERROR: " + stderr})
		assert.False(t, errors.As(err, &se), stderr)
		assert.ErrorContains(t, err, "because of the environment")
	}

	err = programVerdict(ctx, "pg_restore", errors.New("exec: not found"))
	assert.False(t, errors.As(err, &se))

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	err = programVerdict(canceled, "pg_restore", &ProgramError{Program: "pg_restore", ExitCode: 1})
	assert.False(t, errors.As(err, &se), "interrupted, not judged")
}

func TestRequireFree(t *testing.T) {
	free := func(n int64, err error) func(string) (int64, error) {
		return func(string) (int64, error) { return n, err }
	}
	require.NoError(t, requireFree("x", "/", 10, free(10, nil)))
	require.ErrorContains(t, requireFree("x", "/", 11, free(10, nil)), "TMPDIR")
	require.ErrorContains(t, requireFree("x", "/", 1, free(0, errors.New("statfs failed"))), "statfs failed")
}

// spaceSandbox reports a fixed amount of free space for the sandbox directory.
type spaceSandbox struct {
	Sandbox
	free int64
}

func (s spaceSandbox) FreeBytes(context.Context, string) (int64, error) { return s.free, nil }

// TestVerifySandboxSpaceCheck: the sandbox directory needs at least the dump's
// size free, checked before initdb.
func TestVerifySandboxSpaceCheck(t *testing.T) {
	e := getVerifyEnv(t)
	dump := e.dump(t)
	meta := e.buildMeta(t, testGenID, dump, e.bundled.Core, false)
	s := newMemStorage()
	putGeneration(t, s, meta, dump)
	opts := e.opts()
	opts.Sandbox = spaceSandbox{Sandbox: e.sandbox, free: meta.DumpSize - 1}
	_, err := Verify(context.Background(), s, testGenID, opts)
	require.ErrorContains(t, err, "the sandbox directory")
	assert.NotContains(t, s.objects, Key(testGenID, VerifyFile))
	e.sandbox.assertNoLeftovers(t)

	opts.Sandbox = spaceSandbox{Sandbox: e.sandbox, free: meta.DumpSize}
	res, err := Verify(context.Background(), s, testGenID, opts)
	require.NoError(t, err)
	assert.True(t, res.OK)
}

// TestVerifyDetectsDumpThatDoesNotRestore: the TOC is intact (pg_restore
// --list passes) but the table data is damaged, so only the restore fails.
// That is a defect of the generation and is recorded in verify.json.
func TestVerifyDetectsDumpThatDoesNotRestore(t *testing.T) {
	e := getVerifyEnv(t)
	// 表のデータが dump の大半を占めるようにし、真ん中を壊せばデータの塊に当たるようにする。
	p := newDatabase(t)
	p.exec(t,
		`CREATE TABLE big (id int PRIMARY KEY, body text)`,
		`INSERT INTO big SELECT g, md5(g::text) || md5((g * 7)::text) FROM generate_series(1, 100000) g`,
	)
	var out bytes.Buffer
	require.NoError(t, e.sandbox.Run(context.Background(), SandboxCmd{
		Program: "pg_dump", Args: []string{"-Fc", "-U", pgUser, "-d", p.db}, Stdout: &out,
	}))
	damaged := out.Bytes()
	for i := len(damaged) * 45 / 100; i < len(damaged)*55/100; i++ {
		damaged[i] ^= 0x5a
	}
	meta := plainMeta(damaged)
	meta.RowCounts = map[string]int64{"public.big": 100000}
	s := newMemStorage()
	putGeneration(t, s, meta, damaged)

	res, err := Verify(context.Background(), s, testGenID, e.opts())
	require.NoError(t, err)
	requireFailedAt(t, res, StageRestorable, "pg_restore failed")
	assert.Equal(t, res, readVerifyJSON(t, s, testGenID))
	e.sandbox.assertNoLeftovers(t)
}

func TestCreateStagingIsPrivate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dump.pgc")
	w, err := createStaging(p)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	info, err := os.Stat(p)
	require.NoError(t, err)
	// 復号した dump には秘密鍵や token が入る。
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	_, err = createStaging(p)
	require.Error(t, err, "an existing file is not reused")
}
