package deliveryhealth

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/misc/jsdate"
)

func newTestStore(t *testing.T) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return NewStore(rdb), mr
}

func TestStore_FlushAndQueryRoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.Flush(ctx, []Delta{{
		Host:    "a.example",
		ByClass: map[OutcomeClass]int64{ClassSuccess: 10, ClassServerError: 2},
		Latency: [latencyBucketCount]int64{0: 8, 3: 4},
		LastError: &LastError{
			At: jsdate.NewTime(time.Unix(1_700_000_000, 0).UTC()), Class: ClassServerError,
			Status: 503, Message: "boom",
		},
	}}))

	got, err := s.Query(ctx, time.Hour)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "a.example", got[0].Host)
	assert.Equal(t, int64(10), got[0].Success)
	assert.Equal(t, int64(2), got[0].Failure, "serverError は失敗として数える")
	require.NotNil(t, got[0].LastError)
	assert.Equal(t, 503, got[0].LastError.Status)
}

// 複数ノードからの flush が合算されること。#2459 で queue role を分けたので、
// worker が複数いる構成が現実にありうる。
func TestStore_MergesFlushesFromMultipleNodes(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		require.NoError(t, s.Flush(ctx, []Delta{{
			Host:    "a.example",
			ByClass: map[OutcomeClass]int64{ClassSuccess: 5},
		}}))
	}

	got, err := s.Query(ctx, time.Hour)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, int64(15), got[0].Success)
}

// バケットは 1 分ごと。窓の外は合算しない。
func TestStore_WindowExcludesOlderBuckets(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	now := time.Unix(1_700_000_000, 0).UTC()
	s.SetClockForTest(func() time.Time { return now.Add(-30 * time.Minute) })
	require.NoError(t, s.Flush(ctx, []Delta{{Host: "old", ByClass: map[OutcomeClass]int64{ClassSuccess: 7}}}))

	s.SetClockForTest(func() time.Time { return now })
	require.NoError(t, s.Flush(ctx, []Delta{{Host: "new", ByClass: map[OutcomeClass]int64{ClassSuccess: 3}}}))

	// 5 分窓なら 30 分前の分は入らない。
	got, err := s.Query(ctx, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "new", got[0].Host)

	// 1 時間窓なら両方入る。
	got, err = s.Query(ctx, time.Hour)
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

// 窓は MaxWindow で頭打ちにする。これを超えると TTL で消えたバケットを
// 0 として合算し、「急に減った」ように見える。
func TestStore_ClampsWindow(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Flush(ctx, []Delta{{Host: "h", ByClass: map[OutcomeClass]int64{ClassSuccess: 1}}}))

	got, err := s.Query(ctx, 24*time.Hour)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, int64(1), got[0].Success)
}

// 失敗が多い順。運用者が最初に見たいのは壊れているホスト。
func TestStore_SortsByFailureDesc(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Flush(ctx, []Delta{
		{Host: "healthy", ByClass: map[OutcomeClass]int64{ClassSuccess: 100}},
		{Host: "broken", ByClass: map[OutcomeClass]int64{ClassTransport: 50}},
		{Host: "flaky", ByClass: map[OutcomeClass]int64{ClassSuccess: 10, ClassServerError: 5}},
	}))

	got, err := s.Query(ctx, time.Hour)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "broken", got[0].Host)
	assert.Equal(t, "flaky", got[1].Host)
	assert.Equal(t, "healthy", got[2].Host)
}

// lastError が読めなくても件数は返す。観測を全部落とさない。
func TestStore_QueryWithoutLastError(t *testing.T) {
	s, mr := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Flush(ctx, []Delta{{Host: "h", ByClass: map[OutcomeClass]int64{ClassSuccess: 1}}}))
	mr.Del(s.lastErrorKey())

	got, err := s.Query(ctx, time.Hour)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Nil(t, got[0].LastError)
	assert.Equal(t, int64(1), got[0].Success)
}

func TestStore_NilRedisIsNoOp(t *testing.T) {
	s := NewStore(nil)
	require.NoError(t, s.Flush(context.Background(), []Delta{{Host: "h"}}))
	got, err := s.Query(context.Background(), time.Hour)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestStore_FlushEmptyIsNoOp(t *testing.T) {
	s, mr := newTestStore(t)
	require.NoError(t, s.Flush(context.Background(), nil))
	assert.Empty(t, mr.Keys())
}

func TestParseField(t *testing.T) {
	cases := []struct {
		field   string
		host    string
		class   OutcomeClass
		latIdx  int
		wantOK  bool
		comment string
	}{
		{"a.example|success", "a.example", ClassSuccess, -1, true, "class counter"},
		{"a.example|lat|3", "a.example", "", 3, true, "latency bucket"},
		{"|success", "", "", -1, false, "空 host は捨てる"},
		{"a.example", "", "", -1, false, "区切りが足りない"},
		{"a|b|c|d", "", "", -1, false, "区切りが多い"},
		{"a.example|notlat|3", "", "", -1, false, "3 分割だが latency 印が違う"},
		{"a.example|lat|99", "", "", -1, false, "バケット範囲外"},
		{"a.example|lat|x", "", "", -1, false, "数値でない"},
	}
	for _, tc := range cases {
		t.Run(tc.comment, func(t *testing.T) {
			host, class, latIdx, ok := parseField(tc.field)
			assert.Equal(t, tc.wantOK, ok)
			if !tc.wantOK {
				return
			}
			assert.Equal(t, tc.host, host)
			assert.Equal(t, tc.class, class)
			assert.Equal(t, tc.latIdx, latIdx)
		})
	}
}

// 分位数はバケット上限を返す近似。正確な値ではないので、境界の期待値を固定して
// 「どのバケットに落ちたか」を検査する。
func TestApproxQuantileMs(t *testing.T) {
	var b [latencyBucketCount]int64
	// 全部 <=50ms
	b[0] = 100
	assert.Equal(t, int64(50), approxQuantileMs(&b, 0.50))
	assert.Equal(t, int64(50), approxQuantileMs(&b, 0.95))

	// 半分が <=50ms、半分が <=1000ms
	b = [latencyBucketCount]int64{}
	b[0] = 50
	b[4] = 50
	assert.Equal(t, int64(50), approxQuantileMs(&b, 0.50))
	assert.Equal(t, int64(1000), approxQuantileMs(&b, 0.95))

	// 計測上限超えは -1
	b = [latencyBucketCount]int64{}
	b[latencyBucketCount-1] = 10
	assert.Equal(t, int64(-1), approxQuantileMs(&b, 0.50))

	// 観測なしは 0
	b = [latencyBucketCount]int64{}
	assert.Equal(t, int64(0), approxQuantileMs(&b, 0.50))
	assert.Equal(t, int64(0), approxQuantileMs(nil, 0.50))
}

// 送信と受信でキー空間が混ざらないこと (#2471)。混ざると「配送できない host」と
// 「受信を拒否した host」が同じ行に足し込まれ、どちらの話か分からなくなる。
func TestStore_DirectionsUseSeparateKeyspaces(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	outbound := NewStoreForDirection(rdb, DirectionOutbound)
	inbound := NewStoreForDirection(rdb, DirectionInbound)

	require.NoError(t, outbound.Flush(ctx, []Delta{{Host: "a.example", ByClass: map[OutcomeClass]int64{ClassSuccess: 3}}}))
	require.NoError(t, inbound.Flush(ctx, []Delta{{Host: "b.example", ByClass: map[OutcomeClass]int64{ClassAccepted: 7}}}))

	got, err := outbound.Query(ctx, time.Hour)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "a.example", got[0].Host)

	got, err = inbound.Query(ctx, time.Hour)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "b.example", got[0].Host)
	assert.Equal(t, int64(7), got[0].Success, "受信側は accepted を成功に数える")
}

// 受信側の成功判定が Store 側にも効くこと。Aggregator とずれると記録と表示で
// 数字が食い違う。
func TestStore_InboundSuccessClassification(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	s := NewStoreForDirection(rdb, DirectionInbound)
	require.NoError(t, s.Flush(ctx, []Delta{{Host: "h", ByClass: map[OutcomeClass]int64{
		ClassAccepted: 10, ClassUnsupported: 2, ClassBlocked: 5,
	}}}))

	got, err := s.Query(ctx, time.Hour)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, int64(12), got[0].Success, "accepted + unsupported")
	assert.Equal(t, int64(5), got[0].Failure, "blocked は失敗側")
}
