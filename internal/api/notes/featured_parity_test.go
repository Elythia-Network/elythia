package notes

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// featuredParityCase is one candidate note and whether upstream
// notes/featured.ts (2026.10.0) keeps it for the logged-in viewer (keep) and
// for a logged-out one (anonKeep). notCandidate marks notes upstream never
// puts into the global ranking (channel, remote, reply, pure renote), which
// the global SQL fallback must not return either.
type featuredParityCase struct {
	name         string
	note         *model.Note
	keep         bool
	anonKeep     bool
	notCandidate bool
}

// expectedFeatured returns the sorted ids a path should return.
func expectedFeatured(cases []featuredParityCase, ranking, anonymous bool) []string {
	out := []string{}
	for _, c := range cases {
		kept := c.keep
		if anonymous {
			kept = c.anonKeep
		}
		if kept && (ranking || !c.notCandidate) {
			out = append(out, c.note.ID)
		}
	}
	sort.Strings(out)
	return out
}

// featuredParityFixture builds a handler whose viewer mutes "muted", the
// channel "mch" and the instance "remote.example", is blocked by "blocker",
// and whose server blocks "blocked.example". "sus" is suspended.
func featuredParityFixture(t *testing.T) (*Handler, *testutil.MockNoteRepository, []featuredParityCase) {
	t.Helper()
	h, noteRepo, _ := newExtraHandler(t)

	mutingRepo := testutil.NewMockMutingRepository()
	mutingRepo.Mutings["m1"] = &model.Muting{ID: "m1", MuterID: "viewer", MuteeID: "muted"}
	blockingRepo := testutil.NewMockBlockingRepository()
	blockingRepo.Blockings["b1"] = &model.Blocking{ID: "b1", BlockerID: "blocker", BlockeeID: "viewer"}
	channelMutingRepo := testutil.NewMockChannelMutingRepository()
	channelMutingRepo.Mutings["viewer:mch"] = &model.ChannelMuting{ID: "cm1", UserID: "viewer", ChannelID: "mch"}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["viewer"] = &model.User{ID: "viewer", Username: "viewer", UsernameLower: "viewer"}
	userRepo.Profiles["viewer"] = &model.UserProfile{UserID: "viewer", MutedInstances: datatypes.JSON(`["remote.example"]`)}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{BlockedHosts: []string{"blocked.example"}}
	h.mutingRepo = mutingRepo
	h.blockingRepo = blockingRepo
	h.channelMutingRepo = channelMutingRepo
	h.SetUserRepo(userRepo)
	h.metaRepo = metaRepo

	s := func(v string) *string { return &v }
	user := func(id string) *model.User { return &model.User{ID: id} }
	// 2 段目の検査に使う、ランキングに入らないノート (home なので SQL の
	// fallback にも出ない)。
	hidden := func(id, author string, replyUser, renoteUser *string) *model.Note {
		n := &model.Note{ID: id, UserID: author, Visibility: "home", User: user(author), ReplyUserID: replyUser, RenoteUserID: renoteUser}
		noteRepo.Notes[id] = n
		return n
	}
	xReplyMuted := hidden("x1", "u2", s("muted"), nil)
	xRenoteBlocker := hidden("x2", "u2", nil, s("blocker"))
	xBySuspended := hidden("x3", "sus", nil, nil)
	xBySuspended.User.IsSuspended = true

	// リノート系は本文を持たせて引用にし、SQL の fallback の候補に入るようにする
	// (pure renote は候補に入らない)。
	q := s("quote")
	cases := []featuredParityCase{
		{name: "muted channel is not applied", keep: true, anonKeep: true, notCandidate: true,
			note: &model.Note{ID: "k1", UserID: "u", ChannelID: s("mch")}},
		{name: "muted renote channel is not applied", keep: true, anonKeep: true,
			note: &model.Note{ID: "k2", UserID: "u", Text: q, RenoteID: s("x0"), RenoteUserID: s("u3"), RenoteChannelID: s("mch")}},
		{name: "muted instance is not applied", keep: true, anonKeep: true, notCandidate: true,
			note: &model.Note{ID: "k3", UserID: "r", UserHost: s("remote.example")}},
		{name: "renote of a reply to a muted user is kept (depth 2)", keep: true, anonKeep: true,
			note: &model.Note{ID: "k4", UserID: "u", Text: q, RenoteID: s("x1"), RenoteUserID: s("u2"), Renote: xReplyMuted}},
		{name: "renote of a renote of a blocker is kept (depth 2)", keep: true, anonKeep: true,
			note: &model.Note{ID: "k5", UserID: "u", Text: q, RenoteID: s("x2"), RenoteUserID: s("u2"), Renote: xRenoteBlocker}},
		{name: "pure renote is kept but not a fallback candidate", keep: true, anonKeep: true, notCandidate: true,
			note: &model.Note{ID: "k6", UserID: "u", RenoteID: s("x0"), RenoteUserID: s("u3")}},
		{name: "plain reply is kept but not a fallback candidate", keep: true, anonKeep: true, notCandidate: true,
			note: &model.Note{ID: "k7", UserID: "u", Text: q, ReplyID: s("x0"), ReplyUserID: s("u3")}},
		{name: "plain local note is kept", keep: true, anonKeep: true,
			note: &model.Note{ID: "k8", UserID: "u"}},
		{name: "quote with only a CW is a fallback candidate", keep: true, anonKeep: true,
			note: &model.Note{ID: "k9", UserID: "u", CW: s("cw"), RenoteID: s("x0"), RenoteUserID: s("u3")}},
		{name: "muted author is dropped", anonKeep: true,
			note: &model.Note{ID: "d1", UserID: "muted"}},
		{name: "reply to a muted user is dropped", anonKeep: true, notCandidate: true,
			note: &model.Note{ID: "d2", UserID: "u", ReplyID: s("y"), ReplyUserID: s("muted")}},
		{name: "author blocking the viewer is dropped", anonKeep: true,
			note: &model.Note{ID: "d3", UserID: "blocker"}},
		{name: "renote of a blocker is dropped", anonKeep: true,
			note: &model.Note{ID: "d4", UserID: "u", Text: q, RenoteID: s("y"), RenoteUserID: s("blocker")}},
		{name: "reply to a suspended user is dropped", notCandidate: true,
			note: &model.Note{ID: "d5", UserID: "u", ReplyID: s("x3"), ReplyUserID: s("sus"), Reply: xBySuspended}},
		{name: "renote of a suspended user is dropped",
			note: &model.Note{ID: "d6", UserID: "u", Text: q, RenoteID: s("x3"), RenoteUserID: s("sus"), Renote: xBySuspended}},
		{name: "blocked host is dropped", notCandidate: true,
			note: &model.Note{ID: "d7", UserID: "bh", UserHost: s("blocked.example")}},
		{name: "reply to a blocked host is dropped", notCandidate: true,
			note: &model.Note{ID: "d8", UserID: "u", ReplyID: s("y"), ReplyUserID: s("bh"), ReplyUserHost: s("blocked.example")}},
		{name: "renote of a blocked host is dropped",
			note: &model.Note{ID: "d9", UserID: "u", Text: q, RenoteID: s("y"), RenoteUserID: s("bh"), RenoteUserHost: s("sub.blocked.example")}},
		{name: "suspended author is dropped",
			note: &model.Note{ID: "da", UserID: "sus"}},
	}
	for _, c := range cases {
		c.note.Visibility = "public"
		c.note.User = user(c.note.UserID)
		c.note.User.IsSuspended = c.note.UserID == "sus"
		noteRepo.Notes[c.note.ID] = c.note
	}
	return h, noteRepo, cases
}

// TestFeatured_FiltersLikeUpstream pins the exclusions of notes/featured to
// upstream featured.ts (2026.10.0) on both the ranking path and the SQL
// fallback path: depth-1 mute / block via isUserRelated (author included),
// blocked hosts and suspended authors are dropped; channel mutes, instance
// mutes and depth-2 targets are not looked at. The global SQL fallback only
// returns notes upstream could rank globally (#3369).
func TestFeatured_FiltersLikeUpstream(t *testing.T) {
	for _, ranking := range []bool{true, false} {
		name := "sql fallback"
		if ranking {
			name = "ranking"
		}
		t.Run(name, func(t *testing.T) {
			h, _, cases := featuredParityFixture(t)
			if ranking {
				ids := make([]string, 0, len(cases))
				for _, c := range cases {
					ids = append(ids, c.note.ID)
				}
				h.SetFeaturedRanking(&fakeFeaturedReader{global: ids})
			}
			notes, err := h.featuredNotes(context.Background(), &model.User{ID: "viewer"}, "", "", 100, 0, false)
			require.NoError(t, err)
			got := make(map[string]bool, len(notes))
			for _, n := range notes {
				got[n.ID] = true
			}
			for _, c := range cases {
				assert.Equal(t, c.keep && (ranking || !c.notCandidate), got[c.note.ID], c.name)
			}
			// fixture 以外 (2 段目のノート) が混ざっていないこと。
			ids := featuredTestIDs(notes)
			sort.Strings(ids)
			assert.Equal(t, expectedFeatured(cases, ranking, false), ids)
		})
	}
}

// featuredPaths runs fn on the ranking path and on the SQL fallback path.
func featuredPaths(t *testing.T, fn func(t *testing.T, ranking bool)) {
	t.Helper()
	for _, ranking := range []bool{true, false} {
		name := "sql fallback"
		if ranking {
			name = "ranking"
		}
		t.Run(name, func(t *testing.T) { fn(t, ranking) })
	}
}

// setFixtureRanking puts every fixture candidate into the global ranking.
func setFixtureRanking(h *Handler, cases []featuredParityCase) {
	ids := make([]string, 0, len(cases))
	for _, c := range cases {
		ids = append(ids, c.note.ID)
	}
	h.SetFeaturedRanking(&fakeFeaturedReader{global: ids})
}

// TestFeatured_AnonymousStillDropsSuspendedAndBlockedHosts pins that a
// logged-out viewer gets suspended authors (author / reply / renote) and
// admin-blocked hosts (author / reply / renote) dropped, as upstream applies
// generateSuspendedUserQueryForNote and generateBlockedHostQueryForNote
// regardless of me. Mute / block are viewer-scoped and do not apply (#3369).
func TestFeatured_AnonymousStillDropsSuspendedAndBlockedHosts(t *testing.T) {
	featuredPaths(t, func(t *testing.T, ranking bool) {
		h, _, cases := featuredParityFixture(t)
		if ranking {
			setFixtureRanking(h, cases)
		}
		notes, err := h.featuredNotes(context.Background(), nil, "", "", 100, 0, false)
		require.NoError(t, err)
		ids := featuredTestIDs(notes)
		sort.Strings(ids)
		assert.Equal(t, expectedFeatured(cases, ranking, true), ids)
	})
}

type failingMutingRepo struct{ *testutil.MockMutingRepository }

func (failingMutingRepo) ListMuteeIDs(string) ([]string, error) {
	return nil, errors.New("muting lookup failed")
}

// TestFeatured_LookupErrorsFailClosed pins that notes/featured answers 500
// without notes when the mute, block, or blocked-host lookup fails, instead
// of serving the unfiltered candidates.
func TestFeatured_LookupErrorsFailClosed(t *testing.T) {
	cases := []struct {
		name   string
		viewer *model.User
		breakH func(h *Handler)
	}{
		{"muting", &model.User{ID: "viewer"}, func(h *Handler) {
			h.mutingRepo = failingMutingRepo{testutil.NewMockMutingRepository()}
		}},
		{"blocking", &model.User{ID: "viewer"}, func(h *Handler) {
			b := testutil.NewMockBlockingRepository()
			b.ExistsErr = errors.New("blocking lookup failed")
			h.blockingRepo = b
		}},
		{"blocked hosts", &model.User{ID: "viewer"}, func(h *Handler) {
			m := testutil.NewMockMetaRepository()
			m.FetchErr = errors.New("meta lookup failed")
			h.metaRepo = m
		}},
		{"blocked hosts anonymous", nil, func(h *Handler) {
			m := testutil.NewMockMetaRepository()
			m.FetchErr = errors.New("meta lookup failed")
			h.metaRepo = m
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			featuredPaths(t, func(t *testing.T, ranking bool) {
				h, _, fixture := featuredParityFixture(t)
				if ranking {
					setFixtureRanking(h, fixture)
				}
				c.breakH(h)
				rec := postExtra(h.Featured, `{"limit":100}`, c.viewer)
				assert.Equal(t, http.StatusInternalServerError, rec.Code)
				body := rec.Body.String()
				for _, f := range fixture {
					assert.False(t, strings.Contains(body, `"`+f.note.ID+`"`), "500 must not carry note %s", f.note.ID)
				}
			})
		})
	}
}
