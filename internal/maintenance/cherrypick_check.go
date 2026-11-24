package maintenance

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// CherryPickVersion is the only CherryPick version whose database
// "elythia cherrypick" handles (#4246).
const CherryPickVersion = "4.18.0-beta.1"

// cherryPickLatestMigration is the timestamp of the newest migration of
// CherryPick 4.18.0-beta.1 (ApprovalSignup1768893580518).
const cherryPickLatestMigration int64 = 1768893580518

// cherryPickMigrations are the TypeORM migration names CherryPick
// 4.18.0-beta.1 records and Misskey does not (tests/cherrypick/gen-fixture.sh
// writes the database they come from).
//
// **本家と同じ名前の記録は含めない。** Misskey から来た DB も、Elythia が流した
// 後の DB も持っているので、CherryPick の印にならない。
var cherryPickMigrations = []string{
	"disableRightClick1629387925000",
	"increaseSmtpCharLimit1654589815249",
	"multipleTranslationServices1660183643857",
	"Event1681429921400",
	"Event1681673280586",
	"Event1681675881633",
	"tweakVarcharLength21684231006000",
	"MultipleTranslationServices1685378242713",
	"AbuseReportResolver1686908762393",
	"ObjectStorageRemoteSetting1689580926821",
	"NotificationEmailsForAbuseReport1691120548582",
	"NoteEditHistory1696044626209",
	"NoteUpdateAtHistory1696318192428",
	"AddMetaOptions1696402675000",
	"AddMetaOptions1696417300000",
	"PollVotePoll1696604572677",
	"DeleteCreatedAt1697737204579",
	"RemoteAvaterDecoration1699432324194",
	"ScheduleNote1699437894737",
	"BubbleInstances1701647674000",
	"NoteUpdatedAt1704185628000",
	"ExternalWebsiteWarn1711008460816",
	"NoRecursiveDelete1711722198590",
	"AddDeleteAt1720161864577",
	"AddCustomSplash1723982389378",
	"NoteHistory1725344700039",
	"NoteHistory2_1725517326637",
	"RemoteObjectStorage1729518620697",
	"DropObjectStorageRemoteSetting1730162520000",
	"YouBlockedImageUrl1730364663000",
	"checkModeratorInactive1731385181000",
	"checkModeratorInactive21732593870000",
	"LibreTranslate1734793052000",
	"customRobotsTxt1738098171990",
	"setFederationAvatarShape1738776889000",
	"AddAvatarRawUrl1739785206096",
	"MigrateMessagingToChat1742927188000",
	"UserIpsAddDnsNames1746361416550",
	"AddDeliveryTargets1751828594933",
	"DropScheduleNote1760797772000",
	"AddAutoDeleteNoteSettings1761287521000",
	"RemoveNoteEditHistory1761583921158",
	"NoteHistoryAddCwPollEvent1761584610791",
	"chartFederationSoftware1761595707656",
	"AddChatMessageEmojis1762351977309",
	"AddUserCanChat1762447519068",
	"AddInstanceQuarantineLimited1762797644990",
	"ApprovalSignup1768893580518",
}

// CherryPickStatus says whether a database came from CherryPick and whether
// "elythia cherrypick" handles its version.
type CherryPickStatus int

const (
	// NotCherryPick means the database has none of CherryPick's own migration
	// records: it came from Misskey or Elythia.
	//
	// CherryPick の記録を共有する派生 (CherryPick から分かれた fork) の DB も
	// CherryPick として扱う。記録が同じなら、外れるものも同じだから。
	NotCherryPick CherryPickStatus = iota
	// CherryPickSupported means CherryPick 4.18.0-beta.1.
	CherryPickSupported
	// CherryPickUnsupported means another CherryPick version (or a fork that
	// shares some of its migrations).
	CherryPickUnsupported
)

// CherryPickPendingUser is a local user CherryPick kept waiting for approval.
type CherryPickPendingUser struct {
	ID           string
	Username     string
	SignupReason *string
}

// CherryPickReport is what InspectCherryPick found.
type CherryPickReport struct {
	Status CherryPickStatus
	// Reason explains CherryPickUnsupported.
	Reason string

	// Needs action before Elythia serves the database. Counted from the
	// state of the rows, so "elythia cherrypick convert" clears them by
	// changing that state.

	// PendingUsers applied for an account while meta.approvalRequiredForSignup
	// was on and wait for an administrator. Elythia does not read
	// user.approved, so they can sign in.
	PendingUsers []CherryPickPendingUser
	// UnapprovedMembers are local users with approved = false and no signup
	// reason while approval is on: members from before approval was turned
	// on, accounts an administrator created, and applicants who confirmed
	// their email only after approval was turned on. CherryPick refuses their
	// password sign-in; Elythia lets them sign in, so they need action like
	// PendingUsers (convert -unapproved-members).
	UnapprovedMembers int64
	// UnapprovedMemberNames are the usernames of the first
	// CherryPickListLimit users of UnapprovedMembers, in username order.
	UnapprovedMemberNames []string
	// AbuseReportResolvers are rules Elythia does not apply.
	AbuseReportResolvers int64
	// QuarantinedHosts lose the restriction (instance.quarantineLimited).
	QuarantinedHosts []string
	// ScheduledDeletesDue are local notes whose deleteAt has passed; Elythia
	// does not delete them. ScheduledDeletesLater have not reached it yet.
	ScheduledDeletesDue   int64
	ScheduledDeletesLater int64
	// RepositoryURL is meta.repositoryUrl when it still points at CherryPick.
	RepositoryURL string
	// FeedbackURL is meta.feedbackUrl when it is still CherryPick's issue
	// form. 利用者が Elythia の不具合を CherryPick へ報告しないように直す。
	FeedbackURL string

	// Kept in the database but not read by Elythia; reported only.
	NoteHistories int64
	Events        int64
	// RemoteScheduledDeletes are remote notes with deleteAt. 元のサーバーが Delete を
	// 配れば Elythia でも消えるので、要対応には数えない。
	RemoteScheduledDeletes int64
	// AutoDeleteUsers are local users with autoDeleteNotesAfterDays: CherryPick
	// deletes their old notes on a schedule, Elythia does not.
	AutoDeleteUsers int64
	// DraftScheduledDeletes are drafts with deleteAt: the note posted from it
	// is not deleted.
	DraftScheduledDeletes int64
	// UnapprovedStaff are the usernames of the local users with approved =
	// false that PendingUsers and UnapprovedMembers leave out because they are
	// the root user (meta.rootUserId) or hold an administrator or moderator
	// role. convert leaves them as they are: Elythia does not read approved,
	// so they keep signing in.
	UnapprovedStaff []string
	// ConditionalStaffRoles are the names of the conditional roles that make
	// users administrators or moderators. Their members are not left out of
	// PendingUsers and UnapprovedMembers (the formula is not evaluated here),
	// so the operator checks the listed users.
	ConditionalStaffRoles []string

	// Converted is set when "elythia cherrypick convert" has finished
	// (#4248). InspectCherryPickBeforeMigrate fills nothing else then.
	Converted bool
	// Migrated is set when Elythia has migrated the database, so the next
	// step is "elythia cherrypick convert".
	Migrated bool
}

// Unprocessed returns the number of kinds of items that need action. An
// unsupported version counts as one.
func (r CherryPickReport) Unprocessed() int {
	if r.Status == CherryPickUnsupported {
		return 1
	}
	n := 0
	for _, has := range []bool{
		len(r.PendingUsers) > 0,
		r.UnapprovedMembers > 0,
		r.AbuseReportResolvers > 0,
		len(r.QuarantinedHosts) > 0,
		r.ScheduledDeletesDue+r.ScheduledDeletesLater > 0,
		r.RepositoryURL != "" || r.FeedbackURL != "",
	} {
		if has {
			n++
		}
	}
	return n
}

// unapprovedUsersWhere selects the local users with approved = false that can
// still sign in.
//
// CherryPick は approved を既定値 false で足し、既存の行を埋めない。承認制が無効
// なら、approved が false でもサインインでき、その場で true に書き換える
// (SigninApiService)。なので、承認制が有効なときだけ数える (呼び出し側で見る)。
// システムアカウント (instance.actor など) も approved を持たずに作られるが、
// サインインしないので除く。凍結した・消した利用者もサインインできないので数えない
// (convert の「凍結」はこれで処理済みになる)。
//
// 初期ユーザー (meta.rootUserId) と、管理者・モデレーターのロールを割り当てた
// 利用者も除く (cherryPickStaff)。CherryPick の初めの管理者は管理画面の外で作られ、
// approved が false のまま残りうる。凍結・削除の対象に入ると、運営者が管理画面に
// 入れなくなる (凍結を解く手順そのものが使えない) か、管理者のアカウントが連合ごと
// 消える。
const unapprovedUsersWhere = ` WHERE host IS NULL AND approved = false AND "isSuspended" = false AND "isDeleted" = false
	AND NOT EXISTS (SELECT 1 FROM system_account s WHERE s."userId" = "user".id)
	AND NOT ` + cherryPickStaff

// cherryPickStaff matches a "user" row that is the root user or holds an
// unexpired administrator or moderator role assignment. 条件付きのロールは
// 式を SQL で評価できないので見ない (ConditionalStaffRoles で知らせる)。
const cherryPickStaff = `("user".id IN (SELECT "rootUserId" FROM meta WHERE "rootUserId" IS NOT NULL)
	OR EXISTS (SELECT 1 FROM role_assignment ra JOIN role ro ON ro.id = ra."roleId"
		WHERE ra."userId" = "user".id AND (ro."isAdministrator" OR ro."isModerator")
		AND (ra."expiresAt" IS NULL OR ra."expiresAt" > now())))`

// CherryPickListLimit bounds the usernames a report lists for a group of
// users; the count is always given.
const CherryPickListLimit = 20

// unapprovedMembersWhere selects the users of UnapprovedMembers: the users of
// unapprovedUsersWhere without a signup reason (all of them when the column is
// missing).
func unapprovedMembersWhere(withReason bool) string {
	if withReason {
		return unapprovedUsersWhere + ` AND "signupReason" IS NULL`
	}
	return unapprovedUsersWhere
}

// CherryPickStatusError is an error of InspectCherryPick that happened while
// telling the version, before anything was counted.
//
// migrate は、件数を数えられなかったときは -allow-cherrypick で進めるが、版を確かめ
// られなかったときは進めない。一度流すと版を見分けられなくなるため。
type CherryPickStatusError struct{ Err error }

func (e *CherryPickStatusError) Error() string { return e.Err.Error() }
func (e *CherryPickStatusError) Unwrap() error { return e.Err }

// InspectCherryPickBeforeMigrate is InspectCherryPick for "elythia migrate":
// once "elythia cherrypick convert" has finished, it only reports Converted.
// When Elythia has migrated the database and nothing needs action, it records
// the conversion itself (the only write it makes).
//
// convert は Elythia の migrate up の後に流す (Elythia の削除の処理を使うため)。
// その間に本体が起動しないように、Elythia が流した後でも、convert が済むまでは
// 数えて止める。済んだら数えない。migrate up は起動のたびに流れるので、大きな
// note の表を毎回読まないようにするため。
func InspectCherryPickBeforeMigrate(ctx context.Context, db *gorm.DB, now time.Time) (CherryPickReport, error) {
	converted, err := CherryPickConverted(ctx, db)
	if err != nil {
		return CherryPickReport{}, &CherryPickStatusError{Err: err}
	}
	if converted {
		return CherryPickReport{Converted: true}, nil
	}
	r, err := InspectCherryPick(ctx, db, now)
	if err != nil {
		return r, err
	}
	// Elythia が流した後で要対応が無ければ、ここで印を付ける。付けないと、convert を
	// 流さない限り、起動のたびに note の表を全部読み続ける。Elythia はこれらの行を
	// 新しく作らないので、後から要対応が増えることは無い。
	if r.Status == CherryPickSupported && r.Migrated && r.Unprocessed() == 0 {
		if err := MarkCherryPickConverted(ctx, db); err != nil {
			return r, err
		}
		r.Converted = true
	}
	return r, nil
}

// elythiaMigrated reports whether golang-migrate has recorded a version.
func elythiaMigrated(db *gorm.DB) (bool, error) {
	ok, err := hasTable(db, "schema_migrations")
	if err != nil || !ok {
		return false, err
	}
	var n int64
	if err := db.Raw(`SELECT count(*) FROM schema_migrations`).Scan(&n).Error; err != nil {
		return false, fmt.Errorf("cherrypick: read schema_migrations: %w", err)
	}
	return n > 0, nil
}

// InspectCherryPick tells whether the database of db came from CherryPick and
// counts what Elythia would silently drop. It only reads. now decides which
// scheduled deletes are due.
//
// 列や表が無ければ 0 件として扱う。後始末 (elythia cherrypick cleanup) で消した後も
// 同じ判定が通るようにするため。
func InspectCherryPick(ctx context.Context, db *gorm.DB, now time.Time) (CherryPickReport, error) {
	db = db.WithContext(ctx)
	var r CherryPickReport
	status, reason, err := cherryPickStatus(db)
	if err != nil {
		return r, &CherryPickStatusError{Err: err}
	}
	r.Status, r.Reason = status, reason
	if status != CherryPickSupported {
		return r, nil
	}
	if r.Converted, err = CherryPickConverted(ctx, db); err != nil {
		return r, err
	}
	if r.Migrated, err = elythiaMigrated(db); err != nil {
		return r, err
	}

	approval, err := approvalRequired(db)
	if err != nil {
		return r, err
	}
	if ok, err := hasColumns(db, "user", "approved"); err != nil {
		return r, err
	} else if ok && approval {
		// 承認待ち (申請した人) は、登録のときに理由が必須なので、理由を持つ。理由の無い
		// 行は、承認制にする前からの利用者か、管理者が作ったアカウント。例外は、承認制が
		// 無効の間にメールの確認待ちになり、有効にした後で確認した人で、理由を持たない
		// まま承認待ちになる。どれも CherryPick ではパスワードでサインインできず、
		// Elythia ではできるので、理由の無い行 (UnapprovedMembers) も要対応に数える。
		// 管理者が承認していない申請者が混ざりうるので、通さずに選ばせる。
		withReason, err := hasColumns(db, "user", "signupReason")
		if err != nil {
			return r, err
		}
		if withReason {
			if err := db.Raw(`SELECT id, username, "signupReason" AS signup_reason FROM "user"` + unapprovedUsersWhere +
				` AND "signupReason" IS NOT NULL ORDER BY id`).Scan(&r.PendingUsers).Error; err != nil {
				return r, fmt.Errorf("cherrypick: pending users: %w", err)
			}
		}
		if err := db.Raw(`SELECT count(*) FROM "user"` + unapprovedMembersWhere(withReason)).Scan(&r.UnapprovedMembers).Error; err != nil {
			return r, fmt.Errorf("cherrypick: unapproved members: %w", err)
		}
		// 件数だけでは、誰を凍結・削除するのか確かめられない。多いときは先頭だけ。
		if err := db.Raw(`SELECT username FROM "user"`+unapprovedMembersWhere(withReason)+
			` ORDER BY "usernameLower", id LIMIT ?`, CherryPickListLimit).Scan(&r.UnapprovedMemberNames).Error; err != nil {
			return r, fmt.Errorf("cherrypick: unapproved member names: %w", err)
		}
		if err := db.Raw(`SELECT username FROM "user" WHERE host IS NULL AND approved = false
			AND "isSuspended" = false AND "isDeleted" = false AND ` + cherryPickStaff +
			` ORDER BY "usernameLower", id`).Scan(&r.UnapprovedStaff).Error; err != nil {
			return r, fmt.Errorf("cherrypick: unapproved administrators and moderators: %w", err)
		}
		if err := db.Raw(`SELECT name FROM role WHERE target = 'conditional' AND ("isAdministrator" OR "isModerator")
			ORDER BY name`).Scan(&r.ConditionalStaffRoles).Error; err != nil {
			return r, fmt.Errorf("cherrypick: conditional administrator and moderator roles: %w", err)
		}
	}

	if ok, err := hasTable(db, "abuse_report_resolver"); err != nil {
		return r, err
	} else if ok {
		if err := db.Raw(`SELECT count(*) FROM abuse_report_resolver`).Scan(&r.AbuseReportResolvers).Error; err != nil {
			return r, fmt.Errorf("cherrypick: abuse report resolvers: %w", err)
		}
	}

	if ok, err := hasColumns(db, "instance", "quarantineLimited"); err != nil {
		return r, err
	} else if ok {
		if err := db.Raw(`SELECT host FROM instance WHERE "quarantineLimited" ORDER BY host`).
			Pluck("host", &r.QuarantinedHosts).Error; err != nil {
			return r, fmt.Errorf("cherrypick: quarantined instances: %w", err)
		}
	}

	if ok, err := hasColumns(db, "note", "deleteAt"); err != nil {
		return r, err
	} else if ok {
		var c struct{ Due, Later, Remote int64 }
		if err := db.Raw(`SELECT count(*) FILTER (WHERE "userHost" IS NULL AND "deleteAt" <= ?) AS due,
			count(*) FILTER (WHERE "userHost" IS NULL AND "deleteAt" > ?) AS later,
			count(*) FILTER (WHERE "userHost" IS NOT NULL) AS remote
			FROM note WHERE "deleteAt" IS NOT NULL`, now, now).Scan(&c).Error; err != nil {
			return r, fmt.Errorf("cherrypick: scheduled deletes: %w", err)
		}
		r.ScheduledDeletesDue, r.ScheduledDeletesLater, r.RemoteScheduledDeletes = c.Due, c.Later, c.Remote
	}

	var urls []string
	if err := db.Raw(`SELECT "repositoryUrl" FROM meta WHERE "repositoryUrl" IS NOT NULL`).
		Pluck("repositoryUrl", &urls).Error; err != nil {
		return r, fmt.Errorf("cherrypick: meta.repositoryUrl: %w", err)
	}
	for _, u := range urls {
		if isCherryPickRepository(u) {
			r.RepositoryURL = u
			break
		}
	}
	var feedback []string
	if err := db.Raw(`SELECT "feedbackUrl" FROM meta WHERE "feedbackUrl" IS NOT NULL`).
		Pluck("feedbackUrl", &feedback).Error; err != nil {
		return r, fmt.Errorf("cherrypick: meta.feedbackUrl: %w", err)
	}
	for _, u := range feedback {
		if isCherryPickFeedback(u) {
			r.FeedbackURL = u
			break
		}
	}

	for _, t := range []struct {
		table, column, query string
		dst                  *int64
	}{
		{"note_history", "id", `SELECT count(*) FROM note_history`, &r.NoteHistories},
		{"event", "noteId", `SELECT count(*) FROM event`, &r.Events},
		{"user", "autoDeleteNotesAfterDays",
			`SELECT count(*) FROM "user" WHERE host IS NULL AND "autoDeleteNotesAfterDays" IS NOT NULL`, &r.AutoDeleteUsers},
		{"note_draft", "deleteAt", `SELECT count(*) FROM note_draft WHERE "deleteAt" IS NOT NULL`, &r.DraftScheduledDeletes},
	} {
		if ok, err := hasColumns(db, t.table, t.column); err != nil {
			return r, err
		} else if ok {
			if err := db.Raw(t.query).Scan(t.dst).Error; err != nil {
				return r, fmt.Errorf("cherrypick: %s: %w", t.table, err)
			}
		}
	}
	return r, nil
}

// approvalRequired reports whether CherryPick's meta.approvalRequiredForSignup
// is on. Elythia の承認制 (#2554) も同じ列を使うが、承認待ちを user の行として
// 持たないので、ここでは CherryPick の意味だけを見る。
func approvalRequired(db *gorm.DB) (bool, error) {
	ok, err := hasColumns(db, "meta", "approvalRequiredForSignup")
	if err != nil || !ok {
		return false, err
	}
	var on bool
	if err := db.Raw(`SELECT coalesce(bool_or("approvalRequiredForSignup"), false) FROM meta`).Scan(&on).Error; err != nil {
		return false, fmt.Errorf("cherrypick: meta.approvalRequiredForSignup: %w", err)
	}
	return on, nil
}

// cherryPickStatus reads the TypeORM migrations table.
//
// Elythia がまだ流していない DB (schema_migrations が無い) では、記録の最も新しい
// ものが 4.18.0-beta.1 の最後の migration と一致することも見る。Elythia は流すときに
// 本家の記録を足すので、流した後は新しい版の CherryPick と見分けられない。その分は、
// 最初の migrate up がここで止めている (対応していない版は -allow-cherrypick でも
// 通さない)。
func cherryPickStatus(db *gorm.DB) (CherryPickStatus, string, error) {
	ok, err := hasColumns(db, "migrations", "name", "timestamp")
	if err != nil || !ok {
		return NotCherryPick, "", err
	}
	var found int64
	if err := db.Raw(`SELECT count(DISTINCT name) FROM migrations WHERE name IN ?`, cherryPickMigrations).
		Scan(&found).Error; err != nil {
		return NotCherryPick, "", fmt.Errorf("cherrypick: read migrations: %w", err)
	}
	if found == 0 {
		return NotCherryPick, "", nil
	}
	if int(found) < len(cherryPickMigrations) {
		return CherryPickUnsupported, fmt.Sprintf(
			"the database has %d of the %d migrations of CherryPick %s; it is another CherryPick version "+
				"(or a fork sharing some of its migrations). Upgrade CherryPick to %s first",
			found, len(cherryPickMigrations), CherryPickVersion, CherryPickVersion), nil
	}
	touched, err := elythiaMigrated(db)
	if err != nil || touched {
		return CherryPickSupported, "", err
	}
	var latest int64
	if err := db.Raw(`SELECT coalesce(max("timestamp"), 0) FROM migrations`).Scan(&latest).Error; err != nil {
		return NotCherryPick, "", fmt.Errorf("cherrypick: read migrations: %w", err)
	}
	if latest != cherryPickLatestMigration {
		return CherryPickUnsupported, fmt.Sprintf(
			"the database has migrations newer than CherryPick %s; only %s is supported",
			CherryPickVersion, CherryPickVersion), nil
	}
	return CherryPickSupported, "", nil
}

// isCherryPickRepository reports whether u is the repository CherryPick sets
// as meta.repositoryUrl.
func isCherryPickRepository(u string) bool {
	path, ok := githubPath(u)
	return ok && strings.TrimSuffix(path, ".git") == "/kokonect-link/cherrypick"
}

// isCherryPickFeedback reports whether u is the issue form CherryPick sets as
// meta.feedbackUrl.
func isCherryPickFeedback(u string) bool {
	path, ok := githubPath(u)
	return ok && path == "/kokonect-link/cherrypick/issues/new"
}

// githubPath returns the lower-cased path of a github.com URL without a
// trailing slash.
func githubPath(u string) (string, bool) {
	p, err := url.Parse(strings.TrimSpace(u))
	if err != nil || !strings.EqualFold(p.Host, "github.com") {
		return "", false
	}
	return strings.TrimSuffix(strings.ToLower(p.Path), "/"), true
}

// hasTable reports whether the current schema has table.
func hasTable(db *gorm.DB, table string) (bool, error) {
	var n int64
	if err := db.Raw(`SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name = ?`, table).Scan(&n).Error; err != nil {
		return false, fmt.Errorf("cherrypick: look up table %s: %w", table, err)
	}
	return n > 0, nil
}

// hasColumns reports whether table in the current schema has every column.
func hasColumns(db *gorm.DB, table string, columns ...string) (bool, error) {
	var n int64
	if err := db.Raw(`SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = ? AND column_name IN ?`, table, columns).
		Scan(&n).Error; err != nil {
		return false, fmt.Errorf("cherrypick: look up columns of %s: %w", table, err)
	}
	return int(n) == len(columns), nil
}

// CherryPickInspectTimeout bounds one inspection. 予約削除を数えるのに note の表を
// 全部読む (deleteAt に索引が無い) ので、大きな DB でも終わる長さにする。
const CherryPickInspectTimeout = 10 * time.Minute

// CherryPickInspectFunc is InspectCherryPick or InspectCherryPickBeforeMigrate.
type CherryPickInspectFunc func(context.Context, *gorm.DB, time.Time) (CherryPickReport, error)

// InspectCherryPickDSN connects to dsn and runs inspect within
// CherryPickInspectTimeout.
//
// "elythia cherrypick check" と "elythia migrate" が共有する。テストは search_path を
// 付けた接続文字列を渡す (設定からは search_path を渡せない)。
func InspectCherryPickDSN(dsn string, now time.Time, inspect CherryPickInspectFunc) (CherryPickReport, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		// 繋げなければ版も確かめられないので、migrate は -allow-cherrypick でも止める。
		return CherryPickReport{}, &CherryPickStatusError{Err: fmt.Errorf("open database: %w", err)}
	}
	if sqlDB, err := db.DB(); err == nil {
		defer sqlDB.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), CherryPickInspectTimeout)
	defer cancel()
	return inspect(ctx, db, now)
}
