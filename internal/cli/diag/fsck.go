package diag

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/shiroha-a/mk/internal/cli/cliflag"
	"github.com/shiroha-a/mk/internal/core/fsck"
)

// fsckTimeout bounds the whole run. 集計クエリは全表走査になるので、
// healthcheck 系より長めに取る。
const fsckTimeout = 10 * time.Minute

// Fsck implements "elythia fsck": it checks denormalized counters against the
// rows they summarise and returns 0 (clean) or 1 (drift found). It is
// read-only unless -fix is given; orphan rows are never deleted.
func Fsck(args []string) int { return runFsck(defaultEnv(), args) }

// runFsck is Fsck with its dependencies passed in.
//
// **既定は読み取り専用。** -fix のときだけ書き戻す。孤児行は報告に留める
// (カウンタは元データから導けるが、削除した行は復元できない) (#2473)。
func runFsck(e env, args []string) int {
	fs := cliflag.New("fsck", e.stderr)
	path := fs.String("config", defaultConfigPath, "path to configuration file")
	fix := fs.Bool("fix", false, "write the recomputed counters back. Orphan rows are never deleted")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}

	cfg, ok := loadConfig(e, "fsck", *path)
	if !ok {
		return 1
	}
	db, err := e.openDB(cfg)
	if err != nil {
		fmt.Fprintf(e.stderr, "fsck: DB に接続できない: %v\n", err)
		return 1
	}
	defer e.closeDB(db)

	ctx, cancel := context.WithTimeout(context.Background(), fsckTimeout)
	defer cancel()

	report, err := e.runFsck(ctx, db, fsck.Options{Fix: *fix})
	if err != nil {
		fmt.Fprintf(e.stderr, "fsck: %v\n", err)
		return 1
	}
	printFsckReport(e.stdout, report, *fix)
	return fsckExitCode(report, *fix)
}

// fsckExitCode maps a report to the process exit code.
func fsckExitCode(r fsck.Report, fix bool) int {
	if r.OK() {
		return 0
	}
	// 修正したなら成功扱い。孤児だけが残っている場合は「対応が要る」ので 1。
	if fix && len(r.Orphans) == 0 {
		return 0
	}
	return 1
}

// printFsckReport writes the human-facing summary.
func printFsckReport(w io.Writer, r fsck.Report, fix bool) {
	fmt.Fprintln(w)
	if len(r.Drifts) == 0 {
		fmt.Fprintln(w, "  カウンタのずれは見つかりませんでした。")
	} else {
		// 全件は出さない。数千件になると読めないので、内訳と先頭だけ示す。
		byColumn := map[string]int{}
		for _, d := range r.Drifts {
			byColumn[d.Table+"."+d.Column]++
		}
		fmt.Fprintf(w, "  カウンタのずれ: %d 件\n", len(r.Drifts))
		for k, n := range byColumn {
			fmt.Fprintf(w, "    %-24s %d 件\n", k, n)
		}
		fmt.Fprintln(w)
		for i, d := range r.Drifts {
			if i >= 5 {
				fmt.Fprintf(w, "    ... 他 %d 件\n", len(r.Drifts)-i)
				break
			}
			fmt.Fprintf(w, "    %s.%s  id=%s  記録 %d → 実際 %d\n",
				d.Table, d.Column, d.ID, d.Stored, d.Actual)
		}
	}

	if len(r.Orphans) > 0 {
		fmt.Fprintln(w, "\n  孤児行 (自動削除はしません)")
		for _, o := range r.Orphans {
			fmt.Fprintf(w, "    %-12s %s: %d 件\n", o.Table, o.Reason, o.Count)
		}
		fmt.Fprintln(w, "    削除は影響を確認した上で手動で行ってください。")
	}

	fmt.Fprintln(w)
	switch {
	case fix && r.Repaired > 0:
		fmt.Fprintf(w, "  %d 件のカウンタを修正しました。\n", r.Repaired)
	case !fix && len(r.Drifts) > 0:
		fmt.Fprintln(w, "  修正するには -fix を付けて再実行してください。")
	}
	fmt.Fprintln(w)
}
