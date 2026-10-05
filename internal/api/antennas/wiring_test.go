package antennas

import (
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
)

// router が呼ぶ optional setter は wiring 漏れが起きても静かに機能が落ちるだけ
// なので、少なくとも「呼べて値が入る」ことは押さえておく。
func TestOptionalSetters(t *testing.T) {
	h, _, _ := newHandler(t)

	h.SetUserRepo(testutil.NewMockUserRepository())
	assert.NotNil(t, h.userRepo)

	h.SetNoteFieldResolver(entity.NewNoteFieldResolver(nil, nil, nil, nil, nil, nil))
	assert.NotNil(t, h.fieldRes)

	h.SetInstanceRepo(testutil.NewMockInstanceRepository())
	assert.NotNil(t, h.instanceRepo)

	h.SetEmojiRepo(testutil.NewMockEmojiRepository())
	assert.NotNil(t, h.emojiRepo)

	// reaction reader は nil 許容 (未配線なら buffered reaction を読まない)。
	h.SetReactionReader(nil)
	assert.Nil(t, h.bufReader)
}
