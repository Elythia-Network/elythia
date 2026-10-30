package federation

import (
	"encoding/json"
	"errors"
	"fmt"

	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// handleAdd processes an inbound Add activity. actorのfeaturedコレクションへの
// ノートピン留めを処理する。
func (p *Processor) handleAdd(act genericActivity) error {
	if p.pinningRepo == nil {
		return ErrUnsupportedActivity
	}
	actor, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	// targetがactorのfeaturedコレクションか確認
	var target struct {
		Target string `json:"target"`
	}
	_ = json.Unmarshal(act.raw, &target)
	if actor.Featured == nil || target.Target != *actor.Featured {
		return nil
	}
	noteURI, err := readObjectString(act.Object)
	if err != nil {
		return err
	}
	// ローカルノートならDBから検索、リモートならResolveNoteでフェッチ+取り込み
	note, err := p.noteRepo.FindByURI(noteURI)
	if err != nil {
		note, err = p.resolver.ResolveNoteForInbox(noteURI, inboundActorHost(act.Actor))
		if err != nil {
			return err
		}
	}
	if err := p.resolver.RequireInboundLocalNote(note, inboundActorHost(act.Actor)); err != nil {
		return err
	}
	// **ピン留めできるのは自分の投稿だけ。** これを見ないと、署名付きの
	// リモート actor が**他人のノートを自分のプロフィールに並べられる**。
	// ローカルの非公開ノートも対象になりうる (`ResolveNote` はローカル URI を
	// 可視性を見ずに引く) ので、`users/show` の `pinnedNoteIds` 経由でその ID が
	// 未認証の相手に出る。
	//
	// **同じ規則が `featured.go` に既にある** (pull 側の featured 取り込み)。
	// こちらの push 側 (`Add`) だけが取り残されていた。upstream も
	// `NotePiningService.addPinned` が `findOneBy({id, userId: user.id})` で
	// 弾いている。
	if note.UserID != actor.ID {
		return nil
	}
	// **件数にも上限を置く。** `Add` は署名さえ通れば何度でも送れるので、
	// 上限が無いと `user_note_pining` を無制限に増やせる。しかも
	// `internal/api/users/handler.go` の設計メモ (#1489) は `pinnedNoteIds` を
	// 絞らない理由として「pinning は per-user 上限が厳しい (= 数件) ので
	// mass enumeration リスクは低い」と書いており、**その前提がここで壊れる**。
	//
	// 値は pull 側の `featuredPinLimit` と同じ。片方だけ緩いと、同じ
	// リモート actor が経路によって違う上限を受けることになる。
	//
	// **上限に当たったら古い方を外して入れ替える。捨てない。** upstream は
	// `Remove(A)` と `Add(B)` を**別々の deliver job** で配る
	// (`NotePiningService.deliverPinnedChange`)。こちらの inbox は並列に
	// 処理するので `Add(B)` が先に着くことがあり、そこで捨てると
	// (`return nil` は inbox job を成功扱いにするので再試行されない) B は
	// **次の actor 更新まで現れない** (既定 TTL 24 時間)。
	//
	// **再試行にはしない。** upstream は throw して job を再送させるが、
	// mk-go でそれをやると、上限を超える `Add` を投げ続けるだけで job が
	// 8 倍に膨らむ — この上限が防いでいる負荷を別の形で呼び込む。入れ替えなら
	// 行数は上限に収まったまま、順序の入れ替わりも拾える。
	//
	// **数えられなかったら errで返す。** ここで握り潰すと上限が効かないまま
	// 進むし、`nil` で捨てると inbox job が「処理済み」として再試行しない。
	// error なら再試行されるので、DB 障害が上限の無効化にも取りこぼしにも
	// 化けない (#2792 と同じ判断)。
	// **既にピン留め済みなら何もしない。** 再送や順序の入れ替わりで同じ
	// `Add` が二度届く。上限判定より前に置く — ここを通すと「既にあるものを
	// 入れるために別のピンを外す」ことになる。
	switch existing, ferr := p.pinningRepo.FindByPair(actor.ID, note.ID); {
	case ferr == nil && existing != nil:
		return nil
	case ferr != nil && !repository.IsNotFound(ferr):
		// **障害を「まだ無い」に倒さない** (#3116)。倒すと上限判定の側へ進み、
		// 「既にあるものを入れるために別のピンを外す」ことになる。
		return fmt.Errorf("pin dedup: %w", ferr)
	}
	count, cerr := p.pinningRepo.CountByUser(actor.ID)
	if cerr != nil {
		return fmt.Errorf("count pinned notes: %w", cerr)
	}
	if count >= featuredPinLimit {
		rows, lerr := p.pinningRepo.ListByUser(actor.ID)
		if lerr != nil {
			return fmt.Errorf("list pinned notes: %w", lerr)
		}
		// `ListByUser` は id DESC なので末尾が最も古い。新しい方を
		// `featuredPinLimit - 1` 件残して、あふれる分を外す。
		for i := featuredPinLimit - 1; i < len(rows); i++ {
			if derr := p.pinningRepo.Delete(rows[i]); derr != nil {
				return fmt.Errorf("evict pinned note: %w", derr)
			}
		}
	}
	now := nowFn()
	pin := &model.UserNotePining{
		ID:     p.pinningIDGen.Generate(now),
		UserID: actor.ID,
		NoteID: note.ID,
	}
	if err := p.pinningRepo.Create(pin); err != nil {
		// 既にピン留め済み（重複キー）の場合のみ無視
		if existing, _ := p.pinningRepo.FindByPair(actor.ID, note.ID); existing != nil {
			return nil
		}
		return err
	}
	return nil
}

// handleRemove processes an inbound Remove activity. actorのfeaturedコレクションから
// ノートのピン留めを解除する。
func (p *Processor) handleRemove(act genericActivity) error {
	var target struct {
		Target string `json:"target"`
	}
	_ = json.Unmarshal(act.raw, &target)
	// chat room target の Remove は group chat の leave (#1364)。featured pin
	// より先に判定する (pinningRepo 未配線でも chat leave は処理する)。actor の
	// membership を削除する (cherrypick ApInboxService.remove と同じく actor 基準)。
	if roomURI := chatRoomURI(target.Target); roomURI != "" && p.chatRoomReceiver != nil {
		actor, err := p.resolver.ResolveActor(act.Actor)
		if err != nil {
			return err
		}
		if err := p.chatRoomReceiver.RemoveMemberViaAP(roomURI, actor.ID); err != nil {
			if errors.Is(err, corechat.ErrNotFound) || errors.Is(err, corechat.ErrInvalidTarget) {
				return ErrUnsupportedActivity
			}
			return err
		}
		return nil
	}
	if p.pinningRepo == nil {
		return ErrUnsupportedActivity
	}
	actor, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	if actor.Featured == nil || target.Target != *actor.Featured {
		return nil
	}
	noteURI, err := readObjectString(act.Object)
	if err != nil {
		return err
	}
	note, err := p.noteRepo.FindByURI(noteURI)
	if err != nil {
		// 理由は handleAccept と同じ (#3115)。ここで DB 障害を ack すると
		// job が成功扱いになって retry されず、pin が外れないまま残る。
		if repository.IsNotFound(err) {
			return nil
		}
		return err
	}
	pin, err := p.pinningRepo.FindByPair(actor.ID, note.ID)
	if err != nil {
		if repository.IsNotFound(err) {
			return nil
		}
		return err
	}
	// **書き込みの失敗も伝播させる** (#3115)。lookup だけ retry に倒して
	// ここを捨てると、「pin が外れないまま残る」という同じ結末が DELETE 側で
	// 残る。対になる `handleAdd` は Create / eviction の Delete を両方伝播する。
	if err := p.pinningRepo.Delete(pin); err != nil {
		return fmt.Errorf("unpin note: %w", err)
	}
	return nil
}
