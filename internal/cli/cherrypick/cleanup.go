package cherrypick

import (
	"context"
	"fmt"

	"github.com/elythia-network/elythia/internal/cli/cliflag"
	"github.com/elythia-network/elythia/internal/maintenance"
)

// Cleanup implements "elythia cherrypick cleanup": once convert has finished,
// it drops the CherryPick columns and table whose meaning convert replaced
// (#4249). It is optional; Elythia runs with them left in place. It writes only
// with -apply and -backup-taken.
//
// 消したものは戻せない。convert が書き出した規則のファイルと、取ったバックアップが
// 元の値の唯一の写しになる。
func Cleanup(args []string) int { return cleanup(defaultConvertEnv(), args) }

func cleanup(e convertEnv, args []string) int {
	fs := cliflag.New("cherrypick cleanup", e.stderr)
	configPath := fs.String("config", ".config/default.yml", "path to configuration file")
	apply := fs.Bool("apply", false, "drop them; without it, only print what would be dropped")
	backupTaken := fs.Bool("backup-taken", false, "confirm that a backup of the database was taken after convert (required with -apply)")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(e.stderr, "elythia cherrypick cleanup: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	if *apply && !*backupTaken {
		fmt.Fprintln(e.stderr, "elythia cherrypick cleanup: -apply drops data that cannot be restored; "+
			"take a backup (elythia backup take) and pass -backup-taken")
		return 2
	}

	cfg, err := e.loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia cherrypick cleanup: load config: %v\n", err)
		return 1
	}
	db, closeDB, err := e.openDB(cfg)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia cherrypick cleanup: open database: %v\n", err)
		return 1
	}
	defer closeDB()
	ctx, cancel := context.WithTimeout(context.Background(), inspectTimeout)
	defer cancel()

	r, err := maintenance.InspectCherryPick(ctx, db, e.now())
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia cherrypick cleanup: %v\n", err)
		return 1
	}
	switch {
	case r.Status == maintenance.NotCherryPick:
		fmt.Fprintln(e.stdout, "this database did not come from CherryPick; nothing to clean up")
		return 0
	case r.Status == maintenance.CherryPickUnsupported:
		WriteReport(e.stdout, r)
		return ExitNeedsAction
	case !r.Converted:
		fmt.Fprintln(e.stderr, "elythia cherrypick cleanup: run `elythia cherrypick convert` first")
		return 1
	case r.Unprocessed() > 0:
		// 印が付いた後に要対応が増えることは無いはずだが、消す前にもう一度確かめる。
		WriteReport(e.stdout, r)
		fmt.Fprintln(e.stderr, "elythia cherrypick cleanup: some items still need action; run `elythia cherrypick convert` first")
		return ExitNeedsAction
	}

	left, err := maintenance.CherryPickLeftovers(ctx, db)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia cherrypick cleanup: %v\n", err)
		return 1
	}
	if len(left) == 0 {
		fmt.Fprintln(e.stdout, "nothing left to clean up")
		return 0
	}
	fmt.Fprintln(e.stdout, "CherryPick columns and tables Elythia does not use:")
	for _, l := range left {
		fmt.Fprintf(e.stdout, "  %s\n", l)
	}
	if !*apply {
		fmt.Fprintln(e.stdout, "\nnothing was dropped; pass -apply -backup-taken to drop them")
		return 0
	}
	// 消すのは数えるのと別の期限にする (数え上げが期限を使い切っても消せるように)。
	dropCtx, cancelDrop := context.WithTimeout(context.Background(), applyTimeout)
	defer cancelDrop()
	if err := maintenance.DropCherryPickLeftovers(dropCtx, db); err != nil {
		fmt.Fprintf(e.stderr, "elythia cherrypick cleanup: %v (stop Elythia before cleanup)\n", err)
		return 1
	}
	fmt.Fprintf(e.stdout, "\ndropped %d column(s) and table(s)\n", len(left))
	return 0
}
