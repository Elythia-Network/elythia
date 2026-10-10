package backup

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
)

// テストの PostgreSQL に TLS を有効にし、CA を 2 か所に置く:
//   - caPathInContainer: db.extra.sslrootcert で指す CA ファイル
//   - システムの CA の束 (/etc/ssl/cert.pem) の末尾: sslrootcert=system で使われる
const caPathInContainer = "/tmp/elythia-test-ca.crt"

var tlsOnce sync.Once

func enableTLS(t *testing.T, p *pgEnv) {
	t.Helper()
	tlsOnce.Do(func() {
		caPEM, certPEM, keyPEM := testCertificates(t)
		write := func(path, mode, owner string, body []byte) {
			cmd := exec.Command("docker", "exec", "-i", "-u", "root", p.container, "sh", "-c",
				fmt.Sprintf("cat > %s && chmod %s %s && chown %s %s", path, mode, path, owner, path))
			cmd.Stdin = bytes.NewReader(body)
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, string(out))
		}
		write("/var/lib/postgresql/server.crt", "0644", "postgres", certPEM)
		write("/var/lib/postgresql/server.key", "0600", "postgres", keyPEM)
		write(caPathInContainer, "0644", "root", caPEM)
		cmd := exec.Command("docker", "exec", "-i", "-u", "root", p.container, "sh", "-c", "cat >> /etc/ssl/cert.pem")
		cmd.Stdin = bytes.NewReader(caPEM)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		p.exec(t,
			`ALTER SYSTEM SET ssl_cert_file = '/var/lib/postgresql/server.crt'`,
			`ALTER SYSTEM SET ssl_key_file = '/var/lib/postgresql/server.key'`,
			`ALTER SYSTEM SET ssl = on`,
			`SELECT pg_reload_conf()`,
		)
		// reload は非同期なので、TLS で繋がるまで待つ。
		require.Eventually(t, func() bool {
			c := p.runner.Command(context.Background(), []string{"PGPASSWORD=" + pgPassword}, "psql",
				"postgresql://"+pgUser+"@localhost:5432/"+p.db+"?sslmode=require", "-c", "SELECT 1")
			return c.Run() == nil
		}, 10*time.Second, 100*time.Millisecond)
	})
}

// testCertificates returns a CA and a server certificate for "localhost"
// signed by it, in PEM.
func testCertificates(t *testing.T) (caPEM, certPEM, keyPEM []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	ca := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "elythia test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	srvKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	srv := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srv, ca, &srvKey.PublicKey, caKey)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(srvKey)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// TestDumpConn_ConfigURIsWorkWithPgDump builds the pg_dump connection from a
// config file the same way `elythia backup take` does, and runs the real
// pg_dump (in the PostgreSQL container) with it: TCP, UDS and the TLS
// settings of db.extra.
func TestDumpConn_ConfigURIsWorkWithPgDump(t *testing.T) {
	p := newDatabase(t)
	p.seedSimple(t)
	enableTLS(t, p)

	for _, tc := range []struct {
		name, host, extra string
		wantSSL           bool
	}{
		{name: "tcp", host: "localhost"},
		{name: "uds", host: "/var/run/postgresql"},
		// ssl: true は sslrootcert 無しの verify-full。libpq には sslrootcert=system を渡す。
		{name: "ssl true", host: "localhost", extra: "  extra:\n    ssl: true\n", wantSSL: true},
		{name: "sslrootcert", host: "localhost", extra: "  extra:\n    sslmode: verify-full\n    sslrootcert: " + caPathInContainer + "\n", wantSSL: true},
		{name: "verify-ca with CA", host: "localhost", extra: "  extra:\n    sslmode: verify-ca\n    sslrootcert: " + caPathInContainer + "\n", wantSSL: true},
		{name: "no-verify", host: "localhost", extra: "  extra:\n    ssl: no-verify\n", wantSSL: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "default.yml")
			body := fmt.Sprintf("url: http://127.0.0.1:3000/\nport: 3000\ndb:\n  host: %s\n  port: 5432\n  db: %s\n  user: %s\n  pass: %s\n%s",
				tc.host, p.db, pgUser, pgPassword, tc.extra)
			require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
			cfg, err := config.Load(path)
			require.NoError(t, err)
			dc, err := DumpConnFromURL(cfg.DatabaseURL("postgres"))
			require.NoError(t, err)
			assert.NotContains(t, dc.URI, ":"+pgPassword+"@")

			cmd := p.runner.Command(context.Background(), []string{"PGPASSWORD=" + dc.Password},
				"pg_dump", "--schema-only", "--no-password", "--dbname="+dc.URI)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			require.NoError(t, cmd.Run(), "uri %s: %s", dc.URI, stderr.String())
			assert.Contains(t, stdout.String(), "CREATE TABLE public.note")

			// 実際に TLS で繋がっているか。
			check := p.runner.Command(context.Background(), []string{"PGPASSWORD=" + dc.Password},
				"psql", "--no-password", "-At", "--dbname="+dc.URI, "-c", "SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()")
			out, err := check.CombinedOutput()
			require.NoError(t, err, string(out))
			assert.Equal(t, fmt.Sprint(tc.wantSSL)[:1], string(bytes.TrimSpace(out)))
		})
	}
}
