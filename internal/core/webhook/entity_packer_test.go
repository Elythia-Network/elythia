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

// webhook の payload の投稿は、構築子で渡した Packer で包む (#3790)。構築子が引数を
// 捨てる変異 (#3825) もここで落ちる。
//
// 作者の avatarUrl が渡した Packer の media proxy を通ることを、投稿とリアクションの
// 両方の hook で確かめる。
// 渡した Packer を使わずに nil の Packer で包むと生の URL になる。
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
		h := webhook.NewNoteCreateHook(svc, idGen, pk)
		h.OnNoteCreated(&model.Note{ID: "n1", UserID: "alice", Visibility: model.NoteVisibilityPublic}, author, nil, nil)
		require.Len(t, enq.userCalls, 1)
		got := avatarOf(t, enq.userCalls[0].Body)
		assert.True(t, strings.HasPrefix(got, "/avatar.webp?"), "avatarUrl must go through the proxy: %s", got)
	})
	t.Run("reaction", func(t *testing.T) {
		svc, enq, userRepo, _ := newTestService(t)
		userRepo.hooks["h1"] = &model.Webhook{ID: "h1", UserID: "alice", Active: true, On: model.StringArray{"reaction"}}
		h := webhook.NewReactionCreateHook(svc, idGen, pk)
		h.OnReactionCreated(&model.Note{ID: "n1", UserID: "alice", User: author, Visibility: model.NoteVisibilityPublic},
			&model.User{ID: "bob", Username: "bob", UsernameLower: "bob"}, "\U0001F44D")
		require.Len(t, enq.userCalls, 1)
		got := avatarOf(t, enq.userCalls[0].Body)
		assert.True(t, strings.HasPrefix(got, "/avatar.webp?"), "avatarUrl must go through the proxy: %s", got)
	})
}

// userCreated の本文 (UserLite) は、SignupHook の構築子に渡した Packer で包む
// (#3794)。構築子が引数を捨てる変異 (#3823) もここで落ちる。
func TestSignupHook_PacksWithTheGivenPacker(t *testing.T) {
	svc, enq, _, sysRepo := newTestService(t)
	sysRepo.hooks["sh1"] = &model.SystemWebhook{
		ID: "sh1", IsActive: true, On: model.StringArray{"userCreated"},
	}
	h := webhook.NewSignupHook(svc, entity.NewPacker(entity.PackerDeps{MediaURL: entity.NewMediaURLContext(
		"https://local.example", "https://local.example/proxy", []byte("secret"), false, true)}))

	avatar := "https://remote.example/avatar.png"
	h.OnUserCreated(&model.User{ID: "alice", Username: "alice", UsernameLower: "alice", AvatarURL: &avatar})
	require.Len(t, enq.systemCalls, 1)
	var env struct {
		Body struct {
			AvatarURL string `json:"avatarUrl"`
		} `json:"body"`
	}
	require.NoError(t, json.Unmarshal(enq.systemCalls[0].Body, &env), string(enq.systemCalls[0].Body))
	assert.True(t, strings.HasPrefix(env.Body.AvatarURL, "https://local.example/proxy/avatar.webp?"), "body: %s", enq.systemCalls[0].Body)
}
