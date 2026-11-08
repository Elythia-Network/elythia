package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/labstack/echo/v4"
	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/api/paramschema"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/effectivepolicy"
	"github.com/elythia-network/elythia/internal/misc/permissions"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// The answers of the gates of the routes give, as "<status>:<code>[:<id>]".
const (
	gatePassed        = "400:INVALID_PARAM:3d81ceae-475f-4600-b2a8-2bc116157532"
	gateNoCredential  = "401:CREDENTIAL_REQUIRED:1384574d-a912-4b81-8601-c7b1c4085df1"
	gateNotAdmin      = "403:ROLE_PERMISSION_DENIED:c3d38592-54c0-429d-be96-5636b0431a61"
	gateNotModerator  = "403:ROLE_PERMISSION_DENIED:d33d5333-db36-423d-a8f9-1a2b9549da41"
	gateNoPolicy      = "403:ROLE_PERMISSION_DENIED:7f86f06f-7e15-4057-8561-f4b6d4ac755a"
	gateMoved         = "403:YOUR_ACCOUNT_MOVED:56f20ec9-fd06-4fa5-841b-edd6d7d4fa31"
	gateAppPermission = "403:PERMISSION_DENIED:1370e5b7-d4eb-4566-bb1d-7748ee6a1838"
	gateAppNotSecure  = "403:ACCESS_DENIED:56f35758-7dd5-468b-8439-5d6fb8ec9b8e"
)

// TestElythiaAPIDocMatchesRoutes checks the meta of elythia_apidoc.json
// (#4084) against the routes of the built server: each endpoint gets the
// same request from ten principals, and the answers must be the ones the
// declared requireCredential / requireAdmin / requireModerator /
// requiredRolePolicy / prohibitMoved / secure / kind give.
//
// 本文は`[]`にする。引数の検証はルートのmiddlewareの最後に走り、配列はどの
// paramDef(objectの型)にも通らず400で止まるので、handlerには届かない。
// 各ゲートを通ったかどうかは、400(引数の検証まで来た)か、ゲートの拒否の
// コードとidかで分かる。ソースを読んで突き合わせると、複数行の登録や
// 別の書き方を取りこぼす。
//
// 一般の利用者・移行済みの利用者・一般の管理者には、真偽のロールポリシーを
// すべてfalseにするロールを持たせる。既定でtrueのポリシー(canRequestCustomEmojis
// など)のゲートは、持たせないと誰でも通るので、宣言の漏れが見えない。
//
// レート制限に当たらないよう、リクエストごとにRedisを空にする。
func TestElythiaAPIDocMatchesRoutes(t *testing.T) {
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	docs, err := paramschema.ElythiaAPIDocs()
	require.NoError(t, err)
	// 抽出の下限。実在する宣言を名指しで要求する。
	require.Greater(t, len(docs.Meta), 80)
	require.True(t, docs.Meta["admin/backup/list"].Secure)
	require.Equal(t, "canManageCustomEmojis", docs.Meta["admin/emoji-application/list"].RequiredRolePolicy)

	db := serverIntegrationDB
	t.Setenv(config.EnvOnlyServer, "1")
	t.Setenv(config.EnvOnlyQueue, "")
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, rc.Close()) })
	clients := &cache.RedisClients{Default: rc, Pubsub: rc, JobQueue: rc, Timelines: rc, Reactions: rc}
	port, err := strconv.Atoi(mr.Port())
	require.NoError(t, err)
	ro := config.RedisOptions{Host: mr.Host(), Port: port}
	cfg := &config.Config{
		URL: "http://example.test", Host: "example.test", Hostname: "example.test",
		Scheme: "http", WsScheme: "ws", ID: "aidx",
		Redis: ro, RedisForPubsub: ro, RedisForJobQueue: ro, RedisForTimelines: ro, RedisForReactions: ro,
	}

	p := newAPIDocPrincipals(t, docs)
	srv, err := New(cfg, db, clients)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })

	post := func(name, token string) string {
		mr.FlushAll()
		req := httptest.NewRequest(http.MethodPost, "/api/"+name, bytes.NewReader([]byte(`[]`)))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		if token != "" {
			req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		var body struct {
			Error struct{ Code, ID string } `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return fmt.Sprintf("%d:%s:%s", rec.Code, body.Error.Code, body.Error.ID)
	}

	names := make([]string, 0, len(docs.Meta))
	for name := range docs.Meta {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		m := docs.Meta[name]
		check := func(who, token, want string) {
			t.Helper()
			got := post(name, token)
			assert.Equal(t, want, got, "/api/%s as %s: the meta %+v gives %s", name, who, m, want)
		}
		check("anonymous", "", apidocAnonymous(m))
		check("a user", p.user, apidocUser(m))
		check("a moved user", p.moved, apidocMoved(m))
		check("a moved administrator", p.movedAdmin, apidocMovedAdmin(m))
		check("a moderator", p.moderator, apidocModerator(m))
		if m.RequiredRolePolicy != "" {
			check("a user with "+m.RequiredRolePolicy, p.policy[apidocPolicyKey(m)], apidocPolicyHolder(m))
		}
		check("an administrator", p.admin, gatePassed)
		check("an app without permissions", p.appNone, apidocAppNone(m))
		check("an app with every permission", p.appAll, apidocAppAll(m))
		if m.Kind != "" {
			check("an app with "+m.Kind, p.appKind[m.Kind], apidocAppKind(m))
		}
	}
}

func apidocAnonymous(m paramschema.APIMeta) string {
	if m.RequireCredential {
		return gateNoCredential
	}
	return gatePassed
}

// apidocRole is the answer to a user without the role m requires.
func apidocRole(m paramschema.APIMeta) string {
	switch {
	case m.RequireAdmin:
		return gateNotAdmin
	case m.RequireModerator:
		return gateNotModerator
	case m.RequiredRolePolicy != "":
		return gateNoPolicy
	}
	return ""
}

func apidocUser(m paramschema.APIMeta) string {
	if r := apidocRole(m); r != "" {
		return r
	}
	return gatePassed
}

// apidocMoved expects the role gates before prohibitMoved, the order of the
// routes of Elythia; upstream checks prohibitMoved first.
//
// 両方を宣言したものは今は無い(docs/divergence/api.mdの#4084の節)。
func apidocMoved(m paramschema.APIMeta) string {
	if r := apidocRole(m); r != "" {
		return r
	}
	if m.ProhibitMoved {
		return gateMoved
	}
	return gatePassed
}

// apidocMovedAdmin sees prohibitMoved on the endpoints that require a role,
// where a moved user without the role is turned away by the role first.
func apidocMovedAdmin(m paramschema.APIMeta) string {
	if m.ProhibitMoved {
		return gateMoved
	}
	return gatePassed
}

func apidocModerator(m paramschema.APIMeta) string {
	switch {
	case m.RequireAdmin:
		return gateNotAdmin
	case m.RequiredRolePolicy != "":
		return gateNoPolicy
	}
	return gatePassed
}

// apidocPolicyHolder is the answer to a user with only the policy m
// requires, who is a moderator when m requires a moderator.
func apidocPolicyHolder(m paramschema.APIMeta) string {
	if m.RequireAdmin {
		return gateNotAdmin
	}
	return gatePassed
}

// The app tokens belong to the administrator, so only the gates of the
// token answer them.
//
// kindとsecureを両方持つもの(i/update-federation)は、権限のゲートが先に
// 断る。secureのゲートは、宣言したkindだけを持つアプリで確かめる。
func apidocAppNone(m paramschema.APIMeta) string {
	switch {
	case m.Kind != "":
		return gateAppPermission
	case m.Secure:
		return gateAppNotSecure
	case m.RequireCredential:
		// kindが無く資格情報を要するものは、本家と同じくアプリを一律で断る。
		return gateAppPermission
	}
	return gatePassed
}

func apidocAppAll(m paramschema.APIMeta) string {
	switch {
	case m.Secure:
		return gateAppNotSecure
	case m.Kind != "":
		return gatePassed
	case m.RequireCredential:
		return gateAppPermission
	}
	return gatePassed
}

func apidocAppKind(m paramschema.APIMeta) string {
	if m.Secure {
		return gateAppNotSecure
	}
	return gatePassed
}

// apidocPolicyKey tells apart the policy holders: the policy, and whether
// the holder is a moderator too.
func apidocPolicyKey(m paramschema.APIMeta) string {
	return fmt.Sprintf("%s/%t", m.RequiredRolePolicy, m.RequireModerator)
}

// apidocPrincipals are the tokens TestElythiaAPIDocMatchesRoutes sends.
type apidocPrincipals struct {
	user, moved, movedAdmin, moderator, admin string
	// policy are users with a role that has only the policy, by
	// apidocPolicyKey.
	policy          map[string]string
	appNone, appAll string
	appKind         map[string]string
}

func newAPIDocPrincipals(t *testing.T, docs *paramschema.ElythiaAPIDoc) apidocPrincipals {
	t.Helper()
	db := serverIntegrationDB
	cleanup := func() {
		require.NoError(t, db.Exec(`DELETE FROM access_token WHERE id LIKE 'apidoc-%'`).Error)
		require.NoError(t, db.Exec(`DELETE FROM role_assignment WHERE id LIKE 'apidoc-%'`).Error)
		require.NoError(t, db.Exec(`DELETE FROM role WHERE id LIKE 'apidoc-%'`).Error)
		require.NoError(t, db.Exec(`DELETE FROM "user" WHERE id LIKE 'apidoc-%'`).Error)
	}
	cleanup()
	t.Cleanup(cleanup)

	users := repository.NewUserRepository(db)
	n := 0
	newUser := func(admin bool, movedTo string) (id, token string) {
		n++
		id = fmt.Sprintf("apidoc-user-%d", n)
		token = fmt.Sprintf("apidoctoken%05d", n)
		name := fmt.Sprintf("apidoc_%d", n)
		u := &model.User{ID: id, Username: name, UsernameLower: name, Token: &token, IsRoot: admin}
		if movedTo != "" {
			u.MovedToURI = &movedTo
		}
		require.NoError(t, users.Create(u))
		return id, token
	}
	now := time.Now()
	assignRole := func(userID string, role model.Role) {
		n++
		role.ID = fmt.Sprintf("apidoc-role-%d", n)
		role.Name = role.ID
		role.UpdatedAt, role.LastUsedAt = now, now
		require.NoError(t, db.Create(&role).Error)
		require.NoError(t, db.Create(&model.RoleAssignment{ID: fmt.Sprintf("apidoc-assign-%d", n), UserID: userID, RoleID: role.ID}).Error)
	}

	// 真偽のポリシーをすべて、優先度2(その層だけで決まる)でfalseにする。
	denied := map[string]any{}
	for key, v := range effectivepolicy.Defaults() {
		if _, ok := v.(bool); ok {
			denied[key] = map[string]any{"useDefault": false, "priority": 2, "value": false}
		}
	}
	require.Contains(t, denied, "canRequestCustomEmojis")
	deniedPolicies, err := json.Marshal(denied)
	require.NoError(t, err)

	var p apidocPrincipals
	var userID, movedID, modID, adminID string
	userID, p.user = newUser(false, "")
	assignRole(userID, model.Role{Policies: deniedPolicies})
	movedID, p.moved = newUser(false, "https://remote.example/users/moved")
	assignRole(movedID, model.Role{Policies: deniedPolicies})
	_, p.movedAdmin = newUser(true, "https://remote.example/users/moved-admin")
	modID, p.moderator = newUser(false, "")
	assignRole(modID, model.Role{IsModerator: true, Policies: deniedPolicies})
	adminID, p.admin = newUser(true, "")

	p.policy = map[string]string{}
	p.appKind = map[string]string{}
	tokens := repository.NewAccessTokenRepository(db)
	newApp := func(perms ...string) string {
		n++
		id := fmt.Sprintf("apidoc-token-%d", n)
		token := "apidoc-app-" + strconv.Itoa(n)
		require.NoError(t, tokens.Create(&model.AccessToken{ID: id, Token: token, Hash: "hash-" + id, UserID: adminID, Permission: model.StringArray(perms)}))
		return token
	}
	p.appNone = newApp()
	p.appAll = newApp(permissions.All...)
	for _, m := range docs.Meta {
		if key := apidocPolicyKey(m); m.RequiredRolePolicy != "" && p.policy[key] == "" {
			var id string
			id, p.policy[key] = newUser(false, "")
			// 宣言したポリシーだけをtrueにし、ほかは拒むロールと同じくfalseにする。
			// ほかの既定でtrueのポリシーが残ると、ルートが宣言と別のポリシーを
			// 見ていても通ってしまう。一般の管理者にするのは、宣言が一般の管理者を
			// 求めるとき(admin/ip/*)だけにする。いつもそうすると、宣言に無い
			// 一般の管理者のゲートがポリシーのゲートの後ろにあっても通ってしまう。
			only := maps.Clone(denied)
			only[m.RequiredRolePolicy] = map[string]any{"useDefault": false, "priority": 2, "value": true}
			policies, err := json.Marshal(only)
			require.NoError(t, err)
			assignRole(id, model.Role{IsModerator: m.RequireModerator, Policies: policies})
		}
		if m.Kind != "" && p.appKind[m.Kind] == "" {
			p.appKind[m.Kind] = newApp(m.Kind)
		}
	}
	return p
}
