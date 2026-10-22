package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/model"
)

// **閲覧者自身をMeDetailedへ昇格させる6つのhandlerが、組み立てで作った
// `meself.Packer`を受け取ること(#3784)。**
//
// 以前は`meself.SetEnricher`でパッケージの変数に1度登録したので、配線の漏れは
// 起こりえなかった。handlerごとにsetterで渡す形にしたので、1つ渡し忘れると
// その経路だけ、/api/iのhandlerが埋める項目(未読・role・ピン留めなど)が既定値の
// まま返る。エラーにはならないので、ここで見る。
//
// 閲覧者はroot(管理者)にする。entity層のMeDetailedは`isAdmin`をfalseのまま
// にし、/api/iのhandlerだけがroleからtrueを埋めるので、補完が効いたかどうかが
// `isAdmin`の値に出る。
func TestNewServer_WiresTheSelfPackerToEveryHandler(t *testing.T) {
	db := serverIntegrationDB
	if db == nil {
		t.Skip("PostgreSQL unavailable")
	}
	const (
		meID     = "spme3784aaaaaaaa"
		token    = "spmetokenxx3784a"
		remoteID = "spremote3784aaaa"
		host     = "remote3784.example"
		tag      = "selfpacker3784"
	)
	followingID := "spfollow3784aaaa"
	roleID, assignID := "sprole3784aaaaa", "spassign3784aaaa"
	pageID := "sppage3784aaaaaa"
	// 固定のIDで作るので、途中で落ちた前回の行が残っていても作り直せるよう、
	// 作る前にも消す。
	cleanup := func() {
		db.Exec(`DELETE FROM "page" WHERE "id" = ?`, pageID)
		db.Exec(`DELETE FROM "role_assignment" WHERE "id" = ?`, assignID)
		db.Exec(`DELETE FROM "role" WHERE "id" = ?`, roleID)
		db.Exec(`DELETE FROM "following" WHERE "id" = ?`, followingID)
		db.Exec(`DELETE FROM "user" WHERE "id" = ?`, remoteID)
	}
	cleanup()
	t.Cleanup(cleanup)
	cleanupPluginAccountUsers(t, db, "sp_me")
	createLocalUser(t, db, meID, "sp_me", token, "sp-pass", true)
	require.NoError(t, db.Exec(`UPDATE "user" SET "tags" = ARRAY[?]::varchar(128)[] WHERE "id" = ?`, tag, meID).Error)

	// federation/following は、そのホストの利用者(followerHost)のフォローを返し、
	// フォローされる側(=自分)を昇格させる。本家と同じく、名前とは逆に
	// federation/followers は followeeHost で絞るので、自分は出てこない。
	remoteHost := host
	require.NoError(t, db.Create(&model.User{
		ID: remoteID, Username: "sp_remote", UsernameLower: "sp_remote", Host: &remoteHost,
		AvatarDecorations: []byte("[]"),
	}).Error)
	require.NoError(t, db.Create(&model.Following{
		ID: followingID, FolloweeID: meID, FollowerID: remoteID, FollowerHost: &remoteHost,
	}).Error)

	// roles/users は公開かつ探せるロールの持ち主を返す。
	now := time.Now()
	require.NoError(t, db.Create(&model.Role{
		ID: roleID, Name: "sp-role-3784", UpdatedAt: now, LastUsedAt: now, IsPublic: true, IsExplorable: true,
	}).Error)
	require.NoError(t, db.Create(&model.RoleAssignment{ID: assignID, UserID: meID, RoleID: roleID}).Error)

	// page-push は、自分のページへのイベントを自分のmainへ流す。
	require.NoError(t, db.Create(&model.Page{
		ID: pageID, UpdatedAt: now, Title: "sp", Name: "sp-page-3784", UserID: meID,
		Visibility: model.PageVisibilityPublic,
	}).Error)

	srv := newServerWithConfig(t, func(*config.Config) {})

	post := func(t *testing.T, path string, body map[string]any) []byte {
		t.Helper()
		rec := postJSON(t, srv, path, token, body)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
		return rec.Body.Bytes()
	}
	// findSelf returns the entry for the viewer in a list of users (or of
	// objects that embed the user under key).
	findSelf := func(t *testing.T, raw []byte, key string) map[string]any {
		t.Helper()
		var list []map[string]any
		require.NoError(t, json.Unmarshal(raw, &list), string(raw))
		for _, item := range list {
			u := item
			if key != "" {
				u, _ = item[key].(map[string]any)
			}
			if u != nil && u["id"] == meID {
				return u
			}
		}
		require.Failf(t, "the viewer is not in the response", "%s", raw)
		return nil
	}

	for _, tt := range []struct {
		name string
		self func(t *testing.T) map[string]any
	}{
		{name: "users/show", self: func(t *testing.T) map[string]any {
			var u map[string]any
			require.NoError(t, json.Unmarshal(post(t, "/api/users/show", map[string]any{"userId": meID}), &u))
			return u
		}},
		{name: "users/show userIds", self: func(t *testing.T) map[string]any {
			return findSelf(t, post(t, "/api/users/show", map[string]any{"userIds": []string{meID}}), "")
		}},
		{name: "hashtags/users", self: func(t *testing.T) map[string]any {
			return findSelf(t, post(t, "/api/hashtags/users", map[string]any{"tag": tag, "sort": "+follower", "state": "all", "origin": "local"}), "")
		}},
		{name: "federation/following", self: func(t *testing.T) map[string]any {
			return findSelf(t, post(t, "/api/federation/following", map[string]any{"host": host}), "followee")
		}},
		{name: "roles/users", self: func(t *testing.T) map[string]any {
			return findSelf(t, post(t, "/api/roles/users", map[string]any{"roleId": roleID}), "user")
		}},
		{name: "admin/show-users", self: func(t *testing.T) map[string]any {
			return findSelf(t, post(t, "/api/admin/show-users", map[string]any{"username": "sp_me", "origin": "local"}), "")
		}},
		{name: "page-push", self: func(t *testing.T) map[string]any {
			ctx := context.Background()
			sub := srv.redis.Pubsub.PSubscribe(ctx, "stream:*")
			t.Cleanup(func() { _ = sub.Close() })
			_, err := sub.Receive(ctx)
			require.NoError(t, err)
			rec := postJSON(t, srv, "/api/page-push", token, map[string]any{"pageId": pageID, "event": "sp"})
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			timeout := time.After(10 * time.Second)
			for {
				select {
				case msg := <-sub.Channel():
					if !strings.Contains(msg.Payload, `"pageEvent"`) {
						continue
					}
					var ev struct {
						Body struct {
							PageID string         `json:"pageId"`
							User   map[string]any `json:"user"`
						} `json:"body"`
					}
					require.NoError(t, json.Unmarshal([]byte(msg.Payload), &ev), msg.Payload)
					if ev.Body.PageID != pageID {
						continue
					}
					require.NotNil(t, ev.Body.User, msg.Payload)
					return ev.Body.User
				case <-timeout:
					require.FailNow(t, "pageEvent was not published")
					return nil
				}
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			u := tt.self(t)
			assert.Equal(t, meID, u["id"])
			assert.Equal(t, true, u["isAdmin"], "/api/iのhandlerの補完が効いていない(Packerが渡っていない)")
		})
	}
}
