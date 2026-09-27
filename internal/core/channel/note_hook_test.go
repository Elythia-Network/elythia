package channel_test

import (
	"testing"

	"github.com/shiroha-a/mk/internal/core/channel"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNoteCreateHook_EnsureChannelExists(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	hook := channel.NewNoteCreateHook(svc)
	require.NoError(t, hook.EnsureChannelExists("c1"))
}

func TestNoteCreateHook_EnsureChannelMissing(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	hook := channel.NewNoteCreateHook(svc)
	err := hook.EnsureChannelExists("missing")
	assert.ErrorIs(t, err, channel.ErrChannelNotFound)
}

// **アーカイブ済みのチャンネルには投稿させないこと。**
//
// upstream の notes/create は `isArchived: false` で引くので NO_SUCH_CHANNEL に
// なる。drafts 経路 (notes/drafts/create) は既に弾いていたが、notes/create と
// 予約投稿の公開はこの hook の存在確認しか通らず投稿できていた。
func TestNoteCreateHook_EnsureChannelArchived(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1", IsArchived: true}
	hook := channel.NewNoteCreateHook(svc)
	assert.ErrorIs(t, hook.EnsureChannelExists("c1"), channel.ErrChannelNotFound)
}

func TestNoteCreateHook_OnNotePosted(t *testing.T) {
	svc, repo, _, _ := newSvc(t)
	repo.Channels["c1"] = &model.Channel{ID: "c1"}
	hook := channel.NewNoteCreateHook(svc)
	hook.OnNotePosted("c1", "", "")
	assert.Equal(t, 1, repo.Channels["c1"].NotesCount)
}
