package inbox

import (
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubCapabilityRecorder captures ObserveInboundAlg calls.
type stubCapabilityRecorder struct {
	hosts []string
	algs  []string
}

func (s *stubCapabilityRecorder) ObserveInboundAlg(host, alg string) {
	s.hosts = append(s.hosts, host)
	s.algs = append(s.algs, alg)
}

// 同期 verify 経路 (SetEnqueuer 未配線) でも署名方式が記録される。worker 経路
// だけに入れると、この構成では丸ごと記録されない。
func TestInbox_RecordsInboundSignatureAlg(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	h, repo, _ := newHandler(t, pub)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}
	recorder := &stubCapabilityRecorder{}
	h.SetSignatureCapabilityRecorder(recorder)

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)
	c, rec := newPost(t, body)
	req := c.Request()
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest(body),
		[]string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Len(t, recorder.hosts, 1)
	assert.Equal(t, "remote.example", recorder.hosts[0])
	assert.Equal(t, model.SignatureAlgRSA, recorder.algs[0], "RSA 鍵で署名した相手は rsa")
}

// 署名検証に失敗した相手は記録しない。未検証の観測を残すと、任意の送信元が
// 他 host のラベルを書き換えられる。
func TestInbox_DoesNotRecordOnVerifyFailure(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	h, _, _ := newHandler(t, pub)
	recorder := &stubCapabilityRecorder{}
	h.SetSignatureCapabilityRecorder(recorder)

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)
	c, rec := newPost(t, body)
	req := c.Request()
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest(body),
		[]string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"
	// 署名後に body を差し替えて Digest を不一致にする。
	req.Header.Set("Digest", activitypub.SHA256Digest([]byte(`{"type":"Follow"}`)))

	require.NoError(t, h.Inbox(c))
	assert.NotEqual(t, http.StatusAccepted, rec.Code)
	assert.Empty(t, recorder.hosts, "検証に失敗した相手は記録しない")
}

// recorder 未配線でも panic しない。
func TestInbox_CapabilityRecorderUnwired(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	h, repo, _ := newHandler(t, pub)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)
	c, rec := newPost(t, body)
	req := c.Request()
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest(body),
		[]string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusAccepted, rec.Code)
}

func TestRecordSignatureCapability_MapsKeyType(t *testing.T) {
	host := "remote.example"
	tests := []struct {
		name string
		kt   activitypub.KeyType
		want string
	}{
		{"rsa", activitypub.KeyTypeRSA, model.SignatureAlgRSA},
		{"ed25519", activitypub.KeyTypeEd25519, model.SignatureAlgEd25519},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &stubCapabilityRecorder{}
			h := &Handler{capabilities: recorder}
			h.recordSignatureCapability(&model.User{Host: &host}, tt.kt)
			require.Len(t, recorder.algs, 1)
			assert.Equal(t, tt.want, recorder.algs[0])
		})
	}
}

// ローカル actor (Host nil) は記録しない。
func TestRecordSignatureCapability_SkipsLocalActor(t *testing.T) {
	recorder := &stubCapabilityRecorder{}
	h := &Handler{capabilities: recorder}
	h.recordSignatureCapability(&model.User{}, activitypub.KeyTypeRSA)
	h.recordSignatureCapability(nil, activitypub.KeyTypeRSA)
	assert.Empty(t, recorder.hosts)
}
