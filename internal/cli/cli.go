// Package cli dispatches the subcommands of the elythia binary.
//
// cmd/elythia は Main を呼ぶだけにして、どのサブコマンドがどの処理に繋がるかは
// ここで決める。main パッケージは import できずテストから振る舞いを確かめにくいので、
// 振り分けをテストできる側に置く。
package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/elythia-network/elythia/internal/cli/backfill"
	"github.com/elythia-network/elythia/internal/cli/diag"
	"github.com/elythia-network/elythia/internal/cli/migrate"
	"github.com/elythia-network/elythia/internal/cli/serve"
)

// Command is one subcommand. A command with Sub dispatches its first argument
// to one of them instead of running itself.
type Command struct {
	Name    string
	Summary string
	// Run receives the arguments after the command name and returns the
	// process exit code.
	Run func(args []string) int
	Sub []Command
}

// Commands returns the subcommands of the elythia binary, in the order the
// usage lists them.
func Commands() []Command {
	return []Command{
		{Name: "serve", Summary: "start the server", Run: serve.Serve},
		{Name: "migrate", Summary: "apply or roll back database migrations", Run: migrate.Run},
		{Name: "backfill", Summary: "run a one-off maintenance batch", Sub: []Command{
			{Name: "avatar-public-url", Summary: "point avatar / banner URLs at the metadata-stripped variant", Run: backfill.AvatarPublicURL},
			{Name: "emoji-system-file", Summary: "copy approved custom emoji images to system-owned files (dry-run unless -apply)", Run: backfill.EmojiSystemFile},
			{Name: "instance-counts", Summary: "recompute instance notesCount / usersCount", Run: backfill.InstanceCounts},
			{Name: "note-tags", Summary: "normalize note.tags to NFKC + lowercase", Run: backfill.NoteTags},
			{Name: "remote-host", Summary: "normalize stored remote hosts (UTS#46 + punycode)", Run: backfill.RemoteHost},
		}},
		{Name: "doctor", Summary: "check configuration, dependencies and federation, then exit", Run: diag.Doctor},
		{Name: "fsck", Summary: "check denormalized counters (read-only unless -fix)", Run: diag.Fsck},
		{Name: "config-dump", Summary: "print the resolved configuration with secrets masked", Run: diag.ConfigDump},
		{Name: "healthcheck", Summary: "GET /healthz on the configured port and exit 0 / 1", Run: diag.Healthcheck},
		{Name: "dump-routes", Summary: "print the registered HTTP routes as JSON without listening", Run: serve.DumpRoutes},
	}
}

// Main runs the elythia binary and returns the process exit code. argv0 is
// os.Args[0] and args is os.Args[1:].
func Main(argv0 string, args []string) int {
	return RunAs(argv0, args, Commands(), os.Stdout, os.Stderr)
}

// legacyMigrateName is the executable name the former migrate binary had.
const legacyMigrateName = "migrate"

// RunAs is Run, except that when the executable was invoked under the former
// migrate binary's name every argument goes to the migrate subcommand.
//
// **D11 の期限付きの例外 (#3394)。** 配布イメージの `:bundled` は develop への
// push ごとに出るので、古い compose (`migrate` サービスの
// `entrypoint: ["/app/migrate"]`) のまま `docker compose pull && up -d` した
// 運営者の migration が、新しい image で一斉に落ちる。image に `/app/migrate` を
// `elythia` への symlink として残し、その名前で起動されたときだけ以前の flag の
// まま migrate として動かす。**2.x の間だけ残し、3.0 で撤去する。** migrate 以外の
// 旧名は残さない (compose が直接呼んでいたのは migrate だけ)。
func RunAs(argv0 string, args []string, cmds []Command, stdout, stderr io.Writer) int {
	if filepath.Base(argv0) == legacyMigrateName {
		if cmd, ok := find(cmds, legacyMigrateName); ok {
			return cmd.Run(args)
		}
	}
	return Run(args, cmds, stdout, stderr)
}

// Run dispatches args to cmds. With no arguments or an unknown command it
// prints the usage to stderr and returns 2; "help", "-h" and "--help" print it
// to stdout and return 0. "help <command>" shows that command's flags.
//
// **引数が無いときにサーバーを起動しない。** 以前の misskey バイナリは引数無しで
// サーバーとして動いたが、サブコマンドにまとめた後も同じにすると、`elythia` を
// 叩いて使い方を見ようとした運営者が本番と同じ設定でもう 1 つ起動してしまう。
func Run(args []string, cmds []Command, stdout, stderr io.Writer) int {
	return dispatch("elythia", args, cmds, stdout, stderr)
}

func dispatch(prog string, args []string, cmds []Command, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr, prog, cmds)
		return 2
	}
	name := args[0]
	if isHelp(name) {
		if len(args) > 1 {
			// `help <command>` は `<command> -h` と同じ。
			return dispatch(prog, []string{args[1], "-h"}, cmds, stdout, stderr)
		}
		usage(stdout, prog, cmds)
		return 0
	}
	cmd, ok := find(cmds, name)
	if !ok {
		fmt.Fprintf(stderr, "%s: unknown command %q\n\n", prog, name)
		usage(stderr, prog, cmds)
		return 2
	}
	if len(cmd.Sub) > 0 {
		return dispatch(prog+" "+cmd.Name, args[1:], cmd.Sub, stdout, stderr)
	}
	return cmd.Run(args[1:])
}

func isHelp(arg string) bool {
	switch arg {
	case "help", "-h", "-help", "--help":
		return true
	}
	return false
}

func find(cmds []Command, name string) (Command, bool) {
	for _, c := range cmds {
		if c.Name == name {
			return c, true
		}
	}
	return Command{}, false
}

// usage writes the command list for prog.
func usage(w io.Writer, prog string, cmds []Command) {
	width := 0
	for _, c := range cmds {
		width = max(width, len(c.Name))
	}
	fmt.Fprintf(w, "Usage: %s <command> [flags]\n\nCommands:\n", prog)
	for _, c := range cmds {
		fmt.Fprintf(w, "  %-*s  %s\n", width, c.Name, c.Summary)
	}
	fmt.Fprintf(w, "\nRun \"%s <command> -h\" for the flags of a command.\n", prog)
	if prog == "elythia" {
		fmt.Fprintln(w, `Without a command nothing is started; use "elythia serve" to run the server.`)
	}
}
