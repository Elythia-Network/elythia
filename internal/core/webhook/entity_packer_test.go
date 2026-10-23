package webhook_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/webhook"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// webhook の payload の投稿は、SetEntityPacker で渡した Packer で包む (#3790)。
//
// 作者の avatarUrl が渡した Packer の media proxy を通ることを、投稿とリアクションの
// 両方の hook で確かめる。このパッケージのテストは既定の Packer を配線しないので、
// 既定の Packer を読むと生の URL になる。
func TestHooks_PackWithTheGivenPacker(t *testing.T) {
	const remote = "https://remote.example"
	avatar := remote + "/avatar.png"
	author := &model.User{ID: "alice", Username: "alice", UsernameLower: "alice", AvatarURL: &avatar}
	pk := entity.NewPacker(entity.PackerDeps{MediaURL: entity.NewMediaURLContext(
		"https://local.example", "", []byte("secret"), false, true)})
	idGen, _ := id.NewGenerator("aidx")

	avatarOf := func(t *testing.T, body json.RawMessage) string {
		t.Helper()
		var b struct {
			Body struct {
				Note struct {
					User struct {
						AvatarURL string `json:"avatarUrl"`
					} `json:"user"`
				} `json:"note"`
			} `json:"body"`
		}
		require.NoError(t, json.Unmarshal(body, &b), string(body))
		return b.Body.Note.User.AvatarURL
	}

	t.Run("note", func(t *testing.T) {
		svc, enq, userRepo, _ := newTestService(t)
		userRepo.hooks["h1"] = &model.Webhook{ID: "h1", UserID: "alice", Active: true, On: model.StringArray{"note"}}
		h := webhook.NewNoteCreateHook(svc, idGen)
		h.SetEntityPacker(pk)
		h.OnNoteCreated(&model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic}, author, nil, nil)
		require.Len(t, enq.userCalls, 1)
		got := avatarOf(t, enq.userCalls[0].Body)
		assert.True(t, strings.HasPrefix(got, "/avatar.webp?"), "avatarUrl must go through the proxy: %s", got)
	})
	t.Run("reaction", func(t *testing.T) {
		svc, enq, userRepo, _ := newTestService(t)
		userRepo.hooks["h1"] = &model.Webhook{ID: "h1", UserID: "alice", Active: true, On: model.StringArray{"reaction"}}
		h := webhook.NewReactionCreateHook(svc, idGen)
		h.SetEntityPacker(pk)
		h.OnReactionCreated(&model.Note{ID: "n1", UserID: "alice", User: author, Visibility: model.NoteVisibilityPublic},
			&model.User{ID: "bob", Username: "bob", UsernameLower: "bob"}, "\U0001F44D")
		require.Len(t, enq.userCalls, 1)
		got := avatarOf(t, enq.userCalls[0].Body)
		assert.True(t, strings.HasPrefix(got, "/avatar.webp?"), "avatarUrl must go through the proxy: %s", got)
	})
	t.Run("HasEntityPacker", func(t *testing.T) {
		n := webhook.NewNoteCreateHook(nil, idGen)
		r := webhook.NewReactionCreateHook(nil, idGen)
		assert.False(t, n.HasEntityPacker())
		assert.False(t, r.HasEntityPacker())
		n.SetEntityPacker(pk)
		r.SetEntityPacker(pk)
		assert.True(t, n.HasEntityPacker())
		assert.True(t, r.HasEntityPacker())
	})
}
