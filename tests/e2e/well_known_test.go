// well_known_test.go ports packages/backend/test/e2e/well-known.ts
package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWellKnown(t *testing.T) {
	// TS版 beforeAll: signup alice + admin/update-meta federation=all
	resetDB(t)
	alice := signup(t, "alice", nil) // 初回=admin
	resp := apiPost(t, "admin/update-meta", map[string]any{
		"i":          alice.Token,
		"federation": "all",
	})
	resp.Body.Close()
	require.True(t, resp.StatusCode >= 200 && resp.StatusCode < 300,
		"admin/update-meta failed: %d", resp.StatusCode)

	t.Run("nodeinfo", func(t *testing.T) {
		resp := httpGet(t, ".well-known/nodeinfo")
		defer resp.Body.Close()
		assert.True(t, resp.StatusCode >= 200 && resp.StatusCode < 300)
		assert.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))

		body := readJSON(t, resp)
		links, ok := body["links"].([]any)
		require.True(t, ok, "links should be an array")
		require.Len(t, links, 2)

		link0 := links[0].(map[string]any)
		assert.Equal(t, "http://nodeinfo.diaspora.software/ns/schema/2.1", link0["rel"])
		assert.Equal(t, origin+"/nodeinfo/2.1", link0["href"])

		link1 := links[1].(map[string]any)
		assert.Equal(t, "http://nodeinfo.diaspora.software/ns/schema/2.0", link1["rel"])
		assert.Equal(t, origin+"/nodeinfo/2.0", link1["href"])
	})

	t.Run("webfinger", func(t *testing.T) {
		// OPTIONS preflight
		preflight := httpFetch(t, "OPTIONS",
			".well-known/webfinger?resource=acct:alice@"+host,
			map[string]string{
				"Access-Control-Request-Method": "GET",
				"Origin":                        "http://example.com",
			})
		defer preflight.Body.Close()
		assert.True(t, preflight.StatusCode >= 200 && preflight.StatusCode < 300)
		assert.Contains(t, preflight.Header.Get("Access-Control-Allow-Headers"), "Accept")

		// GET
		resp := httpGet(t, ".well-known/webfinger?resource=acct:alice@"+host)
		defer resp.Body.Close()
		assert.True(t, resp.StatusCode >= 200 && resp.StatusCode < 300)
		assert.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))
		assert.Equal(t, "Vary", resp.Header.Get("Access-Control-Expose-Headers"))
		assert.Equal(t, "Accept", resp.Header.Get("Vary"))

		body := readJSON(t, resp)
		assert.Equal(t, "acct:alice@"+host, body["subject"])

		links, ok := body["links"].([]any)
		require.True(t, ok)
		require.Len(t, links, 3)

		self := links[0].(map[string]any)
		assert.Equal(t, "self", self["rel"])
		assert.Equal(t, "application/activity+json", self["type"])
		assert.Equal(t, origin+"/users/"+alice.ID, self["href"])

		profile := links[1].(map[string]any)
		assert.Equal(t, "http://webfinger.net/rel/profile-page", profile["rel"])
		assert.Equal(t, "text/html", profile["type"])
		assert.Equal(t, origin+"/@alice", profile["href"])

		subscribe := links[2].(map[string]any)
		assert.Equal(t, "http://ostatus.org/schema/1.0/subscribe", subscribe["rel"])
		assert.Equal(t, origin+"/authorize-follow?acct={uri}", subscribe["template"])
	})

	t.Run("host-meta", func(t *testing.T) {
		resp := httpGet(t, ".well-known/host-meta")
		defer resp.Body.Close()
		assert.True(t, resp.StatusCode >= 200 && resp.StatusCode < 300)
		assert.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))
	})

	t.Run("host-meta.json", func(t *testing.T) {
		resp := httpGet(t, ".well-known/host-meta.json")
		defer resp.Body.Close()
		assert.True(t, resp.StatusCode >= 200 && resp.StatusCode < 300)
		assert.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))

		body := readJSON(t, resp)
		links, ok := body["links"].([]any)
		require.True(t, ok)
		require.Len(t, links, 1)

		link := links[0].(map[string]any)
		assert.Equal(t, "lrdd", link["rel"])
		assert.Equal(t, "application/jrd+json", link["type"])
		assert.Equal(t, origin+"/.well-known/webfinger?resource={uri}", link["template"])
	})

	t.Run("oauth-authorization-server", func(t *testing.T) {
		resp := httpGet(t, ".well-known/oauth-authorization-server")
		defer resp.Body.Close()
		assert.True(t, resp.StatusCode >= 200 && resp.StatusCode < 300)
		assert.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))

		body := readJSON(t, resp)
		assert.Equal(t, origin, body["issuer"])
		assert.Equal(t, origin+"/oauth/authorize", body["authorization_endpoint"])
		assert.Equal(t, origin+"/oauth/token", body["token_endpoint"])
	})

	// upstream PR 17901: W3C "A Well-Known URL for Changing Passwords"。
	// router に登録されていないと OPTIONS /.well-known/* のワイルドカードに
	// 引っかかって 405 になるので、ここで配線ごと固定する。
	t.Run("change-password", func(t *testing.T) {
		// パスワードマネージャは Location を読むだけでリダイレクトは追わない。
		client := &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		req, err := http.NewRequest(http.MethodGet, baseURL+"/.well-known/change-password", nil)
		require.NoError(t, err)
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()

		assert.Equal(t, http.StatusFound, resp.StatusCode)
		assert.Equal(t, origin+"/settings/security", resp.Header.Get("Location"))
		// global CORS middleware は `/.well-known/` を除外しているので、handler が
		// 付けないとゼロになる。他の discovery endpoint と揃っていることを見る。
		assert.Equal(t, "*", resp.Header.Get("Access-Control-Allow-Origin"))
	})
}
