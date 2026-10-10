package backup

import (
	"context"
	"os"
	"os/exec"
)

// Runner builds the commands that run the PostgreSQL client programs.
//
// バックアップ用の image では pg_dump を同じコンテナの中で直接呼ぶ (LocalRunner)。
// テストでは、ホストの pg_dump の版がサーバー (18) と合わないので、testcontainers で
// 立てた postgres:18-alpine の中で動かす Runner に差し替える。
type Runner interface {
	// Command returns a command running name with args. env holds extra
	// "KEY=value" entries (such as PGPASSWORD) for the program.
	Command(ctx context.Context, env []string, name string, args ...string) *exec.Cmd
}

// LocalRunner runs programs on this host, inheriting its environment.
type LocalRunner struct{}

// Command implements Runner.
func (LocalRunner) Command(ctx context.Context, env []string, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	return cmd
}
