// Package bubbleversus runs 1:1 versus matches of the bubble game between
// local users (#3230).
//
// 盤面はそれぞれのクライアントが動かす (サーバーは物理演算を回せない)。サーバーの
// 役目は、招待と成立、共通のシード・設定・開始時刻の配布、攻撃の中継、切断と
// 制限時間の判定、終局の判定。**対局の状態は Redis にだけ置く** (期限付き)。
// 対戦の記録 (両者の報告と勝敗) は、報告と終局を受けた時点で DB に書く (#3232)。
package bubbleversus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	mrand "math/rand/v2"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// Rules and limits of a match.
const (
	// InviteTTL is how long an unanswered invitation lives.
	InviteTTL = 10 * time.Minute
	// MatchTTL is how long an accepted or running match lives without activity.
	MatchTTL = 30 * time.Minute
	// EndedTTL is how long an ended match stays readable.
	EndedTTL = 24 * time.Hour
	// TimeLimit is the length of a match. 過ぎたら得点で決める。
	TimeLimit = 5 * time.Minute
	// Countdown is the delay between both players being ready and the start.
	Countdown = 3 * time.Second
	// DisconnectAfter is how long a player may be silent before the opponent
	// can claim the win.
	DisconnectAfter = 30 * time.Second
	// TimeUpSkew is how early a "time is up" report is accepted (端末の時計の
	// ずれと通信の遅れを見込む)。
	TimeUpSkew = 5 * time.Second
	// MaxAttackPerMessage bounds one attack message.
	MaxAttackPerMessage = 50
	// MaxLogs bounds the operation log of a report.
	MaxLogs = 50_000
	// MaxGarbagePerDrop mirrors VERSUS_RULES.maxGarbagePerDrop of the engine.
	MaxGarbagePerDrop = 5
)

// Status of a match.
type Status string

// Statuses.
const (
	StatusInvited  Status = "invited"
	StatusAccepted Status = "accepted"
	StatusPlaying  Status = "playing"
	StatusEnded    Status = "ended"
)

// Reasons a match ends.
const (
	ReasonGameOver      = "gameOver"
	ReasonSurrender     = "surrender"
	ReasonTimeUp        = "timeUp"
	ReasonDisconnected  = "disconnected"
	ReasonInvalidReport = "invalidReport"
	// ReasonOpponentEnded is a report sent after the opponent's report ended the
	// match. It only records the board for the replay and never decides the
	// outcome (#3232).
	ReasonOpponentEnded = "opponentEnded"
)

// MaxGameVersion bounds the engine version a report may claim.
const MaxGameVersion = 1 << 20

// RecordRetention is how long a match record is kept after it ended (#3232).
// 終局しないまま放置された記録は、開始からこの期間で消える。
//
// **定義はここに 1 つ。** 消す側 (queue の clean) と、画面や doc で期間を
// 説明する側が違う値を持つと、説明が嘘になる。
const RecordRetention = 30 * 24 * time.Hour

// Errors.
var (
	ErrNoSuchMatch     = errors.New("bubbleversus: no such match")
	ErrNotParticipant  = errors.New("bubbleversus: not a participant")
	ErrInvalidState    = errors.New("bubbleversus: invalid state")
	ErrYourself        = errors.New("bubbleversus: cannot invite yourself")
	ErrRemoteUser      = errors.New("bubbleversus: remote users cannot be invited")
	ErrBlocked         = errors.New("bubbleversus: blocked")
	ErrInvalidGameMode = errors.New("bubbleversus: invalid game mode")
	ErrInvalidReport   = errors.New("bubbleversus: invalid report")
	ErrNotYet          = errors.New("bubbleversus: too early")
)

// Result is what a player reported at the end.
type Result struct {
	Score  int64  `json:"score"`
	Frame  int64  `json:"frame"`
	Reason string `json:"reason"`
	// Garbage は記録に入っているおじゃま石の数。
	Garbage int64 `json:"garbage"`
}

// Player is one side of a match. 0 番が招待した側。
type Player struct {
	UserID string `json:"userId"`
	Ready  bool   `json:"ready"`
	// Sent は相手へ送った石の数 (サーバーが中継した分)。
	Sent   int64   `json:"sent"`
	Result *Result `json:"result,omitempty"`
}

// Match is a versus match.
type Match struct {
	ID       string    `json:"id"`
	GameMode string    `json:"gameMode"`
	Seed     string    `json:"seed,omitempty"`
	Status   Status    `json:"status"`
	Players  [2]Player `json:"players"`
	// CreatedAt / StartAt / EndedAt は unix ms。StartAt はカウントダウン後の開始時刻。
	CreatedAt int64   `json:"createdAt"`
	StartAt   int64   `json:"startAt,omitempty"`
	EndedAt   int64   `json:"endedAt,omitempty"`
	WinnerID  *string `json:"winnerId,omitempty"`
	Reason    string  `json:"reason,omitempty"`
}

// Side returns the index of userID in the match, or -1.
func (m *Match) Side(userID string) int {
	for i := range m.Players {
		if m.Players[i].UserID == userID {
			return i
		}
	}
	return -1
}

// BlockChecker reports whether blocker blocks blockee.
type BlockChecker interface {
	IsBlocked(blockerID, blockeeID string) (bool, error)
}

// Publisher sends events to users and match participants.
type Publisher interface {
	// PublishInvited tells target that inviter invited them to match.
	PublishInvited(target string, inviter *model.User, match *Match)
	// PublishUser sends an event to one user (bubbleVersus:<userId>).
	PublishUser(userID, eventType string, body any)
	// PublishMatch sends an event to both participants (bubbleVersusMatch:<id>).
	PublishMatch(matchID, eventType string, body any)
}

// Service runs matches.
type Service struct {
	rdb     redis.UniversalClient
	pub     Publisher
	blocks  BlockChecker
	idGen   id.Generator
	clock   func() time.Time
	records RecordStore
}

// RecordStore persists the record of a match (#3232). It is satisfied by
// repository.BubbleVersusRepository.
type RecordStore interface {
	SaveReport(base repository.BubbleVersusMatchBase, slot int, report repository.BubbleVersusReport) error
	Finish(base repository.BubbleVersusMatchBase, endedAt time.Time, winnerID *string, reason string) error
}

// SetRecordStore wires where match records are written. Without it nothing is
// recorded.
func (s *Service) SetRecordStore(rs RecordStore) { s.records = rs }

func recordBase(m *Match) repository.BubbleVersusMatchBase {
	return repository.BubbleVersusMatchBase{
		ID: m.ID, User1ID: m.Players[0].UserID, User2ID: m.Players[1].UserID,
		GameMode: m.GameMode, Seed: m.Seed, StartedAt: time.UnixMilli(m.StartAt).UTC(),
	}
}

// recordFinish writes the outcome of an ended match.
//
// **失敗しても呼び出し元のエラーにしない。** 勝敗は Redis で既に確定している。
// ここで失敗を返すと、クライアントが送り直しても「報告済み」で弾かれるだけで
// 記録は戻らない。記録が欠けるだけなので、ログに残して進める。
func (s *Service) recordFinish(m *Match) {
	if s.records == nil {
		return
	}
	if err := s.records.Finish(recordBase(m), time.UnixMilli(m.EndedAt).UTC(), m.WinnerID, m.Reason); err != nil {
		slog.Warn("bubbleversus: record finish failed", "matchId", m.ID, "err", err)
	}
}

// NewService constructs a Service.
func NewService(rdb redis.UniversalClient, pub Publisher, blocks BlockChecker, idGen id.Generator) *Service {
	return &Service{rdb: rdb, pub: pub, blocks: blocks, idGen: idGen, clock: time.Now}
}

// SetClockForTest overrides the time source.
func (s *Service) SetClockForTest(fn func() time.Time) { s.clock = fn }

func matchKey(id string) string      { return "bubbleVersus:match:" + id }
func invitesKey(uid string) string   { return "bubbleVersus:invites:" + uid }
func seenKey(mid, uid string) string { return "bubbleVersus:seen:" + mid + ":" + uid }

func (s *Service) now() int64 { return s.clock().UnixMilli() }

func ttlOf(m *Match) time.Duration {
	switch m.Status {
	case StatusInvited:
		return InviteTTL
	case StatusEnded:
		return EndedTTL
	}
	return MatchTTL
}

// Get returns a match.
func (s *Service) Get(ctx context.Context, matchID string) (*Match, error) {
	return decodeMatch(s.rdb.Get(ctx, matchKey(matchID)).Bytes())
}

// decodeMatch decodes the result of a GET on a match key.
func decodeMatch(raw []byte, err error) (*Match, error) {
	if errors.Is(err, redis.Nil) {
		return nil, ErrNoSuchMatch
	}
	if err != nil {
		return nil, err
	}
	var m Match
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("bubbleversus: decode match: %w", err)
	}
	return &m, nil
}

func (s *Service) save(ctx context.Context, p redis.Pipeliner, m *Match) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	p.Set(ctx, matchKey(m.ID), raw, ttlOf(m))
	return nil
}

// updateAttempts bounds the optimistic-lock retries of update.
const updateAttempts = 30

// update reads, changes and writes a match atomically. 2 人が同時に書き換える
// (攻撃の数、報告) ので、WATCH で競合したら読み直してやり直す。
func (s *Service) update(ctx context.Context, matchID string, fn func(m *Match) error) (*Match, error) {
	var out *Match
	for attempt := 0; attempt < updateAttempts; attempt++ {
		if attempt > 0 {
			// 両者の攻撃が同時に届くと同じ対局を奪い合う。すぐ再試行すると同じ相手と
			// また衝突するので、少しずつ間を空ける。
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(mrand.IntN(attempt*2)+1) * time.Millisecond):
			}
		}
		err := s.rdb.Watch(ctx, func(tx *redis.Tx) error {
			m, err := decodeMatch(tx.Get(ctx, matchKey(matchID)).Bytes())
			if err != nil {
				return err
			}
			if err := fn(m); err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error { return s.save(ctx, p, m) })
			if err == nil {
				out = m
			}
			return err
		}, matchKey(matchID))
		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		return out, err
	}
	return nil, fmt.Errorf("bubbleversus: too much contention on %s", matchID)
}

// Invite invites target to a match. 同じ相手へ未回答の招待があればそれを返す。
func (s *Service) Invite(ctx context.Context, inviter, target *model.User, gameMode string) (*Match, error) {
	if inviter.ID == target.ID {
		return nil, ErrYourself
	}
	if target.Host != nil && *target.Host != "" {
		return nil, ErrRemoteUser
	}
	if !ValidGameMode(gameMode) {
		return nil, ErrInvalidGameMode
	}
	if err := s.checkBlocks(inviter.ID, target.ID); err != nil {
		return nil, err
	}
	pending, err := s.Invitations(ctx, target.ID)
	if err != nil {
		return nil, err
	}
	for _, m := range pending {
		if m.Players[0].UserID != inviter.ID {
			continue
		}
		if m.GameMode == gameMode {
			return m, nil
		}
		// 形か物理を変えて誘い直したときは、古い設定の招待を取り消して作り直す
		// (古い設定のまま受けられると、誘った側の意図と違う対局が始まる)。
		// 取り消すのは未回答のものだけ。読んでから消すまでに受けられていたら、
		// その対局は残す (受けた側の対局を黙って消さない)。
		if err := s.remove(ctx, m.ID, inviter.ID, "canceled", invitedOnly(inviter.ID)); err != nil &&
			!errors.Is(err, ErrNoSuchMatch) && !errors.Is(err, ErrInvalidState) {
			return nil, err
		}
	}

	now := s.clock()
	m := &Match{
		ID: s.idGen.Generate(now), GameMode: gameMode, Status: StatusInvited,
		Players:   [2]Player{{UserID: inviter.ID}, {UserID: target.ID}},
		CreatedAt: now.UnixMilli(),
	}
	_, err = s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		if err := s.save(ctx, p, m); err != nil {
			return err
		}
		p.ZAdd(ctx, invitesKey(target.ID), redis.Z{Score: float64(m.CreatedAt), Member: m.ID})
		p.Expire(ctx, invitesKey(target.ID), InviteTTL)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if s.pub != nil {
		s.pub.PublishInvited(target.ID, inviter, m)
	}
	return m, nil
}

// checkBlocks refuses a match when either side blocks the other.
func (s *Service) checkBlocks(a, b string) error {
	if s.blocks == nil {
		return nil
	}
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		blocked, err := s.blocks.IsBlocked(pair[0], pair[1])
		if err != nil {
			// 判定できないまま通さない (モデレーションの fail-open にしない)。
			return fmt.Errorf("bubbleversus: block check: %w", err)
		}
		if blocked {
			return ErrBlocked
		}
	}
	return nil
}

// Invitations returns the unanswered invitations to userID, newest first.
func (s *Service) Invitations(ctx context.Context, userID string) ([]*Match, error) {
	ids, err := s.rdb.ZRevRange(ctx, invitesKey(userID), 0, 99).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	out := []*Match{}
	for _, mid := range ids {
		m, err := s.Get(ctx, mid)
		if errors.Is(err, ErrNoSuchMatch) || (err == nil && m.Status != StatusInvited) {
			// 期限切れ・応答済みの残骸は掃除する。
			s.rdb.ZRem(ctx, invitesKey(userID), mid)
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// Accept accepts an invitation.
func (s *Service) Accept(ctx context.Context, userID, matchID string) (*Match, error) {
	var seed string
	m, err := s.update(ctx, matchID, func(m *Match) error {
		if m.Side(userID) != 1 {
			return ErrNotParticipant
		}
		if m.Status != StatusInvited {
			return ErrInvalidState
		}
		if err := s.checkBlocks(m.Players[0].UserID, userID); err != nil {
			return err
		}
		if seed == "" {
			seed = newSeed()
		}
		m.Status = StatusAccepted
		m.Seed = seed
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.rdb.ZRem(ctx, invitesKey(userID), matchID)
	if s.pub != nil {
		s.pub.PublishUser(m.Players[0].UserID, "accepted", map[string]any{"matchId": m.ID})
		// 招待した側は対局の画面で待っているので、そちらにも流す。
		s.pub.PublishMatch(m.ID, "accepted", map[string]any{"matchId": m.ID})
	}
	return m, nil
}

// Decline declines an invitation.
func (s *Service) Decline(ctx context.Context, userID, matchID string) error {
	return s.remove(ctx, matchID, userID, "declined", func(m *Match) error {
		if m.Side(userID) != 1 {
			return ErrNotParticipant
		}
		if m.Status != StatusInvited {
			return ErrInvalidState
		}
		return nil
	})
}

// Cancel withdraws an invitation or leaves a match that has not started.
func (s *Service) Cancel(ctx context.Context, userID, matchID string) error {
	return s.remove(ctx, matchID, userID, "canceled", cancelCheck(userID))
}

// invitedOnly allows the inviter to withdraw an unanswered invitation only.
func invitedOnly(userID string) func(m *Match) error {
	return func(m *Match) error {
		if m.Side(userID) != 0 {
			return ErrNotParticipant
		}
		if m.Status != StatusInvited {
			return ErrInvalidState
		}
		return nil
	}
}

// cancelCheck reports whether userID may cancel the match.
func cancelCheck(userID string) func(m *Match) error {
	return func(m *Match) error {
		if m.Side(userID) < 0 {
			return ErrNotParticipant
		}
		// 招待は招待した側だけが取り消せる (受ける側は Decline)。
		if m.Status == StatusInvited && m.Side(userID) != 0 {
			return ErrNotParticipant
		}
		if m.Status != StatusInvited && m.Status != StatusAccepted {
			return ErrInvalidState
		}
		return nil
	}
}

// remove deletes a match when check allows it. 確かめてから消すまでを 1 つの
// トランザクションにする。分けると、相手の準備で始まった直後の対局を消せてしまう。
func (s *Service) remove(ctx context.Context, matchID, by, eventType string, check func(m *Match) error) error {
	var removed *Match
	for attempt := 0; attempt < updateAttempts; attempt++ {
		err := s.rdb.Watch(ctx, func(tx *redis.Tx) error {
			m, err := decodeMatch(tx.Get(ctx, matchKey(matchID)).Bytes())
			if err != nil {
				return err
			}
			if err := check(m); err != nil {
				return err
			}
			_, err = tx.TxPipelined(ctx, func(p redis.Pipeliner) error {
				p.Del(ctx, matchKey(m.ID))
				p.ZRem(ctx, invitesKey(m.Players[1].UserID), m.ID)
				return nil
			})
			if err == nil {
				removed = m
			}
			return err
		}, matchKey(matchID))
		if errors.Is(err, redis.TxFailedErr) {
			continue
		}
		if err != nil {
			return err
		}
		if s.pub != nil {
			other := removed.Players[1-removed.Side(by)].UserID
			s.pub.PublishUser(other, eventType, map[string]any{"matchId": removed.ID, "userId": by})
			s.pub.PublishMatch(removed.ID, eventType, map[string]any{"userId": by})
		}
		return nil
	}
	return fmt.Errorf("bubbleversus: too much contention on %s", matchID)
}

// Ready marks userID ready. 両者が揃ったらカウントダウンの後に始める。
func (s *Service) Ready(ctx context.Context, userID, matchID string, ready bool) (*Match, error) {
	started := false
	m, err := s.update(ctx, matchID, func(m *Match) error {
		side := m.Side(userID)
		if side < 0 {
			return ErrNotParticipant
		}
		if m.Status != StatusAccepted {
			return ErrInvalidState
		}
		m.Players[side].Ready = ready
		started = m.Players[0].Ready && m.Players[1].Ready
		if started {
			m.Status = StatusPlaying
			m.StartAt = s.clock().Add(Countdown).UnixMilli()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if started {
		// 両者の「見た」時刻を開始時刻から数える (開始前に切断を言われないように)。
		s.touch(ctx, m.ID, m.Players[0].UserID, m.StartAt)
		s.touch(ctx, m.ID, m.Players[1].UserID, m.StartAt)
	}
	if s.pub != nil {
		s.pub.PublishMatch(m.ID, "readyStates", map[string]any{"user1": m.Players[0].Ready, "user2": m.Players[1].Ready})
		if started {
			// 端末の時計はずれているので、クライアントは startAt ではなく受け取って
			// から countdownMs 後に始める (startAt は表示と記録用)。
			s.pub.PublishMatch(m.ID, "started", map[string]any{
				"seed": m.Seed, "gameMode": m.GameMode, "startAt": m.StartAt,
				"countdownMs": Countdown.Milliseconds(), "timeLimitMs": TimeLimit.Milliseconds(),
			})
		}
	}
	return m, nil
}

// Attack relays stones from userID to the opponent.
func (s *Service) Attack(ctx context.Context, userID, matchID string, count int64) error {
	if count <= 0 {
		return ErrInvalidReport
	}
	// エンジンは大きなコンボで 1 回の攻撃が上限を超えうる。丸ごと捨てると 1 個も
	// 届かないので、上限で切り詰める。
	count = min(count, MaxAttackPerMessage)
	m, err := s.update(ctx, matchID, func(m *Match) error {
		side := m.Side(userID)
		if side < 0 {
			return ErrNotParticipant
		}
		// 制限時間を過ぎた攻撃は受けない。受けると、負けている側が攻撃を送り続けて
		// 対局の期限を延ばし、終局を引き延ばせる。
		if m.Status != StatusPlaying || s.now() < m.StartAt || s.now() > m.StartAt+TimeLimit.Milliseconds()+TimeUpSkew.Milliseconds() {
			return ErrInvalidState
		}
		m.Players[side].Sent += count
		return nil
	})
	if err != nil {
		return err
	}
	s.touch(ctx, matchID, userID, s.now())
	if s.pub != nil {
		s.pub.PublishMatch(m.ID, "attack", map[string]any{"from": userID, "count": count})
	}
	return nil
}

// State relays a board summary of userID to the opponent. サーバーは中身を解釈
// しない (相手の盤面を横に小さく出すための表示用)。
func (s *Service) State(ctx context.Context, userID, matchID string, state json.RawMessage) {
	s.touch(ctx, matchID, userID, s.now())
	if s.pub != nil {
		s.pub.PublishMatch(matchID, "state", map[string]any{"userId": userID, "state": state})
	}
}

// touch records when userID was last heard from.
func (s *Service) touch(ctx context.Context, matchID, userID string, at int64) {
	s.rdb.Set(ctx, seenKey(matchID, userID), at, MatchTTL)
}

// ClaimDisconnected ends a match in userID's favor when the opponent has been
// silent for DisconnectAfter. サーバーは時間で自分から判定しない (reversi の
// claimTimeIsUp と同じく、残っている側の申告で確かめる)。
func (s *Service) ClaimDisconnected(ctx context.Context, userID, matchID string) (*Match, error) {
	ended := false
	m, err := s.update(ctx, matchID, func(m *Match) error {
		ended = false
		side := m.Side(userID)
		if side < 0 {
			return ErrNotParticipant
		}
		if m.Status != StatusPlaying {
			return ErrInvalidState
		}
		// 相手が報告を済ませているなら切断ではない (時間切れの報告を出して画面を
		// 閉じただけ)。ここを見ないと、報告を出さずに待つだけで勝ちを申告できる。
		if m.Players[1-side].Result != nil {
			return ErrInvalidState
		}
		now := s.now()
		// 制限時間を過ぎても相手が報告しないなら、盤面を送り続けていても待たない。
		// 待つと、負けている側が報告を出さずに終局を引き延ばせる。
		// 開始直後は、開始前に送った盤面の時刻で判定しない。
		if now < m.StartAt+DisconnectAfter.Milliseconds() {
			return ErrNotYet
		}
		// 待たないのは、申告した本人が報告を済ませているときだけ。そうでないと、
		// 負けている側が報告を出さずに待ち、相手の報告が遅れたところで勝てる。
		overdue := m.Players[side].Result != nil &&
			now >= m.StartAt+TimeLimit.Milliseconds()+DisconnectAfter.Milliseconds()
		if !overdue {
			last, err := s.rdb.Get(ctx, seenKey(matchID, m.Players[1-side].UserID)).Int64()
			if errors.Is(err, redis.Nil) {
				last = m.StartAt
			} else if err != nil {
				return err
			}
			if now-last < DisconnectAfter.Milliseconds() {
				return ErrNotYet
			}
		}
		m.Status = StatusEnded
		m.EndedAt = now
		m.WinnerID = &m.Players[side].UserID
		m.Reason = ReasonDisconnected
		ended = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	if ended {
		s.recordFinish(m)
		if s.pub != nil {
			s.pub.PublishMatch(m.ID, "ended", map[string]any{"winnerId": m.WinnerID, "reason": m.Reason})
		}
	}
	return m, nil
}

// Report is what a player sends at the end of their game.
type Report struct {
	Score  int64
	Frame  int64
	Reason string
	Logs   [][]any
	// GameVersion is the engine version the logs were made with. nil when the
	// client did not send it (the replay then refuses to play the board).
	GameVersion *int
}

// SubmitReport records userID's result and decides the match.
//
//   - gameOver / surrender: 先に終わった側の負け。相手がまだ遊んでいれば、ここで
//     終局にする (相手の報告は後から残すだけ)
//   - timeUp: 制限時間を過ぎていること。両者の報告が揃ったら得点で決める (同点は
//     引き分け)
//
// **受け取った石の数を突き合わせる。** 記録に入っている石が、相手が送った数より多い
// のはあり得ないので、その報告は不正として報告した側の負けにする。
func (s *Service) SubmitReport(ctx context.Context, userID, matchID string, r Report) (*Match, error) {
	switch r.Reason {
	case ReasonGameOver, ReasonSurrender, ReasonTimeUp, ReasonOpponentEnded:
	default:
		return nil, ErrInvalidReport
	}
	// 記録の列は 32 bit (#3232)。収まらない値を受けると DB への書き込みが失敗して
	// 記録が黙って欠けるので、ここで弾く。正当な対局で届く値ではない
	// (5 分で約 18,000 フレーム)。
	if r.Score < 0 || r.Frame < 0 || r.Score > math.MaxInt32 || r.Frame > math.MaxInt32 || len(r.Logs) > MaxLogs {
		return nil, ErrInvalidReport
	}
	if r.GameVersion != nil && (*r.GameVersion < 1 || *r.GameVersion > MaxGameVersion) {
		return nil, ErrInvalidReport
	}
	garbage, ok := garbageInLogs(r.Logs)
	if !ok {
		return nil, ErrInvalidReport
	}

	// 判定は全部 1 つの更新の中で行う。外で読んだ値で決めると、その後に届いた攻撃
	// (Sent が増える) や相手の報告を見落として、正しい報告を不正と判定しうる。
	// 記録そのもの (logs) は Redis に置かない。置くと 1 報告ごとに body の上限
	// いっぱいの blob を Redis に 24 時間置ける。記録は更新の後で DB に書く (#3232)。
	result := &Result{Score: r.Score, Frame: r.Frame, Reason: r.Reason, Garbage: garbage}
	ended := false
	slot := -1
	var stored Result
	m, err := s.update(ctx, matchID, func(m *Match) error {
		ended = false
		side := m.Side(userID)
		if side < 0 {
			return ErrNotParticipant
		}
		if m.Status != StatusPlaying && m.Status != StatusEnded {
			return ErrInvalidState
		}
		if m.Players[side].Result != nil {
			return ErrInvalidState
		}
		// 相手の終局を受けての報告は、終局した後にだけ受ける。終局前に受けると、
		// 勝敗に効かない理由で自分の報告を済ませ、切断の判定 (相手の報告済みなら
		// 切断ではない) を相手に使わせないことができる。
		if r.Reason == ReasonOpponentEnded && m.Status != StatusEnded {
			return ErrInvalidReport
		}
		slot = side
		opp := m.Players[1-side].UserID
		res := *result
		m.Players[side].Result = &res
		stored = res
		if m.Status == StatusEnded {
			// 終局後の報告 (opponentEnded など) は勝敗に効かないが、記録には残る。
			// 石の数が相手の送った数を超える記録は、理由を invalidReport にして残す
			// (申告どおりに残すと、リプレイに作り話の盤面が並ぶ)。
			if garbage > m.Players[1-side].Sent {
				res.Reason = ReasonInvalidReport
				stored = res
			}
			return nil
		}
		var winner *string
		reason := r.Reason
		switch {
		case garbage > m.Players[1-side].Sent:
			// 相手が送った数より多くの石を受けたことになっている記録は作れない。
			res.Reason = ReasonInvalidReport
			reason = ReasonInvalidReport
			winner = &opp
		case r.Reason != ReasonTimeUp:
			winner = &opp
		default:
			if s.now() < m.StartAt+TimeLimit.Milliseconds()-TimeUpSkew.Milliseconds() {
				return ErrNotYet
			}
			// 時間切れ: 両者が揃うまで終局にしない。
			other := m.Players[1-side].Result
			if other == nil {
				return nil
			}
			switch {
			case res.Score > other.Score:
				winner = &m.Players[side].UserID
			case res.Score < other.Score:
				winner = &opp
			}
		}
		stored = res
		m.Status = StatusEnded
		m.EndedAt = s.now()
		m.WinnerID = winner
		m.Reason = reason
		ended = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.recordReport(m, slot, stored, r)
	if ended {
		s.recordFinish(m)
		if s.pub != nil {
			s.pub.PublishMatch(m.ID, "ended", map[string]any{"winnerId": m.WinnerID, "reason": m.Reason})
		}
	}
	return m, nil
}

// recordReport writes one player's report into the match record. Like
// recordFinish it only logs failures.
func (s *Service) recordReport(m *Match, slot int, res Result, r Report) {
	if s.records == nil || slot < 0 {
		return
	}
	entries := r.Logs
	if entries == nil {
		// ロビーでの投了は記録を持たない。null ではなく空の記録として残す。
		entries = [][]any{}
	}
	logs, err := json.Marshal(entries)
	if err != nil {
		slog.Warn("bubbleversus: record report marshal failed", "matchId", m.ID, "err", err)
		return
	}
	// 理由は報告の申告ではなく、判定した後の値 (不正なら invalidReport) を残す。
	if err := s.records.SaveReport(recordBase(m), slot, repository.BubbleVersusReport{
		Score: int(res.Score), Frame: int(res.Frame), Reason: res.Reason, GameVersion: r.GameVersion, Logs: logs,
	}); err != nil {
		slog.Warn("bubbleversus: record report failed", "matchId", m.ID, "err", err)
	}
}

// garbageInLogs sums the stones in serialized logs ([frameDelta, op, arg])。
// 形が壊れていれば false。
func garbageInLogs(logs [][]any) (int64, bool) {
	var total int64
	for _, l := range logs {
		// 形はエンジンの serializeLogs と同じものだけ受ける: [fd, 0, x] / [fd, 1] /
		// [fd, 2] / [fd, 3, count]。読まない要素に任意の値を載せられる形は受けない
		// (記録は DB にそのまま残すので、形を締めないと任意の値の置き場になる)。
		if len(l) < 2 || len(l) > 3 {
			return 0, false
		}
		for _, v := range l {
			if _, ok := v.(float64); !ok {
				return 0, false
			}
		}
		// 先頭はフレームの差分。エンジンは 0 以上の整数しか書かない。小数や負の値を
		// 受けると、記録 (DB に残る) を再生したときに操作を当てる位置が決まらず、
		// リプレイが途中から動かなくなる (#3232)。
		if fd := l[0].(float64); fd < 0 || fd != math.Trunc(fd) {
			return 0, false
		}
		op := l[1].(float64)
		wantLen := 2
		if op == 0 || op == 3 {
			wantLen = 3
		}
		if (op != 0 && op != 1 && op != 2 && op != 3) || len(l) != wantLen {
			return 0, false
		}
		if op != 3 {
			continue
		}
		// エンジンは 1 回の投下で VERSUS_RULES.maxGarbagePerDrop (5) 個までしか
		// 降らせない。上限を見ないと巨大な値が int64 へ変換するときに負へ化け、
		// 合計が相手の送った数を下回って不正を見逃す。
		n := l[2].(float64)
		if n < 1 || n > MaxGarbagePerDrop || n != float64(int64(n)) {
			return 0, false
		}
		total += int64(n)
	}
	return total, true
}

func newSeed() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ValidGameMode reports whether mode is a bubble game mode string
// (misskey-bubble-game の parseGameMode と同じ規則)。
func ValidGameMode(mode string) bool {
	bases := []string{"normal", "yen", "square", "sweets", "space"}
	if mode == "bouncy" {
		return true
	}
	for _, b := range bases {
		if mode == b {
			return true
		}
	}
	i := strings.LastIndex(mode, "-")
	if i < 0 {
		return false
	}
	base, physics := mode[:i], mode[i+1:]
	if base == "space" || !contains(bases, base) {
		return false
	}
	if physics != "bouncy" && physics != "friction" {
		return false
	}
	return !(base == "normal" && physics == "bouncy")
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
