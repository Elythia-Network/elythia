package deliveryhealth

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Circuit breaker for outbound delivery (#3048).
//
// 観測 (Store) とは**別のキーに置く**。観測の 1 分バケットは 2 時間で消える
// 集計用で、判断に使う状態 (いつ開いたか / 次にいつ試すか) を持てない。
//
// **状態は Redis に置く。** queue ノードは複数ありうるので、プロセスメモリに
// 持つとノードごとに別の判断になる (Store が Redis を選んだのと同じ理由)。
//
// ブレーカーは 3 つの状態を持つ:
//   - 閉 (hash が無いか o=0): 通す。接続失敗 / 5xx を数える
//   - 開 (o>0, now < p): 通さない。次に試す時刻 p まで遅延させる
//   - 半開 (o>0, now >= p): 1 件だけ通す (probe ロック)。成功で閉じ、失敗で
//     間隔を倍にして開き直す
//
// 429 はブレーカーとは別に扱う (相手は健在で「速すぎる」と言っているだけ)。
// 言われた間は送らず、待たせたジョブには明けた後の送信時刻を 1 件ずつずらして
// 予約させる。
const (
	breakerPrefix = "apDeliveryBreaker"

	// BreakerThreshold is how many consecutive transport / 5xx failures
	// open the breaker.
	//
	// 1 回で開くと、相手の一時的な再起動や瞬断で配送が止まる。並行して飛んで
	// いる配送がまとめて失敗しても開くが、開いた直後の間隔は 1 分なので、
	// 瞬断なら 1 分ほどで戻る (間隔を倍にするのは試行が失敗したときだけ)。
	BreakerThreshold = 5
	// BreakerInitialInterval is the first wait before a probe.
	BreakerInitialInterval = time.Minute
	// BreakerMaxInterval caps the probe interval.
	//
	// **上限を 1 時間より長くしない。** 開いている間の試行が「応答しない」状態を
	// 記録し続けることで、7 日で既存の自動停止 (autoSuspendedForNotResponding) に
	// 入り、溜まったジョブが配送時の判定で捨てられる。試行が止まるとこの上限が
	// 効かなくなる。
	BreakerMaxInterval = time.Hour
	// breakerHoldCap caps how long one held job waits before checking again.
	//
	// **次に試す時刻まで丸ごと待たせない。** 待たせたジョブは Redis の delayed に
	// 入るので、ホスト単位で起こす手段が無い。試行が通ったり管理者が手で閉じたり
	// しても、最大 1 時間待ったままになる。5 分で起きて判定し直せば、再開後は
	// 5 分以内に流れる (起きるたびに Redis を 1 回読むだけ)。
	breakerHoldCap = 5 * time.Minute
	// breakerProbeWait is how long jobs wait while the single probe is in
	// flight. 試行はすぐ終わることが多いので短くする。長いと、試行が通った後も
	// 溜まったジョブがその分待たされる。
	breakerProbeWait = 15 * time.Second
	// breakerProbeLockTTL bounds how long one probe owns the half-open slot.
	// 配送の HTTP タイムアウトより長くする。短いと試行中に次の試行が通り、
	// 半開が「1 件だけ」でなくなる。probe が落ちてロックが残っても、この時間で
	// 次の試行に移る。
	breakerProbeLockTTL = 5 * time.Minute
	// breakerStateTTL keeps a host's state from outliving the auto-suspend
	// horizon for long. 書くたびに延ばすので、開いている間は消えない。
	breakerStateTTL = 8 * 24 * time.Hour
	// breakerFailureWindow forgets consecutive failures that stopped
	// happening. 閉じている間の失敗数だけの hash は、成功が来なくても
	// この時間で消える。
	breakerFailureWindow = time.Hour

	// ThrottleDefault is used for a 429 without a usable Retry-After.
	ThrottleDefault = time.Minute
	// ThrottleMax caps how long a 429 can hold a host.
	ThrottleMax = time.Hour
	// throttleSpacing is the gap between the reserved send times of jobs
	// held by a 429.
	//
	// **明けた瞬間に溜まった分を一斉に送らない。** 一斉に送るとほぼ確実にまた
	// 429 が返り、そのジョブが 1 件ずつ試行回数を失う。待たせるときに「次に空いて
	// いる送信時刻」(絶対時刻) を 1 件ずつ割り当てるので、予約はホストごとに 1 本の
	// 列になる。明けた後に新しく来た配送は間隔の対象外 (普段どおりの流量)。
	//
	// **順番 (相対の番号) にしない。** 429 は並行して何本も返るので、そのたびに
	// 番号を数え直すと同じ番号が重なり、明けた直後に「各回の 1 番」が一斉に起きる。
	// 送り出しの途中で 429 が再発したときも、古い予約と新しい予約が 2 本並んで流れる。
	throttleSpacing = 500 * time.Millisecond
)

// BreakerDecision is the outcome of Check.
type BreakerDecision int

const (
	// BreakerAllow lets the delivery go.
	BreakerAllow BreakerDecision = iota
	// BreakerProbe lets the delivery go as the single half-open probe.
	BreakerProbe
	// BreakerDelay holds the delivery back for the returned duration.
	BreakerDelay
)

// CheckResult is what Check decided for one delivery.
type CheckResult struct {
	Decision BreakerDecision
	// Delay は Decision が BreakerDelay のときの待ち時間。
	Delay time.Duration
	// HasState はこのホストにブレーカーの状態 (数えている失敗か開いている状態) が
	// あるか。429 だけでは立たない。無ければ成功のたびに Redis を叩かずに済む。
	HasState bool
	// ProbeToken は半開の試行の札。試行の結果はこの札と一緒に返す。札が一致する
	// ときだけ「試行の失敗」として間隔を倍にし、枠を外す。
	ProbeToken string
}

// BreakerState is one host's breaker as shown to admins.
type BreakerState struct {
	Host string `json:"host"`
	// Open は開いている (配送を止めている) か。false なら 429 で間隔を空けているか、
	// 待たせた分を予約どおりに送っているだけ。
	Open                bool       `json:"open"`
	ConsecutiveFailures int        `json:"consecutiveFailures"`
	OpenedAt            *time.Time `json:"openedAt"`
	NextProbeAt         *time.Time `json:"nextProbeAt"`
	ProbeIntervalSec    int        `json:"probeIntervalSeconds"`
	// ThrottledUntil は 429 で送らずにいる期限。nil なら止めていない。
	ThrottledUntil *time.Time `json:"throttledUntil"`
	// ReservedUntil は 429 で待たせた配送の最後の予約時刻 (送り終わる見込み)。
	// nil なら予約は残っていない。
	ReservedUntil *time.Time `json:"reservedUntil"`
}

// Breaker holds per-host breaker and throttle state in Redis.
type Breaker struct {
	rdb   redis.UniversalClient
	clock func() time.Time
	// jitter spreads delayed jobs so they do not all wake at the same time.
	jitter func(max time.Duration) time.Duration
}

// NewBreaker constructs a Breaker. rdb が nil なら nil を返す (配送は止めない)。
func NewBreaker(rdb redis.UniversalClient) *Breaker {
	if rdb == nil {
		return nil
	}
	return &Breaker{
		rdb:   rdb,
		clock: time.Now,
		jitter: func(max time.Duration) time.Duration {
			if max <= 0 {
				return 0
			}
			return time.Duration(rand.Int64N(int64(max)))
		},
	}
}

func breakerKey(host string) string  { return breakerPrefix + ":" + host }
func probeKey(host string) string    { return breakerPrefix + ":probe:" + host }
func throttleKey(host string) string { return breakerPrefix + ":throttle:" + host }

// slotKey holds the last reserved send time (unix ms) for a host held by 429.
func slotKey(host string) string { return breakerPrefix + ":slot:" + host }

// reservationKey maps a job key to its reserved send time (unix ms).
func reservationKey(host string) string { return breakerPrefix + ":resv:" + host }
func breakerIndexKey() string           { return breakerPrefix + ":hosts" }

// Check result kinds returned by checkScript.
const (
	checkAllow    = 0
	checkProbe    = 1
	checkOpen     = 2 // 開いている。値は次に試す時刻までの ms
	checkProbing  = 3 // 試行中。値はロックの残り ms
	checkThrottle = 4 // 429 で止めている。値は予約した送信時刻までの ms
)

// checkScript returns {kind, ms, hasState}.
//
// 判定と probe ロックの取得を不可分にする。別々に叩くと、半開の瞬間に
// 複数のノードが同時に「試してよい」と判断する。
var checkScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local lockTTL = tonumber(ARGV[2])
local token = ARGV[3]
local spacing = tonumber(ARGV[4])
local job = ARGV[5]
local hasState = redis.call("EXISTS", KEYS[1])
-- 予約済みのジョブは自分の時刻まで待つ。時刻が来たら予約を消して先へ進む
-- (まだ止めていれば列の最後尾に並び直す = 429 が再発した)。
if job ~= "" then
  local mine = tonumber(redis.call("HGET", KEYS[5], job) or "0")
  if mine > now then
    return {4, mine - now, hasState}
  end
  if mine > 0 then
    redis.call("HDEL", KEYS[5], job)
  end
end
local ttl = redis.call("PTTL", KEYS[3])
if ttl and ttl > 0 then
  -- 予約は「止めている期限」と「最後の予約 + 間隔」の遅い方。前の 429 で予約した
  -- 分が残っていれば、その後ろに並ぶ (列は 1 本)。
  local slot = now + ttl
  local last = tonumber(redis.call("GET", KEYS[4]) or "0")
  if last + spacing > slot then
    slot = last + spacing
  end
  redis.call("SET", KEYS[4], slot, "PX", slot - now + 60000)
  if job ~= "" then
    redis.call("HSET", KEYS[5], job, slot)
    redis.call("PEXPIRE", KEYS[5], slot - now + 60000)
  end
  return {4, slot - now, hasState}
end
local o = tonumber(redis.call("HGET", KEYS[1], "o") or "0")
if o > 0 then
  local p = tonumber(redis.call("HGET", KEYS[1], "p") or "0")
  if now < p then
    return {2, p - now, 1}
  end
  if redis.call("SET", KEYS[2], token, "NX", "PX", lockTTL) then
    return {1, 0, 1}
  end
  return {3, redis.call("PTTL", KEYS[2]), 1}
end
return {0, 0, hasState}
`)

// Check decides whether a delivery to host may go now. job identifies the
// delivery across wake-ups (the processor passes a hash of the payload), so
// a job held by a 429 keeps its reserved send time; "" reserves anew each time.
//
// **Redis が読めなければ通す。** 観測のための仕組みが配送を止める側に倒れると、
// Redis の瞬断が全配送の停止になる。
func (b *Breaker) Check(ctx context.Context, host, job string) CheckResult {
	if b == nil || host == "" {
		return CheckResult{}
	}
	token := b.token()
	res, err := checkScript.Run(ctx, b.rdb,
		[]string{breakerKey(host), probeKey(host), throttleKey(host), slotKey(host), reservationKey(host)},
		b.clock().UnixMilli(), breakerProbeLockTTL.Milliseconds(), token, throttleSpacing.Milliseconds(), job).Int64Slice()
	if err != nil || len(res) != 3 {
		return CheckResult{}
	}
	r := CheckResult{HasState: res[2] == 1}
	ms := time.Duration(res[1]) * time.Millisecond
	switch res[0] {
	case checkProbe:
		r.Decision, r.ProbeToken = BreakerProbe, token
	case checkOpen:
		// 次に試す時刻まで丸ごとは待たせない (breakerHoldCap)。一斉に起きないよう、
		// 待ち時間の 1 割までずらす (待ちは最大 5 分なので、ずれは最大 30 秒)。
		wait := min(ms, breakerHoldCap)
		r.Decision, r.Delay = BreakerDelay, wait+b.jitter(wait/10)
	case checkProbing:
		wait := min(max(ms, time.Second), breakerProbeWait)
		r.Decision, r.Delay = BreakerDelay, wait+b.jitter(wait/2)
	case checkThrottle:
		// 予約した送信時刻まで。ずらしは要らない (1 件ずつ別の時刻になっている)。
		// **丸ごとは待たせない** (開いているときと同じ breakerHoldCap)。予約の列は
		// 溜まった件数 x 間隔まで伸びるので、丸ごと待たせると手で閉じても遅延中の
		// ジョブを起こせない。起きたジョブは自分の予約を見て待ち直す。
		r.Decision, r.Delay = BreakerDelay, min(max(ms, time.Millisecond), breakerHoldCap)
	}
	return r
}

func (b *Breaker) token() string {
	return strconv.FormatUint(rand.Uint64(), 36)
}

// failureScript counts a transport / 5xx failure and opens the breaker,
// or re-opens it with a doubled interval when the failure is the probe's.
// Returns {open, waitMs}.
var failureScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local threshold = tonumber(ARGV[2])
local initial = tonumber(ARGV[3])
local maxInterval = tonumber(ARGV[4])
local stateTTL = tonumber(ARGV[5])
local failureWindow = tonumber(ARGV[6])
-- 札が今の試行のものと一致するときだけ試行の失敗として扱う。古い試行 (ロックが
-- 切れた後に返ってきたもの) が、次の試行の枠を外したり間隔を倍にしたりしない。
local probe = ARGV[8] ~= "" and redis.call("GET", KEYS[2]) == ARGV[8]
local f = redis.call("HINCRBY", KEYS[1], "f", 1)
local o = tonumber(redis.call("HGET", KEYS[1], "o") or "0")
if o > 0 then
  redis.call("PEXPIRE", KEYS[1], stateTTL)
  if probe then
    local i = tonumber(redis.call("HGET", KEYS[1], "i") or tostring(initial))
    i = math.min(i * 2, maxInterval)
    redis.call("HSET", KEYS[1], "i", i, "p", now + i)
    redis.call("DEL", KEYS[2])
    redis.call("ZADD", KEYS[3], now, ARGV[7])
    return {1, i}
  end
  local p = tonumber(redis.call("HGET", KEYS[1], "p") or "0")
  return {1, math.max(p - now, 0)}
end
if f >= threshold then
  redis.call("HSET", KEYS[1], "o", now, "i", initial, "p", now + initial)
  redis.call("PEXPIRE", KEYS[1], stateTTL)
  redis.call("ZADD", KEYS[3], now, ARGV[7])
  redis.call("ZREMRANGEBYSCORE", KEYS[3], "-inf", now - stateTTL)
  return {1, initial}
end
redis.call("PEXPIRE", KEYS[1], failureWindow)
return {0, 0}
`)

// RecordFailure counts a transport / 5xx failure for host and reports
// whether the breaker is open afterwards, with how long until the next
// probe. probeToken is CheckResult.ProbeToken ("" when the delivery was
// not the probe).
//
// **間隔を倍にするのは試行 (probe) が失敗したときだけ。** 開いた瞬間に飛んで
// いた配送の失敗まで倍化に数えると、瞬断で並行に失敗しただけで最初の試行が
// 1 時間後まで延びる。
func (b *Breaker) RecordFailure(ctx context.Context, host, probeToken string) (open bool, untilProbe time.Duration) {
	if b == nil || host == "" {
		return false, 0
	}
	res, err := failureScript.Run(ctx, b.rdb,
		[]string{breakerKey(host), probeKey(host), breakerIndexKey()},
		b.clock().UnixMilli(), BreakerThreshold,
		BreakerInitialInterval.Milliseconds(), BreakerMaxInterval.Milliseconds(),
		breakerStateTTL.Milliseconds(), breakerFailureWindow.Milliseconds(), host, probeToken).Int64Slice()
	if err != nil || len(res) != 2 {
		return false, 0
	}
	return res[0] == 1, time.Duration(res[1]) * time.Millisecond
}

// releaseScript frees the half-open slot if it is still owned by token.
var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
  redis.call("DEL", KEYS[1])
end
return 0
`)

// ReleaseProbe frees the half-open slot without recording an outcome, for
// a probe that failed on our side before reaching the host (signing key,
// etc.). 返さないと、ロックが切れる 5 分間は次の試行ができず、待たせている
// ジョブが試行中として起きては待ち直し続ける。
func (b *Breaker) ReleaseProbe(ctx context.Context, host, probeToken string) {
	if b == nil || host == "" || probeToken == "" {
		return
	}
	_ = releaseScript.Run(ctx, b.rdb, []string{probeKey(host)}, probeToken).Err()
}

// HoldFor returns how long a job that failed while the breaker is open
// should wait, capped like Check's holds.
func (b *Breaker) HoldFor(untilProbe time.Duration) time.Duration {
	if b == nil {
		return untilProbe
	}
	wait := min(max(untilProbe, time.Second), breakerHoldCap)
	return wait + b.jitter(wait/10)
}

// RecordSuccess closes the breaker for host: any HTTP response other than
// 5xx means the host is up (4xx means what we sent is wrong, not that the
// host is down). The 429 hold and its reservations are left alone.
//
// **予約には触らない。** 間隔を空けて送り出している最中の配送はどれも成功で
// 終わるので、ここで予約を消すと残りが一斉に起きる。
func (b *Breaker) RecordSuccess(ctx context.Context, host string) {
	_ = b.close(ctx, host, false)
}

// closeScript forgets a host's breaker state (and with ARGV[2] == "1" its
// 429 hold and reservations too), and its index entry unless a 429 hold or
// reservations remain (then the host stays listed).
// 1 本の Lua にするのは、消してから索引を外すまでの間に別ノードで開き直した
// 状態を、索引から落とさないため。
var closeScript = redis.NewScript(`
redis.call("DEL", KEYS[1], KEYS[2])
if ARGV[2] == "1" then
  redis.call("DEL", KEYS[3], KEYS[5], KEYS[6])
end
if redis.call("EXISTS", KEYS[3]) == 0 and redis.call("EXISTS", KEYS[5]) == 0 then
  redis.call("ZREM", KEYS[4], ARGV[1])
end
return 0
`)

// Close lets deliveries to host go again (an admin closing it by hand): it
// forgets the breaker state, the 429 hold and the reserved send times. Held
// jobs wake within breakerHoldCap and find nothing to wait for.
//
// **429 の予約も消す。** 予約の列は溜まった件数 x 間隔まで伸びる (1 万件で
// 1.4 時間) ので、残すと手で閉じても解けない。
func (b *Breaker) Close(ctx context.Context, host string) error {
	return b.close(ctx, host, true)
}

func (b *Breaker) close(ctx context.Context, host string, all bool) error {
	if b == nil || host == "" {
		return nil
	}
	flag := "0"
	if all {
		flag = "1"
	}
	err := closeScript.Run(ctx, b.rdb,
		[]string{breakerKey(host), probeKey(host), throttleKey(host), breakerIndexKey(), slotKey(host), reservationKey(host)},
		host, flag).Err()
	if err != nil {
		return fmt.Errorf("close breaker %s: %w", host, err)
	}
	return nil
}

// Throttle holds deliveries to host for retryAfter (429). Non-positive
// values use ThrottleDefault; values above ThrottleMax are capped.
//
// 相手は健在で「速すぎる」と言っているだけなので、ブレーカーは開かない。
func (b *Breaker) Throttle(ctx context.Context, host string, retryAfter time.Duration) {
	if b == nil || host == "" {
		return
	}
	if retryAfter <= 0 {
		retryAfter = ThrottleDefault
	}
	retryAfter = min(retryAfter, ThrottleMax)
	now := b.clock().UnixMilli()
	pipe := b.rdb.TxPipeline()
	// 期限だけ延ばす。予約 (slotKey) には触らない — 数え直すと予約が重なる。
	pipe.Set(ctx, throttleKey(host), "1", retryAfter)
	pipe.ZAdd(ctx, breakerIndexKey(), redis.Z{Score: float64(now), Member: host})
	pipe.ZRemRangeByScore(ctx, breakerIndexKey(), "-inf", strconv.FormatInt(now-breakerStateTTL.Milliseconds(), 10))
	_, _ = pipe.Exec(ctx)
}

// List returns the hosts whose breaker is open or that are held by a 429,
// most recently touched first. Stale index entries are dropped.
func (b *Breaker) List(ctx context.Context) ([]BreakerState, error) {
	if b == nil {
		return []BreakerState{}, nil
	}
	hosts, err := b.rdb.ZRevRange(ctx, breakerIndexKey(), 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("list breaker hosts: %w", err)
	}
	out := make([]BreakerState, 0, len(hosts))
	var stale []any
	for _, host := range hosts {
		st, ok, err := b.state(ctx, host)
		if err != nil {
			return nil, err
		}
		if !ok {
			stale = append(stale, host)
			continue
		}
		out = append(out, st)
	}
	if len(stale) > 0 {
		_ = b.rdb.ZRem(ctx, breakerIndexKey(), stale...).Err()
	}
	return out, nil
}

func (b *Breaker) state(ctx context.Context, host string) (BreakerState, bool, error) {
	pipe := b.rdb.Pipeline()
	hget := pipe.HGetAll(ctx, breakerKey(host))
	pttl := pipe.PTTL(ctx, throttleKey(host))
	last := pipe.Get(ctx, slotKey(host))
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return BreakerState{}, false, fmt.Errorf("read breaker %s: %w", host, err)
	}
	h := hget.Val()
	st := BreakerState{Host: host}
	if d := pttl.Val(); d > 0 {
		t := b.clock().Add(d)
		st.ThrottledUntil = &t
	}
	if r := msField(last.Val()); r != nil && r.After(b.clock()) {
		st.ReservedUntil = r
	}
	st.ConsecutiveFailures, _ = strconv.Atoi(h["f"])
	if o := msField(h["o"]); o != nil {
		st.Open = true
		st.OpenedAt = o
		st.NextProbeAt = msField(h["p"])
		if i, err := strconv.ParseInt(h["i"], 10, 64); err == nil {
			st.ProbeIntervalSec = int(i / 1000)
		}
	}
	// 閉じていて 429 でも止めておらず予約も残っていないホストは出さない (索引は
	// List が落とす)。失敗を数えているだけのホストは「止めている」わけではないので
	// 載せない。
	return st, st.Open || st.ThrottledUntil != nil || st.ReservedUntil != nil, nil
}

func msField(v string) *time.Time {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return nil
	}
	t := time.UnixMilli(n)
	return &t
}
