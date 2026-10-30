package federation

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"log/slog"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// ResolveNote returns the local model.Note row for a remote (or local) note
// URI, fetching and creating it if necessary.
//
//   - ローカル URI (urls.NoteURI のプレフィックスに一致) の場合は ID を抽出して
//     noteRepo.FindByID を試みる
//   - リモート URI で既に取り込み済みなら noteRepo.FindByURI で返す
//   - 未知のリモート URI なら fetcher で取得して IngestNote で永続化
func (r *Resolver) ResolveNote(uri string) (*model.Note, error) {
	note, _, err := r.ResolveNoteWithCreated(uri)
	return note, err
}

// ResolveNoteWithCreated is ResolveNote plus whether this call ingested the
// note (= the row did not exist yet).
//
// **取得 URI で既存行を引いて判定してはいけない** (#2751)。note 行は document
// id で保存されるが、Announce の object などは同じ host の**別名 URL**でありうる
// (`https://h/@user/1` に対し id は `https://h/notes/1`)。取得 URI で引くと何度
// 取り込んでも空振りし、「新規のときだけ」の判定が成立しない。
//
// **singleflight に相乗りした呼び出しも created=true を受け取る。** 実際に
// 作ったのは 1 つでも、同じ note を同時に解決した全員が「新規」と見る。
// antenna の押し込み (`pushNote` の ZAdd / `antenna_note_unread` の UNIQUE) は
// 冪等なので重複しても行は増えないが、**`PublishNote` だけは冪等でない** ので
// WS の antenna channel に同じ note が二重に流れる。
//
// **note を作らずに終わる経路は error になる。** AP poll vote (reply note では
// なく投票として処理する形) は `ingestNoteWithCreated` が (nil, false, nil) を
// 返すが、ここでは ErrInvalidNote に倒す。以前は `(nil, nil)` がそのまま
// ResolveNote から出ており、戻り値を nil check せず `note.ID` を読む呼び出し側
// (handleAdd) が panic しうる形だった。ErrInvalidNote は isPermanentSkipError
// なので、Like / Announce は ack + skip になる。
func (r *Resolver) ResolveNoteWithCreated(uri string) (*model.Note, bool, error) {
	return r.resolveNoteDepthOptCreated(uri, 0, false, false, true, nil)
}

// ResolveNoteForInbox marks this resolution tree as an inbound worker operation
// so local shortcut references are checked without affecting native API reads.
func (r *Resolver) ResolveNoteForInbox(uri, sourceHost string) (*model.Note, error) {
	note, _, err := r.resolveNoteDepthOptCreated(uri, 0, false, false, true, &resolveChain{inboundLocalReferences: true, inboundSourceHost: sourceHost})
	return note, err
}

func (r *Resolver) ResolveNoteWithCreatedForInbox(uri, sourceHost string) (*model.Note, bool, error) {
	return r.resolveNoteDepthOptCreated(uri, 0, false, false, true, &resolveChain{inboundLocalReferences: true, inboundSourceHost: sourceHost})
}

func (r *Resolver) ResolveNoteEphemeralForInbox(uri, sourceHost string) (*model.Note, error) {
	ephemeral := r.ephemeralSink != nil && r.ephemeralSink.Enabled()
	note, _, err := r.resolveNoteDepthOptCreated(uri, 0, false, ephemeral, true, &resolveChain{inboundLocalReferences: true, inboundSourceHost: sourceHost})
	return note, err
}

// ResolveNoteEphemeral is ResolveNote for relay-delivered notes: the note and
// its author are written to the ephemeral store instead of the database
// (#2332)。既に DB に在る URI はそのまま DB 行を返す (直接配送で来ていた等)。
//
// **検証は ResolveNote と完全に同一の経路を通る。** host binding / attribution
// forge / 可視性導出 / mention 上限はいずれも分岐しない。ephemeral 用に別の
// 構築経路を書くと検証が二重管理になり、リレー経由が検証を迂回する穴になる。
func (r *Resolver) ResolveNoteEphemeral(uri string) (*model.Note, error) {
	if r.ephemeralSink == nil || !r.ephemeralSink.Enabled() {
		return r.resolveNoteDepth(uri, 0, false, false, nil)
	}
	return r.resolveNoteDepth(uri, 0, false, true, nil)
}

// ResolveActorViaRelay resolves an actor and marks it as relay-derived when the
// row is created (#2340).
//
// 転送活動の LD-Signature 検証は creator の公開鍵を DB に載せる必要があるため
// (転送活動の唯一の認証手段)、この経路の著者は DB に残る。後追いの孤児掃除で
// 回収できるよう、リレー由来であることを記録しておく。
//
// 既に DB に在る行には印を付けない。リレー購読前から居る行や、プロフィール閲覧・
// スレッド遡りで解決された行を巻き込まないため。
func (r *Resolver) ResolveActorViaRelay(uri string) (*model.User, error) {
	key := crossHostKey(uri, false)
	chain := (*resolveChain)(nil).ensureTree()
	v, err := r.joinResolve(&r.resolveActorGroup, chain, actorWaitKey(key), key, func() (any, error) {
		return r.resolveActorOnceWithID(uri, false, "", false, true, false, chain)
	})
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, ErrInvalidActor
	}
	return v.(*model.User), nil
}

// ResolveActorEphemeral resolves an actor without persisting it when the
// ephemeral store is active (#2338)。Create 転送経由のリレー投稿の著者に使う。
func (r *Resolver) ResolveActorEphemeral(uri string) (*model.User, error) {
	if r.ephemeralSink == nil || !r.ephemeralSink.Enabled() {
		return r.ResolveActor(uri)
	}
	return r.resolveNoteAuthor(uri, true, 0, nil)
}

// IngestNoteEphemeral is IngestNoteWithCreated for relay-forwarded Create
// activities: the note and its author go to the ephemeral store instead of the
// database (#2338)。検証は通常経路と完全に同一。
func (r *Resolver) IngestNoteEphemeral(body []byte, deliveringActorURI string) (*model.Note, bool, error) {
	if r.ephemeralSink == nil || !r.ephemeralSink.Enabled() {
		return r.IngestNoteWithCreated(body, deliveringActorURI)
	}
	return r.ingestNoteWithCreated(body, deliveringActorURI, 0, true, nil)
}

func (r *Resolver) IngestNoteEphemeralForInbox(body []byte, deliveringActorURI, sourceHost string) (*model.Note, bool, error) {
	ephemeral := r.ephemeralSink != nil && r.ephemeralSink.Enabled()
	return r.ingestNoteWithCreated(body, deliveringActorURI, 0, ephemeral, &resolveChain{inboundLocalReferences: true, inboundSourceHost: sourceHost})
}

// ResolveNoteAllowCrossHost is ResolveNote for user-initiated lookups
// (/api/ap/show) where upstream relaxes the request-url ↔ id binding to
// CrossOrigin softfail (#1828)。entry URI が attacker 制御でないため cross-host
// redirect を許容する。finalURL ↔ id binding は引き続き適用される。
func (r *Resolver) ResolveNoteAllowCrossHost(uri string) (*model.Note, error) {
	return r.resolveNoteDepth(uri, 0, true, false, nil)
}

// resolveNoteDepth is ResolveNote carrying the current recursion depth so the
// note/quote chain can be bounded (#1828)。quote 解決経路 (resolveQuoteURI) が
// depth+1 で再入する。allowCrossHost は user-initiated ap/show 経路でのみ true。
func (r *Resolver) resolveNoteDepth(uri string, depth int, allowCrossHost, ephemeral bool, chain *resolveChain) (*model.Note, error) {
	return r.resolveNoteDepthOpt(uri, depth, allowCrossHost, ephemeral, true, chain)
}

// resolveNoteBestEffort is resolveNoteDepth for callers that must not block on
// another chain's in-flight resolve of the same note.
//
// **best-effort の経路が待ちの辺を張ると、犠牲者の選ばれ方が壊れる。**
// featured の取り込みは失敗しても投稿本体には影響しないが、待つと (1) その間
// actor の鍵を握り続け、(2) その待ちが循環に見えたときに**本命の note の解決**が
// 代わりに弾かれる (#2685 review HIGH-2 / MEDIUM-2)。待たずに既存行へ落とせば、
// プロセス全体台帳だった頃と同じ挙動になる。
func (r *Resolver) resolveNoteBestEffort(uri string, depth int, chain *resolveChain) (*model.Note, error) {
	// **印は枝ごと引き継ぐ。** 相乗りする瞬間だけ待たない形にすると、自分が
	// 先頭になったときに内側 (著者 actor の解決) で待ってしまい、その間 actor の
	// 鍵を握り続ける (#2685 review round 4 で実測 1.2s)。
	return r.resolveNoteDepthOpt(uri, depth, false, false, false, chain.asBestEffort())
}

// resolveNoteDepthOpt is the body of both. mayWait=false makes the caller give
// up with ErrResolveWouldBlock instead of joining another chain's call.
//
// **mayWait=false を渡してよいのは featured の取り込みだけ。** この値は
// 「待たない」と「入口が featured」を兼ねており、resolveNoteOnce の取得後判定
// (#2695) をどこに効かせるかもこれで決まる。別の best-effort な呼び出し元を
// 足すなら、判定の入口を bool から分けること (#2710 review LOW-3)。
func (r *Resolver) resolveNoteDepthOpt(uri string, depth int, allowCrossHost, ephemeral, mayWait bool, chain *resolveChain) (*model.Note, error) {
	note, _, err := r.resolveNoteDepthOptCreated(uri, depth, allowCrossHost, ephemeral, mayWait, chain)
	return note, err
}

// resolvedNote carries the singleflight result so callers can tell an ingest
// from a cache hit (#2751)。
type resolvedNote struct {
	note    *model.Note
	created bool
}

// resolveNoteDepthOptCreated is the body of resolveNoteDepthOpt.
func (r *Resolver) resolveNoteDepthOptCreated(uri string, depth int, allowCrossHost, ephemeral, mayWait bool, chain *resolveChain) (*model.Note, bool, error) {
	// 同一 URI への並行呼び出しは singleflight で collapse (#300 3-7)。
	// ResolveActor と同じく cold path の HTTP fetch + IngestNote を重複
	// 実行しないことが目的。
	//
	// ephemeral かどうかで書き込み先が変わるため singleflight key も分ける。
	// 混ざると片方の呼び出しが意図しない層へ書かれる。
	// **入った鍵をチェーンへ足す。** 入れ子の解決が「待つと自分を待つことに
	// なる」を判定できるようにする (#2684)。チェーンに閉じているので、
	// 無関係な goroutine が同じ鍵を握っていても巻き込まない (#2685)。
	key := noteGroupKeyForChain(uri, allowCrossHost, ephemeral, chain)
	inner := chain.with(key, "")
	// **待つと循環する場合は諦める。** チェーンローカルの判定は自分の祖先しか
	// 見ないので、cross-goroutine の待ちの循環 (相互に引用し合う 2 投稿を
	// 2 worker が同時に解決する等) を防げない。待ちに上限が無いと循環した
	// 両方が永久に止まる (#2685 review HIGH-1)。
	//
	// uri が空だと key も空になり with が受け取ったものをそのまま返すので、
	// inner が nil のことがある。木の識別子はここで確定させる。
	inner = inner.ensureTree()
	v, err := r.joinResolveOpt(&r.resolveNoteGroup, inner, noteWaitKey(key), key, mayWait, func() (any, error) {
		return r.resolveNoteOnce(uri, depth, allowCrossHost, ephemeral, !mayWait, inner)
	})
	if err != nil {
		return nil, false, err
	}
	if v == nil {
		return nil, false, ErrInvalidNote
	}
	rn, ok := v.(*resolvedNote)
	if !ok || rn == nil || rn.note == nil {
		return nil, false, ErrInvalidNote
	}
	if chain != nil && chain.inboundLocalReferences {
		if err := r.RequireInboundLocalNote(rn.note, chain.inboundSourceHost); err != nil {
			return nil, false, err
		}
	}
	return rn.note, rn.created, nil
}

// chainAfterProbe records the resolved document identity on the chain, under
// both the key the caller entered with (the fetch URI) and the document id
// itself, so a nested resolve can find the row by either.
//
// **値は取得 URI ではなく document id。** 呼び出し側は取得 URI しか知らないが、
// note 行は document id で保存されるので、別名 URL では取得 URI で引くと必ず
// 空振りする (#2684 review MED-1)。
func chainAfterProbe(chain *resolveChain, uri string, allowCrossHost, ephemeral bool, docID string) *resolveChain {
	return chain.with(noteGroupKeyForChain(uri, allowCrossHost, ephemeral, chain), docID).with(docID, docID)
}

// noteInFlightInChain reports whether the current resolve chain is already
// resolving or ingesting this note, and returns the document id it is being
// stored under when known.
//
// **待ってはいけないのは「自分の祖先が握っている」ときだけ。** チェーンに
// 閉じているので、無関係な goroutine が同じ note を扱っていても巻き込まない
// (#2685)。プロセス全体の台帳だった頃は後者も諦めていたため、別の worker が
// 同じ引用先を取り込んでいる最中に引用元が来ると renoteId を恒久的に
// 落としていた。
//
// 鍵は 2 種類ある。呼び出し側は取得 URI しか知らないことも、document id しか
// 知らないこともあるので両方を見る:
//
//   - singleflight の鍵 (`noteGroupKey` = 取得 URI)。resolveNoteDepth が入場時に足す
//   - 正規化後の document id (`apNote.ID`)。resolveNoteOnce と
//     ingestNoteWithCreated が足す。**inbox 直送でも立つ** (#2686)
//
// 別名 URL では取得 URI と document id が食い違うので、片方だけでは取りこぼす
// (#2684 review HIGH-1)。戻り値の docID は既存行を引き当てるのに使う。
func (r *Resolver) noteInFlightInChain(chain *resolveChain, uri string, allowCrossHost, ephemeral bool) (string, bool) {
	if docID, ok := chain.lookup(noteGroupKeyForChain(uri, allowCrossHost, ephemeral, chain)); ok {
		if docID == "" {
			// fetch 前なので id が未確定。取得 URI で引くしかない。
			docID = uri
		}
		return docID, true
	}
	if docID, ok := chain.lookup(uri); ok {
		// document id 側で当たった。載せた側は id→id で入れるので値は uri と
		// 同じはずだが、写像の値をそのまま返すほうが取り違えない。
		//
		// **届かない防御。** ここに空値が入るには「祖先が非 ephemeral で
		// `uri → ""` を載せ、子孫が同じ uri を ephemeral で問い合わせる」が要るが、
		// ephemeral は親から引き継がれるだけで false から true へ転じず、
		// allowCrossHost は根でしか true にならない (#2710 review round 4 LOW-7)。
		if docID == "" {
			docID = uri
		}
		return docID, true
	}
	return "", false
}

// noteByAnyURI returns the stored row for uri, falling back to docID when the
// fetch URI and the document id differ (alias URLs).
func (r *Resolver) noteByAnyURI(uri, docID string) *model.Note {
	if n, err := r.noteRepo.FindByURI(uri); err == nil && n != nil {
		return n
	}
	if docID != "" && docID != uri {
		if n, err := r.noteRepo.FindByURI(docID); err == nil && n != nil {
			return n
		}
	}
	return nil
}

// resolveNoteOnce is the body of resolveNoteDepth, invoked once per URI by
// resolveGroup. depth は quote chain の現在の深さ。allowCrossHost は
// user-initiated ap/show 経路でのみ true。
func (r *Resolver) resolveNoteOnce(uri string, depth int, allowCrossHost, ephemeral, bestEffortEntry bool, chain *resolveChain) (*resolvedNote, error) {
	if r.noteRepo == nil {
		return nil, ErrInvalidNote
	}
	// fragment 付き URL は解決不能なため拒否 (本家 b94fd5b1 guard、#1828)。
	if strings.Contains(uri, "#") {
		return nil, ErrResolveFragment
	}
	// quote chain の再帰上限 (本家 d592da9f guard、#1828)。悪意ある深い
	// quote チェーンによる無限再帰・amplification を防ぐ。
	if depth > resolveRecursionLimit {
		return nil, ErrRecursionLimit
	}
	if id := r.extractLocalNoteID(uri); id != "" {
		existing, lookupErr := r.noteRepo.FindByID(id)
		if lookupErr == nil {
			if chain != nil && chain.inboundLocalReferences {
				if err := r.RequireInboundLocalNote(existing, chain.inboundSourceHost); err != nil {
					return nil, err
				}
			}
			return &resolvedNote{note: existing}, nil
		}
		if chain != nil && chain.inboundLocalReferences && !repository.IsNotFound(lookupErr) {
			return nil, fmt.Errorf("%w: lookup local note: %w", ErrInboundLocalReferenceFailure, lookupErr)
		}
		// ローカル URI なのにDBに無ければ、これ以上 fetch しても意味がない
		return nil, ErrInvalidNote
	}
	if existing, err := r.noteRepo.FindByURI(uri); err == nil {
		return &resolvedNote{note: existing}, nil
	}
	// ephemeral 経路の URI 逆引き (#2397)。ingestNoteWithCreated と同じ理由で、
	// これが無いと Announce 等の解決経路から同じ投稿が別 ID で再取り込みされる。
	if ephemeral {
		if existing := r.ephemeralNoteByURI(uri); existing != nil {
			return &resolvedNote{note: existing}, nil
		}
	}
	// federation policy gate: ホワイトリスト外 / blocked な host の note は
	// HTTP fetch も DB 永続化もしない。Announce 経由で第三者 host の note を
	// 渡されるケースを塞ぐ (handleAnnounce → ResolveNote)。既存行 (上の
	// FindByURI hit) は素通しで legacy 互換。
	if !r.hostAllowedForURI(uri) {
		return nil, ErrHostNotAllowed
	}
	body, finalURL, err := r.fetchObjectWithFinalURL(uri)
	if err != nil {
		return nil, err
	}
	// note.id host が body を返した host と一致するか検証 (object-spoofing 防止、
	// #1820)。IngestNote は apNote.ID をそのまま note.URI に採用するため、ここで
	// bind しておく。
	var idProbe struct {
		ID      string `json:"id"`
		Context any    `json:"@context"`
	}
	_ = json.Unmarshal(body, &idProbe)
	// fetch した document が AS @context を持つことを要求する (本家 resolve の
	// invalid-response guard、#1828)。inbound delivery (handleCreate → IngestNote)
	// で配送される inlined Note は @context を持たないことがあるためここでは
	// 検証せず、fetch した standalone document (= この resolveNoteOnce 経路) の
	// みに適用する。
	if !hasActivityStreamsContext(idProbe.Context) {
		slog.Warn("federation: note missing ActivityStreams @context", "uri", uri)
		return nil, ErrInvalidNote
	}
	// host 検証も正規化後の値で行う (#2662)。実際の取り込みは
	// ingestNoteWithCreated 側で改めて正規化する。
	idProbe.ID = trimWHATWGURL(idProbe.ID)
	if err := assertResponseHostMatches(finalURL, idProbe.ID); err != nil {
		slog.Warn("federation: note id host mismatch", "uri", uri, "finalURL", finalURL, "id", idProbe.ID)
		return nil, err
	}
	// request URI host == id host も要求する (Strict、#1828)。Announce / quote 等
	// attacker 制御の entry URI から cross-host redirect で第三者 note を解決させる
	// のを防ぐ。user-initiated ap/show (allowCrossHost) は cross-host を許容。
	if !allowCrossHost {
		if err := assertRequestHostMatches(uri, idProbe.ID); err != nil {
			slog.Warn("federation: note request host mismatch", "uri", uri, "id", idProbe.ID)
			return nil, err
		}
	}
	// **fetch 後にもう一度 in-flight を見る** (#2695)。手前の判定は取得 URI で
	// 引くので、featured collection が**別名 URL** を載せていると空振りする
	// (`https://h/@user/x` を取りに行って document の id が `https://h/notes/x`)。
	// id はここまで来ないと分からない。
	//
	// **featured の取り込みから入った呼び出しに限る。** ここで諦めると呼び出し側は
	// 「そのピンを 1 件落とす」だけで、次の actor 更新で拾い直せる。一方 quote 経路で
	// 諦めると `renoteId` が nil のまま保存され、再取り込みは FindByURI で早期
	// return するので**恒久的に失われる** (#2684 review R2 HIGH-1 と同じ型)。
	//
	// **判定に chain.mayWait() を使わない。** best-effort の印は枝ごと引き継がれる
	// ので、featured の取り込みの**内側**で走る引用解決も印を持ってしまい、
	// 上の「限定」が効かない。実測で、取り込み中の note を featured のピンが別名
	// URL で引用しているだけで renoteId が落ちた (#2710 review HIGH-1)。入口が
	// 何だったかは resolveNoteDepthOpt の mayWait が持っているので、それを渡す。
	//
	// **代償は created。** ピンが取り込み中の投稿を別名 URL で**引用**している形
	// (featured には正規 URI で載っている) では、その引用解決がここを素通りして
	// 内側で先に行を作るので、外側の Create は UNIQUE に当たり created=false の
	// まま。#2686 の通知欠落はその形では残る。renoteId の恒久的な欠落のほうが
	// 重いので、意図してそちらを取っている (#2710 review MEDIUM-1)。
	if bestEffortEntry {
		if ancestorID, ingesting := chain.ingestedDocID(idProbe.ID); ingesting {
			// 既に行があるならそれで足りる (取り込み済みのピンを落とさない)。
			if n := r.noteByAnyURI(uri, ancestorID); n != nil {
				return &resolvedNote{note: n}, nil
			}
			return nil, ErrNoteAncestorIngesting
		}
	}
	// **入場した singleflight の鍵を、この区間だけ台帳に載せる** (#2684)。
	// singleflight は「この鍵が in-flight か」を外から問い合わせられないので、
	// 入れ子の解決が「待つと自分を待つことになる」を判定できない。
	//
	// fetch 経由は配送 actor が無いので attribution==actor 検証は skip ("")。
	// quote chain の depth を引き継いで再帰上限を効かせる。
	//
	// **区間を ingest に限る。** ここより手前 (FindByURI / ephemeral 逆引き /
	// HTTP fetch / host 検査 / 取得後の in-flight 判定) から resolveNoteDepth や
	// updateFeatured へ
	// 到達する経路は無いので、載せても再入は防げず**他の goroutine を
	// 取りこぼさせるだけ**になる。実際、singleflight 本体全体を覆うと
	// (1) 別 worker が同じ引用先を待てずに renoteId を落とし、
	// (2) 取り込み済みのピンが FindByURI 中に skip されて
	// ReplaceByUser に消される (#2684 review HIGH-1 / HIGH-2)。
	// **値に document id を持たせる。** 呼び出し側は取得 URI しか知らないが、
	// note 行は document id で保存される。別名 URL では両者が食い違うので、
	// 取得 URI だけで既存行を引くと**必ず空振りする** (#2684 review MED-1)。
	// fetch して id が確定したので、鍵にも id を紐づけ直す。別名 URL では
	// 取得 URI と食い違うので、両方から既存行を引けるようにしておく。
	resolved := chainAfterProbe(chain, uri, allowCrossHost, ephemeral, idProbe.ID)
	note, created, err := r.ingestNoteWithCreated(body, "", depth, ephemeral, resolved)
	if err != nil {
		return nil, err
	}
	return &resolvedNote{note: note, created: created}, nil
}

// resolveQuoteTarget resolves the quote target of an inbound note from its
// `_misskey_quote` / `quoteUrl` URIs (misskeyQuote takes precedence, matching
// upstream ApNoteService `note._misskey_quote ?? note.quoteUrl`). Returns nil
// when no quote URI is present or the target cannot be resolved (best-effort:
// an unresolvable quote degrades to a plain note rather than failing ingest).
//
// 既知 note (local ID / 取り込み済み URI) を fetch 無しで優先的に引く。未知 URI は
// ResolveNote で fetch するが、その URI が現在 ingest 中 (quote cycle) の場合は
// fetch を skip して無限再帰を防ぐ (#1527)。
func (r *Resolver) resolveQuoteTarget(misskeyQuote, quoteURL string, depth int, ephemeral bool, chain *resolveChain) (*model.Note, error) {
	if r.noteRepo == nil {
		return nil, nil
	}
	// upstream ApNoteService は `[_misskey_quote, quoteUrl]` を順に解決し、最初に
	// 成功した note を採用する (`.at(0)`)。通常は両者同値だが、片方しか解決できない
	// ケースで取りこぼさないよう順に試す。
	if n, err := r.resolveQuoteURI(misskeyQuote, depth, ephemeral, chain); err != nil || n != nil {
		return n, err
	}
	if quoteURL != misskeyQuote {
		if n, err := r.resolveQuoteURI(quoteURL, depth, ephemeral, chain); err != nil || n != nil {
			return n, err
		}
	}
	return nil, nil
}

// resolveQuoteURI resolves a single quote URI to a local note row. 既知 note
// (local ID / 取り込み済み URI) は fetch 無しで引き、未知 URI は cycle でなければ
// ResolveNote で fetch する。空 URI / 解決不能は nil。
func (r *Resolver) resolveQuoteURI(uri string, depth int, ephemeral bool, chain *resolveChain) (*model.Note, error) {
	if uri == "" {
		return nil, nil
	}
	// 1. ローカル note URI なら ID 抽出して DB から (fetch 不要)。
	//
	// 通常の quote 解決失敗は従来どおり best-effort で無視する。inbound worker の
	// local reference policy reject とavailability確認の障害だけはerrorとして返し、
	// 外側のnoteを保存してからretry不能になる状態を防ぐ。
	if id := r.extractLocalNoteID(uri); id != "" {
		n, lookupErr := r.noteRepo.FindByID(id)
		if lookupErr == nil {
			if chain != nil && chain.inboundLocalReferences {
				if err := r.RequireInboundLocalNote(n, chain.inboundSourceHost); err != nil {
					return nil, err
				}
			}
			return n, nil
		}
		if chain != nil && chain.inboundLocalReferences && !repository.IsNotFound(lookupErr) {
			return nil, fmt.Errorf("%w: lookup local quote target: %w", ErrInboundLocalReferenceFailure, lookupErr)
		}
		return nil, nil
	}
	// 2. 取り込み済みのリモート note なら DB から (fetch 不要)。
	if n, err := r.noteRepo.FindByURI(uri); err == nil {
		return n, nil
	}
	// 2.5. ephemeral 側に既にあれば再 fetch しない (#2397)。これが無いと引用先を
	// 取り込み直して別 ID で保存し、renoteId が指す先が投稿ごとにずれる。
	if ephemeral {
		if n := r.ephemeralNoteByURI(uri); n != nil {
			return n, nil
		}
	}
	// 3. 未知 URI: cycle でなければ fetch して取り込む (本家同様)。**自分の
	// 祖先が既に扱っている URI** は already-resolved として skip し quote cycle を
	// 防ぐ (#1527、本家 0dc86cf6 guard 相当)。depth+1 で再帰上限も効かせる。
	//
	// 判定はチェーンに閉じているので、無関係な goroutine が同じ引用先を
	// 取り込んでいる最中は**待って引ける** (#2685)。
	if docID, inflight := r.noteInFlightInChain(chain, uri, false, ephemeral); inflight {
		// 別名 URL で既に取り込み済みなら、その行で引用を繋げる。
		if n := r.noteByAnyURI(uri, docID); n != nil {
			return n, nil
		}
		// ephemeral 側も同じ理由で document id で引き直す (2.5 は取得 URI だけ)。
		if ephemeral && docID != "" && docID != uri {
			if n := r.ephemeralNoteByURI(docID); n != nil {
				return n, nil
			}
		}
		return nil, nil
	}
	// quote 解決は federation-loop 扱いで Strict (request host binding 有効)。
	// ephemeral は親から引き継ぐ (引用先だけ DB に落ちるのを防ぐ)。
	if n, err := r.resolveNoteDepth(uri, depth+1, false, ephemeral, chain); err == nil {
		return n, nil
	} else if chain != nil && chain.inboundLocalReferences &&
		(errors.Is(err, ErrInboundLocalReferenceUnavailable) || errors.Is(err, ErrInboundLocalReferenceFailure)) {
		return nil, err
	}
	return nil, nil
}

// IngestNote parses an ActivityStreams Note JSON and persists it as a local
// row authored by the (resolved) attributedTo actor. 既に同じ URI の note を
// 取り込み済みなら既存レコードを返す。
//
// `created` フラグが必要な caller (e.g. processor.handleCreate の chart hook 発火
// 判定) は IngestNoteWithCreated を直接呼ぶこと。
func (r *Resolver) IngestNote(body []byte) (*model.Note, error) {
	// fetch 経由は配送 actor が無いので attribution==actor 検証は skip ("")。
	// id host == attributedTo host 検証は IngestNoteWithCreated 側で常に行う。
	note, _, err := r.ingestNoteWithCreated(body, "", 0, false, nil)
	return note, err
}

// IngestNoteWithCreated is the same as IngestNote but also returns whether
// the note was freshly persisted (`created == true`) or returned from the
// dedup cache (`created == false`). Caller can gate chart hooks on `created`
// so non-idempotent counters such as PerUserNotesChart are not double-applied
// when the same Create activity is delivered twice (#1156).
//
// Return value matrix:
//
//   - fresh INSERT (Create succeeded)           → (note,     true,  nil)
//   - dedup hit (existing row matched by URI)   → (existing, false, nil)
//   - AP poll vote (processed as a Vote)        → (nil,      false, nil)
//   - validation / ingest / actor resolve error → (nil,      false, err)
//
// Note that the dedup-hit case is the only path that returns a non-nil note
// with `created == false`; callers MUST check `created` before firing any
// non-idempotent hook (chart counters, notification, etc.) since the dedup
// path otherwise looks indistinguishable from a fresh ingest at the call site.
// IngestNoteWithCreated は note body を取り込み、新規作成かどうかも返す。
// deliveringActorURI は inbound Create(Note) の配送 actor URI で、note の著者
// (attributedTo) が配送者本人であることの検証に使う (なりすまし防止、#1839)。
// fetch 経由 (IngestNote) では空文字を渡し、この検証を skip する (upstream
// validateNote が actor 未指定時に attribution==actor を skip するのと同じ)。
func (r *Resolver) IngestNoteWithCreated(body []byte, deliveringActorURI string) (*model.Note, bool, error) {
	// inbound delivery 起点は quote chain depth 0。
	return r.ingestNoteWithCreated(body, deliveringActorURI, 0, false, nil)
}

func (r *Resolver) IngestNoteWithCreatedForInbox(body []byte, deliveringActorURI, sourceHost string) (*model.Note, bool, error) {
	return r.ingestNoteWithCreated(body, deliveringActorURI, 0, false, &resolveChain{inboundLocalReferences: true, inboundSourceHost: sourceHost})
}
