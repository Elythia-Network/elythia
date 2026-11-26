package channels

import (
	"encoding/json"
	"sync"
	"unicode/utf8"

	"github.com/elythia-network/elythia/internal/misc/hashtag"
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
	// **形の上限を、正規化より前に全部確かめる** (本家 hashtag.ts と同じ上限)。
	//
	// - グループは 1〜32 個、各グループのタグは 1〜32 個。異なるタグの数 (下の
	//   maxHashtagTopics) だけでは、同じタグを並べたグループをフレームの上限まで
	//   書けてしまい、c.q と配信ごとの matchesTags の手間が入力の大きさに比例する
	// - 空のタグは拒否する。以前は正規化して空になったものと一緒に捨てていたので、
	//   ["a", ""] が ["a"] として広く一致していた
	// - タグの長さは正規化の前の文字数で 128 まで (hashtag.name は varchar(128)。
	//   正規化 (NFKC) の手間は入力の長さに応じて増えるので、拒否する入力に手間を
	//   掛けない。正規化の後の長さでは拒否しない (本家と同じ))
	//
	// 購読より前に全部を確かめる (途中で拒否すると、それまでのタグを購読したまま
	// 返してしまう)。
	if !validHashtagQuery(p.Q) {
		return stream.ErrInvalidParams
	}
	// **購読するトピック数に上限を置く。**
	//
	// Elythia は distinct なタグごとにトピックを購読する。upstream は `q` の
	// 内容に関わらずリスナー 1 本でメモリ上でマッチングするので上限が要らない
	// が、こちらは 1 通の `connect` でトピックの枠を任意個増やせる。
	// (Redis の購読は `PubSubService` が 1 本にまとめているので、Redis の接続は
	// 増えない。) 形の上限 (32×32) の内側でも 1024 個まで増えうるので、別に見る。
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
			// 空でないタグは正規化しても空にならないので、validHashtagQuery の後では
			// ここは通らない。正規化の実装が変わったときのために残す。
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
	// 上と同じく、今は通らない防御。
	if len(c.topics) == 0 {
		return stream.ErrInvalidParams
	}
	return nil
}

func (c *HashtagChannel) OnRedisEvent(payload []byte) {
	// payload は配信1回につき1回だけ解析し、各判定はその結果を読む (#3986)。
	note := newNoteView(payload)
	// 単一 tag topic ヒットでは AND グループ成立を保証できないため、payload の tags
	// に対して OR-of-ANDs を再評価する (#1549)。
	if !c.matchesView(note) {
		return
	}
	// dedupe: 同 note が複数 subscribed tag topic 経由で重複到着するので noteId で
	// 1 回に絞る。fanout は topic ごとに呼ばれ並行しうるため mutex で保護する。
	if id := note.id(); id != "" {
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
	if note.anonRequireSigninDrop(viewerIDFromCtx(c.ctx)) {
		return
	}
	// 未ログインの viewer には meta.ugcVisibilityForVisitor を適用する
	// (upstream NoteStreamingHidingService.filter)。
	if note.anonUGCVisibilityDrop(c.ctx, viewerIDFromCtx(c.ctx)) {
		return
	}
	// per-subscriber 可視性 gate (#1549, fail-closed)。TS notesStream は全可視性を
	// 流して consumer 側で isNoteVisibleForMe する設計に合わせる。
	if !note.visibleForViewer(viewerIDFromCtx(c.ctx), c.ctx.FollowingSnapshot()) {
		return
	}
	if !note.shouldEmit(&c.filter, c.ctx.HardMuteRules(), viewerIDFromCtx(c.ctx)) {
		return
	}
	if note.mutedOrBlocked(c.ctx.MuteBlockSnapshot()) {
		return
	}
	// 埋め込みを隠し、pure renote の renote.myReaction を viewer 毎に inject する
	// (#1536 / #2058)。書き換えが要らなければ元の bytes を送る。
	_ = c.ctx.Send("note", json.RawMessage(note.forViewer(c.ctx, viewerIDFromCtx(c.ctx))))
}

// matchesView is matchesQuery over the decoded note.
func (c *HashtagChannel) matchesView(v noteView) bool {
	if v.n == nil {
		return c.matchesQuery(v.raw)
	}
	return c.matchesTags(v.n.Tags)
}

// matchesQuery evaluates the TS OR-of-ANDs hashtag query
// (q.some(group => group.every(tag => noteTags.includes(tag)))) against the
// payload's normalized tags. Returns false on parse error (fail-closed).
func (c *HashtagChannel) matchesQuery(payload []byte) bool {
	var p struct {
		Tags []string `json:"tags"`
	}
	if err := decodeStreamPayload(payload, &p); err != nil {
		return false
	}
	return c.matchesTags(p.Tags)
}

// matchesTags is the decision of matchesQuery over the note's decoded tags.
func (c *HashtagChannel) matchesTags(tags []string) bool {
	noteTags := make(map[string]struct{}, len(tags))
	for _, t := range tags {
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

const (
	// maxHashtagGroups bounds the number of OR groups in q (same as upstream).
	maxHashtagGroups = 32
	// maxHashtagTagsPerGroup bounds the number of AND tags in one group (same as upstream).
	maxHashtagTagsPerGroup = 32
)

// validHashtagQuery reports whether q has 1..maxHashtagGroups groups, each with
// 1..maxHashtagTagsPerGroup non-empty tags of at most hashtag.MaxTagLength
// runes (counted before normalization).
func validHashtagQuery(q [][]string) bool {
	if len(q) == 0 || len(q) > maxHashtagGroups {
		return false
	}
	for _, group := range q {
		if len(group) == 0 || len(group) > maxHashtagTagsPerGroup {
			return false
		}
		for _, tag := range group {
			if tag == "" || utf8.RuneCountInString(tag) > hashtag.MaxTagLength {
				return false
			}
		}
	}
	return true
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
