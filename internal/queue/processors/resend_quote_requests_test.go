package processors

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

type fakeQuoteResender struct {
	at          []time.Time
	n           int
	err         error
	upcomingAt  []time.Time
	upcomingErr error
}

func (f *fakeQuoteResender) ResendPending(now time.Time) (int, error) {
	f.at = append(f.at, now)
	return f.n, f.err
}

func (f *fakeQuoteResender) ScheduleUpcomingResends(now time.Time) (int, error) {
	f.upcomingAt = append(f.upcomingAt, now)
	return 0, f.upcomingErr
}

// 送り直し (#3238) は今の時刻で呼ぶ。期限の時刻に 1 回しか走らないので、探せな
// かったときは queue に試し直させる (#3994)。
func TestResendQuoteRequests(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		n       int
		err     error
		wantErr bool
	}{{n: 0}, {n: 2}, {err: errors.New("db down"), wantErr: true}} {
		f := &fakeQuoteResender{n: tc.n, err: tc.err}
		p := NewResendQuoteRequestsProcessor(f)
		p.now = func() time.Time { return now }
		err := p.Handle(context.Background(), driver.RawTask{TypeName: "test"})
		if tc.wantErr {
			assert.Error(t, err)
		} else {
			require.NoError(t, err)
		}
		assert.Equal(t, []time.Time{now}, f.at)
		assert.Empty(t, f.upcomingAt, "Handle does not reschedule")
	}
}

// 起動時は、期限の来たものを処理してから、まだ来ていない予定のジョブを積み直す (#3994)。
func TestResendQuoteRequests_RunAtStartup(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	f := &fakeQuoteResender{}
	p := NewResendQuoteRequestsProcessor(f)
	p.now = func() time.Time { return now }
	require.NoError(t, p.RunAtStartup(context.Background()))
	assert.Equal(t, []time.Time{now}, f.at)
	assert.Equal(t, []time.Time{now}, f.upcomingAt)

	// 期限の来た分が失敗しても、まだ来ていない予定の積み直しは流す (#3994)。
	f = &fakeQuoteResender{err: errors.New("db down")}
	assert.Error(t, NewResendQuoteRequestsProcessor(f).RunAtStartup(context.Background()))
	assert.Len(t, f.upcomingAt, 1, "upcoming resends are scheduled even when the due ones failed")

	// 両方が失敗したら、両方を返す。
	dueErr, upErr := errors.New("due down"), errors.New("upcoming down")
	f = &fakeQuoteResender{err: dueErr, upcomingErr: upErr}
	err := NewResendQuoteRequestsProcessor(f).RunAtStartup(context.Background())
	assert.ErrorIs(t, err, dueErr)
	assert.ErrorIs(t, err, upErr)

	f = &fakeQuoteResender{upcomingErr: errors.New("db down")}
	assert.Error(t, NewResendQuoteRequestsProcessor(f).RunAtStartup(context.Background()))
}

// 起動時に積むジョブ (payload atStartup) は RunAtStartup を流す (#3994)。
func TestResendQuoteRequests_HandleDispatchesTheStartupJob(t *testing.T) {
	f := &fakeQuoteResender{}
	p := NewResendQuoteRequestsProcessor(f)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{TypeName: "t", Body: []byte(`{"atStartup":true}`)}))
	assert.Len(t, f.at, 1)
	assert.Len(t, f.upcomingAt, 1)

	f = &fakeQuoteResender{}
	p = NewResendQuoteRequestsProcessor(f)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{TypeName: "t", Body: []byte(`{"noteId":"n1","at":1}`)}))
	assert.Len(t, f.at, 1)
	assert.Empty(t, f.upcomingAt)
}

type fakeRequeuer struct {
	got []string
	err error
}

func (r *fakeRequeuer) RequeueQuoteRequestResend(noteID string, at time.Time) error {
	r.got = append(r.got, noteID+" "+at.UTC().Format(time.RFC3339Nano))
	return r.err
}

// 期限の前に走った枠のジョブ (管理画面の promote) は、期限の時刻に積み直す (#3994)。
// 期限の来ている別の行の処理はそのまま行う。
func TestResendQuoteRequests_RequeuesAnEarlyJob(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	at := now.Add(time.Minute)
	slot := func(at time.Time) driver.Task {
		return driver.RawTask{TypeName: "t", Body: []byte(fmt.Sprintf(`{"noteId":"n1","at":%d}`, at.UnixMilli()))}
	}
	for _, tc := range []struct {
		name string
		task driver.Task
		want []string
	}{
		{"early", slot(at), []string{"n1 " + at.Format(time.RFC3339Nano)}},
		{"on time", slot(now), nil},
		{"late", slot(now.Add(-time.Minute)), nil},
		{"cron", driver.RawTask{TypeName: "t"}, nil},
		{"no note", driver.RawTask{TypeName: "t", Body: []byte(fmt.Sprintf(`{"at":%d}`, at.UnixMilli()))}, nil},
		{"broken", driver.RawTask{TypeName: "t", Body: []byte(`{`)}, nil},
		// 枠は最初の送信から 10 分の間にしか無い。それより先を指すものは積み直さない。
		{"too far ahead", slot(now.Add(maxEarlyRequeueAhead + time.Minute)), nil},
		{"far future", driver.RawTask{TypeName: "t", Body: []byte(`{"noteId":"n1","at":8640000000000000}`)}, nil},
		{"last slot ahead", slot(now.Add(10 * time.Minute)), []string{"n1 " + now.Add(10*time.Minute).Format(time.RFC3339Nano)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeQuoteResender{}
			r := &fakeRequeuer{}
			p := NewResendQuoteRequestsProcessor(f)
			p.SetRequeuer(r)
			p.now = func() time.Time { return now }
			require.NoError(t, p.Handle(context.Background(), tc.task))
			assert.Equal(t, tc.want, r.got)
			assert.Len(t, f.at, 1, "the due rows are still handled")
		})
	}

	// 積み直せなくてもジョブは失敗させない (保険の cron が拾う)。積み直す先が無くても動く。
	f := &fakeQuoteResender{}
	p := NewResendQuoteRequestsProcessor(f)
	p.SetRequeuer(&fakeRequeuer{err: errors.New("redis down")})
	p.now = func() time.Time { return now }
	require.NoError(t, p.Handle(context.Background(), slot(at)))
	p = NewResendQuoteRequestsProcessor(f)
	p.now = func() time.Time { return now }
	require.NoError(t, p.Handle(context.Background(), slot(at)))
}
