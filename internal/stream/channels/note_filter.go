package channels

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/core/ugcvisibility"
	"github.com/elythia-network/elythia/internal/core/wordmute"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
)

// injectRenoteMyReaction sets note.renote.myReaction for a pure renote whose
// renote has reactions, computing the viewer's reaction IN-MEMORY from the
// renote's reactionAndUserPairCache (upstream timeline channel onNote の
// populateMyReaction cache 経路、#2058)。per-subscriber の DB lookup を避けるため
// cache-only で算出し、cache が truncated (reaction 種類数 > pair 数) なら skip する
// (upstream は DB fallback するが mk-go は cache のみ)。
//
// 非該当 (未認証 / pure renote でない / renote.reactions 空 / viewer 未 reaction /
// cache truncated / parse 失敗) では payload を変えずに返す。
func injectRenoteMyReaction(payload []byte, viewerID string) []byte {
	if viewerID == "" {
		return payload
	}
	var probe struct {
		notePayload
		Renote *struct {
			Reactions json.RawMessage `json:"reactions"`
			Cache     []string        `json:"reactionAndUserPairCache"`
		} `json:"renote"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return payload
	}
	if !probe.isPureRenote() || probe.Renote == nil {
		return payload
	}
	// upstream populateMyReaction は reactionsCount = Σ(reactions の値) (種類数でなく
	// 総数) で cache 充足を判定する。総数 > cache 長なら cache は truncated なので
	// 算出しない (upstream は DB fallback だが mk-go は cache-only、#2058)。
	reactionCount := reactionValueSum(probe.Renote.Reactions)
	if reactionCount == 0 || len(probe.Renote.Cache) < reactionCount {
		return payload
	}
	prefix := viewerID + "/"
	reaction := ""
	for _, pair := range probe.Renote.Cache {
		if strings.HasPrefix(pair, prefix) {
			// pair suffix は raw reaction。REST 経路と同じく colon-form/legacy 正規化を
			// 適用しないと reactions map の key (正規化済) と突合できない (#2058 HIGH)。
			reaction = entity.NormalizeReactionWithLegacy(pair[len(prefix):])
			break
		}
	}
	if reaction == "" {
		return payload // viewer は reaction していない → myReaction null のまま
	}
	// full unmarshal → set → re-marshal (hideNoteRawForViewer と同パターン)。算出に
	// 成功した pure-renote-with-reactions のときだけ marshal するので hot-path 全体には
	// 乗らない。
	var full entity.NoteEntity
	if err := json.Unmarshal(payload, &full); err != nil || full.Renote == nil {
		return payload
	}
	full.Renote.MyReaction = &reaction
	out, err := json.Marshal(&full)
	if err != nil {
		return payload
	}
	return out
}

// reactionValueSum returns Σ of a reactions JSON object's values (the total
// reaction count, matching upstream `Object.values(reactions).reduce(...)` /
// entity.packReactions、#2058)。0 for null / empty / parse error。
func reactionValueSum(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var m map[string]float64
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0
	}
	total := 0
	for _, v := range m {
		total += int(v)
	}
	return total
}

// viewerIDFromCtx extracts the authenticated user's ID from a ChannelContext.
// Returns "" for anonymous connections so wordmute.MatchNote treats every
// note as "not authored by self" (i.e. eligible for muting). Both the
// notesfilter / wordmute helpers handle empty viewerID safely.
func viewerIDFromCtx(ctx stream.ChannelContext) string {
	u, ok := ctx.User().(*model.User)
	if !ok || u == nil {
		return ""
	}
	return u.ID
}

// viewerUserFromCtx returns the authenticated *model.User (or nil for an
// anonymous connection), guarding against the typed-nil that ctx.User() may
// return (same trap viewerIDFromCtx handles).
func viewerUserFromCtx(ctx stream.ChannelContext) *model.User {
	u, ok := ctx.User().(*model.User)
	if !ok || u == nil {
		return nil
	}
	return u
}

// streamVisibilityProbe is the minimal note metadata for the per-subscriber
// visibility gate, decoded without unmarshalling the whole NoteEntity.
type streamVisibilityProbe struct {
	UserID         string     `json:"userId"`
	ChannelID      *string    `json:"channelId"`
	Visibility     string     `json:"visibility"`
	VisibleUserIDs []string   `json:"visibleUserIds"`
	Mentions       []string   `json:"mentions"`
	Reply          *replyMeta `json:"reply,omitempty"`
}

// streamNoteVisibleForViewer reports whether a streamed note payload is visible
// to viewerID, mirroring core/note.CanSeeNote / TS Connection#isNoteVisibleForMe.
// It is used by channels that go live during the visibility sweep (#1549) as a
// per-subscriber re-filter. FAIL-CLOSED: a parse error returns false, and
// followers/specified notes from a non-self author with a nil following
// snapshot return false (same doctrine as userListVisibilityShouldEmit #1465).
// public / home are always visible.
func streamNoteVisibleForViewer(payload []byte, viewerID string, snap map[string]bool) bool {
	var p streamVisibilityProbe
	if err := json.Unmarshal(payload, &p); err != nil {
		return false
	}
	switch p.Visibility {
	case string(model.NoteVisibilityPublic), string(model.NoteVisibilityHome):
		return true
	case string(model.NoteVisibilitySpecified):
		if viewerID == "" {
			return false
		}
		if viewerID == p.UserID {
			return true
		}
		return slices.Contains(p.VisibleUserIDs, viewerID)
	case string(model.NoteVisibilityFollowers):
		if viewerID == "" {
			return false
		}
		if viewerID == p.UserID {
			return true
		}
		if p.Reply != nil && p.Reply.UserID == viewerID {
			return true
		}
		if slices.Contains(p.Mentions, viewerID) {
			return true
		}
		if snap == nil {
			return false
		}
		_, ok := snap[p.UserID]
		return ok
	}
	// 空 / 未知 visibility は安全側 (fail-closed)。packed NoteEntity は常に
	// visibility を含むので実運用では到達しない。
	return false
}

// noteChannelID decodes the channelId from a streamed note payload ("" when
// absent / unparseable). Used by the channel timeline gate for defense-in-depth.
func noteChannelID(payload []byte) string {
	var p struct {
		ChannelID *string `json:"channelId"`
	}
	if err := json.Unmarshal(payload, &p); err != nil || p.ChannelID == nil {
		return ""
	}
	return *p.ChannelID
}

// noteVisibility decodes the visibility from a streamed note payload ("" when
// absent / unparseable). Used by the roleTimeline gate (public-only, #1549).
func noteVisibility(payload []byte) string {
	var p struct {
		Visibility string `json:"visibility"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return p.Visibility
}

// anonRequireSigninDrop reports whether an anonymous viewer (viewerID == "")
// must be denied this note ENTIRELY because the note — or its embedded renote /
// reply — has an author who enabled requireSigninToViewContents.
//
// upstream channel.ts / hashtag.ts は anon viewer に対し
// `note.user.requireSigninToViewContents` (+ renote.user / reply.user) を
// 3 連で評価して `return` (= note を丸ごと drop) する (#1549)。mk-go の
// hideEmbeds は HideNoteByPrefsDecision 経由で同条件を「blank 化」するが、
// それだと anon client に空の note shell が送られてしまい upstream の
// 「何も送らない」挙動と差が出る。本 gate で blank 前に drop して揃える。
//
// requireCredential=false で anon 接続を受け付ける channel / hashtag 専用。
// 認証済み viewer には常に false (gate 対象外)。parse 失敗は他 gate に委ねる。
func anonRequireSigninDrop(payload []byte, viewerID string) bool {
	if viewerID != "" {
		return false
	}
	type userProbe struct {
		RequireSigninToViewContents *bool `json:"requireSigninToViewContents"`
	}
	type embedRef struct {
		User userProbe `json:"user"`
	}
	var p struct {
		User   userProbe `json:"user"`
		Renote *embedRef `json:"renote"`
		Reply  *embedRef `json:"reply"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return false
	}
	requiresSignin := func(b *bool) bool { return b != nil && *b }
	if requiresSignin(p.User.RequireSigninToViewContents) {
		return true
	}
	if p.Renote != nil && requiresSignin(p.Renote.User.RequireSigninToViewContents) {
		return true
	}
	if p.Reply != nil && requiresSignin(p.Reply.User.RequireSigninToViewContents) {
		return true
	}
	return false
}

// anonUGCVisibilityDrop reports whether an anonymous viewer (viewerID == "")
// must not receive this note under meta.ugcVisibilityForVisitor, mirroring
// upstream NoteStreamingHidingService.filter (`none` → drop every note,
// `local` → drop a note whose own author is remote). Signed-in viewers are
// never affected and never trigger the policy lookup.
//
// 判定に使うのは note 自身の著者 (`user.host`) で、renote 先の著者ではない
// (upstream が `note.user.host` だけを見るのに揃える)。`local` のときに
// payload を読めない、または `user` が無い場合は、著者がリモートでないことを
// 確かめられないので落とす。
func anonUGCVisibilityDrop(ctx stream.ChannelContext, payload []byte, viewerID string) bool {
	if viewerID != "" {
		return false
	}
	policy := ctx.UGCVisibilityForVisitor()
	if ugcvisibility.HidesAll(policy) {
		return true
	}
	if policy != ugcvisibility.Local {
		return false
	}
	var p struct {
		User *struct {
			Host *string `json:"host"`
		} `json:"user"`
	}
	if err := json.Unmarshal(payload, &p); err != nil || p.User == nil {
		return true
	}
	return ugcvisibility.HidesNote(policy, p.User.Host)
}

// streamNoteID decodes the note id from a streamed note payload ("" when
// absent / unparseable). Used by the hashtag channel to dedupe a note that
// arrives via multiple subscribed tag topics (#1549).
func streamNoteID(payload []byte) string {
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return p.ID
}

// embedProbe is the minimal embed metadata needed to decide whether an embedded
// renote/reply must be hidden, WITHOUT decoding the whole NoteEntity. Author
// preference fields ride the embed's UserLite (populated only when the embed
// author was preloaded at pack time).
type embedProbe struct {
	UserID         string   `json:"userId"`
	Visibility     string   `json:"visibility"`
	VisibleUserIDs []string `json:"visibleUserIds"`
	Mentions       []string `json:"mentions"`
	CreatedAt      string   `json:"createdAt"`
	User           struct {
		ID                           string `json:"id"`
		RequireSigninToViewContents  *bool  `json:"requireSigninToViewContents"`
		MakeNotesHiddenBefore        *int   `json:"makeNotesHiddenBefore"`
		MakeNotesFollowersOnlyBefore *int   `json:"makeNotesFollowersOnlyBefore"`
	} `json:"user"`
}

// hideEmbeds blanks embedded renote/reply content the connection's viewer is
// not allowed to see, right before a channel Sends the note (#1536). It pulls
// the viewer + per-connection following snapshot from ctx; the snapshot makes
// the follower check O(1) in-memory (no DB query per subscriber).
func hideEmbeds(ctx stream.ChannelContext, payload []byte) []byte {
	return hideEmbedsForViewer(payload, viewerUserFromCtx(ctx), ctx.FollowingSnapshot(), time.Now().UnixMilli())
}

// hideEmbedsForViewer is the testable core of hideEmbeds. It returns the
// ORIGINAL payload verbatim (zero copy) unless the top-level note or one of its
// embeds is genuinely hideable for viewer. See hideNoteRawForViewer for the
// gate; this is the []byte-returning wrapper the channels call.
func hideEmbedsForViewer(payload []byte, viewer *model.User, snap map[string]bool, nowMs int64) []byte {
	out, _ := hideNoteRawForViewer(payload, viewer, snap, nowMs)
	return out
}

// hideNoteRawForViewer applies the per-viewer hideNote gate to a single note's
// raw JSON and reports whether anything changed. The TOP-LEVEL note gets the
// author-PREFERENCE gate only (HideNoteByPrefsDecision); its intrinsic
// followers/specified visibility is owned by the channel fan-out (LTL/GTL push
// public-only, home/list/role per-follower), so it is not re-applied here
// (#1568). The depth-2 renote/reply embeds get the FULL embed gate (#1536).
//
// It returns the ORIGINAL payload verbatim (changed=false) unless something is
// genuinely hideable, in which case it decodes a FRESH NoteEntity, blanks the
// hide-eligible parts, and re-marshals — never mutating the shared input buffer
// / publisher body cache. snap maps followeeID->withReplies (only key presence
// is used). nil viewer / nil snap fail closed.
func hideNoteRawForViewer(payload []byte, viewer *model.User, snap map[string]bool, nowMs int64) ([]byte, bool) {
	var probe struct {
		embedProbe
		Renote json.RawMessage `json:"renote"`
		Reply  json.RawMessage `json:"reply"`
	}
	if err := json.Unmarshal(payload, &probe); err != nil {
		return payload, false
	}
	hideTop := topNoteHideable(probe.embedProbe, probe.Reply, viewer, snap, nowMs)
	hideRenote := embedRawHideable(probe.Renote, viewer, snap, nowMs)
	hideReply := embedRawHideable(probe.Reply, viewer, snap, nowMs)
	// depth-2 embed (renote.renote / renote.reply): packer がこれらを出すため、
	// 非公開の note が renote 経由で leak しないよう embed gate を適用する。
	// renote raw から nested renote / reply を取り出す。
	var renoteRenoteRaw, renoteReplyRaw json.RawMessage
	if len(probe.Renote) > 0 {
		var rp struct {
			Renote json.RawMessage `json:"renote"`
			Reply  json.RawMessage `json:"reply"`
		}
		if json.Unmarshal(probe.Renote, &rp) == nil {
			renoteRenoteRaw = rp.Renote
			renoteReplyRaw = rp.Reply
		}
	}
	hideRenoteRenote := embedRawHideable(renoteRenoteRaw, viewer, snap, nowMs)
	hideRenoteReply := embedRawHideable(renoteReplyRaw, viewer, snap, nowMs)
	// **#2106 L5 の降格も反映する (viewer 非依存)。** REST (`notehide`) と
	// push (`core/webpush`) は packed の `visibility` を `followers` へ
	// 書き換えるのに、stream だけ元の `public` のまま流していた。同じノートが
	// 経路によって違う visibility で届く。
	downTop := corenote.ShouldDowngradeVisibility(embedFactsFromProbe(probe.embedProbe), nowMs)
	downRenote := embedRawDowngradable(probe.Renote, nowMs)
	downReply := embedRawDowngradable(probe.Reply, nowMs)
	downRenoteRenote := embedRawDowngradable(renoteRenoteRaw, nowMs)
	downRenoteReply := embedRawDowngradable(renoteReplyRaw, nowMs)
	if !hideTop && !hideRenote && !hideReply && !hideRenoteRenote && !hideRenoteReply &&
		!downTop && !downRenote && !downReply && !downRenoteRenote && !downRenoteReply {
		// 隠すものが無い (= no-embed / public / own / follower) → verbatim。
		return payload, false
	}
	var full entity.NoteEntity
	if err := json.Unmarshal(payload, &full); err != nil {
		return payload, false
	}
	if hideTop {
		entity.HideNoteEntity(&full)
	}
	if hideRenote && full.Renote != nil {
		entity.HideNoteEntity(full.Renote)
	}
	if hideRenoteRenote && full.Renote != nil && full.Renote.Renote != nil {
		entity.HideNoteEntity(full.Renote.Renote)
	}
	if hideRenoteReply && full.Renote != nil && full.Renote.Reply != nil {
		entity.HideNoteEntity(full.Renote.Reply)
	}
	if hideReply && full.Reply != nil {
		entity.HideNoteEntity(full.Reply)
	}
	// **hide の後に書き換える。** hide の判定は元の visibility を読むので、
	// 先に降格すると判定が変わる (notehide / webpush と同じ順)。
	downgradeVisibility(&full, downTop)
	if full.Renote != nil {
		downgradeVisibility(full.Renote, downRenote)
		if full.Renote.Renote != nil {
			downgradeVisibility(full.Renote.Renote, downRenoteRenote)
		}
		if full.Renote.Reply != nil {
			downgradeVisibility(full.Renote.Reply, downRenoteReply)
		}
	}
	if full.Reply != nil {
		downgradeVisibility(full.Reply, downReply)
	}
	out, err := json.Marshal(&full)
	if err != nil {
		return payload, false
	}
	return out, true
}

// topNoteHideable decides whether the top-level note (decoded as embedProbe over
// the payload root) must be blanked for viewer using the author-preference gate
// only. replyRaw carries the note's reply target so the
// makeNotesFollowersOnlyBefore downgrade's reply-target escape hatch can fire.
func topNoteHideable(p embedProbe, replyRaw json.RawMessage, viewer *model.User, snap map[string]bool, nowMs int64) bool {
	f := embedFactsFromProbe(p)
	if rid := replyTargetAuthorID(replyRaw); rid != "" {
		f.ReplyTargetAuthorID = rid
	}
	return corenote.HideNoteByPrefsDecision(viewer, f, followsFromSnap(snap), nowMs)
}

// replyTargetAuthorID extracts the reply target's userId from a raw reply embed,
// or "" when absent/null/undecodable.
func replyTargetAuthorID(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var r struct {
		UserID string `json:"userId"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return ""
	}
	return r.UserID
}

// embedRawHideable decodes the embed-probe metadata from a raw embed object and
// runs the FULL embed decision. Absent/null embed or a probe-decode error →
// false (nothing to hide / cannot probe).
func embedRawHideable(raw json.RawMessage, viewer *model.User, snap map[string]bool, nowMs int64) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var p embedProbe
	if err := json.Unmarshal(raw, &p); err != nil {
		return false
	}
	return corenote.HideEmbedDecision(viewer, embedFactsFromProbe(p), followsFromSnap(snap), nowMs)
}

// embedRawDowngradable reports whether a raw embed's packed visibility must be
// rewritten to `followers` (#2106 L5, upstream treatVisibility)。Viewer 非依存。
func embedRawDowngradable(raw json.RawMessage, nowMs int64) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var p embedProbe
	if err := json.Unmarshal(raw, &p); err != nil {
		return false
	}
	return corenote.ShouldDowngradeVisibility(embedFactsFromProbe(p), nowMs)
}

// downgradeVisibility rewrites n's packed visibility when down is true.
func downgradeVisibility(n *entity.NoteEntity, down bool) {
	if down && n != nil {
		n.Visibility = string(model.NoteVisibilityFollowers)
	}
}

// followsFromSnap turns a per-connection following snapshot into the follows
// predicate the core decisions expect. nil snap → never (fail-closed).
func followsFromSnap(snap map[string]bool) func(string) bool {
	return func(id string) bool {
		if snap == nil {
			return false
		}
		_, ok := snap[id]
		return ok
	}
}

func embedFactsFromProbe(p embedProbe) corenote.EmbedFacts {
	f := corenote.EmbedFacts{
		AuthorID:       p.UserID,
		Visibility:     p.Visibility,
		VisibleUserIDs: p.VisibleUserIDs,
		Mentions:       p.Mentions,
		CreatedAtMs:    parseCreatedAtMsStream(p.CreatedAt),
	}
	if p.User.ID != "" {
		f.AuthorPrefsKnown = true
		if p.User.RequireSigninToViewContents != nil {
			f.RequireSigninToViewContents = *p.User.RequireSigninToViewContents
		}
		f.MakeNotesHiddenBefore = p.User.MakeNotesHiddenBefore
		f.MakeNotesFollowersOnlyBefore = p.User.MakeNotesFollowersOnlyBefore
	}
	return f
}

// parseCreatedAtMsStream mirrors api/notehide.parseCreatedAtMs: on any failure
// (empty / unparseable) it returns 0 so shouldHideNoteByTime fails CLOSED on the
// makeNotes*Before time gates (treats the embed as epoch 0 → hidden) instead of
// fail-OPEN when now is substituted (#1567 review).
func parseCreatedAtMsStream(s string) int64 {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// noteFilter provides client-controlled filtering for timeline channels.
// タイムラインチャンネルのconnectパラメータで指定されるフィルタ条件。
//
// #1063 以降、reply の表示可否は noteFilter からは判定しない (= per-channel
// な upstream Misskey TS 互換ロジックに分離した、replyShouldEmit 参照)。
// `WithReplies` field は upstream `local-timeline` / `hybrid-timeline` の
// connect param と互換性を保つために残しつつ、shouldEmit からは参照しない。
type noteFilter struct {
	WithRenotes bool `json:"withRenotes"`
	WithReplies bool `json:"withReplies"`
	WithFiles   bool `json:"withFiles"`
}

// defaultNoteFilter returns a filter with default values matching TS behavior.
func defaultNoteFilter() noteFilter {
	return noteFilter{
		WithRenotes: true,
		WithReplies: false,
		WithFiles:   false,
	}
}

// parseNoteFilter parses filter parameters from the connect params JSON.
func parseNoteFilter(params json.RawMessage) noteFilter {
	f := defaultNoteFilter()
	if len(params) > 0 {
		_ = json.Unmarshal(params, &f)
	}
	return f
}

// replyMeta is the embedded `note.reply` slice. upstream Misskey TS の
// NoteEntityService.pack が `reply.userId` / `reply.visibility` を埋めてくる
// 前提に揃える (#1063)。embed が欠如している場合、reply gate は 3 escape
// hatch のうち "reply.userId" を参照する 2 つを判定できないので
// conservative に drop する側に倒す。
type replyMeta struct {
	UserID     string `json:"userId"`
	Visibility string `json:"visibility"`
}

// notePayload is the minimal structure needed for filtering decisions.
// hardMutedWords (#787) も同 struct で見るので user / cw を含める。
// #1063 で reply 関連を replyMeta 経由で取り込み、reply gate で参照する。
// Mentions は #1195 で追加。reply gate の mention escape hatch で「viewer
// が note.mentions に含まれていれば withReplies 設定に関係なく pass」を
// 判定するのに使う。NoteEntityService.pack は packed note に `mentions:
// string[]` を出力するので JSON tag で拾える。
type notePayload struct {
	UserID         string          `json:"userId"`
	Text           *string         `json:"text"`
	CW             *string         `json:"cw"`
	RenoteID       *string         `json:"renoteId"`
	ReplyID        *string         `json:"replyId"`
	FileIDs        []string        `json:"fileIds"`
	Mentions       []string        `json:"mentions"`
	Poll           json.RawMessage `json:"poll"`
	VisibleUserIDs []string        `json:"visibleUserIds"`
	Reply          *replyMeta      `json:"reply,omitempty"`
	// Renote は純粋リノートの中身。renote.reply の可視性を見るためだけに
	// 持つ (upstream home-timeline.ts:74-81)。
	Renote *renoteMeta `json:"renote,omitempty"`
}

// renoteMeta carries just enough of the renote target to gate a pure renote.
type renoteMeta struct {
	Reply *replyMeta `json:"reply,omitempty"`
}

// isPureRenote reports whether the payload is a pure renote (renote with no
// text/cw/files/poll/reply)。SQL pureRenoteCondSQL / core/note.IsPureRenote /
// timeline.isPureRenote と揃える (#1888)。これが揺れると cache-hit (stream) と
// DB fallback で withRenotes フィルタの挙動が割れる。
func (n *notePayload) isPureRenote() bool {
	return n.RenoteID != nil &&
		n.Text == nil &&
		n.CW == nil &&
		n.ReplyID == nil &&
		len(n.FileIDs) == 0 &&
		(len(n.Poll) == 0 || string(n.Poll) == "null")
}

// shouldEmit returns true if the note passes non-reply filter conditions
// (renote / file / hardmute). reply の表示可否は channel ごとに異なる
// semantics で別途 replyShouldEmit が判定する (#1063)。
//
// hardMuteRules (#787) には viewer の user_profile.hardMutedWords (jsonb) を
// そのまま渡す。空 / nil なら hard mute は no-op。viewerID は self skip 用、
// streaming は authenticated path のみ呼ばれるので空文字は anonymous 扱い。
func (f *noteFilter) shouldEmit(payload []byte, hardMuteRules []byte, viewerID string) bool {
	var note notePayload
	if err := json.Unmarshal(payload, &note); err != nil {
		// パース失敗時はそのまま送信
		return true
	}

	// 純リノート (text/cw/files/poll/reply 全て空 + renoteId あり) をフィルタ。
	// SQL pureRenoteCondSQL / timeline_filter.go の isPureRenote と一致させる (#1888)。
	if !f.WithRenotes && note.isPureRenote() {
		return false
	}

	// ファイル付きのみモード
	if f.WithFiles && len(note.FileIDs) == 0 {
		return false
	}

	// hardMutedWords filter (#787)。rules 空なら no-op。
	if len(hardMuteRules) > 0 {
		text, cw := "", ""
		if note.Text != nil {
			text = *note.Text
		}
		if note.CW != nil {
			cw = *note.CW
		}
		if wordmute.MatchNote(hardMuteRules, note.UserID, viewerID, text, cw) {
			return false
		}
	}

	return true
}

// renotedReplyVisibleTo reports whether a pure renote's target reply is visible
// to the viewer. followers 限定投稿への返信をリノートされたとき、返信先の
// 投稿者をフォローしていない viewer には流さない (upstream home-timeline.ts)。
func renotedReplyVisibleTo(note *notePayload, viewerID string, snap map[string]bool) bool {
	if note.Renote == nil || note.Renote.Reply == nil || !note.isPureRenote() {
		return true
	}
	reply := note.Renote.Reply
	if reply.Visibility != "followers" {
		return true
	}
	if reply.UserID == viewerID {
		return true
	}
	_, follows := snap[reply.UserID]
	return follows
}

// replyGateMode controls how replyShouldEmit decides emission per timeline
// channel. upstream Misskey TS の各 channel impl と semantics を揃える
// (#1063)。
type replyGateMode int

const (
	// replyGateHome is the homeTimeline semantics. snapshot が
	// `note.userId` の withReplies=true を持つなら通常 pass (visibility が
	// followers なら follow 関係を別途 check)、それ以外は 3 escape hatch のみ
	// (reply.userId === me / note.userId === me / reply.userId === note.userId)。
	// upstream `home-timeline.ts` には withReplies connect param が無いので、
	// paramWithReplies は無視する。
	replyGateHome replyGateMode = iota

	// replyGateHybrid is the hybridTimeline semantics. replyGateHome と同じ
	// だが connect param `withReplies` を followee.withReplies と OR で評価する
	// (upstream `hybrid-timeline.ts`)。
	replyGateHybrid

	// replyGateLocal is the localTimeline semantics. upstream `local-timeline`
	// は authenticated viewer 限定で reply を gate し、`!this.withReplies && !
	// this.following[note.userId]?.withReplies` のとき 3 escape hatch を判定する
	// (= anonymous は anyway pass-through、つまり global と同じ)。
	replyGateLocal

	// replyGateGlobal disables reply filtering entirely. upstream
	// `global-timeline` は reply 制御を持たない。
	replyGateGlobal
)

// replyShouldEmit decides whether to emit `payload` given the channel-mode
// semantics, the viewer's followee snapshot, and the connect param
// `withReplies`. payload は packed NoteEntity の JSON。
//
// snapshot は ChannelContext.FollowingSnapshot() の返り値 (followeeID →
// withReplies)。anonymous / lookup 未配線時は nil。nil 時は `snap[X]` が
// 必ず false / "key 無し" を返すので gate は 3 escape hatch のみで動く。
//
// paramWithReplies は connect 時の `withReplies` boolean。upstream で param
// を持たない home-timeline では呼び出し側が常に false を渡す。
//
// Defensive 設計の早期 return が 2 段ある:
//
//  1. mode == replyGateGlobal は unmarshal すら行わずに pass-through する
//     (upstream `global-timeline.ts` には reply gate が無いため hot path
//     のコストも最小化する意図)。
//  2. JSON unmarshal 失敗時も pass-through する。payload が壊れているなら
//     gate しないほうが「全 user に表示しない」より副作用が小さいため。
//     これは shouldEmit と同じポリシー。
func replyShouldEmit(payload []byte, viewerID string, snap map[string]bool, paramWithReplies bool, mode replyGateMode) bool {
	if mode == replyGateGlobal {
		return true
	}
	var note notePayload
	if err := json.Unmarshal(payload, &note); err != nil {
		return true
	}
	// 純粋リノートは、リノート先が「フォローしていない人の followers 限定投稿
	// への返信」なら弾く (upstream home-timeline.ts:76-80)。リノートそのものは
	// reply ではないので、下の reply gate では拾えない。
	if !renotedReplyVisibleTo(&note, viewerID, snap) {
		return false
	}
	if note.ReplyID == nil {
		return true
	}

	// upstream `isMe = this.user!.id === note.userId` の escape hatch。
	// 自分の reply は followee snapshot や withReplies 設定に依存せず常に pass。
	isMe := viewerID != "" && note.UserID == viewerID

	// localTimeline / hybridTimeline は anonymous viewer に対して reply gate
	// を一切かけない (upstream `if (note.reply && this.user && ...)` の
	// `this.user` 条件相当)。upstream hybrid-timeline は requireCredential=true
	// なので「anonymous hybrid」状態は upstream に存在しないが、mk-go は legacy
	// 互換で anonymous viewer にも hybrid 購読を許しており、その場合 followee
	// snapshot を一切持たない (snap=nil)。default 分岐に落とすと selfThread 以外
	// の reply が全て drop されて旧 mk-go (`WithReplies=true → 全 pass`) より
	// 厳しい挙動になるので、anonymous local と同じく pass-through に倒す。
	// homeTimeline は Init で anonymous 自体を no-op にするのでここには来ない。
	if (mode == replyGateLocal || mode == replyGateHybrid) && viewerID == "" {
		return true
	}

	if isMe {
		return true
	}

	// reply embed が欠如している場合は reply.userId 参照ができないので
	// "reply.userId === me" / "reply.userId === note.userId" の判定が
	// 不能。conservative に drop して fanout 側 / DB 側で正しく拾われる
	// (= リロード時に表示される) フォールバックに倒す。
	if note.Reply == nil {
		return false
	}

	replyToMe := viewerID != "" && note.Reply.UserID == viewerID
	selfThread := note.Reply.UserID == note.UserID
	// mk-go 独自 escape hatch (#1195): viewer が note.mentions に含まれて
	// いれば、withReplies / followee.withReplies の設定に関係なく reply gate を
	// pass する。upstream Misskey TS は home/hybrid/local channel いずれもこの
	// escape を持たないので意図的 deviation。
	// ただし specified note の宛先 (visibleUserIds) は、upstream NoteCreateService が
	// mentions 列にも合流させる都合で「本文で @ された」わけではなく載っている。
	// 宛先というだけでこの hatch を発火させると、upstream が HTL に流さない direct
	// reply が流れてしまうので除外する。
	isMentioned := viewerID != "" && slices.Contains(note.Mentions, viewerID) &&
		!slices.Contains(note.VisibleUserIDs, viewerID)

	var followeeWithReplies bool
	if snap != nil {
		followeeWithReplies = snap[note.UserID]
	}

	switch mode {
	case replyGateHome, replyGateHybrid:
		// upstream `home-timeline.ts` / `hybrid-timeline.ts` の reply gate。
		// home は paramWithReplies を見ず、hybrid のみ followee.withReplies と
		// OR で評価する点が違うので mode で switch する。
		paramOR := mode == replyGateHybrid && paramWithReplies
		if followeeWithReplies || paramOR {
			// followers visibility への reply は follow 関係を別途 check
			// (upstream の "自分のフォローしていないユーザーの visibility:
			// followers な投稿への返信は弾く" branch)。snap に reply.userId
			// が含まれていない (= follow していない) かつ自分宛でもないなら drop。
			if note.Reply.Visibility == "followers" && !replyToMe {
				if _, follows := snap[note.Reply.UserID]; !follows {
					return false
				}
			}
			return true
		}
		// 残り 3 escape hatch (replyToMe / selfThread / isMentioned)。
		// isMe は上で抜けている。isMentioned は mk-go 独自 (#1195)。
		return replyToMe || selfThread || isMentioned

	case replyGateLocal:
		// local-timeline は anonymous で reply gate 不要、authenticated 時のみ
		// `!this.withReplies && !following[note.userId].withReplies` → 3 escape
		// hatch + isMentioned で pass。isMentioned は mk-go 独自 (#1195)。
		if !followeeWithReplies && !paramWithReplies {
			return replyToMe || selfThread || isMentioned
		}
		return true
	}
	// 未知の replyGateMode は保守的に pass-through する。replyGateGlobal は
	// 関数頭で早期 return 済み、それ以外の既存 mode は上の switch ですべて
	// return する設計なので、ここに到達するのは将来 mode を追加し忘れた
	// 場合のみ。production には影響しないが「panic より degrade」に倒す方が
	// 既存 streaming filter の defensive 方針と整合する。
	return true
}
