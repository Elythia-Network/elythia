package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/plugin"
)

var clientCtxRe = regexp.MustCompile(`<script type="application/json" id="misskey_clientCtx" data-generated-at="\d+">(.*?)</script>`)

// newClientCtxServer builds a full server on the integration database with
// the given plugins, the way TestNewServer_DeliveryRulesHideNotesFromVisitors
// does (newServerWithConfig takes no plugins).
func newClientCtxServer(t *testing.T, defs []plugin.Definition) *Server {
	t.Helper()
	t.Setenv(config.EnvOnlyServer, "1")
	t.Setenv(config.EnvOnlyQueue, "")
	t.Chdir(t.TempDir())
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, rc.Close()) })
	clients := &cache.RedisClients{Default: rc, Pubsub: rc, JobQueue: rc, Timelines: rc, Reactions: rc}
	port, err := strconv.Atoi(mr.Port())
	require.NoError(t, err)
	ro := config.RedisOptions{Host: mr.Host(), Port: port}
	cfg := &config.Config{
		URL: "http://example.test", Host: "example.test", Hostname: "example.test",
		Scheme: "http", WsScheme: "ws", ID: "aidx", TestMode: true,
		MediaProxySecret: []byte("test-secret"),
		Redis:            ro, RedisForPubsub: ro, RedisForJobQueue: ro, RedisForTimelines: ro, RedisForReactions: ro,
	}
	srv, err := newServer(cfg, serverIntegrationDB, clients, defs, noopStorage)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	return srv
}

// getPage fetches a permalink as a browser does. header adds request headers
// (a signed-in browser's Authorization / Cookie).
func getPage(t *testing.T, srv *Server, path string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, path)
	return rec
}

// clientCtxOf returns the decoded misskey_clientCtx of a page, or nil when the
// page has none. 要素が 2 つ以上ある、または中身が JSON として読めないなら落とす
// (`</script>` で要素が途中で閉じると、ここで読めなくなる)。
func clientCtxOf(t *testing.T, body string) map[string]any {
	t.Helper()
	n := strings.Count(body, `id="misskey_clientCtx"`)
	require.LessOrEqual(t, n, 1, "at most one misskey_clientCtx")
	if n == 0 {
		return nil
	}
	m := clientCtxRe.FindStringSubmatch(body)
	require.NotNil(t, m, "misskey_clientCtx must be a complete script element")
	var ctx map[string]any
	require.NoError(t, json.Unmarshal([]byte(m[1]), &ctx), m[1])
	return ctx
}

func decodeJSON(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	require.NoError(t, json.Unmarshal(b, &v), string(b))
	return v
}

// **ノート・利用者・クリップのページに埋める中身は、未ログインの notes/show・
// users/show・clips/show の応答と同じで、API が断るときは何も埋めない (#4271)。**
//
// 埋める中身は本文と添付をすべて含むので、未ログインに伏せる規則が API とずれると、
// 伏せたはずの中身が HTML から読める。公開範囲、作者の設定、ピン留めの公開境界
// (#3310)、プラグインの条件 (#4203)、凍結、リモート、サーバーの設定の各条件で、
// 埋めた中身を API の応答と突き合わせる。
func TestNewServer_PageClientCtxMatchesTheVisitorAPI(t *testing.T) {
	db := serverIntegrationDB
	if db == nil {
		t.Skip("PostgreSQL unavailable")
	}
	var meta struct{ UgcVisibilityForVisitor string }
	require.NoError(t, db.Raw(`SELECT "ugcVisibilityForVisitor" AS ugc_visibility_for_visitor FROM "meta" LIMIT 1`).Scan(&meta).Error)
	t.Cleanup(func() {
		require.NoError(t, db.Exec(`UPDATE "meta" SET "ugcVisibilityForVisitor" = ?`, meta.UgcVisibilityForVisitor).Error)
	})

	const (
		plainID        = "ccxplain4271aaaa"
		signinID       = "ccxsignin4271aaa"
		hiddenID       = "ccxhidden4271aaa"
		suspendID      = "ccxsuspnd4271aaa"
		deletedID      = "ccxdeletd4271aaa"
		remoteID       = "ccxremote4271aaa"
		plainToken     = "ccxplaintok4271x"
		remoteHostName = "remote.example"
	)
	cleanupPluginAccountUsers(t, db, "ccx_plain", "ccx_signin", "ccx_hidden", "ccx_suspend", "ccx_deleted")
	cleanRemote := func() {
		db.Exec(`DELETE FROM "user_profile" WHERE "userId" = ?`, remoteID)
		db.Exec(`DELETE FROM "user" WHERE "id" = ?`, remoteID)
	}
	cleanRemote()
	t.Cleanup(cleanRemote)
	createLocalUser(t, db, plainID, "ccx_plain", plainToken, "ccx-pass", false)
	createLocalUser(t, db, signinID, "ccx_signin", "ccxsignintk4271x", "ccx-pass", false)
	createLocalUser(t, db, hiddenID, "ccx_hidden", "ccxhiddentk4271x", "ccx-pass", false)
	createLocalUser(t, db, suspendID, "ccx_suspend", "ccxsuspndtk4271x", "ccx-pass", false)
	createLocalUser(t, db, deletedID, "ccx_deleted", "ccxdeletdtk4271x", "ccx-pass", false)
	// 削除したローカルの利用者は、HTML の側の判定 (凍結だけを見る) を通るが、
	// users/show は NO_SUCH_USER で断る。API が断るときに埋めないことを、ここで見る。
	require.NoError(t, db.Exec(`UPDATE "user" SET "isDeleted" = true WHERE "id" = ?`, deletedID).Error)
	require.NoError(t, db.Exec(`UPDATE "user" SET "requireSigninToViewContents" = true WHERE "id" = ?`, signinID).Error)
	// 1 分より前のノートを隠す。下のノートは 1 時間前に作る。
	require.NoError(t, db.Exec(`UPDATE "user" SET "makeNotesHiddenBefore" = -60 WHERE "id" = ?`, hiddenID).Error)
	require.NoError(t, db.Exec(`UPDATE "user" SET "isSuspended" = true WHERE "id" = ?`, suspendID).Error)
	host := remoteHostName
	require.NoError(t, db.Create(&model.User{
		ID: remoteID, Username: "ccx_remote", UsernameLower: "ccx_remote", Host: &host,
		AvatarDecorations: []byte("[]"),
	}).Error)
	require.NoError(t, db.Create(&model.UserProfile{UserID: remoteID}).Error)

	idGen := testIDGen(t)
	now := time.Now()
	old := now.Add(-time.Hour)
	var noteIDs, pinIDs, clipIDs []string
	newNote := func(at time.Time, userID string, vis model.NoteVisibility, text string, mutate func(*model.Note)) string {
		at = at.Add(time.Duration(len(noteIDs)) * time.Millisecond)
		id := idGen.Generate(at)
		n := &model.Note{ID: id, UserID: userID, Visibility: vis}
		if text != "" {
			n.Text = &text
		}
		if userID == remoteID {
			n.UserHost = &host
		}
		if mutate != nil {
			mutate(n)
		}
		require.NoError(t, db.Create(n).Error)
		noteIDs = append(noteIDs, id)
		return id
	}
	pin := func(userID, noteID string) {
		id := idGen.Generate(now)
		require.NoError(t, db.Create(&model.UserNotePining{ID: id, UserID: userID, NoteID: noteID}).Error)
		pinIDs = append(pinIDs, id)
	}
	t.Cleanup(func() {
		db.Exec(`DELETE FROM "clip" WHERE "id" IN ?`, append(clipIDs, ""))
		db.Exec(`DELETE FROM "user_note_pining" WHERE "id" IN ?`, append(pinIDs, ""))
		db.Exec(`DELETE FROM "note" WHERE "id" IN ?`, append(noteIDs, ""))
	})

	// 埋めた HTML の中に、生の `<` `>` `&` U+2028 U+2029 が出ないことを見るための本文。
	// json.Marshal が既定で同じ文字を潰すので、clientCtxJSON の escapeJSONForScript を
	// 外してもこの検査は通る。escapeJSONForScript そのものが効いていることは
	// TestEscapeJSONForScript が見る。
	const breakout = "ccx-public </script><script>alert(1)</script><!-- \u2028\u2029 & >"
	public := newNote(now, plainID, model.NoteVisibilityPublic, breakout, nil)
	home := newNote(now, plainID, model.NoteVisibilityHome, "ccx-home-body", nil)
	followers := newNote(now, plainID, model.NoteVisibilityFollowers, "ccx-secret-followers", nil)
	specified := newNote(now, plainID, model.NoteVisibilitySpecified, "ccx-secret-specified", nil)
	// 公開のリノートの中の、フォロワー限定のノート。外側は埋め、中は伏せる。
	renote := newNote(now, plainID, model.NoteVisibilityPublic, "", func(n *model.Note) { n.RenoteID = &followers })
	restricted := newNote(now, plainID, model.NoteVisibilityPublic, "ccx-secret-plugin", nil)
	signin := newNote(now, signinID, model.NoteVisibilityPublic, "ccx-secret-signin", nil)
	hiddenOld := newNote(old, hiddenID, model.NoteVisibilityPublic, "ccx-secret-hidden-before", nil)
	hiddenPinned := newNote(old, hiddenID, model.NoteVisibilityPublic, "ccx-pinned-hidden-before", nil)
	suspended := newNote(now, suspendID, model.NoteVisibilityPublic, "ccx-suspended-body", nil)
	remote := newNote(now, remoteID, model.NoteVisibilityPublic, "ccx-remote-body", nil)
	// ピン留めは作者の期間の設定を越えて見せ (#3310)、フォロワー限定は越えない。
	pin(hiddenID, hiddenPinned)
	pin(plainID, followers)
	publicClip, privateClip := idGen.Generate(now), idGen.Generate(now.Add(time.Millisecond))
	clipIDs = append(clipIDs, publicClip, privateClip)
	desc := "ccx </script> clip"
	require.NoError(t, db.Create(&model.Clip{ID: publicClip, UserID: plainID, Name: "ccx-clip", IsPublic: true, Description: &desc}).Error)
	require.NoError(t, db.Create(&model.Clip{ID: privateClip, UserID: plainID, Name: "ccx-secret-clip", IsPublic: false}).Error)

	def := noteDeliveryDef("ccx", func(plugin.Context) (plugin.NoteDeliveryRegistration, error) {
		return validNoteDelivery(map[string]plugin.DeliveryTargets{
			restricted: {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"example.test"}},
		}), nil
	})

	// checkPage fetches path, asks api the same thing as a visitor, and checks
	// that the page embeds key exactly when wantEmbed, with the API's response.
	checkPage := func(t *testing.T, srv *Server, path, key, api string, body map[string]any, wantEmbed bool, secret string) {
		t.Helper()
		page := getPage(t, srv, path, nil)
		ctx := clientCtxOf(t, page.Body.String())
		res := postJSON(t, srv, api, "", body)
		if !wantEmbed {
			assert.Nil(t, ctx, "nothing may be embedded (%s answered %d: %s)", api, res.Code, res.Body.String())
			if secret != "" {
				assert.NotContains(t, page.Body.String(), secret)
			}
			return
		}
		require.Equal(t, http.StatusOK, res.Code, res.Body.String())
		require.NotNil(t, ctx, "the page must embed %s", key)
		require.Len(t, ctx, 1)
		assert.Equal(t, decodeJSON(t, res.Body.Bytes()), ctx[key])
		assert.Equal(t, ssrPermalinkCacheControl, page.Header().Get("Cache-Control"))
		if secret != "" {
			assert.NotContains(t, page.Body.String(), secret)
		}
	}
	notePage := func(t *testing.T, srv *Server, id string, wantEmbed bool, secret string) {
		t.Helper()
		checkPage(t, srv, "/notes/"+id, "note", "/api/notes/show", map[string]any{"noteId": id}, wantEmbed, secret)
	}
	userPage := func(t *testing.T, srv *Server, acct, username string, h *string, wantEmbed bool) {
		t.Helper()
		body := map[string]any{"username": username}
		if h != nil {
			body["host"] = *h
		}
		checkPage(t, srv, "/@"+acct, "user", "/api/users/show", body, wantEmbed, "")
	}

	t.Run("local", func(t *testing.T) {
		require.NoError(t, db.Exec(`UPDATE "meta" SET "ugcVisibilityForVisitor" = 'local'`).Error)
		srv := newClientCtxServer(t, []plugin.Definition{def})

		t.Run("note", func(t *testing.T) {
			for _, tt := range []struct {
				name   string
				id     string
				embed  bool
				secret string
			}{
				{name: "public", id: public, embed: true},
				{name: "home", id: home, embed: true},
				{name: "followers", id: followers, secret: "ccx-secret-followers"},
				{name: "specified", id: specified, secret: "ccx-secret-specified"},
				{name: "renote of a followers note", id: renote, embed: true, secret: "ccx-secret-followers"},
				{name: "hidden from visitors by a plugin", id: restricted, secret: "ccx-secret-plugin"},
				{name: "author requires sign-in", id: signin, secret: "ccx-secret-signin"},
				{name: "author hides old notes", id: hiddenOld, secret: "ccx-secret-hidden-before"},
				{name: "pinned past the author's window", id: hiddenPinned, embed: true},
				// 本家と同じく、凍結した作者のノートも notes/show は返す (GetterService は
				// 凍結を見ない)。埋める中身もそれと同じになる。
				{name: "suspended author", id: suspended, embed: true},
				{name: "remote under local", id: remote, secret: "ccx-remote-body"},
			} {
				t.Run(tt.name, func(t *testing.T) { notePage(t, srv, tt.id, tt.embed, tt.secret) })
			}
		})

		t.Run("note page escapes the embedded JSON", func(t *testing.T) {
			body := getPage(t, srv, "/notes/"+public, nil).Body.String()
			m := clientCtxRe.FindStringSubmatch(body)
			require.NotNil(t, m)
			raw := m[1]
			for _, s := range []string{"<", ">", "&", "\u2028", "\u2029"} {
				assert.NotContains(t, raw, s)
			}
			ctx := clientCtxOf(t, body)
			note, _ := ctx["note"].(map[string]any)
			assert.Equal(t, breakout, note["text"])
		})

		t.Run("signed-in request gets the visitor's content", func(t *testing.T) {
			// HTML は閲覧者を見ずに作る。ログインしている人の要求でも、伏せた中身を
			// 埋めない (共有キャッシュに載ったものが他の人に渡っても漏れない)。
			page := getPage(t, srv, "/notes/"+renote, map[string]string{
				"Authorization": "Bearer " + plainToken,
				"Cookie":        "token=" + plainToken,
			})
			assert.NotContains(t, page.Body.String(), "ccx-secret-followers")
			res := postJSON(t, srv, "/api/notes/show", "", map[string]any{"noteId": renote})
			require.Equal(t, http.StatusOK, res.Code)
			assert.Equal(t, decodeJSON(t, res.Body.Bytes()), clientCtxOf(t, page.Body.String())["note"])
		})

		t.Run("user", func(t *testing.T) {
			userPage(t, srv, "ccx_plain", "ccx_plain", nil, true)
			userPage(t, srv, "ccx_plain/notes", "ccx_plain", nil, true)
			userPage(t, srv, "ccx_hidden", "ccx_hidden", nil, true)
			userPage(t, srv, "ccx_signin", "ccx_signin", nil, true)
			userPage(t, srv, "ccx_suspend", "ccx_suspend", nil, false)
			userPage(t, srv, "ccx_deleted", "ccx_deleted", nil, false)
			userPage(t, srv, "ccx_remote@"+remoteHostName, "ccx_remote", &host, false)
			userPage(t, srv, "ccx_nobody", "ccx_nobody", nil, false)
		})

		t.Run("user pins", func(t *testing.T) {
			// ピン留めの中身も users/show と同じ判定を通る (#3310)。
			// フォロワー限定のピン留めは、未ログインの users/show に載らない。
			page := getPage(t, srv, "/@ccx_plain", nil).Body.String()
			assert.NotContains(t, page, "ccx-secret-followers")
			ctx := clientCtxOf(t, page)
			u, _ := ctx["user"].(map[string]any)
			pins, _ := u["pinnedNotes"].([]any)
			assert.Empty(t, pins)
			ctx = clientCtxOf(t, getPage(t, srv, "/@ccx_hidden", nil).Body.String())
			u, _ = ctx["user"].(map[string]any)
			pins, _ = u["pinnedNotes"].([]any)
			require.Len(t, pins, 1)
			assert.Equal(t, "ccx-pinned-hidden-before", pins[0].(map[string]any)["text"])
		})

		t.Run("clip", func(t *testing.T) {
			checkPage(t, srv, "/clips/"+publicClip, "clip", "/api/clips/show", map[string]any{"clipId": publicClip}, true, "")
			checkPage(t, srv, "/clips/"+privateClip, "clip", "/api/clips/show", map[string]any{"clipId": privateClip}, false, "ccx-secret-clip")
		})
	})

	t.Run("none", func(t *testing.T) {
		require.NoError(t, db.Exec(`UPDATE "meta" SET "ugcVisibilityForVisitor" = 'none'`).Error)
		srv := newClientCtxServer(t, nil)
		notePage(t, srv, public, false, "")
		userPage(t, srv, "ccx_plain", "ccx_plain", nil, false)
	})

	t.Run("all", func(t *testing.T) {
		require.NoError(t, db.Exec(`UPDATE "meta" SET "ugcVisibilityForVisitor" = 'all'`).Error)
		srv := newClientCtxServer(t, nil)
		notePage(t, srv, remote, true, "")
		// リモートの利用者には埋めない。users/show は 200 を返すが、HTML で同じ関数を
		// 通すと #943 のカウントの取得を待つので、frontend に users/show を呼ばせる。
		for _, acct := range []string{"ccx_remote@" + remoteHostName, "ccx_remote@" + remoteHostName + "/notes"} {
			page := getPage(t, srv, "/@"+acct, nil)
			assert.Nil(t, clientCtxOf(t, page.Body.String()), acct)
			assert.Contains(t, page.Body.String(), `<meta name="misskey:user-id" content="`+remoteID+`">`, "the page still renders the user's meta")
		}
		userPage(t, srv, "ccx_plain", "ccx_plain", nil, true)
	})
}

// clientCtxJSON embeds nothing when the API declines, and its output has no
// raw `<` `>` `&` U+2028 U+2029, so note text cannot close the script element.
// 出力の性質を見るテストで、json.Marshal の既定でも満たされる。escapeJSONForScript
// そのものは TestEscapeJSONForScript が見る。
func TestClientCtxJSON(t *testing.T) {
	assert.Empty(t, clientCtxJSON("note", func() (any, bool) { return map[string]any{"text": "x"}, false }))
	got := clientCtxJSON("note", func() (any, bool) {
		return map[string]any{"text": "</script><!-- \u2028\u2029 &"}, true
	})
	for _, s := range []string{"<", ">", "&", "\u2028", "\u2029"} {
		assert.NotContains(t, got, s)
	}
	var v map[string]map[string]string
	require.NoError(t, json.Unmarshal([]byte(got), &v))
	assert.Equal(t, "</script><!-- \u2028\u2029 &", v["note"]["text"])

	var h ssrMetaHandler
	assert.Empty(t, h.noteClientCtx(context.Background(), "n"))
	assert.Empty(t, h.userClientCtx(context.Background(), &model.User{ID: "u"}))
	assert.Empty(t, h.clipClientCtx("c"))
}
