// notification_stream_test.go: 連合経由で届いた通知が、受け取り側の `main`
// チャンネルへストリーミング配信されるかを見る (#3132 の調査で追加)。
//
// ローカル同士のリアクションは単体 / API テストで押さえてあるが、**リモートの
// Like activity が inbox 経由で入る経路**だけは実サーバー 2 台を立てないと
// 通らない。通知行自体は Redis に残るので、ここが壊れると「通知欄には出るが
// リアルタイムには出ない」という外から切り分けにくい形になる。
package e2e_federation

import (
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFederation_RemoteReactionNotificationStreaming(t *testing.T) {
	resetDB(t, serverA)
	resetDB(t, serverB)

	alice := signup(t, serverA, "alice", nil)
	bob := signup(t, serverB, "bob", nil)

	noteResult := srvAPICall(t, serverA, "notes/create", map[string]any{
		"i":    alice.Token,
		"text": "reaction target",
	})
	createdNote := noteResult["createdNote"].(map[string]any)
	noteID := createdNote["id"].(string)

	// bob 側で alice のノートを解決してリモート note 行を作る。
	resolved := resolveRemoteNote(t, serverB, noteURI(serverA, noteID), bob)
	remoteNoteID := resolved["id"].(string)

	// alice が main チャンネルを購読してからリアクションさせる。
	stream := connectChannel(t, serverA, alice, "main", nil)
	time.Sleep(500 * time.Millisecond)

	// reactions/create は 204 を返すので srvAPICall (JSON 前提) は使えない。
	resp := srvAPIPost(t, serverB, "notes/reactions/create", map[string]any{
		"i":        bob.Token,
		"noteId":   remoteNoteID,
		"reaction": "👍",
	})
	require.Less(t, resp.StatusCode, 300, "reaction should succeed")
	require.NoError(t, resp.Body.Close())

	msg := stream.WaitForEvent("notification", 15*time.Second)
	var body struct {
		Type     string `json:"type"`
		Reaction string `json:"reaction"`
		User     struct {
			Username string  `json:"username"`
			Host     *string `json:"host"`
		} `json:"user"`
		Note *struct {
			ID string `json:"id"`
		} `json:"note"`
	}
	require.NoError(t, json.Unmarshal(msg.Body, &body))
	assert.Equal(t, "reaction", body.Type)
	assert.Equal(t, "👍", body.Reaction)
	assert.Equal(t, "bob", body.User.Username)
	require.NotNil(t, body.User.Host, "remote notifier must carry host")
	require.NotNil(t, body.Note, "reaction notification must embed the note")
	assert.Equal(t, noteID, body.Note.ID)
}

// TestFederation_RemoteFollowAcceptedNotificationStreaming は、リモートの鍵
// アカウントがフォローリクエストを承認したとき、申請した側 (ローカル) に
// `followRequestAccepted` がストリーミングで届くかを見る。
//
// この経路は inbox の Accept 経由でしか通らない。ローカル同士の承認は
// `following/requests/accept` が直接 AcceptRequest を呼ぶので、そちらが通って
// いてもリモート承認が壊れていることはありうる。
func TestFederation_RemoteFollowAcceptedNotificationStreaming(t *testing.T) {
	resetDB(t, serverA)
	resetDB(t, serverB)

	alice := signup(t, serverA, "alice", nil)
	bob := signup(t, serverB, "bob", nil)

	// bob を鍵アカウントにして承認を挟ませる。
	srvAPICall(t, serverB, "i/update", map[string]any{
		"i":        bob.Token,
		"isLocked": true,
	})

	// alice 側で bob を解決してからフォローする。
	remoteBob := resolveRemoteUser(t, serverA, userURI(serverB, bob.ID), alice)
	remoteBobID := remoteBob["id"].(string)

	stream := connectChannel(t, serverA, alice, "main", nil)
	time.Sleep(500 * time.Millisecond)

	srvAPICall(t, serverA, "following/create", map[string]any{
		"i":      alice.Token,
		"userId": remoteBobID,
	})

	// bob 側に届いた申請を承認する。alice は bob から見ればリモート。
	remoteAlice := resolveRemoteUser(t, serverB, userURI(serverA, alice.ID), bob)
	resp := srvAPIPost(t, serverB, "following/requests/accept", map[string]any{
		"i":      bob.Token,
		"userId": remoteAlice["id"].(string),
	})
	// inbox は enqueue して 202 を返すので、Follow の処理完了を待ってから
	// 承認する。
	var lastBody string
	for i := 0; i < 30; i++ {
		if resp.StatusCode < 300 {
			break
		}
		b, _ := io.ReadAll(resp.Body)
		lastBody = string(b)
		_ = resp.Body.Close()
		time.Sleep(300 * time.Millisecond)
		resp = srvAPIPost(t, serverB, "following/requests/accept", map[string]any{
			"i":      bob.Token,
			"userId": remoteAlice["id"].(string),
		})
	}
	require.Less(t, resp.StatusCode, 300, "accept should succeed (last error: %s)", lastBody)
	require.NoError(t, resp.Body.Close())

	msg := stream.WaitForEvent("notification", 15*time.Second)
	var body struct {
		Type string `json:"type"`
		User struct {
			Username string  `json:"username"`
			Host     *string `json:"host"`
		} `json:"user"`
	}
	require.NoError(t, json.Unmarshal(msg.Body, &body))
	assert.Equal(t, "followRequestAccepted", body.Type)
	assert.Equal(t, "bob", body.User.Username)
}
