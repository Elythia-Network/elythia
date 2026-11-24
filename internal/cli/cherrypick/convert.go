package cherrypick

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/cli/cliflag"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/maintenance"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server"
)

// deleter deletes accounts and notes through the server's delete paths, the
// way admin/delete-account and notes/delete do.
type deleter interface {
	DeleteMigratedAccount(userID string) error
	DeleteMigratedNote(noteID string) error
	// Wait waits until the background work the deletes started (queueing the
	// deliveries) has finished.
	Wait(ctx context.Context) error // the note deletes' deliveries
	// Close stops the server and releases the connections. It fails when
	// background work was given up while stopping.
	Close() error
}

// convertEnv carries the process-level dependencies of convert so tests can
// replace them.
type convertEnv struct {
	stdout, stderr io.Writer
	loadConfig     func(path string) (*config.Config, error)
	openDB         func(cfg *config.Config) (*gorm.DB, func(), error)
	newDeleter     func(cfg *config.Config) (deleter, error)
	// createFile creates path for the abuse report resolvers and fails when
	// it exists.
	createFile func(path string) (io.WriteCloser, error)
	now        func() time.Time
}

func defaultConvertEnv() convertEnv {
	return convertEnv{
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		loadConfig: config.Load,
		openDB:     openDB,
		newDeleter: newServerDeleter,
		createFile: func(path string) (io.WriteCloser, error) {
			// 書き出した規則は、元の表を消した後の唯一の写しになる。前の実行の
			// ファイルを上書きしないよう、既にあれば失敗させる。
			return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		},
		now: time.Now,
	}
}

// openDB connects to the database of cfg the way the server does.
func openDB(cfg *config.Config) (*gorm.DB, func(), error) {
	db, err := model.NewDatabase(cfg)
	if err != nil {
		return nil, nil, err
	}
	return db, func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}, nil
}

// migrationServer is the part of *server.Server serverDeleter uses.
type migrationServer interface {
	DeleteMigratedAccount(userID string) error
	DeleteMigratedNote(noteID string) error
	WaitNoteDeletes(ctx context.Context) error
	Shutdown(ctx context.Context) error
	BackgroundDropped() int64
}

// serverDeleter is a built server that does not listen.
type serverDeleter struct {
	srv         migrationServer
	closeDB     func()
	closeRedis  func() error
	stopTimeout time.Duration
}

// newServerDeleter builds the server (like "elythia dump-routes") to call its
// delete paths.
func newServerDeleter(cfg *config.Config) (deleter, error) {
	return buildServerDeleter(cfg, openDB, cache.NewRedisClients, server.New)
}

// buildServerDeleter is newServerDeleter with the connections and the
// construction passed in, so tests can point them at the test database.
func buildServerDeleter(cfg *config.Config,
	open func(*config.Config) (*gorm.DB, func(), error),
	openRedis func(*config.Config) (*cache.RedisClients, error),
	build func(*config.Config, *gorm.DB, *cache.RedisClients) (*server.Server, error),
) (deleter, error) {
	db, closeDB, err := open(cfg)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	redisClients, err := openRedis(cfg)
	if err != nil {
		closeDB()
		return nil, fmt.Errorf("connect to Redis: %w", err)
	}
	srv, err := build(cfg, db, redisClients)
	if err != nil {
		_ = redisClients.Close()
		closeDB()
		return nil, fmt.Errorf("build the server: %w", err)
	}
	return &serverDeleter{srv: srv, closeDB: closeDB, closeRedis: redisClients.Close, stopTimeout: time.Minute}, nil
}

func (d *serverDeleter) DeleteMigratedAccount(userID string) error {
	return d.srv.DeleteMigratedAccount(userID)
}

func (d *serverDeleter) DeleteMigratedNote(noteID string) error {
	return d.srv.DeleteMigratedNote(noteID)
}

func (d *serverDeleter) Wait(ctx context.Context) error {
	return d.srv.WaitNoteDeletes(ctx)
}

func (d *serverDeleter) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), d.stopTimeout)
	defer cancel()
	err := d.srv.Shutdown(ctx)
	// 捨てた処理があれば、削除が連合先へ届いていないことがある。印を付けない。
	if n := d.srv.BackgroundDropped(); n > 0 {
		err = errors.Join(err, fmt.Errorf("%d piece(s) of background work were given up while stopping "+
			"(they may include deliveries of the deletes)", n))
	}
	err = errors.Join(err, d.closeRedis())
	d.closeDB()
	return err
}

// deliveriesLostHint tells that running convert again does not queue the
// deliveries of notes already deleted.
const deliveriesLostHint = "elythia cherrypick convert: the deleted notes are gone, so running convert again does not queue " +
	"their deliveries again; some remote servers may keep showing them"

// applyTimeout bounds the changes and the deletes of one convert.
const applyTimeout = time.Hour

// convertChoices are the operator's choices.
type convertChoices struct {
	pendingUsers  string
	unapproved    string
	quarantined   string
	laterDeletes  string
	resolversOut  string
	acceptReports bool
}

// Convert implements "elythia cherrypick convert": it replaces what Elythia
// would drop from a CherryPick database with Elythia's own mechanisms, as the
// operator chose (#4248). It writes only with -apply.
//
// Elythia の migrate up の後に流す。前の DB は CherryPick の形で、Elythia の削除の
// 処理が使えないため。流れは check → migrate up -allow-cherrypick → convert → 起動。
func Convert(args []string) int { return convert(defaultConvertEnv(), args) }

func convert(e convertEnv, args []string) int {
	fs := cliflag.New("cherrypick convert", e.stderr)
	configPath := fs.String("config", ".config/default.yml", "path to configuration file")
	apply := fs.Bool("apply", false, "make the changes; without it, only print what would be done")
	var c convertChoices
	fs.StringVar(&c.pendingUsers, "pending-users", maintenance.CherryPickSuspend,
		"users waiting for approval: suspend (decide later from the admin page), delete or approve")
	fs.StringVar(&c.unapproved, "unapproved-members", "",
		"users without the approval flag and without a signup reason (members from before approval was turned on, "+
			"accounts an administrator created, applicants who confirmed their email later): suspend, delete or approve. "+
			"Required when there are any")
	fs.StringVar(&c.quarantined, "quarantined", "",
		"quarantined instances: block (add them to the blocked hosts) or lift. Required when there are any")
	fs.StringVar(&c.laterDeletes, "later-deletes", "",
		"local notes whose scheduled delete is not due yet: delete now or keep (never deleted). Required when there are any")
	fs.StringVar(&c.resolversOut, "resolvers-out", "",
		"file to write the abuse report resolvers to before they are removed (must not exist). Required when there are any")
	fs.BoolVar(&c.acceptReports, "accept-manual-reports", false,
		"accept that Elythia does not resolve abuse reports automatically. Required when there are abuse report resolvers")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(e.stderr, "elythia cherrypick convert: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	if msg := validateChoiceValues(c); msg != "" {
		fmt.Fprintf(e.stderr, "elythia cherrypick convert: %s\n", msg)
		fs.Usage()
		return 2
	}

	cfg, err := e.loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia cherrypick convert: load config: %v\n", err)
		return 1
	}
	db, closeDB, err := e.openDB(cfg)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia cherrypick convert: open database: %v\n", err)
		return 1
	}
	defer closeDB()
	// 段ごとに時間の上限を分ける。数え上げは note の表を全部読むので、1 つの期限を
	// 全段で共有すると、大きな DB では最後の数え上げで時間切れになる。
	ctx, cancel := context.WithTimeout(context.Background(), inspectTimeout)
	defer cancel()

	now := e.now()
	r, err := maintenance.InspectCherryPick(ctx, db, now)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia cherrypick convert: %v\n", err)
		return 1
	}
	switch r.Status {
	case maintenance.NotCherryPick:
		fmt.Fprintln(e.stdout, "this database did not come from CherryPick; nothing to convert")
		return 0
	case maintenance.CherryPickUnsupported:
		WriteReport(e.stdout, r)
		return ExitNeedsAction
	}
	if r.Converted {
		fmt.Fprintln(e.stdout, "this database has already been converted")
		return 0
	}
	migrated, err := maintenance.ElythiaMigrated(ctx, db)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia cherrypick convert: %v\n", err)
		return 1
	}
	if !migrated {
		fmt.Fprintln(e.stderr, "elythia cherrypick convert: Elythia has not migrated this database yet; "+
			"run `elythia migrate -allow-cherrypick` first (convert uses Elythia's tables)")
		return 1
	}

	due, later, err := maintenance.CherryPickScheduledDeletes(ctx, db, now)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia cherrypick convert: %v\n", err)
		return 1
	}
	if missing := missingChoices(r, c); len(missing) > 0 {
		writePlan(e.stdout, r, c, due, later)
		fmt.Fprintln(e.stderr, "elythia cherrypick convert: choose what to do with:")
		for _, m := range missing {
			fmt.Fprintf(e.stderr, "  %s\n", m)
		}
		return 2
	}
	writePlan(e.stdout, r, c, due, later)
	if !*apply {
		fmt.Fprintln(e.stdout, "\nnothing was changed; pass -apply to make these changes")
		return 0
	}

	applyCtx, cancelApply := context.WithTimeout(context.Background(), applyTimeout)
	defer cancelApply()
	if code := e.apply(applyCtx, cfg, db, r, c, due, later, now); code != 0 {
		return code
	}

	afterCtx, cancelAfter := context.WithTimeout(context.Background(), inspectTimeout)
	defer cancelAfter()
	after, err := maintenance.InspectCherryPick(afterCtx, db, e.now())
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia cherrypick convert: %v\n", err)
		return 1
	}
	if after.Unprocessed() > 0 {
		fmt.Fprintln(e.stderr, "elythia cherrypick convert: some items still need action; run it again")
		WriteReport(e.stdout, after)
		return ExitNeedsAction
	}
	if err := maintenance.MarkCherryPickConverted(afterCtx, db); err != nil {
		fmt.Fprintf(e.stderr, "elythia cherrypick convert: %v\n", err)
		return 1
	}
	fmt.Fprintln(e.stdout, "\nconverted; Elythia can now serve this database")
	return 0
}

// apply makes the changes. 先に SQL だけで済むものを 1 つのトランザクションで直し、
// その後で本体の削除の処理を呼ぶ。途中で失敗しても、数え方が行の状態なので、
// もう一度流せば残りだけを処理する。
func (e convertEnv) apply(ctx context.Context, cfg *config.Config, db *gorm.DB, r maintenance.CherryPickReport,
	c convertChoices, due, later []string, now time.Time) int {
	plan := maintenance.CherryPickPlan{
		PendingAction:        c.pendingUsers,
		QuarantinedHosts:     r.QuarantinedHosts,
		QuarantineAction:     c.quarantined,
		KeepLaterDeletes:     len(later) > 0 && c.laterDeletes == "keep",
		RepositoryURL:        r.RepositoryURL,
		ElythiaRepositoryURL: config.MkGoRepositoryURL,
		FeedbackURL:          r.FeedbackURL,
		ElythiaFeedbackURL:   config.MkGoFeedbackURL,
		Now:                  now,
	}
	for _, u := range r.PendingUsers {
		plan.PendingUserIDs = append(plan.PendingUserIDs, u.ID)
	}
	// 消すときは本体の削除の処理を通すので、id は SQL の前に読む (凍結・承認は
	// 条件でまとめて更新する)。
	var unapprovedIDs []string
	if r.UnapprovedMembers > 0 {
		plan.UnapprovedAction = c.unapproved
		if c.unapproved == maintenance.CherryPickDelete {
			ids, err := maintenance.CherryPickUnapprovedMemberIDs(ctx, db)
			if err != nil {
				fmt.Fprintf(e.stderr, "elythia cherrypick convert: %v\n", err)
				return 1
			}
			unapprovedIDs = ids
		}
	}

	if r.AbuseReportResolvers > 0 {
		if err := e.writeResolvers(ctx, db, c.resolversOut); err != nil {
			fmt.Fprintf(e.stderr, "elythia cherrypick convert: %v\n", err)
			return 1
		}
		fmt.Fprintf(e.stdout, "wrote the abuse report resolvers to %s\n", c.resolversOut)
		plan.DeleteResolvers = true
	}
	if err := maintenance.ApplyCherryPickSQL(ctx, db, plan); err != nil {
		fmt.Fprintf(e.stderr, "elythia cherrypick convert: %v\n", err)
		return 1
	}

	var accounts, notes []string
	if c.pendingUsers == maintenance.CherryPickDelete {
		accounts = append(accounts, plan.PendingUserIDs...)
	}
	accounts = append(accounts, unapprovedIDs...)
	notes = append(notes, due...)
	if c.laterDeletes == "delete" {
		notes = append(notes, later...)
	}
	if len(accounts) == 0 && len(notes) == 0 {
		return 0
	}
	d, err := e.newDeleter(cfg)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia cherrypick convert: %v\n", err)
		return 1
	}
	code := 0
	var failedAccounts []string
	for _, id := range accounts {
		if err := d.DeleteMigratedAccount(id); err != nil {
			fmt.Fprintf(e.stderr, "elythia cherrypick convert: delete account %s: %v\n", id, err)
			// flag が立つ前に失敗したものは承認待ちのまま残るので、流し直せばもう一度
			// 処理される。案内するのは、flag が立って後片付けだけを積めなかったもの。
			if errors.Is(err, server.ErrAccountCleanupNotQueued) {
				failedAccounts = append(failedAccounts, id)
			}
			code = 1
		}
	}
	if len(failedAccounts) > 0 {
		// 削除の flag が立った後で後片付けを積めなかった利用者は、流し直しても数えられない
		// (凍結・削除済みは承認待ちに数えない)。起動した後に管理画面から消し直してもらう。
		fmt.Fprintf(e.stderr, "elythia cherrypick convert: after Elythia starts, delete these accounts again from the admin page: %s\n",
			strings.Join(failedAccounts, " "))
	}
	if code == 0 {
		for _, id := range notes {
			if err := d.DeleteMigratedNote(id); err != nil {
				fmt.Fprintf(e.stderr, "elythia cherrypick convert: delete note %s: %v\n", id, err)
				code = 1
				break
			}
		}
		// 連合への配送は裏で積まれる。止める前に、積み終わるのを待つ。待たずに止めると、
		// 停止の待ちを超えた分が捨てられる。削除のループが期限を使い切っても待てるよう、
		// 期限は取り直す。
		waitCtx, cancelWait := context.WithTimeout(context.Background(), applyTimeout)
		if err := d.Wait(waitCtx); err != nil {
			fmt.Fprintf(e.stderr, "elythia cherrypick convert: wait for the deliveries of the deleted notes: %v\n", err)
			fmt.Fprintln(e.stderr, deliveriesLostHint)
			code = 1
		}
		cancelWait()
	}
	// 削除の配送はジョブとして積まれる。積み終わるのを待ってから閉じる。
	if err := d.Close(); err != nil {
		fmt.Fprintf(e.stderr, "elythia cherrypick convert: stop the server: %v\n", err)
		if len(notes) > 0 {
			fmt.Fprintln(e.stderr, deliveriesLostHint)
		}
		code = 1
	}
	if code == 0 {
		fmt.Fprintf(e.stdout, "deleted %d account(s) and %d note(s); the deliveries are sent once Elythia starts\n",
			len(accounts), len(notes))
	}
	return code
}

// writeResolvers writes the abuse report resolvers to path as JSON.
func (e convertEnv) writeResolvers(ctx context.Context, db *gorm.DB, path string) error {
	rows, err := maintenance.CherryPickResolvers(ctx, db)
	if err != nil {
		return err
	}
	f, err := e.createFile(path)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create %s: %w (a file from an earlier run is kept as it is; pass another path)", path, err)
		}
		return fmt.Errorf("create %s: %w", path, err)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rows); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	// 書き出した後で元の行を消すので、ディスクへ書き切ってから閉じる。
	if s, ok := f.(interface{ Sync() error }); ok {
		if err := s.Sync(); err != nil {
			_ = f.Close()
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// validateChoiceValues checks the values of the choice flags.
func validateChoiceValues(c convertChoices) string {
	switch c.pendingUsers {
	case maintenance.CherryPickSuspend, maintenance.CherryPickDelete, maintenance.CherryPickApprove:
	default:
		return fmt.Sprintf("invalid -pending-users %q (want suspend, delete or approve)", c.pendingUsers)
	}
	switch c.unapproved {
	case "", maintenance.CherryPickSuspend, maintenance.CherryPickDelete, maintenance.CherryPickApprove:
	default:
		return fmt.Sprintf("invalid -unapproved-members %q (want suspend, delete or approve)", c.unapproved)
	}
	switch c.quarantined {
	case "", maintenance.CherryPickBlock, maintenance.CherryPickLift:
	default:
		return fmt.Sprintf("invalid -quarantined %q (want block or lift)", c.quarantined)
	}
	switch c.laterDeletes {
	case "", "delete", "keep":
	default:
		return fmt.Sprintf("invalid -later-deletes %q (want delete or keep)", c.laterDeletes)
	}
	return ""
}

// missingChoices lists the choices r needs that c does not make.
func missingChoices(r maintenance.CherryPickReport, c convertChoices) []string {
	var missing []string
	// 既定値を置かない。凍結を既定にすると承認制の前からの会員がまとめて
	// サインインできなくなり、承認を既定にすると管理者が承認していない申請者を通す。
	if r.UnapprovedMembers > 0 && c.unapproved == "" {
		missing = append(missing, "users without the approval flag: -unapproved-members suspend|delete|approve")
	}
	if len(r.QuarantinedHosts) > 0 && c.quarantined == "" {
		missing = append(missing, "quarantined instances: -quarantined block|lift")
	}
	if r.ScheduledDeletesLater > 0 && c.laterDeletes == "" {
		missing = append(missing, "scheduled deletes not due yet: -later-deletes delete|keep")
	}
	if r.AbuseReportResolvers > 0 && (c.resolversOut == "" || !c.acceptReports) {
		missing = append(missing, "abuse report resolvers: -resolvers-out <file> and -accept-manual-reports")
	}
	return missing
}

// writePlan prints what convert does with c.
func writePlan(w io.Writer, r maintenance.CherryPickReport, c convertChoices, due, later []string) {
	fmt.Fprintf(w, "converting a CherryPick %s database:\n", maintenance.CherryPickVersion)
	if n := len(r.PendingUsers); n > 0 {
		fmt.Fprintf(w, "  users waiting for approval  %d  -> %s\n", n, c.pendingUsers)
		// 凍結した後は、Elythia の管理画面に申請の理由が出ない (Elythia は signupReason を
		// 読まない)。承認するか消すかを決める材料として、ここで出しておく。
		for _, u := range r.PendingUsers {
			reason := "(no reason)"
			if u.SignupReason != nil {
				reason = strconv.Quote(*u.SignupReason)
			}
			fmt.Fprintf(w, "    @%s  %s  %s\n", u.Username, u.ID, reason)
		}
	}
	if r.UnapprovedMembers > 0 {
		fmt.Fprintf(w, "  users without approval      %d  -> %s\n", r.UnapprovedMembers, orChoose(c.unapproved))
		writeUsernames(w, r.UnapprovedMemberNames, r.UnapprovedMembers)
	}
	if r.AbuseReportResolvers > 0 {
		fmt.Fprintf(w, "  abuse report resolvers      %d  -> written to %s and removed\n", r.AbuseReportResolvers, orChoose(c.resolversOut))
	}
	if n := len(r.QuarantinedHosts); n > 0 {
		fmt.Fprintf(w, "  quarantined instances       %d  -> %s\n", n, orChoose(c.quarantined))
	}
	if len(due) > 0 {
		fmt.Fprintf(w, "  scheduled deletes, due      %d  -> delete\n", len(due))
	}
	if len(later) > 0 {
		fmt.Fprintf(w, "  scheduled deletes, not due  %d  -> %s\n", len(later), orChoose(c.laterDeletes))
	}
	if r.RepositoryURL != "" {
		fmt.Fprintf(w, "  repositoryUrl               -> %s\n", config.MkGoRepositoryURL)
	}
	if r.FeedbackURL != "" {
		fmt.Fprintf(w, "  feedbackUrl                 -> %s\n", config.MkGoFeedbackURL)
	}
	if r.Unprocessed() == 0 {
		fmt.Fprintln(w, "  nothing needs action")
	}
	writeStaffNotes(w, r)
}

func orChoose(s string) string {
	if s == "" {
		return "(choose)"
	}
	return s
}
