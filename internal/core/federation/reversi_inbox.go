package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/datatypes"
)

// ReversiStreamPublisher forwards per-user reversi events (Invite arrival)
// to the `reversi:<userID>` pub/sub topic so that the WebSocket channel
// can push `invited` in real-time to the recipient's UI (#417 P2). 実装は
// internal/stream.ReversiGamePublisher。未設定なら publish しない。
type ReversiStreamPublisher interface {
	PublishInvited(targetUserID string, inviter *model.User)
}

// --- Incoming reversi AP activities (CherryPick compatible) ---
//
// All four activity types carry a custom Game object as `object`, structured
// as APGame: { type: "Game", game_type_uuid: "<fixed>", game_state: {...} }.
// The inbox handler dispatches based on the top-level activity type
// (Invite / Join / Leave / Update) and, for Update, the inner
// game_state.type (settings | ready_states | putstone).
//
// ゲームの突合は federationId (= game_state.game_session_id) を
// ReversiRepository.FindByFederationID で検索する。
// Invite のみ例外的にまだ DB に行がないため、ここで行を作成する。

// handleReversiInvite processes an incoming Invite from a remote player.
// ローカルフロントエンドに「対戦相手選択画面」が無い問題を回避するため、
// Invite 受信時点で自動的にローカル reversi_game 行を作成し、招待された
// ローカルユーザー (user2) が通常の ready フローで対戦できる状態にする。
// session id → game id の mapping は Redis (FederationIDCache) に保存する。
func (p *Processor) handleReversiInvite(act genericActivity) error {
	if !p.reversiReady() {
		return ErrUnsupportedActivity
	}
	actor, gameObj, state, err := p.parseReversiActivity(act)
	if err != nil {
		return err
	}
	if gameObj == nil || state.GameSessionID == "" {
		return errors.New("reversi invite: missing game state")
	}
	toURI := parseToURI(act.To)
	if toURI == "" {
		return errors.New("reversi invite: missing recipient")
	}
	// ローカルユーザーの user.uri は DB 上 NULL なので FindByURI では解決
	// できない。localBaseURL の接頭辞から ID を読む resolveTargetUser を使って
	// `{localBaseURL}/users/{id}` 形式もローカル ID として解決する。
	invitee, err := p.resolveTargetUser(toURI)
	if err != nil {
		return fmt.Errorf("reversi invite: recipient %s not found", toURI)
	}
	// **招待先がローカル利用者であることを確かめる。**
	//
	// `resolveTargetUser` はローカル prefix に一致しなければ `FindByURI` へ
	// 落ちるので、リモートの利用者も解決される。見ないと、第三者が
	// 「リモート同士の対局行」を作れる。同じディレクトリの chat room 招待は
	// `!invitee.IsLocal()` で落としており、こちらだけ抜けていた。
	if invitee == nil || !invitee.IsLocal() {
		slog.Warn("reversi invite: invitee is not a local user", "target", toURI)
		return ErrUnsupportedActivity
	}
	// **ブロックしている相手からの招待は通さない。** 通すと、ブロック済みの
	// リモート利用者が対象の `reversi:<userID>` ストリームへ `invited` を
	// push できる。
	if p.blockingService != nil {
		blocked, berr := p.blockingService.IsBlocked(invitee.ID, actor.ID)
		if berr != nil {
			// 判定できないときは通さない (#2792 と同じ向き)。
			return fmt.Errorf("reversi invite: cannot check blocking: %w", berr)
		}
		if blocked {
			slog.Debug("reversi invite: dropped from a blocked actor", "actor", actor.ID)
			return nil
		}
	}
	ctx := context.Background()
	// 既にこの sessionID に対応する game が Redis に居れば二重処理を防ぐ。
	if gid, gerr := p.reversiFedCache.Get(ctx, state.GameSessionID); gerr == nil && gid != "" {
		return nil
	}

	// CherryPick は fresh session_id で Invite を 5 秒ごと再送するため、
	// 同じ (inviter, invitee) の pending game があれば session mapping を
	// 最新に差し替えて再利用する。これが無いと招待ごとに row が増殖する。
	if existing := corereversi.FindPendingInvitation(p.reversiRepo, invitee.ID, actor.ID); existing != nil {
		if oldSession, ok := p.reversiFedCache.GetSessionByGame(ctx, existing.ID); ok && oldSession != state.GameSessionID {
			p.reversiFedCache.Delete(ctx, oldSession, "")
		}
		p.reversiFedCache.Set(ctx, state.GameSessionID, existing.ID)
		slog.Info("reversi federation: invite session refreshed",
			"gameId", existing.ID, "session", state.GameSessionID, "inviter", actor.ID, "invitee", invitee.ID)
		return nil
	}

	now := nowFn()
	game := &model.ReversiGame{
		ID:                   p.reversiIDGen.Generate(now),
		User1ID:              actor.ID, // inviter = remote
		User2ID:              invitee.ID,
		Map:                  defaultReversiMap(),
		BW:                   "random",
		TimeLimitForEachTurn: 90,
		Logs:                 datatypes.JSON("[]"),
	}
	if err := p.reversiRepo.Create(game); err != nil {
		return fmt.Errorf("reversi invite: create game: %w", err)
	}
	p.reversiFedCache.Set(ctx, state.GameSessionID, game.ID)
	slog.Info("reversi federation: accepted invite",
		"gameId", game.ID, "session", state.GameSessionID, "inviter", actor.ID, "invitee", invitee.ID)
	// 招待されたユーザーの reversi stream に `invited` を push することで
	// Misskey/CherryPick フロントがリアルタイムで招待カードを表示する
	// (#417 P2: polling/reload 依存の解消)。
	if p.reversiStreamPub != nil {
		p.reversiStreamPub.PublishInvited(invitee.ID, actor)
	}
	return nil
}

// handleReversiJoin processes an incoming Join for a game we (the local
// inviter) previously sent Invite for. CherryPick semantics: the remote
// player has accepted; no local state change beyond acknowledging existence.
func (p *Processor) handleReversiJoin(act genericActivity) error {
	if !p.reversiReady() {
		return ErrUnsupportedActivity
	}
	_, gameObj, state, err := p.parseReversiActivity(act)
	if err != nil {
		return err
	}
	if gameObj == nil || state.GameSessionID == "" {
		return errors.New("reversi join: missing game state")
	}
	gameID, err := p.reversiFedCache.Get(context.Background(), state.GameSessionID)
	if err != nil || gameID == "" {
		return fmt.Errorf("reversi join: unknown session %s", state.GameSessionID)
	}
	slog.Info("reversi federation: peer joined", "session", state.GameSessionID, "gameId", gameID)
	return nil
}

// handleReversiLeave processes a Leave — remote player either surrenders
// (started game) or cancels (pre-start). Dispatch accordingly.
func (p *Processor) handleReversiLeave(act genericActivity) error {
	if !p.reversiReady() {
		return ErrUnsupportedActivity
	}
	actor, gameObj, state, err := p.parseReversiActivity(act)
	if err != nil {
		return err
	}
	if gameObj == nil || state.GameSessionID == "" {
		return errors.New("reversi leave: missing game state")
	}
	ctx := context.Background()
	gameID, err := p.reversiFedCache.Get(ctx, state.GameSessionID)
	if err != nil || gameID == "" {
		return fmt.Errorf("reversi leave: unknown session %s", state.GameSessionID)
	}
	game, err := p.reversiRepo.FindByID(gameID)
	if err != nil {
		return fmt.Errorf("reversi leave: game %s gone", gameID)
	}
	// Service.Surrender / CancelGame が fedCache cleanup も担う
	// (#417 Devin review で全終了経路を Service 側に統一)。
	if game.IsStarted {
		return p.reversiSvc.Surrender(ctx, game.ID, actor.ID)
	}
	return p.reversiSvc.CancelGame(ctx, game.ID, actor.ID)
}

// handleReversiUndoInvite processes an Undo(Invite) — the remote inviter is
// retracting a pre-start invitation. We locate the game by session id and
// route through Service.CancelGame so fedCache cleanup / `canceled` stream
// event fire uniformly (#417 P4).
//
// inner は Undo.object に入っていた元 Invite で、その object が reversi Game。
// reversi Game でない Undo(Invite) は別機能 (未実装) なので ErrNotReversiGame
// で dispatcher に戻し、一般 Undo パスで無視される。
func (p *Processor) handleReversiUndoInvite(act genericActivity, inner genericActivity) error {
	if !p.reversiReady() {
		return ErrUnsupportedActivity
	}
	actor, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	// JSON 構造は Undo → Invite → Game の二段ネスト。act.Object は Invite 全体
	// (= inner)、inner.Object が更にその中の Game 本体。ここで parse したいのは
	// Game なので inner.Object を使う (act.Object ではない)。
	var objMap map[string]any
	if err := json.Unmarshal(inner.Object, &objMap); err != nil {
		return fmt.Errorf("reversi undo(invite): object not a JSON object: %w", err)
	}
	if !corereversi.IsReversiGame(objMap) {
		return ErrNotReversiGame
	}
	state := corereversi.ParseGameState(objMap)
	if state == nil || state.GameSessionID == "" {
		return errors.New("reversi undo(invite): missing game_state")
	}
	ctx := context.Background()
	gameID, err := p.reversiFedCache.Get(ctx, state.GameSessionID)
	if err != nil || gameID == "" {
		// CherryPick は session TTL 切れ後にも undo を送り得るが既に消えて
		// いるので ack 扱い (return nil)。
		slog.Info("reversi undo(invite): unknown session, ignoring",
			"session", state.GameSessionID, "actor", actor.ID)
		return nil
	}
	game, err := p.reversiRepo.FindByID(gameID)
	if err != nil {
		// fedCache hit なのに repo miss は inconsistent state。
		// handleReversiLeave と同じく err を返して可観測性を上げる。
		return fmt.Errorf("reversi undo(invite): game %s gone", gameID)
	}
	if game.IsStarted {
		// Undo(Invite) は pre-start 専用。started 後は Leave を使うべき。
		// 受信しても副作用無しで ack する。
		slog.Warn("reversi undo(invite): game already started, ignoring",
			"gameId", game.ID, "actor", actor.ID)
		return nil
	}
	slog.Info("reversi federation: undo(invite) received",
		"gameId", game.ID, "session", state.GameSessionID, "actor", actor.ID)
	return p.reversiSvc.CancelGame(ctx, game.ID, actor.ID)
}

// handleReversiUpdate dispatches an Update carrying a reversi Game object.
// Called from handleUpdate when the object type is "Game" with the reversi
// UUID. Update semantics come from game_state.type:
//   - settings    → UpdateSettings(key, value)
//   - ready_states → UpdateReady(ready)
//   - putstone    → PutStone(pos)
func (p *Processor) handleReversiUpdate(act genericActivity) error {
	if !p.reversiReady() {
		return ErrUnsupportedActivity
	}
	actor, gameObj, state, err := p.parseReversiActivity(act)
	if err != nil {
		return err
	}
	if gameObj == nil || state.GameSessionID == "" {
		return errors.New("reversi update: missing game state")
	}
	ctx := context.Background()
	gameID, err := p.reversiFedCache.Get(ctx, state.GameSessionID)
	if err != nil || gameID == "" {
		return fmt.Errorf("reversi update: unknown session %s", state.GameSessionID)
	}
	game, err := p.reversiRepo.FindByID(gameID)
	if err != nil {
		return fmt.Errorf("reversi update: game %s gone", gameID)
	}
	slog.Info("reversi inbox: update received",
		"gameId", game.ID, "session", state.GameSessionID, "actorID", actor.ID, "subtype", state.Type,
		"readyPtr", state.Ready != nil, "posPtr", state.Pos != nil, "key", state.Key)
	switch strings.ToLower(state.Type) {
	case "settings":
		if state.Key == "" {
			return errors.New("reversi update settings: missing key")
		}
		raw, _ := json.Marshal(state.Value)
		return p.reversiSvc.UpdateSettings(ctx, game.ID, actor.ID, state.Key, raw)
	case "ready_states":
		if state.Ready == nil {
			return errors.New("reversi update ready: missing ready flag")
		}
		return p.reversiSvc.UpdateReady(ctx, game.ID, actor.ID, *state.Ready)
	case "putstone":
		if state.Pos == nil {
			return errors.New("reversi update putstone: missing pos")
		}
		// 連合経由の手番には client op id が無いため空文字を渡す (#1549)。
		return p.reversiSvc.PutStone(ctx, game.ID, actor.ID, *state.Pos, "")
	}
	return fmt.Errorf("reversi update: unknown game_state.type %q", state.Type)
}

// --- helpers ---

// reversiReady reports whether all reversi federation dependencies are wired.
func (p *Processor) reversiReady() bool {
	return p.reversiSvc != nil && p.reversiRepo != nil && p.reversiIDGen != nil
}

// parseReversiActivity resolves the actor, parses the activity `object` as a
// reversi Game, and returns the extracted game state. Activities whose object
// is not a reversi Game (identified by game_type_uuid) are rejected with a
// dedicated error so the caller can distinguish "not for us" from "malformed".
func (p *Processor) parseReversiActivity(act genericActivity) (*model.User, map[string]any, corereversi.APGameState, error) {
	actor, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return nil, nil, corereversi.APGameState{}, err
	}
	var objMap map[string]any
	if err := json.Unmarshal(act.Object, &objMap); err != nil {
		return nil, nil, corereversi.APGameState{}, fmt.Errorf("reversi: object not a JSON object: %w", err)
	}
	if !corereversi.IsReversiGame(objMap) {
		return nil, nil, corereversi.APGameState{}, ErrNotReversiGame
	}
	state := corereversi.ParseGameState(objMap)
	if state == nil {
		return nil, nil, corereversi.APGameState{}, errors.New("reversi: missing game_state")
	}
	return actor, objMap, *state, nil
}

// parseToURI extracts a single recipient URI from the raw `to` field of an
// activity. Accepts string, []string, or null (返り値は先頭要素 / 空文字)。
func parseToURI(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		return arr[0]
	}
	return ""
}

// ErrNotReversiGame is returned by parseReversiActivity when the object
// payload is not a CherryPick-compatible reversi Game.
var ErrNotReversiGame = errors.New("activity object is not a reversi game")

// defaultReversiMap returns the canonical 8x8 starting board used by
// auto-created invites. 本家と同じ初期配置。
func defaultReversiMap() []string {
	return []string{
		"--------",
		"--------",
		"--------",
		"---wb---",
		"---bw---",
		"--------",
		"--------",
		"--------",
	}
}
