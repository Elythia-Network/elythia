package stream

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// ストリームへ流す投稿は、SetEntityPacker で渡した Packer で包む (#3790)。
//
// 作者の avatarUrl (投稿の本体) と添付ファイル (resolver) の両方が、渡した Packer の
// media proxy を通ることを確かめる。このパッケージのテストは既定の Packer を
// 配線しないので、既定の Packer を読むと生の URL になる。
func TestNotePublisher_PacksWithTheGivenPacker(t *testing.T) {
	pub := &stubPublisher{}
	idGen, _ := id.NewGenerator("aidx")
	np := NewNotePublisher(pub, idGen)
	pk := entity.NewPacker(entity.PackerDeps{MediaURL: entity.NewMediaURLContext(
		"https://local.example", "", []byte("secret"), false, true)})
	np.SetEntityPacker(pk)
	require.True(t, np.HasEntityPacker())

	const remote = "https://remote.example"
	driveRepo := testutil.NewMockDriveFileRepository()
	fileID := idGen.Generate(time.Now())
	host, uri := "remote.example", remote+"/a.png"
	driveRepo.Files[fileID] = &model.DriveFile{
		ID: fileID, Name: "a.png", Type: "image/png", URL: uri, URI: &uri, UserHost: &host, IsLink: true,
	}
	np.SetFieldResolver(entity.NewNoteFieldResolver(pk, driveRepo, nil, nil, nil, nil, idGen))

	avatar := remote + "/avatar.png"
	n := &model.Note{ID: idGen.Generate(time.Now()), UserID: "u1", FileIDs: []string{fileID}, Visibility: model.NoteVisibilityPublic}
	np.PublishNote("homeTimeline:u1", n, &model.User{ID: "u1", Username: "alice", AvatarURL: &avatar})

	require.Len(t, pub.payloads, 1)
	var body struct {
		User struct {
			AvatarURL string `json:"avatarUrl"`
		} `json:"user"`
		Files []struct {
			URL string `json:"url"`
		} `json:"files"`
	}
	require.NoError(t, json.Unmarshal(pub.payloads[0].(json.RawMessage), &body))
	assert.True(t, strings.HasPrefix(body.User.AvatarURL, "/avatar.webp?"), "avatarUrl: %s", body.User.AvatarURL)
	require.Len(t, body.Files, 1)
	assert.True(t, strings.HasPrefix(body.Files[0].URL, "/image.webp?"), "url: %s", body.Files[0].URL)
}
