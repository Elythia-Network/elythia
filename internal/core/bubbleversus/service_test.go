package bubbleversus

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

type event struct {
	to, typ string
	body    any
}

type capturePub struct {
	mu     sync.Mutex
	events []event
}

func (p *capturePub) PublishInvited(target string, _ *model.User, m *Match) {
	p.add(event{to: "user:" + target, typ: "invited", body: m.ID})
}
func (p *capturePub) PublishUser(uid, typ string, body any) {
	p.add(event{to: "user:" + uid, typ: typ, body: body})
}
func (p *capturePub) PublishMatch(mid, typ string, body any) {
	p.add(event{to: "match:" + mid, typ: typ, body: body})
}
func (p *capturePub) add(e event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
}
func (p *capturePub) types(to string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, e := range p.events {
		if e.to == to {
			out = append(out, e.typ)
		}
	}
	return out
}

type fakeBlocks struct {
	pairs map[[2]string]bool
	err   error
}

func (f fakeBlocks) IsBlocked(a, b string) (bool, error) { return f.pairs[[2]string{a, b}], f.err }

var t0 = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

func newTestService(t *testing.T) (*Service, *capturePub, *miniredis.Miniredis, *time.Time) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	pub := &capturePub{}
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	s := NewService(rdb, pub, fakeBlocks{}, gen)
	now := t0
	s.SetClockForTest(func() time.Time { return now })
	return s, pub, mr, &now
}

var (
	alice = &model.User{ID: "alice", Username: "alice"}
	bob   = &model.User{ID: "bob", Username: "bob"}
	carol = &model.User{ID: "carol", Username: "carol"}
)

// accepted: 招待して受けた状態の対局を作る。
func accepted(t *testing.T, s *Service) *Match {
	t.Helper()
	ctx := context.Background()
	m, err := s.Invite(ctx, alice, bob, "normal")
	require.NoError(t, err)
	m, err = s.Accept(ctx, "bob", m.ID)
	require.NoError(t, err)
	return m
}

// playing: 両者が準備して始まった状態の対局を作り、開始時刻まで進める。
func playing(t *testing.T, s *Service, now *time.Time) *Match {
	t.Helper()
	ctx := context.Background()
	m := accepted(t, s)
	_, err := s.Ready(ctx, "alice", m.ID, true)
	require.NoError(t, err)
	m, err = s.Ready(ctx, "bob", m.ID, true)
	require.NoError(t, err)
	*now = now.Add(Countdown)
	return m
}

func TestInvite(t *testing.T) {
	s, pub, _, _ := newTestService(t)
	ctx := context.Background()

	_, err := s.Invite(ctx, alice, alice, "normal")
	assert.ErrorIs(t, err, ErrYourself)
	host := "remote.example"
	_, err = s.Invite(ctx, alice, &model.User{ID: "r", Host: &host}, "normal")
	assert.ErrorIs(t, err, ErrRemoteUser)
	_, err = s.Invite(ctx, alice, bob, "normal-bouncy")
	assert.ErrorIs(t, err, ErrInvalidGameMode)

	m, err := s.Invite(ctx, alice, bob, "square-friction")
	require.NoError(t, err)
	assert.Equal(t, StatusInvited, m.Status)
	assert.Empty(t, m.Seed, "the seed is decided when accepted")
	assert.Equal(t, []string{"invited"}, pub.types("user:bob"))

	// 同じ相手への同じ設定の未回答の招待は増やさない。
	again, err := s.Invite(ctx, alice, bob, "square-friction")
	require.NoError(t, err)
	assert.Equal(t, m.ID, again.ID)

	// 設定を変えて誘い直すと、古い招待は取り消される。
	changed, err := s.Invite(ctx, alice, bob, "normal")
	require.NoError(t, err)
	assert.NotEqual(t, m.ID, changed.ID)
	_, err = s.Get(ctx, m.ID)
	assert.ErrorIs(t, err, ErrNoSuchMatch)
	assert.Contains(t, pub.types("user:bob"), "canceled")

	list, err := s.Invitations(ctx, "bob")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, changed.ID, list[0].ID)
	assert.Equal(t, "normal", list[0].GameMode)
	list, err = s.Invitations(ctx, "alice")
	require.NoError(t, err)
	assert.Empty(t, list)
}

// どちらの向きでもブロックしていれば招待できない。判定できなければ通さない。
func TestInvite_Blocks(t *testing.T) {
	s, _, _, _ := newTestService(t)
	ctx := context.Background()
	s.blocks = fakeBlocks{pairs: map[[2]string]bool{{"bob", "alice"}: true}}
	_, err := s.Invite(ctx, alice, bob, "normal")
	assert.ErrorIs(t, err, ErrBlocked)
	s.blocks = fakeBlocks{pairs: map[[2]string]bool{{"alice", "bob"}: true}}
	_, err = s.Invite(ctx, alice, bob, "normal")
	assert.ErrorIs(t, err, ErrBlocked)
	s.blocks = fakeBlocks{err: errors.New("db down")}
	_, err = s.Invite(ctx, alice, bob, "normal")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrBlocked)

	// 招待の後にブロックしたら受けられない。
	s.blocks = fakeBlocks{}
	m, err := s.Invite(ctx, alice, bob, "normal")
	require.NoError(t, err)
	s.blocks = fakeBlocks{pairs: map[[2]string]bool{{"bob", "alice"}: true}}
	_, err = s.Accept(ctx, "bob", m.ID)
	assert.ErrorIs(t, err, ErrBlocked)
}

// 期限が切れた招待は一覧から消える。
func TestInvitations_Expire(t *testing.T) {
	s, _, mr, _ := newTestService(t)
	ctx := context.Background()
	_, err := s.Invite(ctx, alice, bob, "normal")
	require.NoError(t, err)
	mr.FastForward(InviteTTL + time.Second)
	list, err := s.Invitations(ctx, "bob")
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestAcceptDeclineCancel(t *testing.T) {
	s, pub, _, _ := newTestService(t)
	ctx := context.Background()
	m, err := s.Invite(ctx, alice, bob, "normal")
	require.NoError(t, err)

	_, err = s.Accept(ctx, "alice", m.ID)
	assert.ErrorIs(t, err, ErrNotParticipant, "the inviter cannot accept")
	_, err = s.Accept(ctx, "carol", m.ID)
	assert.ErrorIs(t, err, ErrNotParticipant)
	_, err = s.Accept(ctx, "bob", "missing")
	assert.ErrorIs(t, err, ErrNoSuchMatch)

	m, err = s.Accept(ctx, "bob", m.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusAccepted, m.Status)
	assert.Len(t, m.Seed, 32)
	assert.Contains(t, pub.types("user:alice"), "accepted")
	assert.Contains(t, pub.types("match:"+m.ID), "accepted")
	list, err := s.Invitations(ctx, "bob")
	require.NoError(t, err)
	assert.Empty(t, list, "an accepted invitation leaves the list")
	_, err = s.Accept(ctx, "bob", m.ID)
	assert.ErrorIs(t, err, ErrInvalidState)

	// 受けた後は、どちらからでも始まる前なら抜けられる。
	require.NoError(t, s.Cancel(ctx, "bob", m.ID))
	_, err = s.Get(ctx, m.ID)
	assert.ErrorIs(t, err, ErrNoSuchMatch)
	assert.Contains(t, pub.types("user:alice"), "canceled")

	// 辞退は受ける側だけ。取り消しは招待した側だけ。
	m2, err := s.Invite(ctx, alice, carol, "normal")
	require.NoError(t, err)
	assert.ErrorIs(t, s.Decline(ctx, "alice", m2.ID), ErrNotParticipant)
	assert.ErrorIs(t, s.Cancel(ctx, "carol", m2.ID), ErrNotParticipant)
	require.NoError(t, s.Decline(ctx, "carol", m2.ID))
	assert.Contains(t, pub.types("user:alice"), "declined")
	assert.ErrorIs(t, s.Decline(ctx, "carol", m2.ID), ErrNoSuchMatch)

	m3, err := s.Invite(ctx, alice, carol, "normal")
	require.NoError(t, err)
	require.NoError(t, s.Cancel(ctx, "alice", m3.ID))
	list, err = s.Invitations(ctx, "carol")
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestReady(t *testing.T) {
	s, pub, _, now := newTestService(t)
	ctx := context.Background()
	m := accepted(t, s)

	_, err := s.Ready(ctx, "carol", m.ID, true)
	assert.ErrorIs(t, err, ErrNotParticipant)
	m, err = s.Ready(ctx, "alice", m.ID, true)
	require.NoError(t, err)
	assert.Equal(t, StatusAccepted, m.Status)
	assert.NotContains(t, pub.types("match:"+m.ID), "started")

	m, err = s.Ready(ctx, "bob", m.ID, true)
	require.NoError(t, err)
	assert.Equal(t, StatusPlaying, m.Status)
	assert.Equal(t, now.Add(Countdown).UnixMilli(), m.StartAt)
	assert.Equal(t, []string{"accepted", "readyStates", "readyStates", "started"}, pub.types("match:"+m.ID))

	// 始まった後は抜けられない (投了する)。
	assert.ErrorIs(t, s.Cancel(ctx, "alice", m.ID), ErrInvalidState)
	_, err = s.Ready(ctx, "alice", m.ID, false)
	assert.ErrorIs(t, err, ErrInvalidState)
}

func TestAttack(t *testing.T) {
	s, pub, _, now := newTestService(t)
	ctx := context.Background()
	m := accepted(t, s)
	_, err := s.Ready(ctx, "alice", m.ID, true)
	require.NoError(t, err)
	_, err = s.Ready(ctx, "bob", m.ID, true)
	require.NoError(t, err)

	assert.ErrorIs(t, s.Attack(ctx, "alice", m.ID, 1), ErrInvalidState, "not before the countdown ends")
	*now = now.Add(Countdown)
	assert.ErrorIs(t, s.Attack(ctx, "alice", m.ID, 0), ErrInvalidReport)
	assert.ErrorIs(t, s.Attack(ctx, "carol", m.ID, 1), ErrNotParticipant)
	require.NoError(t, s.Attack(ctx, "alice", m.ID, 3))
	require.NoError(t, s.Attack(ctx, "alice", m.ID, 2))
	got, err := s.Get(ctx, m.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 5, got.Players[0].Sent)
	assert.EqualValues(t, 0, got.Players[1].Sent)
	assert.Contains(t, pub.types("match:"+m.ID), "attack")

	// 上限を超える攻撃は捨てずに切り詰める (大きなコンボでも 1 個も届かないことはない)。
	require.NoError(t, s.Attack(ctx, "bob", m.ID, MaxAttackPerMessage+10))
	got, err = s.Get(ctx, m.ID)
	require.NoError(t, err)
	assert.EqualValues(t, MaxAttackPerMessage, got.Players[1].Sent)
}

// 両者が同時に攻撃しても数が失われない (WATCH で読み直す)。
func TestAttack_Concurrent(t *testing.T) {
	s, _, _, now := newTestService(t)
	ctx := context.Background()
	m := playing(t, s, now)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); assert.NoError(t, s.Attack(ctx, "alice", m.ID, 1)) }()
		go func() { defer wg.Done(); assert.NoError(t, s.Attack(ctx, "bob", m.ID, 2)) }()
	}
	wg.Wait()
	got, err := s.Get(ctx, m.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 20, got.Players[0].Sent)
	assert.EqualValues(t, 40, got.Players[1].Sent)
}

// 相手が 30 秒黙っていれば、残った側が勝ちを申告できる。
func TestClaimDisconnected(t *testing.T) {
	s, pub, _, now := newTestService(t)
	ctx := context.Background()
	m := playing(t, s, now)

	*now = now.Add(DisconnectAfter - time.Second)
	_, err := s.ClaimDisconnected(ctx, "alice", m.ID)
	assert.ErrorIs(t, err, ErrNotYet)

	// bob が何か送れば数え直し。
	s.State(ctx, "bob", m.ID, []byte(`{}`))
	*now = now.Add(DisconnectAfter - time.Second)
	_, err = s.ClaimDisconnected(ctx, "alice", m.ID)
	assert.ErrorIs(t, err, ErrNotYet)

	*now = now.Add(time.Second)
	got, err := s.ClaimDisconnected(ctx, "alice", m.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusEnded, got.Status)
	require.NotNil(t, got.WinnerID)
	assert.Equal(t, "alice", *got.WinnerID)
	assert.Equal(t, ReasonDisconnected, got.Reason)
	assert.Contains(t, pub.types("match:"+m.ID), "ended")

	_, err = s.ClaimDisconnected(ctx, "bob", m.ID)
	assert.ErrorIs(t, err, ErrInvalidState)
}

func logsWithGarbage(counts ...float64) [][]any {
	out := [][]any{{float64(40), float64(0), float64(200)}}
	for _, n := range counts {
		out = append(out, []any{float64(0), float64(3), n})
	}
	return out
}

// 先にゲームオーバーになった側の負け。勝った側の報告は後から残るだけ。
func TestSubmitReport_GameOver(t *testing.T) {
	s, pub, _, now := newTestService(t)
	ctx := context.Background()
	m := playing(t, s, now)
	require.NoError(t, s.Attack(ctx, "alice", m.ID, 4))

	got, err := s.SubmitReport(ctx, "bob", m.ID, Report{Score: 10, Frame: 900, Reason: ReasonGameOver, Logs: logsWithGarbage(3)})
	require.NoError(t, err)
	assert.Equal(t, StatusEnded, got.Status)
	require.NotNil(t, got.WinnerID)
	assert.Equal(t, "alice", *got.WinnerID)
	require.NotNil(t, got.Players[1].Result)
	assert.EqualValues(t, 3, got.Players[1].Result.Garbage)

	got, err = s.SubmitReport(ctx, "alice", m.ID, Report{Score: 50, Frame: 950, Reason: ReasonGameOver, Logs: logsWithGarbage()})
	require.NoError(t, err)
	assert.Equal(t, "alice", *got.WinnerID, "the winner does not change")
	assert.NotNil(t, got.Players[0].Result)
	ended := 0
	for _, typ := range pub.types("match:" + m.ID) {
		if typ == "ended" {
			ended++
		}
	}
	assert.Equal(t, 1, ended, "ended is published once")

	_, err = s.SubmitReport(ctx, "alice", m.ID, Report{Reason: ReasonGameOver})
	assert.ErrorIs(t, err, ErrInvalidState, "one report per player")
}

// 記録の石が相手の送った数より多い報告は不正として、報告した側の負け。
func TestSubmitReport_TooManyStones(t *testing.T) {
	s, _, _, now := newTestService(t)
	ctx := context.Background()
	m := playing(t, s, now)
	require.NoError(t, s.Attack(ctx, "alice", m.ID, 2))
	got, err := s.SubmitReport(ctx, "bob", m.ID, Report{Score: 10, Reason: ReasonTimeUp, Logs: logsWithGarbage(2, 1)})
	// timeUp はまだ早いが、不正の判定が先。
	require.NoError(t, err)
	assert.Equal(t, StatusEnded, got.Status)
	assert.Equal(t, "alice", *got.WinnerID)
	assert.Equal(t, ReasonInvalidReport, got.Reason)
}

func TestSubmitReport_TimeUp(t *testing.T) {
	for _, tc := range []struct {
		name         string
		alice, bob   int64
		wantWinnerID *string
	}{
		{"higher score wins", 30, 20, strPtr("alice")},
		{"lower score loses", 10, 20, strPtr("bob")},
		{"draw", 20, 20, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _, now := newTestService(t)
			ctx := context.Background()
			m := playing(t, s, now)
			_, err := s.SubmitReport(ctx, "alice", m.ID, Report{Score: tc.alice, Reason: ReasonTimeUp})
			assert.ErrorIs(t, err, ErrNotYet)

			*now = now.Add(TimeLimit - TimeUpSkew)
			got, err := s.SubmitReport(ctx, "alice", m.ID, Report{Score: tc.alice, Reason: ReasonTimeUp})
			require.NoError(t, err)
			assert.Equal(t, StatusPlaying, got.Status, "waits for the other report")
			got, err = s.SubmitReport(ctx, "bob", m.ID, Report{Score: tc.bob, Reason: ReasonTimeUp})
			require.NoError(t, err)
			assert.Equal(t, StatusEnded, got.Status)
			assert.Equal(t, ReasonTimeUp, got.Reason)
			assert.Equal(t, tc.wantWinnerID, got.WinnerID)
		})
	}
}

func TestSubmitReport_Invalid(t *testing.T) {
	s, _, _, now := newTestService(t)
	ctx := context.Background()
	m := playing(t, s, now)
	for name, r := range map[string]Report{
		"reason":       {Reason: "win"},
		"score":        {Reason: ReasonGameOver, Score: -1},
		"frame":        {Reason: ReasonGameOver, Frame: -1},
		"short log":    {Reason: ReasonGameOver, Logs: [][]any{{float64(1)}}},
		"string op":    {Reason: ReasonGameOver, Logs: [][]any{{float64(1), "3", float64(1)}}},
		"garbage args": {Reason: ReasonGameOver, Logs: [][]any{{float64(1), float64(3)}}},
		"negative":     {Reason: ReasonGameOver, Logs: [][]any{{float64(1), float64(3), float64(-1)}}},
		"zero stones":  {Reason: ReasonGameOver, Logs: [][]any{{float64(1), float64(3), float64(0)}}},
		"over a drop":  {Reason: ReasonGameOver, Logs: [][]any{{float64(1), float64(3), float64(MaxGarbagePerDrop + 1)}}},
		"fractional":   {Reason: ReasonGameOver, Logs: [][]any{{float64(1), float64(3), 1.5}}},
		// int64 へ変換すると負に化ける大きさ (合計が相手の送った数を下回って素通りする)。
		"huge": {Reason: ReasonGameOver, Logs: [][]any{{float64(1), float64(3), 1e300}}},
		// 読まない要素にも任意の値を載せさせない (記録は DB に残すので、形を締める)。
		"payload in drop": {Reason: ReasonGameOver, Logs: [][]any{{float64(1), float64(0), "x"}}},
		"extra element":   {Reason: ReasonGameOver, Logs: [][]any{{float64(1), float64(0), float64(3), float64(4)}}},
		"hold with arg":   {Reason: ReasonGameOver, Logs: [][]any{{float64(1), float64(1), float64(3)}}},
		"drop without x":  {Reason: ReasonGameOver, Logs: [][]any{{float64(1), float64(0)}}},
		"unknown op":      {Reason: ReasonGameOver, Logs: [][]any{{float64(1), float64(4)}}},
		"string frame":    {Reason: ReasonGameOver, Logs: [][]any{{"1", float64(1)}}},
		"too long":        {Reason: ReasonGameOver, Logs: holds(MaxLogs + 1)},
	} {
		_, err := s.SubmitReport(ctx, "alice", m.ID, r)
		assert.ErrorIs(t, err, ErrInvalidReport, name)
	}
	_, err := s.SubmitReport(ctx, "carol", m.ID, Report{Reason: ReasonGameOver})
	assert.ErrorIs(t, err, ErrNotParticipant)
	// 始まる前は報告できない。
	a := accepted(t, s)
	_, err = s.SubmitReport(ctx, "alice", a.ID, Report{Reason: ReasonGameOver})
	assert.ErrorIs(t, err, ErrInvalidState)
}

// 取り消しは「確かめてから消す」までが 1 つのトランザクション。確かめた後に
// 始まった対局を消さない。
func TestCancel_DoesNotRemoveStartedMatch(t *testing.T) {
	s, _, mr, _ := newTestService(t)
	ctx := context.Background()
	m := accepted(t, s)
	_, err := s.Ready(ctx, "alice", m.ID, true)
	require.NoError(t, err)
	// check を通った直後に相手の準備で始まった状況を作る。
	check := cancelCheck("alice")
	err = s.remove(ctx, m.ID, "alice", "canceled", func(cur *Match) error {
		err := check(cur)
		if err == nil && cur.Status == StatusAccepted {
			_, rerr := s.Ready(ctx, "bob", m.ID, true)
			require.NoError(t, rerr)
		}
		return err
	})
	assert.ErrorIs(t, err, ErrInvalidState)
	assert.True(t, mr.Exists(matchKey(m.ID)), "the started match survives")
	got, err := s.Get(ctx, m.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusPlaying, got.Status)
}

// 開始前に送った盤面の時刻で、始まった直後に切断を言われない。
func TestClaimDisconnected_CountsFromStart(t *testing.T) {
	s, _, _, now := newTestService(t)
	ctx := context.Background()
	m := accepted(t, s)
	s.State(ctx, "bob", m.ID, []byte(`{}`))
	*now = now.Add(time.Minute)
	_, err := s.Ready(ctx, "alice", m.ID, true)
	require.NoError(t, err)
	_, err = s.Ready(ctx, "bob", m.ID, true)
	require.NoError(t, err)
	*now = now.Add(Countdown + DisconnectAfter - time.Second)
	_, err = s.ClaimDisconnected(ctx, "alice", m.ID)
	assert.ErrorIs(t, err, ErrNotYet)
}

// 一覧の集合に残った応答済みの招待は返さず、掃除する。
func TestInvitations_SkipsAnswered(t *testing.T) {
	s, _, mr, _ := newTestService(t)
	ctx := context.Background()
	m := accepted(t, s)
	_, err := mr.ZAdd(invitesKey("bob"), 1, m.ID)
	require.NoError(t, err)
	list, err := s.Invitations(ctx, "bob")
	require.NoError(t, err)
	assert.Empty(t, list)
	members, _ := mr.ZMembers(invitesKey("bob"))
	assert.Empty(t, members)
}

// 時間切れの報告を出して画面を閉じた側は、切断の申告で負けない。
func TestClaimDisconnected_OpponentReported(t *testing.T) {
	s, _, _, now := newTestService(t)
	ctx := context.Background()
	m := playing(t, s, now)
	*now = now.Add(TimeLimit)
	_, err := s.SubmitReport(ctx, "alice", m.ID, Report{Score: 1000, Reason: ReasonTimeUp})
	require.NoError(t, err)
	*now = now.Add(DisconnectAfter * 2)
	_, err = s.ClaimDisconnected(ctx, "bob", m.ID)
	assert.ErrorIs(t, err, ErrInvalidState)
	// 報告した側からは、報告しない相手に勝ちを申告できる。
	got, err := s.ClaimDisconnected(ctx, "alice", m.ID)
	require.NoError(t, err)
	assert.Equal(t, "alice", *got.WinnerID)
}

// 制限時間を過ぎたら、盤面を送り続けて報告しない相手を待たない。攻撃も受けない。
func TestTimeLimitIsEnforced(t *testing.T) {
	s, _, _, now := newTestService(t)
	ctx := context.Background()
	m := playing(t, s, now)
	*now = now.Add(TimeLimit + TimeUpSkew)
	require.NoError(t, s.Attack(ctx, "bob", m.ID, 1), "an attack within the skew is still accepted")
	*now = now.Add(time.Millisecond)
	assert.ErrorIs(t, s.Attack(ctx, "bob", m.ID, 1), ErrInvalidState)

	_, err := s.SubmitReport(ctx, "alice", m.ID, Report{Score: 10, Reason: ReasonTimeUp})
	require.NoError(t, err)
	*now = now.Add(DisconnectAfter - TimeUpSkew - 2*time.Millisecond)
	s.State(ctx, "bob", m.ID, []byte(`{}`))
	_, err = s.ClaimDisconnected(ctx, "alice", m.ID)
	assert.ErrorIs(t, err, ErrNotYet)
	*now = now.Add(TimeUpSkew + 2*time.Millisecond)
	s.State(ctx, "bob", m.ID, []byte(`{}`))
	got, err := s.ClaimDisconnected(ctx, "alice", m.ID)
	require.NoError(t, err)
	assert.Equal(t, StatusEnded, got.Status)
	assert.Equal(t, "alice", *got.WinnerID)
}

// 報告の記録そのものは Redis に残さない。
func TestSubmitReport_DoesNotStoreLogs(t *testing.T) {
	s, _, mr, now := newTestService(t)
	ctx := context.Background()
	m := playing(t, s, now)
	require.NoError(t, s.Attack(ctx, "alice", m.ID, 2))
	_, err := s.SubmitReport(ctx, "bob", m.ID, Report{Score: 1, Reason: ReasonGameOver, Logs: logsWithGarbage(2)})
	require.NoError(t, err)
	raw, err := mr.Get(matchKey(m.ID))
	require.NoError(t, err)
	assert.NotContains(t, raw, "logs")
	assert.Contains(t, raw, `"garbage":2`)
}

// 状態ごとの期限。
func TestTTLs(t *testing.T) {
	s, _, mr, now := newTestService(t)
	ctx := context.Background()
	inv, err := s.Invite(ctx, alice, carol, "normal")
	require.NoError(t, err)
	assert.Equal(t, InviteTTL, mr.TTL(matchKey(inv.ID)))
	m := accepted(t, s)
	assert.Equal(t, MatchTTL, mr.TTL(matchKey(m.ID)))
	m = playing(t, s, now)
	_, err = s.SubmitReport(ctx, "bob", m.ID, Report{Reason: ReasonSurrender})
	require.NoError(t, err)
	assert.Equal(t, EndedTTL, mr.TTL(matchKey(m.ID)))
}

// 報告していない側は、制限時間を過ぎても相手を待たずに勝つことはできない。
func TestClaimDisconnected_OverdueNeedsOwnReport(t *testing.T) {
	s, _, _, now := newTestService(t)
	ctx := context.Background()
	m := playing(t, s, now)
	*now = now.Add(TimeLimit + DisconnectAfter)
	s.State(ctx, "bob", m.ID, []byte(`{}`))
	_, err := s.ClaimDisconnected(ctx, "alice", m.ID)
	assert.ErrorIs(t, err, ErrNotYet)
}

// 開始直後は、開始前に送った盤面の古い時刻で切断を言われない。
func TestClaimDisconnected_NotRightAfterStart(t *testing.T) {
	s, _, mr, now := newTestService(t)
	ctx := context.Background()
	m := playing(t, s, now)
	// 開始の touch より前の値が残った状況を作る (touch が遅れた競合を再現する)。
	require.NoError(t, mr.Set(seenKey(m.ID, "bob"), "0"))
	_, err := s.ClaimDisconnected(ctx, "alice", m.ID)
	assert.ErrorIs(t, err, ErrNotYet)
}

// 招待を設定を変えて出し直す間に受けられていたら、受けた対局は消さない。
func TestInvite_ReplaceKeepsAcceptedMatch(t *testing.T) {
	mr := miniredis.RunT(t)
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	plain := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { plain.Close() })
	hooked := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { hooked.Close() })
	pub := &capturePub{}
	s := NewService(hooked, pub, nil, gen)
	other := NewService(plain, pub, nil, gen)
	ctx := context.Background()

	m, err := s.Invite(ctx, alice, bob, "normal")
	require.NoError(t, err)
	// 取り消しの WATCH の直前に bob が受ける。
	var once sync.Once
	hooked.AddHook(watchHook{before: func() {
		once.Do(func() {
			_, aerr := other.Accept(ctx, "bob", m.ID)
			require.NoError(t, aerr)
		})
	}})
	_, err = s.Invite(ctx, alice, bob, "square")
	require.NoError(t, err)
	got, err := s.Get(ctx, m.ID)
	require.NoError(t, err, "the accepted match survives")
	assert.Equal(t, StatusAccepted, got.Status)
}

type watchHook struct{ before func() }

func (watchHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h watchHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "watch" {
			h.before()
		}
		return next(ctx, cmd)
	}
}
func (watchHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func TestValidGameMode(t *testing.T) {
	for _, m := range []string{"normal", "square", "yen", "sweets", "space", "bouncy", "square-bouncy", "yen-friction", "normal-friction", "sweets-bouncy"} {
		assert.True(t, ValidGameMode(m), m)
	}
	for _, m := range []string{"", "normal-bouncy", "normal-default", "space-bouncy", "foo", "foo-bouncy", "square-", "-bouncy", "square-space"} {
		assert.False(t, ValidGameMode(m), m)
	}
}

func strPtr(s string) *string { return &s }

// holds returns n well-formed hold entries.
func holds(n int) [][]any {
	out := make([][]any, n)
	for i := range out {
		out[i] = []any{float64(1), float64(1)}
	}
	return out
}
