package repository

import (
	"github.com/shiroha-a/mk/internal/model"
	"gorm.io/gorm"
)

// AbuseReportRepository provides data access for the `abuse_user_report` table.
type AbuseReportRepository interface {
	Create(r *model.AbuseUserReport) error
	FindByID(id string) (*model.AbuseUserReport, error)
	// List returns abuse reports filtered by state and reporter/target user
	// origin, paginated via cursor (sinceID / untilID). Ordering follows
	// paginationOrder, so a sinceID-only page comes back oldest-first.
	// reporterOrigin / targetUserOrigin accept "local" / "remote" /
	// "combined" (or empty = combined). origin filter は対応する Host 列の
	// IS NULL / IS NOT NULL で判定する (= upstream Misskey TS と同じ pattern)。
	List(resolved *bool, reporterOrigin, targetUserOrigin, sinceID, untilID string, limit int) ([]*model.AbuseUserReport, error)
	UpdateFields(id string, fields map[string]any) error
	// FindStatesByIDs returns the resolution state of the given reports,
	// keyed by report ID. Missing IDs are simply absent from the map.
	//
	// **通知の read 時に使う (#2868)。** FindByID は TargetUser / Reporter /
	// Assignee を Preload するので 1 件あたり 3-4 SELECT になるが、状態表示に
	// 要るのは 3 スカラーだけ。通知一覧はページあたり最大 100 件なので、
	// 1 件ずつ引くと 1 リクエストで数百 SELECT が直列に走る。
	FindStatesByIDs(ids []string) (map[string]model.AbuseReportState, error)
	// CountUnresolved returns the number of reports that are not resolved yet.
	//
	// **通知をまとめたときに後続の通報を見落とさないために使う (#3200)。**
	// まとめた通知が指す 1 件が対処済みでも、未対応が残っていることを出す。
	CountUnresolved() (int64, error)
	// HasOlderUnresolvedByPair reports whether an unresolved report from
	// reporterID against targetUserID exists with an id older than beforeID.
	//
	// **「自分より古い」に限る (#3200)。** 同じ組の通報が並行して入ると、
	// 「自分以外に在るか」ではお互いを見て両方が通知を見送り、誰にも届かない。
	// id は時刻順なので、最古の 1 件だけが通知する。
	HasOlderUnresolvedByPair(reporterID, targetUserID, beforeID string) (bool, error)
	// HasOlderUnresolvedFromHost reports whether an unresolved report whose
	// reporter lives on host exists with an id older than beforeID. 「古い」に
	// 限る理由は HasOlderUnresolvedByPair と同じ。
	HasOlderUnresolvedFromHost(host, beforeID string) (bool, error)
}

type abuseReportRepository struct {
	db *gorm.DB
}

func NewAbuseReportRepository(db *gorm.DB) AbuseReportRepository {
	return &abuseReportRepository{db: db}
}

func (r *abuseReportRepository) Create(report *model.AbuseUserReport) error {
	return r.db.Create(report).Error
}

func (r *abuseReportRepository) FindByID(id string) (*model.AbuseUserReport, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var report model.AbuseUserReport
	if err := r.db.Preload("TargetUser").Preload("Reporter").Preload("Assignee").
		Where("id = ?", id).First(&report).Error; err != nil {
		return nil, err
	}
	return &report, nil
}

func (r *abuseReportRepository) FindStatesByIDs(ids []string) (map[string]model.AbuseReportState, error) {
	ids = storableIDs(ids)
	out := make(map[string]model.AbuseReportState, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	var rows []struct {
		ID         string
		Resolved   bool
		ResolvedAs *string
		AssigneeID *string
	}
	// **Preload しない。** 状態表示に要るのは 3 スカラーだけで、FindByID が
	// 引く 3 つの user 行は 1 つも使わない。
	if err := r.db.Model(&model.AbuseUserReport{}).
		Select(`id`, `"resolved"`, `"resolvedAs"`, `"assigneeId"`).
		Where("id IN ?", ids).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.ID] = model.AbuseReportState{
			Resolved:   row.Resolved,
			ResolvedAs: row.ResolvedAs,
			AssigneeID: row.AssigneeID,
		}
	}
	return out, nil
}

func (r *abuseReportRepository) CountUnresolved() (int64, error) {
	var n int64
	if err := r.db.Model(&model.AbuseUserReport{}).Where(`"resolved" = ?`, false).Count(&n).Error; err != nil {
		return 0, err
	}
	return n, nil
}

func (r *abuseReportRepository) HasOlderUnresolvedByPair(reporterID, targetUserID, beforeID string) (bool, error) {
	if !storable(reporterID) || !storable(targetUserID) || !storable(beforeID) {
		return false, nil
	}
	// **Count にしない。** `SELECT count(*) ... LIMIT 1` は LIMIT が集計を
	// 打ち切らないので、連打中は通報のたびに該当行を全部数える。1 行見つかれば
	// 足りる。
	var ids []string
	if err := r.db.Model(&model.AbuseUserReport{}).
		Where(`"reporterId" = ? AND "targetUserId" = ? AND "resolved" = ? AND id < ?`, reporterID, targetUserID, false, beforeID).
		Limit(1).Pluck("id", &ids).Error; err != nil {
		return false, err
	}
	return len(ids) > 0, nil
}

func (r *abuseReportRepository) HasOlderUnresolvedFromHost(host, beforeID string) (bool, error) {
	if !storable(host) || !storable(beforeID) {
		return false, nil
	}
	// **Count にしない。** `SELECT count(*) ... LIMIT 1` は LIMIT が集計を
	// 打ち切らないので、連打中は通報のたびに該当行を全部数える。1 行見つかれば
	// 足りる。
	var ids []string
	if err := r.db.Model(&model.AbuseUserReport{}).
		Where(`"reporterHost" = ? AND "resolved" = ? AND id < ?`, host, false, beforeID).
		Limit(1).Pluck("id", &ids).Error; err != nil {
		return false, err
	}
	return len(ids) > 0, nil
}

func (r *abuseReportRepository) List(resolved *bool, reporterOrigin, targetUserOrigin, sinceID, untilID string, limit int) ([]*model.AbuseUserReport, error) {
	q := r.db.Preload("TargetUser").Preload("Reporter").Preload("Assignee")
	if resolved != nil {
		q = q.Where("resolved = ?", *resolved)
	}
	// origin filter: "local" → Host が NULL (= local user)、"remote" → Host が
	// NOT NULL (= remote user)、"combined" / 空文字列は条件追加なし (= all)。
	// upstream Misskey TS と同じ semantics。reporterOrigin と targetUserOrigin
	// は独立に AND される。
	switch reporterOrigin {
	case "local":
		q = q.Where(`"reporterHost" IS NULL`)
	case "remote":
		q = q.Where(`"reporterHost" IS NOT NULL`)
	}
	switch targetUserOrigin {
	case "local":
		q = q.Where(`"targetUserHost" IS NULL`)
	case "remote":
		q = q.Where(`"targetUserHost" IS NOT NULL`)
	}
	// cursor pagination (keyset)。untilID は「より古い id」、sinceID は
	// 「より新しい id」を取り出す方向。frontend MkPagination の untilId 経路が
	// メインの fix 対象 (#1114)。
	//
	// 並び順は paginationOrder に従う (#2713)。upstream
	// admin/abuse-user-reports.ts は makePaginationQuery を使い、その後
	// order を上書きしないので、**sinceId 単独では ASC** になる。
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	q = q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit)
	var reports []*model.AbuseUserReport
	if err := q.Find(&reports).Error; err != nil {
		return nil, err
	}
	return reports, nil
}

func (r *abuseReportRepository) UpdateFields(id string, fields map[string]any) error {
	return r.db.Model(&model.AbuseUserReport{}).Where("id = ?", id).Updates(fields).Error
}

// ModerationLogRepository provides data access for the `moderation_log` table.
type ModerationLogRepository interface {
	Create(log *model.ModerationLog) error
	// CreateMany batch-inserts logs in one SQL statement (GORM slice-insert
	// uses a single multi-row INSERT). Used by Service.LogMany when bulk
	// admin operations want to record N entries without spawning N
	// goroutines + N round-trips (#671).
	CreateMany(logs []*model.ModerationLog) error
	List(filter model.ModerationLogFilter) ([]*model.ModerationLog, error)
}

type moderationLogRepository struct {
	db *gorm.DB
}

func NewModerationLogRepository(db *gorm.DB) ModerationLogRepository {
	return &moderationLogRepository{db: db}
}

func (r *moderationLogRepository) Create(log *model.ModerationLog) error {
	return r.db.Create(log).Error
}

func (r *moderationLogRepository) CreateMany(logs []*model.ModerationLog) error {
	if len(logs) == 0 {
		return nil
	}
	return r.db.Create(&logs).Error
}

func (r *moderationLogRepository) List(filter model.ModerationLogFilter) ([]*model.ModerationLog, error) {
	// 列に入らない文字は保存された値に現れないので、一致しえない (#3025)。
	// **引く前に弾く** — LIKE のパターンに載せるとクエリごと落ちて 500 になる。
	if !storable(filter.Search) || !storable(filter.Type) || !storable(filter.UserID) {
		return nil, nil
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	// upstream show-moderation-logs.ts: type / userId 完全一致 + info::text ILIKE
	// search + id-cursor pagination (sinceId/untilId)。
	q := r.db.Preload("User")
	if filter.Type != "" {
		q = q.Where("type = ?", filter.Type)
	}
	if filter.UserID != "" {
		q = q.Where(`"userId" = ?`, filter.UserID)
	}
	if filter.Search != "" {
		q = q.Where("info::text ILIKE ?", "%"+escapeLike(filter.Search)+"%")
	}
	if filter.UntilID != "" {
		q = q.Where("id < ?", filter.UntilID)
	}
	if filter.SinceID != "" {
		q = q.Where("id > ?", filter.SinceID)
	}
	// upstream admin/show-moderation-logs.ts は makePaginationQuery を使う (#2713)。
	q = q.Order(paginationOrder(filter.SinceID, filter.UntilID, "id")).Limit(limit)
	var logs []*model.ModerationLog
	if err := q.Find(&logs).Error; err != nil {
		return nil, err
	}
	return logs, nil
}
