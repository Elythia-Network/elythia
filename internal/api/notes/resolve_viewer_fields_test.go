package notes

import (
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resolveViewerFields の MyReaction 解決が outer / renote embed に波及すること
// を確認する (#416)。reply embed は upstream で detail:false のため myReaction を
// 持たない (#1816)。
func TestResolveViewerFields_MyReactionEmbedded(t *testing.T) {
	h, _ := newTestHandler(t)
	reactionRepo := testutil.NewMockNoteReactionRepository()
	h.SetNoteReactionRepo(reactionRepo)

	// 外側 / renote / reply それぞれに別の id を付け、全てにリアクションを登録
	reactionRepo.Reactions["outer"] = &model.NoteReaction{ID: "outer", UserID: "v1", NoteID: "n1", Reaction: "👍"}
	reactionRepo.Reactions["renote"] = &model.NoteReaction{ID: "renote", UserID: "v1", NoteID: "n2", Reaction: "❤"}
	reactionRepo.Reactions["reply"] = &model.NoteReaction{ID: "reply", UserID: "v1", NoteID: "n3", Reaction: "🎉"}

	// #1641: myReaction は reactionCount>0 の note のみ fetch するので、
	// reaction 登録済み note には ReactionCount を立てる。
	notes := []entity.NoteEntity{{
		ID:            "n1",
		ReactionCount: 1,
		Renote:        &entity.NoteEntity{ID: "n2", ReactionCount: 1},
		Reply:         &entity.NoteEntity{ID: "n3", ReactionCount: 1},
	}}
	viewer := &model.User{ID: "v1"}

	h.fieldResolver().ResolveViewerFields(notes, viewer)

	require.NotNil(t, notes[0].MyReaction)
	assert.Equal(t, "👍", *notes[0].MyReaction)
	require.NotNil(t, notes[0].Renote.MyReaction)
	assert.Equal(t, "❤", *notes[0].Renote.MyReaction)
	// reply embed (detail:false) は myReaction を持たない (#1816)。
	assert.Nil(t, notes[0].Reply.MyReaction)
}

// Channel 解決も Renote / Reply の embed に波及する。
func TestResolveViewerFields_ChannelEmbedded(t *testing.T) {
	h, _ := newTestHandler(t)
	channelRepo := testutil.NewMockChannelRepository()
	h.SetChannelRepo(channelRepo)
	channelRepo.Channels["ch1"] = &model.Channel{ID: "ch1", Name: "outer-ch"}
	channelRepo.Channels["ch2"] = &model.Channel{ID: "ch2", Name: "renote-ch"}

	ch1 := "ch1"
	ch2 := "ch2"
	notes := []entity.NoteEntity{{
		ID:        "n1",
		ChannelID: &ch1,
		Renote:    &entity.NoteEntity{ID: "n2", ChannelID: &ch2},
	}}

	h.fieldResolver().ResolveViewerFields(notes, nil)

	require.NotNil(t, notes[0].Channel)
	assert.Equal(t, "outer-ch", notes[0].Channel.Name)
	require.NotNil(t, notes[0].Renote.Channel)
	assert.Equal(t, "renote-ch", notes[0].Renote.Channel.Name)
}

// nil エンティティ / 空スライスでも panic しない。
// helper の nil safe 検証は entity package の note_field_resolver_test.go
// に移譲。ここは handler 側の wiring 経路だけ確認する。
func TestResolveViewerFields_NilSafe(t *testing.T) {
	h, _ := newTestHandler(t)
	h.fieldResolver().ResolveViewerFields(nil, nil)
}
