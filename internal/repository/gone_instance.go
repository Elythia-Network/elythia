package repository

import (
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/elythia-network/elythia/internal/model"
)

// GoneInstanceSummary is one goneSuspended host with the relations left
// between it and local users (#3067).
type GoneInstanceSummary struct {
	Host string
	// SuspendedAt は goneSuspended になった時刻。TS が立てた停止など、記録が
	// 無いものは nil。
	SuspendedAt *time.Time
	// Followers は相手のユーザー → ローカルのユーザーのフォロー。
	Followers int64
	// Following はローカルのユーザー → 相手のユーザーのフォロー。
	Following int64
	// FollowRequests は両方向のフォローリクエスト。
	FollowRequests int64
}

// GoneInstanceRepository records when instances became goneSuspended and
// lists the relations left with them (#3067).
//
// FollowingRepository の interface には足さない。手書きの test fake が複数
// パッケージに散っていて、この機能のためだけに全部へ実装を足すことになる。
type GoneInstanceRepository struct {
	db *gorm.DB
}

// NewGoneInstanceRepository constructs a GoneInstanceRepository.
func NewGoneInstanceRepository(db *gorm.DB) *GoneInstanceRepository {
	return &GoneInstanceRepository{db: db}
}

// RecordGone stores when host became goneSuspended, overwriting an older
// record (戻した後に再び消えたときは新しい時刻にする)。
func (r *GoneInstanceRepository) RecordGone(host string, at time.Time) error {
	if host == "" || !storable(host) {
		return nil
	}
	row := model.InstanceGoneSuspension{Host: host, SuspendedAt: at}
	return r.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "host"}},
		DoUpdates: clause.AssignmentColumns([]string{"suspendedAt"}),
	}).Create(&row).Error
}

// ListGone returns every host currently goneSuspended with the relations
// left between it and local users. 停止した時刻の古い順 (記録が無いものが先)。
//
// following にホストの index が無いので、ホストごとに数えず 1 回の集計で
// まとめて数える。**片側がローカルの関係だけを数える** — 片付けの対象は
// ローカルの利用者から見える関係で、リモート同士の行は触らない。
func (r *GoneInstanceRepository) ListGone() ([]GoneInstanceSummary, error) {
	type row struct {
		Host           string
		SuspendedAt    *time.Time `gorm:"column:suspendedAt"`
		Followers      int64
		Following      int64
		FollowRequests int64 `gorm:"column:follow_requests"`
	}
	var rows []row
	err := r.db.Raw(`
WITH gone AS (
  SELECT i.host, g."suspendedAt"
  FROM instance i
  LEFT JOIN instance_gone_suspension g ON g.host = i.host
  WHERE i."suspensionState" = ?
),
fw AS (
  SELECT "followerHost" AS host, count(*) AS n FROM following
  WHERE "followeeHost" IS NULL AND "followerHost" IN (SELECT host FROM gone)
  GROUP BY 1
),
fe AS (
  SELECT "followeeHost" AS host, count(*) AS n FROM following
  WHERE "followerHost" IS NULL AND "followeeHost" IN (SELECT host FROM gone)
  GROUP BY 1
),
fr AS (
  SELECT h AS host, count(*) AS n FROM (
    SELECT "followerHost" AS h FROM follow_request
    WHERE "followeeHost" IS NULL AND "followerHost" IN (SELECT host FROM gone)
    UNION ALL
    SELECT "followeeHost" AS h FROM follow_request
    WHERE "followerHost" IS NULL AND "followeeHost" IN (SELECT host FROM gone)
  ) x GROUP BY 1
)
SELECT gone.host, gone."suspendedAt",
  coalesce(fw.n, 0) AS followers, coalesce(fe.n, 0) AS following, coalesce(fr.n, 0) AS follow_requests
FROM gone
LEFT JOIN fw ON fw.host = gone.host
LEFT JOIN fe ON fe.host = gone.host
LEFT JOIN fr ON fr.host = gone.host
ORDER BY gone."suspendedAt" ASC NULLS FIRST, gone.host`, string(model.SuspensionStateGoneSuspended)).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list gone instances: %w", err)
	}
	out := make([]GoneInstanceSummary, len(rows))
	for i, x := range rows {
		out[i] = GoneInstanceSummary{Host: x.Host, SuspendedAt: x.SuspendedAt, Followers: x.Followers, Following: x.Following, FollowRequests: x.FollowRequests}
	}
	return out, nil
}

// ListRelations returns up to limit follow relations between host and local
// users, in both directions.
func (r *GoneInstanceRepository) ListRelations(host string, limit int) ([]*model.Following, error) {
	if host == "" || !storable(host) {
		return nil, nil
	}
	var rows []*model.Following
	err := r.db.
		Where(`("followerHost" = ? AND "followeeHost" IS NULL) OR ("followeeHost" = ? AND "followerHost" IS NULL)`, host, host).
		Order("id").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list relations with %s: %w", host, err)
	}
	return rows, nil
}

// CountRelations counts the follow relations between host and local users.
func (r *GoneInstanceRepository) CountRelations(host string) (int64, error) {
	if host == "" || !storable(host) {
		return 0, nil
	}
	var n int64
	err := r.db.Model(&model.Following{}).
		Where(`("followerHost" = ? AND "followeeHost" IS NULL) OR ("followeeHost" = ? AND "followerHost" IS NULL)`, host, host).
		Count(&n).Error
	if err != nil {
		return 0, fmt.Errorf("count relations with %s: %w", host, err)
	}
	return n, nil
}

// DeleteFollowRequests deletes the follow requests between host and local
// users, in both directions, and returns how many were deleted.
func (r *GoneInstanceRepository) DeleteFollowRequests(host string) (int64, error) {
	if host == "" || !storable(host) {
		return 0, nil
	}
	res := r.db.
		Where(`("followerHost" = ? AND "followeeHost" IS NULL) OR ("followeeHost" = ? AND "followerHost" IS NULL)`, host, host).
		Delete(&model.FollowRequest{})
	if res.Error != nil {
		return 0, fmt.Errorf("delete follow requests with %s: %w", host, res.Error)
	}
	return res.RowsAffected, nil
}
