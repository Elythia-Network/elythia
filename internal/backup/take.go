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
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"

	"github.com/elythia-network/elythia/internal/config"
)

// DumpConn is how pg_dump connects to the database: a libpq connection URI
// without the password, and the password, which is passed in PGPASSWORD so
// that it does not show up in the process list.
type DumpConn struct {
	URI      string
	Password string
}

// DumpConnFromURL splits a postgres:// URL (config.Config.DatabaseURL) into a
// DumpConn for libpq.
//
// エラーに URL を入れない。この URL はパスワードを含み、db.host の書き間違いなどで
// 読めないときにそのままログへ出ると漏れる。
//
// pgx と libpq では、sslrootcert が無い verify-ca / verify-full の意味が違う。pgx は
// システムの CA で検証するが、libpq は ~/.postgresql/root.crt を探して無ければ失敗する。
// 本体と同じく「システムの CA で検証する」にするため、libpq には sslrootcert=system
// (PostgreSQL 16 以降の libpq) を渡す。libpq は system を verify-full でしか受け付けない
// ので、verify-ca でシステムの CA を使う形は pg_dump では取れない。
func DumpConnFromURL(dbURL string) (DumpConn, error) {
	u, err := url.Parse(dbURL)
	if err != nil {
		return DumpConn{}, errors.New("backup: the database URL built from db: is not a valid URL (check db.host and db.port)")
	}
	var pass string
	if u.User != nil {
		pass, _ = u.User.Password()
		u.User = url.User(u.User.Username())
	}
	q := u.Query()
	if q.Get("sslrootcert") == "" {
		switch q.Get("sslmode") {
		case "verify-full":
			q.Set("sslrootcert", "system")
			u.RawQuery = q.Encode()
		case "verify-ca":
			return DumpConn{}, errors.New("backup: db.extra.sslmode verify-ca needs db.extra.sslrootcert for pg_dump " +
				"(libpq verifies against the system CAs only with verify-full)")
		}
	}
	return DumpConn{URI: u.String(), Password: pass}, nil
}

// TakeOptions configures Take.
type TakeOptions struct {
	Storage Storage
	// DatabaseURL is the pgx connection string used to export the snapshot
	// and to read the row counts and the metadata.
	DatabaseURL string
	// Dump is how pg_dump connects. A zero value derives it from DatabaseURL.
	// テストでは pg_dump を DB のコンテナの中で動かすので、Go 側と接続先が違う。
	Dump DumpConn
	// Runner runs pg_dump. Nil means LocalRunner.
	Runner Runner
	// PgDump is the pg_dump program. Empty means "pg_dump" from PATH.
	PgDump string
	// Recipients enables age encryption when non-empty.
	Recipients []age.Recipient
	// Now returns the time the generation ID is made from. Nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Take dumps the database with pg_dump -Fc into a new generation in the
// storage and returns its Meta once meta.json is stored.
//
// 手順:
//  1. REPEATABLE READ の transaction で pg_export_snapshot() を取り、その snapshot で
//     表ごとの行数と管理表を読む
//  2. 同じ snapshot を pg_dump --snapshot に渡し、出力を (暗号化して) 保存先へ流す
//  3. 保存先から読み直して sha256 と大きさを確かめる
//  4. 最後に meta.json を置く。meta.json の無い世代は途中で止まったものとして扱う
//
// 途中で失敗したときは、置いた dump を消してから返す。
func Take(ctx context.Context, o TakeOptions) (meta *Meta, err error) {
	if o.Storage == nil {
		return nil, errors.New("backup: no storage")
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	dump := o.Dump
	if dump.URI == "" {
		if dump, err = DumpConnFromURL(o.DatabaseURL); err != nil {
			return nil, err
		}
	}
	runner := o.Runner
	if runner == nil {
		runner = LocalRunner{}
	}
	pgDump := o.PgDump
	if pgDump == "" {
		pgDump = "pg_dump"
	}

	createdAt := now().UTC().Truncate(time.Second)
	id := NewID(createdAt)
	existing, err := o.Storage.List(ctx, Key(id, ""))
	if err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		return nil, fmt.Errorf("backup: generation %s already exists", id)
	}

	pgDumpVersion, err := readPgDumpVersion(ctx, runner, pgDump)
	if err != nil {
		return nil, err
	}

	conn, err := pgx.Connect(ctx, o.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("backup: connect: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	// pg_dump が snapshot を使い終えるまで、この transaction を開けておく必要がある。
	// 閉じると pg_dump の SET TRANSACTION SNAPSHOT が失敗する。
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("backup: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// DB やロールに statement_timeout などが設定されていると、大きな表の count(*) や、
	// pg_dump が終わるのを待つ間の idle な transaction が切られる。pg_dump 自身も
	// 自分の接続でこれらを 0 にする。
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = 0; SET LOCAL lock_timeout = 0; SET LOCAL idle_in_transaction_session_timeout = 0"); err != nil {
		return nil, fmt.Errorf("backup: set timeouts: %w", err)
	}
	var snapshot string
	if err := tx.QueryRow(ctx, "SELECT pg_export_snapshot()").Scan(&snapshot); err != nil {
		return nil, fmt.Errorf("backup: export snapshot: %w", err)
	}
	meta = &Meta{
		FormatVersion:  MetaFormatVersion,
		ID:             id,
		CreatedAt:      createdAt,
		ElythiaVersion: config.MkGoVersion,
		ElythiaCommit:  config.MkGoCommit,
		PgDumpVersion:  pgDumpVersion,
	}
	si, err := ReadServerInfo(ctx, tx)
	if err != nil {
		return nil, err
	}
	meta.PostgresVersion, meta.PostgresVersionNum, meta.Database = si.Version, si.VersionNum, si.Database
	if meta.Migrations, err = ReadMigrations(ctx, tx); err != nil {
		return nil, err
	}
	if meta.DatabaseSettings, err = ReadDatabaseSettings(ctx, tx); err != nil {
		return nil, err
	}
	if meta.DatabaseLocale, err = ReadDatabaseLocale(ctx, tx, si.VersionNum); err != nil {
		return nil, err
	}
	if meta.Extensions, err = ReadExtensions(ctx, tx); err != nil {
		return nil, err
	}
	if meta.RowCounts, err = CountRows(ctx, tx); err != nil {
		return nil, err
	}
	logger.Info("backup: counted rows", "id", id, "tables", len(meta.RowCounts), "snapshot", snapshot)

	meta.DumpFile = DumpFile
	if len(o.Recipients) > 0 {
		meta.DumpFile = DumpFileAge
		meta.Encrypted = true
		meta.Encryption = EncryptionAge
	}
	dumpKey := Key(id, meta.DumpFile)

	stored, err := streamDump(ctx, o.Storage, dumpKey, runner, pgDump, dump, snapshot, o.Recipients)
	// dump を Put した後で失敗したら、途中の世代を残さない。meta.json が無いので
	// 読む側は無視するが、容量の料金はかかる。
	// ErrExists は、同じ秒に始めた別のバックアップが先に置いたということ。その dump は
	// 自分のものではないので消さない。
	cleanupDump := !errors.Is(err, ErrExists)
	defer func() {
		if err != nil && cleanupDump {
			deleteQuietly(ctx, o.Storage, logger, dumpKey)
		}
	}()
	if err != nil {
		return nil, err
	}
	// pg_dump は snapshot を使い終えたので、行数を数えた transaction はもう要らない。
	_ = tx.Rollback(ctx)
	meta.DumpSize, meta.DumpSHA256, meta.PlainSHA256 = stored.size, stored.sha256, stored.plainSHA256
	logger.Info("backup: uploaded dump", "id", id, "key", dumpKey, "bytes", stored.size)

	if err = checkStored(ctx, o.Storage, dumpKey, stored.size, stored.sha256); err != nil {
		return nil, err
	}

	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("backup: encode meta: %w", err)
	}
	if err = putNew(ctx, o.Storage, Key(id, MetaFile), bytes.NewReader(body)); err != nil {
		// 失敗と返ってきても、実は置けていることがある (応答が途中で切れた場合など)。
		// dump だけ消して meta.json が残ると、中身の無い世代が complete に見えるので、
		// meta.json も消す。ErrExists なら置いたのは自分ではないので消さない。
		if !errors.Is(err, ErrExists) {
			deleteQuietly(ctx, o.Storage, logger, Key(id, MetaFile))
		}
		return nil, err
	}
	logger.Info("backup: done", "id", id)
	return meta, nil
}

// deleteQuietly removes key after a failure, even when ctx is already done,
// and only logs when it cannot.
func deleteQuietly(ctx context.Context, st Storage, logger *slog.Logger, key string) {
	delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := st.Delete(delCtx, key); err != nil {
		logger.Warn("backup: failed to delete an incomplete object", "key", key, "error", err)
	}
}

// readPgDumpVersion runs `pg_dump --version` and returns its first line.
func readPgDumpVersion(ctx context.Context, runner Runner, pgDump string) (string, error) {
	out, err := runner.Command(ctx, nil, pgDump, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("backup: %s --version: %w", pgDump, err)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return line, nil
}

type storedDump struct {
	size        int64
	sha256      string
	plainSHA256 string
}

// streamDump runs pg_dump and streams its output, encrypted when recipients
// are given, into storage under key.
func streamDump(ctx context.Context, st Storage, key string, runner Runner, pgDump string, dump DumpConn, snapshot string, recipients []age.Recipient) (storedDump, error) {
	dumpCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	pr, pw := io.Pipe()
	// 読み手を先に動かす。age.Encrypt は作った時点でヘッダーを書くので、読み手が
	// いないと pipe への書き込みで止まる。
	putErr := make(chan error, 1)
	go func() {
		err := putNew(ctx, st, key, pr)
		if err != nil {
			// 保存先が読むのをやめたら pg_dump も止める。止めないと pipe が詰まって
			// pg_dump が書き込みで止まり、Wait が返らない。
			cancel()
		}
		_ = pr.CloseWithError(errOr(err, errors.New("backup: storage stopped reading")))
		putErr <- err
	}()
	// abort はまだ pg_dump を待っていないときの失敗を返す。pipe をエラーで閉じて、
	// 保存先に途中までの object を確定させない。
	abort := func(err error) (storedDump, error) {
		_ = pw.CloseWithError(err)
		<-putErr
		return storedDump{}, err
	}

	storedHash := sha256.New()
	counter := &countWriter{}
	sink := io.MultiWriter(pw, storedHash, counter)
	var enc io.WriteCloser
	if len(recipients) > 0 {
		var err error
		if enc, err = age.Encrypt(sink, recipients...); err != nil {
			return abort(fmt.Errorf("backup: start encryption: %w", err))
		}
		sink = enc
	}
	plainHash := sha256.New()

	var env []string
	if dump.Password != "" {
		env = append(env, "PGPASSWORD="+dump.Password)
	}
	cmd := runner.Command(dumpCtx, env, pgDump,
		"--format=custom",
		"--snapshot="+snapshot,
		"--no-password",
		"--dbname="+dump.URI,
	)
	cmd.Stdout = io.MultiWriter(sink, plainHash)
	stderr := &tailBuffer{max: 8 << 10}
	cmd.Stderr = stderr
	// pg_dump を kill した後に stdout の読み手が戻らないと Wait が返らない。
	cmd.WaitDelay = 10 * time.Second

	if err := cmd.Start(); err != nil {
		return abort(fmt.Errorf("backup: start %s: %w", pgDump, err))
	}

	waitErr := cmd.Wait()
	if waitErr != nil {
		waitErr = fmt.Errorf("backup: %s: %w: %s", pgDump, waitErr, strings.TrimSpace(stderr.String()))
	} else if enc != nil {
		if err := enc.Close(); err != nil {
			waitErr = fmt.Errorf("backup: finish encryption: %w", err)
		}
	}
	// pg_dump が失敗したときは pipe をエラーで閉じる。保存先は EOF ではなく
	// エラーを受け取るので、途中までの dump を object として確定させない。
	_ = pw.CloseWithError(waitErr)
	perr := <-putErr
	if waitErr != nil {
		// Put の失敗が先なら pg_dump は kill されただけなので、Put の方を返す。
		if perr != nil {
			return storedDump{}, perr
		}
		return storedDump{}, waitErr
	}
	if perr != nil {
		return storedDump{}, perr
	}
	return storedDump{
		size:        counter.n,
		sha256:      hex.EncodeToString(storedHash.Sum(nil)),
		plainSHA256: hex.EncodeToString(plainHash.Sum(nil)),
	}, nil
}

// checkStored reads key back from storage and compares its size and sha256.
func checkStored(ctx context.Context, st Storage, key string, size int64, sum string) error {
	gotSize, gotSum, err := HashObject(ctx, st, key)
	if err != nil {
		return err
	}
	if gotSize != size || gotSum != sum {
		return fmt.Errorf("backup: %s read back as %d bytes sha256 %s, want %d bytes sha256 %s", key, gotSize, gotSum, size, sum)
	}
	return nil
}

// HashObject reads key and returns its size and hex sha256.
func HashObject(ctx context.Context, st Storage, key string) (int64, string, error) {
	r, err := st.Get(ctx, key)
	if err != nil {
		return 0, "", err
	}
	defer r.Close()
	return hashReader(r)
}

func hashReader(r io.Reader) (int64, string, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return 0, "", fmt.Errorf("backup: read back: %w", err)
	}
	return n, hexSum(h), nil
}

func hexSum(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

func errOr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
