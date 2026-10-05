// Package driveusage aggregates instance-wide drive storage usage for the admin
// API (#3053).
//
// **集計は都度走らせる。** 実測は運用中のインスタンス (drive_file 74,757 行 /
// user 38,694 / emoji 20,907) で 3 本合わせて 225 ms、合成データ (drive_file
// 2,005,400 行 / note 2,000,000 / user 55,000 / emoji 5,000) で 1.84 秒だったので、
// 定期集計のジョブを持つ必要が無い。ただし管理画面がタブ切り替えのたびに叩くと同じ
// 走査を繰り返すので、短い TTL のスナップショットを 1 つだけ持ち、同時要求は
// singleflight で 1 本に畳む。
package driveusage

import (
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/repository"
	"golang.org/x/sync/singleflight"
)

const (
	// DefaultTTL is how long a computed snapshot is reused.
	DefaultTTL = 5 * time.Minute
	// DefaultTopN is how many hosts / users the rankings carry.
	DefaultTopN = 30
	// cacheKey is the singleflight key. 集計は 1 種類しか無いので定数。
	cacheKey = "breakdown"
)

// Result is one snapshot of the instance's drive usage.
type Result struct {
	Breakdown *repository.DriveUsageBreakdown
	// CalculatedAt is when the aggregation ran (not when it was served).
	CalculatedAt time.Time
	// Elapsed is how long the aggregation took.
	Elapsed time.Duration
	// Cached reports that this call reused a stored snapshot instead of running
	// the aggregation. 待ち合わせで他の呼び出しの計算結果を受け取った場合は
	// **false** — その値はこの呼び出しのために計算されたもので、古くない。
	Cached bool
	// TopN is the ranking cap the snapshot was built with.
	TopN int
	// TTL is how long this snapshot stays reusable (0 when caching is off).
	TTL time.Duration
}

// Service serves drive usage snapshots with a short-lived cache.
type Service struct {
	repo repository.DriveUsageRepository
	ttl  time.Duration
	topN int
	// now は時刻の注入口 (テストで TTL の境界を動かすため)。
	now func() time.Time

	group singleflight.Group

	mu     sync.RWMutex
	cached *Result
}

// NewService wires the aggregation source. ttl <= 0 disables caching (every call
// recomputes); topN <= 0 falls back to DefaultTopN.
func NewService(repo repository.DriveUsageRepository, ttl time.Duration, topN int) *Service {
	if topN <= 0 {
		topN = DefaultTopN
	}
	return &Service{repo: repo, ttl: ttl, topN: topN, now: time.Now}
}

// Breakdown returns the current snapshot, recomputing it when the cached one has
// expired or forceRecalc is set.
//
// forceRecalc が立っていても、既に走っている集計があればそれに相乗りする
// (singleflight)。管理者が連打しても走査が積み上がらない。
func (s *Service) Breakdown(forceRecalc bool) (*Result, error) {
	if !forceRecalc {
		if r := s.fresh(); r != nil {
			return r, nil
		}
	}

	// **鮮度の判定はこの 1 箇所だけにする。** `group.Do` の中でもう一度確かめる
	// 形も書けるが、そうすると**どちらか片方を消しても振る舞いが変わらない**ので、
	// 変異検証がどちらの分岐にも効かなくなる (実測でそうなった)。外側に置くのは、
	// 集計が走っている最中に来た要求へ**待たせずに手持ちのスナップショットを
	// 返せる**ため。代償は「判定の直後に別の呼び出しが計算し終えた」ときに
	// 1 回だけ余計に走ること (結果は正しい)。
	v, err, _ := s.group.Do(cacheKey, func() (any, error) {
		started := s.now()
		b, err := s.repo.Breakdown(s.topN)
		if err != nil {
			return nil, err
		}
		res := &Result{
			Breakdown:    b,
			CalculatedAt: started,
			Elapsed:      s.now().Sub(started),
			TopN:         s.topN,
			TTL:          s.ttl,
		}
		s.mu.Lock()
		s.cached = res
		s.mu.Unlock()
		return res, nil
	})
	if err != nil {
		return nil, err
	}
	// **呼び出しごとに値のコピーを返す。** `group.Do` は待機者**全員**に同じ
	// ポインタを返すので、ここで複製しないと (a) 呼び出し側の書き換えでキャッシュ
	// 本体が汚れ、(b) 追従者どうしが同じ構造体を共有する。fresh() 側も複製を
	// 返すので、保存済みスナップショットのポインタはこの関数の外へ出ない。
	out := *v.(*Result)
	return &out, nil
}

// fresh returns the cached snapshot when it is still within the TTL, or nil.
func (s *Service) fresh() *Result {
	// **これは防御であって、現状の唯一の歯止めではない。** 下の比較が `>=` なので
	// ttl が 0 でも負でも「期限切れ」に倒れる (= この分岐を外しても振る舞いは
	// 同じで、テストでも差が出ない)。比較を `>` に変えたときに ttl=0 が
	// 「永久キャッシュ」へ化けるのを止めるために残す。
	if s.ttl <= 0 {
		return nil
	}
	s.mu.RLock()
	cached := s.cached
	s.mu.RUnlock()
	if cached == nil {
		return nil
	}
	if s.now().Sub(cached.CalculatedAt) >= s.ttl {
		return nil
	}
	// 呼び出し側が Cached を書き換えても保存済みスナップショットが汚れないよう
	// 値のコピーを返す。Breakdown 本体は読み取り専用なので共有してよい。
	out := *cached
	out.Cached = true
	return &out
}

// TTL reports the configured cache lifetime (0 when caching is off).
func (s *Service) TTL() time.Duration { return s.ttl }

// TopN reports the ranking cap this service uses.
func (s *Service) TopN() int { return s.topN }
