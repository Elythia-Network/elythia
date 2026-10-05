package fedrule

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/testutil"
)

func TestHitStore(t *testing.T) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		t.Skip("Redis unavailable:", err)
	}
	defer tr.Teardown(ctx)

	now := time.Date(2026, 9, 29, 12, 30, 0, 0, time.UTC)
	s := NewHitStore(tr.Client)
	s.SetClockForTest(func() time.Time { return now })

	hits := []Hit{
		{RuleID: "a", Host: "x.example", Subject: "s1", Kind: "Note", At: now.Add(-25 * time.Hour)}, // 窓の外
		{RuleID: "a", Host: "x.example", Subject: "s2", Kind: "Note", At: now.Add(-3 * time.Hour)},
		{RuleID: "a", Host: "x.example", Subject: "s3", Kind: "Note", Applied: true, At: now},
		{RuleID: "b", Host: "y.example", Subject: "s4", Kind: "Follow", At: now},
	}
	require.NoError(t, s.write(ctx, hits))

	counts, err := s.Counts(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"a": 2, "b": 1}, counts)

	samples, err := s.Samples(ctx, "a", 0)
	require.NoError(t, err)
	require.Len(t, samples, 3)
	assert.Equal(t, "s3", samples[0].Subject, "newest first")
	assert.True(t, samples[0].Applied)
	samples, err = s.Samples(ctx, "a", 1)
	require.NoError(t, err)
	assert.Len(t, samples, 1)

	// 保持は MaxSamples 件まで。
	var many []Hit
	for i := 0; i < MaxSamples+10; i++ {
		many = append(many, Hit{RuleID: "c", At: now})
	}
	require.NoError(t, s.write(ctx, many))
	samples, err = s.Samples(ctx, "c", 1000)
	require.NoError(t, err)
	assert.Len(t, samples, MaxSamples)

	// Forget は件数と記録の両方を消す。
	require.NoError(t, s.Forget(ctx, "a"))
	counts, err = s.Counts(ctx)
	require.NoError(t, err)
	assert.NotContains(t, counts, "a")
	assert.EqualValues(t, 1, counts["b"])
	samples, err = s.Samples(ctx, "a", 0)
	require.NoError(t, err)
	assert.Empty(t, samples)

	// Record は非同期に書き、長い値を切る。
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.Start(runCtx)
	s.Record(Hit{RuleID: "d", Host: strings.Repeat("h", 1000), Subject: strings.Repeat("あ", 1000), Kind: "Note", At: now})
	require.Eventually(t, func() bool {
		got, _ := s.Samples(ctx, "d", 0)
		return len(got) == 1
	}, 5*time.Second, 20*time.Millisecond)
	got, err := s.Samples(ctx, "d", 0)
	require.NoError(t, err)
	assert.Len(t, got[0].Host, 256)
	assert.LessOrEqual(t, len(got[0].Subject), 1024)
	assert.True(t, strings.HasPrefix(got[0].Subject, "あ"))
	assert.NotContains(t, got[0].Subject, "�")
}

// Redis が無い構成でも呼べる (記録しない)。
func TestHitStore_Nil(t *testing.T) {
	ctx := context.Background()
	var s *HitStore
	s.Record(Hit{})
	s.Start(ctx)
	counts, err := s.Counts(ctx)
	assert.NoError(t, err)
	assert.Empty(t, counts)
	samples, err := s.Samples(ctx, "a", 0)
	assert.NoError(t, err)
	assert.Empty(t, samples)
	assert.NoError(t, s.Forget(ctx, "a"))

	s = NewHitStore(nil)
	s.Record(Hit{})
	s.Start(ctx)
	assert.Equal(t, "ab", truncate("ab", 5))
}

// キューが溢れたら捨てて、受信を止めない。
func TestHitStore_RecordNeverBlocks(t *testing.T) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		t.Skip("Redis unavailable:", err)
	}
	defer tr.Teardown(ctx)
	s := NewHitStore(tr.Client) // Start しない = 誰も読まない
	done := make(chan struct{})
	go func() {
		for i := 0; i < hitQueueSize*2; i++ {
			s.Record(Hit{RuleID: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Record blocked on a full queue")
	}
	assert.Len(t, s.queue, hitQueueSize)
}
