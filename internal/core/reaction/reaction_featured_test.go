package reaction

import (
	"context"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeFeaturedRanking records the ranking updates issued by the reaction hook.
type fakeFeaturedRanking struct {
	global  []string
	inCh    map[string][]string
	perUser map[string][]string
}

func newFakeRanking() *fakeFeaturedRanking {
	return &fakeFeaturedRanking{inCh: map[string][]string{}, perUser: map[string][]string{}}
}

func (f *fakeFeaturedRanking) UpdateGlobalNotesRanking(_ context.Context, noteID string, _ float64) error {
	f.global = append(f.global, noteID)
	return nil
}

func (f *fakeFeaturedRanking) UpdateInChannelNotesRanking(_ context.Context, channelID, noteID string, _ float64) error {
	f.inCh[channelID] = append(f.inCh[channelID], noteID)
	return nil
}

func (f *fakeFeaturedRanking) UpdatePerUserNotesRanking(_ context.Context, userID, noteID string, _ float64) error {
	f.perUser[userID] = append(f.perUser[userID], noteID)
	return nil
}

// newSvcWithRanking builds a reaction Service wired to a fake ranking with a
// deterministic sampling decision (alwaysSample → randFn returns 0 < 0.3).
func newSvcWithRanking(t *testing.T, alwaysSample bool) (*Service, *testutil.MockNoteRepository, *fakeFeaturedRanking, id.Generator) {
	t.Helper()
	noteRepo := testutil.NewMockNoteRepository()
	reactRepo := testutil.NewMockNoteReactionRepository()
	emojiRepo := testutil.NewMockEmojiRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := NewService(noteRepo, reactRepo, emojiRepo, followingRepo, idGen)
	fake := newFakeRanking()
	svc.SetFeaturedRanking(fake)
	if alwaysSample {
		svc.randFn = func() float64 { return 0 }
	} else {
		svc.randFn = func() float64 { return 1 }
	}
	return svc, noteRepo, fake, idGen
}

func TestUpdateFeaturedOnReaction_GlobalAndPerUser(t *testing.T) {
	svc, noteRepo, fake, idGen := newSvcWithRanking(t, true)
	noteID := idGen.Generate(time.Now())
	noteRepo.Notes[noteID] = &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	_, err := svc.Create(&model.User{ID: "viewer"}, noteID, "")
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, fake.global)
	assert.Equal(t, []string{noteID}, fake.perUser["author"])
	assert.Empty(t, fake.inCh)
}

func TestUpdateFeaturedOnReaction_Channel(t *testing.T) {
	svc, noteRepo, fake, idGen := newSvcWithRanking(t, true)
	noteID := idGen.Generate(time.Now())
	ch := "ch1"
	noteRepo.Notes[noteID] = &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic, ChannelID: &ch}
	_, err := svc.Create(&model.User{ID: "viewer"}, noteID, "")
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, fake.inCh["ch1"])
	assert.Empty(t, fake.global)
}

func TestUpdateFeaturedOnReaction_SkipSelf(t *testing.T) {
	svc, noteRepo, fake, idGen := newSvcWithRanking(t, true)
	noteID := idGen.Generate(time.Now())
	noteRepo.Notes[noteID] = &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	_, err := svc.Create(&model.User{ID: "author"}, noteID, "")
	require.NoError(t, err)
	assert.Empty(t, fake.global)
}

func TestUpdateFeaturedOnReaction_SkipOldNote(t *testing.T) {
	svc, noteRepo, fake, idGen := newSvcWithRanking(t, true)
	noteID := idGen.Generate(time.Now().Add(-4 * 24 * time.Hour))
	noteRepo.Notes[noteID] = &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	_, err := svc.Create(&model.User{ID: "viewer"}, noteID, "")
	require.NoError(t, err)
	assert.Empty(t, fake.global)
}

func TestUpdateFeaturedOnReaction_SkipNotSampled(t *testing.T) {
	svc, noteRepo, fake, idGen := newSvcWithRanking(t, false)
	noteID := idGen.Generate(time.Now())
	noteRepo.Notes[noteID] = &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	_, err := svc.Create(&model.User{ID: "viewer"}, noteID, "")
	require.NoError(t, err)
	assert.Empty(t, fake.global)
}

// reply note は global ranking から除外される (upstream の replyId == null gate)。
func TestUpdateFeaturedOnReaction_SkipReply(t *testing.T) {
	svc, noteRepo, fake, idGen := newSvcWithRanking(t, true)
	parentID := idGen.Generate(time.Now())
	noteRepo.Notes[parentID] = &model.Note{ID: parentID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	noteID := idGen.Generate(time.Now())
	noteRepo.Notes[noteID] = &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic, ReplyID: &parentID}
	_, err := svc.Create(&model.User{ID: "viewer"}, noteID, "")
	require.NoError(t, err)
	assert.Empty(t, fake.global)
}

// ranking 未配線時は hook が no-op (panic しない)。
func TestUpdateFeaturedOnReaction_NoRankingNoop(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	reactRepo := testutil.NewMockNoteReactionRepository()
	emojiRepo := testutil.NewMockEmojiRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := NewService(noteRepo, reactRepo, emojiRepo, followingRepo, idGen)
	noteID := idGen.Generate(time.Now())
	noteRepo.Notes[noteID] = &model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	_, err := svc.Create(&model.User{ID: "viewer"}, noteID, "")
	require.NoError(t, err)
}
