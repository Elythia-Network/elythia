package stream

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seqInfoInspector returns the next QueueStatsInfo of deliver on each call
// and an empty one for the other queues.
type seqInfoInspector struct {
	deliver []QueueStatsInfo
	idx     int
}

func (s *seqInfoInspector) GetQueueInfo(qname string) (*QueueStatsInfo, error) {
	if qname != "deliver" || len(s.deliver) == 0 {
		return &QueueStatsInfo{}, nil
	}
	i := min(s.idx, len(s.deliver)-1)
	s.idx++
	info := s.deliver[i]
	return &info, nil
}

func deliverActiveSincePrevTick(t *testing.T, insp QueueInspector, ticks int) []int {
	t.Helper()
	pub := &capturePubSub{}
	p := NewQueueStatsPublisher(insp, pub, 0)
	out := make([]int, 0, ticks)
	for i := range ticks {
		p.tick()
		calls := pub.snapshot()
		require.Len(t, calls, i+1)
		var body struct {
			Deliver struct {
				ActiveSincePrevTick int `json:"activeSincePrevTick"`
			} `json:"deliver"`
		}
		require.NoError(t, json.Unmarshal(calls[i].payload, &body))
		out = append(out, body.Deliver.ActiveSincePrevTick)
	}
	return out
}

// TestQueueStatsPublisher_ActiveSincePrevTickUsesProcessedTotal pins #4220:
// once the completed bucket sits at its retention, the jobs processed since
// the previous tick still come from the growing ProcessedTotal.
func TestQueueStatsPublisher_ActiveSincePrevTickUsesProcessedTotal(t *testing.T) {
	insp := &seqInfoInspector{deliver: []QueueStatsInfo{
		{Completed: 30, ProcessedTotal: 100, HasProcessedTotal: true},
		{Completed: 30, ProcessedTotal: 112, HasProcessedTotal: true},
		{Completed: 30, ProcessedTotal: 112, HasProcessedTotal: true},
		{Completed: 30, ProcessedTotal: 150, HasProcessedTotal: true},
	}}

	assert.Equal(t, []int{0, 12, 0, 38}, deliverActiveSincePrevTick(t, insp, 4))
}

// TestQueueStatsPublisher_ActiveSincePrevTickKeepsTotalAcrossReadFailure
// pins that a tick that could not read the processed total (a transient
// Redis error) keeps the previous total, so the next tick reports the jobs
// of both ticks instead of 0 twice (#4220).
func TestQueueStatsPublisher_ActiveSincePrevTickKeepsTotalAcrossReadFailure(t *testing.T) {
	insp := &seqInfoInspector{deliver: []QueueStatsInfo{
		{Completed: 30, ProcessedTotal: 100, HasProcessedTotal: true},
		{Completed: 30},
		{Completed: 30, ProcessedTotal: 120, HasProcessedTotal: true},
	}}

	assert.Equal(t, []int{0, 0, 20}, deliverActiveSincePrevTick(t, insp, 3))
}

// TestQueueStatsPublisher_ActiveSincePrevTickSkipsCounterSwitch pins that a
// tick whose counter switches from Completed to ProcessedTotal reports 0
// instead of a difference of two unrelated numbers (#4220). The opposite
// switch is a failed read and keeps the total (the test above).
func TestQueueStatsPublisher_ActiveSincePrevTickSkipsCounterSwitch(t *testing.T) {
	insp := &seqInfoInspector{deliver: []QueueStatsInfo{
		{Completed: 10},
		{Completed: 12},
		{Completed: 14, ProcessedTotal: 500, HasProcessedTotal: true},
		{Completed: 16, ProcessedTotal: 503, HasProcessedTotal: true},
	}}

	assert.Equal(t, []int{0, 2, 0, 3}, deliverActiveSincePrevTick(t, insp, 4))
}
