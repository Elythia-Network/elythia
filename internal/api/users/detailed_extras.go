package users

import (
	"context"

	"github.com/shiroha-a/mk/internal/api/meself"
	"github.com/shiroha-a/mk/internal/api/notehide"
	"github.com/shiroha-a/mk/internal/core/notesfilter"
	"github.com/shiroha-a/mk/internal/core/userpack"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// FillDetailedExtrasMany fills movedTo / alsoKnownAs, pinnedNoteIds /
// pinnedNotes and pinnedPageId / pinnedPage for every user of a list response,
// mirroring upstream UserEntityService.packMany. It satisfies
// userpack.DetailExtrasMany.
//
// 本家 packMany はピン留めを閲覧者がいるときだけ IN でまとめて引き、匿名の
// 閲覧者には空で返す (pinNotes が空の Map のまま pack に渡る)。ピン留めの
// ページと移行先は利用者ごとに解決する。mk-go はピン留めのノートとページを
// まとめて引き、利用者の数に比例して問い合わせが増えないようにする。
func (h *Handler) FillDetailedExtrasMany(ctx context.Context, viewer *model.User, targets []userpack.DetailTarget) {
	if len(targets) == 0 {
		return
	}
	users := make([]*model.User, len(targets))
	lites := make([]*entity.UserLite, len(targets))
	for i, t := range targets {
		users[i], lites[i] = t.User, &t.Detailed.UserLite
	}
	h.fillUserLites(users, lites)
	h.fillExtras(ctx, viewer, targets, false)
}

// fillUserLites resolves instance / emojis of the packed users of a list
// response in one batch (本家 packMany の instance と emojis)。
func (h *Handler) fillUserLites(users []*model.User, lites []*entity.UserLite) {
	entity.FillUserLites(h.instanceLookup(), h.emojiLookup(), users, lites)
}

// packLites packs users as upstream packMany(users) packs UserLite lists:
// instance and emojis are resolved in one batch.
func (h *Handler) packLites(users []*model.User) []entity.UserLite {
	out := make([]entity.UserLite, len(users))
	lites := make([]*entity.UserLite, len(users))
	for i, u := range users {
		out[i] = entity.PackUserLite(u)
		lites[i] = &out[i]
	}
	h.fillUserLites(users, lites)
	return out
}

// packLitesByID is packLites keyed by user ID, for responses that embed the
// same user in several rows.
func (h *Handler) packLitesByID(users []*model.User) map[string]entity.UserLite {
	lites := h.packLites(users)
	out := make(map[string]entity.UserLite, len(lites))
	for i, u := range users {
		out[u.ID] = lites[i]
	}
	return out
}

// reactionUsers returns the distinct reactors of rows.
func reactionUsers(rows []*model.NoteReaction) []*model.User {
	seen := make(map[string]struct{}, len(rows))
	out := make([]*model.User, 0, len(rows))
	for _, r := range rows {
		if r.User == nil {
			continue
		}
		if _, ok := seen[r.User.ID]; ok {
			continue
		}
		seen[r.User.ID] = struct{}{}
		out = append(out, r.User)
	}
	return out
}

// fillExtras is the shared body of FillDetailedExtras (single pack) and
// FillDetailedExtrasMany (packMany). pinsForAnonymous selects the single-pack
// rule that also shows the pins to an anonymous viewer.
func (h *Handler) fillExtras(ctx context.Context, viewer *model.User, targets []userpack.DetailTarget, pinsForAnonymous bool) {
	if len(targets) == 0 {
		return
	}
	for _, t := range targets {
		// 移行先・別名は設定している利用者だけ引くので、一覧でも問い合わせは
		// その人数分に限られる。
		t.Detailed.ResolveMoveTargets(t.User, h.resolveUserIDByURI)
	}
	if viewer != nil || pinsForAnonymous {
		h.fillPinnedNotes(ctx, viewer, targets)
	}
	h.fillPinnedPages(viewer, targets)
}

// listPinnedNoteIDs returns each user's pinned note IDs ordered by pin id DESC.
//
// 本家 pack は `orderBy('pin.id', 'DESC')`、packMany も同じ順に並べ直す。
func (h *Handler) listPinnedNoteIDs(targets []userpack.DetailTarget) map[string][]string {
	out := map[string][]string{}
	if h.piningRepo == nil {
		return out
	}
	add := func(pins []*model.UserNotePining) {
		for _, p := range pins {
			out[p.UserID] = append(out[p.UserID], p.NoteID)
		}
	}
	if batch, ok := h.piningRepo.(repository.UserNotePiningBatchReader); ok && len(targets) > 1 {
		ids := make([]string, 0, len(targets))
		for _, t := range targets {
			ids = append(ids, t.User.ID)
		}
		if pins, err := batch.ListByUsers(ids); err == nil {
			add(pins)
		}
		return out
	}
	for _, t := range targets {
		if pins, err := h.piningRepo.ListByUser(t.User.ID); err == nil {
			add(pins)
		}
	}
	return out
}

// fillPinnedNotes populates PinnedNoteIDs / PinnedNotes.
//
// 設計メモ — PinnedNoteIDs を visibility filter 前の生 ID 配列で返す理由 (#1489):
//
//   - pin = author の意図的な self-disclosure 行為。「followers にだけ見せる
//     note を profile に固定する」と author が選んだ時点で、pinnedNoteIds が
//     viewer に露出することは upstream Misskey TS でも同 shape (drop-in 互換)。
//   - notes/show ShowForAPI doctrine の境界線上だが、author 自身が pin を選んで
//     いる以上、ID-known な viewer に content が返るのは「意図された情報開示」
//     の範疇とする (#1489 で議論)。
//   - PinnedNotes 本体 (= 中身の埋め込み) は FilterVisible で絞るため、profile
//     表示上は viewer から見えない pin はカード化されない。
//   - pinning は per-user 上限が厳しい (= 数件) ので mass enumeration リスクは
//     低い。
//
// この設計は Option A (現状維持 = upstream-aligned) を採用した結果 (#1489
// wontfix)。Option B (IDs も filter) は frontend drop-in 互換と「author 意図」
// に逆行するため不採用。
func (h *Handler) fillPinnedNotes(ctx context.Context, viewer *model.User, targets []userpack.DetailTarget) {
	pinned := h.listPinnedNoteIDs(targets)
	if len(pinned) == 0 {
		return
	}
	all := make([]string, 0)
	for _, t := range targets {
		if ids := pinned[t.User.ID]; len(ids) > 0 {
			// PinnedNoteIDs は意図的に filter 前の生 IDs (上記設計メモ参照)。
			t.Detailed.PinnedNoteIDs = ids
			all = append(all, ids...)
		}
	}
	if h.noteRepo == nil {
		return
	}
	primary, ok := h.noteRepo.(repository.NotePrimaryReader)
	if !ok {
		return
	}
	notes, err := primary.FindManyByIDsWithUserOnPrimary(all)
	if err != nil {
		return
	}
	notes = notesfilter.FilterVisible(viewer, notes, h.followingRepo)
	entities := entity.PackNotes(ctx, notes, h.idGen, h.instanceLookup(), h.emojiLookup(), h.reactionReader())
	h.fieldRes.Apply(entities, viewer)
	byID := make(map[string]entity.NoteEntity, len(entities))
	for _, e := range entities {
		byID[e.ID] = e
	}
	for _, t := range targets {
		ids := pinned[t.User.ID]
		if len(ids) == 0 {
			continue
		}
		mine := make([]entity.NoteEntity, 0, len(ids))
		for _, nid := range ids {
			if e, ok := byID[nid]; ok {
				mine = append(mine, e)
			}
		}
		notehide.HideProfilePinnedNotes(viewer, mine, t.User.ID)
		packed := make([]any, 0, len(mine))
		for _, pn := range mine {
			packed = append(packed, pn)
		}
		t.Detailed.PinnedNotes = packed
	}
}

// fillPinnedPages populates PinnedPageID / PinnedPage from the profiles.
func (h *Handler) fillPinnedPages(viewer *model.User, targets []userpack.DetailTarget) {
	var pageIDs []string
	for _, t := range targets {
		if t.Profile != nil && t.Profile.PinnedPageID != nil && *t.Profile.PinnedPageID != "" {
			t.Detailed.PinnedPageID = t.Profile.PinnedPageID
			pageIDs = append(pageIDs, *t.Profile.PinnedPageID)
		}
	}
	if len(pageIDs) == 0 || h.pageRepo == nil {
		return
	}
	pages := map[string]*model.Page{}
	if len(pageIDs) == 1 {
		if p, err := h.pageRepo.FindByID(pageIDs[0]); err == nil && p != nil {
			pages[p.ID] = p
		}
	} else if rows, err := h.pageRepo.FindManyByIDs(pageIDs); err == nil {
		for _, p := range rows {
			pages[p.ID] = p
		}
	}
	for _, t := range targets {
		if t.Detailed.PinnedPageID == nil {
			continue
		}
		if p := pages[*t.Detailed.PinnedPageID]; pinnedPageVisibleTo(p, viewer) {
			// golden Page は user 必須。pinnedPage は profile user 自身の page
			// なので owner=u を渡して user (UserLite) を埋める (#1266 follow-up)。
			t.Detailed.PinnedPage = entity.PackPageWithContext(p, entity.PackPageContext{IDGen: h.idGen, Owner: t.User})
		}
	}
}

// detailedBatch collects the packed users of a list response so their detail
// extras are filled in one batch before the response is written.
type detailedBatch []userpack.DetailTarget

// add stores a copy of d and returns the pointer the extras will be filled into.
func (b *detailedBatch) add(d entity.UserDetailed, u *model.User, profile *model.UserProfile) *entity.UserDetailed {
	dd := d
	*b = append(*b, userpack.DetailTarget{User: u, Profile: profile, Detailed: &dd})
	return &dd
}

// fill fills the detail extras of every collected user.
func (b detailedBatch) fill(ctx context.Context, h *Handler, viewer *model.User) {
	h.FillDetailedExtrasMany(ctx, viewer, b)
}

// packAll fills the extras and serializes every user like meself.Pack (the
// viewer's own entry becomes MeDetailed).
func (b detailedBatch) packAll(ctx context.Context, h *Handler, viewer *model.User) []any {
	b.fill(ctx, h, viewer)
	out := make([]any, 0, len(b))
	for _, t := range b {
		out = append(out, meself.Pack(ctx, *t.Detailed, t.User, t.Profile, viewer))
	}
	return out
}
