package iprelation_test

import (
	"math"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/iprelation"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const day = 24 * time.Hour

// 半減期 30 日なら 30 日で 0.5、60 日で 0.25 (#3105 の完了条件)。
func TestWeight_HalvesEveryHalfLife(t *testing.T) {
	h := iprelation.DefaultHalfLife
	assert.InDelta(t, 1.0, iprelation.Weight(0, h), 1e-12)
	assert.InDelta(t, 0.5, iprelation.Weight(30*day, h), 1e-12)
	assert.InDelta(t, 0.25, iprelation.Weight(60*day, h), 1e-12)
	assert.InDelta(t, 0.125, iprelation.Weight(90*day, h), 1e-12)
	// 連続的に下がる (段ではない)。
	assert.InDelta(t, math.Sqrt2/2, iprelation.Weight(15*day, h), 1e-12)
}

// **未来の観測を 1 より大きくしない。** 記録は goroutine で走るので観測時刻が
// わずかに進むことがあり、1 を超えると「1 つ共有した候補」が「3 つ共有した候補」を
// 追い抜きうる。
func TestWeight_ClampsFutureObservations(t *testing.T) {
	assert.Equal(t, 1.0, iprelation.Weight(-time.Hour, iprelation.DefaultHalfLife))
	assert.Equal(t, 1.0, iprelation.Weight(0, iprelation.DefaultHalfLife))
}

// halfLife <= 0 は既定に倒す。0 を割ると NaN になり、並べ替えの比較が全て false に
// なって**並びが入力順のまま**になる (エラーも出ない)。
func TestWeight_NonPositiveHalfLifeFallsBack(t *testing.T) {
	want := iprelation.Weight(30*day, iprelation.DefaultHalfLife)
	assert.Equal(t, want, iprelation.Weight(30*day, 0))
	assert.Equal(t, want, iprelation.Weight(30*day, -time.Hour))
	assert.False(t, math.IsNaN(iprelation.Weight(30*day, 0)))
}

func obs(userID, ip string, targetLast, candidateLast time.Time, count int) iprelation.Observation {
	return iprelation.Observation{
		UserID: userID, IP: ip,
		TargetLastSeen: targetLast, CandidateLastSeen: candidateLast, IPAccountCount: count,
	}
}

func userIDs(cs []iprelation.Candidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.UserID)
	}
	return out
}

// 同じ条件なら、古い一致ほど順位への寄与が小さい。
func TestRank_OlderMatchesRankLower(t *testing.T) {
	now := time.Now()
	got := iprelation.Rank(now, iprelation.DefaultHalfLife, []iprelation.Observation{
		obs("u_old", "192.0.2.1", now.Add(-60*day), now.Add(-60*day), 3),
		obs("u_new", "192.0.2.2", now, now, 3),
		obs("u_mid", "192.0.2.3", now.Add(-30*day), now.Add(-30*day), 3),
	})
	assert.Equal(t, []string{"u_new", "u_mid", "u_old"}, userIDs(got))
	assert.InDelta(t, 1.0, got[0].Score, 1e-12)
	assert.InDelta(t, 0.5, got[1].Score, 1e-12)
	assert.InDelta(t, 0.25, got[2].Score, 1e-12)
}

// **古い側からの経過時間で減衰させる。** 一方が最近その IP を使っただけで、
// もう一方の古い一致まで新しい一致として扱わない (#3105 の完了条件)。
func TestRank_DecaysFromTheOlderSide(t *testing.T) {
	now := time.Now()
	got := iprelation.Rank(now, iprelation.DefaultHalfLife, []iprelation.Observation{
		// 対象は今日使ったが、候補がそのIPを最後に使ったのは 60 日前。
		obs("u_stale_candidate", "192.0.2.1", now, now.Add(-60*day), 2),
		// 候補は今日使ったが、対象がそのIPを最後に使ったのは 60 日前。
		obs("u_stale_target", "192.0.2.2", now.Add(-60*day), now, 2),
		// 双方が今日。
		obs("u_both_now", "192.0.2.3", now, now, 2),
	})
	// **どちら向きでも同じ重み (0.25)。** 新しい側を採ると両方 1.0 になる。
	// 同点なので並びは候補側の観測で決まり、候補が最近使った `u_stale_target` が先。
	assert.Equal(t, []string{"u_both_now", "u_stale_target", "u_stale_candidate"}, userIDs(got))
	assert.InDelta(t, 1.0, got[0].Score, 1e-12)
	assert.InDelta(t, 0.25, got[1].Score, 1e-12)
	assert.InDelta(t, 0.25, got[2].Score, 1e-12)
}

// 複数の異なる共有 IP を、時間減衰後の重みとして統合する。
func TestRank_MergesDistinctIPs(t *testing.T) {
	now := time.Now()
	got := iprelation.Rank(now, iprelation.DefaultHalfLife, []iprelation.Observation{
		obs("u1", "192.0.2.1", now, now, 2),
		obs("u1", "192.0.2.2", now.Add(-30*day), now.Add(-30*day), 2),
		obs("u1", "192.0.2.3", now.Add(-60*day), now.Add(-60*day), 2),
	})
	require.Len(t, got, 1, "複数 IP で一致した候補が 1 アカウントに統合されていない")
	assert.Len(t, got[0].SharedIPs, 3)
	assert.InDelta(t, 1.0+0.5+0.25, got[0].Score, 1e-12)
	// 根拠は重みの大きい順。
	assert.Equal(t, []string{"192.0.2.1", "192.0.2.2", "192.0.2.3"},
		[]string{got[0].SharedIPs[0].IP, got[0].SharedIPs[1].IP, got[0].SharedIPs[2].IP})
}

// **重みが同じ根拠は IP 昇順。** `Rank` は純関数なので、呼び出し側が渡す順序に
// 依存して並びが変わってはいけない。
func TestRank_SharedIPsTieBreakByIP(t *testing.T) {
	now := time.Now()
	got := iprelation.Rank(now, iprelation.DefaultHalfLife, []iprelation.Observation{
		obs("u1", "192.0.2.9", now, now, 2),
		obs("u1", "192.0.2.1", now, now, 2),
		obs("u1", "192.0.2.5", now, now, 2),
	})
	require.Len(t, got, 1)
	require.Len(t, got[0].SharedIPs, 3)
	assert.Equal(t, []string{"192.0.2.1", "192.0.2.5", "192.0.2.9"},
		[]string{got[0].SharedIPs[0].IP, got[0].SharedIPs[1].IP, got[0].SharedIPs[2].IP},
		"重みが同じ根拠の並びが入力順に依存している")
}

// **同じ IP を 2 回数えない。** 観測回数では加点しないので、同じ IP への
// ログインを繰り返しても順位は上がらない (#3105 の完了条件)。
func TestRank_DoesNotCountTheSameIPTwice(t *testing.T) {
	now := time.Now()
	got := iprelation.Rank(now, iprelation.DefaultHalfLife, []iprelation.Observation{
		obs("u1", "192.0.2.1", now, now, 2),
		obs("u1", "192.0.2.1", now, now, 2),
		obs("u1", "192.0.2.1", now, now, 2),
	})
	require.Len(t, got, 1)
	assert.Len(t, got[0].SharedIPs, 1)
	assert.InDelta(t, 1.0, got[0].Score, 1e-12, "同じ IP が重ねて加点されている")
}

// **共有 IP 数で乗算しない。** 数は和に既に効いているので、さらに掛けると
// 二重に加点される。2 IP を今日共有しても 2.0 であって 4.0 ではない。
func TestRank_DoesNotMultiplyBySharedCount(t *testing.T) {
	now := time.Now()
	got := iprelation.Rank(now, iprelation.DefaultHalfLife, []iprelation.Observation{
		obs("u1", "192.0.2.1", now, now, 2),
		obs("u1", "192.0.2.2", now, now, 2),
	})
	require.Len(t, got, 1)
	assert.InDelta(t, 2.0, got[0].Score, 1e-12)
}

// **アカウントの最近の活動では上がらない。** 減衰が見るのは対象 IP の観測だけで、
// 候補が今日ノートを書いたかどうかは入らない (#3105 の完了条件)。
// `Rank` はそもそも活動時刻を受け取らないので、同じ観測なら同じスコアになる。
func TestRank_IgnoresUnrelatedActivity(t *testing.T) {
	now := time.Now()
	one := iprelation.Rank(now, iprelation.DefaultHalfLife, []iprelation.Observation{
		obs("u1", "192.0.2.1", now.Add(-60*day), now.Add(-60*day), 2),
	})
	// 同じ IP 一致を持つ別の候補。活動が新しかろうと入力は同じ。
	two := iprelation.Rank(now, iprelation.DefaultHalfLife, []iprelation.Observation{
		obs("u2", "192.0.2.1", now.Add(-60*day), now.Add(-60*day), 2),
	})
	assert.InDelta(t, one[0].Score, two[0].Score, 1e-12)
	assert.InDelta(t, 0.25, one[0].Score, 1e-12)
}

// **同点の並びは決定的。** 浮動小数の和なので厳密な同点は普通に起きる。
// 決定的でないとページを送ったときに同じ候補が 2 回出たり抜けたりする。
func TestRank_TiesAreStable(t *testing.T) {
	now := time.Now()
	build := func() []iprelation.Observation {
		return []iprelation.Observation{
			obs("u_zz", "192.0.2.1", now, now, 2),
			obs("u_aa", "192.0.2.2", now, now, 2),
			obs("u_mm", "192.0.2.3", now, now, 2),
		}
	}
	want := []string{"u_aa", "u_mm", "u_zz"}
	for i := 0; i < 20; i++ {
		assert.Equal(t, want, userIDs(iprelation.Rank(now, iprelation.DefaultHalfLife, build())),
			"同点の並びが実行ごとに変わっている")
	}
}

// 同点でも共有 IP 数が多いほうが先。次は最新の観測、最後が userId 昇順。
func TestRank_TieBreakOrder(t *testing.T) {
	now := time.Now()
	// score はどちらも 1.0 (0.5 + 0.5 と 1.0)。共有 IP 数で分ける。
	got := iprelation.Rank(now, iprelation.DefaultHalfLife, []iprelation.Observation{
		obs("u_one_ip", "192.0.2.9", now, now, 2),
		obs("u_two_ips", "192.0.2.1", now.Add(-30*day), now.Add(-30*day), 2),
		obs("u_two_ips", "192.0.2.2", now.Add(-30*day), now.Add(-30*day), 2),
	})
	require.Len(t, got, 2)
	assert.InDelta(t, got[0].Score, got[1].Score, 1e-12, "前提: スコアが同点")
	assert.Equal(t, "u_two_ips", got[0].UserID, "共有 IP 数が多いほうが先に来ていない")
}

// 共有 IP 数の tiebreak が同じなら、**候補側の**最新の観測が新しいほうが先。
func TestRank_TieBreakUsesCandidateObservation(t *testing.T) {
	now := time.Now()
	got := iprelation.Rank(now, iprelation.DefaultHalfLife, []iprelation.Observation{
		// どちらも古い側が 30 日前 = score 0.5、共有 IP 1 本。
		obs("u_a", "192.0.2.1", now.Add(-30*day), now.Add(-30*day), 2),
		obs("u_b", "192.0.2.2", now.Add(-30*day), now.Add(-10*day), 2),
	})
	require.Len(t, got, 2)
	assert.InDelta(t, got[0].Score, got[1].Score, 1e-12, "前提: スコアが同点")
	assert.Equal(t, "u_b", got[0].UserID, "候補の観測が新しいほうが先に来ていない")
}

// **対象側の最近の利用では順位が上がらない。** 同点の tiebreak で対象側の観測を
// 見ると、「対象がその IP を最近使った」という理由だけで候補の順位が上がり、
// issue が排除している形をスコアではなく tiebreak の粒度で再導入してしまう。
func TestRank_TieBreakIgnoresTargetRecency(t *testing.T) {
	now := time.Now()
	got := iprelation.Rank(now, iprelation.DefaultHalfLife, []iprelation.Observation{
		// 候補側の観測はどちらも 30 日前。対象側だけが違う。
		obs("u_a", "192.0.2.1", now.Add(-30*day), now.Add(-30*day), 2),
		obs("u_z", "192.0.2.2", now, now.Add(-30*day), 2),
	})
	require.Len(t, got, 2)
	assert.InDelta(t, got[0].Score, got[1].Score, 1e-12, "前提: スコアが同点")
	// 候補側が同じなら userId 昇順。対象側を見ていると u_z が先に来る。
	assert.Equal(t, "u_a", got[0].UserID,
		"対象がその IP を最近使ったという理由だけで候補の順位が上がっている")
}

// 減衰に使った経過日数も返す。**重みそのものは [0,1] なのでパーセントと
// 見分けが付かない** — 事実である経過日数を出し、半減期と合わせて再現させる。
func TestRank_ReportsElapsedDays(t *testing.T) {
	now := time.Now()
	got := iprelation.Rank(now, iprelation.DefaultHalfLife, []iprelation.Observation{
		obs("u1", "192.0.2.1", now.Add(-10*day), now.Add(-40*day), 2),
	})
	require.Len(t, got, 1)
	require.Len(t, got[0].SharedIPs, 1)
	// 古い側 (40 日前) からの経過。
	assert.InDelta(t, 40.0, got[0].SharedIPs[0].ElapsedDays, 1e-9)
}

// 未来の観測でも経過日数を負にしない (重みと同じ理由)。
func TestRank_ElapsedDaysNeverNegative(t *testing.T) {
	now := time.Now()
	got := iprelation.Rank(now, iprelation.DefaultHalfLife, []iprelation.Observation{
		obs("u1", "192.0.2.1", now.Add(day), now.Add(day), 2),
	})
	require.Len(t, got, 1)
	assert.Equal(t, 0.0, got[0].SharedIPs[0].ElapsedDays)
	assert.Equal(t, 1.0, got[0].SharedIPs[0].Weight)
}

// 空の入力は空の結果。nil を返しても panic させない。
func TestRank_EmptyInput(t *testing.T) {
	assert.Empty(t, iprelation.Rank(time.Now(), iprelation.DefaultHalfLife, nil))
	assert.Empty(t, iprelation.Rank(time.Now(), iprelation.DefaultHalfLife, []iprelation.Observation{}))
}

// userId / ip が空の行は捨てる。候補として出せないうえ、空文字で 1 つの
// 「アカウント」に集約されると存在しない候補が最上位に出る。
func TestRank_SkipsIncompleteObservations(t *testing.T) {
	now := time.Now()
	got := iprelation.Rank(now, iprelation.DefaultHalfLife, []iprelation.Observation{
		obs("", "192.0.2.1", now, now, 2),
		obs("u1", "", now, now, 2),
		obs("u1", "192.0.2.1", now, now, 2),
	})
	require.Len(t, got, 1)
	assert.Equal(t, "u1", got[0].UserID)
	assert.Len(t, got[0].SharedIPs, 1)
}

// 根拠はそのまま返す。**「同時に使っていた」とは言わない**ので、双方の
// 最終観測をそれぞれ持たせる。
func TestRank_KeepsBothSidesOfEachMatch(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	targetLast := now.Add(-2 * day)
	candidateLast := now.Add(-10 * day)
	got := iprelation.Rank(now, iprelation.DefaultHalfLife, []iprelation.Observation{
		obs("u1", "192.0.2.1", targetLast, candidateLast, 7),
	})
	require.Len(t, got, 1)
	require.Len(t, got[0].SharedIPs, 1)
	p := got[0].SharedIPs[0]
	assert.Equal(t, targetLast, p.TargetLastSeen)
	assert.Equal(t, candidateLast, p.CandidateLastSeen)
	assert.Equal(t, 7, p.IPAccountCount)
	assert.InDelta(t, iprelation.Weight(10*day, iprelation.DefaultHalfLife), p.Weight, 1e-12)
	assert.InDelta(t, 10.0, p.ElapsedDays, 1e-9)
}
