package reversi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strconv"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/redis/go-redis/v9"
)

// --- Federation ID cache (existing) ---

// FederationIDTTL is how long a reversi federation session mapping lives in
// Redis. 長時間ゲーム (持ち時間無制限) でも 1 日持てば十分というトレードオフ。
const FederationIDTTL = 24 * time.Hour

// FederationIDCache manages federationId ↔ gameId mapping in Redis.
// This is the PRIMARY store for reversi federation session mapping — there
// is no DB column backing it (so `reversi_game` stays本家 Misskey互換)。
// Server restart risks: Redis AOF/RDB 永続化に依存する。flush 時は進行中の
// 連合ゲームが inbox から辿れなくなる (ゲーム行自体はローカルで残る)。
type FederationIDCache struct {
	redis redis.Cmdable
}

// NewFederationIDCache creates a new cache.
func NewFederationIDCache(r redis.Cmdable) *FederationIDCache {
	return &FederationIDCache{redis: r}
}

// sessionToGameKey / gameToSessionKey は Redis 上の key 構成。前者が双方向
// lookup の primary、後者は "このゲームは連合ゲームか" を判定する reverse。
func sessionToGameKey(sessionID string) string { return "reversi:fed:session:" + sessionID }
func gameToSessionKey(gameID string) string    { return "reversi:fed:game:" + gameID }

// Set stores a bidirectional federationId ↔ gameId mapping.
func (c *FederationIDCache) Set(ctx context.Context, federationID, gameID string) {
	if c.redis == nil {
		return
	}
	c.redis.Set(ctx, sessionToGameKey(federationID), gameID, FederationIDTTL)
	c.redis.Set(ctx, gameToSessionKey(gameID), federationID, FederationIDTTL)
}

// Get retrieves a gameId from a federationId.
func (c *FederationIDCache) Get(ctx context.Context, federationID string) (string, error) {
	if c.redis == nil {
		return "", redis.Nil
	}
	return c.redis.Get(ctx, sessionToGameKey(federationID)).Result()
}

// GetSessionByGame returns the federation session id that maps to gameID, or
// ("", false) when the game is not federated (or the mapping has expired).
// 用途: api/reversi/handler が Surrender / Leave 時にリモートへ送信要否を判定する。
func (c *FederationIDCache) GetSessionByGame(ctx context.Context, gameID string) (string, bool) {
	if c.redis == nil {
		return "", false
	}
	v, err := c.redis.Get(ctx, gameToSessionKey(gameID)).Result()
	if err != nil || v == "" {
		return "", false
	}
	return v, true
}

// Delete removes both directions of the mapping. 呼び出しは明示的な削除が
// 必要な場面 (ゲーム終了時、CancelMatch 時) に限る。
func (c *FederationIDCache) Delete(ctx context.Context, federationID, gameID string) {
	if c.redis == nil {
		return
	}
	if federationID != "" {
		c.redis.Del(ctx, sessionToGameKey(federationID))
	}
	if gameID != "" {
		c.redis.Del(ctx, gameToSessionKey(gameID))
	}
}

// joinSentKey / IsJoinSent / MarkJoinSent は accept 時の Join が既に
// 送信済みかを追跡する。/reversi/show-game で User2 が pending game を
// 表示するたびに Join を再送してしまう問題の軽減 (#417 Devin review)。
// delivery 自体は相手側で idempotent だが、無駄なネットワーク呼び出しと
// log ノイズを減らす。
func joinSentKey(federationID string) string { return "reversi:fed:join-sent:" + federationID }

// IsJoinSent reports whether Join has already been delivered for the session.
func (c *FederationIDCache) IsJoinSent(ctx context.Context, federationID string) bool {
	if c.redis == nil || federationID == "" {
		return false
	}
	n, err := c.redis.Exists(ctx, joinSentKey(federationID)).Result()
	return err == nil && n > 0
}

// MarkJoinSent records that Join has been delivered. TTL は federation 本体
// の mapping (24h) と合わせる。
func (c *FederationIDCache) MarkJoinSent(ctx context.Context, federationID string) {
	if c.redis == nil || federationID == "" {
		return
	}
	c.redis.Set(ctx, joinSentKey(federationID), "1", FederationIDTTL)
}

// ValidUpdateKeys lists the keys that can be changed via federation Update.
var ValidUpdateKeys = map[string]bool{
	"map":                  true,
	"bw":                   true,
	"isLlotheo":            true,
	"canPutEverywhere":     true,
	"loopedBoard":          true,
	"timeLimitForEachTurn": true,
	"noIrregularRules":     true,
}

// IsValidUpdateKey checks if a key is allowed for federation settings updates.
func IsValidUpdateKey(key string) bool {
	return ValidUpdateKeys[key]
}

// --- Errors returned by Service methods ---

var (
	// ErrGameNotFound is returned when the referenced game does not exist.
	ErrGameNotFound = errors.New("reversi game not found")
	// ErrNotPlayer is returned when a non-player attempts a player-only action.
	ErrNotPlayer = errors.New("user is not a player in this game")
	// ErrAlreadyStarted is returned when attempting pre-start changes on a
	// running game.
	ErrAlreadyStarted = errors.New("reversi game already started")
	// ErrAlreadyEnded is returned when attempting any action on a concluded game.
	ErrAlreadyEnded = errors.New("reversi game already ended")
	// ErrNotStarted is returned when attempting in-game actions on a pre-start
	// game.
	ErrNotStarted = errors.New("reversi game not yet started")
	// ErrNotYourTurn is returned when a player tries to move out of turn.
	ErrNotYourTurn = errors.New("not your turn")
	// ErrInvalidMove is returned when a move violates game rules.
	ErrInvalidMove = errors.New("invalid move")
	// ErrInvalidSetting is returned when an updateSettings key/value is refused.
	ErrInvalidSetting = errors.New("invalid setting")
)

// --- PubSub publisher interface ---

// GamePublisher is the minimal interface the Service uses to push events to
// the reversi WebSocket channel. 循環依存を避けるため interface で受ける。
type GamePublisher interface {
	PublishGameEvent(gameID, eventType string, body any)
}

// FederationDeliverer is the minimal interface used by the Service to send AP
// activities to a remote opponent when state changes (UpdateReady /
// UpdateSettings / PutStone / CancelGame / Surrender)。循環依存を避けるため
// interface で受ける (実装は core/federation.DeliverService)。
type FederationDeliverer interface {
	DeliverToUser(signerUserID string, recipient *model.User, body []byte) error
}

// UserLookup is the minimal interface used by the Service to resolve the
// opposing player during federation delivery. repository.UserRepository が
// そのまま満たすので実体実装の追加は不要。
type UserLookup interface {
	FindByID(id string) (*model.User, error)
}

// --- Core service ---

// Service manages reversi game state transitions and publishes updates to
// the matching WebSocket channel.
type Service struct {
	repo      repository.ReversiRepository
	publisher GamePublisher
	redis     redis.Cmdable
	fedCache  *FederationIDCache
	// Federation delivery (#417)。未設定時は state 変化時の outbound を何も
	// 送らない = ローカル同士の対戦や非連合セッションではそもそも deliver 不要。
	deliverer FederationDeliverer
	userRepo  UserLookup
	baseURL   string
}

// NewService constructs a Service with the required dependencies.
func NewService(
	repo repository.ReversiRepository,
	publisher GamePublisher,
	r redis.Cmdable,
) *Service {
	return &Service{repo: repo, publisher: publisher, redis: r}
}

// SetFederationCache attaches a FederationIDCache so that federated
// Invite/Join/Leave messages can resolve the game id.
func (s *Service) SetFederationCache(c *FederationIDCache) {
	s.fedCache = c
}

// SetFederationDeliverer attaches a deliverer used to push reversi Update /
// Leave activities to a remote opponent on state changes (#417 P1)。deliverer
// が nil のときは連合外 Service として動作する (既存互換)。
func (s *Service) SetFederationDeliverer(d FederationDeliverer) {
	s.deliverer = d
}

// SetUserRepo attaches a user lookup used by federation delivery to resolve
// actor / opponent Host / URI。
func (s *Service) SetUserRepo(r UserLookup) {
	s.userRepo = r
}

// SetBaseURL records the local instance base URL (https://<host>) used to
// build actor URIs in outbound Reversi activities.
func (s *Service) SetBaseURL(u string) {
	s.baseURL = u
}

// deliverStateToOpponent renders an Update(Game) activity for the state change
// and delivers it to the remote opponent when federation is wired and the
// opponent is actually remote。
//
// actor が remote のとき (inbox 受信経由で Service を呼んだケース) は、相手に
// echo back しないよう skip する。CherryPick は同じ guard を ApGameService 側
// で effectively 入れているが、mk-go では Service 内部で判定して呼び出し元を
// 軽くしている。
func (s *Service) deliverStateToOpponent(ctx context.Context, game *model.ReversiGame, actorUserID string, state APGameState) {
	// skip 経路はローカル対戦でも毎ターン踏むので log しない
	// (#417 Devin review: Info だと busy instance でノイジー)。
	if s.deliverer == nil || s.userRepo == nil || s.fedCache == nil || s.baseURL == "" {
		return
	}
	actor, err := s.userRepo.FindByID(actorUserID)
	if err != nil || actor == nil || actor.Host != nil {
		return
	}
	oppID := opponent(game, actorUserID)
	opp, err := s.userRepo.FindByID(oppID)
	if err != nil || opp == nil || opp.Host == nil || opp.URI == nil {
		return
	}
	sessionID, ok := s.fedCache.GetSessionByGame(ctx, game.ID)
	if !ok {
		return
	}
	state.GameSessionID = sessionID
	activity := RenderUpdate(s.baseURL, s.baseURL+"/users/"+actor.ID, *opp.URI, state)
	body, err := json.Marshal(activity)
	if err != nil {
		return
	}
	slog.Info("reversi deliver: sending update", "gameId", game.ID, "type", state.Type, "session", sessionID, "to", oppID)
	_ = s.deliverer.DeliverToUser(actor.ID, opp, body)
}

// deliverLeaveToOpponent renders a Leave(Game) activity and delivers it to
// the remote opponent on Surrender / CancelGame by a local player。
func (s *Service) deliverLeaveToOpponent(ctx context.Context, game *model.ReversiGame, actorUserID string) {
	if s.deliverer == nil || s.userRepo == nil || s.fedCache == nil || s.baseURL == "" {
		return
	}
	actor, err := s.userRepo.FindByID(actorUserID)
	if err != nil || actor == nil || actor.Host != nil {
		return
	}
	oppID := opponent(game, actorUserID)
	opp, err := s.userRepo.FindByID(oppID)
	if err != nil || opp == nil || opp.Host == nil || opp.URI == nil {
		return
	}
	sessionID, ok := s.fedCache.GetSessionByGame(ctx, game.ID)
	if !ok {
		return
	}
	activity := RenderLeave(s.baseURL, s.baseURL+"/users/"+actor.ID, *opp.URI, sessionID)
	body, err := json.Marshal(activity)
	if err != nil {
		return
	}
	_ = s.deliverer.DeliverToUser(actor.ID, opp, body)
}

// deliverUndoInviteToOpponent renders an Undo(Invite) and delivers it to the
// remote invitee when the local inviter cancels a pre-start game. CherryPick
// protocol-wise Leave is reserved for started games, Undo(Invite) is the
// correct form for retracting a pending invitation (#417 P4)。
// invitee 側 (User2 = local) がキャンセルする経路では Undo(Invite) ではなく
// 従来の Leave を送る (Undo は invitee からは投げないのが CherryPick 仕様)。
func (s *Service) deliverUndoInviteToOpponent(ctx context.Context, game *model.ReversiGame, actorUserID string) {
	if s.deliverer == nil || s.userRepo == nil || s.fedCache == nil || s.baseURL == "" {
		return
	}
	actor, err := s.userRepo.FindByID(actorUserID)
	if err != nil || actor == nil || actor.Host != nil {
		return
	}
	// Undo(Invite) は「招待者が招待を取り消す」セマンティクスなので、
	// キャンセル実行者が招待側 (User1) でない場合は Leave にフォールバック。
	if game.User1ID != actorUserID {
		s.deliverLeaveToOpponent(ctx, game, actorUserID)
		return
	}
	oppID := opponent(game, actorUserID)
	opp, err := s.userRepo.FindByID(oppID)
	if err != nil || opp == nil || opp.Host == nil || opp.URI == nil {
		return
	}
	sessionID, ok := s.fedCache.GetSessionByGame(ctx, game.ID)
	if !ok {
		return
	}
	actorURI := s.baseURL + "/users/" + actor.ID
	original := RenderInvite(s.baseURL, sessionID, actorURI, *opp.URI, "")
	// ActivityPub 慣習では @context は outer activity にのみ付く。
	// CherryPick の normalizer は nested を許容するが strict な peer
	// 向けに inner Invite の Context を空にする (#417 P4 Devin review)。
	original.Context = nil
	undo := RenderUndo(s.baseURL, actorURI, original)
	undo.To = *opp.URI
	body, err := json.Marshal(undo)
	if err != nil {
		return
	}
	slog.Info("reversi deliver: sending undo(invite)", "gameId", game.ID, "session", sessionID, "to", oppID)
	_ = s.deliverer.DeliverToUser(actor.ID, opp, body)
}

// --- Queries ---

// Get fetches a game by id.
func (s *Service) Get(_ context.Context, gameID string) (*model.ReversiGame, error) {
	game, err := s.repo.FindByID(gameID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrGameNotFound
	}
	return game, nil
}

// --- Pre-start transitions ---

// UpdateReady toggles the per-user ready flag. When both players are ready,
// StartGame is fired automatically.
func (s *Service) UpdateReady(ctx context.Context, gameID, userID string, ready bool) error {
	game, err := s.Get(ctx, gameID)
	if err != nil {
		return err
	}
	if game.IsStarted {
		return ErrAlreadyStarted
	}
	if game.IsEnded {
		return ErrAlreadyEnded
	}
	if !isPlayer(game, userID) {
		return ErrNotPlayer
	}
	// ローカルWS経由のreadyと連合inbox経由の相手readyが並行して走ると、
	// Get→full-row Saveのread-modify-writeでは相手のready=trueをstale値で
	// 上書きするlost updateが起きる (#1626)。自分のカラムだけをguard付き
	// 単一UPDATEで書き、読み直した最新行で以降の判定を行う。
	game, err = s.repo.UpdateReadyState(gameID, game.User1ID == userID, ready)
	if err != nil {
		return err
	}
	if game == nil {
		// guard不成立: pre-checkとUPDATEの間にstarted/ended/削除へ遷移した
		fresh, ferr := s.Get(ctx, gameID)
		if ferr != nil {
			return ferr
		}
		if fresh.IsEnded {
			return ErrAlreadyEnded
		}
		return ErrAlreadyStarted
	}
	s.publish(gameID, "changeReadyStates", map[string]any{
		"user1": game.User1Ready,
		"user2": game.User2Ready,
	})
	// 連合対戦の場合は ready_states Update を相手に配信 (#417 P1)。
	readyVal := ready
	s.deliverStateToOpponent(ctx, game, userID, APGameState{
		Type:  "ready_states",
		Ready: &readyVal,
	})
	if game.User1Ready && game.User2Ready {
		// 両者 ready=true を並行送信すると、もう一方の UpdateReady が先に
		// StartGame を完了させ、こちらの再 read が isStarted=true の game を
		// 観測することがある。その状態で StartGame を呼ぶと冒頭 guard が
		// ErrAlreadyStarted を返すが、両者 ready で game が開始済み =
		// 望む終状態に到達しているので idempotent success として nil に倒す
		// (#1636)。MarkStarted の atomic claim は started イベントを 1 回に
		// 抑えており、後勝ち側に spurious な ALREADY_STARTED を返さない。
		// 非並行の「既に開始済み game への ready 操作」は本関数冒頭の
		// game.IsStarted guard が引き続き ErrAlreadyStarted を返す。
		if err := s.StartGame(ctx, game); err != nil && !errors.Is(err, ErrAlreadyStarted) {
			return err
		}
		return nil
	}
	return nil
}

// UpdateSettings mutates a single pre-start setting (map, bw, isLlotheo, etc.)
// and broadcasts the change so both clients can re-render.
func (s *Service) UpdateSettings(ctx context.Context, gameID, userID, key string, value json.RawMessage) error {
	game, err := s.Get(ctx, gameID)
	if err != nil {
		return err
	}
	if game.IsStarted || game.IsEnded {
		return ErrAlreadyStarted
	}
	if !isPlayer(game, userID) {
		return ErrNotPlayer
	}
	if !IsValidUpdateKey(key) {
		return ErrInvalidSetting
	}
	if err := applySetting(game, key, value); err != nil {
		return err
	}
	// 設定変更は ready 状態をリセットする (本家挙動)
	game.User1Ready = false
	game.User2Ready = false
	if err := s.repo.Update(game); err != nil {
		return err
	}
	var valueAny any
	_ = json.Unmarshal(value, &valueAny)
	s.publish(gameID, "updateSettings", map[string]any{
		"userId": userID,
		"key":    key,
		"value":  valueAny,
	})
	s.publish(gameID, "changeReadyStates", map[string]any{
		"user1": false, "user2": false,
	})
	// 連合対戦の場合は settings Update を相手に配信 (#417 P1)。
	s.deliverStateToOpponent(ctx, game, userID, APGameState{
		Type:  "settings",
		Key:   key,
		Value: valueAny,
	})
	return nil
}

// CancelGame aborts a pre-start game. Both players receive `canceled` and the
// game row is deleted.
func (s *Service) CancelGame(ctx context.Context, gameID, userID string) error {
	game, err := s.Get(ctx, gameID)
	if err != nil {
		return err
	}
	if game.IsStarted {
		return ErrAlreadyStarted
	}
	if !isPlayer(game, userID) {
		return ErrNotPlayer
	}
	// 終了 AP activity の配信は Delete 前に行う (配信に必要な session
	// マッピングが残っているうちに送る)。成功後に fedCache も片付ける
	// (#417 Devin review で全終了経路に統一)。
	// CherryPick protocol-wise: pre-start + actor が招待側は Undo(Invite)、
	// それ以外は Leave (#417 P4)。CancelGame は pre-start 用 API なので
	// Undo(Invite) 分岐、Surrender 経路は引き続き Leave。
	s.deliverUndoInviteToOpponent(ctx, game, userID)
	if err := s.repo.Delete(gameID); err != nil {
		return err
	}
	s.cleanupFedCache(ctx, gameID)
	s.publish(gameID, "canceled", map[string]any{"userId": userID})
	return nil
}

// --- Start ---

// StartGame transitions the game to in-progress state. color 割り当ては
// game.BW ("random" | "1" | "2") に従い、Redis に最初のターンタイマーをセット
// してから `started` イベントを publish する。
func (s *Service) StartGame(ctx context.Context, game *model.ReversiGame) error {
	if game.IsStarted {
		return ErrAlreadyStarted
	}
	if game.IsEnded {
		return ErrAlreadyEnded
	}

	// 連合対戦の場合は session id から決定論的に black を算出する
	// (両サイドで同じ値になる必要がある)。CherryPick startGame と一致。
	sessionID := ""
	if s.fedCache != nil {
		if sid, ok := s.fedCache.GetSessionByGame(ctx, game.ID); ok {
			sessionID = sid
		}
	}
	black := pickBlack(game.BW, sessionID)
	game.Black = &black
	game.IsStarted = true
	now := time.Now()
	game.StartedAt = &now

	// 開始時点の盤面 CRC32 を保存する (#1553)。upstream startGame は fresh
	// engine の calcCrc32 を crc32 カラムに書き、以降 /reversi/verify は
	// この保存値とクライアント申告値を比較する (ReversiService.ts:321,618)。
	// 開始時は logs が空なので EngineFromGame は初期盤面の engine を返す。
	if engine, err := EngineFromGame(game); err == nil {
		crc := strconv.FormatUint(uint64(engine.CalcCRC32()), 10)
		game.CRC32 = &crc
	}

	// 並行UpdateReadyの両者がboth-readyを観測すると、StartGameに二重到達
	// する (#1626)。isStarted=falseをguardにしたatomic claimで先勝ちを決め、
	// 負けた側はtimer設定もstarted publishもせず静かに成功扱いにする。
	claimed, err := s.repo.MarkStarted(game)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	// 初期ターンタイマー: logsCount=0 (まだ一手も置かれていない)
	s.setTurnTimer(ctx, game.ID, 0, game.TimeLimitForEachTurn)
	s.publish(game.ID, "started", map[string]any{"game": packGame(game)})
	return nil
}

// --- In-game actions ---

// PutStone validates and applies a move, publishes the log, and ends the game
// if the engine reports terminal state.
// PutStone places a stone at pos for userID. opID is the client-supplied
// operation id (stream putStone の body.id); echoed back in the "log" event so
// the client can reconcile its optimistic move (upstream putStoneToGame の id、
// #1549)。federation 経由など op id が無い経路は空文字を渡す。
func (s *Service) PutStone(ctx context.Context, gameID, userID string, pos int, opID string) error {
	game, err := s.Get(ctx, gameID)
	if err != nil {
		return err
	}
	if !game.IsStarted {
		return ErrNotStarted
	}
	if game.IsEnded {
		return ErrAlreadyEnded
	}
	if !isPlayer(game, userID) {
		return ErrNotPlayer
	}
	engine, err := EngineFromGame(game)
	if err != nil {
		return err
	}
	myColor, err := playerColor(game, userID)
	if err != nil {
		return err
	}
	if engine.Turn == nil || *engine.Turn != myColor {
		return ErrNotYourTurn
	}
	if !engine.CanPut(myColor, pos) {
		return ErrInvalidMove
	}
	engine.PutStone(pos)

	// logs format は misskey-reversi の SerializedLog と一致させる:
	// [timeDelta, player(0|1), operation(0=put), pos]。フロントエンドの
	// Reversi.Serializer.deserializeLogs がこの shape を要求するため、他の
	// 形式だと restoreGame が空の engine を返し盤面が消える (#417 P1 UDS
	// 検証で発覚)。player は Black=1 / White=0。operation は put のみ。
	var logs [][]int
	if len(game.Logs) > 0 {
		_ = json.Unmarshal(game.Logs, &logs)
	}
	now := time.Now().UnixMilli()
	var timeDelta int64
	if len(logs) == 0 {
		timeDelta = now
	} else {
		// CherryPick: timeDelta = log.time - logs[i-1].time。過去の
		// timeDelta を全部積み上げれば前回 log の絶対時刻になる。
		var prevTotal int64
		for _, e := range logs {
			if len(e) > 0 {
				prevTotal += int64(e[0])
			}
		}
		timeDelta = now - prevTotal
	}
	playerInt := 0
	if myColor { // true == Black
		playerInt = 1
	}
	colorInt := 1
	if !myColor {
		colorInt = 2
	}
	logs = append(logs, []int{int(timeDelta), playerInt, 0, pos})
	raw, _ := json.Marshal(logs)
	game.Logs = raw

	// 一手ごとに盤面 CRC32 を保存する (#1553)。upstream putStoneToGame が
	// engine.calcCrc32() を都度 game.crc32 に書くのと同じ
	// (ReversiService.ts:489-493)。/reversi/verify の desync 判定はこの
	// 保存値に対して行われるため、更新を怠ると対局中の verify が常に
	// desynced になる。
	crc := strconv.FormatUint(uint64(engine.CalcCRC32()), 10)
	game.CRC32 = &crc

	if engine.Turn == nil {
		// ゲーム終了
		if err := s.finalizeGame(ctx, game, engine); err != nil {
			return err
		}
	} else {
		if err := s.repo.Update(game); err != nil {
			return err
		}
		s.setTurnTimer(ctx, game.ID, len(logs), game.TimeLimitForEachTurn)
	}

	// upstream putStoneToGame は log event に client op id を含める
	// (`{...log, id: id ?? null}`、#1549)。空 opID は null で返す。
	var logID any
	if opID != "" {
		logID = opID
	}
	s.publish(gameID, "log", map[string]any{
		"pos":       pos,
		"color":     colorInt,
		"player":    myColor,
		"time":      time.Now().UnixMilli(),
		"operation": "put",
		"id":        logID,
	})
	if engine.Turn == nil {
		s.publish(gameID, "ended", map[string]any{
			"winnerId": game.WinnerID,
			"game":     packGame(game),
		})
	}
	// 連合対戦の場合は putstone Update を相手に配信 (#417 P1)。ゲーム終了
	// (engine.Turn == nil) でも相手側で同じ engine を回すため最後の手を送る。
	// fedCache 参照に session mapping が必要なので、必ず配信後に cleanup する
	// (#417 Devin review: 逆順だと mapping が既に消えていて deliver が skip
	// されていた)。
	posVal := pos
	s.deliverStateToOpponent(ctx, game, userID, APGameState{
		Type: "putstone",
		Pos:  &posVal,
	})
	if engine.Turn == nil {
		s.cleanupFedCache(ctx, game.ID)
	}
	return nil
}

// cleanupFedCache removes the federation session mapping for a terminated
// game。ゲーム終了 / キャンセル時にすべての経路 (PutStone 自然終了 /
// Surrender / CancelGame / CheckTimeout) から呼び出して Redis の mapping
// が 24h TTL で残り続けるのを防ぐ。
func (s *Service) cleanupFedCache(ctx context.Context, gameID string) {
	if s.fedCache == nil {
		return
	}
	if sessionID, ok := s.fedCache.GetSessionByGame(ctx, gameID); ok {
		s.fedCache.Delete(ctx, sessionID, gameID)
	}
}

// Surrender marks the opposing user as the winner and ends the game.
func (s *Service) Surrender(ctx context.Context, gameID, userID string) error {
	game, err := s.Get(ctx, gameID)
	if err != nil {
		return err
	}
	// PutStone 等と同じ順序でガードする。未スタートの対局に対する surrender は
	// 「勝ち逃げ」と区別が付かなくなるので、開始前は拒否する。
	// #2106 L17 (intentional divergence): vanilla/cherrypick の surrender は IsStarted を
	// ガードせず pending game も終局させられるが、mk-go は勝ち逃げ防止のため未開始を
	// NOT_STARTED で弾く意図的拡張。除去せず明文化する。
	if !game.IsStarted {
		return ErrNotStarted
	}
	if game.IsEnded {
		return ErrAlreadyEnded
	}
	if !isPlayer(game, userID) {
		return ErrNotPlayer
	}
	winnerID := opponent(game, userID)
	now := time.Now()
	game.IsEnded = true
	game.EndedAt = &now
	game.WinnerID = &winnerID
	game.SurrenderedUserID = &userID
	if err := s.repo.Update(game); err != nil {
		return err
	}
	s.publish(gameID, "ended", map[string]any{
		"winnerId": winnerID,
		"reason":   "surrender",
		"game":     packGame(game),
	})
	// 連合対戦の場合は Leave を相手に配信 (#417 P1)。配信後に fedCache を
	// 片付ける (session mapping 参照が Leave 配信側に必要)。
	s.deliverLeaveToOpponent(ctx, game, userID)
	s.cleanupFedCache(ctx, gameID)
	return nil
}

// CheckTimeout is invoked when a client claims the opponent's turn timer has
// expired. If the matching Redis key is gone, the opponent wins by timeout.
func (s *Service) CheckTimeout(ctx context.Context, gameID string) error {
	game, err := s.Get(ctx, gameID)
	if err != nil {
		return err
	}
	if !game.IsStarted || game.IsEnded {
		return nil
	}
	var logs [][]int
	if len(game.Logs) > 0 {
		_ = json.Unmarshal(game.Logs, &logs)
	}
	exists, err := s.turnTimerExists(ctx, gameID, len(logs))
	if err != nil {
		return err
	}
	if exists {
		// タイマー未期限: タイムアウト主張は無効。
		return nil
	}
	// タイムアウト発生: 現在のターンプレイヤー (logs 末尾 + 1) を敗者にする。
	engine, err := EngineFromGame(game)
	if err != nil {
		return err
	}
	if engine.Turn == nil {
		return nil
	}
	loserID := playerIDForColor(game, *engine.Turn)
	if loserID == "" {
		return nil
	}
	winnerID := opponent(game, loserID)
	now := time.Now()
	game.IsEnded = true
	game.EndedAt = &now
	game.WinnerID = &winnerID
	game.TimeoutUserID = &loserID
	if err := s.repo.Update(game); err != nil {
		return err
	}
	s.publish(gameID, "ended", map[string]any{
		"winnerId": winnerID,
		"reason":   "timeout",
		"game":     packGame(game),
	})
	s.cleanupFedCache(ctx, gameID)
	return nil
}

// --- helpers ---

func (s *Service) publish(gameID, eventType string, body any) {
	if s.publisher == nil {
		return
	}
	s.publisher.PublishGameEvent(gameID, eventType, body)
}

func (s *Service) setTurnTimer(ctx context.Context, gameID string, turnIndex, seconds int) {
	if s.redis == nil || seconds <= 0 {
		return
	}
	key := turnTimerKey(gameID, turnIndex)
	s.redis.Set(ctx, key, "1", time.Duration(seconds)*time.Second)
}

func (s *Service) turnTimerExists(ctx context.Context, gameID string, turnIndex int) (bool, error) {
	if s.redis == nil {
		return true, nil
	}
	key := turnTimerKey(gameID, turnIndex)
	n, err := s.redis.Exists(ctx, key).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func turnTimerKey(gameID string, turnIndex int) string {
	return fmt.Sprintf("reversi:game:turnTimer:%s:%d", gameID, turnIndex)
}

func (s *Service) finalizeGame(_ context.Context, game *model.ReversiGame, engine *Game) error {
	now := time.Now()
	game.IsEnded = true
	game.EndedAt = &now
	crc := strconv.FormatUint(uint64(engine.CalcCRC32()), 10)
	game.CRC32 = &crc
	if winnerColor := engine.Winner(); winnerColor != nil {
		winnerID := playerIDForColor(game, *winnerColor)
		if winnerID != "" {
			game.WinnerID = &winnerID
		}
	}
	return s.repo.Update(game)
}

// isPlayer returns true when userID matches one of the game's two players.
func isPlayer(game *model.ReversiGame, userID string) bool {
	return game.User1ID == userID || game.User2ID == userID
}

// opponent returns the other player's id given userID.
func opponent(game *model.ReversiGame, userID string) string {
	if game.User1ID == userID {
		return game.User2ID
	}
	return game.User1ID
}

// playerColor computes a user's stone color from game.Black (1 | 2).
func playerColor(game *model.ReversiGame, userID string) (Color, error) {
	if game.Black == nil {
		return false, errors.New("game.Black not set")
	}
	user1IsBlack := *game.Black == 1
	switch userID {
	case game.User1ID:
		return colorFromBool(user1IsBlack), nil
	case game.User2ID:
		return colorFromBool(!user1IsBlack), nil
	}
	return false, ErrNotPlayer
}

// playerIDForColor is the inverse of playerColor — returns the user id that
// plays the given color.
func playerIDForColor(game *model.ReversiGame, color Color) string {
	if game.Black == nil {
		return ""
	}
	user1IsBlack := *game.Black == 1
	if color == Black {
		if user1IsBlack {
			return game.User1ID
		}
		return game.User2ID
	}
	if user1IsBlack {
		return game.User2ID
	}
	return game.User1ID
}

func colorFromBool(black bool) Color {
	if black {
		return Black
	}
	return White
}

// pickBlack returns 1 or 2 per the BW setting. For federated games
// (sessionID != ""), bw == "random" を session id の先頭コードポイントから
// 決定論的に導く (CherryPick startGame と一致させて両サイドで同じ bw を
// 得るため)。ローカル対戦では従来通り rand.Intn を使う。
func pickBlack(bw, sessionID string) int {
	switch bw {
	case "1":
		return 1
	case "2":
		return 2
	}
	// "random" など
	if sessionID != "" {
		// CherryPick: codePointAt(0) % 2 === 0 ? 1 : 2
		var cp rune
		for _, r := range sessionID {
			cp = r
			break
		}
		if cp%2 == 0 {
			return 1
		}
		return 2
	}
	//nolint:gosec // G404 — UX randomness, non-cryptographic is fine
	if rand.Intn(2) == 0 {
		return 1
	}
	return 2
}

// applySetting writes the typed raw JSON value into the matching game column.
func applySetting(game *model.ReversiGame, key string, raw json.RawMessage) error {
	switch key {
	case "map":
		var m []string
		if err := json.Unmarshal(raw, &m); err != nil {
			return ErrInvalidSetting
		}
		game.Map = m
	case "bw":
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			return ErrInvalidSetting
		}
		game.BW = fmt.Sprint(v)
	case "isLlotheo":
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return ErrInvalidSetting
		}
		game.IsLlotheo = b
	case "canPutEverywhere":
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return ErrInvalidSetting
		}
		game.CanPutEverywhere = b
	case "loopedBoard":
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return ErrInvalidSetting
		}
		game.LoopedBoard = b
	case "timeLimitForEachTurn":
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			return ErrInvalidSetting
		}
		if n < 0 {
			return ErrInvalidSetting
		}
		game.TimeLimitForEachTurn = n
	case "noIrregularRules":
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return ErrInvalidSetting
		}
		game.NoIrregularRules = b
	default:
		return ErrInvalidSetting
	}
	return nil
}

// EngineFromGame restores a reversi.Game engine from the persisted state by
// replaying the move log against the original map.
func EngineFromGame(game *model.ReversiGame) (*Game, error) {
	opts := Options{
		IsLlotheo:        game.IsLlotheo,
		CanPutEverywhere: game.CanPutEverywhere,
		LoopedBoard:      game.LoopedBoard,
	}
	engine := NewGame([]string(game.Map), opts)
	if len(game.Logs) == 0 {
		return engine, nil
	}
	var logs [][]int
	if err := json.Unmarshal(game.Logs, &logs); err != nil {
		return nil, fmt.Errorf("decode logs: %w", err)
	}
	// Log shape: [timeDelta, player, operation(0=put), pos]。過去の
	// 2要素 [pos, color] 形式との互換も残す (旧データ移行中のフォールバック)。
	for _, entry := range logs {
		switch {
		case len(entry) >= 4 && entry[2] == 0: // put operation
			engine.PutStone(entry[3])
		case len(entry) == 2: // legacy [pos, color]
			engine.PutStone(entry[0])
		}
	}
	return engine, nil
}

// packGame is a minimal JSON projection used by event bodies. 詳細ペイロード
// は api ハンドラ側の packGame を使うため、ここでは stream 配信に必要な最小の
// フィールドのみ含める。
func packGame(game *model.ReversiGame) map[string]any {
	out := map[string]any{
		"id":                   game.ID,
		"user1Id":              game.User1ID,
		"user2Id":              game.User2ID,
		"user1Ready":           game.User1Ready,
		"user2Ready":           game.User2Ready,
		"black":                game.Black,
		"isStarted":            game.IsStarted,
		"isEnded":              game.IsEnded,
		"winnerId":             game.WinnerID,
		"surrenderedUserId":    game.SurrenderedUserID,
		"timeoutUserId":        game.TimeoutUserID,
		"timeLimitForEachTurn": game.TimeLimitForEachTurn,
		"logs":                 game.Logs,
		"map":                  game.Map,
		"bw":                   game.BW,
		"isLlotheo":            game.IsLlotheo,
		"canPutEverywhere":     game.CanPutEverywhere,
		"loopedBoard":          game.LoopedBoard,
		"noIrregularRules":     game.NoIrregularRules,
		"startedAt":            game.StartedAt,
		"endedAt":              game.EndedAt,
	}
	// user1 / user2 (UserLite 互換) はフロントエンドの GameBoard が対戦
	// 相手のアバター描画などで必要とする。preload 済みなら埋め、nil なら
	// キー省略。entity パッケージに依存せず最小限のフィールドだけ手で組み
	// 立てる (#417 のレビューでこの形にした)。
	if game.User1 != nil {
		out["user1"] = userLiteMap(game.User1)
	}
	if game.User2 != nil {
		out["user2"] = userLiteMap(game.User2)
	}
	// winner は upstream Misskey TS の ReversiGameEntityService.packDetail と
	// 同じく winnerId から派生させて UserLite として埋める。frontend
	// (game.board.vue) は `v-if="game.winner"` で勝敗表示を切り替えるため、
	// winnerId だけでは draw に倒れて #649 の症状になる。
	if game.WinnerID != nil {
		if game.User1 != nil && game.User1.ID == *game.WinnerID {
			out["winner"] = userLiteMap(game.User1)
		} else if game.User2 != nil && game.User2.ID == *game.WinnerID {
			out["winner"] = userLiteMap(game.User2)
		}
	}
	return out
}

// userLiteMap builds a UserLite-compatible map without importing the
// entity package (core layer must not depend on presentation layer).
// フィールドは entity.PackUserLite の必須部分のみ (optional な TS 互換
// フィールドは省略)。avatar が空なら identicon URL を返すのは本家互換。
func userLiteMap(u *model.User) map[string]any {
	avatarURL := u.AvatarURL
	if avatarURL == nil || *avatarURL == "" {
		host := ""
		if u.Host != nil {
			host = "@" + *u.Host
		}
		identicon := "/identicon/" + u.Username + host
		avatarURL = &identicon
	}
	return map[string]any{
		"id":                u.ID,
		"name":              u.Name,
		"username":          u.Username,
		"host":              u.Host,
		"avatarUrl":         avatarURL,
		"avatarBlurhash":    u.AvatarBlurhash,
		"avatarDecorations": u.AvatarDecorations,
		"isBot":             u.IsBot,
		"isCat":             u.IsCat,
		"emojis":            map[string]string{},
		"onlineStatus":      "unknown",
		"badgeRoles":        []any{},
	}
}
