package webpush_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/webpush"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// push の payload は、NewNoteRepoPacker に渡した Packer で包む (#3790)。
//
// 作者の avatarUrl (投稿の本体) と添付ファイル (内部の resolver) の両方が、渡した
// Packer の media proxy を通ることを確かめる。
// 渡した Packer を使わずに nil の Packer で包むと生の URL になる。
func TestNoteRepoPacker_UsesTheGivenPacker(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	repo := testutil.NewMockNoteRepository()
	drive := testutil.NewMockDriveFileRepository()
	const remote = "https://remote.example"
	avatar := remote + "/avatar.png"
	n := &model.Note{
		ID: idGen.Generate(time.Now()), UserID: "u1",
		Visibility: model.NoteVisibilityPublic, FileIDs: model.StringArray{"f1"},
		User: &model.User{ID: "u1", Username: "alice", AvatarURL: &avatar},
	}
	repo.Notes[n.ID] = n
	host, uri := "remote.example", remote+"/a.png"
	drive.Files["f1"] = &model.DriveFile{
		ID: "f1", Name: "a.png", Type: "image/png", URL: uri, URI: &uri, UserHost: &host, IsLink: true,
	}
	pk := entity.NewPacker(entity.NewMediaURLContext(
		"https://local.example", "", []byte("secret"), false, true), entity.PackerDeps{})

	out, ok := webpush.NewNoteRepoPacker(repo, idGen, nil, drive, pk).PackNoteByID(n.ID, "viewer")
	require.True(t, ok)
	user, _ := out["user"].(map[string]any)
	assert.False(t, strings.HasPrefix(user["avatarUrl"].(string), remote), "avatarUrl: %v", user["avatarUrl"])
	files, _ := out["files"].([]any)
	require.Len(t, files, 1)
	f, _ := files[0].(map[string]any)
	assert.False(t, strings.HasPrefix(f["url"].(string), remote), "url: %v", f["url"])
}

// 通知の送り主 (UserLite) は、NewUserRepoPacker に渡した Packer で包む (#3794)。
func TestUserRepoPacker_UsesTheGivenPacker(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	avatar := "https://remote.example/avatar.png"
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", AvatarURL: &avatar}
	pk := entity.NewPacker(entity.NewMediaURLContext(
		"https://local.example", "https://local.example/proxy", []byte("secret"), false, true), entity.PackerDeps{})

	p := webpush.NewUserRepoPacker(repo, pk)
	assert.True(t, p.HasEntityPacker())
	assert.False(t, webpush.NewUserRepoPacker(repo, nil).HasEntityPacker())
	out, ok := p.PackUserByID("u1")
	require.True(t, ok)
	got, _ := out["avatarUrl"].(string)
	assert.True(t, strings.HasPrefix(got, "https://local.example/proxy/avatar.webp?"), "avatarUrl: %s", got)
}
