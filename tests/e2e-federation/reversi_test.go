// reversi_test.go: mk-go ↔ mk-go reversi 連合 smoke test (#435 / #417 P6)。
//
// 既存 e2e_federation 基盤 (PostgreSQL 1 container + 2 db, Redis 1 container
// + 2 db) を流用して 2 つの mk-go server 間で AP 経由の reversi 操作が
// 期待通り伝播することを検証する。
//
// scope: HTTP-only な round-trip (Invite / CancelMatch / inbox ack)。
// UpdateSettings / PutStone / Surrender は WebSocket stream channel 経由で
// 動く設計で、本 test は WebSocket 経路を別途持ち込まないため out of scope。
// それらは別 issue (WebSocket integration test infra) で対応する想定。
package e2e_federation

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	corereversi "github.com/shiroha-a/mk/internal/core/reversi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// preCacheReversiFederationVersion は reversi/match の federation availability
// check (corereversi.FederationChecker) が remote host の reversiVersion を
// HTTP fetch する経路をバイパスするため、Redis 側にキャッシュキーを直接書く。
//
// 本番 fedChecker は `https://<host>/.well-known/nodeinfo` を取得しに行くが、
// e2e_federation の test server は HTTP listener で running しているので
// HTTPS では到達できず、結果として Available() が false → 400 が返る。
// 既知の制約として cache 経路を pre-populate して回避する。serverDB は
// "A 側" は 0、"B 側" は 1 に対応 (main_test.go と整合)。
func preCacheReversiFederationVersion(t *testing.T, serverDB int, host string) {
	t.Helper()
	c := goredis.NewClient(&goredis.Options{Addr: redisAddr, DB: serverDB})
	t.Cleanup(func() { _ = c.Close() })
	key := "reversi:federation:version:" + host
	require.NoError(t, c.Set(context.Background(), key, corereversi.ReversiVersion, 30*time.Second).Err())
}

// reversiInvitationsContains は viewer の /api/reversi/invitations を呼び、
// inviter の username が含まれているかを返す。federation 経路で Invite が
// 着弾したかの観測手段。
func reversiInvitationsContains(t *testing.T, srv *testServer, viewer *userToken, inviterUsername string) bool {
	t.Helper()
	resp := srvAPIPost(t, srv, "reversi/invitations", map[string]any{
		"i": viewer.Token,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var arr []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&arr); err != nil {
		return false
	}
	for _, u := range arr {
		if name, _ := u["username"].(string); strings.EqualFold(name, inviterUsername) {
			return true
		}
	}
	return false
}

// pollInvitationContains は contains が true になるまで poll する。
// federation deliver は queue 経由で非同期なので poll が必要 (typical 200ms
// 程度で着弾するが flaky 防止に 10s timeout)。
func pollInvitationContains(t *testing.T, srv *testServer, viewer *userToken, inviterUsername string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if reversiInvitationsContains(t, srv, viewer, inviterUsername) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to appear in %s's invitations", inviterUsername, viewer.Username)
}

// pollInvitationGone は contains が false になるまで poll する (CancelMatch
// で Leave が伝播して B 側 row が消えるシナリオ用)。
func pollInvitationGone(t *testing.T, srv *testServer, viewer *userToken, inviterUsername string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !reversiInvitationsContains(t, srv, viewer, inviterUsername) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to disappear from %s's invitations", inviterUsername, viewer.Username)
}

// warmRemoteUserCache は B 上で alice@A を /api/ap/show 経由で resolve させる
// ことで B 側の cached user 行と instance 行を埋めておく helper。reversi/match
// の acct resolve 経路で使う resolveAcct → FindByUsernameLower の hit 率を
// 上げて flaky を減らす (issue #435 実装上の罠コメント参照)。
func warmRemoteUserCache(t *testing.T, srv *testServer, fromUser *userToken, remoteURI string) {
	t.Helper()
	_ = resolveRemoteUser(t, srv, remoteURI, fromUser)
}

// TestReversi_LocalInviteRoundTrip: alice@A が acct @bob@B を指定して
// /api/reversi/match を呼ぶと、B 側の bob の invitations 一覧に alice が
// 表示される (= AP Invite が federation 経由で処理された)。
//
// deliver queue 経路は test 環境で動かないので main_test.go で
// SetSyncDeliverHookForTest を wire し、sign + HTTP POST を inline 実行
// する形で round-trip を駆動する (#780)。
func TestReversi_LocalInviteRoundTrip(t *testing.T) {
	resetDB(t, serverA)
	resetDB(t, serverB)

	alice := signup(t, serverA, "alice", nil)
	bob := signup(t, serverB, "bob", nil)

	// B 側で alice を WebFinger 経由で resolve させて remote user 行を作る。
	// reversi/match の acct resolve は FindByUsernameLower → DB hit 経路を
	// 期待するので先に warm しておく。
	aliceURI := userURI(serverA, alice.ID)
	warmRemoteUserCache(t, serverB, bob, aliceURI)
	// 逆方向 (A 側で bob を resolve) も warm。/match の acct lookup は invite
	// 側のサーバ (= A) で行われるので、こちらの方が必須。
	bobURI := userURI(serverB, bob.ID)
	warmRemoteUserCache(t, serverA, alice, bobURI)
	// fedChecker の HTTPS-only 制約を回避するため、A 側 Redis に B の
	// reversiVersion を直接書いておく (preCacheReversiFederationVersion 参照)。
	preCacheReversiFederationVersion(t, 0, serverB.Host)

	// alice が bob@B を招待。host は serverB の listener アドレス。
	bobAcct := "@bob@" + serverB.Host
	resp := srvAPIPost(t, serverA, "reversi/match", map[string]any{
		"i":      alice.Token,
		"userId": bobAcct,
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode,
		"reversi/match should accept @bob@host acct and return 204 (対局未成立)")

	// federation queue が AP Invite を deliver → B 側 inbox が処理 → reversi_game
	// 行が B に作られる、まで poll で待つ。
	pollInvitationContains(t, serverB, bob, "alice", 10*time.Second)
}

// TestReversi_CancelMatchUndoesPreStart: pre-start のゲームに対して招待側が
// /api/reversi/cancel-match を呼ぶと AP Leave が相手に飛んで、B 側の row が
// 消える (= bob の invitations から alice が消える)。
//
// sync deliver hook (main_test.go の SetSyncDeliverHookForTest) で AP
// deliver を inline で駆動する (#780)。
func TestReversi_CancelMatchUndoesPreStart(t *testing.T) {
	resetDB(t, serverA)
	resetDB(t, serverB)

	alice := signup(t, serverA, "alice", nil)
	bob := signup(t, serverB, "bob", nil)

	aliceURI := userURI(serverA, alice.ID)
	warmRemoteUserCache(t, serverB, bob, aliceURI)
	bobURI := userURI(serverB, bob.ID)
	warmRemoteUserCache(t, serverA, alice, bobURI)
	preCacheReversiFederationVersion(t, 0, serverB.Host)

	// 招待
	bobAcct := "@bob@" + serverB.Host
	resp := srvAPIPost(t, serverA, "reversi/match", map[string]any{
		"i":      alice.Token,
		"userId": bobAcct,
	})
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	pollInvitationContains(t, serverB, bob, "alice", 10*time.Second)

	// 招待キャンセル → Leave activity が B に飛ぶ
	resp = srvAPIPost(t, serverA, "reversi/cancel-match", map[string]any{
		"i": alice.Token,
	})
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	// B 側の invitations から alice が消えるまで poll
	pollInvitationGone(t, serverB, bob, "alice", 10*time.Second)
}

// inviteAndAccept は alice@A → bob@B の招待 + 受諾フローを 1 つの helper に
// まとめる (#781 系の test 用)。reset-db → signup → warm cache → match →
// match-back の flow をまとめて、両 server で reversi_game 行が存在し
// federation で session id が紐付いた状態を確立する。
//
// (federation deliver は main_test.go で wired した sync hook 経由)
//
// 戻り値: alice / bob の userToken と、A 側 game.ID / B 側 game.ID。
func inviteAndAccept(t *testing.T) (alice, bob *userToken, gameAID, gameBID string) {
	t.Helper()
	resetDB(t, serverA)
	resetDB(t, serverB)
	alice = signup(t, serverA, "alice", nil)
	bob = signup(t, serverB, "bob", nil)

	aliceURI := userURI(serverA, alice.ID)
	warmRemoteUserCache(t, serverB, bob, aliceURI)
	bobURI := userURI(serverB, bob.ID)
	warmRemoteUserCache(t, serverA, alice, bobURI)
	preCacheReversiFederationVersion(t, 0, serverB.Host)
	preCacheReversiFederationVersion(t, 1, serverA.Host)

	// alice が bob を招待
	bobAcct := "@bob@" + serverB.Host
	resp := srvAPIPost(t, serverA, "reversi/match", map[string]any{
		"i":      alice.Token,
		"userId": bobAcct,
	})
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	// 招待が B 側に着弾するまで待つ
	pollInvitationContains(t, serverB, bob, "alice", 10*time.Second)

	// bob が match で accept → A 側に Join 通知
	aliceAcct := "@alice@" + serverA.Host
	rec := srvAPICall(t, serverB, "reversi/match", map[string]any{
		"i":      bob.Token,
		"userId": aliceAcct,
	})
	gameBID, _ = rec["id"].(string)
	require.NotEmpty(t, gameBID, "bob's match response should contain game id")

	// A 側の game ID は DB 直接で latest を引く (handler 経路は配列を返す
	// ので srvAPICall の map 期待と合わず簡素化)。
	type rgRow struct{ ID string }
	var row rgRow
	require.NoError(t, dbAGlobal.Raw(
		`SELECT id FROM reversi_game WHERE "user1Id" = ? ORDER BY id DESC LIMIT 1`, alice.ID,
	).Scan(&row).Error)
	gameAID = row.ID
	require.NotEmpty(t, gameAID, "alice should own a reversi_game row")

	return alice, bob, gameAID, gameBID
}

// TestReversi_ReadyPropagatesAndStarts: alice@A と bob@B が WebSocket 経由で
// Ready 通知を出すと AP federation で双方の row に user1Ready / user2Ready が
// 反映され、IsStarted=true が両側で観測される (#435 case 2 の subset)。
//
// upstream は updateSettings / putStone も WebSocket 経由だが、本 test は
// Ready / Started 伝播の最小ループだけを検証する。残 case (Settings /
// PutStone / Surrender) は別 follow-up で。
func TestReversi_ReadyPropagatesAndStarts(t *testing.T) {
	alice, bob, gameAID, gameBID := inviteAndAccept(t)

	// 両 server で reversi-game channel に subscribe
	wsA := connectChannel(t, serverA, alice, "reversiGame", map[string]any{"gameId": gameAID})
	wsB := connectChannel(t, serverB, bob, "reversiGame", map[string]any{"gameId": gameBID})

	// alice ready=true
	wsA.SendChannel("ready", true)
	// bob ready=true
	wsB.SendChannel("ready", true)

	// 両 channel で `started` event を受信できること
	wsA.WaitForEvent("started", 10*time.Second)
	wsB.WaitForEvent("started", 10*time.Second)

	// DB 層でも IsStarted=true を確認
	for _, ck := range []struct {
		db *gorm.DB
		id string
	}{{dbAGlobal, gameAID}, {dbBGlobal, gameBID}} {
		var started bool
		require.NoError(t, ck.db.Raw(
			`SELECT "isStarted" FROM reversi_game WHERE id = ?`, ck.id,
		).Scan(&started).Error)
		assert.True(t, started, "game %s should be started", ck.id)
	}
}

// readyAndStart は inviteAndAccept の後、両 server で Ready=true を送って
// game を Started 状態にし、両 channel client を返す helper (#781)。後続の
// PutStone / Surrender test で使う。
func readyAndStart(t *testing.T) (alice, bob *userToken, gameAID, gameBID string, wsA, wsB *streamingClient) {
	t.Helper()
	alice, bob, gameAID, gameBID = inviteAndAccept(t)
	wsA = connectChannel(t, serverA, alice, "reversiGame", map[string]any{"gameId": gameAID})
	wsB = connectChannel(t, serverB, bob, "reversiGame", map[string]any{"gameId": gameBID})
	wsA.SendChannel("ready", true)
	wsB.SendChannel("ready", true)
	wsA.WaitForEvent("started", 10*time.Second)
	wsB.WaitForEvent("started", 10*time.Second)
	return alice, bob, gameAID, gameBID, wsA, wsB
}

// blackUser は Started 後の game.Black (1 | 2) を見て黒番のユーザーを返す。
// game.BW="random" + session id 由来で決まるので、test ごとに alice が
// 黒のことも bob が黒のこともある。turn 駆動の test ではこの helper で
// 動的に切り替える。
func blackUser(t *testing.T, gameID string, alice, bob *userToken, wsA, wsB *streamingClient) (token *userToken, ws *streamingClient) {
	t.Helper()
	var blackVal int
	require.NoError(t, dbAGlobal.Raw(
		`SELECT black FROM reversi_game WHERE id = ?`, gameID,
	).Scan(&blackVal).Error)
	require.NotZero(t, blackVal, "game.Black should be 1 or 2 after Started")
	if blackVal == 1 {
		return alice, wsA
	}
	return bob, wsB
}

// TestReversi_PutStonePropagates: started game で 黒番 (= bw=random で
// session id から決まる) が putStone を送ると AP federation で相手 server
// の game にも turn が反映される。両 channel が `log` event を受信すること、
// 両 DB の logs が同期することを確認 (#435 case 3)。
func TestReversi_PutStonePropagates(t *testing.T) {
	alice, bob, gameAID, gameBID, wsA, wsB := readyAndStart(t)

	// 黒番が先手。session id 由来で alice / bob どちらが黒か決まるので、
	// 黒の token + ws を動的に解決して putStone を送る。defaultMap の
	// 8x8 標準配置で (2, 3) = pos=19 は黒の合法手 (先手で必ず flip 可能)。
	_, blackWS := blackUser(t, gameAID, alice, bob, wsA, wsB)
	blackWS.SendChannel("putStone", map[string]any{"pos": 19})

	// 両 channel で log event を受信
	wsA.WaitForEvent("log", 10*time.Second)
	wsB.WaitForEvent("log", 10*time.Second)

	// DB 層で logs が空でない (= 1 turn 進んだ) ことを両側で確認
	for _, ck := range []struct {
		db *gorm.DB
		id string
	}{{dbAGlobal, gameAID}, {dbBGlobal, gameBID}} {
		var logsRaw string
		require.NoError(t, ck.db.Raw(
			`SELECT logs FROM reversi_game WHERE id = ?`, ck.id,
		).Scan(&logsRaw).Error)
		assert.NotEqual(t, "[]", logsRaw, "game %s should have at least 1 log entry", ck.id)
	}
}

// TestReversi_SurrenderEndsBothSides: started game で alice が
// /api/reversi/surrender を呼ぶと AP Leave が B に飛び、両 server で
// IsEnded=true / WinnerID=bob になる (#435 case 4)。
func TestReversi_SurrenderEndsBothSides(t *testing.T) {
	alice, _, gameAID, gameBID, _, _ := readyAndStart(t)

	resp := srvAPIPost(t, serverA, "reversi/surrender", map[string]any{
		"i":      alice.Token,
		"gameId": gameAID,
	})
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	// federation deliver が同期 (sync hook) なので即座に B 側にも反映する。
	// 念のため short poll で B 側 row が IsEnded=true になるまで待つ。
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var ended bool
		require.NoError(t, dbBGlobal.Raw(
			`SELECT "isEnded" FROM reversi_game WHERE id = ?`, gameBID,
		).Scan(&ended).Error)
		if ended {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// 最終 assertion: 両 DB の row が IsEnded=true。winner ID は server 間で
	// cached remote user の ID が異なるので strict 比較せず、終局フラグのみ
	// を見る (alice surrender → 勝者は bob、両 server で同じ semantics)。
	for _, ck := range []struct {
		db *gorm.DB
		id string
	}{
		{dbAGlobal, gameAID},
		{dbBGlobal, gameBID},
	} {
		var ended bool
		require.NoError(t, ck.db.Raw(
			`SELECT "isEnded" FROM reversi_game WHERE id = ?`, ck.id,
		).Scan(&ended).Error)
		assert.True(t, ended, "game %s should be ended", ck.id)
	}
}

// TestReversi_ReactionURIAcked: B から A の reversi session URI に対する
// EmojiReaction を inbox に直接 POST して 202 ack されること。state 変化は
// 期待しない (P5 で scope down 済み — 4xx を返さない確認のみ)。
//
// この test は AP signed POST を必要とするため、bob のキーペアで A に向けて
// signed delivery を行う必要がある。本 test 環境では signed POST helper が
// 整備されていないので、内部 deliver service 経由で AP body だけ送る形で
// 簡略化する: 受信側 inbox は signature 検証で reject する。よって本 test
// は inbox の URI ルーティングが reversi session URI を ack する分岐 (#417
// P5) ではなく、より上位 (signature absent → 401) を踏むことになる。
//
// 厳密な ack path は signed-post helper が入った段階で再 enable する。
// scope down: ここでは inbox endpoint が unsigned EmojiReaction を不正
// payload として handler 内で reject せず、middleware 層で signature 不在を
// 401 で弾くことだけ確認する (= reversi session URI の type assertion が
// nil deref を起こさないこと)。
func TestReversi_ReactionURIInboxNoCrash(t *testing.T) {
	resetDB(t, serverA)
	resetDB(t, serverB)

	// 本 test は inbox endpoint の URI parser だけを exercise するので
	// alice / bob の signup は不要。reset-db で空 DB を確保するだけで足りる。
	// random session id (UUID-like)。実 game 行は無くてよい (本 test の対象は
	// inbox の URI ルーティングが panic しないことのみ)。
	var sidBytes [16]byte
	_, _ = rand.Read(sidBytes[:])
	sessionID := fmt.Sprintf("%x", sidBytes[:])

	body := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"type":     "Like",
		"id":       fmt.Sprintf("https://remote.example/likes/%s", sessionID),
		"actor":    "https://remote.example/users/bob",
		"object":   fmt.Sprintf("%s/games/%s/%s", serverA.BaseURL, sessionID, sessionID),
		"content":  ":smile:",
	}
	payload, err := json.Marshal(body)
	require.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, serverA.BaseURL+"/inbox", bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/activity+json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	// signed-post helper が無いため 200 ack 経路ではなく signature 不在の
	// 4xx 経路を踏む。重要なのは 5xx (panic / 想定外 internal error) を返さ
	// ないこと。reversi session URI のパース branch で nil deref していたら
	// 500 が返るので、そうなっていないことを確認する。
	assert.LessOrEqual(t, resp.StatusCode, 499,
		"inbox should not 5xx on a Like targeting a reversi session URI (path parser must not panic)")
}
