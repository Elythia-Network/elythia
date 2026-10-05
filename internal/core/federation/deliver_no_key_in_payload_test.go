package federation_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
)

// **enqueue する payload に署名鍵を載せない。**
//
// deliver job は `admin/queue/jobs` が moderator へ返すので、載せると鍵が
// そこから読める。取得できれば任意のローカルユーザーとして署名付き連合
// リクエストを偽造できる。worker は `SignerUserID` から配送時に引く
// (upstream Misskey も配送時に引く設計)。
func TestDeliverActivity_DoesNotPutSigningKeyInPayload(t *testing.T) {
	svc, enq, userRepo, _, keypairRepo := newDeliverService(t)
	installLocalSigner(t, userRepo, keypairRepo)

	err := svc.DeliverActivity("alice", []byte(`{"type":"Create"}`),
		[]string{"https://remote.example/inbox"})
	require.NoError(t, err)
	require.Len(t, enq.calls, 1)

	p := enq.calls[0]
	require.Emptyf(t, p.KeyPEM,
		"payload に RSA 署名鍵が載っている。admin/queue/jobs から moderator に読める")
	require.Emptyf(t, p.Ed25519PrivPEM,
		"payload に Ed25519 署名鍵が載っている。admin/queue/jobs から moderator に読める")

	// 代わりに署名者を載せる。これが無いと worker が鍵を引けず配送できない。
	require.Equal(t, "alice", p.SignerUserID)
	// keyID は署名ヘッダに載る公開の識別子なので残す。
	require.Equal(t, "https://example.com/users/alice#main-key", p.KeyID)
}

// **sync hook (test 経路) には従来どおり鍵を渡す。** queue を経由しないので
// Redis にも admin API にも出ない。ここを落とすと e2e_federation が壊れる。
func TestDeliverActivity_SyncHookStillReceivesKey(t *testing.T) {
	svc, _, userRepo, _, keypairRepo := newDeliverService(t)
	installLocalSigner(t, userRepo, keypairRepo)

	var got queue.DeliverPayload
	svc.SetSyncDeliverHookForTest(func(p queue.DeliverPayload) error {
		got = p
		return nil
	})

	err := svc.DeliverActivity("alice", []byte(`{"type":"Create"}`),
		[]string{"https://remote.example/inbox"})
	require.NoError(t, err)
	require.Equal(t, "PEM-DATA", got.KeyPEM, "inline 経路は鍵を受け取ること")
	require.Equal(t, "alice", got.SignerUserID)
}

// 署名者の鍵が無ければ enqueue しない (従来どおり)。payload から鍵を外したので
// 「鍵が無くても enqueue され、worker で初めて落ちる」形にならないことを見る。
func TestDeliverActivity_StillRejectsSignerWithoutKey(t *testing.T) {
	svc, enq, userRepo, _, _ := newDeliverService(t)
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}

	err := svc.DeliverActivity("bob", []byte(`{"type":"Create"}`),
		[]string{"https://remote.example/inbox"})
	require.Error(t, err)
	require.Empty(t, enq.calls, "鍵が無い署名者の配送は enqueue しないこと")
}
