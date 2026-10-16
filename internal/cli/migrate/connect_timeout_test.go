package migrate

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gomigrate "github.com/golang-migrate/migrate/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRun_ConnectTimeoutCarriesTheHint connects the real golang-migrate
// driver to a server that accepts and never answers. migrate is what the
// standard deployments run first, so its error must say which server did not
// answer and which setting bounds the wait (#3632).
func TestRun_ConnectTimeoutCarriesTheHint(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	port := l.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "default.yml")
	require.NoError(t, os.WriteFile(cfgPath, fmt.Appendf(nil, `url: https://example.tld/
db:
  host: 127.0.0.1
  port: %d
  db: misskey
  user: mk
  pass: secret
  extra:
    connectionTimeoutMillis: 1000
`, port), 0o600))
	src := filepath.Join(dir, "migration")
	require.NoError(t, os.MkdirAll(src, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "000001_x.up.sql"), []byte("SELECT 1;"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(src, "000001_x.down.sql"), []byte("SELECT 1;"), 0o600))

	e, out, _ := testEnv(t, nil, nil)
	e.open = func(src, db string) (migrator, error) { return gomigrate.New(src, db) }
	e.coreDir = src

	done := make(chan int, 1)
	start := time.Now()
	go func() { done <- run(e, &bytes.Buffer{}, []string{"-config", cfgPath, "-track", "core"}) }()
	var code int
	select {
	case code = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("migrate did not return within 15s against a server that never answers")
	}
	assert.Equal(t, 1, code)
	assert.Less(t, time.Since(start), 5*time.Second)
	log := out.String()
	assert.Contains(t, log, "failed to create migrator")
	assert.Contains(t, log, fmt.Sprintf("PostgreSQL at 127.0.0.1:%d did not answer within 1s", port))
	assert.Contains(t, log, "db.extra.connectionTimeoutMillis")
	assert.False(t, strings.Contains(log, "secret"), "the password must not be logged")
}
