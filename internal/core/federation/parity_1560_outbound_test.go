package federation_test

import (
	"encoding/json"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newBlockingHook(t *testing.T) (*federation.BlockingDeliveryHook, *stubEnqueuer, *testutil.MockUserRepository, *testutil.MockUserKeypairRepository) {
	t.Helper()
	enq := &stubEnqueuer{}
	userRepo := testutil.NewMockUserRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	keypairRepo := testutil.NewMockUserKeypairRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	deliver := federation.NewDeliverService(enq, userRepo, followingRepo, keypairRepo, urls)
	renderer := activitypub.NewRenderer(urls)
	hook := federation.NewBlockingDeliveryHook(deliver, userRepo, renderer)
	return hook, enq, userRepo, keypairRepo
}

// #1560 [MEDIUM] local→remote の Block / Undo(Block) を相手 inbox へ配信。
func TestBlockingHook_DeliversBlockAndUndo(t *testing.T) {
	hook, enq, userRepo, keypairRepo := newBlockingHook(t)
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"} // local
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}
	bobURI, bobInbox, host := "https://remote.example/users/bob", "https://remote.example/users/bob/inbox", "remote.example"
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &host, URI: &bobURI, Inbox: &bobInbox}

	hook.OnBlocked("alice", "bob")
	require.Len(t, enq.calls, 1)
	var blk map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &blk))
	assert.Equal(t, "Block", blk["type"])
	assert.Equal(t, bobURI, blk["object"])
	assert.Contains(t, blk["actor"], "alice")

	hook.OnUnblocked("alice", "bob")
	require.Len(t, enq.calls, 2)
	var undo map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[1].Body, &undo))
	assert.Equal(t, "Undo", undo["type"])
	inner, ok := undo["object"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Block", inner["type"])
	assert.Equal(t, bobURI, inner["object"])
}

// blockee がローカル or URI 無しなら配信しない。
func TestBlockingHook_NoDeliveryForLocalOrUnknown(t *testing.T) {
	hook, enq, userRepo, _ := newBlockingHook(t)
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol"} // local blockee
	hook.OnBlocked("alice", "carol")
	assert.Empty(t, enq.calls, "local blockee → no AP delivery")
}

func newProfileUpdateHook(t *testing.T) (*federation.ProfileUpdateDeliveryHook, *stubEnqueuer, *testutil.MockUserRepository, *testutil.MockFollowingRepository, *testutil.MockUserKeypairRepository) {
	t.Helper()
	enq := &stubEnqueuer{}
	userRepo := testutil.NewMockUserRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	keypairRepo := testutil.NewMockUserKeypairRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	deliver := federation.NewDeliverService(enq, userRepo, followingRepo, keypairRepo, urls)
	renderer := activitypub.NewRenderer(urls)
	hook := federation.NewProfileUpdateDeliveryHook(deliver, renderer, userRepo, keypairRepo)
	return hook, enq, userRepo, followingRepo, keypairRepo
}

// #1560 [MEDIUM] profile 編集で Update(Person) を remote followers へ配信。
func TestProfileUpdateHook_DeliversToFollowers(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo := newProfileUpdateHook(t)
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"} // local
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}
	// remote follower bob の inbox を登録
	followingRepo.RemoteInboxes["alice"] = []string{"https://remote.example/users/bob/inbox"}

	hook.OnLocalProfileUpdated("alice")
	require.Len(t, enq.calls, 1)
	assert.Equal(t, "https://remote.example/users/bob/inbox", enq.calls[0].Inbox)
	var upd map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &upd))
	assert.Equal(t, "Update", upd["type"])
	inner, ok := upd["object"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Person", inner["type"])
	assert.Contains(t, inner["id"], "alice")
}

// resolve の早期 return 分岐: blocker 不在 / blocker が非ローカルなら配信しない。
func TestBlockingHook_NoDeliveryForMissingOrRemoteBlocker(t *testing.T) {
	hook, enq, userRepo, _ := newBlockingHook(t)
	bobURI, bobInbox, host := "https://remote.example/users/bob", "https://remote.example/users/bob/inbox", "remote.example"
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &host, URI: &bobURI, Inbox: &bobInbox}

	// blocker が存在しない
	hook.OnBlocked("ghost", "bob")
	hook.OnUnblocked("ghost", "bob")
	assert.Empty(t, enq.calls, "missing blocker → no AP delivery")

	// blocker がリモート
	rhost, rURI := "remote.example", "https://remote.example/users/remoteblocker"
	userRepo.Users["remoteblocker"] = &model.User{ID: "remoteblocker", Username: "rb", Host: &rhost, URI: &rURI}
	hook.OnBlocked("remoteblocker", "bob")
	assert.Empty(t, enq.calls, "remote blocker → no AP delivery")
}

// OnLocalProfileUpdated の早期 return: user 不在 / 非ローカル / keypair 不在では配信しない。
func TestProfileUpdateHook_NoDeliveryForMissingOrRemoteOrNoKeypair(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo := newProfileUpdateHook(t)

	// user 不在
	hook.OnLocalProfileUpdated("ghost")
	assert.Empty(t, enq.calls, "missing user → no delivery")

	// 非ローカル user
	host := "remote.example"
	userRepo.Users["remote"] = &model.User{ID: "remote", Username: "remote", Host: &host}
	hook.OnLocalProfileUpdated("remote")
	assert.Empty(t, enq.calls, "remote user → no delivery")

	// keypair 不在 (local user だが鍵が無い)
	userRepo.Users["nokey"] = &model.User{ID: "nokey", Username: "nokey"}
	hook.OnLocalProfileUpdated("nokey")
	assert.Empty(t, enq.calls, "no keypair → no delivery")

	// keypair はあるが follower が居なければ enqueue 0 件 (profile 行無しでも続行)
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}
	hook.OnLocalProfileUpdated("alice")
	assert.Empty(t, enq.calls, "no remote followers → no enqueue")
}

// SetKeypairExtraRepo を設定すると配信 Person に ed25519 assertionMethod が載る。
// + lookupEd25519 の各分岐 (有効 PEM / 不正 PEM / row 無し) を網羅する。
func TestProfileUpdateHook_Ed25519AndRelay(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo := newProfileUpdateHook(t)
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}
	followingRepo.RemoteInboxes["alice"] = []string{"https://remote.example/users/bob/inbox"}

	_, pubPEM, err := activitypub.GenerateEd25519Keypair()
	require.NoError(t, err)
	extraRepo := testutil.NewMockUserKeypairExtraRepository()
	require.NoError(t, extraRepo.Upsert(&model.UserKeypairExtra{UserID: "alice", Ed25519PublicKey: pubPEM, Ed25519PrivateKey: "x"}))
	hook.SetKeypairExtraRepo(extraRepo)
	relay := &fakeRelayBroadcaster{}
	hook.SetRelayBroadcaster(relay)

	hook.OnLocalProfileUpdated("alice")
	require.Len(t, enq.calls, 1)
	var upd map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &upd))
	inner := upd["object"].(map[string]any)
	assert.NotNil(t, inner["assertionMethod"], "ed25519 row があれば assertionMethod が載る")
	require.Len(t, relay.calls, 1, "relay subscriber にも fan-out する")
	assert.Equal(t, "alice", relay.calls[0].SignerID)

	// 不正な PEM は黙って RSA-only に fallback (assertionMethod 無し)。
	enq.calls = nil
	require.NoError(t, extraRepo.Upsert(&model.UserKeypairExtra{UserID: "alice", Ed25519PublicKey: "not-a-pem", Ed25519PrivateKey: "x"}))
	hook.OnLocalProfileUpdated("alice")
	require.Len(t, enq.calls, 1)
	var upd2 map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &upd2))
	inner2 := upd2["object"].(map[string]any)
	assert.Nil(t, inner2["assertionMethod"], "不正 PEM は assertionMethod を載せない")

	// row 無し (extra repo はあるが該当 user の行が無い) も RSA-only。
	enq.calls = nil
	require.NoError(t, extraRepo.Delete("alice"))
	hook.OnLocalProfileUpdated("alice")
	require.Len(t, enq.calls, 1)
}

// 配送 enqueue が失敗しても hook は panic せず log のみ (best-effort)。
func TestOutboundHooks_DeliverErrorIsBestEffort(t *testing.T) {
	bhook, benq, buserRepo, bkeypairRepo := newBlockingHook(t)
	benq.err = assert.AnError
	buserRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	bkeypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}
	bobURI, bobInbox, host := "https://remote.example/users/bob", "https://remote.example/users/bob/inbox", "remote.example"
	buserRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", Host: &host, URI: &bobURI, Inbox: &bobInbox}
	assert.NotPanics(t, func() {
		bhook.OnBlocked("alice", "bob")
		bhook.OnUnblocked("alice", "bob")
	})

	phook, penq, puserRepo, pfollowingRepo, pkeypairRepo := newProfileUpdateHook(t)
	penq.err = assert.AnError
	puserRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	pkeypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PrivateKey: "PEM"}
	pfollowingRepo.RemoteInboxes["alice"] = []string{"https://remote.example/users/bob/inbox"}
	relay := &fakeRelayBroadcaster{err: assert.AnError}
	phook.SetRelayBroadcaster(relay)
	assert.NotPanics(t, func() { phook.OnLocalProfileUpdated("alice") })
}
