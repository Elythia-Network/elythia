package notehide

import (
	"testing"

	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

const heNowMs int64 = 1_700_000_000_000

func heStr(s string) *string { return &s }

type countingFollowRepo struct {
	*testutil.MockFollowingRepository
	calls int
}

func (c *countingFollowRepo) FilterFollowingsFromAnchor(anchorID string, candidateIDs []string) ([]string, error) {
	c.calls++
	return c.MockFollowingRepository.FilterFollowingsFromAnchor(anchorID, candidateIDs)
}

func followsRepo(pairs ...[2]string) *countingFollowRepo {
	m := testutil.NewMockFollowingRepository()
	for i, p := range pairs {
		m.Followings[string(rune('a'+i))] = &model.Following{FollowerID: p[0], FolloweeID: p[1]}
	}
	return &countingFollowRepo{MockFollowingRepository: m}
}

func followersEmbed(id, author string) *entity.NoteEntity {
	return &entity.NoteEntity{
		ID: id, UserID: author, CreatedAt: "2026-01-02T03:04:05.000Z",
		User: entity.UserLite{ID: author}, Visibility: "followers",
		Text: heStr("secret " + id), FileIDs: []string{}, Files: []any{},
		VisibleUserIDs: []string{}, Mentions: []string{},
	}
}

func noteWithRenote(parentID string, embed *entity.NoteEntity) entity.NoteEntity {
	return entity.NoteEntity{ID: parentID, UserID: "parent-author", CreatedAt: "2026-01-02T03:04:05.000Z", Renote: embed}
}

func TestHideEmbedsAt_SingleQueryForMixedAuthors(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	repo := followsRepo([2]string{"viewer", "a1"})
	packed := []entity.NoteEntity{
		noteWithRenote("p1", followersEmbed("e1", "a1")),
		noteWithRenote("p2", followersEmbed("e2", "a2")),
		noteWithRenote("p3", followersEmbed("e3", "a3")),
	}
	hideEmbedsAt(viewer, packed, repo, heNowMs)

	if repo.calls != 1 {
		t.Fatalf("expected exactly 1 batched follow query, got %d", repo.calls)
	}
	if packed[0].Renote.IsHidden || packed[0].Renote.Text == nil {
		t.Error("embed of followed author must stay visible")
	}
	if !packed[1].Renote.IsHidden || packed[1].Renote.Text != nil {
		t.Error("embed of non-followed author must be hidden")
	}
	if !packed[2].Renote.IsHidden {
		t.Error("embed e3 must be hidden")
	}
}

// depth-2 引用先 (renote.renote) が followers-only の場合、非フォロワー viewer には
// blank される (#timeline nested quote の embed leak 防止)。renote (quote) 自体は
// public なので残る。
func TestHideEmbedsAt_DepthTwoQuoteTargetHidden(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	repo := followsRepo() // viewer は誰もフォローしていない
	quoteTarget := followersEmbed("qt", "secret-author")
	quote := &entity.NoteEntity{
		ID: "quote", UserID: "qa", CreatedAt: "2026-01-02T03:04:05.000Z",
		User: entity.UserLite{ID: "qa"}, Visibility: "public", Text: heStr("quoting"),
		FileIDs: []string{}, Files: []any{}, VisibleUserIDs: []string{}, Mentions: []string{},
		Renote: quoteTarget,
	}
	packed := []entity.NoteEntity{noteWithRenote("boost", quote)}
	hideEmbedsAt(viewer, packed, repo, heNowMs)

	if packed[0].Renote.IsHidden || packed[0].Renote.Text == nil {
		t.Error("public quote embed must stay visible")
	}
	if !packed[0].Renote.Renote.IsHidden || packed[0].Renote.Renote.Text != nil {
		t.Error("depth-2 followers-only quote target must be hidden for non-follower")
	}
}

func TestHideEmbedsAt_PublicBatchZeroQuery(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	repo := followsRepo()
	pub := &entity.NoteEntity{ID: "e", UserID: "a", CreatedAt: "2026-01-02T03:04:05.000Z", User: entity.UserLite{ID: "a"}, Visibility: "public", Text: heStr("hi"), FileIDs: []string{}, Files: []any{}, VisibleUserIDs: []string{}, Mentions: []string{}}
	packed := []entity.NoteEntity{noteWithRenote("p", pub)}
	hideEmbedsAt(viewer, packed, repo, heNowMs)

	if repo.calls != 0 {
		t.Fatalf("public-only batch must issue ZERO follow queries, got %d", repo.calls)
	}
	if packed[0].Renote.IsHidden {
		t.Error("public embed must not be hidden")
	}
}

func TestHideEmbedsAt_AnonymousHidesFollowersKeepsPublic(t *testing.T) {
	repo := followsRepo()
	pub := &entity.NoteEntity{ID: "pub", UserID: "a", CreatedAt: "2026-01-02T03:04:05.000Z", User: entity.UserLite{ID: "a"}, Visibility: "public", Text: heStr("hi"), FileIDs: []string{}, Files: []any{}, VisibleUserIDs: []string{}, Mentions: []string{}}
	packed := []entity.NoteEntity{
		noteWithRenote("p1", followersEmbed("e1", "a1")),
		noteWithRenote("p2", pub),
	}
	hideEmbedsAt(nil, packed, repo, heNowMs)

	if repo.calls != 0 {
		t.Fatalf("anonymous viewer must not query, got %d", repo.calls)
	}
	if !packed[0].Renote.IsHidden {
		t.Error("anonymous must not see followers embed")
	}
	if packed[1].Renote.IsHidden {
		t.Error("anonymous must still see public embed")
	}
}

func TestHideEmbedsAt_OwnEmbedVisibleNoQuery(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	repo := followsRepo()
	own := followersEmbed("e", "viewer")
	packed := []entity.NoteEntity{noteWithRenote("p", own)}
	hideEmbedsAt(viewer, packed, repo, heNowMs)

	if repo.calls != 0 {
		t.Fatalf("own embed needs no query, got %d", repo.calls)
	}
	if packed[0].Renote.IsHidden {
		t.Error("own followers embed must stay visible")
	}
}

func TestHideEmbedsAt_SpecifiedMembership(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	repo := followsRepo()
	in := &entity.NoteEntity{ID: "in", UserID: "a", CreatedAt: "2026-01-02T03:04:05.000Z", User: entity.UserLite{ID: "a"}, Visibility: "specified", Text: heStr("dm"), FileIDs: []string{}, Files: []any{}, VisibleUserIDs: []string{"viewer"}, Mentions: []string{}}
	out := &entity.NoteEntity{ID: "out", UserID: "a", CreatedAt: "2026-01-02T03:04:05.000Z", User: entity.UserLite{ID: "a"}, Visibility: "specified", Text: heStr("dm"), FileIDs: []string{}, Files: []any{}, VisibleUserIDs: []string{"someone-else"}, Mentions: []string{}}
	packed := []entity.NoteEntity{noteWithRenote("p1", in), noteWithRenote("p2", out)}
	hideEmbedsAt(viewer, packed, repo, heNowMs)

	if repo.calls != 0 {
		t.Fatalf("specified is decided from visibleUserIds, ZERO query expected, got %d", repo.calls)
	}
	if packed[0].Renote.IsHidden {
		t.Error("specified embed listing viewer must be visible")
	}
	if !packed[1].Renote.IsHidden {
		t.Error("specified embed not listing viewer must be hidden")
	}
}

func TestParseCreatedAtMs(t *testing.T) {
	// 失敗時は fail-closed のため 0 を返す (#1567 review): 絶対 epoch ゲートで
	// 0 <= threshold となり隠す側に倒れる。
	if got := parseCreatedAtMs(""); got != 0 {
		t.Errorf("empty -> 0 (fail-closed), got %d", got)
	}
	if got := parseCreatedAtMs("not-a-time"); got != 0 {
		t.Errorf("invalid -> 0 (fail-closed), got %d", got)
	}
	if got := parseCreatedAtMs("2023-11-14T22:13:20.000Z"); got != 1_700_000_000_000 {
		t.Errorf("valid RFC3339-ms parse, got %d", got)
	}
}

func TestEmbedFactsFromEntity_AuthorPrefs(t *testing.T) {
	tr := true
	hb := -3600
	fb := -7200
	e := &entity.NoteEntity{
		ID: "e", UserID: "a", CreatedAt: "2023-11-14T22:13:20.000Z",
		Visibility: "public", VisibleUserIDs: []string{"x"}, Mentions: []string{"m"},
		User: entity.UserLite{ID: "a", RequireSigninToViewContents: &tr, MakeNotesHiddenBefore: &hb, MakeNotesFollowersOnlyBefore: &fb},
	}
	f := embedFactsFromEntity(e)
	if !f.AuthorPrefsKnown || !f.RequireSigninToViewContents || f.MakeNotesHiddenBefore == nil || *f.MakeNotesHiddenBefore != -3600 || f.MakeNotesFollowersOnlyBefore == nil {
		t.Errorf("author prefs not populated: %+v", f)
	}
	if f.AuthorID != "a" || f.CreatedAtMs != 1_700_000_000_000 {
		t.Errorf("base facts wrong: %+v", f)
	}
}

func TestEmbedFactsFromEntity_NoAuthorPrefs(t *testing.T) {
	// embed.User.ID == "" => prefs not known.
	e := &entity.NoteEntity{ID: "e", UserID: "a", CreatedAt: "2023-11-14T22:13:20.000Z", Visibility: "followers"}
	f := embedFactsFromEntity(e)
	if f.AuthorPrefsKnown {
		t.Error("AuthorPrefsKnown must be false when embed.User unset")
	}
}

func TestHideEmbedsAt_MakeNotesHiddenBeforeHidesEvenFollower(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	repo := followsRepo([2]string{"viewer", "author"}) // viewer follows author
	hb := -1                                           // hide notes older than 1s
	old := &entity.NoteEntity{
		ID: "old", UserID: "author", CreatedAt: "2020-01-01T00:00:00.000Z", // far in the past vs heNowMs
		User: entity.UserLite{ID: "author", MakeNotesHiddenBefore: &hb}, Visibility: "public",
		Text: heStr("old"), FileIDs: []string{}, Files: []any{}, VisibleUserIDs: []string{}, Mentions: []string{},
	}
	packed := []entity.NoteEntity{noteWithRenote("p", old)}
	hideEmbedsAt(viewer, packed, repo, heNowMs)
	if !packed[0].Renote.IsHidden {
		t.Error("makeNotesHiddenBefore must hide an old embed even for a follower")
	}
}

// TestHideEmbedsAt_UnparseableCreatedAtFailsClosed guards the #1567 review fix:
// an embed whose createdAt cannot be parsed must FAIL CLOSED on the absolute
// makeNotesHiddenBefore gate. Before the fix the parser returned now, so for an
// absolute (positive epoch, in the past) threshold `now_seconds <= threshold`
// was false and the old note leaked. With createdAt -> 0 it now hides.
func TestHideEmbedsAt_UnparseableCreatedAtFailsClosed(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	repo := followsRepo()
	hb := 1_600_000_000 // absolute epoch seconds, before heNowMs
	bad := &entity.NoteEntity{
		ID: "bad", UserID: "author", CreatedAt: "not-a-timestamp",
		User: entity.UserLite{ID: "author", MakeNotesHiddenBefore: &hb}, Visibility: "public",
		Text: heStr("old"), FileIDs: []string{}, Files: []any{}, VisibleUserIDs: []string{}, Mentions: []string{},
	}
	packed := []entity.NoteEntity{noteWithRenote("p", bad)}
	hideEmbedsAt(viewer, packed, repo, heNowMs)
	if !packed[0].Renote.IsHidden {
		t.Error("unparseable createdAt must fail CLOSED on absolute makeNotesHiddenBefore")
	}
}

func TestHideEmbeds_PackageLevelRepo(t *testing.T) {
	// exercise the exported HideEmbeds + package-level repo wiring.
	prev := followingRepo
	defer func() { followingRepo = prev }()
	SetFollowingRepo(testutil.NewMockFollowingRepository())
	viewer := &model.User{ID: "viewer"}
	packed := []entity.NoteEntity{noteWithRenote("p", followersEmbed("e", "author"))}
	HideEmbeds(viewer, packed)
	if !packed[0].Renote.IsHidden {
		t.Error("followers embed must be hidden via package-level HideEmbeds for a non-follower")
	}
}

func hePtrInt(v int) *int { return &v }

// tlNote builds a top-level note authored by `author` with visibility `vis`.
// user carries the author preference fields; its ID is forced to author.
func tlNote(id, author, vis, createdAt string, user entity.UserLite) entity.NoteEntity {
	user.ID = author
	return entity.NoteEntity{
		ID: id, UserID: author, CreatedAt: createdAt, User: user, Visibility: vis,
		Text: heStr("top " + id), FileIDs: []string{}, Files: []any{},
		VisibleUserIDs: []string{}, Mentions: []string{},
	}
}

// pastWindow is well before heNowMs, so any negative-relative time gate fires.
const pastWindow = "2020-01-01T00:00:00.000Z"

func TestHideTopLevelAt_FollowersOnlyBeforeDowngrade(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	t.Run("hidden from non-follower", func(t *testing.T) {
		repo := followsRepo()
		packed := []entity.NoteEntity{tlNote("n", "author", "public", pastWindow, entity.UserLite{MakeNotesFollowersOnlyBefore: hePtrInt(-3600)})}
		hideEmbedsAt(viewer, packed, repo, heNowMs)
		if repo.calls != 1 {
			t.Fatalf("downgrade candidate must batch one follow query, got %d", repo.calls)
		}
		if !packed[0].IsHidden || packed[0].Text != nil {
			t.Error("public note past makeNotesFollowersOnlyBefore must be blanked for non-follower")
		}
	})
	t.Run("visible to follower", func(t *testing.T) {
		repo := followsRepo([2]string{"viewer", "author"})
		packed := []entity.NoteEntity{tlNote("n", "author", "public", pastWindow, entity.UserLite{MakeNotesFollowersOnlyBefore: hePtrInt(-3600)})}
		hideEmbedsAt(viewer, packed, repo, heNowMs)
		if packed[0].IsHidden {
			t.Error("downgraded note must stay visible to a follower")
		}
	})
}

func TestHideTopLevelAt_MakeNotesHiddenBefore(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	repo := followsRepo([2]string{"viewer", "author"}) // even a follower
	packed := []entity.NoteEntity{tlNote("n", "author", "public", pastWindow, entity.UserLite{MakeNotesHiddenBefore: hePtrInt(-1)})}
	hideEmbedsAt(viewer, packed, repo, heNowMs)
	if !packed[0].IsHidden {
		t.Error("makeNotesHiddenBefore must blank an old top-level note even for a follower")
	}
}

func TestHideTopLevelAt_RequireSignin(t *testing.T) {
	pref := entity.UserLite{RequireSigninToViewContents: heBool(true)}
	t.Run("anonymous blanked", func(t *testing.T) {
		repo := followsRepo()
		packed := []entity.NoteEntity{tlNote("n", "author", "public", pastWindow, pref)}
		hideEmbedsAt(nil, packed, repo, heNowMs)
		if !packed[0].IsHidden {
			t.Error("requireSigninToViewContents must blank a top-level note for anonymous")
		}
	})
	t.Run("authenticated visible", func(t *testing.T) {
		repo := followsRepo()
		packed := []entity.NoteEntity{tlNote("n", "author", "public", pastWindow, pref)}
		hideEmbedsAt(&model.User{ID: "viewer"}, packed, repo, heNowMs)
		if packed[0].IsHidden {
			t.Error("requireSignin must not blank for an authenticated viewer")
		}
	})
}

// TestHideTopLevelAt_IntrinsicNotBlanked locks in #799/#1488 for followers: the
// top-level prefs gate must NOT blank a followers note merely for its intrinsic
// visibility — it is served deliberately by CanSeeNote / push-down and the
// notes-show ID-known doctrine. specified は逆に、upstream shouldHideNote と
// 同じく visibleUserIds 外の viewer には blank する。
func TestHideTopLevelAt_IntrinsicNotBlanked(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	repo := followsRepo() // viewer follows nobody
	packed := []entity.NoteEntity{
		tlNote("f", "a1", "followers", "2026-01-02T03:04:05.000Z", entity.UserLite{}),
		tlNote("s", "a2", "specified", "2026-01-02T03:04:05.000Z", entity.UserLite{}),
	}
	packed[2-1].VisibleUserIDs = []string{"someone-else"}
	hideEmbedsAt(viewer, packed, repo, heNowMs)
	if packed[0].IsHidden {
		t.Error("intrinsic followers top-level must NOT be blanked by the prefs gate (#799)")
	}
	if !packed[1].IsHidden {
		t.Error("specified top-level must be blanked for a non-recipient (upstream shouldHideNote)")
	}
}

// specified でも visibleUserIds に入っている viewer には blank しない。
func TestHideTopLevelAt_SpecifiedRecipientKept(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	packed := []entity.NoteEntity{
		tlNote("s", "a2", "specified", "2026-01-02T03:04:05.000Z", entity.UserLite{}),
	}
	packed[0].VisibleUserIDs = []string{"viewer"}
	hideEmbedsAt(viewer, packed, followsRepo(), heNowMs)
	if packed[0].IsHidden {
		t.Error("specified top-level must survive for a recipient")
	}
}

func TestHideTopLevelAt_OwnNoteVerbatim(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	repo := followsRepo()
	packed := []entity.NoteEntity{tlNote("n", "viewer", "public", pastWindow, entity.UserLite{MakeNotesHiddenBefore: hePtrInt(-1), RequireSigninToViewContents: heBool(true)})}
	hideEmbedsAt(viewer, packed, repo, heNowMs)
	if packed[0].IsHidden {
		t.Error("author must always see their own top-level note")
	}
}

// TestHideTopLevelAt_SingleQueryTopAndEmbed verifies a page mixing a top-level
// downgrade candidate and an embed-followers author still issues exactly one
// batched follow query, and both are gated.
func TestHideTopLevelAt_SingleQueryTopAndEmbed(t *testing.T) {
	viewer := &model.User{ID: "viewer"}
	repo := followsRepo()
	top := tlNote("top", "tlauthor", "public", pastWindow, entity.UserLite{MakeNotesFollowersOnlyBefore: hePtrInt(-3600)})
	withEmbed := noteWithRenote("p", followersEmbed("e", "embedauthor"))
	packed := []entity.NoteEntity{top, withEmbed}
	hideEmbedsAt(viewer, packed, repo, heNowMs)
	if repo.calls != 1 {
		t.Fatalf("top-level + embed authors must batch into one query, got %d", repo.calls)
	}
	if !packed[0].IsHidden {
		t.Error("top-level downgrade candidate must be blanked for non-follower")
	}
	if !packed[1].Renote.IsHidden {
		t.Error("embed followers note must be blanked for non-follower")
	}
}

func heBool(b bool) *bool { return &b }

func notifNoteMap(note entity.NoteEntity) map[string]any {
	return map[string]any{"id": "notif", "type": "reply", "noteId": note.ID, "note": note}
}

func TestHideNotificationNotesAt(t *testing.T) {
	viewer := &model.User{ID: "viewer"}

	t.Run("depth-2 followers renote blanked for non-follower, note kept", func(t *testing.T) {
		repo := followsRepo()
		note := tlNote("base", "noteauthor", "public", "2026-01-02T03:04:05.000Z", entity.UserLite{})
		note.Renote = followersEmbed("r", "renoteauthor")
		packed := []map[string]any{notifNoteMap(note)}
		hideNotificationNotesAt(viewer, packed, repo, heNowMs)
		if repo.calls != 1 {
			t.Fatalf("one batched follow query expected, got %d", repo.calls)
		}
		got := packed[0]["note"].(entity.NoteEntity)
		if got.Renote == nil || !got.Renote.IsHidden || got.Renote.Text != nil {
			t.Error("depth-2 followers renote must be blanked for non-follower")
		}
		if got.IsHidden {
			t.Error("the public notification note itself must stay visible")
		}
	})

	t.Run("depth-2 followers renote visible to follower", func(t *testing.T) {
		repo := followsRepo([2]string{"viewer", "renoteauthor"})
		note := tlNote("base", "noteauthor", "public", "2026-01-02T03:04:05.000Z", entity.UserLite{})
		note.Renote = followersEmbed("r", "renoteauthor")
		packed := []map[string]any{notifNoteMap(note)}
		hideNotificationNotesAt(viewer, packed, repo, heNowMs)
		got := packed[0]["note"].(entity.NoteEntity)
		if got.Renote.IsHidden {
			t.Error("follower must see the depth-2 renote")
		}
	})

	t.Run("top-level pref refinement blanks notification note (value reassigned)", func(t *testing.T) {
		repo := followsRepo()
		note := tlNote("base", "noteauthor", "public", pastWindow, entity.UserLite{MakeNotesHiddenBefore: hePtrInt(-1)})
		packed := []map[string]any{notifNoteMap(note)}
		hideNotificationNotesAt(viewer, packed, repo, heNowMs)
		got := packed[0]["note"].(entity.NoteEntity)
		if !got.IsHidden || got.Text != nil {
			t.Error("makeNotesHiddenBefore must blank the notification note and be written back into the map")
		}
	})

	t.Run("notification without note is skipped", func(t *testing.T) {
		repo := followsRepo()
		packed := []map[string]any{{"id": "n", "type": "follow"}}
		hideNotificationNotesAt(viewer, packed, repo, heNowMs) // must not panic
		if _, ok := packed[0]["note"]; ok {
			t.Error("follow notification must stay note-less")
		}
		if repo.calls != 0 {
			t.Fatalf("no note -> zero queries, got %d", repo.calls)
		}
	})
}

func TestHideNotificationNotes_NilRepoFailClosed(t *testing.T) {
	prev := followingRepo
	defer func() { followingRepo = prev }()
	followingRepo = nil
	note := tlNote("base", "noteauthor", "public", "2026-01-02T03:04:05.000Z", entity.UserLite{})
	note.Renote = followersEmbed("r", "renoteauthor")
	packed := []map[string]any{notifNoteMap(note)}
	HideNotificationNotes(&model.User{ID: "viewer"}, packed)
	got := packed[0]["note"].(entity.NoteEntity)
	if got.Renote == nil || !got.Renote.IsHidden {
		t.Error("nil followingRepo must fail closed (blank followers depth-2 embed)")
	}
}

// #2106 L5: public note が makeNotesFollowersOnlyBefore window を過ぎたら packed visibility を
// followers へ降格する (viewer 非依存)。createdAt は heNowMs より十分過去にする。
func TestHideEmbedsAt_TreatVisibilityDowngrade(t *testing.T) {
	mk := -3600
	pub := entity.NoteEntity{ID: "p", UserID: "a", CreatedAt: "2023-01-01T00:00:00.000Z",
		User: entity.UserLite{ID: "a", MakeNotesFollowersOnlyBefore: &mk}, Visibility: "public",
		Text: heStr("hi"), FileIDs: []string{}, Files: []any{}, VisibleUserIDs: []string{}, Mentions: []string{}}
	packed := []entity.NoteEntity{pub}
	hideEmbedsAt(nil, packed, followsRepo(), heNowMs)
	if packed[0].Visibility != "followers" {
		t.Errorf("public note past makeNotesFollowersOnlyBefore window should downgrade to followers, got %q", packed[0].Visibility)
	}
}

func TestHidePinnedNotesAt_AuthorPreferenceExceptionMatrix(t *testing.T) {
	hidden := 0
	requireSignin := true
	base := entity.NoteEntity{
		ID: "p", UserID: "author", CreatedAt: "2023-01-01T00:00:00.000Z",
		User:       entity.UserLite{ID: "author", MakeNotesHiddenBefore: &hidden},
		Visibility: "public", Text: heStr("published by pin"),
		FileIDs: []string{}, Files: []any{}, VisibleUserIDs: []string{}, Mentions: []string{},
	}

	t.Run("anonymous public pin bypasses time lockdown without downgrade", func(t *testing.T) {
		packed := []entity.NoteEntity{base}
		hidePinnedNotesAt(nil, packed, followsRepo(), heNowMs)
		if packed[0].IsHidden || packed[0].Text == nil || *packed[0].Text != "published by pin" {
			t.Fatal("anonymous public pin must retain content")
		}
		if packed[0].Visibility != "public" {
			t.Fatalf("pin exception must retain public visibility, got %q", packed[0].Visibility)
		}
	})

	t.Run("followers-only-before does not downgrade a qualifying pin", func(t *testing.T) {
		n := base
		followersOnly := 0
		n.User.MakeNotesFollowersOnlyBefore = &followersOnly
		packed := []entity.NoteEntity{n}
		hidePinnedNotesAt(nil, packed, followsRepo(), heNowMs)
		if packed[0].IsHidden || packed[0].Text == nil {
			t.Fatal("a qualifying public pin must bypass makeNotesFollowersOnlyBefore")
		}
		if packed[0].Visibility != "public" {
			t.Fatalf("a qualifying pin must not be downgraded, got %q", packed[0].Visibility)
		}
	})

	t.Run("anonymous sign-in-required pin remains hidden", func(t *testing.T) {
		n := base
		n.User.RequireSigninToViewContents = &requireSignin
		packed := []entity.NoteEntity{n}
		hidePinnedNotesAt(nil, packed, followsRepo(), heNowMs)
		if !packed[0].IsHidden || packed[0].Text != nil {
			t.Fatal("requireSigninToViewContents must hide an anonymous pin")
		}
	})

	t.Run("authenticated sign-in-required public pin bypasses time lockdown", func(t *testing.T) {
		n := base
		n.User.RequireSigninToViewContents = &requireSignin
		packed := []entity.NoteEntity{n}
		hidePinnedNotesAt(&model.User{ID: "viewer"}, packed, followsRepo(), heNowMs)
		if packed[0].IsHidden || packed[0].Text == nil {
			t.Fatal("authenticated viewer must receive a public pin despite author preference gates")
		}
	})

	t.Run("followers pin never gets exception", func(t *testing.T) {
		n := base
		n.Visibility = "followers"
		packed := []entity.NoteEntity{n}
		hidePinnedNotesAt(&model.User{ID: "viewer"}, packed, followsRepo(), heNowMs)
		if !packed[0].IsHidden {
			t.Fatal("followers pin must remain subject to author preference gates")
		}
	})
}
