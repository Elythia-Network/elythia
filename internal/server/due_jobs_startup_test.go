package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/queue"
)

// 起動で、期限の時刻にしか走らないジョブ (お知らせの自動アーカイブと引用の送り直し)
// の起動時の分を積む (#3994)。queue を回すプロセスだけが積む。積まないと、止まって
// いた間に期限の来た行と、Redis から失われた予定が拾われない。
func TestStartBackground_EnqueuesDueJobsAtStartup(t *testing.T) {
	for _, tc := range []struct {
		role config.ProcessRole
		want []string
	}{
		{config.RoleQueue, []string{queue.TaskTypeArchiveAnnouncements, queue.TaskTypeResendQuoteRequests}},
		{config.RoleBoth, []string{queue.TaskTypeArchiveAnnouncements, queue.TaskTypeResendQuoteRequests}},
		{config.RoleServer, nil},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			f := newStartOnceFixture(tc.role)
			drv := newFakeQueueDriver()
			f.srv.queueClient = queue.NewClient(drv)
			require.NoError(t, f.srv.StartBackground())
			t.Cleanup(func() { _ = f.srv.StopBackground(context.Background()) })

			var got []string
			for _, c := range drv.client.calls {
				got = append(got, c.taskType)
				assert.JSONEq(t, `{"atStartup":true}`, string(c.payload), c.taskType)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// queue の client が無い世代でも落ちない。
func TestEnqueueDueJobsAtStartup_NoClient(t *testing.T) {
	assert.NotPanics(t, (&Server{}).enqueueDueJobsAtStartup)
}

// 送り直しの枠のジョブの試し直しは、3 分の許容の内側で終わり、かつ許容いっぱいまで
// 続く (#3994)。許容か試し直しの片方だけを変えると落ちる。保険の cron の間隔は
// 許容より短い (どの許容の窓にも必ず 1 回入る)。
func TestResendSlotRetriesFitTheTolerance(t *testing.T) {
	tol := corefederation.QuoteRequestResendTolerance
	last := queue.ResendSlotLastTry()
	assert.Less(t, last, tol, "the last try runs after the slot is given up")
	assert.Greater(t, last+queue.ResendSlotRetryInterval, tol, "one more try would still fit in the tolerance")

	drv := newFakeQueueDriver()
	s := &Server{queueScheduler: queue.NewScheduler(drv)}
	s.registerSchedulerJobs()
	found := false
	for _, c := range drv.scheduler.calls {
		if c.taskType == queue.TaskTypeResendQuoteRequests {
			found = true
			require.Equal(t, "*/2 * * * *", c.cron)
			assert.Less(t, 2*time.Minute, tol)
		}
	}
	require.True(t, found, "the resend safety cron is not registered")
}
