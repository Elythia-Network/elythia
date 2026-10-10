package backup

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/cli/cliflag"
	"github.com/elythia-network/elythia/internal/cli/migrate"
	"github.com/elythia-network/elythia/internal/config"
)

// verifyEnv carries the dependencies of runVerify so tests can replace them.
type verifyEnv struct {
	stdout, stderr io.Writer
	loadConfig     func(path string) (*config.Config, error)
	openStorage    func(config.BackupStorageOptions) (backup.Storage, error)
	newSandbox     func(*config.BackupOptions) backup.Sandbox
	verify         func(context.Context, backup.Storage, string, backup.VerifyOptions) (backup.VerifyResult, error)
	// coreDir / localDir are the bundled migration directories, the same
	// ones `elythia migrate` and `elythia doctor` read.
	coreDir, localDir string
}

func defaultVerifyEnv() verifyEnv {
	return verifyEnv{
		stdout:      os.Stdout,
		stderr:      os.Stderr,
		loadConfig:  config.Load,
		openStorage: backup.OpenStorage,
		newSandbox: func(o *config.BackupOptions) backup.Sandbox {
			return backup.LocalSandbox{Tools: o.Tools}
		},
		verify:   backup.Verify,
		coreDir:  migrate.CoreDir,
		localDir: migrate.LocalDir,
	}
}

// Verify implements "elythia backup verify <id|latest>": it reads the
// generation, restores it into a throwaway PostgreSQL server, checks that this
// binary can serve it and stores verify.json next to meta.json. It returns 0
// when every stage passed (verify.json is stored by then) and 1 otherwise.
func Verify(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runVerify(ctx, defaultVerifyEnv(), args)
}

func runVerify(ctx context.Context, e verifyEnv, args []string) int {
	fs := cliflag.New("backup verify", e.stderr)
	fs.Usage = func() {
		fmt.Fprintf(e.stderr, "Usage: elythia backup verify [flags] <id|latest>\n\nFlags:\n")
		fs.PrintDefaults()
	}
	path := fs.String("config", defaultConfigPath, "path to configuration file")
	// 世代の ID を位置引数で受けるので、cliflag.Parse (位置引数を拒否する) は使わない。
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}

	cfg, err := e.loadConfig(*path)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia backup verify: failed to load config: %v\n", err)
		return 1
	}
	if cfg.Backup == nil {
		fmt.Fprintln(e.stderr, "elythia backup verify: the config file has no backup: section (see docs/configuration.md)")
		return 1
	}

	core, _, err := migrate.LatestVersion(e.coreDir)
	if err != nil || core == 0 {
		// 同梱の番号が分からないと「進みすぎていないか」を確かめられない。
		fmt.Fprintf(e.stderr, "elythia backup verify: cannot read the bundled migrations in %s (run it in /app of the backup image): %v\n", e.coreDir, err)
		return 1
	}
	local, _, err := migrate.LatestVersion(e.localDir)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia backup verify: cannot read the fork migrations in %s: %v\n", e.localDir, err)
		return 1
	}

	opts := backup.VerifyOptions{
		Sandbox: e.newSandbox(cfg.Backup),
		Bundled: backup.BundledMigrations{Core: int64(core), Local: int64(local)},
	}
	if f := cfg.Backup.Encryption.IdentityFile; f != "" {
		ids, err := backup.LoadIdentities(f)
		if err != nil {
			fmt.Fprintf(e.stderr, "elythia backup verify: cannot read backup.encryption.identityFile: %v\n", err)
			return 1
		}
		opts.Identities = ids
	}

	storage, err := e.openStorage(cfg.Backup.Storage)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia backup verify: failed to open storage: %v\n", err)
		return 1
	}
	id, err := backup.ResolveGenerationID(ctx, storage, fs.Arg(0))
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia backup verify: %v\n", err)
		return 1
	}

	res, err := e.verify(ctx, storage, id, opts)
	if len(res.Stages) > 0 {
		printResult(e.stdout, res)
	}
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia backup verify: %v\n", err)
		return 1
	}
	if !res.OK {
		return 1
	}
	return 0
}

// printResult writes the human-facing summary.
func printResult(w io.Writer, res backup.VerifyResult) {
	fmt.Fprintf(w, "generation %s\n", res.ID)
	for _, st := range res.Stages {
		mark := "ok  "
		switch {
		case st.Skipped:
			mark = "skip"
		case !st.OK:
			mark = "FAIL"
		}
		fmt.Fprintf(w, "  [%s] %s", mark, st.Stage)
		if st.Error != "" {
			fmt.Fprintf(w, ": %s", st.Error)
		}
		fmt.Fprintln(w)
		for _, warn := range st.Warnings {
			fmt.Fprintf(w, "         warning: %s\n", warn)
		}
	}
	for _, m := range res.Mismatches {
		actual := fmt.Sprint(m.Actual)
		if m.Actual < 0 {
			actual = "missing"
		}
		fmt.Fprintf(w, "  %s: meta.json %d rows / restored %s\n", m.Table, m.Expected, actual)
	}
	if res.OK {
		fmt.Fprintln(w, "This backup can be restored.")
	} else {
		fmt.Fprintln(w, "This backup cannot be restored.")
	}
}
