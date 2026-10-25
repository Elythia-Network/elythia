package notes

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestFeatured_DBFiltersLikeUpstream runs notes/featured on the real
// repositories and the relation cache, on both the ranking path and the SQL
// fallback path. The SQL fallback hydrates the reply / renote embeds and
// their authors, which the suspended-target and depth-2 cases read, and
// returns only notes upstream could put into the global ranking: no channel,
// remote, reply, or pure-renote notes (#3369).
func TestFeatured_DBFiltersLikeUpstream(t *testing.T) {
	db := openRelationDB(t)
	const p = "nfp3369_"
	cleanup := func() {
		db.Exec(`DELETE FROM "note" WHERE "userId" LIKE ?`, p+"%")
		db.Exec(`DELETE FROM "muting" WHERE id LIKE ?`, p+"%")
		db.Exec(`DELETE FROM "blocking" WHERE id LIKE ?`, p+"%")
		db.Exec(`DELETE FROM "channel_muting" WHERE id LIKE ?`, p+"%")
		db.Exec(`DELETE FROM "channel" WHERE id LIKE ?`, p+"%")
		db.Exec(`DELETE FROM "user_profile" WHERE "userId" LIKE ?`, p+"%")
		db.Exec(`DELETE FROM "user" WHERE id LIKE ?`, p+"%")
	}
	cleanup()
	t.Cleanup(cleanup)

	s := func(v string) *string { return &v }
	mkUser := func(name string, host *string, suspended bool) string {
		uid := p + name
		require.NoError(t, db.Create(&model.User{ID: uid, Username: uid, UsernameLower: uid, Host: host, IsSuspended: suspended, AvatarDecorations: datatypes.JSON([]byte("[]"))}).Error)
		return uid
	}
	viewer := mkUser("v", nil, false)
	u := mkUser("u", nil, false)
	u2 := mkUser("u2", nil, false)
	muted := mkUser("m", nil, false)
	blocker := mkUser("b", nil, false)
	sus := mkUser("s", nil, true)
	remote := mkUser("r", s("remote.example"), false)
	require.NoError(t, db.Create(&model.UserProfile{UserID: viewer, Fields: datatypes.JSON([]byte("[]")), MutedInstances: datatypes.JSON([]byte(`["remote.example"]`))}).Error)
	ch := p + "ch"
	require.NoError(t, db.Exec(`INSERT INTO "channel" (id, name, "userId") VALUES (?, ?, ?)`, ch, ch, u).Error)
	require.NoError(t, db.Create(&model.Muting{ID: p + "mu", MuterID: viewer, MuteeID: muted}).Error)
	require.NoError(t, db.Create(&model.Blocking{ID: p + "bl", BlockerID: blocker, BlockeeID: viewer}).Error)
	require.NoError(t, db.Create(&model.ChannelMuting{ID: p + "cm", UserID: viewer, ChannelID: ch}).Error)

	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	base := time.Now().Add(-time.Hour)
	seq := 0
	ids := map[string]string{}
	// 候補は renoteCount を大きくして、SQL の fallback で他のテストの行より
	// 上に来るようにする。候補でないノートは home にして fallback に出さない。
	mk := func(key, author string, candidate bool, mod func(n *model.Note)) string {
		seq++
		n := &model.Note{ID: idGen.Generate(base.Add(time.Duration(seq) * time.Second)), UserID: author, Visibility: "home"}
		if candidate {
			n.Visibility = "public"
			n.RenoteCount = 30000
		}
		if mod != nil {
			mod(n)
		}
		require.NoError(t, db.Create(n).Error)
		ids[n.ID] = key
		return n.ID
	}
	byMuted := mk("byMuted", muted, false, nil)
	byBlocker := mk("byBlocker", blocker, false, nil)
	bySus := mk("bySus", sus, false, nil)
	replyToMuted := mk("replyToMuted", u2, false, func(n *model.Note) { n.ReplyID, n.ReplyUserID = &byMuted, &muted })
	renoteOfBlocker := mk("renoteOfBlocker", u2, false, func(n *model.Note) { n.RenoteID, n.RenoteUserID = &byBlocker, &blocker })

	// リノート系は本文を持たせて引用にし、全体の fallback の候補に入るようにする。
	// notCandidate は本家が全体のランキングへ入れないノート (チャンネル・リモート・
	// 返信・pure renote) で、全体の fallback も返さない。
	quote := func(target, targetUser *string) func(n *model.Note) {
		return func(n *model.Note) { n.Text, n.RenoteID, n.RenoteUserID = s("quote"), target, targetUser }
	}
	plain := mk("plainTarget", u2, false, nil)
	notCandidate := map[string]bool{}
	nc := func(id string) string { notCandidate[id] = true; return id }
	keep := []string{
		nc(mk("inMutedChannel", u, true, func(n *model.Note) { n.ChannelID = &ch })),
		nc(mk("byMutedInstance", remote, true, func(n *model.Note) { n.UserHost = s("remote.example") })),
		mk("depth2Muted", u, true, quote(&replyToMuted, &u2)),
		mk("depth2Blocker", u, true, quote(&renoteOfBlocker, &u2)),
		nc(mk("pureRenote", u, true, func(n *model.Note) { n.RenoteID, n.RenoteUserID = &plain, &u2 })),
		nc(mk("plainReply", u, true, func(n *model.Note) { n.Text, n.ReplyID, n.ReplyUserID = s("re"), &plain, &u2 })),
		mk("plainLocal", u, true, nil),
	}
	drop := []string{
		mk("authorMuted", muted, true, nil),
		nc(mk("replyMuted", u, true, func(n *model.Note) { n.ReplyID, n.ReplyUserID = &byMuted, &muted })),
		mk("authorBlocker", blocker, true, nil),
		mk("renoteBlocker", u, true, quote(&byBlocker, &blocker)),
		nc(mk("replySuspended", u, true, func(n *model.Note) { n.ReplyID, n.ReplyUserID = &bySus, &sus })),
		mk("renoteSuspended", u, true, quote(&bySus, &sus)),
		mk("authorSuspended", sus, true, nil),
	}
	candidates := append(append([]string{}, keep...), drop...)

	rc := repository.NewRelationCacheWithTTL(db, time.Hour)
	newHandler := func() *Handler {
		h := newHandlerNilDeps(repository.NewNoteRepository(db), nil, nil, nil, nil, nil, nil, nil, idGen)
		h.SetUserRepo(repository.NewUserRepositoryWithRelationCache(db, rc))
		h.SetMutingRepo(repository.NewMutingRepositoryWithRelationCache(db, rc))
		h.SetBlockingRepo(repository.NewBlockingRepositoryWithRelationCache(db, rc))
		h.SetChannelMutingRepo(repository.NewChannelMutingRepositoryWithRelationCache(db, rc))
		h.SetRelationCache(rc)
		return h
	}
	names := func(notes []*model.Note) []string {
		out := []string{}
		for _, n := range notes {
			if key, ok := ids[n.ID]; ok {
				out = append(out, key)
			} else if strings.HasPrefix(n.UserID, p) {
				out = append(out, n.ID)
			}
		}
		sort.Strings(out)
		return out
	}
	want := func(ranking bool) []string {
		out := []string{}
		for _, k := range keep {
			if ranking || !notCandidate[k] {
				out = append(out, ids[k])
			}
		}
		sort.Strings(out)
		return out
	}

	for _, ranking := range []bool{true, false} {
		name := "sql fallback"
		if ranking {
			name = "ranking"
		}
		t.Run(name, func(t *testing.T) {
			h := newHandler()
			if ranking {
				h.SetFeaturedRanking(&fakeFeaturedReader{global: candidates})
			}
			notes, err := h.featuredNotes(context.Background(), &model.User{ID: viewer}, "", "", 100, 0, false)
			require.NoError(t, err)
			assert.Equal(t, want(ranking), names(notes))
		})
	}
}
