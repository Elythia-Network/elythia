package processors_test

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本番の *federation.Processor が inbox の optional interface を満たし続けることを
// コンパイル時に確かめる (#3610)。dispatch は型アサーションで分岐するので、満たさなく
// なってもビルドもテストも通り、黙ってもう 1 回 Normalize する経路に戻ってしまう。
var (
	_ processors.FederationProcessor         = (*federation.Processor)(nil)
	_ processors.SignerAwareProcessor        = (*federation.Processor)(nil)
	_ processors.NormalizedActivityProcessor = (*federation.Processor)(nil)
)

// normalizedRecorder implements every processor entry point and records which
// one the inbox used (#3610).
type normalizedRecorder struct {
	normalized []federation.NormalizedActivity
	signers    []*model.User
	rawCalls   int
}

func (r *normalizedRecorder) Process([]byte) error { r.rawCalls++; return nil }

func (r *normalizedRecorder) ProcessWithSigner([]byte, *model.User) error {
	r.rawCalls++
	return nil
}

func (r *normalizedRecorder) ProcessNormalized(n federation.NormalizedActivity, signer *model.User) error {
	r.normalized = append(r.normalized, n)
	r.signers = append(r.signers, signer)
	return nil
}

// #3610: 署名者 == actor の経路では、gate が正規化した結果をそのまま処理へ渡す。
// 渡すものは生の本文から作ったもので、その id を再投函の判定に使う。
func TestInboxProcessor_DirectActivityIsDispatchedNormalized(t *testing.T) {
	body := []byte(`[{"id":"https://remote.example/follows/7","type":"Follow","actor":{"id":"https://remote.example/users/alice"},"object":"https://example.com/users/bob"}]`)
	verifier, payload := signedReplayFixture(t, body)
	rec := &normalizedRecorder{}
	guard := newMemReplayGuard()
	p := processors.NewInboxProcessor(rec, verifier, nil, nil, nil, guard)

	task := driver.RawTask{TypeName: queue.TaskTypeInbox, Body: mustEncode(t, payload)}
	require.NoError(t, p.Handle(context.Background(), task))
	require.Len(t, rec.normalized, 1)
	assert.Zero(t, rec.rawCalls, "a normalizing processor must not get the raw body to normalize again")
	got := rec.normalized[0]
	assert.Equal(t, payload.Body, got.Source(), "the activity must be built from the signed body")
	assert.Equal(t, federation.ActivityFields{
		Actor: "https://remote.example/users/alice",
		ID:    "https://remote.example/follows/7",
		Type:  "Follow",
	}, got.Fields())
	assert.Same(t, verifier.actor, rec.signers[0], "the verified signer row must be handed over")
	assert.True(t, guard.seen["https://remote.example/follows/7"], "the replay guard must remember the activity id")

	require.NoError(t, p.Handle(context.Background(), task))
	assert.Len(t, rec.normalized, 1, "the replay must be dropped")
}

// 転送の経路では、処理へ渡すのも再投函の判定に使う id も、LD-Signature が覆う
// compact 済みの文書から取る。生の本文の正規化を渡してはいけない。
func TestInboxProcessor_ForwardedActivityIsDispatchedFromCompactedBody(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://relay.example/actor#main-key", priv)
	require.NoError(t, err)
	relayHost := "relay.example"
	originHost := "origin.example"
	verifier := &multiActorVerifier{
		pubKey: pub,
		byURI: map[string]*model.User{
			"https://relay.example/actor":        {ID: "relay", Host: &relayHost, URI: uptr("https://relay.example/actor")},
			"https://origin.example/users/alice": {ID: "alice", Host: &originHost, URI: uptr("https://origin.example/users/alice")},
		},
	}
	compacted := []byte(`{"id":"https://origin.example/creates/1#signed","type":"Create","actor":"https://origin.example/users/alice"}`)
	rec := &normalizedRecorder{}
	guard := newMemReplayGuard()
	p := processors.NewInboxProcessor(rec, verifier, nil, nil, nil, guard)
	p.SetLDSignatureVerifier(&stubLDVerifier{present: true, creator: "https://origin.example/users/alice#main-key", compactedBody: compacted})

	body := []byte(`{"id":"https://origin.example/creates/1","type":"Create","actor":"https://origin.example/users/alice","_misskey_content":"unsigned","signature":{"type":"RsaSignature2017","creator":"https://origin.example/users/alice#main-key"}}`)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, signedInboxPayload(t, key, body)),
	}))
	require.Len(t, rec.normalized, 1)
	got := rec.normalized[0]
	assert.Equal(t, compacted, got.Source(), "a forwarded activity must be processed from the compacted document")
	assert.Equal(t, "https://origin.example/creates/1#signed", got.Fields().ID)
	assert.True(t, guard.seen["https://origin.example/creates/1#signed"], "the replay guard must see the compacted document's id")
	assert.False(t, guard.seen["https://origin.example/creates/1"])
	assert.Equal(t, "relay", rec.signers[0].ID, "the HTTP signer (not the LD creator) is handed over")
}

// 署名の無い経路 (Headers 無し) は gate を通らないので、今どおり本文を渡す。
func TestInboxProcessor_LegacyPayloadDispatchesRawBody(t *testing.T) {
	rec := &normalizedRecorder{}
	p := processors.NewInboxProcessor(rec, nil, nil, nil, nil, nil)
	body := []byte(`{"id":"https://remote.example/follows/8","type":"Follow","actor":"https://remote.example/users/alice"}`)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, queue.InboxPayload{Body: body}),
	}))
	assert.Empty(t, rec.normalized)
	assert.Equal(t, 1, rec.rawCalls)
}

// #3610: 署名者 == actor の受信 1 件で、DB の往復は署名者の actor を引く 1 回だけ。
// 以前は publickeyExtra の見つからない問い合わせと、処理の冒頭での actor の
// 引き直しを合わせて 3 回だった。
func TestInboxProcessor_DirectActivityDBRoundTrips(t *testing.T) {
	p := newInboxPipeline(t)
	body := misskeyCreateBody(t, 0, 0)
	task := driver.RawTask{TypeName: queue.TaskTypeInbox, Body: mustEncode(t, signedInboxPayload(t, p.key, body))}
	// 1 回目は鍵のキャッシュを温める。
	require.NoError(t, p.inbox.Handle(context.Background(), task))
	p.resetCounters()

	require.NoError(t, p.inbox.Handle(context.Background(), task))
	assert.Equal(t, int64(1), p.userFinds.Load(), "the signer is looked up once and reused for the moderation check")
	assert.Zero(t, p.extra.finds.Load(), "a known-missing extra key must not be queried again")
	assert.Equal(t, int64(1), p.dbRoundTrips())
}
