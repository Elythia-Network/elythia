package plugintest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/plugin"
)

// 本体と同じく、同じジョブ名の 2 回目と、本体が受け付けない名前 (`_` で始まる
// 予約名など) を登録の失敗として記録する (#3369)。
func TestFakeJobs_RefusesWhatTheHostRefuses(t *testing.T) {
	noop := func(context.Context, json.RawMessage) error { return nil }
	for _, tc := range []struct {
		name  string
		names []string
		err   string
	}{
		{"distinct names", []string{"a", "b-1", "c_2"}, ""},
		{"duplicate", []string{"a", "a"}, `ジョブ "a" を2回登録しています`},
		{"peer reserved", []string{queue.PluginPeerJobName}, `ジョブ名 "_peer" は使えません`},
		{"notification reserved", []string{queue.PluginNotificationJobName}, `ジョブ名 "_notification" は使えません`},
		{"colon", []string{"a:b"}, `ジョブ名 "a:b" は使えません`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &fakeJobs{set: &JobSet{Handlers: map[string]plugin.JobHandler{}}}
			for _, n := range tc.names {
				j.Handle(n, noop)
			}
			if tc.err == "" {
				assert.NoError(t, j.err)
				assert.Len(t, j.set.Handlers, len(tc.names))
				return
			}
			assert.ErrorContains(t, j.err, tc.err)
		})
	}
}

// Schedule も Handle と同じ規則で名前を見る (#3369)。
func TestFakeJobs_ScheduleRefusesWhatTheHostRefuses(t *testing.T) {
	for _, name := range []string{queue.PluginNotificationJobName, "a:b"} {
		j := &fakeJobs{set: &JobSet{Handlers: map[string]plugin.JobHandler{}}}
		j.Schedule("* * * * *", name, nil)
		assert.ErrorContains(t, j.err, "は使えません", name)
		assert.Empty(t, j.set.Schedules, name)
	}
	j := &fakeJobs{set: &JobSet{Handlers: map[string]plugin.JobHandler{}}}
	j.Schedule("* * * * *", "sync", nil)
	assert.NoError(t, j.err)
	assert.Len(t, j.set.Schedules, 1)
}

// 写した規則が、本体 (internal/queue) の規則と一致すること。
func TestValidJobNameMatchesTheHost(t *testing.T) {
	for _, name := range []string{"a", "A1", "a-b", "a_b", "1", "_peer", "_notification", "_x", "-a", "", "a:b", "a b", "あ", "a.b",
		strings.Repeat("a", 64), strings.Repeat("a", 256), strings.Repeat("a", 1024)} {
		assert.Equal(t, queue.ValidPluginJobName(name), validJobName(name), name)
	}
}
