// Package migrate implements "elythia migrate", which applies or rolls back
// the SQL migrations under migration/ (the core track) and, for forks,
// migration/local/ (the local track, #3428).
package migrate

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"

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
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}
	if msg := validateFlags(*direction, *steps, *track); msg != "" {
		fmt.Fprintf(flagOut, "elythia migrate: %s\n", msg)
		fs.Usage()
		return 2
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
		logDBError(log, cfg, "migration failed", err)
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
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read %s: %w", dir, err)
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		m, perr := source.Parse(ent.Name())
		if perr != nil || m.Direction != source.Up {
			continue
		}
		count++
		if m.Version > latest {
			latest = m.Version
		}
	}
	return latest, count, nil
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
