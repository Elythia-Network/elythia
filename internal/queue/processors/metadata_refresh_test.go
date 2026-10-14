package processors_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
)

// stubMetadataRefresher records the hosts handed to MaybeRefresh.
type stubMetadataRefresher struct{ hosts []string }

func (s *stubMetadataRefresher) MaybeRefresh(host string) { s.hosts = append(s.hosts, host) }

// **受信のたびに、相手の nodeinfo の取り直しを頼むこと (#3564)。** 受信を拒否した
// 相手も頼む (拒否の判定を新しい版で行うため)。ドメインブロックと連合の許可外の
// 相手は本家と同じく頼まない。
func TestInboxProcessor_RefreshesMetadata(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	host := "remote.example"
	aliceURI := "https://remote.example/users/alice"
	body := []byte(`{"id":"https://remote.example/follows/1","type":"Follow","actor":"https://remote.example/users/alice"}`)

	for _, tc := range []struct {
		name string
		// keyID の host は大文字混じりにできる (検証前の拒否は小文字にして見る)。
		keyID     string
		suspended map[string]bool
		blocked   bool
		allowed   bool
		want      []string
		processed bool
	}{
		{name: "accepted activity", keyID: "https://remote.example/users/alice#main-key",
			allowed: true, want: []string{host}, processed: true},
		{name: "refused before verification", keyID: "https://Remote.example/users/alice#main-key",
			suspended: map[string]bool{host: true}, allowed: true, want: []string{host}},
		{name: "domain-blocked host refused before verification", keyID: "https://remote.example/users/alice#main-key",
			suspended: map[string]bool{host: true}, blocked: true, allowed: true},
		{name: "domain-blocked host", keyID: "https://remote.example/users/alice#main-key",
			blocked: true, allowed: true},
		{name: "host outside the allowlist", keyID: "https://remote.example/users/alice#main-key",
			allowed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, err := activitypub.NewPrivateKey(tc.keyID, priv)
			require.NoError(t, err)
			stub := &stubFedProcessor{}
			refresher := &stubMetadataRefresher{}
			p := processors.NewInboxProcessor(stub)
			p.SetSignatureVerifier(&stubVerifier{actor: &model.User{ID: "alice", Host: &host, URI: &aliceURI}, pubKey: pub})
			p.SetHostBlockChecker(&stubBlocker{
				blocked: func(string) bool { return tc.blocked },
				allowed: func(string) bool { return tc.allowed },
			})
			p.SetSoftwareSuspensionChecker(stubSoftwareBlocker{suspended: tc.suspended})
			p.SetMetadataRefresher(refresher)

			require.NoError(t, p.Handle(context.Background(), driver.RawTask{
				TypeName: queue.TaskTypeInbox,
				Body:     mustEncode(t, signedInboxPayload(t, key, body)),
			}))
			assert.Equal(t, tc.want, refresher.hosts)
			assert.Equal(t, tc.processed, len(stub.calls) > 0)
		})
	}
}

// **検証の後の判定 (isBlocked) で拒否した相手も取り直すこと (#3564)。** keyId の
// ホストでは当たらず、DB で正規化したホストで当たる相手がここに来る。
func TestInboxProcessor_RefreshesMetadataWhenRefusedAfterVerification(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://alias.example/users/alice#main-key", priv)
	require.NoError(t, err)
	host := "remote.example"
	aliceURI := "https://remote.example/users/alice"

	stub := &stubFedProcessor{}
	refresher := &stubMetadataRefresher{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(&stubVerifier{actor: &model.User{ID: "alice", Host: &host, URI: &aliceURI}, pubKey: pub})
	p.SetHostBlockChecker(&stubBlocker{})
	p.SetSoftwareSuspensionChecker(stubSoftwareBlocker{suspended: map[string]bool{host: true}})
	p.SetMetadataRefresher(refresher)

	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, signedInboxPayload(t, key, []byte(`{"id":"https://remote.example/follows/1","type":"Follow","actor":"https://remote.example/users/alice"}`))),
	}))
	assert.Empty(t, stub.calls)
	assert.Equal(t, []string{host}, refresher.hosts)
}

// 署名検証に失敗した受信では取り直さない (相手を確かめられていない)。
func TestInboxProcessor_NoMetadataRefreshOnSignatureFailure(t *testing.T) {
	priv, _, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	_, otherPub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)
	host := "remote.example"
	aliceURI := "https://remote.example/users/alice"

	refresher := &stubMetadataRefresher{}
	p := processors.NewInboxProcessor(&stubFedProcessor{})
	p.SetSignatureVerifier(&stubVerifier{actor: &model.User{ID: "alice", Host: &host, URI: &aliceURI}, pubKey: otherPub})
	p.SetHostBlockChecker(&stubBlocker{})
	p.SetMetadataRefresher(refresher)

	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, signedInboxPayload(t, key, []byte(`{"id":"https://remote.example/follows/1","type":"Follow","actor":"https://remote.example/users/alice"}`))),
	}))
	assert.Empty(t, refresher.hosts)
}

// **配送に成功したら、相手の nodeinfo の取り直しを頼むこと (#3564)。** 本家と
// 同じく 2xx のときだけ、enableStatsForFederatedInstances が有効なときだけ。
func TestDeliverProcessor_RefreshesMetadata(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		statsEnabled func() bool
		suspended    bool
		want         []string
	}{
		{name: "2xx", status: http.StatusAccepted, want: []string{"remote.example"}},
		{name: "2xx with stats enabled", status: http.StatusOK, statsEnabled: func() bool { return true }, want: []string{"remote.example"}},
		{name: "2xx with stats disabled", status: http.StatusOK, statsEnabled: func() bool { return false }},
		{name: "4xx", status: http.StatusForbidden},
		{name: "404", status: http.StatusNotFound},
		{name: "429", status: http.StatusTooManyRequests},
		{name: "5xx", status: http.StatusInternalServerError},
		{name: "software suspended", status: http.StatusOK, suspended: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refresher := &stubMetadataRefresher{}
			p := processors.NewDeliverProcessor(&stubSigner{resp: okResponse(tc.status)})
			p.SetSuspendedChecker(stubSoftwareSuspension{suspended: map[string]bool{"remote.example": tc.suspended}})
			p.SetMetadataRefresher(refresher, tc.statsEnabled)
			_ = p.Handle(context.Background(), makeTask(t, makePayload(t)))
			assert.Equal(t, tc.want, refresher.hosts)
		})
	}
}
