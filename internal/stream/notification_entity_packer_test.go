package stream

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corenotification "github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// 通知の本文 (通知した利用者) と、埋め込んだ投稿の作者は、構築子で渡された
// Packer で包む (#3796)。
// 渡した Packer を使わずに nil の Packer で包むと avatarUrl が生のリモートの
// URL になって落ちる。
func TestNotificationPublisher_PacksWithTheGivenPacker(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	avatar := "https://remote.example/avatar.png"
	notifier := &model.User{ID: "alice", Username: "alice", AvatarURL: &avatar}
	text := "hi"
	note := &model.Note{ID: "9zzzzzzzzz", UserID: "alice", User: notifier, Text: &text, Visibility: model.NoteVisibilityPublic}

	p := &NotificationPublisher{}
	p.SetRepos(&stubNotifUserRepo{user: notifier}, &stubNotifNoteRepo{note: note}, idGen)
	p.entityPacker = entity.NewPacker(entity.PackerDeps{MediaURL: entity.NewMediaURLContext(
		"https://local.example", "https://local.example/proxy", []byte("secret"), false, true)})

	n := &corenotification.Notification{ID: "n1", Type: corenotification.TypeMention, NotifierID: "alice", NoteID: note.ID}
	packed, ok := p.Pack("bob", n).(map[string]any)
	require.True(t, ok)
	proxied := func(t *testing.T, got any) {
		t.Helper()
		s, _ := got.(string)
		assert.True(t, strings.HasPrefix(s, "https://local.example/proxy/avatar.webp?"), "avatarUrl: %v", got)
	}
	user, _ := packed["user"].(entity.UserLite)
	proxied(t, user.AvatarURL)
	packedNote, _ := packed["note"].(entity.NoteEntity)
	require.NotNil(t, packedNote.User, "note: %+v", packed["note"])
	proxied(t, packedNote.User.AvatarURL)
}
