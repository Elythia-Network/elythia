package bubbleversus

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/repository"
)

type savedReport struct {
	base   repository.BubbleVersusMatchBase
	slot   int
	report repository.BubbleVersusReport
}

type finished struct {
	base     repository.BubbleVersusMatchBase
	endedAt  time.Time
	winnerID *string
	reason   string
}

type fakeRecords struct {
	mu       sync.Mutex
	reports  []savedReport
	finishes []finished
	err      error
}

func (f *fakeRecords) SaveReport(base repository.BubbleVersusMatchBase, slot int, report repository.BubbleVersusReport) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, savedReport{base, slot, report})
	return f.err
}

func (f *fakeRecords) Finish(base repository.BubbleVersusMatchBase, endedAt time.Time, winnerID *string, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finishes = append(f.finishes, finished{base, endedAt, winnerID, reason})
	return f.err
}

func newRecordedService(t *testing.T) (*Service, *fakeRecords, *time.Time) {
	t.Helper()
	s, _, _, now := newTestService(t)
	rec := &fakeRecords{}
	s.SetRecordStore(rec)
	return s, rec, now
}

func intPtr(n int) *int { return &n }

// 先に終わった側の報告で、その報告と終局が記録される。勝った側が後から送る
// opponentEnded は報告だけが記録され、勝敗は変わらない (#3232)。
func TestRecord_GameOverThenOpponentEnded(t *testing.T) {
	s, rec, now := newRecordedService(t)
	ctx := context.Background()
	m := playing(t, s, now)
	require.NoError(t, s.Attack(ctx, "alice", m.ID, 4))

	_, err := s.SubmitReport(ctx, "bob", m.ID, Report{Score: 10, Frame: 900, Reason: ReasonGameOver, Logs: logsWithGarbage(3), GameVersion: intPtr(4)})
	require.NoError(t, err)
	require.Len(t, rec.reports, 1)
	r := rec.reports[0]
	assert.Equal(t, 1, r.slot, "bob は招待された側 (user2)")
	assert.Equal(t, 10, r.report.Score)
	assert.Equal(t, 900, r.report.Frame)
	assert.Equal(t, ReasonGameOver, r.report.Reason)
	require.NotNil(t, r.report.GameVersion)
	assert.Equal(t, 4, *r.report.GameVersion)
	assert.JSONEq(t, `[[40,0,200],[0,3,3]]`, string(r.report.Logs))
	assert.Equal(t, m.ID, r.base.ID)
	assert.Equal(t, "alice", r.base.User1ID)
	assert.Equal(t, "bob", r.base.User2ID)
	assert.Equal(t, m.Seed, r.base.Seed)
	assert.Equal(t, "normal", r.base.GameMode)
	assert.Equal(t, time.UnixMilli(m.StartAt).UTC(), r.base.StartedAt)

	require.Len(t, rec.finishes, 1)
	f := rec.finishes[0]
	require.NotNil(t, f.winnerID)
	assert.Equal(t, "alice", *f.winnerID)
	assert.Equal(t, ReasonGameOver, f.reason)
	assert.Equal(t, now.UTC(), f.endedAt)

	got, err := s.SubmitReport(ctx, "alice", m.ID, Report{Score: 50, Frame: 950, Reason: ReasonOpponentEnded, Logs: logsWithGarbage(), GameVersion: intPtr(4)})
	require.NoError(t, err)
	assert.Equal(t, "alice", *got.WinnerID, "勝敗は変わらない")
	assert.Equal(t, ReasonGameOver, got.Reason)
	require.Len(t, rec.reports, 2)
	assert.Equal(t, 0, rec.reports[1].slot)
	assert.Equal(t, ReasonOpponentEnded, rec.reports[1].report.Reason)
	assert.Len(t, rec.finishes, 1, "終局は 1 度だけ記録する")
}

// 終局前の opponentEnded は受けない。受けると、勝敗に効かない理由で報告を
// 済ませ、相手の切断の申告を通らなくできる。
func TestRecord_OpponentEndedBeforeEndIsRejected(t *testing.T) {
	s, rec, now := newRecordedService(t)
	ctx := context.Background()
	m := playing(t, s, now)
	_, err := s.SubmitReport(ctx, "alice", m.ID, Report{Reason: ReasonOpponentEnded})
	assert.ErrorIs(t, err, ErrInvalidReport)
	assert.Empty(t, rec.reports)
	assert.Empty(t, rec.finishes)

	got, err := s.Get(ctx, m.ID)
	require.NoError(t, err)
	assert.Nil(t, got.Players[0].Result, "報告済みにならない")
}

// 時間切れは両者の報告がそろうまで終局しない。先に来た報告は、その時点で記録する
// (記録を Redis に置かないため、ここで書かないと失われる)。
func TestRecord_TimeUpRecordsFirstReportBeforeEnd(t *testing.T) {
	s, rec, now := newRecordedService(t)
	ctx := context.Background()
	m := playing(t, s, now)
	*now = now.Add(TimeLimit)

	_, err := s.SubmitReport(ctx, "alice", m.ID, Report{Score: 30, Reason: ReasonTimeUp, Logs: holds(2)})
	require.NoError(t, err)
	require.Len(t, rec.reports, 1)
	assert.Empty(t, rec.finishes, "まだ終局していない")

	_, err = s.SubmitReport(ctx, "bob", m.ID, Report{Score: 20, Reason: ReasonTimeUp, Logs: holds(1)})
	require.NoError(t, err)
	require.Len(t, rec.reports, 2)
	require.Len(t, rec.finishes, 1)
	assert.Equal(t, "alice", *rec.finishes[0].winnerID)
	assert.Equal(t, ReasonTimeUp, rec.finishes[0].reason)
}

// 報告が拒まれたら何も記録しない (時間切れの早すぎる報告)。
func TestRecord_RejectedReportIsNotRecorded(t *testing.T) {
	s, rec, now := newRecordedService(t)
	ctx := context.Background()
	m := playing(t, s, now)
	_, err := s.SubmitReport(ctx, "alice", m.ID, Report{Score: 30, Reason: ReasonTimeUp})
	assert.ErrorIs(t, err, ErrNotYet)
	assert.Empty(t, rec.reports)
}

func TestRecord_Disconnected(t *testing.T) {
	s, rec, now := newRecordedService(t)
	ctx := context.Background()
	m := playing(t, s, now)
	*now = now.Add(DisconnectAfter)
	_, err := s.ClaimDisconnected(ctx, "alice", m.ID)
	require.NoError(t, err)
	assert.Empty(t, rec.reports, "切断の申告は報告ではない")
	require.Len(t, rec.finishes, 1)
	assert.Equal(t, "alice", *rec.finishes[0].winnerID)
	assert.Equal(t, ReasonDisconnected, rec.finishes[0].reason)
}

// 不正な報告は、申告の理由ではなく判定した後の理由で残す。
func TestRecord_InvalidReportKeepsJudgedReason(t *testing.T) {
	s, rec, now := newRecordedService(t)
	ctx := context.Background()
	m := playing(t, s, now)
	require.NoError(t, s.Attack(ctx, "alice", m.ID, 1))
	_, err := s.SubmitReport(ctx, "bob", m.ID, Report{Score: 10, Reason: ReasonGameOver, Logs: logsWithGarbage(2)})
	require.NoError(t, err)
	require.Len(t, rec.reports, 1)
	assert.Equal(t, ReasonInvalidReport, rec.reports[0].report.Reason)
	assert.Equal(t, ReasonInvalidReport, rec.finishes[0].reason)
}

// ロビーでの投了は記録を持たない。null ではなく空の記録として残す。
func TestRecord_NilLogsAreStoredAsEmpty(t *testing.T) {
	s, rec, now := newRecordedService(t)
	ctx := context.Background()
	m := playing(t, s, now)
	_, err := s.SubmitReport(ctx, "bob", m.ID, Report{Reason: ReasonSurrender})
	require.NoError(t, err)
	require.Len(t, rec.reports, 1)
	assert.JSONEq(t, `[]`, string(rec.reports[0].report.Logs))
	assert.Nil(t, rec.reports[0].report.GameVersion, "版を送らない古いクライアント")
}

func TestRecord_ReportBounds(t *testing.T) {
	s, rec, now := newRecordedService(t)
	ctx := context.Background()
	m := playing(t, s, now)
	for name, r := range map[string]Report{
		"score over int32": {Score: math.MaxInt32 + 1, Reason: ReasonGameOver},
		"frame over int32": {Frame: math.MaxInt32 + 1, Reason: ReasonGameOver},
		"version zero":     {Reason: ReasonGameOver, GameVersion: intPtr(0)},
		"version too big":  {Reason: ReasonGameOver, GameVersion: intPtr(MaxGameVersion + 1)},
	} {
		_, err := s.SubmitReport(ctx, "bob", m.ID, r)
		assert.ErrorIs(t, err, ErrInvalidReport, name)
	}
	assert.Empty(t, rec.reports)

	_, err := s.SubmitReport(ctx, "bob", m.ID, Report{Score: math.MaxInt32, Reason: ReasonGameOver, GameVersion: intPtr(MaxGameVersion)})
	require.NoError(t, err, "上限ちょうどは受ける")
}

// 記録に失敗しても報告は成功する。勝敗は Redis で確定しているので、失敗を
// 返すとクライアントの送り直しが「報告済み」で弾かれるだけになる。
func TestRecord_StoreFailureDoesNotFailReport(t *testing.T) {
	s, rec, now := newRecordedService(t)
	rec.err = errors.New("db down")
	ctx := context.Background()
	m := playing(t, s, now)
	got, err := s.SubmitReport(ctx, "bob", m.ID, Report{Reason: ReasonGameOver})
	require.NoError(t, err)
	assert.Equal(t, StatusEnded, got.Status)
	assert.Len(t, rec.reports, 1)
	assert.Len(t, rec.finishes, 1)
}

// 終局後の報告も石の数を見る。超えていれば記録の理由を invalidReport にする
// (勝敗は変えない)。
func TestRecord_LateReportWithTooManyStonesIsMarkedInvalid(t *testing.T) {
	s, rec, now := newRecordedService(t)
	ctx := context.Background()
	m := playing(t, s, now)
	require.NoError(t, s.Attack(ctx, "bob", m.ID, 1))
	_, err := s.SubmitReport(ctx, "bob", m.ID, Report{Score: 10, Reason: ReasonGameOver})
	require.NoError(t, err)

	got, err := s.SubmitReport(ctx, "alice", m.ID, Report{Score: 50, Reason: ReasonOpponentEnded, Logs: logsWithGarbage(2)})
	require.NoError(t, err)
	assert.Equal(t, "alice", *got.WinnerID, "勝敗は変えない")
	assert.Equal(t, ReasonGameOver, got.Reason)
	require.Len(t, rec.reports, 2)
	assert.Equal(t, ReasonInvalidReport, rec.reports[1].report.Reason, "受けた石が送られた数より多い")

	// 送られた数以内なら申告どおり。
	s2, rec2, now2 := newRecordedService(t)
	m2 := playing(t, s2, now2)
	require.NoError(t, s2.Attack(ctx, "bob", m2.ID, 2))
	_, err = s2.SubmitReport(ctx, "bob", m2.ID, Report{Reason: ReasonGameOver})
	require.NoError(t, err)
	_, err = s2.SubmitReport(ctx, "alice", m2.ID, Report{Reason: ReasonOpponentEnded, Logs: logsWithGarbage(2)})
	require.NoError(t, err)
	assert.Equal(t, ReasonOpponentEnded, rec2.reports[1].report.Reason)
}

// 記録の各操作の先頭 (フレームの差分) は 0 以上の整数だけ受ける。リプレイが
// 操作を当てる位置を決められなくなる記録を残さない。
func TestRecord_FrameDeltaMustBeNonNegativeInteger(t *testing.T) {
	s, rec, now := newRecordedService(t)
	ctx := context.Background()
	m := playing(t, s, now)
	for name, logs := range map[string][][]any{
		"negative": {{float64(-1), float64(0), float64(100)}},
		"fraction": {{float64(0.5), float64(0), float64(100)}},
	} {
		_, err := s.SubmitReport(ctx, "bob", m.ID, Report{Reason: ReasonGameOver, Logs: logs})
		assert.ErrorIs(t, err, ErrInvalidReport, name)
	}
	assert.Empty(t, rec.reports)
	_, err := s.SubmitReport(ctx, "bob", m.ID, Report{Reason: ReasonGameOver, Logs: [][]any{{float64(0), float64(0), float64(100)}, {float64(35), float64(1)}}})
	require.NoError(t, err, "0 と正の整数は受ける")
}
