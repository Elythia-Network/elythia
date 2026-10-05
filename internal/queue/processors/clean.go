package processors

import (
	"context"
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/core/bubbleversus"
	"github.com/elythia-network/elythia/internal/core/iplog"
	"github.com/elythia-network/elythia/internal/core/iplookuplog"
	"github.com/elythia-network/elythia/internal/queue/driver"
)

// Narrow interfaces the daily clean job depends on. Each is satisfied by the
// corresponding repository; a nil dependency makes that sub-task a no-op so
// callers can wire a partial subset.
type (
	// UserIPPruner removes old user_ip rows (repository.UserIPRepository).
	//
	// **基準は最終観測 (#3103)。** `createdAt` は初回観測なので、それを基準に
	// 刈ると「初めて見たのは 1 年前だが今も使っている IP」まで消える。
	UserIPPruner interface {
		DeleteLastSeenBefore(t time.Time) (int64, error)
	}
	// RoleAssignmentPruner removes expired role_assignment rows
	// (repository.RoleAssignmentRepository).
	RoleAssignmentPruner interface {
		DeleteExpired(now time.Time) (int64, error)
	}
	// OutdatedGamePruner removes abandoned reversi games
	// (repository.ReversiRepository).
	OutdatedGamePruner interface {
		DeleteOutdatedGames(thresholdID string) (int64, error)
	}
	// CleanIDGenerator generates the reversi threshold id (misc/id.Generator).
	CleanIDGenerator interface {
		Generate(t time.Time) string
	}
	// AntennaDeactivator deactivates antennas unused since a cutoff
	// (repository.AntennaRepository).
	AntennaDeactivator interface {
		DeactivateUnusedSince(cutoff time.Time) (int64, error)
	}
	// PendingSignupPruner removes expired user_pending rows
	// (repository.UserPendingRepository).
	PendingSignupPruner interface {
		DeleteOlderThan(thresholdID string) (int64, error)
	}
	// IPLookupLogPruner removes old ip_lookup_log rows
	// (repository.IPLookupLogRepository).
	//
	// **保持期間が要る理由は記録の中身** — 照会に使った IP が入るので、永久に
	// 残すと `moderation_log` に IP を書くのと変わらなくなる (#3106)。
	IPLookupLogPruner interface {
		DeleteOlderThan(t time.Time) (int64, error)
	}
	// BubbleVersusRecordPruner removes old bubble game versus records
	// (repository.BubbleVersusRepository, #3232).
	BubbleVersusRecordPruner interface {
		DeleteExpired(cutoff time.Time) (int64, error)
	}
)

const (
	// userIPRetention は user_ip 行を保持する期間。**定義は `core/iplog` に 1 つ**
	// — 検索 (#3104) が「この期間より前の接続は残っていない」と画面に出す根拠と
	// 同じ値でなければ、画面が嘘をつく。
	userIPRetention = iplog.Retention
	// ipLookupLogRetention は IP 照会の監査記録を残す期間。**定義は
	// `core/iplookuplog` に 1 つ** — 保持期間を読み手に説明する側と刈る側が
	// 違う値だと、画面や doc が嘘をつく (#3106)。
	ipLookupLogRetention = iplookuplog.Retention
	// bubbleVersusRecordRetention は対戦の記録を残す期間。**定義は
	// `core/bubbleversus` に 1 つ** (理由は ipLookupLogRetention と同じ)。
	bubbleVersusRecordRetention = bubbleversus.RecordRetention
	// reversiOutdatedAfter は開始されないまま放置された reversi game を outdated
	// と見なす猶予。upstream cleanOutdatedGames は now-10min の id を閾値にする。
	reversiOutdatedAfter = 10 * time.Minute
	// pendingSignupRetention は期限切れ `user_pending` 行を残す猶予 (#3037)。
	//
	// **`signup.PendingSignupTTL` (30 分) より十分長くする。** 掃除は日次なので
	// 厳密さは要らず、短くしても得は無い一方、短すぎると「昇格できるはずの行を
	// 掃除が先に消す」競合が生まれる。
	//
	// **upstream に対応する cron は無い** (`CleanProcessorService` は
	// `user_pending` を見ない) mk-go 独自の追加。`PromotePending` は期限切れを
	// 拒否するだけで行を消さないので、放置された登録の**メールアドレスと
	// パスワードハッシュ**が無期限に貯まっていた。
	pendingSignupRetention = 24 * time.Hour
)

// CleanProcessor implements the upstream generic `clean` systemQueue cron
// (`0 0 * * *`, #1563): (1) prune user_ip older than 90 days, (2) delete
// expired role assignments, (3) delete not-yet-started reversi games older
// than ~10 minutes, (4) deactivate antennas unused for longer than
// antennaThreshold (#1604; relies on antenna.lastUsedAt being bumped on
// read/update so active antennas are not wrongly deactivated).
type CleanProcessor struct {
	userIP           UserIPPruner
	roleAssign       RoleAssignmentPruner
	reversi          OutdatedGamePruner
	idGen            CleanIDGenerator
	antenna          AntennaDeactivator
	antennaThreshold time.Duration
	pending          PendingSignupPruner
	ipLookupLog      IPLookupLogPruner
	bubbleVersus     BubbleVersusRecordPruner
}

// SetIPLookupLogPruner wires the IP lookup audit retention (#3106).
//
// **コンストラクタの引数にしない。** 既に 7 つあり、位置引数を増やすと全呼び出し元と
// テストを触ることになる。nil なら sub-task は no-op。
func (p *CleanProcessor) SetIPLookupLogPruner(pr IPLookupLogPruner) { p.ipLookupLog = pr }

// SetBubbleVersusRecordPruner wires the bubble game versus record retention
// (#3232). Like SetIPLookupLogPruner it is a setter so the constructor does not
// grow; nil makes the sub-task a no-op.
func (p *CleanProcessor) SetBubbleVersusRecordPruner(pr BubbleVersusRecordPruner) {
	p.bubbleVersus = pr
}

// NewCleanProcessor constructs the processor. Any nil dependency disables its
// sub-task (no-op) rather than panicking. antennaThreshold <= 0 also disables
// the antenna deactivate sub-task (matching upstream's `> 0` guard).
func NewCleanProcessor(userIP UserIPPruner, roleAssign RoleAssignmentPruner, reversi OutdatedGamePruner, idGen CleanIDGenerator, antenna AntennaDeactivator, antennaThreshold time.Duration, pending PendingSignupPruner) *CleanProcessor {
	return &CleanProcessor{
		userIP:           userIP,
		roleAssign:       roleAssign,
		reversi:          reversi,
		idGen:            idGen,
		antenna:          antenna,
		antennaThreshold: antennaThreshold,
		pending:          pending,
	}
}

// Handle implements the driver handler contract. Each sub-task logs and
// swallows its own error so one failure does not abort the others; the job is
// always reported as success (MaxRetry(0)).
func (p *CleanProcessor) Handle(_ context.Context, _ driver.Task) error {
	now := time.Now()

	if p.userIP != nil {
		if n, err := p.userIP.DeleteLastSeenBefore(now.Add(-userIPRetention)); err != nil {
			slog.Warn("clean: prune user_ip failed", "err", err)
		} else if n > 0 {
			slog.Info("clean: pruned user_ip", "count", n)
		}
	}

	if p.roleAssign != nil {
		if n, err := p.roleAssign.DeleteExpired(now); err != nil {
			slog.Warn("clean: delete expired role assignments failed", "err", err)
		} else if n > 0 {
			slog.Info("clean: deleted expired role assignments", "count", n)
		}
	}

	if p.reversi != nil && p.idGen != nil {
		threshold := p.idGen.Generate(now.Add(-reversiOutdatedAfter))
		if n, err := p.reversi.DeleteOutdatedGames(threshold); err != nil {
			slog.Warn("clean: delete outdated reversi games failed", "err", err)
		} else if n > 0 {
			slog.Info("clean: deleted outdated reversi games", "count", n)
		}
	}

	if p.ipLookupLog != nil {
		if n, err := p.ipLookupLog.DeleteOlderThan(now.Add(-ipLookupLogRetention)); err != nil {
			slog.Warn("clean: prune ip lookup log failed", "err", err)
		} else if n > 0 {
			slog.Info("clean: pruned ip lookup log", "count", n)
		}
	}

	if p.bubbleVersus != nil {
		if n, err := p.bubbleVersus.DeleteExpired(now.Add(-bubbleVersusRecordRetention)); err != nil {
			slog.Warn("clean: prune bubble game versus records failed", "err", err)
		} else if n > 0 {
			slog.Info("clean: pruned bubble game versus records", "count", n)
		}
	}

	if p.pending != nil && p.idGen != nil {
		threshold := p.idGen.Generate(now.Add(-pendingSignupRetention))
		if n, err := p.pending.DeleteOlderThan(threshold); err != nil {
			slog.Warn("clean: delete expired pending signups failed", "err", err)
		} else if n > 0 {
			slog.Info("clean: deleted expired pending signups", "count", n)
		}
	}

	if p.antenna != nil && p.antennaThreshold > 0 {
		if n, err := p.antenna.DeactivateUnusedSince(now.Add(-p.antennaThreshold)); err != nil {
			slog.Warn("clean: deactivate unused antennas failed", "err", err)
		} else if n > 0 {
			slog.Info("clean: deactivated unused antennas", "count", n)
		}
	}

	return nil
}
