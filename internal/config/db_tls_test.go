package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTestCA writes a self-signed CA certificate and returns its path.
func writeTestCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "mk test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	return path
}

func TestResolveDBTLS(t *testing.T) {
	tests := []struct {
		name     string
		extra    map[string]any
		wantMode string
		wantRoot string
		wantErr  string
	}{
		{name: "nil extra is plaintext", extra: nil, wantMode: "disable"},
		{name: "unrelated keys are ignored", extra: map[string]any{"statement_timeout": 1000}, wantMode: "disable"},
		{name: "ssl null is plaintext", extra: map[string]any{"ssl": nil}, wantMode: "disable"},
		{name: "ssl bool true verifies", extra: map[string]any{"ssl": true}, wantMode: "verify-full"},
		{name: "ssl bool false is plaintext", extra: map[string]any{"ssl": false}, wantMode: "disable"},
		{name: "ssl string true verifies", extra: map[string]any{"ssl": "true"}, wantMode: "verify-full"},
		{name: "ssl string 1 verifies", extra: map[string]any{"ssl": "1"}, wantMode: "verify-full"},
		{name: "ssl string false", extra: map[string]any{"ssl": "false"}, wantMode: "disable"},
		{name: "ssl bad string", extra: map[string]any{"ssl": "yes please"}, wantErr: "not a boolean"},
		// node-postgres が文書化している「検証しない TLS」の書き方。
		{name: "ssl no-verify skips verification", extra: map[string]any{"ssl": "no-verify"}, wantMode: "require"},
		{name: "ssl no-verify case and space", extra: map[string]any{"ssl": " No-Verify "}, wantMode: "require"},
		{name: "rootcert with no-verify", extra: map[string]any{"ssl": "no-verify", "sslrootcert": "/etc/ca.pem"}, wantErr: "contradicts"},
		// pgx は prefer / allow では CA を渡しても検証せず、平文へ落ちうる。
		{name: "rootcert with sslmode prefer", extra: map[string]any{"sslmode": "prefer", "sslrootcert": "/etc/ca.pem"}, wantErr: "no effect with db.extra.sslmode prefer"},
		{name: "rootcert with sslmode allow", extra: map[string]any{"sslmode": "allow", "sslrootcert": "/etc/ca.pem"}, wantErr: "no effect with db.extra.sslmode allow"},
		{name: "sslmode prefer alone", extra: map[string]any{"sslmode": "prefer"}, wantMode: "prefer"},
		{name: "rootcert with sslmode require", extra: map[string]any{"sslmode": "require", "sslrootcert": "/etc/ca.pem"}, wantMode: "require", wantRoot: "/etc/ca.pem"},
		{name: "ssl bad type", extra: map[string]any{"ssl": 3}, wantErr: "unsupported type"},
		{name: "upstream rejectUnauthorized false", extra: map[string]any{"ssl": map[string]any{"rejectunauthorized": false}}, wantMode: "require"},
		{name: "upstream rejectUnauthorized camel case", extra: map[string]any{"ssl": map[string]any{"rejectUnauthorized": "false"}}, wantMode: "require"},
		{name: "upstream rejectUnauthorized true", extra: map[string]any{"ssl": map[string]any{"rejectUnauthorized": true}}, wantMode: "verify-full"},
		{name: "empty tls options verify", extra: map[string]any{"ssl": map[string]any{}}, wantMode: "verify-full"},
		{name: "map any any form", extra: map[string]any{"ssl": map[any]any{"rejectUnauthorized": false}}, wantMode: "require"},
		{name: "rejectUnauthorized not bool", extra: map[string]any{"ssl": map[string]any{"rejectUnauthorized": 7}}, wantErr: "rejectUnauthorized"},
		{name: "rejectUnauthorized bad string", extra: map[string]any{"ssl": map[string]any{"rejectUnauthorized": "nope"}}, wantErr: "rejectUnauthorized"},
		{name: "inline ca rejected", extra: map[string]any{"ssl": map[string]any{"ca": "-----BEGIN CERTIFICATE-----"}}, wantErr: "sslrootcert"},
		{name: "unknown tls option rejected", extra: map[string]any{"ssl": map[string]any{"servername": "db"}}, wantErr: "servername is not supported"},
		{name: "explicit sslmode", extra: map[string]any{"sslmode": "verify-ca"}, wantMode: "verify-ca"},
		{name: "explicit sslmode case", extra: map[string]any{"SSLMode": " Require "}, wantMode: "require"},
		{name: "explicit sslmode invalid", extra: map[string]any{"sslmode": "strict"}, wantErr: "sslmode"},
		{name: "explicit sslmode not string", extra: map[string]any{"sslmode": true}, wantErr: "sslmode"},
		{name: "ssl and sslmode conflict", extra: map[string]any{"ssl": true, "sslmode": "require"}, wantErr: "both set"},
		{name: "rootcert with ssl true", extra: map[string]any{"ssl": true, "sslrootcert": "/etc/ca.pem"}, wantMode: "verify-full", wantRoot: "/etc/ca.pem"},
		{name: "rootcert with sslmode", extra: map[string]any{"sslmode": "verify-ca", "sslrootcert": "/etc/ca.pem"}, wantMode: "verify-ca", wantRoot: "/etc/ca.pem"},
		{name: "rootcert with sslmode disable", extra: map[string]any{"sslmode": "disable", "sslrootcert": "/etc/ca.pem"}, wantErr: "disable"},
		{name: "rootcert without tls", extra: map[string]any{"sslrootcert": "/etc/ca.pem"}, wantErr: "TLS is disabled"},
		{name: "rootcert with rejectUnauthorized false", extra: map[string]any{"ssl": map[string]any{"rejectUnauthorized": false}, "sslrootcert": "/etc/ca.pem"}, wantErr: "contradicts"},
		{name: "rootcert empty", extra: map[string]any{"ssl": true, "sslrootcert": " "}, wantErr: "sslrootcert"},
		{name: "rootcert not string", extra: map[string]any{"ssl": true, "sslrootcert": 1}, wantErr: "sslrootcert"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveDBTLS(tt.extra)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantMode, got.SSLMode)
			assert.Equal(t, tt.wantRoot, got.SSLRootCert)
		})
	}
}

// TestLoad_DBExtraSSL checks the YAML shapes operators actually write,
// including the upstream node-postgres object form.
func TestLoad_DBExtraSSL(t *testing.T) {
	tests := []struct {
		name     string
		extra    string
		wantMode string
		wantErr  string
	}{
		// 以前は bool の true が weak decode で "1" になり、"true" との比較に
		// 外れて平文で繋いでいた。
		{name: "bool true", extra: "    ssl: true\n", wantMode: "sslmode=verify-full"},
		{name: "quoted true", extra: "    ssl: 'true'\n", wantMode: "sslmode=verify-full"},
		{name: "bool false", extra: "    ssl: false\n", wantMode: "sslmode=disable"},
		{name: "upstream object form", extra: "    ssl:\n      rejectUnauthorized: false\n", wantMode: "sslmode=require"},
		{name: "node-postgres no-verify", extra: "    ssl: no-verify\n", wantMode: "sslmode=require"},
		{name: "prefer with rootcert fails load", extra: "    sslmode: prefer\n    sslrootcert: /etc/ssl/pg-ca.pem\n", wantErr: "no effect"},
		{name: "explicit sslmode", extra: "    sslmode: verify-ca\n    sslrootcert: /etc/ssl/pg-ca.pem\n", wantMode: "sslmode=verify-ca"},
		{name: "invalid value fails load", extra: "    ssl: maybe\n", wantErr: "db.extra.ssl"},
		{name: "inline ca fails load", extra: "    ssl:\n      ca: abc\n", wantErr: "sslrootcert"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yaml := "url: https://example.com\ndb:\n  host: db.internal\n  port: 5432\n  db: misskey\n  user: u\n  pass: p\n  extra:\n" + tt.extra
			cfg, err := Load(writeTestConfig(t, yaml))
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Contains(t, cfg.DSN(), tt.wantMode)
		})
	}
}

// db.extra.* は bindEnvKeys に登録していないので、MK_DB_EXTRA_SSL が効くのは
// 設定ファイルにそのキーがあるときだけ。docs/configuration.md の記述を固定する。
func TestLoad_DBExtraSSLFromEnv(t *testing.T) {
	const base = "url: https://example.com\ndb:\n  host: db.internal\n  port: 5432\n  db: misskey\n  user: u\n  pass: p\n"

	t.Setenv("MK_DB_EXTRA_SSL", "true")
	cfg, err := Load(writeTestConfig(t, base+"  extra:\n    ssl: false\n"))
	require.NoError(t, err)
	assert.Contains(t, cfg.DSN(), "sslmode=verify-full")

	cfg, err = Load(writeTestConfig(t, base))
	require.NoError(t, err)
	assert.Contains(t, cfg.DSN(), "sslmode=disable")
}

// TestDSN_ParsesWithPgx feeds the generated DSNs to pgx itself, so quoting
// and TLS settings are checked against the real parser instead of by string
// matching.
func TestDSN_ParsesWithPgx(t *testing.T) {
	caPath := writeTestCA(t)
	tests := []struct {
		name         string
		pass         string
		extra        map[string]any
		wantTLS      bool
		wantInsecure bool
		wantRootCAs  bool
	}{
		{name: "plaintext", pass: "secret", wantTLS: false},
		{name: "empty password", pass: "", wantTLS: false},
		{name: "password with symbols", pass: `p@ss w0rd'\=:/?#&%`, wantTLS: false},
		{name: "ssl true verifies hostname", pass: "secret", extra: map[string]any{"ssl": true}, wantTLS: true},
		{name: "rejectUnauthorized false skips verification", pass: "secret", extra: map[string]any{"ssl": map[string]any{"rejectUnauthorized": false}}, wantTLS: true, wantInsecure: true},
		{name: "custom CA", pass: "secret", extra: map[string]any{"ssl": true, "sslrootcert": caPath}, wantTLS: true, wantRootCAs: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{DB: DBOptions{Host: "db.internal", Port: 5433, DB: "mis key", User: "u ser", Pass: tt.pass, Extra: tt.extra}}
			for label, dsn := range map[string]string{"keyword": cfg.DSN(), "url": cfg.DatabaseURL("postgres")} {
				pc, err := pgconn.ParseConfig(dsn)
				require.NoError(t, err, label)
				assert.Equal(t, "db.internal", pc.Host, label)
				assert.Equal(t, uint16(5433), pc.Port, label)
				assert.Equal(t, "mis key", pc.Database, label)
				assert.Equal(t, "u ser", pc.User, label)
				assert.Equal(t, tt.pass, pc.Password, label)
				if !tt.wantTLS {
					assert.Nil(t, pc.TLSConfig, label)
					continue
				}
				require.NotNil(t, pc.TLSConfig, label)
				assert.Equal(t, tt.wantInsecure, pc.TLSConfig.InsecureSkipVerify, label)
				if !tt.wantInsecure {
					assert.Equal(t, "db.internal", pc.TLSConfig.ServerName, label)
				}
				assert.Equal(t, tt.wantRootCAs, pc.TLSConfig.RootCAs != nil, label)
				// verify-full / require は平文へ落ちる fallback を持たない。
				assert.Empty(t, pc.Fallbacks, label)
			}
		})
	}
}

func TestDSN_PluginstoreSuffixStillParses(t *testing.T) {
	// pluginstore は DSN の末尾に " search_path=<schema>" を足す。
	cfg := &Config{DB: DBOptions{Host: "db", Port: 5432, DB: "misskey", User: "u", Pass: ""}}
	pc, err := pgconn.ParseConfig(cfg.DSN() + " search_path=plugin_x")
	require.NoError(t, err)
	assert.Equal(t, "", pc.Password)
	assert.Equal(t, "plugin_x", pc.RuntimeParams["search_path"])
}

func TestSlaveDSN_InheritsPrimaryUpstreamTLSOptions(t *testing.T) {
	cfg := &Config{
		DB:       DBOptions{Host: "primary", Port: 5432, Extra: map[string]any{"ssl": map[string]any{"rejectUnauthorized": false}}},
		DBSlaves: []DBSlaveOptions{{Host: "replica1", Port: 5432, DB: "misskey", User: "u", Pass: "p w"}},
	}
	pc, err := pgconn.ParseConfig(cfg.SlaveDSN(0))
	require.NoError(t, err)
	assert.Equal(t, "replica1", pc.Host)
	assert.Equal(t, "p w", pc.Password)
	require.NotNil(t, pc.TLSConfig)
	assert.True(t, pc.TLSConfig.InsecureSkipVerify)
}

func TestDSN_InvalidExtraFailsClosed(t *testing.T) {
	// Load を通らずに組み立てた Config でも、平文 / 無検証に倒さない。
	cfg := &Config{DB: DBOptions{Host: "db", Port: 5432, Extra: map[string]any{"ssl": "garbage"}}}
	assert.Contains(t, cfg.DSN(), "sslmode=verify-full")
}

func TestDatabaseURL(t *testing.T) {
	t.Run("tcp escapes credentials and carries TLS", func(t *testing.T) {
		cfg := &Config{DB: DBOptions{
			Host: "db.internal", Port: 5432, DB: "misskey", User: "mk", Pass: "a@b:c/d?e#f%g h",
			Extra: map[string]any{"ssl": true, "sslrootcert": "/etc/ssl/ca.pem"},
		}}
		raw := cfg.DatabaseURL("pgx5")
		u, err := url.Parse(raw)
		require.NoError(t, err)
		assert.Equal(t, "pgx5", u.Scheme)
		assert.Equal(t, "db.internal:5432", u.Host)
		pass, _ := u.User.Password()
		assert.Equal(t, "a@b:c/d?e#f%g h", pass)
		assert.Equal(t, "verify-full", u.Query().Get("sslmode"))
		assert.Equal(t, "/etc/ssl/ca.pem", u.Query().Get("sslrootcert"))
	})
	t.Run("ipv6 host is bracketed", func(t *testing.T) {
		cfg := &Config{DB: DBOptions{Host: "::1", Port: 5432, DB: "misskey", User: "mk", Pass: "p"}}
		u, err := url.Parse(cfg.DatabaseURL("pgx5"))
		require.NoError(t, err)
		assert.Equal(t, "[::1]:5432", u.Host)
		assert.Equal(t, "disable", u.Query().Get("sslmode"))
	})
	t.Run("unix socket uses host query and disables TLS", func(t *testing.T) {
		cfg := &Config{DB: DBOptions{
			Host: "/var/run/postgresql", Port: 5433, DB: "misskey", User: "mk", Pass: "p&q=r",
			Extra: map[string]any{"ssl": true},
		}}
		raw := cfg.DatabaseURL("pgx5")
		u, err := url.Parse(raw)
		require.NoError(t, err)
		assert.Empty(t, u.Host)
		assert.Equal(t, "/var/run/postgresql", u.Query().Get("host"))
		assert.Equal(t, "5433", u.Query().Get("port"))
		assert.Equal(t, "disable", u.Query().Get("sslmode"))
		pc, err := pgconn.ParseConfig("postgres" + raw[len("pgx5"):])
		require.NoError(t, err)
		assert.Equal(t, "/var/run/postgresql", pc.Host)
		assert.Equal(t, "p&q=r", pc.Password)
		assert.Nil(t, pc.TLSConfig)
	})
}

func TestDBTLSErrorHint(t *testing.T) {
	sslTrue := &Config{DB: DBOptions{Extra: map[string]any{"ssl": true}}}
	assert.Empty(t, sslTrue.DBTLSErrorHint(nil))
	assert.Empty(t, sslTrue.DBTLSErrorHint(errors.New("connection refused")))

	wrapped := []error{
		fmt.Errorf("connect: %w", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}),
		fmt.Errorf("connect: %w", x509.UnknownAuthorityError{}),
		fmt.Errorf("connect: %w", x509.HostnameError{Host: "db"}),
		fmt.Errorf("connect: %w", x509.CertificateInvalidError{Reason: x509.Expired}),
	}
	for _, err := range wrapped {
		hint := sslTrue.DBTLSErrorHint(err)
		assert.Contains(t, hint, "db.extra.ssl: true now verifies")
		assert.Contains(t, hint, "db.extra.sslrootcert")
		assert.Contains(t, hint, "rejectUnauthorized: false")
	}
}

// sslmode を明示した構成や CA ファイルを指定した構成に「ssl: true が検証する
// ようになった」と出すと、書いていないキーへ誘導してしまう。
func TestDBTLSErrorHint_FollowsConfiguredKeys(t *testing.T) {
	verr := fmt.Errorf("connect: %w", x509.UnknownAuthorityError{})

	explicit := &Config{DB: DBOptions{Extra: map[string]any{"sslmode": "verify-full"}}}
	hint := explicit.DBTLSErrorHint(verr)
	assert.NotContains(t, hint, "ssl: true")
	assert.NotContains(t, hint, "rejectUnauthorized")
	assert.Contains(t, hint, "db.extra.sslmode: verify-full")
	assert.Contains(t, hint, "db.extra.sslmode: require")
	assert.Contains(t, hint, "db.extra.sslrootcert")

	withCA := &Config{DB: DBOptions{Extra: map[string]any{"ssl": true, "sslrootcert": "/etc/ssl/pg-ca.pem"}}}
	hint = withCA.DBTLSErrorHint(verr)
	assert.NotContains(t, hint, "ssl: true now verifies")
	assert.Contains(t, hint, "/etc/ssl/pg-ca.pem")
	assert.Contains(t, hint, "db.host")

	explicitCA := &Config{DB: DBOptions{Extra: map[string]any{"sslmode": "verify-ca", "sslrootcert": "/etc/ssl/pg-ca.pem"}}}
	assert.Contains(t, explicitCA.DBTLSErrorHint(verr), "/etc/ssl/pg-ca.pem")
}
