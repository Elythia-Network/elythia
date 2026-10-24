package reaction_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/reaction"
	"github.com/elythia-network/elythia/internal/model"
)

func strPtr(s string) *string { return &s }

// ローカル限定の絵文字は、投稿者 (renote なら renote 元の投稿者も) が
// リアクションした人と同じサーバーのときだけ使え、そうでなければ ❤ になる
// (本家 ReactionService.create の canUseLocalOnlyEmoji)。
func TestService_Create_LocalOnlyEmoji(t *testing.T) {
	tests := []struct {
		name           string
		reacterHost    *string
		emojiKey       string // mock key: name@host
		userHost       *string
		renoteUserHost *string
		quote          bool
		want           string
	}{
		{name: "local note by local reacter", emojiKey: "lo@", want: ":lo@.:"},
		{name: "remote note by local reacter", emojiKey: "lo@", userHost: strPtr("remote.example"), want: reaction.FallbackReaction},
		{name: "local quote of a remote note", emojiKey: "lo@", quote: true, renoteUserHost: strPtr("remote.example"), want: reaction.FallbackReaction},
		{name: "local quote of a local note", emojiKey: "lo@", quote: true, want: ":lo@.:"},
		{name: "remote quote of a local note", emojiKey: "lo@", quote: true, userHost: strPtr("remote.example"), want: reaction.FallbackReaction},
		{name: "remote reacter on a note of the same host", reacterHost: strPtr("remote.example"), emojiKey: "lo@remote.example", userHost: strPtr("Remote.Example"), want: ":lo@remote.example:"},
		{name: "remote reacter on a local note", reacterHost: strPtr("remote.example"), emojiKey: "lo@remote.example", want: reaction.FallbackReaction},
		{name: "remote reacter on a quote of another host", reacterHost: strPtr("remote.example"), emojiKey: "lo@remote.example", userHost: strPtr("remote.example"), quote: true, renoteUserHost: strPtr("other.example"), want: reaction.FallbackReaction},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo, reactRepo, emojiRepo, _ := newService(t)
			emojiRepo.Emojis[tc.emojiKey] = &model.Emoji{ID: "e_lo", Name: "lo", Host: tc.reacterHost, LocalOnly: true}
			n := seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
			n.UserHost = tc.userHost
			if tc.quote {
				text := "quote"
				n.Text = &text
				n.RenoteID = strPtr("n0")
				n.RenoteUserHost = tc.renoteUserHost
			}
			r, err := svc.Create(&model.User{ID: "reacter", Host: tc.reacterHost}, "n1", ":lo:")
			require.NoError(t, err)
			assert.Equal(t, tc.want, r)
			assert.Equal(t, tc.want, reactionStored(reactRepo))
		})
	}
}

// ローカル限定でない絵文字は、リモートの投稿にも使える (境界の反対側)。
func TestService_Create_NonLocalOnlyEmojiOnRemoteNote(t *testing.T) {
	svc, repo, _, emojiRepo, _ := newService(t)
	seedLocalEmoji(emojiRepo, "shared", false, nil)
	n := seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
	n.UserHost = strPtr("remote.example")
	r, err := svc.Create(&model.User{ID: "viewer"}, "n1", ":shared:")
	require.NoError(t, err)
	assert.Equal(t, ":shared@.:", r)
}
