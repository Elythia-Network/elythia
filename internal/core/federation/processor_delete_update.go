package federation

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/repository"
)

// handleDelete removes a remote note (or actor) referenced by a Delete activity.
func (p *Processor) handleDelete(act genericActivity) error {
	targetURI, err := readObjectString(act.Object)
	if err != nil {
		return err
	}
	// こちらの引用が受けていた承認の取り消し (FEP-044f、#3234 段階 4)。承認の型か、
	// 型の分からない id のときだけ照合する。型の付いたノート (Tombstone / Note) と、
	// actor 自身の Delete (アカウント削除。object が actor の id) では引かない
	// (Mastodon も actor の削除を先に見る)。
	if p.quoteAnswers != nil && targetURI != act.Actor && mayBeQuoteAuthorization(act.Object) {
		handled, err := p.quoteAnswers.HandleRevocation(act.Actor, targetURI)
		if handled || err != nil {
			return err
		}
	}
	// upstream Misskey #17294 (= 2026.5.0 fix / triage #1001): object が Actor
	// (self-delete または object.type が Actor 系) で、その actor がローカルに
	// 存在しないなら無視する。これをやらないと ResolveActor が remote fetch を
	// 試みて 404 で失敗 → queue retry に乗り続ける挙動になる (= "存在しない
	// actor の delete を受け取り続けて queue が膨れる" 既存 bug)。
	if isActorDelete(act.Actor, targetURI, act.Object) {
		if _, ferr := p.userRepo.FindByURI(targetURI); ferr != nil {
			// **not-found だけ ack する** (#3116)。存在しない actor の delete は
			// retry しても結果が変わらないので、ここで止めないと queue が膨れる
			// (上のコメントの元の動機)。
			//
			// **DB 障害は伝播させる。** 以前は「他の DB error も ignore して
			// retry を避ける方がマシ」と書いて**種別を見ずに ack** していたが、
			// それだと一時的な接続断だけでリモートのアカウント削除を取りこぼす。
			// #3115 が Accept / Reject で確立した原則と正面から衝突していた。
			if repository.IsNotFound(ferr) {
				return nil
			}
			return fmt.Errorf("actor delete: lookup actor: %w", ferr)
		}
		// 存在する actor の delete は従来通り resolve に進む。
	}
	author, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	// Actor 自身の Delete (アカウント削除) は tombstone + cascade purge する
	// (#1220, upstream ApInboxService.deleteActor 相当)。
	if author.URI != nil && *author.URI == targetURI {
		return p.handleActorDelete(author)
	}
	note, err := p.noteRepo.FindByURI(targetURI)
	if err != nil {
		// 既に存在しないなら成功扱い。**障害は伝播させる** (#3116) —
		// ack すると job が成功扱いになって retry されず、**削除されたはずの
		// ノートがこちらに残り続ける**。
		if repository.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete note: lookup note: %w", err)
	}
	if note.UserID != author.ID {
		// 作者でない actor の Delete は何度送られても通らない。error で返すと、
		// 1 通で inbox の job が 8 回回って failed に積まれる (公開ノートの URI を
		// 知っていれば誰でも送れる)。upstream も文字列を返して ack する。
		slog.Warn("federation: dropping Delete of a note by a non-author", "actor", act.Actor, "note", note.ID)
		return nil
	}
	if p.noteDeleteSvc != nil {
		return p.noteDeleteSvc.Delete(author, note.ID)
	}
	return p.noteRepo.Delete(note)
}

// handleActorDelete processes a remote actor's self-delete (account deletion):
// the user is tombstoned (isDeleted=true) and a cascade purge of their notes /
// drive files / following is enqueued. Mirrors upstream
// ApInboxService.deleteActor.
//
// Local users are never deleted via inbound AP (loopback / spoof guard), and
// an already-deleted user is a no-op so AP retries stay idempotent. Marking
// runs before enqueue (upstream order); a purge-enqueue failure is logged but
// not retried (the tombstone is already committed, and re-running would skip on
// the idempotency guard anyway).
func (p *Processor) handleActorDelete(actor *model.User) error {
	if actor.IsLocal() {
		return nil
	}
	if actor.IsDeleted {
		return nil
	}
	if err := p.userRepo.UpdateUser(actor.ID, map[string]any{"isDeleted": true}); err != nil {
		return fmt.Errorf("actor delete: mark deleted: %w", err)
	}
	if p.accountDeleteEnqueuer != nil {
		// #2230: inbound Delete(actor) は remote user なので Soft=true で行を tombstone として
		// 残す。物理削除すると再連合 (resolve) でアカウントが復活し得る (upstream 同様)。
		if err := p.accountDeleteEnqueuer.EnqueueDeleteAccount(queue.DeleteAccountPayload{UserID: actor.ID, Soft: true}); err != nil {
			slog.Warn("actor delete: enqueue cascade purge failed", "userId", actor.ID, "err", err)
		}
	}
	return nil
}

// isActorDelete reports whether the given Delete activity targets an actor.
// Heuristics: (a) actor URI と object URI が一致するなら self-delete、
// (b) object が embedded 形式で type が Actor 系 (Person / Service / Application
// / Group / Organization) なら actor delete とみなす。
// upstream Misskey の isActor() / getApId() の組合せに相当する判定。
func isActorDelete(actorURI, targetURI string, object json.RawMessage) bool {
	if actorURI != "" && actorURI == targetURI {
		return true
	}
	switch strings.ToLower(peekObjectType(object)) {
	case "person", "service", "application", "group", "organization":
		return true
	}
	return false
}

// handleUpdate refreshes a remote actor's stored profile fields, or applies
// an inbound Update Note activity from a federated peer.
//
// Misskey 本家にはノート編集 API が無いが、Mastodon 系の Update Note は受信
// するだけ受信して反映する (`Resolver.UpdateRemoteNote` 経由)。それ以外の
// type (Question / Article 等) は no-op。
// Reversi Game 型の Update (CherryPick 拡張) はこの中で分岐する。
func (p *Processor) handleUpdate(act genericActivity) error {
	// 先に object の type を覗いて Note / Person / Game の判定を行う。
	objectType := peekObjectType(act.Object)
	if strings.EqualFold(objectType, "game") {
		// Game オブジェクトは reversi 拡張専用。未ワイヤなら skip。
		if !p.reversiReady() {
			return nil
		}
		if err := p.handleReversiUpdate(act); err != nil {
			if errors.Is(err, ErrNotReversiGame) {
				return nil
			}
			return err
		}
		return nil
	}
	if strings.EqualFold(objectType, "note") {
		// act.Actor を渡して note 著者との一致を検証させる (#1819、Question 経路と対称)。
		_, err := p.resolver.UpdateRemoteNote(act.Object, act.Actor, inboundActorHost(act.Actor))
		// ErrInvalidNote は受信側の不備として skip 扱い (200 を返す)。
		if errors.Is(err, ErrInvalidNote) {
			return nil
		}
		return err
	}
	if strings.EqualFold(objectType, "question") {
		// inbound Update(Question): リモート poll の vote 数を refresh する
		// (upstream ApInboxService.update -> apQuestionService.updateQuestion、#1779)。
		return p.resolver.UpdateRemoteQuestion(act.Object, act.Actor)
	}

	// object が単なる URI なら fetch、object 内に Person があればそれを使う
	var person activitypub.Person
	if err := json.Unmarshal(act.Object, &person); err != nil || person.ID == "" {
		// object が文字列 URI のケース
		uri, rerr := readObjectString(act.Object)
		if rerr != nil {
			return rerr
		}
		person.ID = uri
	}
	if person.Type != "" && !strings.EqualFold(person.Type.String(), "person") {
		// Note / Question / Game は上で dispatch 済。残り (Article 等) は未対応。
		return nil
	}
	// upstream ApInboxService.update は配送 actor 本人 (actor.uri) のみを再取得・
	// 更新する。object の id が配送 actor と異なる Update(Person) は、別 actor の
	// TTL-bypass 強制再取得 (amplification) を誘発するため拒否する (Note/Question
	// 経路と対称、#1848)。act.Actor は authorizeActor で署名者と一致を検証済み。
	if act.Actor != "" && person.ID != act.Actor {
		return nil
	}
	if _, err := p.userRepo.FindByURI(person.ID); err != nil {
		// 未取得のリモートユーザーなら無視 (次回 follow/inbox などで取り込まれる)。
		// **障害は伝播させる** (#3116) — ack するとプロフィールの更新が消える。
		if repository.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("update person: lookup user: %w", err)
	}
	// upstream ApInboxService.update -> ApPersonService.updatePerson は actor を
	// 強制再取得して name だけでなく avatar/banner/bio/fields/isBot/isCat/
	// isLocked/movedTo/alsoKnownAs/emojis 等の全プロフィールを更新する (#1560)。
	// mk-go も ForceResolveActor (TTL バイパスで refreshActor を回す既存経路、
	// handleMove と同じ) を呼んで full refresh する。旧実装は name のみ反映して
	// いた。fetch 失敗時は refreshActor が silent に既存値を維持する。
	if _, err := p.resolver.ForceResolveActor(person.ID); err != nil {
		return err
	}
	return nil
}

// peekObjectType reads only the "type" field from a JSON object body. パース
// 不能・object でない・type フィールド無しの場合は空文字を返す。
func peekObjectType(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	return obj.Type
}

// objectAPID extracts the AP id of an activity object whether it is encoded as
// a bare string URI or as an object with an "id" field (upstream getApId 相当)。
func objectAPID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// readObjectString と同じ理由で正規化する (#2662)。
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return trimWHATWGURL(s)
	}
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return trimWHATWGURL(obj.ID)
	}
	return ""
}

// isBearcapURI reports whether the object id is a bearcaps (`bear:`) URI.
// upstream create()/announce() は bearcaps を未対応として skip する
// (ApInboxService.ts:401,291、#1560)。mk-go の resolver も bear: を扱えないため
// fetch で error にする前に cleanly skip する。
func isBearcapURI(raw json.RawMessage) bool {
	return strings.HasPrefix(objectAPID(raw), "bear:")
}
