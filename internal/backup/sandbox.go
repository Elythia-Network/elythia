package backup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/elythia-network/elythia/internal/config"
)

// SandboxCmd is one PostgreSQL program run inside a Sandbox. Program is the
// bare name ("pg_restore", "initdb", "pg_ctl"); the sandbox maps it to a path.
type SandboxCmd struct {
	Program string
	Args    []string
	// Stdin, when non-nil, is fed to the program. Stdout, when non-nil,
	// receives its standard output.
	Stdin  io.Reader
	Stdout io.Writer
}

// SandboxServer tells Verify how to start the throwaway server in a
// directory returned by Sandbox.MkdirTemp and how to reach it.
type SandboxServer struct {
	// Options are passed to postgres through `pg_ctl -o`. Values must not
	// contain spaces.
	Options []string
	// ToolHost / ToolPort are what programs inside the sandbox connect to.
	ToolHost string
	ToolPort int
	// Host / Port are what the verifying process itself connects to.
	Host string
	Port int
}

// Sandbox is where `backup verify` starts the throwaway PostgreSQL server.
//
// 本番ではバックアップ用の image (postgres:18-alpine + elythia) の中で、同じ
// プロセスから直接 initdb / pg_ctl を呼ぶ (LocalSandbox)。
//
// プログラムを動かすだけなら Take の Runner で足りるが、検証では一時ディレクトリを
// 作って消すことと、立てたサーバーへ「中のプログラム」と「このプロセス」がそれぞれ
// どこへ繋ぐかも要る。テストではサーバーを container の中に立て、このプロセスからは
// 割り当てた port で繋ぐので、それらを Runner とは別にここへ閉じる。プログラムの
// 実行そのものは LocalSandbox.Runner (Runner) に任せる。
type Sandbox interface {
	// Run runs one program and returns an error that includes its standard
	// error output when it exits non-zero.
	Run(ctx context.Context, cmd SandboxCmd) error
	// MkdirTemp creates an empty private directory for one verification.
	MkdirTemp(ctx context.Context) (string, error)
	// RemoveAll removes a directory returned by MkdirTemp.
	RemoveAll(ctx context.Context, dir string) error
	// Server describes the throwaway server whose files live under dir.
	Server(dir string) SandboxServer
}

// LocalSandbox runs the PostgreSQL programs as child processes of this
// process. It is what `elythia backup verify` uses in the backup image.
type LocalSandbox struct {
	// Tools overrides the program paths; empty fields are looked up in PATH.
	Tools config.BackupToolsOptions
	// Runner runs the programs. Nil means LocalRunner.
	Runner Runner
	// TempDir is the parent of the temporary directories; empty means
	// os.TempDir().
	TempDir string
}

// localSocketPort is the port of the throwaway server. 待ち受けは
// 一時ディレクトリの unix socket だけなので、番号は socket のファイル名にしか
// 使われず、他のサーバーと衝突しない。
const localSocketPort = 5432

func (s LocalSandbox) path(program string) string {
	override := map[string]string{
		"pg_restore": s.Tools.PgRestore,
		"initdb":     s.Tools.Initdb,
		"pg_ctl":     s.Tools.PgCtl,
		"pg_dump":    s.Tools.PgDump,
		"psql":       s.Tools.Psql,
	}[program]
	if override != "" {
		return override
	}
	return program
}

// Run implements Sandbox.
func (s LocalSandbox) Run(ctx context.Context, cmd SandboxCmd) error {
	runner := s.Runner
	if runner == nil {
		runner = LocalRunner{}
	}
	c := runner.Command(ctx, nil, s.path(cmd.Program), cmd.Args...)
	c.Stdin = cmd.Stdin
	c.Stdout = cmd.Stdout
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", cmd.Program, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// MkdirTemp implements Sandbox.
// The socket directory is created up front because postgres does not create
// unix_socket_directories itself.
func (s LocalSandbox) MkdirTemp(context.Context) (string, error) {
	dir, err := os.MkdirTemp(s.TempDir, "elythia-verify-")
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(filepath.Join(dir, "sock"), 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// RemoveAll implements Sandbox.
func (s LocalSandbox) RemoveAll(_ context.Context, dir string) error {
	return os.RemoveAll(dir)
}

// Server implements Sandbox. The server listens only on a unix socket in dir,
// so nothing outside this host (or container) can reach it.
func (s LocalSandbox) Server(dir string) SandboxServer {
	sock := filepath.Join(dir, "sock")
	return SandboxServer{
		Options:  []string{"-c", "listen_addresses=", "-c", "unix_socket_directories=" + sock},
		ToolHost: sock,
		ToolPort: localSocketPort,
		Host:     sock,
		Port:     localSocketPort,
	}
}
