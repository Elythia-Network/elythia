package relay_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/activitypub/ld"
	"github.com/shiroha-a/mk/internal/core/relay"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSysAcct records Fetch calls and returns a canned user.
type fakeSysAcct struct {
	user *model.User
	err  error
	mu   sync.Mutex
	n    int
}

func (f *fakeSysAcct) Fetch(kind string) (*model.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return f.user, f.err
}

// fakeDeliverer captures bodies + inboxes for assertion.
type fakeDeliverer struct {
	mu    sync.Mutex
	calls []deliverCall
	err   error
}

type deliverCall struct {
	SignerID string
	Body     []byte
	Inboxes  []string
}

func (d *fakeDeliverer) DeliverActivity(signerUserID string, body []byte, inboxes []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, deliverCall{SignerID: signerUserID, Body: append([]byte(nil), body...), Inboxes: append([]string(nil), inboxes...)})
	return d.err
}

func newService(t *testing.T) (*relay.Service, *testutil.MockRelayRepository, *fakeSysAcct, *fakeDeliverer) {
	t.Helper()
	repo := testutil.NewMockRelayRepository()
	sys := &fakeSysAcct{user: &model.User{ID: "sysrelay"}}
	deliv := &fakeDeliverer{}
	r := activitypub.NewRenderer(activitypub.NewURLBuilder("https://example.com"))
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	svc := relay.NewService(repo, sys, r, deliv, idGen)
	svc.SetClock(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() })
	return svc, repo, sys, deliv
}

func TestAdd_PersistsRowAndDeliversFollow(t *testing.T) {
	svc, repo, sys, deliv := newService(t)
	rel, err := svc.Add(context.Background(), "https://r.example/inbox")
	require.NoError(t, err)
	require.NotNil(t, rel)
	assert.Equal(t, "https://r.example/inbox", rel.Inbox)
	assert.Equal(t, relay.StatusRequesting, rel.Status)
	assert.Len(t, repo.Relays, 1)
	assert.Equal(t, 1, sys.n)
	require.Len(t, deliv.calls, 1)
	call := deliv.calls[0]
	assert.Equal(t, "sysrelay", call.SignerID)
	assert.Equal(t, []string{"https://r.example/inbox"}, call.Inboxes)
	// body は Follow Activity
	var follow map[string]any
	require.NoError(t, json.Unmarshal(call.Body, &follow))
	assert.Equal(t, "Follow", follow["type"])
	assert.Equal(t, activitypub.Public, follow["object"])
}

func TestAdd_EmptyInboxRejected(t *testing.T) {
	svc, _, _, _ := newService(t)
	_, err := svc.Add(context.Background(), "")
	assert.Error(t, err)
}

func TestAdd_DelivererErrorBubbles(t *testing.T) {
	svc, repo, _, deliv := newService(t)
	deliv.err = errors.New("network")
	_, err := svc.Add(context.Background(), "https://r.example/inbox")
	require.Error(t, err)
	// Follow 配信失敗時に挿入済み行がロールバックされる: unique inbox 制約を
	// 避けるため次回 Add を可能にする重要な挙動 (Devin review #171 対応)。
	assert.Empty(t, repo.Relays)
}

func TestRemove_SendsUndoAndDeletes(t *testing.T) {
	svc, repo, _, deliv := newService(t)
	rel, err := svc.Add(context.Background(), "https://r.example/inbox")
	require.NoError(t, err)
	deliv.mu.Lock()
	deliv.calls = nil
	deliv.mu.Unlock()

	require.NoError(t, svc.Remove(context.Background(), rel.ID))
	assert.Empty(t, repo.Relays)
	require.Len(t, deliv.calls, 1)
	var undo map[string]any
	require.NoError(t, json.Unmarshal(deliv.calls[0].Body, &undo))
	assert.Equal(t, "Undo", undo["type"])
	// 元 Follow が object にネストされている
	inner, ok := undo["object"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Follow", inner["type"])
}

func TestRemove_NotFoundIsNoop(t *testing.T) {
	svc, _, _, deliv := newService(t)
	// 存在しない id を渡しても err=nil、deliver も呼ばれない
	require.NoError(t, svc.Remove(context.Background(), "nonexistent"))
	assert.Empty(t, deliv.calls)
}

func TestList_ReturnsAll(t *testing.T) {
	svc, _, _, _ := newService(t)
	_, err := svc.Add(context.Background(), "https://a.example/inbox")
	require.NoError(t, err)
	_, err = svc.Add(context.Background(), "https://b.example/inbox")
	require.NoError(t, err)

	list, err := svc.List(context.Background())
	require.NoError(t, err)
	assert.Len(t, list, 2)
}

// TestFindByID_ReturnsRow は inbox の Accept / Reject が所有権検証に使う lookup を
// 固定する (federation.RelayStatusMarker.FindByID)。
func TestFindByID_ReturnsRow(t *testing.T) {
	svc, _, _, _ := newService(t)
	rel, err := svc.Add(context.Background(), "https://r.example/inbox")
	require.NoError(t, err)

	got, err := svc.FindByID(context.Background(), rel.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, rel.ID, got.ID)
	assert.Equal(t, "https://r.example/inbox", got.Inbox)

	// 存在しない id は error (呼び出し側はこれを見て drop する)。
	_, err = svc.FindByID(context.Background(), "no-such-relay")
	assert.Error(t, err)
}

func TestMarkAccepted_UpdatesStatus(t *testing.T) {
	svc, repo, _, _ := newService(t)
	rel, err := svc.Add(context.Background(), "https://r.example/inbox")
	require.NoError(t, err)
	require.NoError(t, svc.MarkAccepted(context.Background(), rel.ID))
	assert.Equal(t, relay.StatusAccepted, repo.Relays[rel.ID].Status)
}

func TestMarkRejected_UpdatesStatus(t *testing.T) {
	svc, repo, _, _ := newService(t)
	rel, err := svc.Add(context.Background(), "https://r.example/inbox")
	require.NoError(t, err)
	require.NoError(t, svc.MarkRejected(context.Background(), rel.ID))
	assert.Equal(t, relay.StatusRejected, repo.Relays[rel.ID].Status)
}

// 本家 2026.10.0 updateRequestingRelayStatus: 応答を反映するのは requesting の
// 行だけ。確定した relay に遅れて届いた Accept / Reject は status を動かさない。
func TestMarkStatus_OnlyFromRequesting(t *testing.T) {
	for _, tc := range []struct {
		name string
		from string
		mark func(*relay.Service, string) error
	}{
		{"accepted then Reject", relay.StatusAccepted, func(s *relay.Service, id string) error { return s.MarkRejected(context.Background(), id) }},
		{"rejected then Accept", relay.StatusRejected, func(s *relay.Service, id string) error { return s.MarkAccepted(context.Background(), id) }},
		{"accepted then Accept", relay.StatusAccepted, func(s *relay.Service, id string) error { return s.MarkAccepted(context.Background(), id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo, _, _ := newService(t)
			rel, err := svc.Add(context.Background(), "https://r.example/inbox")
			require.NoError(t, err)
			repo.Relays[rel.ID].Status = tc.from

			require.NoError(t, tc.mark(svc, rel.ID), "late answers are ignored, not errors")
			assert.Equal(t, tc.from, repo.Relays[rel.ID].Status)
		})
	}
}

func TestMarkAccepted_InvalidatesCache(t *testing.T) {
	svc, _, _, deliv := newService(t)
	rel, err := svc.Add(context.Background(), "https://r.example/inbox")
	require.NoError(t, err)
	// まず何もない状態で DeliverToAccepted → no-op (accepted relay が無い)
	require.NoError(t, svc.DeliverToAccepted(context.Background(), "alice", map[string]any{"type": "Note"}))
	deliv.mu.Lock()
	addedCalls := len(deliv.calls)
	deliv.mu.Unlock()

	// MarkAccepted してキャッシュ無効化、再度 DeliverToAccepted で届く
	require.NoError(t, svc.MarkAccepted(context.Background(), rel.ID))
	require.NoError(t, svc.DeliverToAccepted(context.Background(), "alice", map[string]any{"type": "Note"}))

	deliv.mu.Lock()
	finalCalls := len(deliv.calls)
	deliv.mu.Unlock()
	assert.Equal(t, addedCalls+1, finalCalls)
}

func TestDeliverToAccepted_NilActivity(t *testing.T) {
	svc, _, _, deliv := newService(t)
	require.NoError(t, svc.DeliverToAccepted(context.Background(), "alice", nil))
	assert.Empty(t, deliv.calls)
}

func TestDeliverToAccepted_CacheHit(t *testing.T) {
	svc, repo, _, _ := newService(t)
	// accepted relay を直接 repo に入れる (Add→MarkAccepted の順番を短縮)
	rel := &model.Relay{ID: "r1", Inbox: "https://a.example/inbox", Status: relay.StatusAccepted}
	require.NoError(t, repo.Create(rel))

	// 1 回目: DB から取得してキャッシュ
	require.NoError(t, svc.DeliverToAccepted(context.Background(), "alice", map[string]any{"type": "Note"}))
	// 途中で DB から消しても、キャッシュ TTL 内なら 2 回目も届く
	delete(repo.Relays, "r1")
	require.NoError(t, svc.DeliverToAccepted(context.Background(), "alice", map[string]any{"type": "Note"}))
}

func TestSetClockAndCacheMaxAge_NilZeroIgnored(t *testing.T) {
	svc, _, _, _ := newService(t)
	svc.SetClock(nil)
	svc.SetCacheMaxAge(0)
	_, err := svc.Add(context.Background(), "https://r.example/inbox")
	require.NoError(t, err)
}

func TestAdd_SysAccountErrorBubbles(t *testing.T) {
	svc, _, sys, _ := newService(t)
	sys.err = errors.New("sys-down")
	_, err := svc.Add(context.Background(), "https://r.example/inbox")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sys-down")
}

// Remove は Undo 配信エラーが起きても行自体は削除する (Devin review #171):
// 行が残ると admin UI から relay が消えず管理者が復旧できなくなるため、
// 配信失敗は警告ログで済ませ delete は必ず実行する。
func TestRemove_UndoDeliveryErrorStillDeletesRow(t *testing.T) {
	svc, repo, sys, _ := newService(t)
	rel, err := svc.Add(context.Background(), "https://r.example/inbox")
	require.NoError(t, err)
	sys.err = errors.New("sys-down")
	require.NoError(t, svc.Remove(context.Background(), rel.ID))
	// 行が削除されている
	assert.Empty(t, repo.Relays)
}

// failingRepo lets tests exercise repository error branches.
type failingRepo struct {
	*testutil.MockRelayRepository
	createErr error
	updateErr error
	listErr   error
	deleteErr error
}

func (f *failingRepo) Create(r *model.Relay) error {
	if f.createErr != nil {
		return f.createErr
	}
	return f.MockRelayRepository.Create(r)
}
func (f *failingRepo) UpdateStatusFrom(id, from, to string) (bool, error) {
	if f.updateErr != nil {
		return false, f.updateErr
	}
	return f.MockRelayRepository.UpdateStatusFrom(id, from, to)
}
func (f *failingRepo) ListByStatus(status string) ([]*model.Relay, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.MockRelayRepository.ListByStatus(status)
}
func (f *failingRepo) Delete(id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	return f.MockRelayRepository.Delete(id)
}

func TestAdd_RepoCreateErrorBubbles(t *testing.T) {
	repo := &failingRepo{MockRelayRepository: testutil.NewMockRelayRepository(), createErr: errors.New("db boom")}
	sys := &fakeSysAcct{user: &model.User{ID: "sysrelay"}}
	deliv := &fakeDeliverer{}
	r := activitypub.NewRenderer(activitypub.NewURLBuilder("https://example.com"))
	idGen, _ := id.NewGenerator("aidx")
	svc := relay.NewService(repo, sys, r, deliv, idGen)
	_, err := svc.Add(context.Background(), "https://r.example/inbox")
	require.Error(t, err)
}

func TestMarkAccepted_RepoErrorBubbles(t *testing.T) {
	repo := &failingRepo{MockRelayRepository: testutil.NewMockRelayRepository(), updateErr: errors.New("db boom")}
	sys := &fakeSysAcct{user: &model.User{ID: "sysrelay"}}
	deliv := &fakeDeliverer{}
	r := activitypub.NewRenderer(activitypub.NewURLBuilder("https://example.com"))
	idGen, _ := id.NewGenerator("aidx")
	svc := relay.NewService(repo, sys, r, deliv, idGen)
	err := svc.MarkAccepted(context.Background(), "x")
	assert.Error(t, err)
	err = svc.MarkRejected(context.Background(), "x")
	assert.Error(t, err)
}

func TestDeliverToAccepted_RepoListError(t *testing.T) {
	repo := &failingRepo{MockRelayRepository: testutil.NewMockRelayRepository(), listErr: errors.New("db boom")}
	sys := &fakeSysAcct{user: &model.User{ID: "sysrelay"}}
	deliv := &fakeDeliverer{}
	r := activitypub.NewRenderer(activitypub.NewURLBuilder("https://example.com"))
	idGen, _ := id.NewGenerator("aidx")
	svc := relay.NewService(repo, sys, r, deliv, idGen)
	err := svc.DeliverToAccepted(context.Background(), "alice", map[string]any{"type": "Note"})
	assert.Error(t, err)
}

func TestRemove_RepoDeleteErrorBubbles(t *testing.T) {
	repo := &failingRepo{MockRelayRepository: testutil.NewMockRelayRepository(), deleteErr: errors.New("db boom")}
	sys := &fakeSysAcct{user: &model.User{ID: "sysrelay"}}
	deliv := &fakeDeliverer{}
	r := activitypub.NewRenderer(activitypub.NewURLBuilder("https://example.com"))
	idGen, _ := id.NewGenerator("aidx")
	svc := relay.NewService(repo, sys, r, deliv, idGen)
	rel, err := svc.Add(context.Background(), "https://r.example/inbox")
	require.NoError(t, err)
	err = svc.Remove(context.Background(), rel.ID)
	assert.Error(t, err)
}

// upstream Misskey #17308 (= 2026.5.0 fix / triage #1002): relay 由来の Announce
// を検出するため Service に IsRelayActor を追加。actor.Inbox / SharedInbox が
// 登録済み (status=accepted) relay の Inbox と一致したら true。
func TestIsRelayActor(t *testing.T) {
	svc, repo, _, _ := newService(t)
	relayInbox := "https://relay.example/inbox"
	// accepted な relay を登録 (= cache 経由で IsRelayActor の判定対象に入る)
	require.NoError(t, repo.Create(&model.Relay{ID: "r1", Inbox: relayInbox, Status: relay.StatusAccepted}))

	t.Run("nil_actor", func(t *testing.T) {
		assert.False(t, svc.IsRelayActor(nil))
	})

	t.Run("actor_without_inbox", func(t *testing.T) {
		assert.False(t, svc.IsRelayActor(&model.User{ID: "u1"}))
	})

	t.Run("actor_inbox_does_not_match", func(t *testing.T) {
		other := "https://other.example/inbox"
		assert.False(t, svc.IsRelayActor(&model.User{ID: "u2", Inbox: &other}))
	})

	t.Run("actor_inbox_matches", func(t *testing.T) {
		inbox := relayInbox
		assert.True(t, svc.IsRelayActor(&model.User{ID: "u3", Inbox: &inbox}),
			"actor.Inbox が登録済み relay の Inbox と一致 → true")
	})

	t.Run("actor_shared_inbox_matches", func(t *testing.T) {
		other := "https://relay.example/users/relay/inbox"
		shared := relayInbox
		assert.True(t, svc.IsRelayActor(&model.User{ID: "u4", Inbox: &other, SharedInbox: &shared}),
			"actor.SharedInbox が登録済み relay の Inbox と一致 → true")
	})
}

// stubLdSigner records its inputs and attaches a marker signature.
type stubLdSigner struct {
	gotActivity   map[string]any
	gotPrivateKey string
	gotCreator    string
	err           error
}

func (s *stubLdSigner) SignRsaSignature2017(activity map[string]any, privateKeyPEM, creator string, _ time.Time) (map[string]any, error) {
	s.gotActivity = activity
	s.gotPrivateKey = privateKeyPEM
	s.gotCreator = creator
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[string]any, len(activity)+1)
	for k, v := range activity {
		out[k] = v
	}
	out["signature"] = map[string]any{"type": "RsaSignature2017", "stub": true}
	return out, nil
}

type stubKeypairProvider struct {
	kp  *model.UserKeypair
	err error
}

func (s *stubKeypairProvider) FindByUserID(string) (*model.UserKeypair, error) {
	return s.kp, s.err
}

func acceptedRelaySvc(t *testing.T) (*relay.Service, *fakeDeliverer) {
	t.Helper()
	svc, _, _, deliv := newService(t)
	rel, err := svc.Add(context.Background(), "https://r.example/inbox")
	require.NoError(t, err)
	require.NoError(t, svc.MarkAccepted(context.Background(), rel.ID))
	return svc, deliv
}

func lastDeliveredBody(t *testing.T, deliv *fakeDeliverer) map[string]any {
	t.Helper()
	deliv.mu.Lock()
	defer deliv.mu.Unlock()
	require.NotEmpty(t, deliv.calls)
	var m map[string]any
	require.NoError(t, json.Unmarshal(deliv.calls[len(deliv.calls)-1].Body, &m))
	return m
}

// LD 署名が配線されていると relay 配送 activity に to=Public 補完 + RsaSignature2017
// が付与され、署名は author の keypair + creator key URI で行われる (#1870)。
func TestDeliverToAccepted_AttachesLdSignature(t *testing.T) {
	svc, deliv := acceptedRelaySvc(t)
	signer := &stubLdSigner{}
	kpr := &stubKeypairProvider{kp: &model.UserKeypair{UserID: "alice", PrivateKey: "PRIVPEM"}}
	svc.SetLdSigning(kpr, signer, "https://example.com")

	// to を持たない activity → Public 補完 + 署名
	err := svc.DeliverToAccepted(context.Background(), "alice",
		map[string]any{"type": "Create", "actor": "https://example.com/users/alice"})
	require.NoError(t, err)

	// signer が author の private key + creator key URI で呼ばれる
	assert.Equal(t, "PRIVPEM", signer.gotPrivateKey)
	assert.Equal(t, "https://example.com/users/alice#main-key", signer.gotCreator)
	// signer に渡る activity は to 補完済み
	assert.Equal(t, []any{activitypub.Public}, signer.gotActivity["to"])

	// 配送 body に signature が乗り、to も補完されている
	sent := lastDeliveredBody(t, deliv)
	assert.Contains(t, sent, "signature")
	assert.Equal(t, []any{activitypub.Public}, sent["to"])
}

// keypair が引けないときは署名を諦めて unsigned で配送する (best-effort、
// relay 配送自体は止めない)。to 補完は適用される。
func TestDeliverToAccepted_LdSigningKeypairMissing_UnsignedFallback(t *testing.T) {
	svc, deliv := acceptedRelaySvc(t)
	signer := &stubLdSigner{}
	svc.SetLdSigning(&stubKeypairProvider{err: errors.New("not found")}, signer, "https://example.com")

	require.NoError(t, svc.DeliverToAccepted(context.Background(), "alice",
		map[string]any{"type": "Create"}))

	assert.Empty(t, signer.gotCreator, "signer must not be called when keypair is missing")
	sent := lastDeliveredBody(t, deliv)
	assert.NotContains(t, sent, "signature", "keypair missing → unsigned fallback")
	assert.Contains(t, sent, "to", "to is still completed")
}

// 署名処理がエラーになっても unsigned で配送する。
func TestDeliverToAccepted_LdSigningError_UnsignedFallback(t *testing.T) {
	svc, deliv := acceptedRelaySvc(t)
	signer := &stubLdSigner{err: errors.New("sign boom")}
	svc.SetLdSigning(&stubKeypairProvider{kp: &model.UserKeypair{UserID: "alice", PrivateKey: "PRIVPEM"}}, signer, "https://example.com")

	require.NoError(t, svc.DeliverToAccepted(context.Background(), "alice",
		map[string]any{"type": "Create"}))

	sent := lastDeliveredBody(t, deliv)
	assert.NotContains(t, sent, "signature", "sign error → unsigned fallback")
}

// LD 署名が未配線なら従来どおり unsigned で配送する (existing 挙動の regression guard)。
func TestDeliverToAccepted_NoLdSigning_Unsigned(t *testing.T) {
	svc, deliv := acceptedRelaySvc(t)
	require.NoError(t, svc.DeliverToAccepted(context.Background(), "alice",
		map[string]any{"type": "Create"}))
	sent := lastDeliveredBody(t, deliv)
	assert.NotContains(t, sent, "signature")
}

// end-to-end: 実 ld.Processor + 実 RSA keypair で marshalForRelay 経由に署名し、
// 配送 body が production verifier (VerifyRsaSignature2017) を通ることを確認する。
// stub signer ではなく実署名経路を drop-in critical path として封じる (#1870)。
func TestDeliverToAccepted_LdSignature_VerifiesEndToEnd(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	privPEM := string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv),
	}))
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	require.NoError(t, err)
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))

	svc, deliv := acceptedRelaySvc(t)
	svc.SetLdSigning(
		&stubKeypairProvider{kp: &model.UserKeypair{UserID: "alice", PrivateKey: privPEM}},
		ld.NewProcessor(),
		"https://example.com",
	)

	// to を持たない実 shape (@context 付き Create) を流す。
	activity := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       "https://example.com/notes/n1/activity",
		"type":     "Create",
		"actor":    "https://example.com/users/alice",
		"object": map[string]any{
			"id":      "https://example.com/notes/n1",
			"type":    "Note",
			"content": "hello relay",
		},
	}
	require.NoError(t, svc.DeliverToAccepted(context.Background(), "alice", activity))

	sent := lastDeliveredBody(t, deliv)
	require.Contains(t, sent, "signature", "real signer must attach a signature")
	assert.Equal(t, []any{activitypub.Public}, sent["to"], "to は Public に補完される")

	// production verifier で検証が通る = downstream が真正性を検証できる shape。
	require.NoError(t, ld.NewProcessor().VerifyRsaSignature2017(sent, pubPEM),
		"relay-signed activity must verify under the production LD verifier")
}
