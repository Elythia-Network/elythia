package migrate

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gomigrate "github.com/golang-migrate/migrate/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMigrator records which operation the command asked for.
type fakeMigrator struct {
	calls  []string
	steps  int
	err    error
	closed bool
}

func (f *fakeMigrator) Up() error   { f.calls = append(f.calls, "up"); return f.err }
func (f *fakeMigrator) Down() error { f.calls = append(f.calls, "down"); return f.err }
func (f *fakeMigrator) Steps(n int) error {
	f.calls = append(f.calls, "steps")
	f.steps = n
	return f.err
}
func (f *fakeMigrator) Close() (error, error) { f.closed = true; return nil, nil }

func writeConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "default.yml")
	require.NoError(t, os.WriteFile(path, []byte(`url: https://example.tld/
db:
  host: db.example
  port: 5432
  db: misskey
  user: mk
  pass: secret
`), 0o600))
	return path
}

// testEnv returns an env whose migrator is m and whose logger never replaces
// the process default.
func testEnv(m *fakeMigrator, openErr error) (env, *bytes.Buffer, *[]string) {
	var out bytes.Buffer
	var opened []string
	return env{
		stdout:    &out,
		setLogger: func(*slog.Logger) {},
		open: func(src, db string) (migrator, error) {
			opened = append(opened, src, db)
			if openErr != nil {
				return nil, openErr
			}
			return m, nil
		},
	}, &out, &opened
}

func TestRun_Directions(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantCalls []string
		wantSteps int
		wantLog   string
	}{
		{name: "default is up all", args: nil, wantCalls: []string{"up"}, wantLog: "migration completed"},
		{name: "up with steps", args: []string{"-direction", "up", "-steps", "2"}, wantCalls: []string{"steps"}, wantSteps: 2},
		{name: "down all", args: []string{"-direction", "down"}, wantCalls: []string{"down"}},
		{name: "down one step", args: []string{"-direction", "down", "-steps", "1"}, wantCalls: []string{"steps"}, wantSteps: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &fakeMigrator{}
			e, out, opened := testEnv(m, nil)
			args := append([]string{"-config", writeConfig(t)}, tt.args...)
			assert.Equal(t, 0, run(e, &bytes.Buffer{}, args))
			assert.Equal(t, tt.wantCalls, m.calls)
			assert.Equal(t, tt.wantSteps, m.steps)
			assert.True(t, m.closed, "the migrator must be closed")
			require.Len(t, *opened, 2)
			assert.Equal(t, "file://migration", (*opened)[0])
			assert.True(t, strings.HasPrefix((*opened)[1], "pgx5://"), "database URL must use the pgx5 driver: %s", (*opened)[1])
			if tt.wantLog != "" {
				assert.Contains(t, out.String(), tt.wantLog)
			}
		})
	}
}

func TestRun_NoChangeIsSuccess(t *testing.T) {
	m := &fakeMigrator{err: gomigrate.ErrNoChange}
	e, out, _ := testEnv(m, nil)
	assert.Equal(t, 0, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t)}))
	assert.Contains(t, out.String(), "no migration changes to apply")
}

func TestRun_Failures(t *testing.T) {
	t.Run("invalid flag values stop before the database", func(t *testing.T) {
		for _, tt := range []struct {
			args []string
			want string
		}{
			{[]string{"-direction", "sideways"}, `invalid -direction "sideways"`},
			{[]string{"-direction", "UP"}, `invalid -direction "UP"`},
			{[]string{"-steps", "-1"}, "-steps must be 0 or greater, got -1"},
			{[]string{"-direction", "down", "-steps", "-3"}, "-steps must be 0 or greater, got -3"},
		} {
			m := &fakeMigrator{}
			e, _, opened := testEnv(m, nil)
			var flagOut bytes.Buffer
			assert.Equal(t, 2, run(e, &flagOut, append([]string{"-config", writeConfig(t)}, tt.args...)), tt.args)
			assert.Empty(t, *opened, "must not connect: %v", tt.args)
			assert.Empty(t, m.calls, tt.args)
			assert.Contains(t, flagOut.String(), tt.want)
			assert.Contains(t, flagOut.String(), "Usage: elythia migrate")
		}
	})
	t.Run("migration error", func(t *testing.T) {
		m := &fakeMigrator{err: errors.New("boom")}
		e, out, _ := testEnv(m, nil)
		assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t)}))
		assert.Contains(t, out.String(), "migration failed")
	})
	t.Run("open error", func(t *testing.T) {
		e, out, _ := testEnv(nil, errors.New("dial"))
		assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t)}))
		assert.Contains(t, out.String(), "failed to create migrator")
	})
	t.Run("tls error carries the hint", func(t *testing.T) {
		certErr := &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}
		e, out, _ := testEnv(nil, certErr)
		assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t)}))
		assert.Contains(t, out.String(), "hint=")
	})
	t.Run("missing config", func(t *testing.T) {
		e, out, opened := testEnv(&fakeMigrator{}, nil)
		assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", filepath.Join(t.TempDir(), "absent.yml")}))
		assert.Contains(t, out.String(), "failed to load config")
		assert.Empty(t, *opened)
	})
}

func TestRun_Flags(t *testing.T) {
	var flagOut bytes.Buffer
	e, _, opened := testEnv(&fakeMigrator{}, nil)
	assert.Equal(t, 0, run(e, &flagOut, []string{"-h"}))
	for _, name := range []string{"-config", "-direction", "-steps"} {
		assert.Contains(t, flagOut.String(), name)
	}
	assert.Equal(t, 2, run(e, &bytes.Buffer{}, []string{"-nope"}))
	assert.Empty(t, *opened, "flag errors must stop before touching the database")
}

func TestDefaultEnv(t *testing.T) {
	e := defaultEnv()
	assert.Equal(t, os.Stdout, e.stdout)
	// 存在しない source を渡すと golang-migrate がエラーを返す。本物の
	// gomigrate.New に繋がっていることだけを確かめる。
	_, err := e.open("file://"+filepath.Join(t.TempDir(), "absent"), "pgx5://127.0.0.1:1/none")
	assert.Error(t, err)
}

func TestRunEntryPoint_MissingConfig(t *testing.T) {
	// 既定の env は slog.Default を張り替えるので、テストの後に戻す。
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	assert.Equal(t, 1, Run([]string{"-config", filepath.Join(t.TempDir(), "absent.yml")}))
}
