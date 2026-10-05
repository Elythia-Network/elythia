// Package iplog records which IP addresses an authenticated account was seen
// from (#3103).
//
// **記録するのは認証済みリクエストのたび。** upstream の `ApiCallService.logIp` と
// 同じ契機で、同じくプロセス内の集合で重複を抑える (upstream は 1 時間ごとに
// clear する)。#3103 より前の mk-go はサインイン成功時しか記録しておらず、
// 「ログインに使った IP」しか残らないため、再ログインしない利用者は IP が 1 つの
// まま固定されていた。
//
// **`meta.enableIpLogging` は呼び出しのたびに読む。** 起動時に 1 度だけ読む形だと、
// 管理画面で有効にしても再起動するまで記録が始まらない (#3107)。
package iplog

import (
	"log/slog"
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/misc/ipnorm"
	"github.com/elythia-network/elythia/internal/model"
)

// Observer persists one observation. Implemented by repository.UserIPRepository.
type Observer interface {
	Observe(userID, ip string, at time.Time) error
}

// MetaSource reads the instance meta. Implemented by repository.MetaRepository.
//
// router が渡すのは 5 分 TTL の `CachedMetaRepository` なので、呼び出しのたびに
// 読んでも毎回 DB へ行くわけではない。`admin/update-meta` が更新時に cache を
// invalidate する (worker 跨ぎの hook 付き) ので、**切り替えは TTL を待たずに効く**。
//
// **ただし無料ではない。** 認証済みリクエストのたびに `RWMutex.RLock` を通り、
// TTL が切れた瞬間は並行リクエストが write lock と DB クエリの後ろに並ぶ。
// upstream はプロセス内のオブジェクトを読むだけなので、そこは mk-go の方が高い。
type MetaSource interface {
	Fetch() (*model.Meta, error)
}

// DedupeWindow is how long a (user, ip) pair stays suppressed after it was
// recorded. upstream の `userIpHistories` は 1 時間ごとに clear するので合わせる。
const DedupeWindow = time.Hour

// Retention is how long a `user_ip` row is kept. upstream の
// `CleanProcessorService` と同じく 90 日。
//
// **記録側と、それを読む側の両方が同じ値を要る。** 日次の掃除 (`queue/processors`)
// が刈る基準であり、検索 (#3104) が「この期間より前の接続は残っていない」と
// 画面に出す根拠でもある。2 箇所に書くと、片方だけ変えたときに**画面が嘘をつく**
// (「記録が無い」と「もう消した」の区別が付かなくなる) ので、ここを唯一の定義にする。
const Retention = 90 * 24 * time.Hour

// Service records observations, skipping ones it has already written recently.
type Service struct {
	repo Observer
	meta MetaSource
	// now は時刻の注入口 (テストで窓の境界を動かすため)。
	now    func() time.Time
	window time.Duration
	// dispatch は書き込みの走らせ方。既定は goroutine で、テストだけ同期にする。
	dispatch func(func())

	mu sync.Mutex
	// seen は (userID -> ip) の集合。upstream と同じく**窓ごとに丸ごと捨てる**。
	// 個別に期限を持たせないのは、要るのが「同じ観測を書き続けない」ことだけで、
	// 正確な有効期限ではないため。
	seen      map[string]map[string]struct{}
	clearedAt time.Time
}

// NewService wires the recorder. window <= 0 falls back to DedupeWindow.
func NewService(repo Observer, meta MetaSource, window time.Duration) *Service {
	if window <= 0 {
		window = DedupeWindow
	}
	now := time.Now
	return &Service{
		repo:      repo,
		meta:      meta,
		now:       now,
		window:    window,
		dispatch:  func(f func()) { go f() },
		seen:      make(map[string]map[string]struct{}),
		clearedAt: now(),
	}
}

// Record notes that userID was seen from rawIP.
//
// 書き込みは goroutine へ逃がす。認証済みリクエストのたびに呼ばれるので、DB の
// 応答を待たせない。重複排除は同期で行うので、2 回目以降は何も起きない。
func (s *Service) Record(userID, rawIP string) {
	if s == nil || s.repo == nil || s.meta == nil || userID == "" {
		return
	}
	m, err := s.meta.Fetch()
	if err != nil || m == nil || !m.EnableIPLogging {
		// **無効なら何もしない。** 記録の可否は運用者の設定で、この機能のために
		// 暗黙に有効化しない (#3066 §7)。
		return
	}
	ip, ok := ipnorm.Normalize(rawIP)
	if !ok {
		// IP として読めない値は保存しない。どの検索にも一致しえない。
		return
	}
	at := s.now()
	if !s.mark(userID, ip, at) {
		return
	}
	s.dispatch(func() { s.write(userID, ip, at) })
}

// write persists one observation, releasing the dedupe mark when it fails so the
// next request retries instead of losing the whole window.
//
// **DB が落ちている間は重複排除が実質無効になる。** 認証済みリクエストごとに
// goroutine 1 個と `slog.Warn` 1 行が出る (upstream は mark を残すので窓内は
// 再試行しない)。観測を窓ぶん丸ごと落とすより、落ちている間だけ騒がしいほうを
// 採った。
func (s *Service) write(userID, ip string, at time.Time) {
	if err := s.repo.Observe(userID, ip, at); err != nil {
		slog.Warn("iplog: failed to record observation", "userId", userID, "err", err)
		s.unmark(userID, ip)
	}
}

// mark reports whether this observation still needs to be written, recording the
// intent when it does.
func (s *Service) mark(userID, ip string, at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at.Sub(s.clearedAt) >= s.window {
		s.seen = make(map[string]map[string]struct{})
		s.clearedAt = at
	}
	ips := s.seen[userID]
	if ips == nil {
		ips = make(map[string]struct{})
		s.seen[userID] = ips
	}
	if _, dup := ips[ip]; dup {
		return false
	}
	ips[ip] = struct{}{}
	return true
}

func (s *Service) unmark(userID, ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ips := s.seen[userID]; ips != nil {
		delete(ips, ip)
	}
}
