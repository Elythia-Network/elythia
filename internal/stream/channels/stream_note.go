package channels

import (
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/elythia-network/elythia/internal/core/ugcvisibility"
	"github.com/elythia-network/elythia/internal/stream"
)

// payloadDecodeHook, when set, is called on every decode of a streamed note
// payload (or of a probe over it). Tests use it to count decodes per delivery
// (#3986); it is nil in production.
var payloadDecodeHook atomic.Pointer[func()]

// decodeStreamPayload unmarshals a streamed payload (or a probe over it).
// 配信1回あたりの解析の回数をテストで数えるため、payload を読む箇所はここを通す
// (#3986)。
func decodeStreamPayload(data []byte, v any) error {
	if h := payloadDecodeHook.Load(); h != nil {
		(*h)()
	}
	return json.Unmarshal(data, v)
}

// streamNoteUser is the part of a packed note's `user` the stream gates read.
type streamNoteUser struct {
	ID                           string  `json:"id"`
	Host                         *string `json:"host"`
	RequireSigninToViewContents  *bool   `json:"requireSigninToViewContents"`
	MakeNotesHiddenBefore        *int    `json:"makeNotesHiddenBefore"`
	MakeNotesFollowersOnlyBefore *int    `json:"makeNotesFollowersOnlyBefore"`
}

// streamNote is a streamed packed note decoded once per delivery (#3986). It is
// the union of every probe the timeline channels used to decode separately, with
// the same Go type per JSON key, so a payload that decodes as streamNote decodes
// to the same values under each of those probes.
//
// **欄の型は各 probe と揃える。** 型が違うと、同じ payload で片方だけ解析に失敗
// しうる。そうなると、解析の失敗のときの扱い (判定ごとに通す / 落とすが違う) が
// 変わってしまう。
//
// **payload は publisher が entity.NoteEntity を json.Marshal して作る前提に立つ。**
// その出力は、同じキーを2回含まず、大文字小文字だけが違うキー (`renote` と
// `Renote`) も含まない。この前提が崩れると意味が変わる。従来の
// hideNoteRawForViewer は `renote` / `reply` を json.RawMessage で受けて後勝ちで
// 置き換えていたが、ここでは構造体へのポインタで受けるので、2つの値の欄が
// マージされ、埋め込みを隠すかどうかの判定が変わりうる。重複の検知は、payload を
// もう1度走査するか encoding/json/v2 の別の規則 (大文字小文字を区別するなど) で
// 読むことになり、1回で済ませる目的と合わないので入れていない。
// **publisher の経路を変える (外から受けた bytes をそのまま流すなど) ときは、
// ここを見直すこと。**
type streamNote struct {
	ID                       string          `json:"id"`
	UserID                   string          `json:"userId"`
	ChannelID                *string         `json:"channelId"`
	Visibility               string          `json:"visibility"`
	VisibleUserIDs           []string        `json:"visibleUserIds"`
	Mentions                 []string        `json:"mentions"`
	CreatedAt                string          `json:"createdAt"`
	Text                     *string         `json:"text"`
	CW                       *string         `json:"cw"`
	RenoteID                 *string         `json:"renoteId"`
	ReplyID                  *string         `json:"replyId"`
	FileIDs                  []string        `json:"fileIds"`
	Poll                     json.RawMessage `json:"poll"`
	Tags                     []string        `json:"tags"`
	User                     *streamNoteUser `json:"user"`
	Reactions                json.RawMessage `json:"reactions"`
	ReactionAndUserPairCache []string        `json:"reactionAndUserPairCache"`
	Reply                    *streamNote     `json:"reply"`
	Renote                   *streamNote     `json:"renote"`
	// userListStream: の membership event ({type, body}) を、同じ1回の解析で
	// note と見分けるために持つ。packed note はどちらのキーも持たない。
	Type string          `json:"type"`
	Body json.RawMessage `json:"body"`
}

func (n *streamNote) user() streamNoteUser {
	if n.User == nil {
		return streamNoteUser{}
	}
	return *n.User
}

func (n *streamNote) emitProbe() emitProbe {
	return emitProbe{Text: n.Text, CW: n.CW, RenoteID: n.RenoteID, ReplyID: n.ReplyID, FileIDs: n.FileIDs, Poll: n.Poll}
}

func (n *streamNote) embedProbe() embedProbe {
	u := n.user()
	p := embedProbe{
		UserID:         n.UserID,
		Visibility:     n.Visibility,
		VisibleUserIDs: n.VisibleUserIDs,
		Mentions:       n.Mentions,
		CreatedAt:      n.CreatedAt,
	}
	p.User.ID = u.ID
	p.User.RequireSigninToViewContents = u.RequireSigninToViewContents
	p.User.MakeNotesHiddenBefore = u.MakeNotesHiddenBefore
	p.User.MakeNotesFollowersOnlyBefore = u.MakeNotesFollowersOnlyBefore
	return p
}

// embedProbePtr returns the embed probe of n, or nil when n is absent.
func (n *streamNote) embedProbePtr() *embedProbe {
	if n == nil {
		return nil
	}
	p := n.embedProbe()
	return &p
}

func (n *streamNote) hardMuteProbe() *hardMuteProbe {
	if n == nil {
		return nil
	}
	return &hardMuteProbe{
		embedProbe: n.embedProbe(),
		emitProbe:  n.emitProbe(),
		Reply:      n.Reply.hardMuteProbe(),
		Renote:     n.Renote.hardMuteProbe(),
	}
}

func (n *streamNote) replyMeta() *replyMeta {
	if n == nil {
		return nil
	}
	return &replyMeta{UserID: n.UserID, Visibility: n.Visibility}
}

func (n *streamNote) notePayload() notePayload {
	p := notePayload{
		UserID:         n.UserID,
		Text:           n.Text,
		CW:             n.CW,
		RenoteID:       n.RenoteID,
		ReplyID:        n.ReplyID,
		FileIDs:        n.FileIDs,
		Mentions:       n.Mentions,
		Poll:           n.Poll,
		VisibleUserIDs: n.VisibleUserIDs,
		Reply:          n.Reply.replyMeta(),
	}
	if n.Renote != nil {
		p.Renote = &renoteMeta{Reply: n.Renote.Reply.replyMeta()}
	}
	return p
}

func (n *streamNote) muteBlockEmbed() *muteBlockEmbed {
	if n == nil {
		return nil
	}
	u := n.user()
	return &muteBlockEmbed{
		UserID:    n.UserID,
		ChannelID: n.ChannelID,
		User:      muteBlockUserProbe{ID: u.ID, Host: u.Host},
	}
}

func (n *streamNote) muteBlockProbe() muteBlockProbe {
	u := n.user()
	return muteBlockProbe{
		UserID:    n.UserID,
		ChannelID: n.ChannelID,
		User:      muteBlockUserProbe{ID: u.ID, Host: u.Host},
		RenoteID:  n.RenoteID,
		ReplyID:   n.ReplyID,
		Text:      n.Text,
		CW:        n.CW,
		Poll:      n.Poll,
		FileIDs:   n.FileIDs,
		Reply:     n.Reply.muteBlockEmbed(),
		Renote:    n.Renote.muteBlockEmbed(),
	}
}

func (n *streamNote) visibilityProbe() streamVisibilityProbe {
	return streamVisibilityProbe{
		UserID:         n.UserID,
		ChannelID:      n.ChannelID,
		Visibility:     n.Visibility,
		VisibleUserIDs: n.VisibleUserIDs,
		Mentions:       n.Mentions,
		Reply:          n.Reply.replyMeta(),
	}
}

// noteView is one delivery's note payload as the timeline channels see it
// (#3986). newNoteView decodes the payload once; every gate then reads the
// decoded note instead of decoding the payload again.
//
// **解析に失敗した payload は、従来の判定 (payload を判定ごとに読む形) に回す。**
// 判定ごとに、解析の失敗を「通す」か「落とす」かが違い、型の合わない欄があると
// 一部の欄だけ読めることもある。従来どおりに読み直せば、どの判定の結果も変わらない。
// publisher が出す payload は必ず解析できるので、この経路は通常は通らない。
type noteView struct {
	raw []byte
	n   *streamNote
}

func newNoteView(payload []byte) noteView {
	var n streamNote
	if err := decodeStreamPayload(payload, &n); err != nil {
		return noteView{raw: payload}
	}
	return noteView{raw: payload, n: &n}
}

// membershipEvent reports whether the payload is a userListStream: membership
// envelope and returns its type and body.
func (v noteView) membershipEvent() (string, json.RawMessage, bool) {
	typ, body := v.n.typeAndBody(v.raw)
	switch typ {
	case "userAdded", "userRemoved", "userUpdated":
		return typ, body, true
	}
	return "", nil, false
}

// typeAndBody returns the envelope type and body ("" / nil when absent or when
// a legacy decode fails).
func (n *streamNote) typeAndBody(raw []byte) (string, json.RawMessage) {
	if n != nil {
		return n.Type, n.Body
	}
	var env struct {
		Type string          `json:"type"`
		Body json.RawMessage `json:"body"`
	}
	if decodeStreamPayload(raw, &env) != nil {
		return "", nil
	}
	return env.Type, env.Body
}

func (v noteView) id() string {
	if v.n == nil {
		return streamNoteID(v.raw)
	}
	return v.n.ID
}

func (v noteView) userID() string {
	if v.n == nil {
		var author struct {
			UserID string `json:"userId"`
		}
		_ = decodeStreamPayload(v.raw, &author)
		return author.UserID
	}
	return v.n.UserID
}

func (v noteView) channelID() string {
	if v.n == nil {
		return noteChannelID(v.raw)
	}
	if v.n.ChannelID == nil {
		return ""
	}
	return *v.n.ChannelID
}

func (v noteView) visibility() string {
	if v.n == nil {
		return noteVisibility(v.raw)
	}
	return v.n.Visibility
}

// anonRequireSigninDrop is anonRequireSigninDrop over the decoded note.
func (v noteView) anonRequireSigninDrop(viewerID string) bool {
	if v.n == nil {
		return anonRequireSigninDrop(v.raw, viewerID)
	}
	if viewerID != "" {
		return false
	}
	requiresSignin := func(n *streamNote) bool {
		u := n.user()
		return u.RequireSigninToViewContents != nil && *u.RequireSigninToViewContents
	}
	if requiresSignin(v.n) {
		return true
	}
	if v.n.Renote != nil && requiresSignin(v.n.Renote) {
		return true
	}
	return v.n.Reply != nil && requiresSignin(v.n.Reply)
}

// anonUGCVisibilityDrop is anonUGCVisibilityDrop over the decoded note.
func (v noteView) anonUGCVisibilityDrop(ctx stream.ChannelContext, viewerID string) bool {
	if v.n == nil {
		return anonUGCVisibilityDrop(ctx, v.raw, viewerID)
	}
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
	if v.n.User == nil {
		return true
	}
	return ugcvisibility.HidesNote(policy, v.n.User.Host)
}

// shouldEmit is noteFilter.shouldEmit over the decoded note.
func (v noteView) shouldEmit(f *noteFilter, hardMuteRules []byte, viewerID string) bool {
	if v.n == nil {
		return f.shouldEmit(v.raw, hardMuteRules, viewerID)
	}
	if !f.passes(v.n.emitProbe()) {
		return false
	}
	if len(hardMuteRules) == 0 {
		return true
	}
	return !hardMutedProbe(v.n.hardMuteProbe(), hardMuteRules, viewerID, time.Now().UnixMilli())
}

// mutedOrBlocked is noteMutedOrBlocked over the decoded note.
func (v noteView) mutedOrBlocked(snap *stream.MuteBlockSnapshot) bool {
	if v.n == nil {
		return noteMutedOrBlocked(v.raw, snap)
	}
	if snap == nil {
		return false
	}
	return muteBlockProbeDrops(v.n.muteBlockProbe(), snap)
}

// mutedOrBlockedInChannel is noteMutedOrBlockedInChannel over the decoded note.
func (v noteView) mutedOrBlockedInChannel(snap *stream.MuteBlockSnapshot, viewingChannelID string) bool {
	if v.n == nil {
		return noteMutedOrBlockedInChannel(v.raw, snap, viewingChannelID)
	}
	if snap == nil {
		return false
	}
	return muteBlockProbeDropsInChannel(v.n.muteBlockProbe(), snap, viewingChannelID)
}

// replyShouldEmit is replyShouldEmit over the decoded note.
func (v noteView) replyShouldEmit(viewerID string, snap map[string]bool, paramWithReplies bool, mode replyGateMode) bool {
	if v.n == nil {
		return replyShouldEmit(v.raw, viewerID, snap, paramWithReplies, mode)
	}
	if mode == replyGateGlobal {
		return true
	}
	note := v.n.notePayload()
	return replyGateAllows(&note, viewerID, snap, paramWithReplies, mode)
}

// visibleForViewer is streamNoteVisibleForViewer over the decoded note.
func (v noteView) visibleForViewer(viewerID string, snap map[string]bool) bool {
	if v.n == nil {
		return streamNoteVisibleForViewer(v.raw, viewerID, snap)
	}
	p := v.n.visibilityProbe()
	return streamVisibilityAllows(&p, viewerID, snap)
}

// userListVisibilityShouldEmit is userListVisibilityShouldEmit over the
// decoded note.
func (v noteView) userListVisibilityShouldEmit(viewerID string, snap map[string]bool) bool {
	if v.n == nil {
		return userListVisibilityShouldEmit(v.raw, viewerID, snap)
	}
	return userListVisibilityAllows(userListVisibilityPayload{UserID: v.n.UserID, Visibility: v.n.Visibility}, viewerID, snap)
}

// userListReplyShouldEmit is userListReplyShouldEmit over the decoded note.
func (v noteView) userListReplyShouldEmit(viewerID string, authorWithReplies bool, following map[string]bool) bool {
	if v.n == nil {
		return userListReplyShouldEmit(v.raw, viewerID, authorWithReplies, following)
	}
	if v.n.Reply == nil {
		return true
	}
	note := userListReplyPayload{UserID: v.n.UserID, Reply: v.n.Reply.replyMeta()}
	return userListReplyAllows(&note, viewerID, authorWithReplies, following)
}

// forViewer applies hideEmbeds and injectRenoteMyReaction for the connection's
// viewer and returns the bytes to send. It re-encodes only when something has
// to change for this viewer; otherwise it returns the original payload.
func (v noteView) forViewer(ctx stream.ChannelContext, viewerID string) []byte {
	if v.n == nil {
		return injectRenoteMyReaction(hideEmbeds(ctx, v.raw), viewerID)
	}
	n := v.n
	// depth-2 embed (renote.renote / renote.reply) も hideNoteRawForViewer と同じく見る。
	var renoteRenote, renoteReply *streamNote
	if n.Renote != nil {
		renoteRenote, renoteReply = n.Renote.Renote, n.Renote.Reply
	}
	plan := planHide(n.embedProbePtr(), n.replyAuthorID(),
		n.Renote.embedProbePtr(), n.Reply.embedProbePtr(),
		renoteRenote.embedProbePtr(), renoteReply.embedProbePtr(),
		viewerUserFromCtx(ctx), ctx.FollowingSnapshot(), time.Now().UnixMilli())
	if out, changed := applyHidePlan(v.raw, plan); changed {
		// 隠した後の note は、元の解析結果と中身が違う (本文や reactions が空になり
		// うる)。従来どおり、書き換えた後の bytes から myReaction を求め直す。
		// 書き換える接続だけが通る経路。
		return injectRenoteMyReaction(out, viewerID)
	}
	if viewerID == "" || n.Renote == nil || !n.emitProbe().isPureRenote() {
		return v.raw
	}
	reaction := renoteMyReaction(n.Renote.Reactions, n.Renote.ReactionAndUserPairCache, viewerID)
	if reaction == "" {
		return v.raw
	}
	return setRenoteMyReaction(v.raw, reaction)
}

// replyAuthorID returns the reply target's userId ("" when there is none).
func (n *streamNote) replyAuthorID() string {
	if n.Reply == nil {
		return ""
	}
	return n.Reply.UserID
}
