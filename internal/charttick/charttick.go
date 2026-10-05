// Package charttick provides TickFunc closures that re-derive absolute
// chart counters from the live PostgreSQL state. They are bound to
// chart engines at construction so the periodic chart cron (#151) can
// correct event-driven drift.
package charttick

import (
	"context"

	"github.com/elythia-network/elythia/internal/core/chart"
	"gorm.io/gorm"
)

// localRemoteCount は host 列の有無で local/remote を 1 クエリで集計するための
// 共通ヘルパ。Users と Notes でロジックが同じなので集約してテスト面を縮める。
func localRemoteCount(ctx context.Context, db *gorm.DB, table, hostCol string) (local, remote int64, err error) {
	row := db.WithContext(ctx).Raw(
		`SELECT COUNT(*) FILTER (WHERE "` + hostCol + `" IS NULL),
		        COUNT(*) FILTER (WHERE "` + hostCol + `" IS NOT NULL)
		   FROM "` + table + `"`,
	).Row()
	err = row.Scan(&local, &remote)
	return local, remote, err
}

// Users returns a TickFunc that, on a major tick, recomputes the
// instance-wide local / remote user totals against the live DB. Mirrors
// upstream UsersChart.tickMajor; tickMinor is empty (returns nil).
func Users(db *gorm.DB) chart.TickFunc {
	return func(ctx context.Context, _ string, major bool) (map[string]int64, error) {
		if !major {
			return nil, nil
		}
		local, remote, err := localRemoteCount(ctx, db, "user", "host")
		if err != nil {
			return nil, err
		}
		return map[string]int64{
			"local.total":  local,
			"remote.total": remote,
		}, nil
	}
}

// Notes returns a TickFunc that, on a major tick, recomputes the
// instance-wide local / remote note totals. Mirrors upstream
// NotesChart.tickMajor; tickMinor is empty.
func Notes(db *gorm.DB) chart.TickFunc {
	return func(ctx context.Context, _ string, major bool) (map[string]int64, error) {
		if !major {
			return nil, nil
		}
		local, remote, err := localRemoteCount(ctx, db, "note", "userHost")
		if err != nil {
			return nil, err
		}
		return map[string]int64{
			"local.total":  local,
			"remote.total": remote,
		}, nil
	}
}

// Federation returns a TickFunc that, on a minor tick, recomputes the
// federation chart's instance counters. Mirrors upstream
// FederationChart.tickMinor; tickMajor is empty.
//
// 集計対象:
//   - sub: 我々がフォローしている対象 host 数
//   - pub: 我々をフォローしている remote host 数
//   - pubsub: 双方向 (sub と pub の積集合)
//   - subActive: 上記 sub のうち suspend されていない host 数
//   - pubActive: 上記 pub のうち suspend されていない host 数
//
// blockedHosts のフィルタは Misskey 本家側で適用されるが、本実装では Phase 1 として
// 全 host を対象とする。将来 meta.blockedHosts を渡せる構造に拡張予定。
func Federation(db *gorm.DB) chart.TickFunc {
	return func(ctx context.Context, _ string, major bool) (map[string]int64, error) {
		if major {
			return nil, nil
		}
		count := func(query string) (int64, error) {
			var n int64
			row := db.WithContext(ctx).Raw(query).Row()
			if err := row.Scan(&n); err != nil {
				return 0, err
			}
			return n, nil
		}
		sub, err := count(`SELECT COUNT(DISTINCT "followeeHost") FROM "following" WHERE "followeeHost" IS NOT NULL`)
		if err != nil {
			return nil, err
		}
		pub, err := count(`SELECT COUNT(DISTINCT "followerHost") FROM "following" WHERE "followerHost" IS NOT NULL`)
		if err != nil {
			return nil, err
		}
		pubsub, err := count(`
			SELECT COUNT(DISTINCT host) FROM (
				SELECT "followeeHost" AS host FROM "following" WHERE "followeeHost" IS NOT NULL
				INTERSECT
				SELECT "followerHost" FROM "following" WHERE "followerHost" IS NOT NULL
			) AS pubsub_hosts
		`)
		if err != nil {
			return nil, err
		}
		subActive, err := count(`
			SELECT COUNT(DISTINCT f."followeeHost") FROM "following" f
			LEFT JOIN "instance" i ON i.host = f."followeeHost"
			WHERE f."followeeHost" IS NOT NULL
			  AND (i."suspensionState" IS NULL OR i."suspensionState" = 'none')
		`)
		if err != nil {
			return nil, err
		}
		pubActive, err := count(`
			SELECT COUNT(DISTINCT f."followerHost") FROM "following" f
			LEFT JOIN "instance" i ON i.host = f."followerHost"
			WHERE f."followerHost" IS NOT NULL
			  AND (i."suspensionState" IS NULL OR i."suspensionState" = 'none')
		`)
		if err != nil {
			return nil, err
		}
		return map[string]int64{
			"sub":       sub,
			"pub":       pub,
			"pubsub":    pubsub,
			"subActive": subActive,
			"pubActive": pubActive,
		}, nil
	}
}
