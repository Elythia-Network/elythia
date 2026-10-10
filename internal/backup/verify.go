package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/fsck"
)

// sandboxSuperuser is the superuser initdb creates in the throwaway server.
const sandboxSuperuser = "postgres"

// BundledMigrations is the latest migration version this binary ships for
// each track. A restored database whose tracking table is ahead of it cannot
// be served by this binary.
type BundledMigrations struct {
	// Core is the latest version in migration/. It must be positive.
	Core int64
	// Local is the latest version in migration/local/ (#3428), 0 when the
	// fork ships none.
	Local int64
}

// VerifyOptions configures Verify.
type VerifyOptions struct {
	// Sandbox runs the throwaway PostgreSQL server. Required.
	Sandbox Sandbox
	// Identities decrypt encrypted generations. Required for those only.
	Identities []age.Identity
	// Bundled is compared with the restored tracking tables. Required.
	Bundled BundledMigrations
	// TempDir is where the decrypted dump is staged on this host; empty
	// means os.TempDir().
	TempDir string
	// ElythiaVersion is recorded in the result; empty means the running
	// binary's version.
	ElythiaVersion string
	// Now returns the verification time; nil means time.Now.
	Now func() time.Time
}

// stageError is a verification failure caused by the generation itself, as
// opposed to an error of the environment (storage, sandbox, configuration).
type stageError struct{ msg string }

func (e *stageError) Error() string { return e.msg }

func failf(format string, args ...any) error {
	return &stageError{msg: fmt.Sprintf(format, args...)}
}

// Verify checks that generation id can be read, restored into a throwaway
// PostgreSQL server and served by this binary, and stores the result as
// verify.json next to meta.json. The stored dump and meta.json are only read.
//
// A failing generation is reported through VerifyResult.OK with a nil error.
// The error is non-nil when verification itself could not be carried out
// (the generation has no meta.json, the storage or the sandbox failed, the
// throwaway server lacks an extension the dump needs, the options are
// incomplete), when the throwaway server could not be cleaned up, or when
// verify.json could not be stored. A nil error therefore means verify.json
// has been stored with the returned result.
func Verify(ctx context.Context, s Storage, id string, opts VerifyOptions) (VerifyResult, error) {
	if !ValidID(id) {
		return VerifyResult{}, fmt.Errorf("backup verify: invalid generation id %q", id)
	}
	if opts.Sandbox == nil {
		return VerifyResult{}, errors.New("backup verify: no sandbox")
	}
	// 同梱の番号が分からないまま進むと「進みすぎていないか」の比較が常に通る。
	if opts.Bundled.Core <= 0 {
		return VerifyResult{}, errors.New("backup verify: the bundled core migration version is unknown")
	}
	meta, err := ReadMeta(ctx, s, id)
	if errors.Is(err, ErrNotFound) {
		// meta.json の無い世代は取り終わっていない (Meta の GoDoc)。
		return VerifyResult{}, fmt.Errorf("backup verify: generation %s has no %s (incomplete)", id, MetaFile)
	}
	if err != nil {
		return VerifyResult{}, fmt.Errorf("backup verify: %w", err)
	}

	v := &verifier{storage: s, id: id, meta: *meta, opts: opts}
	stages, verr := v.run(ctx)

	version := opts.ElythiaVersion
	if version == "" {
		version = config.MkGoVersion
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	res := VerifyResult{
		ID:             id,
		VerifiedAt:     now().UTC(),
		OK:             verr == nil && len(stages) == 3 && !slices.ContainsFunc(stages, func(r StageResult) bool { return !r.OK }),
		Stages:         stages,
		Mismatches:     v.mismatches,
		ElythiaVersion: version,
	}
	if verr != nil {
		return res, verr
	}
	if err := writeVerifyResult(ctx, s, res); err != nil {
		return res, err
	}
	return res, nil
}

func writeVerifyResult(ctx context.Context, s Storage, res VerifyResult) error {
	body, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return fmt.Errorf("backup verify: encode %s: %w", VerifyFile, err)
	}
	if err := s.Put(ctx, Key(res.ID, VerifyFile), bytes.NewReader(body)); err != nil {
		return fmt.Errorf("backup verify: write %s: %w", VerifyFile, err)
	}
	return nil
}

// ResolveGenerationID turns a command-line argument into a generation ID.
// "latest" is the newest generation that has meta.json; anything else must be
// a valid ID.
func ResolveGenerationID(ctx context.Context, s Storage, arg string) (string, error) {
	if arg != "latest" {
		if !ValidID(arg) {
			return "", fmt.Errorf("backup: invalid generation id %q", arg)
		}
		return arg, nil
	}
	objs, err := s.List(ctx, GenerationPrefix())
	if err != nil {
		return "", fmt.Errorf("backup: list generations: %w", err)
	}
	latest := ""
	for _, o := range objs {
		id := GenerationIDFromKey(o.Key)
		if id != "" && o.Key == Key(id, MetaFile) && id > latest {
			latest = id
		}
	}
	if latest == "" {
		return "", errors.New("backup: no complete generation")
	}
	return latest, nil
}

type verifier struct {
	storage    Storage
	id         string
	meta       Meta
	opts       VerifyOptions
	mismatches []RowMismatch
	dumpPath   string
}

// run executes the stages in order. After a failed stage the rest are
// recorded as skipped. A non-nil error is an environment error.
func (v *verifier) run(ctx context.Context) (stages []StageResult, err error) {
	record := func(stage VerifyStage, serr error, warnings []string) (bool, error) {
		var se *stageError
		switch {
		case serr == nil:
			stages = append(stages, StageResult{Stage: stage, OK: true, Warnings: warnings})
			return true, nil
		case errors.As(serr, &se):
			stages = append(stages, StageResult{Stage: stage, Error: se.msg})
			return false, nil
		default:
			return false, serr
		}
	}
	skipRest := func(from int) {
		order := []VerifyStage{StageReadable, StageRestorable, StageUsable}
		for _, st := range order[from:] {
			stages = append(stages, StageResult{Stage: st, Skipped: true})
		}
	}

	stageDir, err := os.MkdirTemp(v.opts.TempDir, "elythia-verify-dump-")
	if err != nil {
		return nil, fmt.Errorf("backup verify: %w", err)
	}
	// 復号した dump には秘密鍵や token が入るので、成否に関わらず消す。
	defer func() { _ = os.RemoveAll(stageDir) }()
	v.dumpPath = filepath.Join(stageDir, "dump.pgc")

	ok, err := record(StageReadable, v.readable(ctx), nil)
	if err != nil || !ok {
		if err == nil {
			skipRest(1)
		}
		return stages, err
	}

	srv, cleanup, err := v.startServer(ctx)
	if err != nil {
		return stages, err
	}
	defer func() {
		// 使い捨てのサーバーは失敗しても必ず止めて消す。ctx が切れていても止めたい。
		if cerr := cleanup(context.WithoutCancel(ctx)); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()

	conn, db, err := connectSandbox(ctx, srv)
	if err != nil {
		return stages, err
	}
	defer func() {
		_ = conn.Close(context.WithoutCancel(ctx))
		closeGorm(db)
	}()

	// 拡張が無いのは使い捨てのサーバー (環境) の問題で、世代の欠陥ではない。
	// pg_restore の CREATE EXTENSION の失敗として restorable を落とすと、世代が
	// 壊れているように見えるので、戻す前に環境の誤りとして止める。
	if err := checkExtensions(ctx, conn, v.meta.Extensions); err != nil {
		return stages, err
	}

	ok, err = record(StageRestorable, v.restorable(ctx, srv, conn), nil)
	if err != nil || !ok {
		if err == nil {
			skipRest(2)
		}
		return stages, err
	}
	warnings, uerr := v.usable(ctx, conn, db)
	_, err = record(StageUsable, uerr, warnings)
	return stages, err
}

// readable checks the stored bytes against meta.json, decrypts them into
// v.dumpPath and runs `pg_restore --list` on the result.
func (v *verifier) readable(ctx context.Context) error {
	m := v.meta
	wantName := DumpFile
	if m.Encrypted {
		wantName = DumpFileAge
		if m.Encryption != EncryptionAge {
			return failf("unknown encryption %q", m.Encryption)
		}
		if len(v.opts.Identities) == 0 {
			return errors.New("backup verify: the generation is encrypted but no identity is configured (backup.encryption.identityFile)")
		}
	}
	// meta.json の値でキーを組み立てるので、決まった名前以外は受け付けない。
	if m.DumpFile != wantName {
		return failf("meta.json names dump file %q, want %q", m.DumpFile, wantName)
	}

	rc, err := v.storage.Get(ctx, Key(v.id, m.DumpFile))
	if errors.Is(err, ErrNotFound) {
		return failf("dump file %s is missing", m.DumpFile)
	}
	if err != nil {
		return fmt.Errorf("backup verify: read dump: %w", err)
	}
	defer func() { _ = rc.Close() }()

	f, err := os.OpenFile(v.dumpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("backup verify: %w", err)
	}
	stored := &countingHash{h: sha256.New()}
	raw := io.TeeReader(rc, stored)
	plain := io.Reader(raw)
	if m.Encrypted {
		dec, err := Decrypt(raw, v.opts.Identities...)
		if err != nil {
			_ = f.Close()
			return failf("cannot decrypt the dump: %v", err)
		}
		plain = dec
	}
	plainHash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(f, plainHash), plain)
	if copyErr == nil {
		// age は末尾の余分なバイトを読まずに終わることがあるので、保存された
		// バイト列の hash を取り切る。
		_, copyErr = io.Copy(io.Discard, raw)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("backup verify: %w", err)
	}
	if copyErr != nil {
		return failf("cannot read the dump: %v", copyErr)
	}

	if stored.n != m.DumpSize {
		return failf("dump size %d does not match meta.json (%d)", stored.n, m.DumpSize)
	}
	if got := hex.EncodeToString(stored.h.Sum(nil)); !strings.EqualFold(got, m.DumpSHA256) {
		return failf("dump sha256 %s does not match meta.json (%s)", got, m.DumpSHA256)
	}
	gotPlain := hex.EncodeToString(plainHash.Sum(nil))
	// 暗号化していない世代では PlainSHA256 を省いてよい (DumpSHA256 と同じ値になる)。
	if (m.Encrypted || m.PlainSHA256 != "") && !strings.EqualFold(gotPlain, m.PlainSHA256) {
		return failf("decrypted dump sha256 %s does not match meta.json (%s)", gotPlain, m.PlainSHA256)
	}

	return v.withDump(func(r io.Reader) error {
		if err := v.opts.Sandbox.Run(ctx, SandboxCmd{Program: "pg_restore", Args: []string{"--list"}, Stdin: r, Stdout: io.Discard}); err != nil {
			return failf("pg_restore --list failed: %v", err)
		}
		return nil
	})
}

func (v *verifier) withDump(fn func(io.Reader) error) error {
	f, err := os.Open(v.dumpPath)
	if err != nil {
		return fmt.Errorf("backup verify: %w", err)
	}
	defer func() { _ = f.Close() }()
	return fn(f)
}

// startServer creates the throwaway server. The returned cleanup stops it and
// removes its files; it is safe to call when the server never started.
func (v *verifier) startServer(ctx context.Context) (SandboxServer, func(context.Context) error, error) {
	sb := v.opts.Sandbox
	dir, err := sb.MkdirTemp(ctx)
	if err != nil {
		return SandboxServer{}, nil, fmt.Errorf("backup verify: create the sandbox directory: %w", err)
	}
	srv := sb.Server(dir)
	data := dir + "/data"
	// stop は start を試みた後だけ呼ぶ。start が失敗したときの stop の失敗は
	// 「動いていない」なので無視する (pg_ctl start -w が待ちきれずに失敗しても
	// postmaster が残っていることがあるので、start の失敗後も stop は呼ぶ)。
	stopMode := 0 // 0: 呼ばない、1: 呼んで失敗を無視、2: 呼んで失敗を返す
	cleanup := func(ctx context.Context) error {
		var errs []error
		if stopMode > 0 {
			err := sb.Run(ctx, SandboxCmd{Program: "pg_ctl", Args: []string{"stop", "-D", data, "-m", "immediate", "-w"}})
			if err != nil && stopMode == 2 {
				errs = append(errs, fmt.Errorf("backup verify: stop the throwaway server: %w", err))
			}
		}
		if err := sb.RemoveAll(ctx, dir); err != nil {
			errs = append(errs, fmt.Errorf("backup verify: remove the sandbox directory: %w", err))
		}
		return errors.Join(errs...)
	}
	fail := func(err error) (SandboxServer, func(context.Context) error, error) {
		return SandboxServer{}, nil, errors.Join(err, cleanup(context.WithoutCancel(ctx)))
	}

	// 中身は検証が終われば捨てるので、fsync を省いて速くする。
	initdb := append([]string{"-D", data, "-U", sandboxSuperuser, "--auth=trust", "--no-sync"}, initdbLocaleArgs(v.meta.DatabaseLocale)...)
	if err := sb.Run(ctx, SandboxCmd{Program: "initdb", Args: initdb}); err != nil {
		return fail(fmt.Errorf("backup verify: initdb (encoding and locale from meta.json: %+v): %w", v.meta.DatabaseLocale, err))
	}
	opts := append([]string{"-c", "fsync=off"}, srv.Options...)
	stopMode = 1
	if err := sb.Run(ctx, SandboxCmd{Program: "pg_ctl", Args: []string{
		"start", "-w", "-D", data, "-l", dir + "/server.log", "-o", strings.Join(opts, " "),
	}}); err != nil {
		return fail(fmt.Errorf("backup verify: start the throwaway server: %w", err))
	}
	stopMode = 2
	return srv, cleanup, nil
}

// initdbLocaleArgs returns the initdb options that recreate the encoding and
// locale of the dumped database. A meta.json without them (taken before
// #3458 recorded them) falls back to UTF8 and the C locale.
//
// pg_restore は戻す先の DB (initdb が作る postgres) の encoding と locale をそのまま
// 使う。元の DB と違うと、encoding の変換で落ちたり、照合順序に依る索引の並びが
// 元と変わったりするので、元と同じ値で initdb する。
func initdbLocaleArgs(l DatabaseLocale) []string {
	encoding := l.Encoding
	if encoding == "" {
		encoding = "UTF8"
	}
	args := []string{"--encoding=" + encoding, "--locale=C"}
	if l.Collate != "" {
		args = append(args, "--lc-collate="+l.Collate)
	}
	if l.Ctype != "" {
		args = append(args, "--lc-ctype="+l.Ctype)
	}
	switch l.Provider {
	case "icu":
		args = append(args, "--locale-provider=icu", "--icu-locale="+l.Locale)
	case "builtin":
		args = append(args, "--locale-provider=builtin", "--builtin-locale="+l.Locale)
	}
	return args
}

// connectSandbox opens a pgx connection (for the row counts and the tracking
// tables, read with the same functions Take uses) and a GORM handle (for
// fsck) to the throwaway server.
func connectSandbox(ctx context.Context, srv SandboxServer) (*pgx.Conn, *gorm.DB, error) {
	dsn := fmt.Sprintf("host=%s port=%d user=%s dbname=postgres sslmode=disable", srv.Host, srv.Port, sandboxSuperuser)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("backup verify: connect to the throwaway server: %w", err)
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		_ = conn.Close(context.WithoutCancel(ctx))
		return nil, nil, fmt.Errorf("backup verify: connect to the throwaway server: %w", err)
	}
	return conn, db, nil
}

func closeGorm(db *gorm.DB) {
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

// checkExtensions fails when the throwaway server cannot install an
// extension that meta.json records, so that pg_restore would stop at its
// CREATE EXTENSION.
func checkExtensions(ctx context.Context, q Queryer, exts []ExtensionInfo) error {
	missing, err := missingExtensions(ctx, q, exts)
	if err != nil {
		return fmt.Errorf("backup verify: %w", err)
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("backup verify: the backup uses extension(s) %s that the throwaway PostgreSQL server cannot install; "+
		"run verify where they are installed (the backup image has pg_bigm; other extensions need an image that adds them)",
		strings.Join(missing, ", "))
}

// missingExtensions returns the names in exts that the server q is
// connected to cannot install (pg_available_extensions). verify checks the
// throwaway server with it and restore the target server (#3461).
func missingExtensions(ctx context.Context, q Queryer, exts []ExtensionInfo) ([]string, error) {
	if len(exts) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, "SELECT name FROM pg_available_extensions")
	if err != nil {
		return nil, fmt.Errorf("list the available extensions: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list the available extensions: %w", err)
	}
	var missing []string
	for _, e := range exts {
		if !slices.Contains(names, e.Name) {
			missing = append(missing, e.Name)
		}
	}
	return missing, nil
}

// restorable restores the dump into the throwaway server and compares the
// row counts with meta.json.
func (v *verifier) restorable(ctx context.Context, srv SandboxServer, conn *pgx.Conn) error {
	// 行数が 1 つも無いと突き合わせが何も検査せずに通るので、失敗にする。
	if len(v.meta.RowCounts) == 0 {
		return failf("meta.json has no row counts to compare")
	}
	err := v.withDump(func(r io.Reader) error {
		// 所有者と権限は戻す先に同じ role が無いので落とす。検証には要らない。
		args := []string{
			"-h", srv.ToolHost, "-p", strconv.Itoa(srv.ToolPort), "-U", sandboxSuperuser, "-d", "postgres",
			"--no-owner", "--no-privileges", "--exit-on-error",
		}
		if err := v.opts.Sandbox.Run(ctx, SandboxCmd{Program: "pg_restore", Args: args, Stdin: r}); err != nil {
			return failf("pg_restore failed: %v", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// 取るときと同じ CountRows で数える。数える表の選び方 (分割表は子、拡張の持ち物は
	// 外す、など) が揃うので、食い違いは dump の中身の違いだけになる。
	got, err := CountRows(ctx, conn)
	if err != nil {
		return fmt.Errorf("backup verify: %w", err)
	}
	v.mismatches = compareRowCounts(v.meta.RowCounts, got)
	if len(v.mismatches) > 0 {
		return failf("row counts of %d table(s) do not match meta.json", len(v.mismatches))
	}
	return nil
}

// compareRowCounts returns the tables whose counts differ, sorted by name.
// A table missing on one side is reported with -1 on that side.
func compareRowCounts(want, got map[string]int64) []RowMismatch {
	tables := make([]string, 0, len(want)+len(got))
	for t := range want {
		tables = append(tables, t)
	}
	for t := range got {
		if _, ok := want[t]; !ok {
			tables = append(tables, t)
		}
	}
	slices.Sort(tables)
	var out []RowMismatch
	for _, t := range tables {
		w, wok := want[t]
		g, gok := got[t]
		if !wok {
			w = -1
		}
		if !gok {
			g = -1
		}
		if !wok || !gok || w != g {
			out = append(out, RowMismatch{Table: t, Expected: w, Actual: g})
		}
	}
	return out
}

// usable checks the tracking tables and runs the fsck queries. Counter drift
// found by fsck is returned as warnings: it is a property of the source
// database at the snapshot, not a defect of the backup.
func (v *verifier) usable(ctx context.Context, conn *pgx.Conn, db *gorm.DB) ([]string, error) {
	// 取るときと同じ ReadMigrations で読む。空の表は NilMigrationVersion (-1) になり、
	// meta.json の記録と同じ形で比べられる。
	states, err := ReadMigrations(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("backup verify: %w", err)
	}
	core, local := MigrationTables[0], MigrationTables[1]
	latest := map[string]int64{core: v.opts.Bundled.Core, local: v.opts.Bundled.Local}
	restored := map[string]MigrationState{}
	for _, st := range states {
		restored[st.Table] = st
		if st.Missing {
			// 本体の管理表が無い DB は、Elythia の DB ではない。fork の管理表は
			// migration/local/ を持たない版では作られない。
			if st.Table == core {
				return nil, failf("%s does not exist", st.Table)
			}
			continue
		}
		if st.Dirty {
			return nil, failf("%s is dirty at version %d", st.Table, st.Version)
		}
		if st.Version > latest[st.Table] {
			return nil, failf("%s is at version %d, newer than this binary's %d", st.Table, st.Version, latest[st.Table])
		}
	}
	for _, want := range v.meta.Migrations {
		got, ok := restored[want.Table]
		if ok && got != want {
			return nil, failf("%s is %+v after restore but meta.json records %+v", want.Table, got, want)
		}
	}

	rep, err := fsck.Run(ctx, db, fsck.Options{})
	if err != nil {
		return nil, failf("fsck cannot run on the restored database: %v", err)
	}
	var warnings []string
	if len(rep.Drifts) > 0 {
		warnings = append(warnings, fmt.Sprintf("fsck: %d counter drift(s) (also present in the source database)", len(rep.Drifts)))
	}
	for _, o := range rep.Orphans {
		warnings = append(warnings, fmt.Sprintf("fsck: %d orphan row(s) in %s: %s", o.Count, o.Table, o.Reason))
	}
	return warnings, nil
}

// countingHash hashes and counts what is written to it.
type countingHash struct {
	h hash.Hash
	n int64
}

func (c *countingHash) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return c.h.Write(p)
}
