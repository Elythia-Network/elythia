// Package cherrypick implements "elythia cherrypick <name>", which moves a
// CherryPick 4.18.0-beta.1 database to Elythia (#4246).
//
// CherryPick の DB は Elythia の migrate up をそのまま通って起動するが、CherryPick
// にしか無い制限 (承認待ち・隔離・通報の自動処理の規則・予約削除) はエラーにならずに
// 外れる。check はそれを数えて示す。
package cherrypick

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/elythia-network/elythia/internal/cli/cliflag"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/maintenance"
)

// ExitNeedsAction is the exit code of check when the database came from
// CherryPick and something still needs action, or its version is not
// supported.
const ExitNeedsAction = 3

// inspectTimeout bounds one inspection of convert (maintenance.CherryPickInspectTimeout).
const inspectTimeout = maintenance.CherryPickInspectTimeout

// env carries the process-level dependencies so tests can replace them.
type env struct {
	stdout, stderr io.Writer
	loadConfig     func(path string) (*config.Config, error)
	inspect        func(cfg *config.Config) (maintenance.CherryPickReport, error)
}

func defaultEnv() env {
	return env{
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		loadConfig: config.Load,
		inspect:    Inspect,
	}
}

// Inspect connects to the database of cfg and inspects it with
// maintenance.InspectCherryPick.
func Inspect(cfg *config.Config) (maintenance.CherryPickReport, error) {
	return maintenance.InspectCherryPickDSN(cfg.DSN(), time.Now(), maintenance.InspectCherryPick)
}

// Check implements "elythia cherrypick check": it reads the database and
// prints what Elythia would drop. It exits 0 when the database did not come
// from CherryPick or nothing needs action, and ExitNeedsAction otherwise.
func Check(args []string) int { return check(defaultEnv(), args) }

func check(e env, args []string) int {
	fs := cliflag.New("cherrypick check", e.stderr)
	configPath := fs.String("config", ".config/default.yml", "path to configuration file")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(e.stderr, "elythia cherrypick check: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	cfg, err := e.loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia cherrypick check: load config: %v\n", err)
		return 1
	}
	r, err := e.inspect(cfg)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia cherrypick check: %v\n", err)
		return 1
	}
	// CherryPick から来た DB でなければ何も出さない。migrate の前に毎回流しても
	// 邪魔にならないようにするため。
	if r.Status == maintenance.NotCherryPick {
		return 0
	}
	WriteReport(e.stdout, r)
	if r.Unprocessed() > 0 {
		return ExitNeedsAction
	}
	return 0
}

// WriteReport prints r for an operator.
func WriteReport(w io.Writer, r maintenance.CherryPickReport) {
	if r.Status == maintenance.CherryPickUnsupported {
		fmt.Fprintf(w, "this database came from a CherryPick version other than %s, which is the only one supported:\n  %s\n",
			maintenance.CherryPickVersion, r.Reason)
		return
	}
	fmt.Fprintf(w, "this database came from CherryPick %s\n\n", maintenance.CherryPickVersion)

	fmt.Fprintln(w, "needs action (Elythia does not keep these):")
	fmt.Fprintf(w, "  users waiting for approval (they can sign in on Elythia)  %d\n", len(r.PendingUsers))
	for _, u := range r.PendingUsers {
		reason := "(no reason)"
		if u.SignupReason != nil {
			reason = strconv.Quote(*u.SignupReason)
		}
		fmt.Fprintf(w, "    @%s  %s  %s\n", u.Username, u.ID, reason)
	}
	fmt.Fprintf(w, "  abuse report resolvers (not applied)                      %d\n", r.AbuseReportResolvers)
	fmt.Fprintf(w, "  quarantined instances (the restriction is lost)           %d\n", len(r.QuarantinedHosts))
	for _, h := range r.QuarantinedHosts {
		fmt.Fprintf(w, "    %s\n", h)
	}
	fmt.Fprintf(w, "  scheduled deletes (not deleted): due                      %d\n", r.ScheduledDeletesDue)
	fmt.Fprintf(w, "  scheduled deletes (not deleted): not yet due              %d\n", r.ScheduledDeletesLater)
	if r.RepositoryURL != "" {
		fmt.Fprintf(w, "  repositoryUrl still points at CherryPick                  %s\n", r.RepositoryURL)
	}
	if r.FeedbackURL != "" {
		fmt.Fprintf(w, "  feedbackUrl still points at CherryPick                    %s\n", r.FeedbackURL)
	}

	fmt.Fprintln(w, "\nkept in the database but not read by Elythia:")
	fmt.Fprintf(w, "  note edit history                          %d\n", r.NoteHistories)
	fmt.Fprintf(w, "  events                                     %d\n", r.Events)
	fmt.Fprintf(w, "  users with automatic deletion of old notes %d\n", r.AutoDeleteUsers)
	fmt.Fprintf(w, "  drafts with a scheduled delete             %d\n", r.DraftScheduledDeletes)
	fmt.Fprintf(w, "  remote notes with a scheduled delete       %d\n", r.RemoteScheduledDeletes)
	// 承認制にする前からの利用者と、管理者が作ったアカウント。CherryPick はパスワード
	// でのサインインを断るが、今のセッションはそのまま使わせている。申請した人では
	// ないので、要対応には数えない。
	fmt.Fprintf(w, "  members without the approval flag          %d  (CherryPick refuses their password sign-in; Elythia does not)\n", r.UnapprovedMembers)

	if n := r.Unprocessed(); n > 0 {
		fmt.Fprintf(w, "\n%d kind(s) of items need action before Elythia serves this database\n", n)
		return
	}
	fmt.Fprintln(w, "\nnothing needs action")
}
