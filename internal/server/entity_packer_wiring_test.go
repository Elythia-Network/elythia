package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
)

// **投稿を包む handler は、組み立てで渡された Packer でリモートのファイルを
// media proxy へ通す (#3790)。**
//
// 以前は既定の Packer (パッケージの変数) を読んでいた。組み立てで Packer を
// 渡す形にしたので、handler か NoteFieldResolver が既定の Packer を読むように
// 戻ると、既定の Packer を配線しない組み立て (段階 (5-10) 以降) では、閲覧者の
// ブラウザがリモートのファイルを直接取りに行く。ここでは組み立ての後に既定の
// Packer の mediaURL を外し、渡した Packer だけで proxy が付くことを確かめる。
//
// 見る経路は notes/show (notes の handler と、その handler が作る resolver) と
// users/notes (users の handler と、router が作る共有の resolver)。どちらも
// 作者の avatarUrl (投稿の本体を包む側) と、引用した投稿の添付 (resolver の側) まで見る。
func TestNewServer_NotePackersUseTheWiredPacker(t *testing.T) {
	db := serverIntegrationDB
	if db == nil {
		t.Skip("PostgreSQL unavailable")
	}
	const (
		userID = "eppuser3790aaaaa"
		token  = "epptokenx3790aaa"
		host   = "remote.example"
	)
	idGen := testIDGen(t)
	fileID := idGen.Generate(time.Now())
	innerID := idGen.Generate(time.Now())
	quoteID := idGen.Generate(time.Now())
	rawURL := "https://" + host + "/files/epp3790.png"
	cleanupPluginAccountUsers(t, db, "epp_user")
	t.Cleanup(func() {
		db.Exec(`DELETE FROM "note" WHERE "id" IN ?`, []string{quoteID, innerID})
		db.Exec(`DELETE FROM "drive_file" WHERE "id" = ?`, fileID)
	})
	createLocalUser(t, db, userID, "epp_user", token, "epp-pass", false)
	// 投稿の本体を包む側 (PackNotes) も見るため、作者の avatarUrl を別オリジンにする。
	avatarURL := "https://" + host + "/files/epp3790-avatar.png"
	require.NoError(t, db.Exec(`UPDATE "user" SET "avatarUrl" = ? WHERE "id" = ?`, avatarURL, userID).Error)
	require.NoError(t, db.Create(&model.DriveFile{
		ID: fileID, UserID: strPtrLeak(userID), UserHost: strPtrLeak(host), MD5: "d41d8cd98f00b204e9800998ecf8427e",
		Name: "epp3790.png", Type: "image/png", URL: rawURL, URI: strPtrLeak(rawURL), IsLink: true,
	}).Error)
	inner := "with a remote file"
	require.NoError(t, db.Create(&model.Note{
		ID: innerID, UserID: userID, Visibility: model.NoteVisibility("public"), Text: &inner,
		FileIDs: model.StringArray{fileID},
	}).Error)
	quote := "quoting it"
	require.NoError(t, db.Create(&model.Note{
		ID: quoteID, UserID: userID, Visibility: model.NoteVisibility("public"), Text: &quote,
		RenoteID: strPtrLeak(innerID), FileIDs: model.StringArray{fileID},
	}).Error)

	srv := newServerWithConfig(t, func(*config.Config) {})
	// 既定の Packer の mediaURL を外す。restoreProcessGlobals が後で nil に戻すので、
	// ここで外した分を戻す必要は無い。
	entity.SetMediaURLContext(nil)

	assertProxied := func(t *testing.T, files []map[string]any) {
		t.Helper()
		require.Len(t, files, 1)
		url, _ := files[0]["url"].(string)
		assert.NotEqual(t, rawURL, url, "the remote file must not be returned raw")
		assert.False(t, strings.HasPrefix(url, "https://"+host), "url: %s", url)
	}
	type packedNote struct {
		User struct {
			AvatarURL string `json:"avatarUrl"`
		} `json:"user"`
		Files  []map[string]any `json:"files"`
		Renote *struct {
			Files []map[string]any `json:"files"`
		} `json:"renote"`
	}

	t.Run("notes/show", func(t *testing.T) {
		rec := postJSON(t, srv, "/api/notes/show", token, map[string]any{"noteId": quoteID})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var n packedNote
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &n), rec.Body.String())
		assert.False(t, strings.HasPrefix(n.User.AvatarURL, "https://"+host), "avatarUrl: %s", n.User.AvatarURL)
		assertProxied(t, n.Files)
		require.NotNil(t, n.Renote, rec.Body.String())
		assertProxied(t, n.Renote.Files)
	})
	t.Run("users/notes", func(t *testing.T) {
		rec := postJSON(t, srv, "/api/users/notes", token, map[string]any{"userId": userID, "limit": 1})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var ns []packedNote
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ns), rec.Body.String())
		require.Len(t, ns, 1, rec.Body.String())
		assert.False(t, strings.HasPrefix(ns[0].User.AvatarURL, "https://"+host), "avatarUrl: %s", ns[0].User.AvatarURL)
		assertProxied(t, ns[0].Files)
		require.NotNil(t, ns[0].Renote, rec.Body.String())
		assertProxied(t, ns[0].Renote.Files)
	})
}

// **利用者の詳細を包む部品も、組み立てで渡された Packer で avatarUrl を media
// proxy へ通す (#3792)。**
//
// 見る経路は users/show (users の handler)、i (i の handler の MeDetailed)、
// blocking/create (router が作る共有の userpack.Packer)。上と同じく、組み立ての
// 後に既定の Packer の mediaURL を外し、渡した Packer だけで proxy が付くことを
// 確かめる。
func TestNewServer_UserDetailedPackersUseTheWiredPacker(t *testing.T) {
	db := serverIntegrationDB
	if db == nil {
		t.Skip("PostgreSQL unavailable")
	}
	const (
		viewerID = "eppviewer3792aaa"
		token    = "epptokenx3792aaa"
		targetID = "epptarget3792aaa"
		host     = "remote.example"
	)
	cleanupPluginAccountUsers(t, db, "epp_viewer")
	cleanupPluginAccountUsers(t, db, "epp_target")
	t.Cleanup(func() {
		db.Exec(`DELETE FROM "blocking" WHERE "blockerId" = ?`, viewerID)
	})
	createLocalUser(t, db, viewerID, "epp_viewer", token, "epp-pass", false)
	createLocalUser(t, db, targetID, "epp_target", "epptokenx3792bbb", "epp-pass", false)
	for _, id := range []string{viewerID, targetID} {
		avatarURL := "https://" + host + "/files/" + id + ".png"
		require.NoError(t, db.Exec(`UPDATE "user" SET "avatarUrl" = ? WHERE "id" = ?`, avatarURL, id).Error)
	}

	srv := newServerWithConfig(t, func(*config.Config) {})
	// 既定の Packer の mediaURL を外す。restoreProcessGlobals が後で nil に戻す。
	entity.SetMediaURLContext(nil)

	for _, tc := range []struct {
		name string
		path string
		body map[string]any
	}{
		{"users/show", "/api/users/show", map[string]any{"userId": targetID}},
		{"i", "/api/i", map[string]any{}},
		{"blocking/create", "/api/blocking/create", map[string]any{"userId": targetID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, srv, tc.path, token, tc.body)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var u struct {
				AvatarURL string `json:"avatarUrl"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &u), rec.Body.String())
			// 否定形だと空文字でも通るので、proxy の形であることを肯定形で見る。
			assert.Contains(t, u.AvatarURL, "/avatar.webp?", "avatarUrl: %s", u.AvatarURL)
			assert.NotContains(t, u.AvatarURL, "://"+host+"/", "avatarUrl: %s", u.AvatarURL)
		})
	}
}
