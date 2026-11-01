package e2e_federation

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// followRemoteAndWait makes follower (on followerSrv) follow target (on
// targetSrv) and waits until the follow is established on followerSrv, i.e.
// the Accept arrived.
func followRemoteAndWait(t *testing.T, followerSrv, targetSrv *testServer, follower, target *userToken) {
	t.Helper()
	remote := resolveRemoteUser(t, followerSrv, userURI(targetSrv, target.ID), follower)
	remoteID := remote["id"].(string)
	srvAPICall(t, followerSrv, "following/create", map[string]any{
		"i":      follower.Token,
		"userId": remoteID,
	})
	// 条件の関数は別の goroutine で動くので、その中で require を使わない
	// (srvAPICall は失敗を require で落とす)。
	require.Eventually(t, func() bool {
		resp := srvAPIPost(t, followerSrv, "users/show", map[string]any{
			"i":      follower.Token,
			"userId": remoteID,
		})
		defer resp.Body.Close()
		var u struct {
			IsFollowing bool `json:"isFollowing"`
		}
		return resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&u) == nil && u.IsFollowing
	}, 15*time.Second, 200*time.Millisecond, "the follow is not established")
}

// A follower on another Elythia can look up a followers-only note it has not
// received (#3913): server B fetches it with the instance actor (refused), then
// with the follower's key (#3915), and server A answers the signed GET because
// the signer follows the author (#3914). A user on B who does not follow the
// author gets the note hidden.
func TestFederation_FollowersOnlyNoteLookupByFollower(t *testing.T) {
	resetDB(t, serverA)
	resetDB(t, serverB)

	alice := signup(t, serverA, "alice", nil)
	bob := signup(t, serverB, "bob", nil)
	carol := signup(t, serverB, "carol", bob)

	// フォローする前に作るので、B には配送されていない。
	created := srvAPICall(t, serverA, "notes/create", map[string]any{
		"i":          alice.Token,
		"text":       "for followers",
		"visibility": "followers",
	})
	uri := noteURI(serverA, created["createdNote"].(map[string]any)["id"].(string))

	// 署名の無い GET には返さない。
	req, err := http.NewRequest(http.MethodGet, uri, nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "application/activity+json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	followRemoteAndWait(t, serverB, serverA, bob, alice)

	obj := resolveRemoteNote(t, serverB, uri, bob)
	assert.Equal(t, "for followers", obj["text"])
	assert.Equal(t, "followers", obj["visibility"])

	// carol は A の誰もフォローしていない。取り込み済みの投稿が返るが、本文は隠れる。
	result := srvAPICall(t, serverB, "ap/show", map[string]any{
		"i":   carol.Token,
		"uri": uri,
	})
	require.Equal(t, "Note", result["type"])
	hidden := result["object"].(map[string]any)
	assert.Nil(t, hidden["text"])
	assert.Equal(t, true, hidden["isHidden"])
}

// A locked account's renote of its own followers-only note that the receiving
// server has never seen is ingested (#3913): server B retries the Announce
// target with a local follower's key (#3916), and server A answers it (#3914).
func TestFederation_OwnFollowersOnlyRenoteOfUnseenNote(t *testing.T) {
	resetDB(t, serverA)
	resetDB(t, serverB)

	alice := signup(t, serverA, "alice", nil)
	bob := signup(t, serverB, "bob", nil)

	created := srvAPICall(t, serverA, "notes/create", map[string]any{
		"i":          alice.Token,
		"text":       "old followers note",
		"visibility": "followers",
	})
	targetID := created["createdNote"].(map[string]any)["id"].(string)
	targetURI := noteURI(serverA, targetID)

	followRemoteAndWait(t, serverB, serverA, bob, alice)

	srvAPICall(t, serverA, "notes/create", map[string]any{
		"i":          alice.Token,
		"renoteId":   targetID,
		"visibility": "followers",
	})

	// 条件の関数は別の goroutine で動くので、その中で require を使わない。
	require.Eventually(t, func() bool {
		var n int64
		err := dbBGlobal.Raw(
			`SELECT count(*) FROM note r JOIN note t ON r."renoteId" = t.id WHERE t.uri = ?`, targetURI).Scan(&n).Error
		return err == nil && n == 1
	}, 15*time.Second, 200*time.Millisecond, "the renote of the unseen followers-only note is not ingested on B")

	var visibility string
	require.NoError(t, dbBGlobal.Raw(`SELECT visibility FROM note WHERE uri = ?`, targetURI).Scan(&visibility).Error)
	assert.Equal(t, "followers", visibility)
}
