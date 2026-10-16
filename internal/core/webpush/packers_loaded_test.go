package webpush_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/core/webpush"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
)

// PackLoadedNote is PackNoteByID for a note already read (#3611): the same
// payload and the same recipient gate.
func TestNoteRepoPacker_PackLoadedNote_MatchesPackNoteByID(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	repo := testutil.NewMockNoteRepository()
	follow := testutil.NewMockFollowingRepository()
	p := webpush.NewNoteRepoPacker(repo, idGen, follow, nil)

	public := seedNote(repo, idGen, "author", model.NoteVisibilityPublic, nil)
	followers := seedNote(repo, idGen, "author", model.NoteVisibilityFollowers, nil)
	specified := seedNote(repo, idGen, "author", model.NoteVisibilitySpecified, []string{"friend"})
	seedFollow(follow, "fan", "author")

	for _, n := range []*model.Note{public, followers, specified} {
		for _, viewer := range []string{"", "stranger", "fan", "friend", "author"} {
			want, wantOK := p.PackNoteByID(n.ID, viewer)
			got, gotOK := p.PackLoadedNote(n, viewer)
			assert.Equal(t, wantOK, gotOK, "%s/%s", n.Visibility, viewer)
			assert.Equal(t, want, got, "%s/%s", n.Visibility, viewer)
		}
	}
	_, ok := p.PackLoadedNote(nil, "viewer")
	assert.False(t, ok)
	var nilPacker *webpush.NoteRepoPacker
	_, ok = nilPacker.PackLoadedNote(public, "viewer")
	assert.False(t, ok)
}

// noPrimaryRepo hides the mock's primary reads.
type noPrimaryRepo struct{ repository.NoteRepository }

// PackNoteByIDOnPrimary reads through the primary when the repository offers
// it, and falls back to PackNoteByID's read otherwise.
func TestNoteRepoPacker_PackNoteByIDOnPrimary(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	mock := testutil.NewMockNoteRepository()
	n := seedNote(mock, idGen, "author", model.NoteVisibilityPublic, nil)

	lag := &laggingReplicaRepo{MockNoteRepository: mock, stale: map[string]*model.Note{}}
	out, ok := webpush.NewNoteRepoPacker(lag, idGen, nil, nil).PackNoteByIDOnPrimary(n.ID, "viewer")
	assert.True(t, ok)
	assert.Equal(t, n.ID, out["id"])
	assert.Equal(t, 1, lag.primaryReads)

	_, ok = webpush.NewNoteRepoPacker(lag, idGen, nil, nil).PackNoteByIDOnPrimary("missing", "viewer")
	assert.False(t, ok)

	out, ok = webpush.NewNoteRepoPacker(noPrimaryRepo{mock}, idGen, nil, nil).PackNoteByIDOnPrimary(n.ID, "viewer")
	assert.True(t, ok)
	assert.Equal(t, n.ID, out["id"])

	var nilPacker *webpush.NoteRepoPacker
	_, ok = nilPacker.PackNoteByIDOnPrimary(n.ID, "viewer")
	assert.False(t, ok)
}
