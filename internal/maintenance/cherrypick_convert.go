package maintenance

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/pgarray"
)

// cherryPickConvertedMarker is the TypeORM migrations record "elythia
// cherrypick convert" adds once nothing needs action (#4248).
//
// 表を増やさずに印を残すため、CherryPick が使っていた migrations の表に 1 行足す。
// timestamp は 0 にして、版の判定 (最新の記録を見る) に掛からないようにする。
const cherryPickConvertedMarker = "ElythiaCherryPickConverted"

// Pending user actions of CherryPickPlan.
const (
	CherryPickSuspend = "suspend"
	CherryPickDelete  = "delete"
	CherryPickApprove = "approve"
)

// Quarantine actions of CherryPickPlan.
const (
	CherryPickBlock = "block"
	CherryPickLift  = "lift"
)

// CherryPickPlan is what ApplyCherryPickSQL changes. Deleting accounts and
// notes is not part of it: that goes through the server's delete paths.
type CherryPickPlan struct {
	// PendingUserIDs are suspended (PendingAction CherryPickSuspend) or
	// approved (CherryPickApprove). CherryPickDelete changes nothing here.
	PendingUserIDs []string
	PendingAction  string
	// QuarantinedHosts are added to meta.blockedHosts (CherryPickBlock) or
	// not (CherryPickLift); either way their quarantineLimited is cleared.
	QuarantinedHosts []string
	QuarantineAction string
	// KeepLaterDeletes clears deleteAt of the local notes not yet due.
	KeepLaterDeletes bool
	// DeleteResolvers deletes every abuse_report_resolver row. 呼び出し側が
	// ファイルへ書き出してから立てる。
	DeleteResolvers bool
	// RepositoryURL is the CherryPick meta.repositoryUrl to replace with
	// ElythiaRepositoryURL. Empty leaves it.
	RepositoryURL        string
	ElythiaRepositoryURL string
	// FeedbackURL is the CherryPick meta.feedbackUrl to replace with
	// ElythiaFeedbackURL. Empty leaves it.
	FeedbackURL        string
	ElythiaFeedbackURL string
	// Now decides which scheduled deletes are not yet due.
	Now time.Time
}

// CherryPickResolver is one abuse_report_resolver row, written out before it
// is deleted.
type CherryPickResolver struct {
	ID                   string     `json:"id"`
	Name                 string     `json:"name"`
	TargetUserPattern    *string    `json:"targetUserPattern"`
	ReporterPattern      *string    `json:"reporterPattern"`
	ReportContentPattern *string    `json:"reportContentPattern"`
	ExpiresAt            string     `json:"expiresAt"`
	ExpirationDate       *time.Time `json:"expirationDate"`
	Forward              bool       `json:"forward"`
}

// CherryPickResolvers returns the abuse report resolvers in id order.
func CherryPickResolvers(ctx context.Context, db *gorm.DB) ([]CherryPickResolver, error) {
	db = db.WithContext(ctx)
	if ok, err := hasTable(db, "abuse_report_resolver"); err != nil || !ok {
		return nil, err
	}
	var rows []CherryPickResolver
	if err := db.Raw(`SELECT id, name, "targetUserPattern" AS target_user_pattern, "reporterPattern" AS reporter_pattern,
		"reportContentPattern" AS report_content_pattern, "expiresAt"::text AS expires_at,
		"expirationDate" AS expiration_date, forward
		FROM abuse_report_resolver ORDER BY id`).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("cherrypick: read abuse report resolvers: %w", err)
	}
	return rows, nil
}

// CherryPickScheduledDeletes returns the ids of the local notes with deleteAt,
// split into those due at now and those not yet due.
func CherryPickScheduledDeletes(ctx context.Context, db *gorm.DB, now time.Time) (due, later []string, err error) {
	db = db.WithContext(ctx)
	if ok, err := hasColumns(db, "note", "deleteAt"); err != nil || !ok {
		return nil, nil, err
	}
	if err := db.Raw(`SELECT id FROM note WHERE "userHost" IS NULL AND "deleteAt" <= ? ORDER BY id`, now).
		Scan(&due).Error; err != nil {
		return nil, nil, fmt.Errorf("cherrypick: read due scheduled deletes: %w", err)
	}
	if err := db.Raw(`SELECT id FROM note WHERE "userHost" IS NULL AND "deleteAt" > ? ORDER BY id`, now).
		Scan(&later).Error; err != nil {
		return nil, nil, fmt.Errorf("cherrypick: read scheduled deletes: %w", err)
	}
	return due, later, nil
}

// ApplyCherryPickSQL makes the changes of plan in one transaction.
func ApplyCherryPickSQL(ctx context.Context, db *gorm.DB, plan CherryPickPlan) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(plan.PendingUserIDs) > 0 {
			switch plan.PendingAction {
			case CherryPickSuspend:
				if err := tx.Exec(`UPDATE "user" SET "isSuspended" = true WHERE id IN ?`, plan.PendingUserIDs).Error; err != nil {
					return fmt.Errorf("cherrypick: suspend pending users: %w", err)
				}
			case CherryPickApprove:
				if err := tx.Exec(`UPDATE "user" SET approved = true WHERE id IN ?`, plan.PendingUserIDs).Error; err != nil {
					return fmt.Errorf("cherrypick: approve pending users: %w", err)
				}
			case CherryPickDelete:
			default:
				return fmt.Errorf("cherrypick: unknown pending user action %q", plan.PendingAction)
			}
		}

		if len(plan.QuarantinedHosts) > 0 {
			switch plan.QuarantineAction {
			case CherryPickBlock:
				if err := addBlockedHosts(tx, plan.QuarantinedHosts); err != nil {
					return err
				}
			case CherryPickLift:
			default:
				return fmt.Errorf("cherrypick: unknown quarantine action %q", plan.QuarantineAction)
			}
			if err := tx.Exec(`UPDATE instance SET "quarantineLimited" = false WHERE host IN ?`, plan.QuarantinedHosts).Error; err != nil {
				return fmt.Errorf("cherrypick: clear quarantine: %w", err)
			}
		}

		if plan.KeepLaterDeletes {
			if err := tx.Exec(`UPDATE note SET "deleteAt" = NULL WHERE "userHost" IS NULL AND "deleteAt" > ?`, plan.Now).Error; err != nil {
				return fmt.Errorf("cherrypick: keep scheduled deletes: %w", err)
			}
		}

		if plan.DeleteResolvers {
			if err := tx.Exec(`DELETE FROM abuse_report_resolver`).Error; err != nil {
				return fmt.Errorf("cherrypick: delete abuse report resolvers: %w", err)
			}
		}

		if plan.RepositoryURL != "" {
			if err := tx.Exec(`UPDATE meta SET "repositoryUrl" = ? WHERE "repositoryUrl" = ?`,
				plan.ElythiaRepositoryURL, plan.RepositoryURL).Error; err != nil {
				return fmt.Errorf("cherrypick: replace meta.repositoryUrl: %w", err)
			}
		}
		if plan.FeedbackURL != "" {
			if err := tx.Exec(`UPDATE meta SET "feedbackUrl" = ? WHERE "feedbackUrl" = ?`,
				plan.ElythiaFeedbackURL, plan.FeedbackURL).Error; err != nil {
				return fmt.Errorf("cherrypick: replace meta.feedbackUrl: %w", err)
			}
		}
		return nil
	})
}

// addBlockedHosts adds hosts to meta.blockedHosts, keeping the hosts already
// there and their order. A host already there in another letter case is not
// added again.
func addBlockedHosts(tx *gorm.DB, hosts []string) error {
	res := tx.Exec(`UPDATE meta SET "blockedHosts" = "blockedHosts" || ARRAY(
		SELECT h FROM unnest(?::varchar[]) WITH ORDINALITY AS t(h, i)
		WHERE NOT EXISTS (SELECT 1 FROM unnest(meta."blockedHosts") b WHERE lower(b) = lower(h))
		ORDER BY i)::varchar[]`, pgarray.StringArray(hosts))
	if res.Error != nil {
		return fmt.Errorf("cherrypick: add to meta.blockedHosts: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("cherrypick: no meta row to add blocked hosts to")
	}
	return nil
}

// CherryPickConverted reports whether "elythia cherrypick convert" has
// finished on the database.
func CherryPickConverted(ctx context.Context, db *gorm.DB) (bool, error) {
	db = db.WithContext(ctx)
	if ok, err := hasColumns(db, "migrations", "name", "timestamp"); err != nil || !ok {
		return false, err
	}
	var n int64
	if err := db.Raw(`SELECT count(*) FROM migrations WHERE name = ?`, cherryPickConvertedMarker).Scan(&n).Error; err != nil {
		return false, fmt.Errorf("cherrypick: read the converted marker: %w", err)
	}
	return n > 0, nil
}

// MarkCherryPickConverted records that "elythia cherrypick convert" has
// finished. Marking twice adds one record.
func MarkCherryPickConverted(ctx context.Context, db *gorm.DB) error {
	if err := db.WithContext(ctx).Exec(`INSERT INTO migrations ("timestamp", name)
		SELECT 0, ? WHERE NOT EXISTS (SELECT 1 FROM migrations WHERE name = ?)`,
		cherryPickConvertedMarker, cherryPickConvertedMarker).Error; err != nil {
		return fmt.Errorf("cherrypick: write the converted marker: %w", err)
	}
	return nil
}

// ElythiaMigrated reports whether Elythia has migrated the database
// (schema_migrations has a row).
func ElythiaMigrated(ctx context.Context, db *gorm.DB) (bool, error) {
	return elythiaMigrated(db.WithContext(ctx))
}

// CherryPickLeftover is a column or table of CherryPick that "elythia
// cherrypick cleanup" drops.
type CherryPickLeftover struct {
	// Table is the table; Column is empty when the whole table is dropped.
	Table, Column string
}

func (l CherryPickLeftover) String() string {
	if l.Column == "" {
		return l.Table
	}
	return l.Table + "." + l.Column
}

// cherryPickLeftovers are what cleanup drops (#4249): the columns and the
// table whose meaning convert replaced. 編集の履歴やイベントなど、読まれなく
// なるだけのデータは消さない (運営者が後で取り出せるように残す)。
var cherryPickLeftovers = []CherryPickLeftover{
	{Table: "user", Column: "approved"},
	{Table: "instance", Column: "quarantineLimited"},
	{Table: "abuse_report_resolver"},
	{Table: "note", Column: "deleteAt"},
}

// CherryPickLeftovers returns the leftovers of cherryPickLeftovers still in
// the database.
func CherryPickLeftovers(ctx context.Context, db *gorm.DB) ([]CherryPickLeftover, error) {
	db = db.WithContext(ctx)
	var left []CherryPickLeftover
	for _, l := range cherryPickLeftovers {
		var ok bool
		var err error
		if l.Column == "" {
			ok, err = hasTable(db, l.Table)
		} else {
			ok, err = hasColumns(db, l.Table, l.Column)
		}
		if err != nil {
			return nil, err
		}
		if ok {
			left = append(left, l)
		}
	}
	return left, nil
}

// DropCherryPickLeftovers drops the leftovers in one transaction.
//
// 文は固定で、名前を組み立てない (IF EXISTS なので、途中まで消えた DB で
// 流し直しても通る)。abuse_report_resolver の expiresAt の enum 型も表と一緒に消す。
func DropCherryPickLeftovers(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, sql := range []string{
			// 列を消す ALTER は ACCESS EXCLUSIVE を取る。動いている本体の読み取りと
			// 取り合ったときに、後ろの全クエリを詰まらせ続けないよう、待つのは30秒まで。
			`SET LOCAL lock_timeout = '30s'`,
			`ALTER TABLE "user" DROP COLUMN IF EXISTS approved`,
			`ALTER TABLE instance DROP COLUMN IF EXISTS "quarantineLimited"`,
			`DROP TABLE IF EXISTS abuse_report_resolver`,
			`DROP TYPE IF EXISTS abuse_report_resolver_expiresat_enum`,
			`ALTER TABLE note DROP COLUMN IF EXISTS "deleteAt"`,
		} {
			if err := tx.Exec(sql).Error; err != nil {
				return fmt.Errorf("cherrypick: cleanup: %w", err)
			}
		}
		return nil
	})
}
