// Package iplookuplog records who looked up which IP, and when (#3106、親 #3066).
//
// **`moderation_log` には入れない。** あちらは保持期間を持たず永久に残るのに、
// ここに書くのは照会に使った IP そのもので、IP とアカウントの対応と同じだけ機密性が
// ある。専用テーブルにして保持期間を切る。
//
// **結果そのものは残さない。** 残す件数は「何件返したか」だけで、候補のアカウントや
// 一致した IP は書かない — 書くと、この表が第 2 の「IP とアカウントの対応」になる。
package iplookuplog

import (
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// Retention is how long an audit record is kept.
//
// 90 日。**照会の記録は `user_ip` より長く持たない** — 引いた元の記録が消えた後まで
// 「誰がその IP を引いたか」だけが残っても、対応する事実を確かめる手段が無い。
// `iplog.Retention` と同じ値だが、**別の理由で同じ値**なので定数は分けてある
// (あちらは観測の保持、こちらは監査の保持)。
const Retention = 90 * 24 * time.Hour

// Entry is one lookup to record.
type Entry struct {
	// UserID は照会した人。
	UserID string
	Kind   string
	// IP は照会に使った正規形 (IP 起点のときだけ)。
	IP string
	// TargetUserID は対象の利用者 (利用者起点のときだけ)。
	TargetUserID string
	SinceDays    int
	ResultCount  int
}

// Service appends audit records.
type Service struct {
	repo  repository.IPLookupLogRepository
	idGen id.Generator
	now   func() time.Time
}

// NewService wires the recorder. どちらかが nil なら Record は黙って何もしない
// (部分配線のテスト向け)。**本番の未配線は起動時の critical wiring 検査が知らせる。**
func NewService(repo repository.IPLookupLogRepository, idGen id.Generator) *Service {
	return &Service{repo: repo, idGen: idGen, now: time.Now}
}

// Record appends one audit record.
//
// **失敗しても照会は成立させる。** 監査に書けなかったことを理由に照会を 500 にすると、
// 監査の障害が調査そのものを止める。代わりに Error で残す — 記録が落ちたことに
// 気付けないほうが問題なので、Warn ではなく Error にしてある。
func (s *Service) Record(e Entry) {
	if s == nil || s.repo == nil || s.idGen == nil {
		return
	}
	now := s.now()
	row := &model.IPLookupLog{
		ID:           s.idGen.Generate(now),
		UserID:       e.UserID,
		Kind:         e.Kind,
		IP:           e.IP,
		TargetUserID: e.TargetUserID,
		SinceDays:    e.SinceDays,
		ResultCount:  e.ResultCount,
		CreatedAt:    now,
	}
	if err := s.repo.Create(row); err != nil {
		slog.Error("ip lookup audit: record failed",
			"kind", e.Kind, "moderator", e.UserID, "error", err)
	}
}

// Wired reports whether the recorder can actually write. 起動時検査に使う。
func (s *Service) Wired() bool {
	return s != nil && s.repo != nil && s.idGen != nil
}
