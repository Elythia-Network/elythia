package entity

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestPackDriveFile_Basic(t *testing.T) {
	idGen := newTestIDGen(t)
	fileID := idGen.Generate(time.Now())
	uid := "u1"
	cmt := "comment"
	bh := "blurhash"
	thumb := "https://example.com/t.jpg"
	folderID := "fid"

	f := &model.DriveFile{
		ID:           fileID,
		UserID:       &uid,
		MD5:          "abc",
		Name:         "hello.txt",
		Type:         "text/plain",
		Size:         5,
		IsSensitive:  true,
		Blurhash:     &bh,
		Comment:      &cmt,
		URL:          "https://example.com/files/x",
		ThumbnailURL: &thumb,
		FolderID:     &folderID,
	}

	out := PackDriveFile(f, idGen)
	assert.Equal(t, fileID, out.ID)
	assert.NotEmpty(t, out.CreatedAt)
	assert.Equal(t, "hello.txt", out.Name)
	assert.Equal(t, "abc", out.MD5)
	assert.Equal(t, 5, out.Size)
	assert.True(t, out.IsSensitive)
	assert.Equal(t, &bh, out.Blurhash)
	assert.Equal(t, &cmt, out.Comment)
	assert.Equal(t, &thumb, out.ThumbnailURL)
	assert.Equal(t, &folderID, out.FolderID)
}

func TestPackDriveFile_InvalidIDLeavesCreatedAtEmpty(t *testing.T) {
	idGen := newTestIDGen(t)
	f := &model.DriveFile{ID: "not-a-valid-id"}
	out := PackDriveFile(f, idGen)
	assert.Empty(t, out.CreatedAt)
}

func TestPackDriveFolder_Basic(t *testing.T) {
	idGen := newTestIDGen(t)
	folderID := idGen.Generate(time.Now())
	parentID := "parent"

	f := &model.DriveFolder{
		ID:       folderID,
		Name:     "Test Folder",
		ParentID: &parentID,
	}
	out := PackDriveFolder(f, idGen)
	assert.Equal(t, folderID, out.ID)
	assert.NotEmpty(t, out.CreatedAt)
	assert.Equal(t, "Test Folder", out.Name)
	assert.Equal(t, &parentID, out.ParentID)
}

func TestPackDriveFolder_InvalidIDLeavesCreatedAtEmpty(t *testing.T) {
	idGen := newTestIDGen(t)
	f := &model.DriveFolder{ID: "not-a-valid-id", Name: "x"}
	out := PackDriveFolder(f, idGen)
	assert.Empty(t, out.CreatedAt)
}

func TestPackDriveFile_WithPropertiesAndWebpublic(t *testing.T) {
	idGen := newTestIDGen(t)
	fileID := idGen.Generate(time.Now())
	uid := "u1"
	wpURL := "https://example.com/wp.webp"
	wpType := "image/webp"
	props := datatypes.JSON(`{"width":800,"height":600}`)

	f := &model.DriveFile{
		ID:            fileID,
		UserID:        &uid,
		Name:          "photo.jpg",
		Type:          "image/jpeg",
		Properties:    props,
		URL:           "https://example.com/files/x",
		WebpublicURL:  &wpURL,
		WebpublicType: &wpType,
	}

	out := PackDriveFile(f, idGen)
	assert.Equal(t, &wpURL, out.WebpublicURL)
	// typed DriveFileProperties に変換されているため、width/height が
	// ポインタ int で直接触れる。
	require.NotNil(t, out.Properties.Width)
	require.NotNil(t, out.Properties.Height)
	assert.Equal(t, 800, *out.Properties.Width)
	assert.Equal(t, 600, *out.Properties.Height)
}

func TestPackDriveFile_EmptyProperties(t *testing.T) {
	idGen := newTestIDGen(t)
	fileID := idGen.Generate(time.Now())

	f := &model.DriveFile{
		ID:   fileID,
		Name: "file.txt",
		URL:  "https://example.com/files/x",
	}
	out := PackDriveFile(f, idGen)
	// properties 空 → zero-valued DriveFileProperties (全フィールド nil)
	assert.Nil(t, out.Properties.Width)
	assert.Nil(t, out.Properties.Height)
	assert.Nil(t, out.Properties.Orientation)
	assert.Nil(t, out.Properties.AvgColor)
	assert.Nil(t, out.WebpublicURL)
	// Folder / User は pack 側でセットしないので nil のまま
	assert.Nil(t, out.Folder)
	assert.Nil(t, out.User)
}

func TestPackDriveFile_AllPropertyFields(t *testing.T) {
	idGen := newTestIDGen(t)
	fileID := idGen.Generate(time.Now())
	props, err := json.Marshal(map[string]any{
		"width":       1280,
		"height":      720,
		"orientation": 8,
		"avgColor":    "rgb(40,65,87)",
	})
	require.NoError(t, err)
	f := &model.DriveFile{
		ID:         fileID,
		Name:       "photo.jpg",
		URL:        "https://example.com/p.jpg",
		Properties: datatypes.JSON(props),
	}
	// #1948-14: PackDriveFile は非 self (公開) view なので getPublicProperties を
	// 適用する。orientation=8 (>=5) なので width/height が swap され、orientation は
	// strip される (upstream getPublicProperties)。
	out := PackDriveFile(f, idGen)
	require.NotNil(t, out.Properties.Width)
	assert.Equal(t, 720, *out.Properties.Width, "orientation>=5 で width/height が swap (#1948-14)")
	require.NotNil(t, out.Properties.Height)
	assert.Equal(t, 1280, *out.Properties.Height)
	assert.Nil(t, out.Properties.Orientation, "非 self view は orientation を strip (#1948-14)")
	require.NotNil(t, out.Properties.AvgColor)
	assert.Equal(t, "rgb(40,65,87)", *out.Properties.AvgColor)

	// #1948-14: self path (PackDriveFileSelf) は raw properties を保持する。
	self := PackDriveFileSelf(f, idGen)
	assert.Equal(t, 1280, *self.Properties.Width, "self view は raw width を保持")
	assert.Equal(t, 720, *self.Properties.Height)
	require.NotNil(t, self.Properties.Orientation, "self view は orientation を保持 (#1948-14)")
	assert.Equal(t, 8, *self.Properties.Orientation)
}

// #1948-14: orientation < 5 は width/height を swap せず orientation だけ strip する。
func TestPackDriveFile_PublicPropertiesNoSwapBelowFive(t *testing.T) {
	idGen := newTestIDGen(t)
	props, _ := json.Marshal(map[string]any{"width": 1280, "height": 720, "orientation": 3})
	f := &model.DriveFile{ID: idGen.Generate(time.Now()), Name: "p.jpg", URL: "https://e/p.jpg", Properties: datatypes.JSON(props)}
	out := PackDriveFile(f, idGen)
	assert.Equal(t, 1280, *out.Properties.Width, "orientation<5 は swap しない")
	assert.Equal(t, 720, *out.Properties.Height)
	assert.Nil(t, out.Properties.Orientation, "orientation は strip される")
}

func TestPackDriveFile_InvalidPropertiesJSON_Defaults(t *testing.T) {
	idGen := newTestIDGen(t)
	f := &model.DriveFile{
		ID:         idGen.Generate(time.Now()),
		Name:       "bad.jpg",
		URL:        "x",
		Properties: datatypes.JSON([]byte("not-json")),
	}
	out := PackDriveFile(f, idGen)
	// invalid JSON → 全フィールド nil にフォールバック
	assert.Nil(t, out.Properties.Width)
	assert.Nil(t, out.Properties.Height)
}

func TestPackDriveFile_WithFolderAndUser(t *testing.T) {
	idGen := newTestIDGen(t)
	fileID := idGen.Generate(time.Now())
	folderID := idGen.Generate(time.Now())
	userID := "u1"
	f := &model.DriveFile{ID: fileID, Name: "a.jpg", URL: "x", FolderID: &folderID, UserID: &userID}
	folder := &model.DriveFolder{ID: folderID, Name: "Pictures"}
	user := &model.User{ID: userID, Username: "alice"}

	out := PackDriveFile(f, idGen)
	// caller が pack して後付けするパターン
	packedFolder := PackDriveFolder(folder, idGen)
	out.Folder = &packedFolder
	lite := PackUserLite(user)
	out.User = &lite

	require.NotNil(t, out.Folder)
	assert.Equal(t, "Pictures", out.Folder.Name)
	require.NotNil(t, out.User)
	assert.Equal(t, "alice", out.User.Username)
}

func TestPackDriveFileWithRelations_BothSet(t *testing.T) {
	idGen := newTestIDGen(t)
	fileID := idGen.Generate(time.Now())
	folderID := idGen.Generate(time.Now())
	userID := "u1"
	f := &model.DriveFile{ID: fileID, Name: "a.jpg", URL: "x", FolderID: &folderID, UserID: &userID}
	folder := &model.DriveFolder{ID: folderID, Name: "Pictures"}
	user := &model.User{ID: userID, Username: "alice"}

	out := PackDriveFileWithRelations(f, idGen, folder, user)
	require.NotNil(t, out.Folder)
	assert.Equal(t, "Pictures", out.Folder.Name)
	require.NotNil(t, out.User)
	assert.Equal(t, "alice", out.User.Username)
}

// #460: link-format remote files have no stored thumbnailUrl. The
// upstream frontend (`MkMediaImage.vue:100`) accesses
// `image.thumbnailUrl!` with a non-null assertion, so a NULL value
// breaks timeline thumbnail rendering. PackDriveFile must mirror
// upstream's getThumbnailUrl fallback chain (thumbnailUrl ??
// webpublicUrl ?? url) for image MIME types.
func TestPackDriveFile_FallbackThumbnailFromURL(t *testing.T) {
	idGen := newTestIDGen(t)
	f := &model.DriveFile{
		ID:   idGen.Generate(time.Now()),
		Name: "remote.png",
		Type: "image/png",
		URL:  "https://r/remote.png",
		// ThumbnailURL / WebpublicURL は nil (link-format で生成され
		// ていない remote attachment を想定)
	}
	out := PackDriveFile(f, idGen)
	require.NotNil(t, out.ThumbnailURL)
	assert.Equal(t, "https://r/remote.png", *out.ThumbnailURL)
}

func TestPackDriveFile_FallbackThumbnailFromWebpublic(t *testing.T) {
	idGen := newTestIDGen(t)
	web := "https://r/remote-webpublic.webp"
	f := &model.DriveFile{
		ID:           idGen.Generate(time.Now()),
		Name:         "remote.webp",
		Type:         "image/webp",
		URL:          "https://r/remote.webp",
		WebpublicURL: &web,
	}
	out := PackDriveFile(f, idGen)
	require.NotNil(t, out.ThumbnailURL)
	// webpublicUrl が優先される (upstream getThumbnailUrl と一致)
	assert.Equal(t, web, *out.ThumbnailURL)
}

func TestPackDriveFile_NoFallbackForNonImage(t *testing.T) {
	idGen := newTestIDGen(t)
	f := &model.DriveFile{
		ID:   idGen.Generate(time.Now()),
		Name: "video.mp4",
		Type: "video/mp4",
		URL:  "https://r/video.mp4",
	}
	out := PackDriveFile(f, idGen)
	// video など非 image MIME はフォールバックさせない (frontend が
	// thumbnail として動画を読み込んでしまうのを避ける)
	assert.Nil(t, out.ThumbnailURL)
}

func TestPackDriveFile_PreservesExistingThumbnailUrl(t *testing.T) {
	idGen := newTestIDGen(t)
	thumb := "https://r/explicit-thumb.png"
	f := &model.DriveFile{
		ID:           idGen.Generate(time.Now()),
		Name:         "x",
		Type:         "image/png",
		URL:          "https://r/x.png",
		ThumbnailURL: &thumb,
	}
	out := PackDriveFile(f, idGen)
	require.NotNil(t, out.ThumbnailURL)
	assert.Equal(t, thumb, *out.ThumbnailURL)
}

func TestPackDriveFileWithRelations_NilRelations(t *testing.T) {
	idGen := newTestIDGen(t)
	f := &model.DriveFile{ID: idGen.Generate(time.Now()), Name: "a", URL: "x"}
	// folder/user が nil のときは omitempty で省略される
	out := PackDriveFileWithRelations(f, idGen, nil, nil)
	assert.Nil(t, out.Folder)
	assert.Nil(t, out.User)
}
