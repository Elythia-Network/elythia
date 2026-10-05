package channels

import (
	"encoding/json"
	"sync"

	"github.com/elythia-network/elythia/internal/misc/searchnorm"
	"github.com/elythia-network/elythia/internal/stream"
)

// hashtagSeenCap bounds the per-connection dedupe set; when exceeded it is
// cleared wholesale (note ids are monotonic aidx, so a stale re-delivery right
// after a clear is effectively impossible during the simultaneous multi-topic
// burst the dedupe targets).
const hashtagSeenCap = 1024

// HashtagChannel forwards notes matching a hashtag query (OR-of-ANDs over q).
type HashtagChannel struct {
	ctx    stream.ChannelContext
	q      [][]string // normalized (NFKC+lower) OR-of-ANDs groups
	topics []string   // subscribed hashtag:<tag> topics (for Dispose)
	filter noteFilter

	mu   sync.Mutex
	seen map[string]struct{} // recently delivered note ids (cross-topic dedupe)
}

// NewHashtag returns a channel factory for "hashtag".
func NewHashtag(ctx stream.ChannelContext) stream.Channel {
	return &HashtagChannel{ctx: ctx, seen: make(map[string]struct{})}
}

func (c *HashtagChannel) Init(params json.RawMessage) error {
	var p struct {
		Q [][]string `json:"q"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	// upstream hashtag.ts:36-42 は `q.every(x => Array.isArray(x) && x.length >= 1)`
	// で**全グループ**が非空であることを要求し、1 つでも空なら接続拒否する。以前は
	// q[0] (第1グループ) のみ検証していた (#1948-20)。
	if len(p.Q) == 0 {
		return stream.ErrInvalidParams
	}
	for _, group := range p.Q {
		if len(group) == 0 {
			return stream.ErrInvalidParams
		}
	}
	// **購読するトピック数に上限を置く。**
	//
	// mk-go は distinct なタグごとに Redis の SUBSCRIBE を張る。upstream は
	// `q` の内容に関わらずリスナー 1 本でメモリ上でマッチングするので上限が
	// 要らないが、こちらは 1 通の `connect` で Redis 接続を任意個増やせる。
	// 本番 valkey の `maxclients` に達すると、同じ valkey を使う job queue /
	// timelines / reactions を含めてインスタンス全体が新規接続を拒否する。
	//
	// 値は 1 接続あたりのチャンネル数上限 (`maxChannelsPerConnection` = 32) と
	// 揃える。実運用でタグを 32 個超える購読は想定していない。
	if distinct := countDistinctTags(p.Q); distinct > maxHashtagTopics {
		return stream.ErrInvalidParams
	}
	c.filter = parseNoteFilter(params)
	// q を正規化して保持し、全グループの全タグの distinct 正規化キーを購読する。
	// 旧実装は q[0][0] (第1グループ第1タグ) のみ購読していて multi-tag / OR group を
	// 取りこぼしていた (#1549)。正規化は fanout 側 publish と searchnorm.Normalize で
	// 揃える (揃えないと topic 文字列が一致せず1件も届かない)。
	subscribed := make(map[string]struct{})
	for _, group := range p.Q {
		ng := make([]string, 0, len(group))
		for _, tag := range group {
			key := searchnorm.Normalize(tag)
			if key == "" {
				continue
			}
			ng = append(ng, key)
			if _, ok := subscribed[key]; !ok {
				subscribed[key] = struct{}{}
				topic := "hashtag:" + key
				c.topics = append(c.topics, topic)
				c.ctx.Subscribe(topic)
			}
		}
		if len(ng) > 0 {
			c.q = append(c.q, ng)
		}
	}
	if len(c.topics) == 0 {
		return stream.ErrInvalidParams
	}
	return nil
}

func (c *HashtagChannel) OnRedisEvent(payload []byte) {
	// 単一 tag topic ヒットでは AND グループ成立を保証できないため、payload の tags
	// に対して OR-of-ANDs を再評価する (#1549)。
	if !c.matchesQuery(payload) {
		return
	}
	// dedupe: 同 note が複数 subscribed tag topic 経由で重複到着するので noteId で
	// 1 回に絞る。fanout は topic ごとに呼ばれ並行しうるため mutex で保護する。
	if id := streamNoteID(payload); id != "" {
		c.mu.Lock()
		if _, dup := c.seen[id]; dup {
			c.mu.Unlock()
			return
		}
		if len(c.seen) >= hashtagSeenCap {
			c.seen = make(map[string]struct{}, hashtagSeenCap)
		}
		c.seen[id] = struct{}{}
		c.mu.Unlock()
	}
	// anon viewer + 著者 requireSigninToViewContents は note を丸ごと drop する
	// (#1549, upstream hashtag.ts の note/renote/reply 3 連 gate)。hideEmbeds の
	// blank 化より前に弾いて「何も送らない」upstream 挙動に揃える。
	if anonRequireSigninDrop(payload, viewerIDFromCtx(c.ctx)) {
		return
	}
	// 未ログインの viewer には meta.ugcVisibilityForVisitor を適用する
	// (upstream NoteStreamingHidingService.filter)。
	if anonUGCVisibilityDrop(c.ctx, payload, viewerIDFromCtx(c.ctx)) {
		return
	}
	// per-subscriber 可視性 gate (#1549, fail-closed)。TS notesStream は全可視性を
	// 流して consumer 側で isNoteVisibleForMe する設計に合わせる。
	if !streamNoteVisibleForViewer(payload, viewerIDFromCtx(c.ctx), c.ctx.FollowingSnapshot()) {
		return
	}
	if !c.filter.shouldEmit(payload, c.ctx.HardMuteRules(), viewerIDFromCtx(c.ctx)) {
		return
	}
	if noteMutedOrBlocked(payload, c.ctx.MuteBlockSnapshot()) {
		return
	}
	payload = hideEmbeds(c.ctx, payload)
	// pure renote の renote.myReaction を viewer 毎に inject (#2058)。
	payload = injectRenoteMyReaction(payload, viewerIDFromCtx(c.ctx))
	_ = c.ctx.Send("note", json.RawMessage(payload))
}

// matchesQuery evaluates the TS OR-of-ANDs hashtag query
// (q.some(group => group.every(tag => noteTags.includes(tag)))) against the
// payload's normalized tags. Returns false on parse error (fail-closed).
func (c *HashtagChannel) matchesQuery(payload []byte) bool {
	var p struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return false
	}
	noteTags := make(map[string]struct{}, len(p.Tags))
	for _, t := range p.Tags {
		noteTags[searchnorm.Normalize(t)] = struct{}{}
	}
	for _, group := range c.q {
		if len(group) == 0 {
			continue
		}
		all := true
		for _, t := range group {
			if _, ok := noteTags[t]; !ok {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func (c *HashtagChannel) OnClientMessage(string, json.RawMessage) {}

func (c *HashtagChannel) Dispose() {
	for _, t := range c.topics {
		c.ctx.Unsubscribe(t)
	}
}

// maxHashtagTopics bounds how many Redis subscriptions one hashtag channel
// may open. 1 接続あたりのチャンネル数上限と同じ値にしてある。
const maxHashtagTopics = 32

// countDistinctTags counts the normalized tags a subscription would open a
// Redis subscription for.
func countDistinctTags(q [][]string) int {
	seen := make(map[string]struct{})
	for _, group := range q {
		for _, tag := range group {
			if key := searchnorm.Normalize(tag); key != "" {
				seen[key] = struct{}{}
			}
		}
	}
	return len(seen)
}
