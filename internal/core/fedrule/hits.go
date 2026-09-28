package fedrule

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"
)

const (
	hitsPrefix = "apFederationRule"
	// HitWindow is how far back the hit counts reach.
	HitWindow = 24 * time.Hour
	// MaxSamples is how many recent hits are kept per rule.
	MaxSamples = 50
	sampleTTL  = 7 * 24 * time.Hour
	// hitQueueSize bounds the hits waiting to be written. 溢れたら捨てる —
	// 記録のために受信を止めない。
	hitQueueSize = 1024
)

// HitStore counts the hits per rule in hourly Redis buckets and keeps the most
// recent ones, so an admin can see what a rule would do before enforcing it.
//
// **書き込みは 1 つの goroutine がまとめて行う。** 受信の経路 (Record) は
// channel へ積むだけで、Redis の遅延や障害を受信へ伝えない。
type HitStore struct {
	rdb   redis.UniversalClient
	clock func() time.Time
	queue chan Hit
}

// NewHitStore constructs a HitStore. Start must be called to write the hits.
func NewHitStore(rdb redis.UniversalClient) *HitStore {
	return &HitStore{rdb: rdb, clock: time.Now, queue: make(chan Hit, hitQueueSize)}
}

// SetClockForTest overrides the time source.
func (s *HitStore) SetClockForTest(fn func() time.Time) { s.clock = fn }

func (s *HitStore) countKey(t time.Time) string {
	return hitsPrefix + ":hits:" + strconv.FormatInt(t.UTC().Unix()/3600, 10)
}

func sampleKey(ruleID string) string { return hitsPrefix + ":samples:" + ruleID }

// Record queues a hit. It never blocks.
func (s *HitStore) Record(h Hit) {
	if s == nil || s.rdb == nil {
		return
	}
	// 相手が決める値なので長さを切る (管理画面に出すだけだが、Redis に
	// 巨大な値を積ませない)。
	h.Host = truncate(h.Host, 256)
	h.Subject = truncate(h.Subject, 1024)
	h.Kind = truncate(h.Kind, 64)
	select {
	case s.queue <- h:
	default:
		// 溢れた分は数えない (件数は下限になる)。
	}
}

// Start writes the queued hits until ctx is done.
func (s *HitStore) Start(ctx context.Context) {
	if s == nil || s.rdb == nil {
		return
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case h := <-s.queue:
				batch := []Hit{h}
			drain:
				for len(batch) < 256 {
					select {
					case h := <-s.queue:
						batch = append(batch, h)
					default:
						break drain
					}
				}
				wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				if err := s.write(wctx, batch); err != nil {
					slog.Warn("fedrule: cannot record rule hits", "err", err, "hits", len(batch))
				}
				cancel()
			}
		}
	}()
}

func (s *HitStore) write(ctx context.Context, hits []Hit) error {
	pipe := s.rdb.Pipeline()
	keys := map[string]bool{}
	for _, h := range hits {
		key := s.countKey(h.At)
		keys[key] = true
		pipe.HIncrBy(ctx, key, h.RuleID, 1)
		if encoded, err := json.Marshal(h); err == nil {
			pipe.LPush(ctx, sampleKey(h.RuleID), encoded)
			pipe.LTrim(ctx, sampleKey(h.RuleID), 0, MaxSamples-1)
			pipe.Expire(ctx, sampleKey(h.RuleID), sampleTTL)
		}
	}
	for key := range keys {
		pipe.Expire(ctx, key, HitWindow+2*time.Hour)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// Counts returns the hits of each rule in the last HitWindow.
//
// 時間単位のバケットなので、窓の端は最大 1 時間ぶん広い。
func (s *HitStore) Counts(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	if s == nil || s.rdb == nil {
		return out, nil
	}
	now := s.clock()
	pipe := s.rdb.Pipeline()
	var cmds []*redis.MapStringStringCmd
	for i := 0; i < int(HitWindow/time.Hour); i++ {
		cmds = append(cmds, pipe.HGetAll(ctx, s.countKey(now.Add(-time.Duration(i)*time.Hour))))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, err
	}
	for _, cmd := range cmds {
		for id, v := range cmd.Val() {
			n, err := strconv.ParseInt(v, 10, 64)
			if err == nil {
				out[id] += n
			}
		}
	}
	return out, nil
}

// Samples returns the most recent hits of a rule, newest first.
func (s *HitStore) Samples(ctx context.Context, ruleID string, limit int) ([]Hit, error) {
	out := []Hit{}
	if s == nil || s.rdb == nil {
		return out, nil
	}
	if limit <= 0 || limit > MaxSamples {
		limit = MaxSamples
	}
	raw, err := s.rdb.LRange(ctx, sampleKey(ruleID), 0, int64(limit-1)).Result()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	for _, r := range raw {
		var h Hit
		if json.Unmarshal([]byte(r), &h) == nil {
			out = append(out, h)
		}
	}
	return out, nil
}

// Forget drops the hits recorded for a rule: the kept samples and its counts
// in every bucket of the window. 削除したときと、条件を変えたとき (前の条件で
// 数えた件数は新しい条件の当たり具合を表さない) に呼ぶ。
func (s *HitStore) Forget(ctx context.Context, ruleID string) error {
	if s == nil || s.rdb == nil {
		return nil
	}
	now := s.clock()
	pipe := s.rdb.Pipeline()
	pipe.Del(ctx, sampleKey(ruleID))
	// 期限 (窓 + 2 時間) のあいだ残るバケットをすべて見る。
	for i := 0; i <= int(HitWindow/time.Hour)+2; i++ {
		pipe.HDel(ctx, s.countKey(now.Add(-time.Duration(i)*time.Hour)), ruleID)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// バイト数で切って、壊れた末尾の rune を落とす。
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
