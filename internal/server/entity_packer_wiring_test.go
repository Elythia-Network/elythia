package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/model"
)

// **投稿を包む handler は、組み立てで渡された Packer でリモートのファイルを
// media proxy へ通す (#3790)。**
//
// 以前は既定の Packer (パッケージの変数) を読んでいた。組み立てで Packer を
// 渡す形にし、#3800 で既定の Packer は本番から無くなった。handler か
// NoteFieldResolver に Packer が渡らないと、閲覧者のブラウザがリモートの
// ファイルを直接取りに行く。ここでは渡した Packer で proxy が付くことを確かめる。
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
// blocking/create (router が作る共有の userpack.Packer)。上と同じく、渡した
// Packer で proxy が付くことを確かめる。
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

// **UserLite を包む部品も、組み立てで渡された Packer で avatarUrl を media proxy
// へ通す (#3794)。**
//
// 見る経路は flash/create (flash の handler)、chat/rooms/create (chat の handler の
// レシーバの無い packRoom)、/embed/user-timeline (router のクロージャ)。上と同じく、
// 渡した Packer で proxy が付くことを確かめる。
func TestNewServer_UserLitePackersUseTheWiredPacker(t *testing.T) {
	db := serverIntegrationDB
	if db == nil {
		t.Skip("PostgreSQL unavailable")
	}
	const (
		userID = "eppuser3794aaaaa"
		token  = "epptokenx3794aaa"
		host   = "remote.example"
	)
	cleanupPluginAccountUsers(t, db, "epp_lite")
	t.Cleanup(func() {
		db.Exec(`DELETE FROM "flash" WHERE "userId" = ?`, userID)
		db.Exec(`DELETE FROM "chat_room" WHERE "ownerId" = ?`, userID)
	})
	createLocalUser(t, db, userID, "epp_lite", token, "epp-pass", false)
	avatarURL := "https://" + host + "/files/epp3794-avatar.png"
	require.NoError(t, db.Exec(`UPDATE "user" SET "avatarUrl" = ? WHERE "id" = ?`, avatarURL, userID).Error)

	srv := newServerWithConfig(t, func(*config.Config) {})

	t.Run("flash/create", func(t *testing.T) {
		rec := postJSON(t, srv, "/api/flash/create", token, map[string]any{
			"title": "t", "summary": "s", "script": "", "permissions": []string{},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var f struct {
			User struct {
				AvatarURL string `json:"avatarUrl"`
			} `json:"user"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &f), rec.Body.String())
		// 否定形だと空文字でも通るので、proxy の形であることを肯定形で見る。
		assert.Contains(t, f.User.AvatarURL, "/avatar.webp?", "avatarUrl: %s", f.User.AvatarURL)
		assert.NotContains(t, f.User.AvatarURL, "://"+host+"/", "avatarUrl: %s", f.User.AvatarURL)
	})
	t.Run("chat/rooms/create", func(t *testing.T) {
		rec := postJSON(t, srv, "/api/chat/rooms/create", token, map[string]any{"name": "epp3794"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var room struct {
			Owner struct {
				AvatarURL string `json:"avatarUrl"`
			} `json:"owner"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &room), rec.Body.String())
		assert.Contains(t, room.Owner.AvatarURL, "/avatar.webp?", "avatarUrl: %s", room.Owner.AvatarURL)
		assert.NotContains(t, room.Owner.AvatarURL, "://"+host+"/", "avatarUrl: %s", room.Owner.AvatarURL)
	})
	t.Run("embed/user-timeline", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/embed/user-timeline/"+userID, nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		// 利用者は misskey_embedCtx の JSON に埋め込まれる。
		body := rec.Body.String()
		const open = `<script type="application/json" id="misskey_embedCtx">`
		start := strings.Index(body, open)
		require.GreaterOrEqual(t, start, 0, body)
		rest := body[start+len(open):]
		end := strings.Index(rest, "</script>")
		require.GreaterOrEqual(t, end, 0, body)
		var ctx struct {
			User struct {
				AvatarURL string `json:"avatarUrl"`
			} `json:"user"`
		}
		require.NoError(t, json.Unmarshal([]byte(rest[:end]), &ctx), rest[:end])
		assert.True(t, strings.HasPrefix(ctx.User.AvatarURL, "/avatar.webp?"), "avatarUrl: %s", ctx.User.AvatarURL)
	})
}

// **残りの Pack を呼ぶ部品も、組み立てで渡された Packer でリモートの URL を media
// proxy へ通す (#3796)。**
//
// 見る経路は announcements (お知らせの imageUrl) と drive/files/update (リモートの
// ファイルの url)。上と同じく、渡した Packer で proxy が付くことを
// 確かめる。通知は stream の
// TestNotificationPublisher_PacksWithTheGivenPacker が見る。
func TestNewServer_RemainingPackersUseTheWiredPacker(t *testing.T) {
	db := serverIntegrationDB
	if db == nil {
		t.Skip("PostgreSQL unavailable")
	}
	const (
		userID = "eppuser3796aaaaa"
		token  = "epptokenx3796aaa"
		host   = "remote.example"
	)
	idGen := testIDGen(t)
	annID := idGen.Generate(time.Now())
	fileID := idGen.Generate(time.Now())
	cleanupPluginAccountUsers(t, db, "epp_rest")
	t.Cleanup(func() {
		db.Exec(`DELETE FROM "announcement" WHERE "id" = ?`, annID)
		db.Exec(`DELETE FROM "drive_file" WHERE "id" = ?`, fileID)
	})
	createLocalUser(t, db, userID, "epp_rest", token, "epp-pass", false)
	imageURL := "https://" + host + "/files/epp3796-ann.png"
	require.NoError(t, db.Create(&model.Announcement{
		ID: annID, Title: "t", Text: "x", ImageURL: &imageURL, IsActive: true, UserID: strPtrLeak(userID),
	}).Error)
	fileURL := "https://" + host + "/files/epp3796.png"
	require.NoError(t, db.Create(&model.DriveFile{
		ID: fileID, UserID: strPtrLeak(userID), MD5: "d41d8cd98f00b204e9800998ecf8427e",
		Name: "epp3796.png", Type: "image/png", URL: fileURL, URI: strPtrLeak(fileURL), IsLink: true,
	}).Error)

	srv := newServerWithConfig(t, func(*config.Config) {})
	// 否定形だと空文字でも通るので、proxy の形であることを肯定形で見る。
	proxied := func(t *testing.T, got string) {
		t.Helper()
		assert.Contains(t, got, "/image.webp?", "url: %s", got)
		assert.False(t, strings.HasPrefix(got, "https://"+host), "url: %s", got)
	}

	t.Run("announcements", func(t *testing.T) {
		rec := postJSON(t, srv, "/api/announcements", token, map[string]any{})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var anns []struct {
			ID       string `json:"id"`
			ImageURL string `json:"imageUrl"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &anns), rec.Body.String())
		var got string
		for _, a := range anns {
			if a.ID == annID {
				got = a.ImageURL
			}
		}
		proxied(t, got)
	})
	t.Run("drive/files/update", func(t *testing.T) {
		rec := postJSON(t, srv, "/api/drive/files/update", token, map[string]any{"fileId": fileID, "name": "renamed.png"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var f struct {
			URL string `json:"url"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &f), rec.Body.String())
		proxied(t, f.URL)
	})
}

// **/avatar と /emoji は、組み立てで作った Packer でリモートの画像を media proxy
// へ通す (#3798)。**
//
// どちらも部品を持たず、router が組み立てた Packer をそのまま渡す。上と同じく、
// 渡した Packer で proxy が付くことを確かめる。
func TestNewServer_MediaRoutesUseTheWiredPacker(t *testing.T) {
	db := serverIntegrationDB
	if db == nil {
		t.Skip("PostgreSQL unavailable")
	}
	const (
		userID = "eppuser3798aaaaa"
		token  = "epptokenx3798aaa"
		host   = "remote.example"
	)
	idGen := testIDGen(t)
	emojiID := idGen.Generate(time.Now())
	cleanupPluginAccountUsers(t, db, "epp_media")
	t.Cleanup(func() {
		db.Exec(`DELETE FROM "emoji" WHERE "id" = ?`, emojiID)
	})
	createLocalUser(t, db, userID, "epp_media", token, "epp-pass", false)
	avatarURL := "https://" + host + "/files/epp3798-avatar.png"
	require.NoError(t, db.Exec(`UPDATE "user" SET "avatarUrl" = ? WHERE "id" = ?`, avatarURL, userID).Error)
	emojiURL := "https://" + host + "/files/epp3798-emoji.png"
	require.NoError(t, db.Create(&model.Emoji{
		ID: emojiID, Name: "epp3798", Host: strPtrLeak(host), OriginalURL: emojiURL, PublicURL: emojiURL,
	}).Error)

	srv := newServerWithConfig(t, func(*config.Config) {})

	for _, tc := range []struct{ name, path, shape string }{
		{"avatar", "/avatar/@epp_media", "/avatar.webp?"},
		{"emoji", "/emoji/epp3798@" + host + ".webp", "/emoji.webp?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			require.Equal(t, http.StatusFound, rec.Code, rec.Body.String())
			loc := rec.Header().Get("Location")
			// 否定形だと空文字でも通るので、proxy の形であることを肯定形で見る。
			assert.Contains(t, loc, tc.shape, "Location: %s", loc)
			assert.False(t, strings.HasPrefix(loc, "https://"+host), "Location: %s", loc)
		})
	}
}
