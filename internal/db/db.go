// Package db wires the GORM PostgreSQL connection used by the rest of the
// application. Lives in its own package so its dbresolver wiring tests do not
// drag down internal/model coverage (model is mostly pure struct definitions).
package db

import (
	"fmt"
	stdlog "log"
	"log/slog"
	"os"
	"time"

	"github.com/shiroha-a/mk/internal/config"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/plugin/dbresolver"
)

// New opens a GORM database connection. If cfg.DBReplications is true and
// cfg.DBSlaves is non-empty, register read replicas via the dbresolver
// plugin so SELECT queries are routed to replicas.
func New(cfg *config.Config) (*gorm.DB, error) {
	gdb, err := gorm.Open(postgres.Open(cfg.DSN()), &gorm.Config{
		Logger: newGormLogger(cfg),
		// TypeORM互換: テーブル名をそのまま使う
		DisableNestedTransaction: true,
		// クエリのprepared statementをコネクション単位でキャッシュし、
		// パース/プラン作成のオーバーヘッドを削減する
		PrepareStmt: true,
	})
	if err != nil {
		if hint := cfg.DBTLSErrorHint(err); hint != "" {
			return nil, fmt.Errorf("failed to connect to database (%s): %w", hint, err)
		}
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	// コネクションプール設定。未指定時はWebアプリケーション向けの適正値を採用する。
	// Goのデフォルト(MaxOpenConns=0=無制限)だと高負荷時にPGバックエンド生成コストが
	// テールレイテンシに直結するため、明示的に上限を設ける。
	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("failed to get underlying sql.DB: %w", err)
	}

	maxOpen := 25
	if cfg.DB.MaxOpenConns != nil {
		maxOpen = *cfg.DB.MaxOpenConns
	}
	maxIdle := 25
	if cfg.DB.MaxIdleConns != nil {
		maxIdle = *cfg.DB.MaxIdleConns
	}
	connMaxLifetime := 5 * time.Minute
	if cfg.DB.ConnMaxLifetime != nil {
		connMaxLifetime = time.Duration(*cfg.DB.ConnMaxLifetime) * time.Second
	}
	connMaxIdleTime := 5 * time.Minute
	if cfg.DB.ConnMaxIdleTime != nil {
		connMaxIdleTime = time.Duration(*cfg.DB.ConnMaxIdleTime) * time.Second
	}

	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetConnMaxLifetime(connMaxLifetime)
	sqlDB.SetConnMaxIdleTime(connMaxIdleTime)

	slog.Info("connected to PostgreSQL",
		"host", cfg.DB.Host,
		"port", cfg.DB.Port,
		"db", cfg.DB.DB,
		"maxOpenConns", maxOpen,
		"maxIdleConns", maxIdle,
		"connMaxLifetime", connMaxLifetime,
		"connMaxIdleTime", connMaxIdleTime,
	)

	// TS互換: dbReplications フラグを明示的に有効化しない限り dbSlaves を無視する
	if cfg.DBReplications && len(cfg.DBSlaves) > 0 {
		replicas := make([]gorm.Dialector, 0, len(cfg.DBSlaves))
		for i := range cfg.DBSlaves {
			replicas = append(replicas, postgres.Open(cfg.SlaveDSN(i)))
		}
		// dbresolver は SELECT を Replicas (RandomPolicy) へ、書き込みと
		// dbresolver.Write 明示クエリを Sources (primary) へ振り分ける。
		// Use() は内部で Initialize エラーを返しうるが現実装では起きないため
		// fail-fast でラップして上位に返す。
		if err := gdb.Use(dbresolver.Register(dbresolver.Config{
			Replicas: replicas,
			Policy:   dbresolver.RandomPolicy{},
		}).
			SetMaxOpenConns(maxOpen).
			SetMaxIdleConns(maxIdle).
			SetConnMaxLifetime(connMaxLifetime).
			SetConnMaxIdleTime(connMaxIdleTime),
		); err != nil {
			return nil, fmt.Errorf("failed to register db replicas: %w", err)
		}
		slog.Info("registered PostgreSQL read replicas", "count", len(cfg.DBSlaves))
	}

	return gdb, nil
}

// newGormLogger builds the GORM logger used for SQL logging.
//
// **既定でバインド値を SQL 文字列へ展開しない。**
//
// `logger.Default` は `IgnoreRecordNotFoundError: false` なので、`Warn` でも
// **`record not found` を含む全エラーで**展開済みの SQL を stdout に出す
// (`logger.go` の `case err != nil && l.LogLevel >= Error && (!errors.Is(err,
// ErrRecordNotFound) || !l.IgnoreRecordNotFoundError)`)。
//
// これが認証経路を直撃する。`auth.Authenticate` はまず native token として
// 引くので、アプリ / OAuth / MiAuth のトークンでは**必ず** not-found を経て
// から `access_token` 側で成功する。つまり通常利用でトークンが平文でログに
// 出る。同じ形でパスワードリセットトークン・メール確認コード・アプリ
// secret・招待コードが出うるし、書き込みエラーなら `twoFactorSecret` や
// `smtpPass` も出る。slow query (200ms 超) の経路は**成功したクエリ**でも
// 同じ展開をするので、ネイティブトークンもそこから出る。
//
// upstream は `postgres.ts` が `logging: process.env.NODE_ENV !== 'production'`
// で本番では SQL ログ自体を持たず、dev でも TypeORM がプレースホルダ付き SQL と
// パラメータを別々に出すので値が SQL に埋まらない。
//
// `logging.sql.enableQueryParamLogging` を真にしたときだけ従来どおり展開する
// (運用者が明示的に選んだ場合)。既定は偽。
func newGormLogger(cfg *config.Config) logger.Interface {
	return newGormLoggerTo(cfg, stdlog.New(os.Stdout, "\r\n", stdlog.LstdFlags))
}

// newGormLoggerTo is newGormLogger with an explicit sink, so tests can observe
// what actually reaches the log.
func newGormLoggerTo(cfg *config.Config, w logger.Writer) logger.Interface {
	paramLog := cfg.Logging != nil && cfg.Logging.SQL != nil && cfg.Logging.SQL.EnableQueryParamLog
	// **`disableQueryTruncation` を実際に効かせる。** 宣言と env バインドは
	// あったのにどこからも読まれておらず (dead config)、設定しても何も起きな
	// かった。upstream は `postgres.ts` の `truncateSql` で既定 100 文字に
	// 切り詰め、このフラグで無効化できる。
	truncate := true
	if cfg.Logging != nil && cfg.Logging.SQL != nil && cfg.Logging.SQL.DisableQueryTruncation {
		truncate = false
	}
	logLevel := logger.Warn
	if paramLog {
		logLevel = logger.Info
	}
	if truncate {
		w = truncatingWriter{inner: w}
	}
	return logger.New(w, logger.Config{
		SlowThreshold: 200 * time.Millisecond,
		LogLevel:      logLevel,
		// **not-found を SQL ごと出さない。** 認証経路が必ず通る枝で、
		// そこに渡る値は生のトークンそのもの。
		IgnoreRecordNotFoundError: true,
		// **バインド値を埋めない。** GORM はこれが偽だと
		// `Dialector.Explain(sql, vars...)` で値を SQL へ展開する。
		ParameterizedQueries: !paramLog,
		Colorful:             false,
	})
}

// sqlLogMaxChars mirrors upstream postgres.ts truncateSql (既定 100 文字)。
const sqlLogMaxChars = 100

// truncatingWriter clips long SQL statements before they reach the log.
//
// upstream は `truncateSql` で 100 文字に切り詰め、`disableQueryTruncation` で
// 無効化できる。mk-go はそのフラグを宣言だけしていて効いていなかった。
type truncatingWriter struct{ inner logger.Writer }

func (t truncatingWriter) Printf(format string, args ...any) {
	for i, a := range args {
		s, ok := a.(string)
		if !ok || len([]rune(s)) <= sqlLogMaxChars {
			continue
		}
		args[i] = string([]rune(s)[:sqlLogMaxChars]) + "..."
	}
	t.inner.Printf(format, args...)
}
