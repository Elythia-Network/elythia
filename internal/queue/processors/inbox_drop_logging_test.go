package processors_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
)

// captureLogs swaps the default slog handler for the duration of the test.
//
// **JSON で取る。** text 形式だと `err` の中の `signer=...` と属性としての
// `signer=...` を区別できず、専用の属性を落とす変異を見逃す (#2716)。
//
// `slog.SetDefault` はプロセス global なので、このパッケージで `t.Parallel()` を
// 使わない前提に乗っている (現状 0 件)。
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// findLogRecord returns the first captured record whose msg contains want.
func findLogRecord(t *testing.T, buf *bytes.Buffer, want string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if msg, _ := rec["msg"].(string); strings.Contains(msg, want) {
			return rec
		}
	}
	t.Fatalf("log record containing %q not found in:\n%s", want, buf.String())
	return nil
}

// forwardedDropFixture builds the **production** shape: signer != body actor,
// LD verifier wired (router 側は必ず配線する)、LD-Signature の検証が失敗する。
//
// 実運用のログ 27/27 がこの分岐 (`ld-signature verify failed`) だった。
func forwardedDropFixture(t *testing.T, ldErr error) (*processors.InboxProcessor, *stubFedProcessor, queue.InboxPayload) {
	t.Helper()
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://relay.example/actor#main-key", priv)
	require.NoError(t, err)

	relayHost := "relay.example"
	originHost := "origin.example"
	verifier := &multiActorVerifier{
		pubKey: pub,
		byURI: map[string]*model.User{
			"https://relay.example/actor":      {ID: "relay", Host: &relayHost, URI: uptr("https://relay.example/actor")},
			"https://origin.example/users/mal": {ID: "mal", Host: &originHost, URI: uptr("https://origin.example/users/mal")},
		},
	}
	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)
	p.SetLDSignatureVerifier(&stubLDVerifier{present: true, err: ldErr})

	body := []byte(`{"id":"https://origin.example/notes/1/activity","type":"Create","actor":"https://origin.example/users/mal","signature":{"type":"RsaSignature2017","creator":"https://origin.example/users/mal#main-key"},"object":{"id":"https://origin.example/notes/1","type":"Note"}}`)
	return p, stub, signedInboxPayload(t, key, body)
}

// activity を捨てたときに**相手が判る**こと (#2716)。
//
// 実運用では 24 時間の警告の半分がこの破棄で、しかも `host=""` で相手が一切
// 残っていなかった。activity を捨てる経路なので、誤って落としていた場合に
// 気付く手段がこのログしか無い。
//
// **本番と同じ分岐を通す。** LD verifier を配線しないと
// `actor mismatch and no LD verifier` に落ちるが、router は必ず配線するので
// その分岐は本番に存在しない (#2724 review HIGH-1)。
func TestInboxProcessor_DropLogIdentifiesTheSender(t *testing.T) {
	buf := captureLogs(t)
	p, stub, payload := forwardedDropFixture(t, errors.New("ld-sig: signature verification failed: crypto/rsa: verification error"))
	require.NoError(t, p.Handle(context.Background(), queue.NewInboxTask(payload)))

	// **判定は変えていない。** 検証に失敗した転送活動は従来どおり捨てる。
	assert.Empty(t, stub.calls, "捨てるはずの activity が処理されている")

	rec := findLogRecord(t, buf, "dropping activity")
	assert.Equal(t, "relay.example", rec["host"], "署名者の host が出ていない")
	assert.Equal(t, "https://relay.example/actor", rec["signer"], "signer が出ていない")
	assert.Equal(t, "https://origin.example/users/mal", rec["actor"], "body の actor が出ていない")
	assert.Equal(t, "Create", rec["activityType"], "activity の type が出ていない")

	// **原因が切り落とされないこと。** `LastError.Message` は 200 rune で切られる
	// ので、signer / actor を error にも重ねると原因が落ちる (#2724 review MEDIUM-1)。
	errMsg, _ := rec["err"].(string)
	require.Contains(t, errMsg, "ld-signature verify failed")
	assert.Contains(t, errMsg, "crypto/rsa: verification error", "原因が error から落ちている")
	assert.Contains(t, errMsg, `creator="https://origin.example/users/mal#main-key"`)
	assert.LessOrEqual(t, len([]rune(errMsg)), 200,
		"LastError.Message の切り詰め (200 rune) で原因が落ちる長さになっている")
}

// 署名検証に失敗した破棄でも host が空にならないこと (#2724 review MEDIUM-3)。
//
// 同じ関数の中に `payload.Host` を出す破棄ログが 3 つ残っていた。
func TestInboxProcessor_SignatureFailureLogHasHost(t *testing.T) {
	buf := captureLogs(t)
	priv, _, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://relay.example/actor#main-key", priv)
	require.NoError(t, err)

	// 別の鍵ペアで検証させて失敗させる。
	_, otherPub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	relayHost := "relay.example"
	verifier := &multiActorVerifier{
		pubKey: otherPub,
		byURI: map[string]*model.User{
			"https://relay.example/actor": {ID: "relay", Host: &relayHost, URI: uptr("https://relay.example/actor")},
		},
	}
	p := processors.NewInboxProcessor(&stubFedProcessor{})
	p.SetSignatureVerifier(verifier)

	body := []byte(`{"id":"https://relay.example/a/1","type":"Follow","actor":"https://relay.example/actor"}`)
	payload := signedInboxPayload(t, key, body)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox, Body: mustEncode(t, payload),
	}))

	rec := findLogRecord(t, buf, "signature verification failed in worker")
	assert.Equal(t, "relay.example", rec["host"], "host が空のまま出ている")
}
