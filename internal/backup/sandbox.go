package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

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
	// Run runs one program. When the program ran and exited with a failure
	// status, the error is (or wraps) a *ProgramError. Any other error means
	// the program could not be run at all (missing, not executable, killed).
	Run(ctx context.Context, cmd SandboxCmd) error
	// MkdirTemp creates an empty private directory for one verification.
	MkdirTemp(ctx context.Context) (string, error)
	// RemoveAll removes a directory returned by MkdirTemp.
	RemoveAll(ctx context.Context, dir string) error
	// Server describes the throwaway server whose files live under dir.
	Server(dir string) SandboxServer
}

// ProgramError is a program that ran and exited with a failure status.
//
// verify は「プログラムが dump を拒んだ」(世代の欠陥) と「プログラムを動かせなかった」
// (環境の誤り) を分ける。前者だけをこの型で返す。
type ProgramError struct {
	Program  string
	ExitCode int
	// Stderr is the tail of the program's standard error output.
	Stderr string
}

func (e *ProgramError) Error() string {
	return fmt.Sprintf("%s exited with status %d: %s", e.Program, e.ExitCode, e.Stderr)
}

// SpaceReporter is implemented by sandboxes that can tell how much space is
// free under a directory returned by MkdirTemp.
type SpaceReporter interface {
	FreeBytes(ctx context.Context, dir string) (int64, error)
}

// freeBytes returns the space available to an unprivileged user under dir.
func freeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", dir, err)
	}
	return int64(st.Bavail) * int64(st.Bsize), nil //nolint:gosec // ブロック数と大きさは int64 に収まる
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
		msg := strings.TrimSpace(stderr.String())
		// 126 / 127 は sh や docker exec が「実行できない / 見つからない」に使う番号で、
		// プログラム自身の失敗ではない。シグナルで止まったもの (-1) も同じ扱い。
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if code := exitErr.ExitCode(); code > 0 && code != 126 && code != 127 {
				return &ProgramError{Program: cmd.Program, ExitCode: code, Stderr: msg}
			}
		}
		return fmt.Errorf("%s: %w: %s", cmd.Program, err, msg)
	}
	return nil
}

// FreeBytes implements SpaceReporter.
func (s LocalSandbox) FreeBytes(_ context.Context, dir string) (int64, error) {
	return freeBytes(dir)
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
