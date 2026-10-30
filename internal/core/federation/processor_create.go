package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/bgwork"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/model"
)

// mergeCreateAudience unions the Create activity's to/cc onto the carried Note
// object's to/cc and fills the object's attributedTo from the activity actor
// when absent, mirroring upstream ApInboxService.create
// (packages/backend/src/.../ApInboxService.ts:403-417). The merged JSON is what
// IngestNoteWithCreated parses, so visibility (public/home/followers/specified)
// and visibleUserIds are derived from the combined audience rather than the
// Note object's own to/cc only (#1560).
//
// 本家は activity / object 双方の to/cc を同一 union 値に揃えるが、mk-go は
// object 側しか後段で参照しないので object の to/cc のみ書き戻す。union は
// 順序保持 dedup で、activity 側を先・object 側を後に連結する (本家 concat 順)。
//
// object が JSON object でない (string IRI 等) / parse 不能なケースでは元の
// act.Object をそのまま返し、既存挙動へ degrade する (fail-safe)。union 自体は
// visibility を広げる方向ではなく、Create 側にしか無い followers/specified の
// audience を拾うので、誤って public 化する事故を防ぐ fail-closed 寄りの補正。
func mergeCreateAudience(act genericActivity) json.RawMessage {
	if len(act.Object) == 0 {
		return act.Object
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(act.Object, &obj); err != nil || obj == nil {
		// object が embedded JSON object でなければ何もできない (本家も
		// `typeof activity.object === 'object'` でガードしている)。
		return act.Object
	}

	actTo := decodeAudience(act.To)
	actCC := decodeAudience(act.CC)
	objTo := decodeAudience(obj["to"])
	objCC := decodeAudience(obj["cc"])

	mergedTo := unionStrings(actTo, objTo)
	mergedCC := unionStrings(actCC, objCC)

	mutated := false
	if encoded, ok := encodeAudienceIfChanged(obj["to"], mergedTo); ok {
		obj["to"] = encoded
		mutated = true
	}
	if encoded, ok := encodeAudienceIfChanged(obj["cc"], mergedCC); ok {
		obj["cc"] = encoded
		mutated = true
	}

	// object.attributedTo が無ければ activity.actor で補う。IngestNote は空の
	// attributedTo を ErrInvalidNote で弾くので、Create actor から埋めて本家の
	// `activity.object.attributedTo = activity.actor` に揃える。
	if _, present := obj["attributedTo"]; !present && act.Actor != "" {
		if encoded, err := json.Marshal(act.Actor); err == nil {
			obj["attributedTo"] = encoded
			mutated = true
		}
	}

	if !mutated {
		return act.Object
	}
	merged, err := json.Marshal(obj)
	if err != nil {
		return act.Object
	}
	return merged
}

// decodeAudience parses an AP audience field into a []string. The value may be
// a single id, an object carrying `id`, or an array of either. Unreadable
// elements are dropped. Empty / null input yields nil.
func decodeAudience(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	// APIDList は upstream getApIds と同じく `{"id": ...}` 形式の要素も拾う。
	// `[]string` 決め打ちだと object 要素で unmarshal ごと失敗し audience が丸ごと
	// 落ちる (#2662)。
	var list activitypub.APIDList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	return list
}

// unionStrings concatenates a then b, preserving first-seen order and dropping
// duplicates, matching upstream `unique(concat([toArray(a), toArray(b)]))`.
func unionStrings(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, v := range a {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	for _, v := range b {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// encodeAudienceIfChanged re-encodes merged as a JSON array and reports whether
// it differs from the original raw value. Returns ok=false (no write) when the
// merged audience is empty so a missing field is not materialised as `[]`, and
// when the canonical array form already equals the original (avoids needless
// string→array normalisation churn). 元と等価なら書き戻さないことで、純粋に
// audience 追加が無い一般 note では act.Object をそのまま素通しする。
func encodeAudienceIfChanged(raw json.RawMessage, merged []string) (json.RawMessage, bool) {
	if len(merged) == 0 {
		return nil, false
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, false
	}
	if bytes.Equal(bytes.TrimSpace(raw), encoded) {
		return nil, false
	}
	return encoded, true
}

// handleCreate persists an inbound Note or Question carried by a Create activity.
// Question (投票) はNote同様にIngestNoteで処理され、Pollレコードが自動作成される。
// ノート取込み成功後、fanoutHookが配線されていればタイムライン/ストリーミングに
// 配信する (#330)。
//
// `_misskey_talk: true` flag が立った Note は CherryPick / レガシー Misskey の
// 1-on-1 chat federation。notes テーブルではなく chat_messages として処理する
// ため IngestNote をスキップして chatService にルートする (#692)。
func (p *Processor) handleCreate(act genericActivity, signer *model.User) error {
	// bearcaps (bear:) object URI は未対応として skip する (#1560、upstream
	// ApInboxService.create の 'skip: bearcaps url not supported')。
	if isBearcapURI(act.Object) {
		slog.Info("federation: skipping Create with bearcaps object url", "actor", act.Actor)
		return nil
	}
	// Mastodon 系リレーは Announce ではなく Create を LD-Signature 付きで
	// 転送する (#2338)。配送してきたのが購読中のリレーなら、著者を DB に
	// 作らず ephemeral 経路で解決する。
	//
	// signer が nil (署名なし経路 / 未配線) のときは従来どおり DB 経路に
	// 倒れる (fail-safe)。
	viaRelay := p.isRelayDelivery(signer)
	actor, err := p.resolveCreateActor(act.Actor, viaRelay)
	if err != nil {
		if errors.Is(err, ErrHostNotAllowed) {
			// federation policy で許可されない host の actor からの Create は
			// retry で解消しないため ack して drop する (#1419 review)。署名検証
			// 経路では verifyPayload が先に弾くが、署名なし inbound 経路でも
			// retry ループにならないようここでも吸収する。
			slog.Info("federation: dropping Create from non-federated host actor",
				"actor", act.Actor)
			return nil
		}
		return err
	}
	// Note の `_misskey_talk` を覗き見て chat か通常 note かを分岐する。
	// IngestNote が走る前に短絡しないと chat message が notes テーブルに
	// 流れ込んでタイムラインを汚す。1-on-1 は chatService、group (room) は
	// chatRoomReceiver が処理するのでどちらか配線済みなら probe する。
	if p.chatService != nil || p.chatRoomReceiver != nil {
		var probe struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Content string `json:"content"`
			// **MFM の原文も拾う。** `content` は HTML なので、MFM に戻すと
			// 情報が落ちる (装飾・色・絵文字・引用の改行など)。note 取り込みは
			// 同じ 3 段 (source -> _misskey_content -> content) で拾っており、
			// chat だけ `content` しか見ていなかった。
			Source *struct {
				Content   string `json:"content"`
				MediaType string `json:"mediaType"`
			} `json:"source"`
			MisskeyContent string          `json:"_misskey_content"`
			MisskeyTalk    bool            `json:"_misskey_talk"`
			To             json.RawMessage `json:"to"`
			Context        json.RawMessage `json:"@context"`
		}
		if err := json.Unmarshal(act.Object, &probe); err == nil && probe.MisskeyTalk && probe.Type == "Note" {
			mfmSource := ""
			if probe.Source != nil && probe.Source.MediaType == "text/x.misskeymarkdown" {
				mfmSource = probe.Source.Content
			}
			if mfmSource == "" {
				mfmSource = probe.MisskeyContent
			}
			// **メッセージの id を配送してきた actor のホストに縛る。**
			//
			// この分岐は `ingestCreateNote` の手前で短絡するので、通常の note に
			// 掛かる `id host == attributedTo host` の検査 (`resolver_note_ingest.go` の
			// `validateNote` 相当) を一度も通らない。縛らないと、署名が通る
			// リモート actor が**任意ホストの URI を名乗って chat メッセージを
			// 1 通送れる**。以後その URI を使う正規の連合 chat メッセージは
			// `FindMessageByURI` が hit して黙って捨てられる (= 相手のメッセージを
			// 先回りして潰せる)。
			//
			// **`sameDeliveryHost` を使う。`assertRequestHostMatches` では緩い。**
			// あちらは `normalizeMatchHost` 経由で **`www.` を剥がして同一視する**
			// ので、`www.remote.example` の actor が `remote.example` の id を
			// 名乗れてしまう。`normalizeMatchHost` 自身のコメントが「actor が
			// 申告する値の host 検証には `sameDeliveryHost` を使うこと」と書いて
			// いて (#2662)、chat の id はまさに actor の申告値。note 側の
			// `validateNote` も `punyHost` 比較で `www.` を剥がさない。
			//
			// **1-on-1 と room の両方の手前に置く。** どちらも同じ `probe.ID` を
			// メッセージの uri として保存する。
			if !sameDeliveryHost(act.Actor, probe.ID) {
				slog.Warn("federation: chat message id host does not match delivering actor",
					"actor", act.Actor, "id", probe.ID)
				// retry では解決しないので ack して drop する。
				return ErrUnsupportedActivity
			}
			// note の @context が room URI なら group chat message (#1209)。
			// それ以外は従来の 1-on-1 DM。
			if roomURI, isRoom := chatRoomURIFromContext(probe.Context); isRoom {
				return p.handleChatRoomMessageCreate(actor, probe.ID, probe.Content, mfmSource, roomURI)
			}
			return p.handleChatCreate(actor, probe.ID, probe.Content, mfmSource, probe.To, inboundActorHost(act.Actor))
		}
	}
	// 本家 ApInboxService.create は resolve 前に activity.to/cc を Note object の
	// to/cc に union し、object.attributedTo が無ければ activity.actor で埋める
	// (#1560)。これをしないと audience を Create 側だけに載せる実装 (一部 Mastodon
	// 系) の note が followers/specified を public 等に誤判定し得る。visibility は
	// fail-closed に倒すため union 後の object を ingest に渡す。union に失敗した
	// 場合は元の object を使う (= 既存挙動へ degrade)。
	object := mergeCreateAudience(act)
	// act.Actor を配送 actor として渡し、note の著者 (attributedTo) が配送者本人で
	// あることを検証させる (なりすまし forge 防止、#1839)。
	note, created, err := p.ingestCreateNote(object, act.Actor, viaRelay, inboundActorHost(act.Actor))
	if errors.Is(err, corenote.ErrContainsTooManyMentions) {
		// upstream Misskey #17167 (= 2026.5.0 fix / triage #1004): role policy
		// 由来の "note contains too many mentions" は永続的に解決しない error な
		// ので queue retry に乗せず ack して drop する。upstream は同種 error を
		// IdentifiableError('9f466dab-...') で switch する patch だが、mk-go は
		// sentinel error の errors.Is で同等の non-retry 化を行う。
		// 上限は投稿者の role policy で決まる (#3330) ので、既定値を記録しない。
		slog.Info("federation: dropping inbound note exceeding mentionLimit",
			"actor", act.Actor)
		return nil
	}
	if errors.Is(err, ErrHostNotAllowed) {
		// federation policy (none / specified / blockedHosts) で許可されない
		// host の Create(Note) は retry で解消しないため ack して drop する
		// (#1419 review)。handleAnnounce 等は isPermanentSkipError で吸収する
		// が、handleCreate は他の error を retry に乗せる方針なので明示分岐。
		slog.Info("federation: dropping inbound Create(Note) from non-federated host",
			"actor", act.Actor)
		return nil
	}
	if errors.Is(err, ErrNoteAttributionMismatch) {
		// id/attributedTo host 不一致・attribution≠配送actor (なりすまし forge) は
		// retry で解消しないため ack して drop する (#1839)。
		slog.Info("federation: dropping forged inbound Create(Note) (attribution mismatch)",
			"actor", act.Actor)
		return nil
	}
	if errors.Is(err, ErrInvalidNote) {
		// 形の壊れた note (object が文字列の URI だけ、必須の欄が無い等) も送り
		// 直しで直らない。error で返すと 1 通で inbox の job が 8 回回って failed に
		// 積まれていた。
		slog.Info("federation: dropping inbound Create with an invalid note", "actor", act.Actor, "err", err)
		return nil
	}
	if err != nil {
		return err
	}
	if note != nil {
		// IngestNoteが既存ノートを返した場合 (重複) でもfanout自体は
		// べき等なので呼んで問題ない (通知フックは冪等でないので下で created
		// ゲートを掛ける)。authorはnoteに紐付くUserを使うが、
		// IngestNoteはUser fieldを設定しないためactorを使う。
		// Renote/Reply relation は IngestNote 直後では未ロード (ID だけ) な
		// ので、ストリーミング payload で renote 先が null にならないように
		// preload 付きで再取得する (#416)。
		//
		// Reply / Renote が無い「素の Create(Note)」(連合の大半を占める一般
		// 投稿) では preload する relation が無いので reload を skip する
		// (#569)。User 関係だけ caller の actor から手で詰める。これにより
		// inbox worker drain time の支配的コストの 1 つだった redundant
		// SELECT が消える。
		hydrated := note
		if note.ReplyID != nil || note.RenoteID != nil {
			hydrated = hydrateNoteForFanout(p.noteRepo, note)
		} else if note.User == nil {
			note.User = actor
		}
		// fanoutHook / notificationHook はベストエフォートで、local note
		// create 側 (note_create_service) では既に safeGo で非同期発火して
		// いる。federation 側もこれに揃え、worker が Redis LPUSH / publish
		// 待ちで block されないようにする (#569)。順序保証は Misskey TS と
		// 同じく各 hook 側の冪等性で吸収する。
		if p.fanoutHook != nil {
			p.goHook(func() { p.fanoutHook.OnNoteCreated(hydrated, actor) })
		}
		// **リレー経由では発火させない (#2743)。** Mastodon 系リレーは元の
		// Create をそのまま転送して署名だけがリレーのものになるので、
		// ingestCreateNote と同じ viaRelay で判定する。これを見落とすと
		// ephemeral 有効時に DB に行が無い note が antenna の ZSET に積まれ、
		// 上限 200 を幽霊 ID が埋める。
		// **`created` では gate しない。** notification / chart hook は dedup hit を
		// 除くが、antenna で同じことをすると「リレーや Announce の target 解決が
		// 先に DB へ入れた note は、その後の直接配送でも antenna に入らない」に
		// なる (ephemeral 無効時、リレー経由でも DB 行はできる)。リレーを購読
		// している構成では取りこぼしが恒常化する。再配送での WS 重複は
		// fanoutHook も同じく許している側で、こちらのほうが軽い。
		if p.antennaHook != nil && !viaRelay {
			p.goHook(func() { p.antennaHook.OnNoteCreated(hydrated, actor) })
		}
		// 通知フックは dedup hit では発火させない。**通知は冪等ではない** —
		// 呼ぶたびに新しい id で 1 件積まれるので、同じ Create(Note) が再配送
		// されると受信者の通知一覧に同じメンション / DM が 2 件並ぶ。実運用の
		// インスタンスでは配送リトライや inbox / sharedInbox への二重投げが
		// 日常的に起きるため、fanout (note id で冪等) と同じ扱いにはできない。
		//
		// upstream も通知は NoteCreateService.create の中でしか作らないので、
		// 既存ノートを返した経路では通知が出ない (= ここと同じ挙動)。
		if p.notificationHook != nil && created {
			p.goHook(func() {
				// reply / quote / mention 通知を local notifiee に対して
				// 生成する。hydrated.Reply / hydrated.Renote は preload
				// 済みなので、notification hook 側で UserID を取れる
				// (#415)。
				p.notificationHook.OnNoteCreated(hydrated, actor, hydrated.Reply, hydrated.Renote)
			})
		}
		// chart hook は resolver が新しく作ったときに呼ぶ (Resolver.fireNoteCreated、
		// #3330)。ここでも呼ぶと二重に数える。dedup hit で呼ばないのは
		// resolver 側も同じ (#1156)。
	}
	return nil
}

// goHook runs a best-effort hook in the group of the server generation. 呼び出し
// 側で箱を選ばせないために、Processor の hook は全部ここを通す (#3661)。
func (p *Processor) goHook(fn func()) { safeGoFedHook(p.background, fn) }

// safeGoFedHook runs fn in a new goroutine of g, recovering from panics. local
// note service の safeGo と同じ振る舞い (`note_create_service.go`)。g は世代の
// 箱で、世代が止まった後は fn を走らせない (#3661)。
func safeGoFedHook(g *bgwork.Group, fn func()) {
	g.Go(func(context.Context) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("recovered panic in federation best-effort hook", "panic", r)
			}
		}()
		fn()
	})
}

// hydrateNoteForFanout reloads a freshly-created note via
// FindByIDWithRelations so that its Renote/Reply relations (and their authors
// and polls) are hydrated before handing it to the fanout hook. reload が失敗した場合は元の note をその
// まま返す (best-effort)。
func hydrateNoteForFanout(repo interface {
	FindByIDWithRelations(id string) (*model.Note, error)
}, note *model.Note) *model.Note {
	if repo == nil || note == nil {
		return note
	}
	if loaded, err := repo.FindByIDWithRelations(note.ID); err == nil && loaded != nil {
		return loaded
	}
	return note
}
