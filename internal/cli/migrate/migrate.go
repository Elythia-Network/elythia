// Package migrate implements "elythia migrate", which applies or rolls back
// the SQL migrations under migration/ (the core track) and, for forks,
// migration/local/ (the local track, #3428).
package migrate

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net/url"
	"os"
	"strconv"

	gomigrate "github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	"github.com/elythia-network/elythia/internal/cli/cliflag"
	"github.com/elythia-network/elythia/internal/config"
)

const (
	// CoreDir holds the core migrations, relative to the working directory.
	// `elythia doctor` counts the same directory.
	CoreDir = "migration"
	// LocalDir holds a fork's own migrations (#3428). Elythia itself ships
	// none; an absent or empty directory makes the local track a no-op.
	//
	// CoreDir の下に置くので、image へは `COPY /app/migration` がそのまま運ぶ。
	// golang-migrate の file source はサブディレクトリを読まないので、core の
	// 系列に local の番号が混ざることはない (TestCoreSourceIgnoresLocalDir)。
	LocalDir = "migration/local"
	// LocalTable is the golang-migrate tracking table of the local track.
	//
	// **core の `schema_migrations` と分けることが要点。** golang-migrate は適用した
	// 番号の一覧を持たず、最後の 1 行だけを持って「それより大きいもの」を当てる。
	// fork が 900001 を core と同じディレクトリに置いて同じ表で流すと、後から本体が
	// 足した 000117 は 900001 より小さいので**エラーも出ずに飛ばされる**。
	// ディレクトリだけ分けて表を共有すると、core の側が自分の知らない 900001 に
	// 当たって `no migration found for version 900001` で止まる。
	LocalTable = "schema_migrations_local"
)

// Track names accepted by -track.
const (
	trackCore  = "core"
	trackLocal = "local"
)

// migrator is the part of *gomigrate.Migrate this command drives.
type migrator interface {
	Up() error
	Down() error
	Steps(n int) error
	Force(version int) error
	Version() (version uint, dirty bool, err error)
	Close() (source error, database error)
}

// env carries the process-level dependencies so tests can replace them.
type env struct {
	stdout io.Writer
	// setLogger installs the process-wide slog logger. テストでは slog.Default を
	// 張り替えないように差し替える (-shuffle で後続のテストに漏れるため)。
	setLogger func(*slog.Logger)
	open      func(sourceURL, databaseURL string) (migrator, error)
	// databaseURL builds the golang-migrate database URL from the config. テストで
	// 専用 schema に向けるために差し替える。
	databaseURL func(*config.Config) string
	// coreDir / localDir are the migration directories of the two tracks.
	coreDir, localDir string
}

func defaultEnv() env {
	return env{
		stdout:    os.Stdout,
		setLogger: slog.SetDefault,
		open: func(src, db string) (migrator, error) {
			return gomigrate.New(src, db)
		},
		// scheme は pgx5 (golang-migrate の pgx/v5 driver)。lib/pq を使う
		// `postgres` driver は使わない (#2628: GO-2026-6173 に修正版が無く、
		// 依存を残すと govulncheck が通らない)。driver 側が接続直前に scheme を
		// `postgres` へ書き戻して `sql.Open("pgx/v5", ...)` するので、**DSN の形は
		// libpq 互換のまま**でよい。pgx の ParseConfig も libpq 互換なので UDS の
		// 書き方も変わらない。
		// DSN の組み立て (TLS 設定・資格情報のエスケープ・UDS の扱い) は本体と
		// 共通の config.DatabaseURL に任せる。以前はここで独自に組んでおり、
		// db.extra.ssl を見ずに常に sslmode=disable で繋ぎ、TCP 経路では
		// パスワードをエスケープせずに URL へ埋めていた。
		databaseURL: func(cfg *config.Config) string { return cfg.DatabaseURL("pgx5") },
		coreDir:     CoreDir,
		localDir:    LocalDir,
	}
}

// Run implements "elythia migrate" and returns the process exit code.
func Run(args []string) int { return run(defaultEnv(), os.Stderr, args) }

func run(e env, flagOut io.Writer, args []string) int {
	fs := cliflag.New("migrate", flagOut)
	configPath := fs.String("config", ".config/default.yml", "path to configuration file")
	direction := fs.String("direction", "up", "migration direction: up or down")
	steps := fs.Int("steps", 0, "number of steps (0 = all)")
	track := fs.String("track", "", "migration track: core (migration/) or local (migration/local/). "+
		"Required for -direction down and for -steps; up without it applies core, then local")
	force := fs.String("force", "", "set the tracking table of -track to this version and clear dirty, "+
		"without running any migration. The version must be a migration of that track")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	msg := validateFlags(*direction, *steps, *track)
	if set["force"] {
		msg = validateForceFlags(set, *track)
	}
	if msg != "" {
		fmt.Fprintf(flagOut, "elythia migrate: %s\n", msg)
		fs.Usage()
		return 2
	}
	var forceVersion uint
	if set["force"] {
		v, err := strconv.ParseUint(*force, 10, 0)
		// golang-migrate の Force は int を取るので、int に収まらない番号も弾く。
		if err != nil || v > math.MaxInt {
			fmt.Fprintf(flagOut, "elythia migrate: invalid -force %q (want a migration version)\n", *force)
			fs.Usage()
			return 2
		}
		forceVersion = uint(v)
	}

	logger := slog.New(slog.NewTextHandler(e.stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	e.setLogger(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "error", err)
		return 1
	}

	if set["force"] {
		return e.forceTrack(logger, cfg, *track, forceVersion)
	}

	tracks := []string{*track}
	if *track == "" {
		// up の既定は両方。**core を先に流す。** fork の migration は本体の表に
		// 列を足すことがあるので、本体の側が揃ってからでないと当たらない。
		tracks = []string{trackCore, trackLocal}
	}
	for _, name := range tracks {
		if code := e.runTrack(logger, cfg, name, *direction, *steps); code != 0 {
			// core が落ちたら local は流さない。中途半端な core の上に fork の
			// migration を重ねると、失敗の原因が 2 か所に散る。
			return code
		}
	}
	return 0
}

// validateFlags returns a message for an invalid flag combination, or "".
//
// **DB に繋ぐ前に flag の値を検査する。** 以前は不正な -direction を接続の後で
// 弾いていた。-steps の負の値は「0 より大きくない」ので「全部」と同じ扱いになり、
// `-direction down -steps -1` が全段の down (全テーブルが消える) になっていた。
func validateFlags(direction string, steps int, track string) string {
	if direction != "up" && direction != "down" {
		return fmt.Sprintf("invalid -direction %q (want up or down)", direction)
	}
	if steps < 0 {
		return fmt.Sprintf("-steps must be 0 or greater, got %d", steps)
	}
	if track != "" && track != trackCore && track != trackLocal {
		return fmt.Sprintf("invalid -track %q (want core or local)", track)
	}
	// **down は系列の指定を必須にする** (#3428)。系列が 2 つになったので、
	// 「どちらを戻すか」を省略させると、`-steps 1` のつもりで両方の系列を 1 段ずつ
	// 戻したり、意図しない系列を戻したりする。戻す操作は取り返しがつかない。
	if direction == "down" && track == "" {
		return "-direction down requires -track core or -track local"
	}
	// -steps は 1 つの系列の段数なので、2 つの系列にまたがって数えさせない。
	if steps > 0 && track == "" {
		return "-steps requires -track core or -track local"
	}
	return ""
}

// validateForceFlags returns a message for an invalid flag combination with
// -force, or "".
//
// **-force は管理表を書き換えるだけで、取り消す手段が無い。** 系列を省略させると、
// 本体の管理表に fork の番号を書く (またはその逆) 取り違えが起きる。-direction /
// -steps と一緒に書かれたときも、どちらを意図したのか分からないので拒否する。
func validateForceFlags(set map[string]bool, track string) string {
	if set["direction"] || set["steps"] {
		return "-force cannot be combined with -direction or -steps"
	}
	if track != trackCore && track != trackLocal {
		return "-force requires -track core or -track local"
	}
	return ""
}

// forceTrack sets the tracking table of one track to version and clears
// dirty, without running any migration.
//
// golang-migrate の Force は、番号が同梱のファイルに実在するかを見ずに何でも書く。
// **打ち間違えた番号を書くと、間の migration が黙って飛ばされるか、当たった
// migration が次の up でもう一度流れる。** 本体の migration は流し直してよいように
// 書かれていない (000077 は登録申請を全て消す。#3453) ので、その系列の同梱の
// ファイルに実在する番号だけを受ける。管理表を空にする -1 (NilVersion) も、同じ
// 理由で受けない。
func (e env) forceTrack(logger *slog.Logger, cfg *config.Config, track string, version uint) int {
	log := logger.With("track", track)
	dir := e.coreDir
	if track == trackLocal {
		dir = e.localDir
	}
	ok, err := hasUpMigration(dir, version)
	if err != nil {
		log.Error("failed to read migrations", "dir", dir, "error", err)
		return 1
	}
	if !ok {
		log.Error("no such migration in this track; refusing to force", "dir", dir, "version", version)
		return 1
	}

	dbURL := e.databaseURL(cfg)
	if track == trackLocal {
		dbURL, err = withMigrationsTable(dbURL, LocalTable)
		if err != nil {
			// url.Error は URL 全体 (パスワードを含む) を文面に持つ。
			log.Error("failed to build database URL for the local track")
			return 1
		}
	}
	m, err := e.open("file://"+dir, dbURL)
	if err != nil {
		logDBError(log, cfg, "failed to create migrator", err)
		return 1
	}
	defer m.Close()

	// **dirty でない管理表は書き換えない。** -force は中断した migration の後始末に
	// 限る。dirty でない表を小さい番号へ戻すと、当たった migration が次の up で
	// もう一度流れる。開く処理は advisory lock を待つので、流れている最中の migrate
	// と並べて打つと、その完了を待ってから当たった版を巻き戻してしまう (完了後の
	// 表は dirty でないので、ここで止まる)。空の表 (ErrNilVersion) も、当たって
	// いない migration を当たったことにするだけなので受けない。
	before, dirty, err := m.Version()
	switch {
	case errors.Is(err, gomigrate.ErrNilVersion):
		// 空の表と、最初の migration の down が落ちた表 (-1 で dirty) がここに来る。
		log.Error("the tracking table is empty, or -1 after a failed down of the first migration; " +
			"refusing to force (see docs/deployment.md)")
		return 1
	case err != nil:
		logDBError(log, cfg, "failed to read the current version", err)
		return 1
	case !dirty:
		log.Error("the tracking table is not dirty; refusing to force (-force only recovers a dirty table)",
			"version", before)
		return 1
	case before == version:
		// **記録された番号そのものは、どちらの失敗でも正解にならない。** up で
		// 落ちたら記録された番号は当たっておらず (1 つ前が正解)、down で落ちたら
		// 記録は戻す先なので、失敗したファイルは記録の次。そのまま -force すると、
		// up の失敗では当たっていない migration が黙って飛ばされる。
		log.Error("refusing to force the recorded version itself; after a failed up force the previous migration, "+
			"after a failed down the next one (see docs/deployment.md)", "version", before)
		return 1
	}
	// 書き換える前の値を残す。誤って流したときに、元へ戻す手がかりになる。
	log.Info("tracking table before force", "version", before, "dirty", dirty)

	if err := m.Force(int(version)); err != nil {
		logDBError(log, cfg, "force failed", err)
		return 1
	}
	log.Info("forced the tracking table", "version", version, "dirty", false)
	return 0
}

// hasUpMigration reports whether dir has an up migration numbered version.
func hasUpMigration(dir string, version uint) (bool, error) {
	versions, err := upVersions(dir)
	if err != nil {
		return false, err
	}
	for _, v := range versions {
		if v == version {
			return true, nil
		}
	}
	return false, nil
}

// runTrack migrates one track and returns the exit code.
func (e env) runTrack(logger *slog.Logger, cfg *config.Config, track, direction string, steps int) int {
	log := logger.With("track", track)
	dir := e.coreDir
	dbURL := e.databaseURL(cfg)
	if track == trackLocal {
		dir = e.localDir
		_, n, err := LatestVersion(dir)
		if err != nil {
			log.Error("failed to read local migrations", "dir", dir, "error", err)
			return 1
		}
		if n == 0 {
			// **local が無いことは異常ではない。** Elythia 本体の利用者は local を
			// 持たないので、管理表も作らずに済ませる (golang-migrate は開いた時点で
			// 管理表を作る)。明示的に戻せと言われたときだけ、戻せないことを失敗にする。
			if direction == "down" {
				log.Error("no local migrations to roll back", "dir", dir)
				return 1
			}
			log.Info("no local migrations; skipping", "dir", dir)
			return 0
		}
		dbURL, err = withMigrationsTable(dbURL, LocalTable)
		if err != nil {
			// **err をそのままログに出さない。** url.Error は URL 全体 (パスワードを
			// 含む) を文面に持つ。
			log.Error("failed to build database URL for the local track")
			return 1
		}
	}

	m, err := e.open("file://"+dir, dbURL)
	if err != nil {
		logDBError(log, cfg, "failed to create migrator", err)
		return 1
	}
	defer m.Close()

	switch {
	case direction == "up" && steps > 0:
		err = m.Steps(steps)
	case direction == "up":
		err = m.Up()
	case steps > 0:
		err = m.Steps(-steps)
	default:
		err = m.Down()
	}

	if errors.Is(err, gomigrate.ErrNoChange) {
		log.Info("no migration changes to apply")
		return 0
	}
	if err != nil {
		// direction を残す。dirty になった管理表を -force で戻すときの番号は、
		// 失敗したのが up か down かで変わる (docs/deployment.md)。
		logDBError(log.With("direction", direction), cfg, "migration failed", err)
		return 1
	}
	log.Info("migration completed", "direction", direction)
	return 0
}

// withMigrationsTable points the golang-migrate pgx5 driver at another
// tracking table.
//
// `x-migrations-table` は driver が読んで接続前に URL から落とす独自の query
// (migrate.FilterCustomQuery)。PostgreSQL には渡らない。
func withMigrationsTable(databaseURL, table string) (string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return "", fmt.Errorf("parse database url: %w", err)
	}
	q := u.Query()
	q.Set("x-migrations-table", table)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// LatestVersion returns the highest version and the number of up migrations
// in dir, using the same file-name rule as golang-migrate. A missing dir is
// not an error: it reports zero migrations.
//
// **数え方を golang-migrate に揃える。** glob で `*.up.sql` を数えると、
// golang-migrate が読まない名前 (番号の無いファイルなど) まで数えて、
// 「local がある」と判断したのに golang-migrate からは空に見える、というずれが出る。
func LatestVersion(dir string) (latest uint, count int, err error) {
	versions, err := upVersions(dir)
	if err != nil {
		return 0, 0, err
	}
	for _, v := range versions {
		latest = max(latest, v)
	}
	return latest, len(versions), nil
}

// upVersions returns the versions of the up migrations in dir, using the same
// file-name rule as golang-migrate. A missing dir has none.
func upVersions(dir string) ([]uint, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var versions []uint
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		m, perr := source.Parse(ent.Name())
		if perr != nil || m.Direction != source.Up {
			continue
		}
		versions = append(versions, m.Version)
	}
	return versions, nil
}

// logDBError logs a DB error, adding the TLS remediation hint when the
// failure was a certificate verification error.
func logDBError(logger *slog.Logger, cfg *config.Config, msg string, err error) {
	if hint := cfg.DBTLSErrorHint(err); hint != "" {
		logger.Error(msg, "error", err, "hint", hint)
		return
	}
	logger.Error(msg, "error", err)
}
