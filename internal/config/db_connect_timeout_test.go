package config

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveDBConnectTimeout(t *testing.T) {
	tests := []struct {
		name    string
		extra   map[string]any
		want    time.Duration
		wantErr string
	}{
		{name: "nil extra uses the default", extra: nil, want: DefaultDBConnectTimeout},
		{name: "other keys only use the default", extra: map[string]any{"ssl": true, "statement_timeout": 1000}, want: DefaultDBConnectTimeout},
		{name: "null value uses the default", extra: map[string]any{"connectiontimeoutmillis": nil}, want: DefaultDBConnectTimeout},
		{name: "empty string uses the default", extra: map[string]any{"connectiontimeoutmillis": " "}, want: DefaultDBConnectTimeout},
		{name: "whole seconds", extra: map[string]any{"connectiontimeoutmillis": 5000}, want: 5 * time.Second},
		{name: "camelCase key as written upstream", extra: map[string]any{"connectionTimeoutMillis": 3000}, want: 3 * time.Second},
		{name: "rounds up below one second", extra: map[string]any{"connectiontimeoutmillis": 1}, want: time.Second},
		{name: "rounds up a fraction of a second", extra: map[string]any{"connectiontimeoutmillis": 1500}, want: 2 * time.Second},
		{name: "zero disables the timeout", extra: map[string]any{"connectiontimeoutmillis": 0}, want: 0},
		{name: "int64", extra: map[string]any{"connectiontimeoutmillis": int64(4000)}, want: 4 * time.Second},
		{name: "uint64", extra: map[string]any{"connectiontimeoutmillis": uint64(4000)}, want: 4 * time.Second},
		{name: "float", extra: map[string]any{"connectiontimeoutmillis": 2500.0}, want: 3 * time.Second},
		{name: "numeric string from an environment variable", extra: map[string]any{"connectiontimeoutmillis": "7000"}, want: 7 * time.Second},
		{name: "maximum", extra: map[string]any{"connectiontimeoutmillis": math.MaxInt32}, want: time.Duration(math.Ceil(float64(math.MaxInt32)/1000)) * time.Second},
		{name: "negative", extra: map[string]any{"connectiontimeoutmillis": -1}, wantErr: "out of range"},
		{name: "too large", extra: map[string]any{"connectiontimeoutmillis": int64(math.MaxInt32) + 1}, wantErr: "out of range"},
		{name: "NaN", extra: map[string]any{"connectiontimeoutmillis": "NaN"}, wantErr: "out of range"},
		{name: "not a number", extra: map[string]any{"connectiontimeoutmillis": "10s"}, wantErr: "not a number of milliseconds"},
		{name: "unsupported type", extra: map[string]any{"connectiontimeoutmillis": true}, wantErr: "unsupported type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveDBConnectTimeout(tt.extra)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Contains(t, err.Error(), "db.extra.connectionTimeoutMillis")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestDSN_ConnectTimeoutParsesWithPgx checks the connect timeout by feeding
// every connection string Elythia builds to pgx itself.
func TestDSN_ConnectTimeoutParsesWithPgx(t *testing.T) {
	tests := []struct {
		name  string
		host  string
		extra map[string]any
		want  time.Duration
	}{
		{name: "default over tcp", host: "db.internal", want: DefaultDBConnectTimeout},
		{name: "default over a unix socket", host: "/var/run/postgresql", want: DefaultDBConnectTimeout},
		{name: "configured", host: "db.internal", extra: map[string]any{"connectionTimeoutMillis": 2500}, want: 3 * time.Second},
		{name: "configured with TLS", host: "db.internal", extra: map[string]any{"ssl": true, "connectionTimeoutMillis": 4000}, want: 4 * time.Second},
		{name: "zero means no timeout", host: "db.internal", extra: map[string]any{"connectionTimeoutMillis": 0}, want: 0},
		// Load を通らない不正な値は既定に倒す (0 に倒すと待ち続ける)。
		{name: "invalid hand-built value falls back to the default", host: "db.internal", extra: map[string]any{"connectionTimeoutMillis": -5}, want: DefaultDBConnectTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{
				DB:       DBOptions{Host: tt.host, Port: 5432, DB: "misskey", User: "u", Pass: "p", Extra: tt.extra},
				DBSlaves: []DBSlaveOptions{{Host: tt.host, Port: 5433, DB: "misskey", User: "u", Pass: "p"}},
			}
			assert.Equal(t, tt.want, cfg.DBConnectTimeout())
			for label, dsn := range map[string]string{
				"keyword":  cfg.DSN(),
				"slave":    cfg.SlaveDSN(0),
				"url":      cfg.DatabaseURL("postgres"),
				"pluginst": cfg.DSN() + " search_path=plugin_x",
			} {
				pc, err := pgconn.ParseConfig(dsn)
				require.NoError(t, err, label)
				assert.Equal(t, tt.want, pc.ConnectTimeout, label)
			}
		})
	}
}

// TestDSN_ConnectTimeoutOverridesPGCONNECT_TIMEOUT pins that the configured
// value, not the libpq environment variable pgx falls back to, decides the
// timeout. 0 も接続文字列に書かないと、待ち続けるつもりの運営者が
// PGCONNECT_TIMEOUT の値で区切られる。
func TestDSN_ConnectTimeoutOverridesPGCONNECT_TIMEOUT(t *testing.T) {
	tests := []struct {
		name  string
		env   string
		extra map[string]any
		want  time.Duration
	}{
		{name: "default over PGCONNECT_TIMEOUT=0", env: "0", want: DefaultDBConnectTimeout},
		{name: "configured 0 over PGCONNECT_TIMEOUT=7", env: "7", extra: map[string]any{"connectionTimeoutMillis": 0}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PGCONNECT_TIMEOUT", tt.env)
			cfg := &Config{
				DB:       DBOptions{Host: "db", Port: 5432, DB: "misskey", User: "u", Pass: "p", Extra: tt.extra},
				DBSlaves: []DBSlaveOptions{{Host: "replica", Port: 5432, DB: "misskey", User: "u", Pass: "p"}},
			}
			for label, dsn := range map[string]string{
				"keyword": cfg.DSN(),
				"slave":   cfg.SlaveDSN(0),
				// pgconn は pgx5:// を URL と見なさないので、golang-migrate の driver が
				// 書き戻すのと同じ postgres:// で読む。
				"url": cfg.DatabaseURL("postgres"),
			} {
				pc, err := pgconn.ParseConfig(dsn)
				require.NoError(t, err, label)
				assert.Equal(t, tt.want, pc.ConnectTimeout, label)
			}
		})
	}
}

func TestDBConnectTimeoutHint(t *testing.T) {
	cfg := &Config{DB: DBOptions{Host: "db.internal", Port: 5433, Extra: map[string]any{"connectionTimeoutMillis": 2500}}}
	timeout := fmt.Errorf("connect: %w", context.DeadlineExceeded)

	assert.Empty(t, cfg.DBConnectTimeoutHint(errors.New("connection refused")))
	assert.Empty(t, cfg.DBConnectTimeoutHint(nil))
	assert.Empty(t, cfg.DBReplicaConnectTimeoutHint(errors.New("connection refused")))

	hint := cfg.DBConnectTimeoutHint(timeout)
	assert.Contains(t, hint, "PostgreSQL at db.internal:5433 did not answer within 3s")
	assert.Contains(t, hint, "db.extra.connectionTimeoutMillis")
	assert.Contains(t, cfg.DBReplicaConnectTimeoutHint(timeout), "a PostgreSQL read replica (dbSlaves) did not answer within 3s")

	uds := &Config{DB: DBOptions{Host: "/var/run/postgresql", Port: 5432}}
	assert.Contains(t, uds.DBConnectTimeoutHint(timeout), "PostgreSQL at /var/run/postgresql/.s.PGSQL.5432 did not answer within 10s")
}

func TestDatabaseURL_ConnectTimeoutForLibpq(t *testing.T) {
	// pg_dump / pg_restore (libpq) にも同じ値が渡る。
	cfg := &Config{DB: DBOptions{Host: "db", Port: 5432, DB: "misskey", User: "u", Pass: "p", Extra: map[string]any{"connectionTimeoutMillis": 6000}}}
	u, err := url.Parse(cfg.DatabaseURL("postgresql"))
	require.NoError(t, err)
	assert.Equal(t, "6", u.Query().Get("connect_timeout"))
}

func TestLoad_DBConnectTimeout(t *testing.T) {
	base := `
url: https://misskey.example.com
port: 3000
db:
  host: localhost
  port: 5432
  db: misskey
  user: postgres
  pass: secret
`
	t.Run("absent uses the default", func(t *testing.T) {
		cfg, err := Load(writeTestConfig(t, base))
		require.NoError(t, err)
		assert.Equal(t, DefaultDBConnectTimeout, cfg.DBConnectTimeout())
	})
	t.Run("upstream key in db.extra", func(t *testing.T) {
		cfg, err := Load(writeTestConfig(t, base+"  extra:\n    statement_timeout: 10000\n    connectionTimeoutMillis: 2500\n"))
		require.NoError(t, err)
		assert.Equal(t, 3*time.Second, cfg.DBConnectTimeout())
	})
	t.Run("invalid value stops Load", func(t *testing.T) {
		_, err := Load(writeTestConfig(t, base+"  extra:\n    connectionTimeoutMillis: -1\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "db.extra.connectionTimeoutMillis")
	})
	t.Run("environment variable without the key in the file", func(t *testing.T) {
		// bindEnvKeys に登録しているので、ファイルに extra: が無くても効く。
		t.Setenv("MK_DB_EXTRA_CONNECTIONTIMEOUTMILLIS", "4000")
		cfg, err := Load(writeTestConfig(t, base))
		require.NoError(t, err)
		assert.Equal(t, 4*time.Second, cfg.DBConnectTimeout())
	})
	t.Run("environment variable overrides the file and keeps the other extra keys", func(t *testing.T) {
		t.Setenv("MK_DB_EXTRA_CONNECTIONTIMEOUTMILLIS", "0")
		cfg, err := Load(writeTestConfig(t, base+"  extra:\n    sslmode: require\n    connectionTimeoutMillis: 2500\n"))
		require.NoError(t, err)
		assert.Equal(t, time.Duration(0), cfg.DBConnectTimeout())
		assert.Contains(t, cfg.DSN(), "sslmode=require")
	})
	t.Run("invalid environment variable stops Load", func(t *testing.T) {
		t.Setenv("MK_DB_EXTRA_CONNECTIONTIMEOUTMILLIS", "soon")
		_, err := Load(writeTestConfig(t, base))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "db.extra.connectionTimeoutMillis")
	})
}
