package config

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultDBConnectTimeout is the connect timeout used when
// db.extra.connectionTimeoutMillis is not set (#3632).
//
// 本家は TypeORM の connectTimeoutMS も node-postgres の connectionTimeoutMillis も
// 指定しておらず、どちらの既定も 0 (= 待ち続ける。pg 8.23.0 の lib/client.js、
// typeorm 1.1.0 の PostgresDriver.createPool)。受け付けたまま応答しない相手に繋ぐと
// 起動も DB を使う処理も戻らないので、Elythia は既定で区切る。
// 10 秒にするのは、健全な PostgreSQL への接続 (TCP、TLS、SCRAM 認証、backend の
// fork) が負荷の高いときでも 1 秒を大きく下回り、桁の違う余裕を取れるため。
// 一方で、止まった DB に気付くまでの時間を起動の healthcheck と同じ桁に収める。
const DefaultDBConnectTimeout = 10 * time.Second

// connectTimeoutKey is the db.extra key read by ResolveDBConnectTimeout, in
// the lower case Viper gives map keys.
const connectTimeoutKey = "connectiontimeoutmillis"

// maxConnectTimeoutMillis bounds db.extra.connectionTimeoutMillis.
//
// node-postgres は setTimeout に渡すので、Node の上限 (2^31-1 ms、約 24.8 日) を
// 超える値は 1 ms として扱われる。黙って意味の違う値で動かさず、起動エラーにする。
const maxConnectTimeoutMillis = math.MaxInt32

// ResolveDBConnectTimeout interprets db.extra.connectionTimeoutMillis, the
// node-postgres pool option upstream forwards from db.extra, and returns the
// connect timeout to put into connection strings.
//
// The value is in milliseconds (number, or a numeric string as environment
// variables give). It is rounded up to whole seconds because libpq's and
// pgx's connect_timeout is in seconds. 0 means no timeout, as in
// node-postgres. A missing or empty value means DefaultDBConnectTimeout.
// Negative, non-numeric and out-of-range values are errors.
func ResolveDBConnectTimeout(extra map[string]any) (time.Duration, error) {
	var raw any
	var present bool
	for k, v := range extra {
		if strings.ToLower(k) == connectTimeoutKey {
			raw, present = v, true
		}
	}
	if !present || raw == nil {
		return DefaultDBConnectTimeout, nil
	}

	var ms float64
	switch t := raw.(type) {
	case int:
		ms = float64(t)
	case int64:
		ms = float64(t)
	case uint64:
		ms = float64(t)
	case float64:
		ms = t
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return DefaultDBConnectTimeout, nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, fmt.Errorf("config: db.extra.connectionTimeoutMillis %q is not a number of milliseconds", t)
		}
		ms = f
	default:
		return 0, fmt.Errorf("config: db.extra.connectionTimeoutMillis has unsupported type %T; use a number of milliseconds", raw)
	}
	if math.IsNaN(ms) || ms < 0 || ms > maxConnectTimeoutMillis {
		return 0, fmt.Errorf("config: db.extra.connectionTimeoutMillis %v is out of range; use 0 (no timeout) to %d milliseconds", raw, maxConnectTimeoutMillis)
	}
	// 秒へ切り上げる。切り捨てると 1〜999 ms が 0 (= 待ち続ける) になり、
	// 区切りたくて書いた値が逆の意味になる。
	return time.Duration(math.Ceil(ms/1000)) * time.Second, nil
}

// dbConnectTimeoutSeconds returns the connect_timeout value, in whole seconds,
// derived from c.DB.Extra.
//
// Load は ResolveDBConnectTimeout で検証してから Config を返すので、ここで
// エラーになるのは Config を手で組み立てた場合だけ。そのときは既定に倒す
// (0 に倒すと、誤った設定が「待ち続ける」接続として表に出ない)。
func (c *Config) dbConnectTimeoutSeconds() int {
	d, err := ResolveDBConnectTimeout(c.DB.Extra)
	if err != nil {
		d = DefaultDBConnectTimeout
	}
	return int(d / time.Second)
}

// DBConnectTimeout returns the connect timeout applied to each PostgreSQL
// connection attempt. 0 means no timeout.
func (c *Config) DBConnectTimeout() time.Duration {
	return time.Duration(c.dbConnectTimeoutSeconds()) * time.Second
}

// DBConnectTimeoutHint returns an operator-facing hint when err is the
// connect timeout (db.extra.connectionTimeoutMillis) expiring while
// connecting to the primary (db.host / db.port), or "" otherwise.
//
// pgx の時間切れのエラーは "timeout: context deadline exceeded" で、どの設定が
// 効いたのか、相手が応答しなかったのか拒んだのかが読み取れない (#3632)。
// serve (internal/db) と migrate の両方が使う。標準の起動手順では migrate が
// 先に DB へ繋ぐので、運営者が最初に見るのはそちらのエラーになる。
func (c *Config) DBConnectTimeoutHint(err error) string {
	if !isConnectTimeout(err) {
		return ""
	}
	where := net.JoinHostPort(c.DB.Host, strconv.Itoa(c.DB.Port))
	if IsUnixSocketPath(c.DB.Host) {
		// UDS の host はソケットのディレクトリで、実際に繋ぐのはその下の
		// .s.PGSQL.<port>。"<dir>:<port>" と書くと実在しないパスに見える。
		where = filepath.Join(c.DB.Host, ".s.PGSQL."+strconv.Itoa(c.DB.Port))
	}
	return c.connectTimeoutHint("PostgreSQL at " + where)
}

// DBReplicaConnectTimeoutHint is DBConnectTimeoutHint for a read replica
// (dbSlaves). Which replica it was is in err itself.
func (c *Config) DBReplicaConnectTimeoutHint(err error) string {
	if !isConnectTimeout(err) {
		return ""
	}
	return c.connectTimeoutHint("a PostgreSQL read replica (dbSlaves)")
}

func (c *Config) connectTimeoutHint(who string) string {
	return fmt.Sprintf("%s did not answer within %s; check that the server is up and reachable, "+
		"or change db.extra.connectionTimeoutMillis (see docs/configuration.md)", who, c.DBConnectTimeout())
}

// isConnectTimeout reports whether err is the connect deadline expiring.
//
// pgx は connect_timeout を context の期限として扱い、dial の時間切れも
// 応答待ちの時間切れも context.DeadlineExceeded を包んで返す。
func isConnectTimeout(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}
