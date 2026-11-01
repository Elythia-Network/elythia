package federation

import (
	"sync/atomic"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
)

// resolveChain is the set of note identities held by the current resolve
// chain — the singleflight keys taken by `resolveNoteDepth` and the document
// ids taken by `ingestNoteWithCreated`.
//
// **プロセス全体で 1 つの台帳では区別が付かない。** 入れ子の解決が
// 「自分の祖先が握っている」(待つと自分を待つので解けない) のか
// 「無関係な goroutine が握っている」(待つのが正しい) のかを分けるには、
// 解決チェーンに閉じた集合が要る。upstream の `Resolver.history` が activity
// ごとに作られる Set なのはこのため (#2685)。
//
// プロセス全体の台帳だった頃は後者も巻き込んで諦めていたので、別の worker が
// 同じ引用先を取り込んでいる最中に引用元が来ると `renoteId` を落としたまま
// 保存し、**恒久的に失っていた** (再取り込みは FindByURI で早期 return する)。
//
// **不変にしてある。** 追加は新しい chain を返し、元は変えない。defer で外す
// 必要が無く、外し忘れも起きない。兄弟の解決 (featured の 2 件目など) に前の枝の
// 鍵が漏れることもない。深さは resolveRecursionLimit (256) で頭打ちだが、
// 実際は 2-3 段なので複製のコストは問題にならない。
//
// **集合ではなく写像。** 値は「その識別子で取り込まれる note の document id」。
// 呼び出し側は取得 URI しか知らないことが多いが、note 行は document id で
// 保存されるので、既存行を引き当てるには対応が要る。別名 URL では両者が
// 食い違う (#2684 review MED-1)。fetch 前は id が分からないので "" を入れる。
type resolveChain struct {
	// id identifies the resolve tree this chain belongs to. 枝分かれしても
	// 同じ値を引き継ぐ。cross-goroutine の待ち循環を検出する resolveWaits が
	// これを「誰が」の単位に使う (#2685 review HIGH-1)。
	id uint64
	// budget は**この解決木が待ちに費やした時間**。木の全枝で共有する。
	//
	// 上限を join ごとに掛けると、1 回の解決が待つ回数だけ積み上がる
	// (引用チェーンは resolveRecursionLimit まで入れ子になりうる、
	// #2685 review HIGH-2)。
	//
	// **期限 (時刻) ではなく費やした時間で持つ。** 根で `now + 上限` を打つ形に
	// すると、fetch のように**待ち以外の作業**でも予算が減る。上限より長くかかる
	// 解決の途中で正当な待ちに出会うと 1ms も待たずに諦めることになり、
	// #2685 が直した renoteId 欠落がそのまま戻る (#2685 review round 4)。
	budget *treeBudget
	// bestEffort は「この枝から先は待ちの辺を張らない」印。featured の取り込みが
	// 使う。**枝に伝播させるのが要点** — 相乗りする瞬間だけ待たない形にしても、
	// 自分が先頭になれば内側 (著者 actor の解決) で待ってしまう
	// (#2685 review round 4)。
	bestEffort             bool
	inboundLocalReferences bool
	inboundSourceHost      string
	ids                    map[string]string
	// fetchKey は、instance actor では見えなかった投稿を取り直すときに使う
	// ローカルの利用者の鍵 (#3915)。引用先の解決と actor の解決 (とそこから入る
	// featured) には渡さない (resolveQuoteURI / resolveActorOnceWithID が
	// withoutFetchKey で外す)。nil なら取り直さない。
	fetchKey *fetchKeySource
}

// treeBudget is the wait allowance shared by every chain in one resolve tree.
type treeBudget struct{ spent atomic.Int64 }

// nextResolveChainID hands out resolve-tree identities. 0 は「未採番」。
var nextResolveChainID atomic.Uint64

// lookup reports whether this chain holds key, and the document id recorded
// for it ("" when the chain entered before the id was known). nil は空。
func (c *resolveChain) lookup(key string) (string, bool) {
	if c == nil {
		return "", false
	}
	docID, ok := c.ids[key]
	return docID, ok
}

// with returns a chain holding everything this one holds plus key→docID.
//
// レシーバは変更しない。nil レシーバでも呼べる (チェーンの外から入る経路が
// 毎回 chain を組み立てなくて済むようにするため)。同じ key を後から docID 付きで
// 入れ直すと上書きする — resolveNoteDepth が鍵だけで入り、resolveNoteOnce が
// fetch 後に id を確定させるため。
func (c *resolveChain) with(key, docID string) *resolveChain {
	if key == "" {
		return c
	}
	n := 1
	if c != nil {
		n += len(c.ids)
	}
	ids := make(map[string]string, n)
	if c != nil {
		for k, v := range c.ids {
			ids[k] = v
		}
	}
	ids[key] = docID
	next := &resolveChain{ids: ids}
	if c != nil {
		next.id, next.budget, next.bestEffort = c.id, c.budget, c.bestEffort
		next.inboundLocalReferences, next.inboundSourceHost = c.inboundLocalReferences, c.inboundSourceHost
		next.fetchKey = c.fetchKey
	}
	next.rootStamp()
	return next
}

// asBestEffort returns a chain whose subtree must never wait on another chain.
func (c *resolveChain) asBestEffort() *resolveChain {
	if c != nil && c.bestEffort {
		return c
	}
	next := &resolveChain{bestEffort: true}
	if c != nil {
		next.id, next.budget, next.ids = c.id, c.budget, c.ids
		next.inboundLocalReferences, next.inboundSourceHost = c.inboundLocalReferences, c.inboundSourceHost
		next.fetchKey = c.fetchKey
	}
	next.rootStamp()
	return next
}

// treeID returns the tree identity, or 0 when this chain has none.
func (c *resolveChain) treeID() uint64 {
	if c == nil {
		return 0
	}
	return c.id
}

// waitBudget returns how long this chain may still spend waiting.
//
// 予算が未設定 (木の外から来た呼び出し) なら 1 回分を丸ごと使う。
func (c *resolveChain) waitBudget() time.Duration {
	if c == nil || c.budget == nil {
		return resolveJoinTimeout
	}
	return resolveJoinTimeout - time.Duration(c.budget.spent.Load())
}

// chargeWait records time this chain spent blocked on another chain.
func (c *resolveChain) chargeWait(d time.Duration) {
	if c == nil || c.budget == nil || d <= 0 {
		return
	}
	c.budget.spent.Add(int64(d))
}

// ingestedDocID returns the id an **ancestor** of this chain recorded for the
// given document, and whether such an entry exists.
//
// **鍵と値が一致する entry だけを見る。** 祖先が「自分が取り込んでいる document」
// として載せるのは `chainAfterProbe` / `ingestNoteWithCreated` の `with(id, id)`
// だけで、そこは必ず鍵 == 値になる。分けたい相手は 2 つある (#2695)。
//
//   - `resolveNoteDepth` が fetch 前に入れる「鍵 → ""」。正規形
//     (取得 URI == document id) では自分自身の entry が同じ鍵に見える
//   - `chainAfterProbe` が入れる「取得 URI → document id」。**値は空ではない**
//     ので、値の有無で分けると、featured の item の document id がたまたま祖先の
//     取得 URI と一致したときに誤爆する。祖先の行は quote / 著者の解決より後に
//     作られるのでこの時点では通常まだ無く、**そのピンが落ちる** (実測。
//     他の goroutine が窓の中で行を作っていれば、既存行 fallback が**別の note の
//     行**を返す) (#2710 review round 3 MEDIUM-1)
//
// 戻り値の id は条件から**常に引数と同じ**。呼び出し側で「祖先が記録した id」と
// 読めるように返しているだけで、別の値を持つ経路は無い (#2710 review round 4 LOW-3)。
func (c *resolveChain) ingestedDocID(docID string) (string, bool) {
	// **届かない防御。** with は空の鍵を弾くので ids に空鍵は入らず、この分岐を
	// 消しても挙動は変わらない (#2710 review LOW-1)。lookup の実装が変わったとき
	// に「空鍵が全件に一致する」形へ倒れないよう残してある。
	if docID == "" {
		return "", false
	}
	if v, ok := c.lookup(docID); ok && v == docID {
		return v, true
	}
	return "", false
}

// mayWait reports whether this chain is allowed to block on another chain.
func (c *resolveChain) mayWait() bool {
	return c == nil || !c.bestEffort
}

// rootStamp assigns the tree identity and wait allowance if this chain is the
// root of a new tree.
func (c *resolveChain) rootStamp() {
	if c.id != 0 {
		return
	}
	c.id = nextResolveChainID.Add(1)
	c.budget = &treeBudget{}
}

// ensureTree returns a chain with a tree identity (and wait budget) assigned.
//
// note 側は `with` で鍵を足すときに採番されるが、actor 側は鍵を `ids` に入れない
// (`ids` は note の同一性判定に使うもので、actor の鍵を混ぜる意味が無い) ので、
// 木の識別子だけをここで確定させる。
func (c *resolveChain) ensureTree() *resolveChain {
	if c != nil && c.id != 0 {
		return c
	}
	next := &resolveChain{}
	if c != nil {
		// ids は不変に扱っているので共有してよい。
		next.ids, next.bestEffort = c.ids, c.bestEffort
		next.inboundLocalReferences, next.inboundSourceHost = c.inboundLocalReferences, c.inboundSourceHost
		next.fetchKey = c.fetchKey
	}
	next.rootStamp()
	return next
}

// fetchKeyFor returns the local user's key this chain retries the hidden note
// at uri with, or nil. The key is only used for notes on the host it was given
// for.
func (c *resolveChain) fetchKeyFor(uri string) *activitypub.PrivateKey {
	if !c.fetchKeyAppliesTo(uri) {
		return nil
	}
	return c.fetchKey.key
}

// fetchKeyIDFor identifies the key this chain would retry the note at uri with,
// or "" when it would not retry it.
func (c *resolveChain) fetchKeyIDFor(uri string) string {
	if !c.fetchKeyAppliesTo(uri) {
		return ""
	}
	return c.fetchKey.key.KeyID
}

// fetchKeyAppliesTo reports whether this chain has a key for the host of uri.
func (c *resolveChain) fetchKeyAppliesTo(uri string) bool {
	if c == nil || c.fetchKey == nil {
		return false
	}
	host, err := hostFromURI(uri)
	return err == nil && host == c.fetchKey.host
}

// withoutFetchKey returns this chain with no key to retry with.
//
// **actor の解決には鍵を持ち込まない。** actor を作るか読み直すと featured
// (ピン留め) を取り込み、その引用先までたどる。そこで鍵を使うと、照会した人が
// 選んでいない投稿 (他のホストのものを含む) へ、その人の署名付き GET が出る。
func (c *resolveChain) withoutFetchKey() *resolveChain {
	if c == nil || c.fetchKey == nil {
		return c
	}
	next := *c
	next.fetchKey = nil
	return &next
}

// fetchKeySource is the local user's key a hidden note is retried with, and
// the only host it is used for.
//
// **鍵を使うのは 1 つのホストだけ。** 引用先と actor の解決では鍵を外すが、
// ホストでも縛っておく。どこかの経路で鍵が枝に漏れても、相手が選んだ他の
// ホストへ鍵の持ち主の署名付き GET を出させないため。
type fetchKeySource struct {
	key  *activitypub.PrivateKey
	host string
}

// newFetchKeySource returns a source for key used only for notes on the host of
// uri. It returns nil (no retry) when key is nil or uri has no host.
func newFetchKeySource(key *activitypub.PrivateKey, uri string) *fetchKeySource {
	if key == nil {
		return nil
	}
	host, err := hostFromURI(uri)
	if err != nil {
		return nil
	}
	return &fetchKeySource{key: key, host: host}
}
