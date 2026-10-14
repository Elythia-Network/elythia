package processors_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/misc/idnhost"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/elythia-network/elythia/internal/testutil"
)

// **keyId のホストの書き方 (既定のポート、末尾のドット、Unicode) が違っても、
// 署名を検証する前に止まること (#3563)。** 判定は本物の NewInboxSuspendedChecker
// で行い、ホストは checker の入口で instance の行の形に揃う。
func TestInboxProcessor_SoftwareSuspendedKeyIDSpellings(t *testing.T) {
	instances := testutil.NewMockInstanceRepository()
	name, ver := "misskey", "2024.10.0"
	for i, h := range []string{"remote.example", idnhost.Puny("例え.example")} {
		require.NoError(t, instances.Create(&model.Instance{ID: fmt.Sprintf("i%d", i), Host: h, SoftwareName: &name, SoftwareVersion: &ver}))
	}
	meta := testutil.NewMockMetaRepository()
	meta.Meta = &model.Meta{InboxSuspendedSoftware: []byte(`[{"software":"misskey","versionRange":"<=2024.11.0"}]`)}

	for _, keyID := range []string{
		"https://remote.example:443/users/alice#main-key",
		"https://Remote.example./users/alice#main-key",
		"https://例え.example/users/alice#main-key",
	} {
		t.Run(keyID, func(t *testing.T) {
			priv, pub, err := activitypub.GenerateRSAKeypair()
			require.NoError(t, err)
			key, err := activitypub.NewPrivateKey(keyID, priv)
			require.NoError(t, err)
			host := "remote.example"
			aliceURI := "https://remote.example/users/alice"
			verifier := &countingVerifier{stubVerifier: &stubVerifier{actor: &model.User{ID: "alice", Host: &host, URI: &aliceURI}, pubKey: pub}}

			stub := &stubFedProcessor{}
			p := processors.NewInboxProcessor(stub)
			p.SetSignatureVerifier(verifier)
			p.SetHostBlockChecker(&stubBlocker{})
			p.SetSoftwareSuspensionChecker(federation.NewInboxSuspendedChecker(meta, instances))

			payload := signedInboxPayload(t, key, []byte(`{"id":"https://remote.example/follows/1","type":"Follow","actor":"https://remote.example/users/alice"}`))
			require.NoError(t, p.Handle(context.Background(), driver.RawTask{
				TypeName: queue.TaskTypeInbox,
				Body:     mustEncode(t, payload),
			}))
			assert.Zero(t, verifier.resolved, "拒否する相手の actor を解決しないこと")
			assert.Empty(t, stub.calls)
		})
	}
}
