package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/model"
)

// **組み立てたサーバーで、主な GET のルートが HEAD に GET と同じ status と
// header を本文なしで返す (#3925)。** 本番のアクセスログで HEAD が 405 だった
// ルート (`/`、`/users/:id`、`/identicon/*`、`/@:acct`、`/files/*`) を含む。
// handler を直接呼ぶのではなく、本物のルーターと middleware を通す。
func TestNewServer_GETRoutesAnswerHEAD(t *testing.T) {
	db := serverIntegrationDB
	if db == nil {
		t.Skip("PostgreSQL unavailable")
	}
	// 連合を切っていると ActivityPub の応答と .well-known が 403 になるので、
	// 200 の応答で比べられるよう、組み立てる前に連合を開く。
	var meta struct{ Federation string }
	require.NoError(t, db.Raw(`SELECT "federation" AS federation FROM "meta" LIMIT 1`).Scan(&meta).Error)
	require.NoError(t, db.Exec(`UPDATE "meta" SET "federation" = 'all'`).Error)
	t.Cleanup(func() {
		require.NoError(t, db.Exec(`UPDATE "meta" SET "federation" = ?`, meta.Federation).Error)
	})
	srv := newServerWithConfig(t, func(*config.Config) {})

	const (
		userID    = "hruser3925aaaaaa"
		accessKey = "hr3925-access-key"
	)
	cleanupPluginAccountUsers(t, db, "hr_user")
	createLocalUser(t, db, userID, "hr_user", "hrusertokenx3925", "hr-pass", false)
	// actor の描画には鍵が要る。
	privPEM, pubPEM, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	require.NoError(t, db.Create(&model.UserKeypair{UserID: userID, PublicKey: pubPEM, PrivateKey: privPEM}).Error)
	idGen := testIDGen(t)
	noteID := idGen.Generate(time.Now())
	text := "head routes"
	require.NoError(t, db.Create(&model.Note{ID: noteID, UserID: userID, Visibility: model.NoteVisibility("public"), Text: &text}).Error)
	t.Cleanup(func() { db.Exec(`DELETE FROM "note" WHERE "id" = ?`, noteID) })

	// newServerWithConfig は一時ディレクトリへ移っているので、ローカルの
	// ドライブ (./drive-files) はそこに作られる。
	content := pngObject(10000)
	require.NoError(t, os.MkdirAll("drive-files", 0o755))
	require.NoError(t, os.WriteFile(filepath.Join("drive-files", accessKey), content, 0o644))
	key := accessKey
	uid := userID
	require.NoError(t, db.Create(&model.DriveFile{
		ID: idGen.Generate(time.Now()), UserID: &uid, MD5: "d41d8cd98f00b204e9800998ecf8427e",
		Name: "a.png", Type: "image/png", Size: len(content), StoredInternal: true,
		URL: "http://example.test/files/" + accessKey, AccessKey: &key,
	}).Error)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	ap := http.Header{"Accept": []string{"application/activity+json"}}
	common := []string{echo.HeaderContentType, echo.HeaderCacheControl, echo.HeaderLocation, echo.HeaderVary}
	for _, tc := range []struct {
		path   string
		header http.Header
		status int
	}{
		{path: "/"},
		{path: "/users/" + userID},
		{path: "/users/" + userID, header: ap, status: http.StatusOK},
		{path: "/notes/" + noteID},
		{path: "/notes/" + noteID, header: ap, status: http.StatusOK},
		{path: "/@hr_user"},
		{path: "/@hr_user", header: ap, status: http.StatusOK},
		{path: "/files/" + accessKey, status: http.StatusOK},
		{path: "/identicon/hr_user", status: http.StatusOK},
		{path: "/api/emojis", status: http.StatusOK},
		{path: "/api/server-info", status: http.StatusOK},
		// api.Any("/*") の catch-all。POST だけのエンドポイントと未知のパスは、
		// GET と同じく 404。
		{path: "/api/notes/create", status: http.StatusNotFound},
		{path: "/api/no-such-endpoint", status: http.StatusNotFound},
		{path: "/.well-known/nodeinfo", status: http.StatusOK},
		{path: "/.well-known/webfinger?resource=acct:hr_user@example.test", status: http.StatusOK},
		{path: "/proxy/image.webp"},
	} {
		name := tc.path
		if tc.header != nil {
			name += " (ActivityPub)"
		}
		t.Run(name, func(t *testing.T) {
			assertHEADLikeGET(t, ts.URL, tc.path, tc.header, common...)
			if tc.status != 0 {
				res := doRaw(t, ts.URL, http.MethodHead, tc.path, tc.header)
				assert.Equal(t, tc.status, res.StatusCode)
			}
		})
	}

	// handler には GET に見えるので、振り分けると WebSocket へ切り替えてしまう。
	// 本家 (ws の handleUpgrade) も #3925 より前も 405。
	t.Run("/streaming HEAD does not upgrade", func(t *testing.T) {
		upgrade := http.Header{
			"Connection": {"Upgrade"}, "Upgrade": {"websocket"},
			"Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="}, "Sec-Websocket-Version": {"13"},
		}
		res := doRaw(t, ts.URL, http.MethodHead, "/streaming", upgrade)
		assert.Equal(t, http.StatusMethodNotAllowed, res.StatusCode)
	})

	t.Run("/files HEAD answers the whole size", func(t *testing.T) {
		res := doRaw(t, ts.URL, http.MethodHead, "/files/"+accessKey, nil)
		require.Equal(t, http.StatusOK, res.StatusCode)
		assert.Equal(t, strconv.Itoa(len(content)), res.Header.Get(echo.HeaderContentLength))
		assert.Equal(t, "image/png", res.Header.Get(echo.HeaderContentType))
		assert.Empty(t, res.trailing)
	})
}
