package webhook

import (
	"time"

	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// gateNoteEmbeds blanks the renote / reply / 引用先 (renote.renote) embeds of n
// that the webhook recipient (viewer) is not allowed to see, using ONE batched
// FilterFollowingsFromAnchor. webhook の note payload は受信者ごとに pack して
// recipient を viewer として gate する (upstream の mention webhook が
// `pack(note, recipient)` で per-recipient gate するのに倣う; note/reply/renote は
// upstream では skipHide=true だが、mk-go は安全側に倒して全イベントで gate する)。
//
// 可視性ゲートの本体は REST (api/notehide) / streaming (stream/channels/note_filter)
// / Web Push (core/webpush) と同型のコピーで、いずれかを変更したら全箇所を同じ
// depth (renote/reply の depth-1 + 引用先 renote.renote の depth-2) で同期させること
// (core は api/notehide を import できないため共有できず、各 boundary に複製している)。
// viewer / repo が nil の場合は fail-closed (followers/specified embed を隠す)。
func gateNoteEmbeds(viewer *model.User, n *entity.NoteEntity, repo repository.FollowingRepository, nowMs int64) {
	if n == nil {
		return
	}
	follows := buildEmbedFollowSet(viewer, n, repo)
	hideEmbedIfNeeded(viewer, n.Renote, follows, nowMs)
	hideEmbedIfNeeded(viewer, n.Reply, follows, nowMs)
	// depth-2 embed (renote.renote / renote.reply)。packer がこれらを出すため、
	// 非公開の note が webhook payload に leak しないよう gate する。
	if n.Renote != nil {
		hideEmbedIfNeeded(viewer, n.Renote.Renote, follows, nowMs)
		hideEmbedIfNeeded(viewer, n.Renote.Reply, follows, nowMs)
	}
}

// buildEmbedFollowSet resolves, in ONE query, which embed authors viewer follows.
// embed 著者のうち follow 判定が要るもの (followers、または makeNotesFollowersOnlyBefore
// で降格しうる public/home) だけを集めて FilterFollowingsFromAnchor を 1 回発行する。
// viewer nil / repo nil / query 失敗は「誰もフォローしていない」(fail-closed)。
func buildEmbedFollowSet(viewer *model.User, n *entity.NoteEntity, repo repository.FollowingRepository) func(string) bool {
	never := func(string) bool { return false }
	if viewer == nil || repo == nil {
		return never
	}
	seen := make(map[string]struct{})
	collectEmbedAuthor(n.Renote, viewer.ID, seen)
	collectEmbedAuthor(n.Reply, viewer.ID, seen)
	if n.Renote != nil {
		collectEmbedAuthor(n.Renote.Renote, viewer.ID, seen)
		collectEmbedAuthor(n.Renote.Reply, viewer.ID, seen)
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

// collectEmbedAuthor adds an embed author to seen only when a follow check is
// actually needed (followers, or public/home that could downgrade via the
// author's makeNotesFollowersOnlyBefore). specified/unknown need no follow query.
// notehide.collectEmbedAuthor / webpush.collectEmbedAuthor と同一。
func collectEmbedAuthor(embed *entity.NoteEntity, viewerID string, seen map[string]struct{}) {
	if embed == nil || embed.UserID == "" || embed.UserID == viewerID {
		return
	}
	switch embed.Visibility {
	case string(model.NoteVisibilityFollowers):
		// followers は必ず follow 判定が要る。
	case string(model.NoteVisibilityPublic), string(model.NoteVisibilityHome):
		if embed.User.MakeNotesFollowersOnlyBefore == nil {
			return
		}
	default:
		return
	}
	seen[embed.UserID] = struct{}{}
}

// hideEmbedIfNeeded blanks an embed in place when corenote.HideEmbedDecision says
// viewer cannot see it. notehide / webpush と同一。
func hideEmbedIfNeeded(viewer *model.User, embed *entity.NoteEntity, follows func(string) bool, nowMs int64) {
	if embed == nil {
		return
	}
	if corenote.HideEmbedDecision(viewer, embedFactsFromEntity(embed), follows, nowMs) {
		entity.HideNoteEntity(embed)
	}
}

// embedFactsFromEntity translates a packed embed NoteEntity into core/note
// EmbedFacts. notehide / webpush の同名関数と同一。
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
// failure returns 0 (fail-closed on the time-window gates). notehide / webpush と同一。
func parseCreatedAtMs(s string) int64 {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}
