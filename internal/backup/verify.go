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

// sandboxRestoreRole / sandboxRestoreDB are the role that restores the dump in
// the throwaway server, and the database it restores into (owned by it).
//
// **dump は superuser で流さない。** dump は保存先から読んだ SQL で、pg_restore は
// それをそのままサーバーへ送る。検証は daemon の起動時や取った直後に自動で流れる
// ので、保存先 (S3 や別の機器) の中身にサーバー側の強い権限を渡さない。本番へ
// 戻す処理は設定の DB の利用者で流すので、そちらは保存先の中身を信頼している。使い捨てのサーバーはバックアップ用の
// サービスと同じコンテナで動き、設定ファイルと本番の DB の socket に届く。
// superuser でない role なら、サーバー側でプログラムやファイルに触る操作と、
// 信頼されない言語の関数の作成は権限で落ちる (検証は失敗し、世代の欠陥として
// 残る)。戻した後の数え上げと fsck も同じ role で行う (superuser で数えると、
// 戻した view や関数を superuser の権限で実行する)。
const (
	sandboxRestoreRole = "elythia_verify"
	sandboxRestoreDB   = "elythia_verify"
)

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
	// IntegrityKey, when set, refuses to verify a generation whose meta.json
	// is not signed with it (backup.integrityKeyFile).
	IntegrityKey []byte
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
// The error is non-nil when verification itself could not be carried out:
// the generation has no meta.json, the storage could not be read, the
// identity does not match, a program could not be run, the temporary
// directory is short of space, the throwaway server lacks an extension the
// dump needs, or the options are incomplete. verify.json is not written in
// those cases, so an earlier result stays as it was.
//
// Once every stage has a verdict, verify.json is written even when the
// throwaway server cannot be cleaned up afterwards; that failure (and a
// failure to write verify.json) is then returned as the error together with
// the result. A nil error therefore means verify.json has been stored with
// the returned result.
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
	// 署名が無い・合わない世代は判定しない (verify.json を書かない)。署名の合わない
	// 世代は戻す処理も断るので、前の結果が残っても戻されない。
	meta, err := ReadMetaVerified(ctx, s, id, opts.IntegrityKey, false)
	if errors.Is(err, ErrNotFound) {
		// meta.json の無い世代は取り終わっていない (Meta の GoDoc)。
		return VerifyResult{}, fmt.Errorf("backup verify: generation %s has no %s (incomplete)", id, MetaFile)
	}
	if err != nil {
		return VerifyResult{}, fmt.Errorf("backup verify: %w", err)
	}

	v := &verifier{storage: s, id: id, meta: *meta, opts: opts, create: createStaging, free: freeBytes}
	stages, verr, cleanupErr := v.run(ctx)

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
		DumpSHA256:     meta.DumpSHA256,
	}
	if verr != nil {
		// 環境の誤りでは世代の良し悪しが決まっていないので、verify.json を書かない
		// (前回の結果を上書きしない)。
		return res, errors.Join(verr, cleanupErr)
	}
	// 判定が出ていれば、後始末の失敗とは別に結果を残す。残さないと、壊れていると
	// 分かった世代でも前回の ok: true が残る。
	if err := writeVerifyResult(context.WithoutCancel(ctx), s, res); err != nil {
		return res, errors.Join(err, cleanupErr)
	}
	return res, cleanupErr
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
// a valid ID. When the meta.json of that newest generation cannot be read,
// it fails instead of falling back to an older generation.
//
// 読めない世代を飛ばして古い世代を確かめると、「最新は確かめた」と誤解される。
// 名指しで ID を渡せば、古い世代は確かめられる。
func ResolveGenerationID(ctx context.Context, s Storage, arg string) (string, error) {
	if arg != "latest" {
		if !ValidID(arg) {
			return "", fmt.Errorf("backup: invalid generation id %q", arg)
		}
		return arg, nil
	}
	gens, err := ListGenerations(ctx, s)
	if err != nil {
		return "", fmt.Errorf("backup: list generations: %w", err)
	}
	for i := len(gens) - 1; i >= 0; i-- {
		g := gens[i]
		switch {
		case g.MetaError != nil:
			return "", fmt.Errorf("backup: the newest generation %s has an unreadable %s (%v); give an older generation's ID to verify it", g.ID, MetaFile, g.MetaError)
		case g.Meta != nil:
			return g.ID, nil
		}
	}
	return "", errors.New("backup: no complete generation")
}

type verifier struct {
	storage    Storage
	id         string
	meta       Meta
	opts       VerifyOptions
	mismatches []RowMismatch
	dumpPath   string
	// create opens the staging file for the decrypted dump.
	create func(path string) (io.WriteCloser, error)
	// free reports the free space under the staging directory.
	free func(dir string) (int64, error)
}

func createStaging(path string) (io.WriteCloser, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
}

// run executes the stages in order. After a failed stage the rest are
// recorded as skipped. err is an environment error; cleanupErr is a failure
// to stop or remove the throwaway server, reported separately so that a
// verdict is still stored.
func (v *verifier) run(ctx context.Context) (stages []StageResult, err, cleanupErr error) {
	record := func(stage VerifyStage, serr error, warnings []string) (bool, error) {
		r, ok, err := stageOutcome(ctx, stage, serr, warnings)
		if err != nil {
			return false, err
		}
		stages = append(stages, r)
		return ok, nil
	}
	skipRest := func(from int) {
		order := []VerifyStage{StageReadable, StageRestorable, StageUsable}
		for _, st := range order[from:] {
			stages = append(stages, StageResult{Stage: st, Skipped: true})
		}
	}

	stageDir, err := os.MkdirTemp(v.opts.TempDir, "elythia-verify-dump-")
	if err != nil {
		return nil, fmt.Errorf("backup verify: %w", err), nil
	}
	// 復号した dump には秘密鍵や token が入るので、成否に関わらず消す。
	defer func() { _ = os.RemoveAll(stageDir) }()
	v.dumpPath = filepath.Join(stageDir, "dump.pgc")

	ok, err := record(StageReadable, v.readable(ctx), nil)
	if err != nil || !ok {
		if err == nil {
			skipRest(1)
		}
		return stages, err, nil
	}

	srv, cleanup, err := v.startServer(ctx)
	if err != nil {
		return stages, err, nil
	}
	defer func() {
		// 使い捨てのサーバーは失敗しても必ず止めて消す。ctx が切れていても止めたい。
		cleanupErr = cleanup(context.WithoutCancel(ctx))
	}()

	// 拡張が無いのは使い捨てのサーバー (環境) の問題で、世代の欠陥ではない。
	// pg_restore の CREATE EXTENSION の失敗として restorable を落とすと、世代が
	// 壊れているように見えるので、戻す前に環境の誤りとして止める。
	if err := prepareRestoreTarget(ctx, srv, v.meta.Extensions); err != nil {
		return stages, err, nil
	}

	conn, db, err := connectSandbox(ctx, srv, sandboxRestoreRole, sandboxRestoreDB)
	if err != nil {
		return stages, err, nil
	}
	defer func() {
		_ = conn.Close(context.WithoutCancel(ctx))
		closeGorm(db)
	}()

	ok, err = record(StageRestorable, v.restorable(ctx, srv, conn), nil)
	if err != nil || !ok {
		if err == nil {
			skipRest(2)
		}
		return stages, err, nil
	}
	warnings, uerr := v.usable(ctx, conn, db)
	_, err = record(StageUsable, uerr, warnings)
	return stages, err, nil
}

// stageOutcome turns the result of one stage into its StageResult, or into
// an environment error.
//
// 段の途中で ctx が切れた (Ctrl-C、SIGTERM) ときの失敗は、世代の良し悪しではない。
// 切れた接続で fsck や行数の数え上げが落ちたものを世代の欠陥として残すと、前回の
// ok: true を ok: false で上書きしてしまう (daemon の restoreVerifyFile と同じ考え方)。
func stageOutcome(ctx context.Context, stage VerifyStage, serr error, warnings []string) (StageResult, bool, error) {
	var se *stageError
	switch {
	case serr == nil:
		return StageResult{Stage: stage, OK: true, Warnings: warnings}, true, nil
	case errors.As(serr, &se) && ctx.Err() == nil:
		return StageResult{Stage: stage, Error: se.msg}, false, nil
	case errors.As(serr, &se):
		return StageResult{}, false, fmt.Errorf("backup verify: interrupted during %s (%s): %w", stage, se.msg, ctx.Err())
	default:
		return StageResult{}, false, serr
	}
}

// TempDirWarning returns a warning when verification would stage the
// decrypted dump and the throwaway server under the default temporary
// directory: tempDir (VerifyOptions.TempDir) is empty and getenv("TMPDIR")
// is unset. It returns "" otherwise.
//
// 空きの検査は dump の大きさを下限にしているだけで、戻した DB の大きさは測れない。
// 既定の /tmp は、compose ではコンテナの書き込み層で、本番の DB の volume と同じ
// ディスクであることが多い。埋まると本番の PostgreSQL が WAL を書けなくなるので、
// 別のディスクへ向ける手順へ誘導する。
func TempDirWarning(tempDir string, getenv func(string) string) string {
	if tempDir != "" || getenv("TMPDIR") != "" {
		return ""
	}
	return "TMPDIR is not set, so verification stages the decrypted dump and the restored database under " + os.TempDir() +
		", which may be on the same disk as the production database; point TMPDIR at another disk (docs/backup.md)"
}

// requireFree fails when dir has less than need bytes free. free reports the
// free space; an error from it is an environment error too.
func requireFree(what, dir string, need int64, free func(string) (int64, error)) error {
	got, err := free(dir)
	if err != nil {
		return fmt.Errorf("backup verify: check the free space of %s: %w", what, err)
	}
	if got < need {
		return fmt.Errorf("backup verify: %s has %d bytes free, less than the %d bytes this generation needs at least; "+
			"point TMPDIR at a larger disk that is not the database's", what, got, need)
	}
	return nil
}

// envMarkers are pg_restore / server messages that come from the
// environment, not from the dump.
var envMarkers = []string{
	"No space left on device",
	"Disk quota exceeded",
	"out of memory",
	"could not connect to server",
	"connection to server",
	"server closed the connection unexpectedly",
	"terminating connection",
}

// programVerdict turns the error of a sandboxed program that read the dump
// into a stage failure when the program ran and rejected the dump, or into
// an environment error otherwise.
//
// pg_restore の失敗は終了コードでは区別できない (どれも 1)。プログラムを動かせな
// かった場合 (ProgramError でない)、ctx が切れた場合、stderr に環境の誤りの文言が
// ある場合を環境の誤りにし、残りは世代の欠陥に倒す。--exit-on-error の pg_restore が
// 落ちるのは、ほとんどが dump の中の SQL やデータを戻せないときだから。環境の文言の
// 一覧に無い環境の誤り (例: 未知の権限の問題) は世代の欠陥に見えるが、その場合でも
// プログラムの文言 (redactStderr) が verify.json の error に、全文がログに残る。
//
// ここに来るのは保存物の sha256 が meta.json と一致した後なので、戻しているのは
// 取ったときの dump そのもの。環境の誤りに倒して verify.json を書かないのは、その
// dump を pg_dump が作ったときから壊れていた場合だけ前回の結果を残しうる。
func programVerdict(ctx context.Context, what string, err error) error {
	var pe *ProgramError
	if ctx.Err() != nil || !errors.As(err, &pe) {
		return fmt.Errorf("backup verify: run %s: %w", what, err)
	}
	primary := primaryMessages(pe.Stderr)
	for _, m := range envMarkers {
		if strings.Contains(primary, m) {
			return fmt.Errorf("backup verify: %s failed because of the environment: %w", what, err)
		}
	}
	return failf("%s failed: %v", what, err)
}

// primaryMessages keeps the lines of pg_restore's standard error that carry
// its own messages and the server's primary messages, with double-quoted
// parts removed.
//
// DETAIL / CONTEXT / HINT / "Command was:" の行と、引用符の中には、利用者が決め
// られる値 (投稿の本文、識別子) が出る。そこに「No space left on device」と書かれて
// いるだけで環境の誤りに倒れ、壊れた世代を判定しないまま前回の結果が残るのを防ぐ。
func primaryMessages(stderr string) string {
	var b strings.Builder
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "pg_restore:") && !strings.HasPrefix(line, "ERROR:") && !strings.HasPrefix(line, "FATAL:") {
			continue
		}
		b.WriteString(stripQuoted(line))
		b.WriteByte('\n')
	}
	return b.String()
}

// stripQuoted removes double-quoted parts of s. An unterminated quote
// removes the rest of the line.
func stripQuoted(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '"':
			in = !in
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// decryptFailure classifies a failure of age to open the encrypted dump.
//
// age はヘッダの破損と鍵の不一致を区別しない (どちらも NoIdentityMatchError に
// なりうる)。エラーだけで「鍵の誤り」(環境の誤り) に分けると、ヘッダが壊れた世代を
// 判定しないまま前回の ok: true が残る (#3459 のレビュー2周目 H1)。保存物を最後まで
// 読んで大きさと sha256 を取り切り、meta.json と一致したときだけ鍵の誤りにする。
func (v *verifier) decryptFailure(ctx context.Context, derr error, src *errReader, raw io.Reader, stored *countingHash) error {
	var noMatch *age.NoIdentityMatchError
	if !errors.As(derr, &noMatch) {
		if src.err != nil {
			return fmt.Errorf("backup verify: read dump: %w", src.err)
		}
		return failf("cannot decrypt the dump: %v", derr)
	}
	_, _ = io.Copy(io.Discard, raw)
	switch {
	case src.err != nil:
		return fmt.Errorf("backup verify: read dump: %w", src.err)
	case ctx.Err() != nil:
		return fmt.Errorf("backup verify: %w", ctx.Err())
	}
	m := v.meta
	if stored.n == m.DumpSize && strings.EqualFold(hex.EncodeToString(stored.h.Sum(nil)), m.DumpSHA256) {
		return fmt.Errorf("backup verify: backup.encryption.identityFile does not match the key the generation was encrypted to: %w", derr)
	}
	return failf("cannot decrypt the dump, and the stored bytes do not match meta.json (size %d, want %d): %v", stored.n, m.DumpSize, derr)
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

	key := Key(v.id, m.DumpFile)
	info, err := v.storage.Stat(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return failf("dump file %s is missing", m.DumpFile)
	}
	if err != nil {
		return fmt.Errorf("backup verify: stat dump: %w", err)
	}
	// 大きさの照合を空きの検査より先に行う。meta.json の dumpSize が化けていると、
	// 空きの検査が「足りない」(環境の誤り) で止まり、壊れた世代を判定しないまま
	// 前回の結果が残る。
	if info.Size != m.DumpSize {
		return failf("dump size %d does not match meta.json (%d)", info.Size, m.DumpSize)
	}
	// 復号した dump を置く前に、少なくとも dump の大きさの空きがあるかを見る。
	stageDir := filepath.Dir(v.dumpPath)
	free := v.free
	if free == nil {
		free = freeBytes
	}
	if err := requireFree("the temporary directory "+stageDir, stageDir, m.DumpSize, free); err != nil {
		return err
	}

	rc, err := v.storage.Get(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return failf("dump file %s is missing", m.DumpFile)
	}
	if err != nil {
		return fmt.Errorf("backup verify: read dump: %w", err)
	}
	defer func() { _ = rc.Close() }()

	f, err := v.create(v.dumpPath)
	if err != nil {
		return fmt.Errorf("backup verify: %w", err)
	}
	// 読み出し (保存先) と書き込み (一時ファイル) の失敗は環境の誤り。どちらで
	// 起きたかを覚えておき、復号の失敗 (世代の欠陥) と区別する。
	src := &errReader{r: rc}
	dst := &errWriter{w: f}
	stored := &countingHash{h: sha256.New()}
	raw := io.TeeReader(src, stored)
	plain := io.Reader(raw)
	if m.Encrypted {
		dec, err := Decrypt(raw, v.opts.Identities...)
		if err != nil {
			_ = f.Close()
			return v.decryptFailure(ctx, err, src, raw, stored)
		}
		plain = dec
	}
	plainHash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(dst, plainHash), plain)
	if copyErr == nil {
		// age は末尾の余分なバイトを読まずに終わることがあるので、保存された
		// バイト列の hash を取り切る。
		_, copyErr = io.Copy(io.Discard, raw)
	}
	closeErr := f.Close()
	switch {
	case src.err != nil:
		return fmt.Errorf("backup verify: read dump: %w", src.err)
	case dst.err != nil:
		return fmt.Errorf("backup verify: write the decrypted dump to the temporary directory: %w", dst.err)
	case closeErr != nil:
		return fmt.Errorf("backup verify: write the decrypted dump to the temporary directory: %w", closeErr)
	case ctx.Err() != nil:
		return fmt.Errorf("backup verify: %w", ctx.Err())
	case copyErr != nil:
		// 残るのは復号の途中の失敗 (暗号文の改ざんや欠け)。
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
			return programVerdict(ctx, "pg_restore --list", err)
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

	// 戻した DB は圧縮した dump より小さくならない (dump は表のデータを圧縮して持ち、
	// 索引を持たない)。dump の大きさは必ず要る量の下限なので、これを下回る空きで
	// 止めても、通るはずの検証を止めることはない。一時ファイルの dump を置いた後に
	// 測るので、同じディスクならその分も引かれている。
	if rep, ok := sb.(SpaceReporter); ok {
		free := func(d string) (int64, error) { return rep.FreeBytes(ctx, d) }
		if err := requireFree("the sandbox directory "+dir, dir, v.meta.DumpSize, free); err != nil {
			return fail(err)
		}
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
func connectSandbox(ctx context.Context, srv SandboxServer, user, dbname string) (*pgx.Conn, *gorm.DB, error) {
	dsn := fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=disable", srv.Host, srv.Port, user, dbname)
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

// prepareRestoreTarget creates, as the superuser, the role and the database
// the dump is restored into, and installs the extensions meta.json records so
// that the role does not need to.
//
// 拡張の作成には superuser が要るものがある (pg_bigm など)。dump の
// `CREATE EXTENSION IF NOT EXISTS` は作ってあれば何もしないが、続く
// `COMMENT ON EXTENSION` は拡張の所有者でないと落ちるので、作った拡張の所有者を
// 戻す role に渡す (使い捨てのサーバーの中だけの操作)。拡張のスクリプトは image に
// 入っているもので、dump から来るものではない。public 以外の schema の拡張は作らない
// (dump の `CREATE SCHEMA` とぶつかる)。dump の側で作るので、信頼された拡張なら通り、
// そうでなければ戻すところで落ちる。
func prepareRestoreTarget(ctx context.Context, srv SandboxServer, exts []ExtensionInfo) error {
	conn, err := pgx.Connect(ctx, fmt.Sprintf("host=%s port=%d user=%s dbname=postgres sslmode=disable", srv.Host, srv.Port, sandboxSuperuser))
	if err != nil {
		return fmt.Errorf("backup verify: connect to the throwaway server: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if err := checkExtensions(ctx, conn, exts); err != nil {
		return err
	}
	role := pgx.Identifier{sandboxRestoreRole}.Sanitize()
	for _, q := range []string{
		"CREATE ROLE " + role + " LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS",
		"CREATE DATABASE " + pgx.Identifier{sandboxRestoreDB}.Sanitize() + " OWNER " + role,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			return fmt.Errorf("backup verify: prepare the restore target: %w", err)
		}
	}

	var names []string
	for _, e := range exts {
		if e.Schema == "public" {
			names = append(names, e.Name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	target, err := pgx.Connect(ctx, fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=disable", srv.Host, srv.Port, sandboxSuperuser, sandboxRestoreDB))
	if err != nil {
		return fmt.Errorf("backup verify: connect to the throwaway server: %w", err)
	}
	defer func() { _ = target.Close(context.WithoutCancel(ctx)) }()
	for _, name := range names {
		if _, err := target.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH SCHEMA public"); err != nil {
			return fmt.Errorf("backup verify: install extension %s in the throwaway server: %w", name, err)
		}
	}
	if _, err := target.Exec(ctx,
		`UPDATE pg_catalog.pg_extension SET extowner = (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = $1) WHERE extname = ANY($2)`,
		sandboxRestoreRole, names); err != nil {
		return fmt.Errorf("backup verify: hand the extensions to the restore role: %w", err)
	}
	return nil
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
			"-h", srv.ToolHost, "-p", strconv.Itoa(srv.ToolPort), "-U", sandboxRestoreRole, "-d", sandboxRestoreDB,
			"--no-owner", "--no-privileges", "--exit-on-error",
		}
		if err := v.opts.Sandbox.Run(ctx, SandboxCmd{Program: "pg_restore", Args: args, Stdin: r}); err != nil {
			return programVerdict(ctx, "pg_restore", err)
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
	if n := rep.DriftTotal(); n > 0 {
		warnings = append(warnings, fmt.Sprintf("fsck: %d counter drift(s) (also present in the source database)", n))
	}
	for _, o := range rep.Orphans {
		warnings = append(warnings, fmt.Sprintf("fsck: %d orphan row(s) in %s: %s", o.Count, o.Table, o.Reason))
	}
	return warnings, nil
}

// errReader remembers the first error of the reader it wraps.
type errReader struct {
	r   io.Reader
	err error
}

func (e *errReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && err != io.EOF && e.err == nil {
		e.err = err
	}
	return n, err
}

// errWriter remembers the first error of the writer it wraps.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	n, err := e.w.Write(p)
	if err != nil && e.err == nil {
		e.err = err
	}
	return n, err
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
