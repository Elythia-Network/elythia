package note_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
)

// main stream へ流す投稿は、SetEntityPacker で渡した Packer で包む (#3790)。
//
// 返信先の投稿者の avatarUrl が、渡した Packer の media proxy を通ることを確かめる。
// (このテストの mock は作った投稿に作者を付けないので、作者ではなく返信先で見る。) この
// パッケージのテストは既定の Packer を配線しないので、既定の Packer を読むと生の
// URL になる。
func TestCreateService_PublishesWithTheGivenPacker(t *testing.T) {
	svc, noteRepo, _ := newCreateService(t)
	pub := &stubMainStreamPublisher{}
	svc.SetMainStreamPublisher(pub)
	assert.False(t, svc.HasEntityPacker())
	svc.SetEntityPacker(entity.NewPacker(entity.PackerDeps{MediaURL: entity.NewMediaURLContext(
		"https://local.example", "", []byte("secret"), false, true)}))
	require.True(t, svc.HasEntityPacker())
	avatar := "https://remote.example/avatar.png"
	noteRepo.Notes["r1"] = &model.Note{
		ID: "r1", UserID: "bob", Visibility: model.NoteVisibilityPublic,
		User: &model.User{ID: "bob", Username: "bob", AvatarURL: &avatar},
	}

	replyID, text := "r1", "hi"
	_, err := svc.Create(note.CreateInput{
		User: &model.User{ID: "alice", Username: "alice"}, Text: &text, ReplyID: &replyID,
	})
	require.NoError(t, err)

	var got string
	for _, c := range pub.calls {
		if c.eventType != "reply" {
			continue
		}
		raw, err := json.Marshal(c.body)
		require.NoError(t, err)
		var b struct {
			Reply struct {
				User struct {
					AvatarURL string `json:"avatarUrl"`
				} `json:"user"`
			} `json:"reply"`
		}
		require.NoError(t, json.Unmarshal(raw, &b), string(raw))
		got = b.Reply.User.AvatarURL
	}
	assert.True(t, strings.HasPrefix(got, "/avatar.webp?"), "avatarUrl: %q", got)
}
