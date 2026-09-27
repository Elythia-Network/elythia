// Package notehide centralizes the per-viewer hideNote gate (#1536 / #1568) so
// EVERY REST endpoint that packs notes applies it uniformly, mirroring upstream
// Misskey's NoteEntityService.hideNote applied in pack().
//
// Two layers, both blanking content in place (entity.HideNoteEntity), never
// filtering rows:
//   - Embedded renote/reply (depth-1) と引用先 (renote.renote, depth-2): the FULL
//     decision (corenote.HideEmbedDecision), including intrinsic
//     followers/specified — embeds have no upstream visibility gate of their own
//     (#1536)。packer が pure renote → quote の引用先 (renote.renote) を depth-2 で
//     出すようになったため、その引用先も同じ embed gate で blank する。
//   - Top-level note: the author-PREFERENCE subset only
//     (corenote.HideNoteByPrefsDecision): treatVisibility downgrade
//     (makeNotesFollowersOnlyBefore), makeNotesHiddenBefore and
//     requireSigninToViewContents. The intrinsic followers/specified hide stays
//     owned by RequireVisible / FilterVisible / SQL push-down / the notes-show
//     ID-known doctrine (#799 / #1488); re-applying it here would re-blank notes
//     those gates deliberately served (#1568).
//   - Top-level notes packed from IDs the viewer saved earlier (i/favorites):
//     HideStoredNotes applies the FULL decision to them as well, because no
//     other gate re-checks visibility on that path.
//
// Streaming has its own per-connection gate in internal/stream/channels.
package notehide

import (
	"time"

	corenote "github.com/shiroha-a/mk/internal/core/note"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// followingRepo is wired once at startup (router) so handlers can call
// HideEmbeds without each threading a FollowingRepository. A nil repo fails
// closed (followers embeds hidden), which is safe.
var followingRepo repository.FollowingRepository

// SetFollowingRepo wires the FollowingRepository used by HideEmbeds. Call once
// during server setup before requests are served.
func SetFollowingRepo(r repository.FollowingRepository) {
	followingRepo = r
}

// HideEmbeds blanks embedded renote/reply content that `viewer` is not allowed
// to see, across the packed batch, using a SINGLE batched follow query for the
// whole page (never a per-embed query). Safe to call on every note-returning
// REST response. viewer==nil (anonymous) hides followers/specified embeds with
// no query.
func HideEmbeds(viewer *model.User, packed []entity.NoteEntity) {
	hideEmbedsAt(viewer, packed, followingRepo, time.Now().UnixMilli())
}

// HideStoredNotes is HideEmbeds plus the FULL hideNote decision on the
// top-level notes (intrinsic followers/specified included), mirroring upstream
// NoteEntityService.pack(note, me) without skipHide.
//
// Use it for endpoints that pack notes from IDs the viewer saved earlier
// (i/favorites) and therefore have no visibility gate of their own: the
// viewer may have lost access since (unfollowed the author, or the author
// removed them), and upstream blanks such notes instead of returning them.
func HideStoredNotes(viewer *model.User, packed []entity.NoteEntity) {
	hideAt(viewer, packed, followingRepo, time.Now().UnixMilli(), true)
}

// HideNotificationNotes applies the per-viewer hideNote gate to the embedded
// `note` of each packed notification (the map shape produced by
// entity.PackNotifications). The notification's note itself gets the top-level
// author-preference gate; its depth-2 renote/reply embeds get the full embed
// gate (#1568). The REST i/notifications path already runs FilterVisible (#1444)
// which drops notes the viewer cannot see at all, so the intrinsic
// followers/specified gate is owned there; this only refines the survivors and
// closes the depth-2 embed leak FilterVisible does not recurse into. One batched
// follow query for the whole page.
func HideNotificationNotes(viewer *model.User, packed []map[string]any) {
	hideNotificationNotesAt(viewer, packed, followingRepo, time.Now().UnixMilli())
}

func hideNotificationNotesAt(viewer *model.User, packed []map[string]any, repo repository.FollowingRepository, nowMs int64) {
	if len(packed) == 0 {
		return
	}
	// Pull the embedded NoteEntity values out so the shared batch-follow + gate
	// machinery (hideEmbedsAt) runs over them, then write the gated value back.
	// Renote/Reply are pointers shared with the map's note, so embed blanks are
	// reflected directly; the top-level blank lives on the value, hence write-back.
	notes := make([]entity.NoteEntity, 0, len(packed))
	idx := make([]int, 0, len(packed))
	for i := range packed {
		n, ok := packed[i]["note"].(entity.NoteEntity)
		if !ok {
			continue
		}
		notes = append(notes, n)
		idx = append(idx, i)
	}
	if len(notes) == 0 {
		return
	}
	hideEmbedsAt(viewer, notes, repo, nowMs)
	for j, i := range idx {
		packed[i]["note"] = notes[j]
	}
}

func hideEmbedsAt(viewer *model.User, packed []entity.NoteEntity, repo repository.FollowingRepository, nowMs int64) {
	hideAt(viewer, packed, repo, nowMs, false)
}

// hideAt is the shared body of HideEmbeds / HideStoredNotes. fullTopLevel
// selects the full decision (HideEmbedDecision) for top-level notes instead of
// the author-preference subset.
func hideAt(viewer *model.User, packed []entity.NoteEntity, repo repository.FollowingRepository, nowMs int64, fullTopLevel bool) {
	if len(packed) == 0 {
		return
	}
	follows := buildFollowSet(viewer, packed, repo, fullTopLevel)
	for i := range packed {
		// HideNoteEntity が要素を mutate できるよう slice element の pointer を渡す。
		if fullTopLevel {
			// 保存済み ID から引いた note は可視性ゲートを通っていないので、
			// intrinsic followers/specified も含めて判定する。
			hideStoredTopLevelIfNeeded(viewer, &packed[i], follows, nowMs)
		} else {
			// 著者設定ゲートを top-level に適用 (intrinsic followers/specified は別ゲート
			// 任せ)。
			hideTopLevelIfNeeded(viewer, &packed[i], follows, nowMs)
		}
		hideEmbedIfNeeded(viewer, packed[i].Renote, follows, nowMs)
		hideEmbedIfNeeded(viewer, packed[i].Reply, follows, nowMs)
		// pure renote → quote の引用先 (renote.renote) と、renote 先の返信先
		// (renote.reply) の depth-2 embed にも同じ gate を適用する。packer が
		// これらを出すため、非公開の note が renote 経由で leak しないよう blank する。
		if packed[i].Renote != nil {
			hideEmbedIfNeeded(viewer, packed[i].Renote.Renote, follows, nowMs)
			hideEmbedIfNeeded(viewer, packed[i].Renote.Reply, follows, nowMs)
		}
		// #2106 L5: treatVisibility downgrade を packed entity の visibility field にも反映する
		// (hide 判定が元の visibility を読み終えた後に書き換える、viewer 非依存)。
		downgradeVisibilityIfNeeded(&packed[i], nowMs)
		downgradeVisibilityIfNeeded(packed[i].Renote, nowMs)
		downgradeVisibilityIfNeeded(packed[i].Reply, nowMs)
		if packed[i].Renote != nil {
			downgradeVisibilityIfNeeded(packed[i].Renote.Renote, nowMs)
			downgradeVisibilityIfNeeded(packed[i].Renote.Reply, nowMs)
		}
	}
}

// downgradeVisibilityIfNeeded rewrites a public/home note's packed visibility to
// 'followers' when the author's makeNotesFollowersOnlyBefore window has passed
// (#2106 L5, upstream treatVisibility). Viewer-independent.
func downgradeVisibilityIfNeeded(n *entity.NoteEntity, nowMs int64) {
	if n == nil {
		return
	}
	if corenote.ShouldDowngradeVisibility(embedFactsFromEntity(n), nowMs) {
		n.Visibility = string(model.NoteVisibilityFollowers)
	}
}

func hideEmbedIfNeeded(viewer *model.User, embed *entity.NoteEntity, follows func(string) bool, nowMs int64) {
	if embed == nil {
		return
	}
	if corenote.HideEmbedDecision(viewer, embedFactsFromEntity(embed), follows, nowMs) {
		entity.HideNoteEntity(embed)
	}
}

// hideTopLevelIfNeeded blanks a top-level note when the author's preference
// gates (HideNoteByPrefsDecision) hide it from viewer. Intrinsic
// followers/specified are intentionally NOT evaluated here (see package doc).
func hideTopLevelIfNeeded(viewer *model.User, n *entity.NoteEntity, follows func(string) bool, nowMs int64) {
	if n == nil {
		return
	}
	if corenote.HideNoteByPrefsDecision(viewer, topLevelFactsFromEntity(n), follows, nowMs) {
		entity.HideNoteEntity(n)
	}
}

// hideStoredTopLevelIfNeeded blanks a top-level note with the full decision
// (HideEmbedDecision), using the reply-target author a top-level note carries.
func hideStoredTopLevelIfNeeded(viewer *model.User, n *entity.NoteEntity, follows func(string) bool, nowMs int64) {
	if corenote.HideEmbedDecision(viewer, topLevelFactsFromEntity(n), follows, nowMs) {
		entity.HideNoteEntity(n)
	}
}

// buildFollowSet resolves, in ONE query, which embed authors `viewer` follows.
// It collects the distinct authors of embeds that may require a follow check
// (followers, plus public/home that could downgrade via the author's
// makeNotesFollowersOnlyBefore), then issues a single FilterFollowingsFromAnchor.
func buildFollowSet(viewer *model.User, packed []entity.NoteEntity, repo repository.FollowingRepository, fullTopLevel bool) func(string) bool {
	never := func(string) bool { return false }
	if viewer == nil || repo == nil {
		return never
	}
	seen := make(map[string]struct{})
	for i := range packed {
		if fullTopLevel {
			// 全判定では top-level の followers も follow 判定が要る (embed と同じ収集)。
			collectEmbedAuthor(&packed[i], viewer.ID, seen)
		} else {
			collectTopLevelAuthor(&packed[i], viewer.ID, seen)
		}
		collectEmbedAuthor(packed[i].Renote, viewer.ID, seen)
		collectEmbedAuthor(packed[i].Reply, viewer.ID, seen)
		// depth-2 embed (renote.renote / renote.reply) の著者も follow 判定対象に含める。
		if packed[i].Renote != nil {
			collectEmbedAuthor(packed[i].Renote.Renote, viewer.ID, seen)
			collectEmbedAuthor(packed[i].Renote.Reply, viewer.ID, seen)
		}
	}
	if len(seen) == 0 {
		return never
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	followed, err := repo.FilterFollowingsFromAnchor(viewer.ID, ids)
	if err != nil {
		// fail-closed: 失敗時は「誰もフォローしていない」扱いで followers embed を隠す。
		return never
	}
	set := make(map[string]struct{}, len(followed))
	for _, id := range followed {
		set[id] = struct{}{}
	}
	return func(id string) bool {
		_, ok := set[id]
		return ok
	}
}

func collectEmbedAuthor(embed *entity.NoteEntity, viewerID string, seen map[string]struct{}) {
	if embed == nil || embed.UserID == "" || embed.UserID == viewerID {
		return
	}
	switch embed.Visibility {
	case string(model.NoteVisibilityFollowers):
		// followers は必ず follow 判定が要る。
	case string(model.NoteVisibilityPublic), string(model.NoteVisibilityHome):
		// public/home は通常 follow 不要。著者が makeNotesFollowersOnlyBefore を
		// 設定している時だけ followers へ降格しうるので、その場合のみ収集する。
		if embed.User.MakeNotesFollowersOnlyBefore == nil {
			return
		}
	default:
		// specified / 不明: follow 判定不要 (specified は visibleUserIds で判定)。
		return
	}
	seen[embed.UserID] = struct{}{}
}

// collectTopLevelAuthor adds a top-level note's author to seen only when the
// prefs gate actually needs a follow check: a public/home note whose author
// opted into makeNotesFollowersOnlyBefore (the treatVisibility downgrade).
// Intrinsic followers/specified top-level notes are not gated here, so they
// require no follow query — keeping the page to one batched lookup.
func collectTopLevelAuthor(n *entity.NoteEntity, viewerID string, seen map[string]struct{}) {
	if n == nil || n.UserID == "" || n.UserID == viewerID {
		return
	}
	switch n.Visibility {
	case string(model.NoteVisibilityPublic), string(model.NoteVisibilityHome):
		if n.User.MakeNotesFollowersOnlyBefore == nil {
			return
		}
	default:
		return
	}
	seen[n.UserID] = struct{}{}
}

// topLevelFactsFromEntity is embedFactsFromEntity plus the reply-target author,
// which IS available for a top-level note (its Reply is packed at depth 1,
// unlike a depth-2 embed). The makeNotesFollowersOnlyBefore downgrade's
// reply-target escape hatch needs it populated.
func topLevelFactsFromEntity(n *entity.NoteEntity) corenote.EmbedFacts {
	f := embedFactsFromEntity(n)
	if n.Reply != nil {
		f.ReplyTargetAuthorID = n.Reply.UserID
	}
	return f
}

// embedFactsFromEntity translates a packed embed NoteEntity into core/note
// EmbedFacts, reading the author preference fields off the embed's UserLite
// (populated only when the embed author was preloaded — AuthorPrefsKnown
// reflects that). ReplyTargetAuthorID は depth-1 embed では取得できない
// (embed 自身の reply target = depth 2 は pack されない) ため常に空のままで、
// core 側の reply-target escape hatch は embed では発火しない (= conservative)。
func embedFactsFromEntity(embed *entity.NoteEntity) corenote.EmbedFacts {
	f := corenote.EmbedFacts{
		AuthorID:       embed.UserID,
		Visibility:     embed.Visibility,
		VisibleUserIDs: embed.VisibleUserIDs,
		Mentions:       embed.Mentions,
		CreatedAtMs:    parseCreatedAtMs(embed.CreatedAt),
	}
	if embed.User.ID != "" {
		f.AuthorPrefsKnown = true
		if embed.User.RequireSigninToViewContents != nil {
			f.RequireSigninToViewContents = *embed.User.RequireSigninToViewContents
		}
		f.MakeNotesHiddenBefore = embed.User.MakeNotesHiddenBefore
		f.MakeNotesFollowersOnlyBefore = embed.User.MakeNotesFollowersOnlyBefore
	}
	return f
}

// parseCreatedAtMs parses the packed RFC3339-ms createdAt back to unix-ms. On
// any failure (empty / unparseable) it returns 0, which fails CLOSED on the
// time-window gates: shouldHideNoteByTime then treats the embed as "created at
// epoch 0", so both the absolute (createdAt <= threshold) and relative (elapsed
// >= window) makeNotes*Before gates HIDE it rather than leak a note whose age
// cannot be verified. createdAt は自前 packer 生成なので実運用では失敗しないが、
// 失敗時に now を返すと絶対 epoch ゲートが fail-OPEN になる (#1567 review)。
func parseCreatedAtMs(s string) int64 {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}
