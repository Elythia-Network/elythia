package reaction_test

import (
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/core/reaction"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `note_reaction.reaction` は varchar(260)。**絵文字だけで構成された長い文字列**
// は「非絵文字なら ❤」の gate を通ってしまうので、列に収まるかを別に見る。
// 溢れると Create が 22001 で落ち、Like 配送が retry を使い切って dead になる
// (#2726)。
func TestService_Create_OversizedUnicodeReactionFallsBack(t *testing.T) {
	svc, repo, reactRepo, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)

	got, err := svc.Create(&model.User{ID: "u"}, "n1", strings.Repeat("😀", 300))
	require.NoError(t, err)
	assert.Equal(t, reaction.FallbackReaction, got)
	require.Len(t, reactRepo.Reactions, 1)
	for _, r := range reactRepo.Reactions {
		assert.Equal(t, reaction.FallbackReaction, r.Reaction)
	}
}

// 上限ちょうどは通す (境界を締めすぎていないこと)。
// 😀 は 1 rune (U+1F600) なので 260 個で 260 code point。
func TestService_Create_MaxLengthUnicodeReactionStored(t *testing.T) {
	svc, repo, reactRepo, _, _ := newService(t)
	seedNote(repo, "n1", "author", model.NoteVisibilityPublic)

	want := strings.Repeat("😀", 260)
	require.Equal(t, 260, len([]rune(want)))
	got, err := svc.Create(&model.User{ID: "u"}, "n1", want)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	require.Len(t, reactRepo.Reactions, 1)
}
