// Package retention runs the daily retention aggregation that backs
// /api/retention and the admin overview「定着率」heatmap (#421)。
//
// 処理は本家 AggregateRetentionProcessorService と同じ:
//
//  1. 今日 (UTC) 1 日に新規登録したローカルユーザー一覧を取得
//  2. 同じ dateKey で `retention_aggregation` 行が無ければ新規 INSERT
//     (重複は別 worker が同時走行した場合の自然なスキップ)
//  3. 直近 31 日分の retention_aggregation 行を読み出す
//  4. 今日アクティブだったローカルユーザーの ID 集合を求める
//  5. 各過去行の userIds と intersection を取り、`data[今日の dateKey]`
//     にその数を書き込んで Update
package retention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"gorm.io/datatypes"
)

// Default cohort window. Misskey TS uses ~30 days.
const cohortWindow = 31 * 24 * time.Hour

// dailyWindow は「今日」と判定する遡及時間。本家と同じ 24 時間。
const dailyWindow = 24 * time.Hour

// Service aggregates retention statistics into the retention_aggregation
// table. Stateless — safe to call Aggregate concurrently; the unique
// dateKey constraint handles double-runs.
type Service struct {
	userRepo      repository.UserRepository
	retentionRepo repository.RetentionAggregationRepository
	idGen         id.Generator
	clock         func() time.Time
}

// NewService constructs a Service.
func NewService(userRepo repository.UserRepository, retentionRepo repository.RetentionAggregationRepository, idGen id.Generator) *Service {
	return &Service{
		userRepo:      userRepo,
		retentionRepo: retentionRepo,
		idGen:         idGen,
		clock:         time.Now,
	}
}

// SetClock overrides the time source. Tests use this to make Aggregate
// deterministic.
func (s *Service) SetClock(clock func() time.Time) {
	if clock != nil {
		s.clock = clock
	}
}

// Aggregate runs one pass of the daily retention computation.
func (s *Service) Aggregate(ctx context.Context) error {
	now := s.clock()
	dateKey := formatDateKey(now)

	// 1. Today's new local user IDs (registered within the last 24 hours).
	cutoff := now.Add(-dailyWindow)
	// idGen.Generate(cutoff) は cutoff のミリ秒タイムスタンプ + nodeID +
	// counter サフィックスから合成 ID を作る。本物のユーザー ID とサフィックス
	// 比較が一致する保証は無いので、ちょうど cutoff ミリ秒に登録されたユーザー
	// が ±1 件ぶれる可能性がある。24h 集計では誤差は無視できるレベル。
	// 副作用として shared idGen の counter を 1 つ消費するが、retention は
	// 1 日数回しか走らないため uint32 wraparound には実用上到達しない。
	cursorID := s.idGen.Generate(cutoff)
	newIDs, err := s.userRepo.ListLocalUserIDsRegisteredAfter(cursorID)
	if err != nil {
		return err
	}
	// nil のままだと model.StringArray.Value が NULL を返し、`userIds` カラムに
	// SQL NULL が入る。entity 層 / API JSON で `null` が露出すると Misskey
	// 互換の `[]` 表現が崩れて frontend heatmap 描画が壊れるので、empty な
	// 場合は明示的に non-nil の空 slice にしておく。
	if newIDs == nil {
		newIDs = []string{}
	}

	// 2. Insert today's row. Duplicate dateKey -> already processed elsewhere.
	row := &model.RetentionAggregation{
		ID:         s.idGen.Generate(now),
		CreatedAt:  now,
		UpdatedAt:  now,
		DateKey:    dateKey,
		UserIDs:    model.StringArray(newIDs),
		UsersCount: len(newIDs),
		Data:       datatypes.JSON([]byte("{}")),
	}
	if err := s.retentionRepo.Insert(row); err != nil {
		if !errors.Is(err, repository.ErrDuplicateKey) {
			return err
		}
		// 重複は別 worker / 同日の startup-fire で既に処理済み。Insert 自体は
		// skip するが、past cohort の data[dateKey] 更新 (steps 3-5) は continue
		// する。同日に startup goroutine と再起動 / cron が複数回走った時、
		// 最後に走った Aggregate の active set で past row を最新化したい。
		// past 側 self-update は line 116 (past.DateKey == dateKey) で弾かれる
		// のでこのまま loop に進んでも安全。
		slog.Debug("retention: dateKey already processed, refreshing past cohorts", "dateKey", dateKey)
	}

	// 3-5. Refresh data[dateKey] on the last 31 days of cohort rows.
	pastRows, err := s.retentionRepo.ListSince(now.Add(-cohortWindow))
	if err != nil {
		return err
	}
	activeIDs, err := s.userRepo.ListLocalUserIDsActiveSince(cutoff)
	if err != nil {
		return err
	}
	activeSet := make(map[string]struct{}, len(activeIDs))
	for _, id := range activeIDs {
		activeSet[id] = struct{}{}
	}

	for _, past := range pastRows {
		// Today's row: the cohort itself is brand new and trivially "active",
		// but skip the self-update since data[dateKey] for today is implicit.
		if past.DateKey == dateKey {
			continue
		}
		retained := 0
		for _, uid := range past.UserIDs {
			if _, ok := activeSet[uid]; ok {
				retained++
			}
		}
		merged, err := mergeDataKey(past.Data, dateKey, retained)
		if err != nil {
			slog.Warn("retention: data merge failed", "dateKey", dateKey, "rowId", past.ID, "err", err)
			continue
		}
		if err := s.retentionRepo.Update(past.ID, now, merged); err != nil {
			slog.Warn("retention: row update failed", "rowId", past.ID, "err", err)
		}
	}
	return nil
}

// formatDateKey returns the upstream `<year>-<month>-<day>` (no zero
// padding) using UTC. Misskey TS の AggregateRetentionProcessorService
// で使う key 形式と一致させ、相互運用性を保つ。
func formatDateKey(t time.Time) string {
	y, m, d := t.UTC().Date()
	return fmt.Sprintf("%d-%d-%d", y, int(m), d)
}

// mergeDataKey reads the existing JSON object, sets `[key] = value`, and
// returns the new bytes. Empty / missing input is treated as `{}`.
func mergeDataKey(raw datatypes.JSON, key string, value int) (datatypes.JSON, error) {
	m := map[string]int{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
	}
	m[key] = value
	out, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return datatypes.JSON(out), nil
}
