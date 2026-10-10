package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/cli/backfill"
	clibackup "github.com/elythia-network/elythia/internal/cli/backup"
	"github.com/elythia-network/elythia/internal/cli/diag"
	"github.com/elythia-network/elythia/internal/cli/migrate"
	"github.com/elythia-network/elythia/internal/cli/serve"
)

// recorder builds a command table whose runners record the path they were
// reached by and the arguments they received.
type recorder struct {
	got  []string
	args [][]string
}

func (r *recorder) cmd(label, name string, code int) Command {
	return Command{Name: name, Summary: name + " summary", Run: func(args []string) int {
		r.got = append(r.got, label)
		r.args = append(r.args, args)
		return code
	}}
}

func (r *recorder) table() []Command {
	return []Command{
		r.cmd("serve", "serve", 11),
		r.cmd("migrate", "migrate", 12),
		{Name: "backfill", Summary: "batches", Sub: []Command{
			r.cmd("backfill/note-tags", "note-tags", 21),
			r.cmd("backfill/remote-host", "remote-host", 22),
		}},
		r.cmd("doctor", "doctor", 13),
	}
}

func run(t *testing.T, args ...string) (*recorder, int, string, string) {
	t.Helper()
	r := &recorder{}
	var stdout, stderr bytes.Buffer
	code := Run(args, r.table(), &stdout, &stderr)
	return r, code, stdout.String(), stderr.String()
}

func TestRun_NoArgsPrintsUsageAndStartsNothing(t *testing.T) {
	r, code, stdout, stderr := run(t)
	assert.Equal(t, 2, code)
	assert.Empty(t, r.got, "no runner may be invoked without a subcommand")
	assert.Empty(t, stdout)
	assert.Contains(t, stderr, "Usage: elythia <command> [flags]")
	assert.Contains(t, stderr, "serve")
	assert.Contains(t, stderr, `use "elythia serve" to run the server`)
}

func TestRun_UnknownCommand(t *testing.T) {
	// 以前の misskey バイナリの flag をそのまま渡しても、サーバーを起動しない。
	for _, args := range [][]string{{"nope"}, {"-config", "x.yml"}, {"-healthcheck"}} {
		r, code, _, stderr := run(t, args...)
		assert.Equal(t, 2, code, args)
		assert.Empty(t, r.got, args)
		assert.Contains(t, stderr, "elythia: unknown command "+`"`+args[0]+`"`)
		assert.Contains(t, stderr, "Usage: elythia")
	}
}

func TestRun_Help(t *testing.T) {
	for _, h := range []string{"help", "-h", "-help", "--help"} {
		r, code, stdout, stderr := run(t, h)
		assert.Equal(t, 0, code, h)
		assert.Empty(t, r.got, h)
		assert.Contains(t, stdout, "Usage: elythia <command> [flags]", h)
		assert.Empty(t, stderr, h)
	}
}

func TestRun_HelpForCommand(t *testing.T) {
	r, code, _, _ := run(t, "help", "migrate")
	assert.Equal(t, 12, code)
	assert.Equal(t, []string{"migrate"}, r.got)
	assert.Equal(t, [][]string{{"-h"}}, r.args)

	r, code, stdout, _ := run(t, "help", "backfill")
	assert.Equal(t, 0, code)
	assert.Empty(t, r.got)
	assert.Contains(t, stdout, "Usage: elythia backfill <command> [flags]")
	assert.Contains(t, stdout, "note-tags")

	_, code, _, stderr := run(t, "help", "nope")
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr, `unknown command "nope"`)
}

func TestRun_RoutesToTheNamedRunner(t *testing.T) {
	tests := []struct {
		args     []string
		want     string
		wantArgs []string
		wantCode int
	}{
		{args: []string{"serve"}, want: "serve", wantArgs: []string{}, wantCode: 11},
		{args: []string{"serve", "-config", "x.yml"}, want: "serve", wantArgs: []string{"-config", "x.yml"}, wantCode: 11},
		{args: []string{"migrate", "-direction", "down", "-steps", "1"}, want: "migrate", wantArgs: []string{"-direction", "down", "-steps", "1"}, wantCode: 12},
		{args: []string{"doctor", "-config", "y.yml"}, want: "doctor", wantArgs: []string{"-config", "y.yml"}, wantCode: 13},
		{args: []string{"backfill", "note-tags", "-dry-run"}, want: "backfill/note-tags", wantArgs: []string{"-dry-run"}, wantCode: 21},
		{args: []string{"backfill", "remote-host"}, want: "backfill/remote-host", wantArgs: []string{}, wantCode: 22},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			r, code, _, _ := run(t, tt.args...)
			assert.Equal(t, tt.wantCode, code)
			assert.Equal(t, []string{tt.want}, r.got)
			require.Len(t, r.args, 1)
			assert.Equal(t, tt.wantArgs, r.args[0])
		})
	}
}

func TestRun_BackfillGroup(t *testing.T) {
	r, code, _, stderr := run(t, "backfill")
	assert.Equal(t, 2, code)
	assert.Empty(t, r.got)
	assert.Contains(t, stderr, "Usage: elythia backfill <command> [flags]")
	assert.NotContains(t, stderr, `use "elythia serve"`)

	r, code, _, stderr = run(t, "backfill", "backfill-note-tags")
	assert.Equal(t, 2, code, "the old binary names are not accepted")
	assert.Empty(t, r.got)
	assert.Contains(t, stderr, `elythia backfill: unknown command "backfill-note-tags"`)

	r, code, _, _ = run(t, "backfill", "help", "note-tags")
	assert.Equal(t, 21, code)
	assert.Equal(t, [][]string{{"-h"}}, r.args)
}

// funcPtr identifies a top-level function so the real table can be compared
// against the runner each name must reach.
func funcPtr(f func([]string) int) uintptr { return reflect.ValueOf(f).Pointer() }

func TestCommands_WiresEveryNameToItsRunner(t *testing.T) {
	want := map[string]func([]string) int{
		"serve":                      serve.Serve,
		"migrate":                    migrate.Run,
		"doctor":                     diag.Doctor,
		"fsck":                       diag.Fsck,
		"config-dump":                diag.ConfigDump,
		"healthcheck":                diag.Healthcheck,
		"dump-routes":                serve.DumpRoutes,
		"backfill avatar-public-url": backfill.AvatarPublicURL,
		"backfill emoji-system-file": backfill.EmojiSystemFile,
		"backfill instance-counts":   backfill.InstanceCounts,
		"backfill note-tags":         backfill.NoteTags,
		"backfill remote-host":       backfill.RemoteHost,
		"backup take":                clibackup.Take,
		"backup list":                clibackup.List,
	}
	got := map[string]uintptr{}
	for _, c := range Commands() {
		assert.NotEmpty(t, c.Summary, c.Name)
		if len(c.Sub) > 0 {
			assert.Nil(t, c.Run, "a group must not run by itself: %s", c.Name)
			for _, s := range c.Sub {
				assert.NotEmpty(t, s.Summary, s.Name)
				got[c.Name+" "+s.Name] = funcPtr(s.Run)
			}
			continue
		}
		got[c.Name] = funcPtr(c.Run)
	}
	require.Len(t, got, len(want))
	for name, fn := range want {
		assert.Equal(t, funcPtr(fn), got[name], "%s is wired to the wrong runner", name)
	}
}

func TestMain_UsesTheRealTable(t *testing.T) {
	// 実際の表でも、引数無しは何も起動せずに 2 を返す。
	assert.Equal(t, 2, Main("/app/elythia", nil))
	assert.Equal(t, 0, Main("/app/elythia", []string{"help"}))
	// 旧名の migrate で起動されたら migrate の flag として解釈する (-h で 0)。
	assert.Equal(t, 0, Main("/app/migrate", []string{"-h"}))
}

// 旧 migrate バイナリの名前で起動されたら、引数を全部 migrate に渡す (#3394)。
// 古い compose の `entrypoint: ["/app/migrate"]` + `command: ["-config", ...]` の形。
func TestRunAs_LegacyMigrateName(t *testing.T) {
	for _, argv0 := range []string{"/app/migrate", "migrate", "./built/migrate"} {
		r := &recorder{}
		var stdout, stderr bytes.Buffer
		args := []string{"-config", ".config/default.yml", "-direction", "up"}
		code := RunAs(argv0, args, r.table(), &stdout, &stderr)
		assert.Equal(t, 12, code, argv0)
		assert.Equal(t, []string{"migrate"}, r.got, argv0)
		assert.Equal(t, [][]string{args}, r.args, argv0)
	}
}

func TestRunAs_OtherNamesUseSubcommands(t *testing.T) {
	for _, argv0 := range []string{"/app/elythia", "elythia", "/tmp/go-build123/exe/elythia", "/app/misskey", "migrate-tool"} {
		r := &recorder{}
		var stdout, stderr bytes.Buffer
		code := RunAs(argv0, []string{"-config", "x.yml"}, r.table(), &stdout, &stderr)
		assert.Equal(t, 2, code, argv0)
		assert.Empty(t, r.got, argv0)
		assert.Contains(t, stderr.String(), `unknown command "-config"`, argv0)

		r = &recorder{}
		assert.Equal(t, 11, RunAs(argv0, []string{"serve"}, r.table(), &stdout, &stderr), argv0)
		assert.Equal(t, []string{"serve"}, r.got, argv0)
	}
}

func TestRunAs_LegacyNameWithoutMigrateCommand(t *testing.T) {
	// 表に migrate が無ければ通常の振り分けに落ちる (旧名で別の処理を起動しない)。
	r := &recorder{}
	var stdout, stderr bytes.Buffer
	table := []Command{r.cmd("serve", "serve", 11)}
	assert.Equal(t, 2, RunAs("/app/migrate", nil, table, &stdout, &stderr))
	assert.Empty(t, r.got)
}
