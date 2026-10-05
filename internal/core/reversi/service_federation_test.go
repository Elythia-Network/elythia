package reversi

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// --- test doubles ---

type captureDeliverer struct {
	mu    sync.Mutex
	calls []deliverCall
}

type deliverCall struct {
	signerUserID string
	recipientID  string
	body         []byte
}

func (d *captureDeliverer) DeliverToUser(signerUserID string, recipient *model.User, body []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, deliverCall{signerUserID: signerUserID, recipientID: recipient.ID, body: append([]byte(nil), body...)})
	return nil
}

type stubUserLookup struct {
	users map[string]*model.User
}

func (s *stubUserLookup) FindByID(id string) (*model.User, error) {
	if u, ok := s.users[id]; ok {
		return u, nil
	}
	return nil, ErrGameNotFound // reuse for simplicity
}

// wireFederated sets up a Service with a federation cache seeded with the
// given session↔game mapping, plus a local "alice" user and a remote "bob"
// user. Returns the deliverer so tests can assert on calls. Uses the shared
// reversiTestRedis (testcontainers) from service_redis_test.go's TestMain.
func wireFederated(t *testing.T, svc *Service, game *model.ReversiGame, sessionID string) *captureDeliverer {
	t.Helper()
	// 既存の reversiTestRedis を流用。FlushAll は caller が pendingGame 側で
	// 済ませるので、ここでは追加 flush しない (同じ testcontainers client)。
	fedCache := NewFederationIDCache(reversiTestRedis.Client)
	fedCache.Set(context.Background(), sessionID, game.ID)
	svc.SetFederationCache(fedCache)

	remoteHost := "remote.example"
	remoteURI := "https://remote.example/users/bob"
	users := &stubUserLookup{users: map[string]*model.User{
		"alice": {ID: "alice", Username: "alice"},
		"bob":   {ID: "bob", Username: "bob", Host: &remoteHost, URI: &remoteURI},
	}}
	svc.SetUserRepo(users)
	svc.SetBaseURL("https://local.example")

	d := &captureDeliverer{}
	svc.SetFederationDeliverer(d)
	return d
}

// --- tests ---

func TestService_UpdateReady_DeliversReadyStatesUpdateToRemote(t *testing.T) {
	game, _, _, svc := newPendingGame(t)
	d := wireFederated(t, svc, game, "sess-ready")

	require.NoError(t, svc.UpdateReady(context.Background(), game.ID, "alice", true))

	require.Len(t, d.calls, 1)
	assert.Equal(t, "alice", d.calls[0].signerUserID)
	assert.Equal(t, "bob", d.calls[0].recipientID)

	var act map[string]any
	require.NoError(t, json.Unmarshal(d.calls[0].body, &act))
	assert.Equal(t, "Update", act["type"])
	obj := act["object"].(map[string]any)
	assert.Equal(t, "Game", obj["type"])
	state := obj["game_state"].(map[string]any)
	assert.Equal(t, "ready_states", state["type"])
	assert.Equal(t, true, state["ready"])
	assert.Equal(t, "sess-ready", state["game_session_id"])
}

func TestService_UpdateSettings_DeliversSettingsUpdateToRemote(t *testing.T) {
	game, _, _, svc := newPendingGame(t)
	d := wireFederated(t, svc, game, "sess-setting")

	require.NoError(t, svc.UpdateSettings(context.Background(), game.ID, "alice", "isLlotheo", json.RawMessage("true")))

	require.Len(t, d.calls, 1)
	var act map[string]any
	require.NoError(t, json.Unmarshal(d.calls[0].body, &act))
	state := act["object"].(map[string]any)["game_state"].(map[string]any)
	assert.Equal(t, "settings", state["type"])
	assert.Equal(t, "isLlotheo", state["key"])
	assert.Equal(t, true, state["value"])
}

func TestService_PutStone_DeliversPutstoneUpdateToRemote(t *testing.T) {
	game, _, _, svc := newPendingGame(t)
	d := wireFederated(t, svc, game, "sess-put")

	ctx := context.Background()
	require.NoError(t, svc.UpdateReady(ctx, game.ID, "alice", true))
	require.NoError(t, svc.UpdateReady(ctx, game.ID, "bob", true))
	// 1 つ目の PutStone 前のデリバリー: 2 件 (alice / bob の ready_states)
	// ただし bob は local user (fakeRepo の newPendingGame で local) なので
	// deliver は bob 側だけ skip される。alice が local、bob が remote という
	// setup なので alice → remote bob に 2 回 ready_states が飛んだ状態。
	require.GreaterOrEqual(t, len(d.calls), 1)
	before := len(d.calls)

	// alice が 1 手置く (初手は 19 などの有効な黒手)
	require.NoError(t, svc.PutStone(ctx, game.ID, "alice", 19, ""))

	assert.Equal(t, before+1, len(d.calls))
	last := d.calls[len(d.calls)-1]
	var act map[string]any
	require.NoError(t, json.Unmarshal(last.body, &act))
	state := act["object"].(map[string]any)["game_state"].(map[string]any)
	assert.Equal(t, "putstone", state["type"])
	assert.Equal(t, float64(19), state["pos"])
	assert.Equal(t, "sess-put", state["game_session_id"])
}

func TestService_CancelGame_InviterSendsUndoInviteToRemote(t *testing.T) {
	// alice = User1 (local inviter), bob = User2 (remote)。inviter 側が
	// CancelGame を呼ぶと CherryPick protocol-compliant に Undo(Invite) が
	// 送られる (#417 P4)。pre-start 専用で Surrender (started) は Leave。
	game, _, _, svc := newPendingGame(t)
	d := wireFederated(t, svc, game, "sess-cancel")

	require.NoError(t, svc.CancelGame(context.Background(), game.ID, "alice"))

	require.Len(t, d.calls, 1)
	var act map[string]any
	require.NoError(t, json.Unmarshal(d.calls[0].body, &act))
	assert.Equal(t, "Undo", act["type"])
	// @context は outer Undo のみに付く (inner Invite には付かない) こと
	// を確認する (#417 P4 Devin review: nested @context 除去)。
	_, outerHasContext := act["@context"]
	assert.True(t, outerHasContext, "outer Undo must carry @context")
	obj := act["object"].(map[string]any)
	assert.Equal(t, "Invite", obj["type"])
	_, innerHasContext := obj["@context"]
	assert.False(t, innerHasContext, "inner Invite must not carry nested @context")
	// 元 Invite にも同じ session id が埋まっていることを確認する
	inner := obj["object"].(map[string]any)
	state := inner["game_state"].(map[string]any)
	assert.Equal(t, "sess-cancel", state["game_session_id"])
}

func TestService_CancelGame_InviteeSendsLeaveToRemote(t *testing.T) {
	// User1 = 招待者 = remote (bob), User2 = 招待される側 = local (alice)。
	// 招待された側が CancelGame する場合は Undo(Invite) を投げるのは誤りで、
	// 従来通り Leave にフォールバックする (#417 P4)。
	repo := newFakeRepo()
	pub := &capturePublisher{}
	svc := NewService(repo, pub, nil)
	game := &model.ReversiGame{
		ID:                   "g2",
		User1ID:              "bob",
		User2ID:              "alice",
		Map:                  model.StringArray{"--------", "--------", "--------", "---wb---", "---bw---", "--------", "--------", "--------"},
		BW:                   "1",
		TimeLimitForEachTurn: 90,
		Logs:                 datatypes.JSON("[]"),
	}
	require.NoError(t, repo.Create(game))
	d := wireFederated(t, svc, game, "sess-decline")

	require.NoError(t, svc.CancelGame(context.Background(), game.ID, "alice"))

	require.Len(t, d.calls, 1)
	var act map[string]any
	require.NoError(t, json.Unmarshal(d.calls[0].body, &act))
	assert.Equal(t, "Leave", act["type"])
}

func TestService_Surrender_DeliversLeaveToRemote(t *testing.T) {
	game, repo, _, svc := newPendingGame(t)
	d := wireFederated(t, svc, game, "sess-surrender")

	// start the game first (simulate both ready → start)。fedCache が設定済み
	// なので ready 更新で deliver が 2 件走るが、この後 surrender の Leave を
	// assert する目的なのでカウントを before で記録しておく。
	ctx := context.Background()
	require.NoError(t, svc.UpdateReady(ctx, game.ID, "alice", true))
	require.NoError(t, svc.UpdateReady(ctx, game.ID, "bob", true))
	// fakeRepo から最新 game を取り直して IsStarted を確認する
	got, err := repo.FindByID(game.ID)
	require.NoError(t, err)
	require.True(t, got.IsStarted)

	before := len(d.calls)
	require.NoError(t, svc.Surrender(ctx, game.ID, "alice"))

	require.Equal(t, before+1, len(d.calls))
	var act map[string]any
	require.NoError(t, json.Unmarshal(d.calls[before].body, &act))
	assert.Equal(t, "Leave", act["type"])
}

// Remote actor (Host != nil) が state 変化を起こす場合は echo back しない。
// inbox handler が Service を呼ぶ経路の guard。
func TestService_UpdateReady_RemoteActor_NoDeliver(t *testing.T) {
	game, _, _, svc := newPendingGame(t)
	d := wireFederated(t, svc, game, "sess-echo")

	// bob (remote) が ready を切り替えたケースをシミュレート
	require.NoError(t, svc.UpdateReady(context.Background(), game.ID, "bob", true))

	assert.Empty(t, d.calls, "remote actor update must not be echoed back")
}

// opponent がローカルユーザーならそもそも federate しない。
func TestService_UpdateReady_LocalOpponent_NoDeliver(t *testing.T) {
	game, _, _, svc := newPendingGame(t)
	// federation cache / deliverer は wire するが、opponent "bob" を local に
	// する (Host == nil)。
	fedCache := NewFederationIDCache(reversiTestRedis.Client)
	fedCache.Set(context.Background(), "sess-local", game.ID)
	svc.SetFederationCache(fedCache)
	users := &stubUserLookup{users: map[string]*model.User{
		"alice": {ID: "alice", Username: "alice"},
		"bob":   {ID: "bob", Username: "bob"}, // Host == nil
	}}
	svc.SetUserRepo(users)
	svc.SetBaseURL("https://local.example")
	d := &captureDeliverer{}
	svc.SetFederationDeliverer(d)

	require.NoError(t, svc.UpdateReady(context.Background(), game.ID, "alice", true))
	assert.Empty(t, d.calls, "no delivery expected when opponent is local")
}

// deliverer 未配線では deliver 試行自体が no-op。
func TestService_UpdateReady_DelivererUnwired_NoOp(t *testing.T) {
	game, _, _, svc := newPendingGame(t)
	// deliverer を wire せずに UpdateReady
	require.NoError(t, svc.UpdateReady(context.Background(), game.ID, "alice", true))
	// panic せず成功すれば OK
}
