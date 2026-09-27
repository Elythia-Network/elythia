package note

import (
	"slices"

	"github.com/shiroha-a/mk/internal/model"
)

// EmbedFacts carries the minimal, source-agnostic facts HideEmbedDecision needs
// to decide whether an embedded renote/reply must be blanked for a viewer.
//
// Callers build it from their own representation (REST from the packed
// entity.NoteEntity, streaming from the decoded payload) so core/note depends on
// neither the entity nor the stream layer.
//
// AuthorPrefsKnown reports whether the embed author's preference fields
// (RequireSigninToViewContents / MakeNotes*Before) were available at pack time.
// When false (= the embed author row was not preloaded), the author-preference
// gates are skipped — the note-intrinsic visibility gates (specified / followers)
// still apply, so the core embed-leak is closed regardless; only the opt-in
// author-pref refinements (which most authors never set) are not evaluated.
type EmbedFacts struct {
	AuthorID                     string
	Visibility                   string
	VisibleUserIDs               []string
	ReplyTargetAuthorID          string
	Mentions                     []string
	RequireSigninToViewContents  bool
	MakeNotesHiddenBefore        *int
	MakeNotesFollowersOnlyBefore *int
	CreatedAtMs                  int64
	AuthorPrefsKnown             bool
}

// HideEmbedDecision reports whether an embedded note described by facts must be
// hidden (its content blanked) from viewer. It is a faithful port of upstream
// Misskey NoteEntityService.treatVisibility followed by shouldHideNote (in that
// order). It is pure: the caller supplies `follows` (does viewer follow
// authorID) from its own batched set / per-connection snapshot, and `nowMs`
// (current unix milliseconds) for the time-window gates. viewer may be nil
// (anonymous), in which case followers/specified embeds are hidden.
//
// The same decision drives both the REST batch path and the streaming
// per-connection path; only the `follows` source differs.
func HideEmbedDecision(viewer *model.User, f EmbedFacts, follows func(authorID string) bool, nowMs int64) bool {
	vis := f.Visibility

	// treatVisibility: 投稿者の makeNotesFollowersOnlyBefore ウィンドウを過ぎた
	// public/home note は、フォロワー判定の前に followers へ降格する。著者設定
	// 由来なので prefs が判明している時のみ。
	if vis == string(model.NoteVisibilityPublic) || vis == string(model.NoteVisibilityHome) {
		if f.AuthorPrefsKnown && ShouldHideNoteByTime(f.MakeNotesFollowersOnlyBefore, f.CreatedAtMs, nowMs) {
			vis = string(model.NoteVisibilityFollowers)
		}
	}

	var viewerID string
	if viewer != nil {
		viewerID = viewer.ID
	}

	// 自分の投稿は常に見える (upstream: meId === userId)。
	if viewer != nil && viewerID == f.AuthorID {
		return false
	}
	// requireSigninToViewContents: 未ログインには隠す。
	if f.AuthorPrefsKnown && f.RequireSigninToViewContents && viewer == nil {
		return true
	}
	// makeNotesHiddenBefore: 期限切れの古い note を隠す。
	if f.AuthorPrefsKnown && ShouldHideNoteByTime(f.MakeNotesHiddenBefore, f.CreatedAtMs, nowMs) {
		return true
	}
	// specified: 指定ユーザー以外には隠す (note-intrinsic, prefs 不要)。
	if vis == string(model.NoteVisibilitySpecified) {
		if viewer == nil {
			return true
		}
		return !slices.Contains(f.VisibleUserIDs, viewerID)
	}
	// followers: 投稿者のフォロワー / reply 先本人 / mention 対象以外には隠す。
	if vis == string(model.NoteVisibilityFollowers) {
		return !viewerIsFollowersRecipient(viewerID, f, follows)
	}

	return false
}

// HideNoteByPrefsDecision reports whether a TOP-LEVEL note described by facts
// must be blanked from viewer due to the author's PREFERENCE gates only:
// upstream treatVisibility (makeNotesFollowersOnlyBefore downgrade),
// makeNotesHiddenBefore and requireSigninToViewContents, plus the own-note
// short-circuit.
//
// Unlike HideEmbedDecision it deliberately does NOT hide a note merely because
// its intrinsic visibility is followers. At the top level that gate is owned by
// CanSeeNote / FilterVisible / the SQL push-down / the notes-show ID-known
// doctrine (#799 / #1488); re-applying it here would silently re-blank notes
// those gates deliberately served. `specified` IS evaluated here, because the
// SQL push-down (upstream generateVisibilityQuery) lets a note through on a
// `mentions` match regardless of visibility while upstream shouldHideNote still
// blanks a direct note whose viewer is not in visibleUserIds — 本文で @ された
// だけの相手に direct note の中身を見せないための gate。 The downgrade branch DOES end in a
// followers-style recipient check, but it only fires when the author opted into
// makeNotesFollowersOnlyBefore on a public/home note (= new author-pref
// coverage, not the intrinsic followers gate) (#1568).
//
// Pure, same contract as HideEmbedDecision: caller supplies `follows` and
// `nowMs`. viewer may be nil (anonymous).
func HideNoteByPrefsDecision(viewer *model.User, f EmbedFacts, follows func(authorID string) bool, nowMs int64) bool {
	var viewerID string
	if viewer != nil {
		viewerID = viewer.ID
	}

	// 自分の投稿は常に見える (upstream: meId === userId)。
	if viewer != nil && viewerID == f.AuthorID {
		return false
	}
	// specified (direct) は visibleUserIds に入っていない viewer には隠す
	// (upstream shouldHideNote の specified 分岐)。著者 prefs の有無に依らない
	// intrinsic gate なので AuthorPrefsKnown より前に評価する。
	if f.Visibility == string(model.NoteVisibilitySpecified) {
		if viewer == nil || !slices.Contains(f.VisibleUserIDs, viewerID) {
			return true
		}
	}
	// 著者 prefs が不明なら著者設定ゲートは評価しない。top-level の intrinsic
	// followers は別ゲート (CanSeeNote 等) が担うのでここでは隠さない。
	if !f.AuthorPrefsKnown {
		return false
	}
	// requireSigninToViewContents: 未ログインには隠す。
	if f.RequireSigninToViewContents && viewer == nil {
		return true
	}
	// makeNotesHiddenBefore: 期限切れの古い note を隠す。
	if ShouldHideNoteByTime(f.MakeNotesHiddenBefore, f.CreatedAtMs, nowMs) {
		return true
	}
	// treatVisibility: public/home が makeNotesFollowersOnlyBefore ウィンドウを
	// 過ぎたら followers へ降格。降格した時だけ followers-recipient 判定で hide
	// する (元から followers/specified の note は intrinsic ゲート任せ)。
	if f.Visibility == string(model.NoteVisibilityPublic) || f.Visibility == string(model.NoteVisibilityHome) {
		if ShouldHideNoteByTime(f.MakeNotesFollowersOnlyBefore, f.CreatedAtMs, nowMs) {
			return !viewerIsFollowersRecipient(viewerID, f, follows)
		}
	}
	return false
}

// ShouldDowngradeVisibility reports whether a public/home note must have its
// PACKED visibility field downgraded to 'followers' because the author opted into
// makeNotesFollowersOnlyBefore and the note is past that window (#2106 L5, upstream
// treatVisibility). Viewer-INDEPENDENT — even a follower who can still see the note
// receives visibility:'followers' (drives the frontend lock icon / renote 可否)。
// content-hide は別 concern (HideNoteByPrefsDecision)。
func ShouldDowngradeVisibility(f EmbedFacts, nowMs int64) bool {
	if !f.AuthorPrefsKnown {
		return false
	}
	if f.Visibility != string(model.NoteVisibilityPublic) && f.Visibility != string(model.NoteVisibilityHome) {
		return false
	}
	return ShouldHideNoteByTime(f.MakeNotesFollowersOnlyBefore, f.CreatedAtMs, nowMs)
}

// viewerIsFollowersRecipient reports whether viewer is allowed to see a
// followers-visibility note: the reply-target author, a mentioned user, or a
// follower of the author. Anonymous (viewerID == "") is never a recipient.
// Shared by HideEmbedDecision's intrinsic followers branch and
// HideNoteByPrefsDecision's makeNotesFollowersOnlyBefore downgrade branch so the
// escape-hatch logic cannot drift between the two entry points.
func viewerIsFollowersRecipient(viewerID string, f EmbedFacts, follows func(authorID string) bool) bool {
	if viewerID == "" {
		return false
	}
	if f.ReplyTargetAuthorID != "" && viewerID == f.ReplyTargetAuthorID {
		return true
	}
	if slices.Contains(f.Mentions, viewerID) {
		return true
	}
	return follows != nil && follows(f.AuthorID)
}

// ShouldHideNoteByTime ports upstream misc/should-hide-note-by-time.ts.
//
// hiddenBefore は秒単位。nil は無効 (隠さない)。<= 0 は「作成からの経過秒」での相対
// 判定で、経過 >= |hiddenBefore| 秒なら隠す。> 0 は絶対 epoch 秒での判定で、
// createdAt(秒) <= hiddenBefore なら隠す。createdAtMs / nowMs はミリ秒。
func ShouldHideNoteByTime(hiddenBefore *int, createdAtMs, nowMs int64) bool {
	if hiddenBefore == nil {
		return false
	}
	v := *hiddenBefore
	if v <= 0 {
		elapsedSeconds := float64(nowMs-createdAtMs) / 1000.0
		return elapsedSeconds >= float64(-v)
	}
	createdAtSeconds := float64(createdAtMs) / 1000.0
	return createdAtSeconds <= float64(v)
}
