package notesfilter_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/core/notesfilter"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func mkNote(authorID, text, cw string) *model.Note {
	var pt, pc *string
	if text != "" {
		t := text
		pt = &t
	}
	if cw != "" {
		c := cw
		pc = &c
	}
	return &model.Note{ID: authorID + ":" + text, UserID: authorID, Text: pt, CW: pc}
}

func TestApplyHardMute_NoLookupWired(t *testing.T) {
	notes := []*model.Note{mkNote("other", "anything", "")}
	out := notesfilter.ApplyHardMute(nil, &model.User{ID: "viewer"}, notes)
	assert.Len(t, out, 1)
}

func TestApplyHardMute_AnonymousViewer(t *testing.T) {
	lookup := testutil.NewMockUserRepository()
	notes := []*model.Note{mkNote("other", "muted text", "")}
	out := notesfilter.ApplyHardMute(lookup, nil, notes)
	assert.Len(t, out, 1)
}

func TestApplyHardMute_EmptyNotes(t *testing.T) {
	lookup := testutil.NewMockUserRepository()
	lookup.Profiles["viewer"] = &model.UserProfile{
		UserID:         "viewer",
		HardMutedWords: datatypes.JSON([]byte(`["x"]`)),
	}
	out := notesfilter.ApplyHardMute(lookup, &model.User{ID: "viewer"}, nil)
	assert.Empty(t, out)
}

func TestApplyHardMute_FiltersMutedExemptsSelfAndKeepsClean(t *testing.T) {
	lookup := testutil.NewMockUserRepository()
	lookup.Profiles["viewer"] = &model.UserProfile{
		UserID:         "viewer",
		HardMutedWords: datatypes.JSON([]byte(`["politics","/spam[0-9]+/i"]`)),
	}
	viewer := &model.User{ID: "viewer"}
	notes := []*model.Note{
		mkNote("other", "today's politics rant", ""),
		mkNote("other", "harmless cat photo", ""),
		mkNote("other", "totally SPAM42 here", ""),
		mkNote("viewer", "politics by me, should not be filtered", ""),
	}
	out := notesfilter.ApplyHardMute(lookup, viewer, notes)
	require.Len(t, out, 2)
	assert.Equal(t, "harmless cat photo", *out[0].Text)
	assert.Equal(t, "viewer", out[1].UserID)
}

func TestApplyHardMute_MatchesCW(t *testing.T) {
	lookup := testutil.NewMockUserRepository()
	lookup.Profiles["viewer"] = &model.UserProfile{
		UserID:         "viewer",
		HardMutedWords: datatypes.JSON([]byte(`["spoiler"]`)),
	}
	viewer := &model.User{ID: "viewer"}
	notes := []*model.Note{mkNote("other", "innocuous body", "spoiler ahead")}
	out := notesfilter.ApplyHardMute(lookup, viewer, notes)
	assert.Empty(t, out)
}

func TestApplyHardMute_EmptyRulesIsNoOp(t *testing.T) {
	lookup := testutil.NewMockUserRepository()
	lookup.Profiles["viewer"] = &model.UserProfile{UserID: "viewer"}
	notes := []*model.Note{mkNote("other", "anything", "")}
	out := notesfilter.ApplyHardMute(lookup, &model.User{ID: "viewer"}, notes)
	assert.Len(t, out, 1)
}

func TestApplyHardMute_ProfileFetchErrorDoesNotPanic(t *testing.T) {
	lookup := testutil.NewMockUserRepository()
	// viewer の profile を入れない → mock は ErrNotFound を返す
	notes := []*model.Note{mkNote("other", "any", "")}
	out := notesfilter.ApplyHardMute(lookup, &model.User{ID: "viewer"}, notes)
	assert.Len(t, out, 1)
}

func TestMatchOne(t *testing.T) {
	rules := []byte(`["topic"]`)
	n := mkNote("u2", "talking about TOPIC today", "")
	assert.True(t, notesfilter.MatchOne(rules, "u1", n))

	// viewer 自身の note は match しない
	own := mkNote("u1", "topic by self", "")
	assert.False(t, notesfilter.MatchOne(rules, "u1", own))

	// 空 rule / nil note は false
	assert.False(t, notesfilter.MatchOne(nil, "u1", n))
	assert.False(t, notesfilter.MatchOne(rules, "u1", nil))
}
