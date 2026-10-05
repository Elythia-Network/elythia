package note

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

// fakeFeaturedRanking records the ranking updates issued by the renote hook.
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

func newCreateSvcWithRanking(t *testing.T, alwaysSample bool) (*CreateService, *testutil.MockNoteRepository, *fakeFeaturedRanking, id.Generator) {
	t.Helper()
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := NewCreateService(noteRepo, pollRepo, idGen, nil)
	fake := newFakeRanking()
	svc.SetFeaturedRanking(fake)
	if alwaysSample {
		svc.randFn = func() float64 { return 0 }
	} else {
		svc.randFn = func() float64 { return 1 }
	}
	return svc, noteRepo, fake, idGen
}

func TestUpdateFeaturedOnRenote_GlobalAndPerUser(t *testing.T) {
	svc, noteRepo, fake, idGen := newCreateSvcWithRanking(t, true)
	targetID := idGen.Generate(time.Now())
	noteRepo.Notes[targetID] = &model.Note{ID: targetID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	_, err := svc.Create(CreateInput{User: &model.User{ID: "renoter"}, RenoteID: &targetID})
	require.NoError(t, err)
	assert.Equal(t, []string{targetID}, fake.global)
	assert.Equal(t, []string{targetID}, fake.perUser["author"])
	assert.Empty(t, fake.inCh)
}

func TestUpdateFeaturedOnRenote_Channel(t *testing.T) {
	svc, noteRepo, fake, idGen := newCreateSvcWithRanking(t, true)
	ch := "ch1"
	targetID := idGen.Generate(time.Now())
	noteRepo.Notes[targetID] = &model.Note{ID: targetID, UserID: "author", Visibility: model.NoteVisibilityPublic, ChannelID: &ch}
	_, err := svc.Create(CreateInput{User: &model.User{ID: "renoter"}, RenoteID: &targetID, ChannelID: &ch})
	require.NoError(t, err)
	assert.Equal(t, []string{targetID}, fake.inCh["ch1"])
	assert.Empty(t, fake.global)
}

func TestUpdateFeaturedOnRenote_SkipSelf(t *testing.T) {
	svc, noteRepo, fake, idGen := newCreateSvcWithRanking(t, true)
	targetID := idGen.Generate(time.Now())
	noteRepo.Notes[targetID] = &model.Note{ID: targetID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	_, err := svc.Create(CreateInput{User: &model.User{ID: "author"}, RenoteID: &targetID})
	require.NoError(t, err)
	assert.Empty(t, fake.global)
}

func TestUpdateFeaturedOnRenote_SkipBot(t *testing.T) {
	svc, noteRepo, fake, idGen := newCreateSvcWithRanking(t, true)
	targetID := idGen.Generate(time.Now())
	noteRepo.Notes[targetID] = &model.Note{ID: targetID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	_, err := svc.Create(CreateInput{User: &model.User{ID: "renoter", IsBot: true}, RenoteID: &targetID})
	require.NoError(t, err)
	assert.Empty(t, fake.global)
}

func TestUpdateFeaturedOnRenote_SkipOld(t *testing.T) {
	svc, noteRepo, fake, idGen := newCreateSvcWithRanking(t, true)
	targetID := idGen.Generate(time.Now().Add(-4 * 24 * time.Hour))
	noteRepo.Notes[targetID] = &model.Note{ID: targetID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	_, err := svc.Create(CreateInput{User: &model.User{ID: "renoter"}, RenoteID: &targetID})
	require.NoError(t, err)
	assert.Empty(t, fake.global)
}

func TestUpdateFeaturedOnRenote_SkipNotSampled(t *testing.T) {
	svc, noteRepo, fake, idGen := newCreateSvcWithRanking(t, false)
	targetID := idGen.Generate(time.Now())
	noteRepo.Notes[targetID] = &model.Note{ID: targetID, UserID: "author", Visibility: model.NoteVisibilityPublic}
	_, err := svc.Create(CreateInput{User: &model.User{ID: "renoter"}, RenoteID: &targetID})
	require.NoError(t, err)
	assert.Empty(t, fake.global)
}
