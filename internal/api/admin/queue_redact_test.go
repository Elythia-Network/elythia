package admin

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/queue"
)

// **deliver job の署名鍵が `admin/queue/jobs` から読めないこと。**
//
// 現在の producer は payload に鍵を詰めないが、この変更より前に積まれた job は
// 持っている。取得できれば任意のローカルユーザーとして署名付き連合リクエストを
// 偽造できるので、到達点でも伏せる (moderator + `read:admin:queue` で届く)。
func TestPackJobData_RedactsSigningKeys(t *testing.T) {
	raw := []byte(`{"inbox":"https://remote.example/inbox","keyId":"https://x/users/u1#main-key",` +
		`"keyPem":"-----BEGIN RSA PRIVATE KEY-----SECRET-----END RSA PRIVATE KEY-----",` +
		`"ed25519PrivPem":"-----BEGIN PRIVATE KEY-----ED-SECRET-----END PRIVATE KEY-----"}`)

	got := packJobData(&QueueTaskSummary{Type: "deliver", Payload: raw})
	encoded, err := json.Marshal(got)
	require.NoError(t, err)

	require.NotContainsf(t, string(encoded), "SECRET",
		"data から RSA 署名鍵が読める: %s", encoded)
	require.NotContainsf(t, string(encoded), "ED-SECRET",
		"data から Ed25519 署名鍵が読める: %s", encoded)
	// 鍵以外は残す — 運用で job の中身を見るための画面なので、伏せすぎると使えない。
	require.Contains(t, string(encoded), "https://remote.example/inbox")
	require.Contains(t, string(encoded), "#main-key")
}

func TestRedactPayloadSecrets(t *testing.T) {
	raw := []byte(`{"inbox":"https://remote.example/inbox","keyPem":"SECRET-RSA","ed25519PrivPem":"SECRET-ED"}`)
	got := redactPayloadSecrets(raw)
	require.NotContains(t, got, "SECRET-RSA")
	require.NotContains(t, got, "SECRET-ED")
	require.Contains(t, got, "https://remote.example/inbox")

	// 鍵を持たない payload は素通し (無駄な再シリアライズをしない)。
	plain := []byte(`{"inbox":"https://remote.example/inbox"}`)
	require.Equal(t, string(plain), redactPayloadSecrets(plain))

	// JSON でないものはそのまま (非 JSON payload の job がありうる)。
	require.Equal(t, "not-json", redactPayloadSecrets([]byte("not-json")))
	require.Equal(t, "", redactPayloadSecrets(nil))
}

// **redact の一覧が payload の json tag と一致していること。**
//
// `jobSecretKeys` は文字列の集合なので、`queue.DeliverPayload` の tag を
// 変えると**黙って redact が死ぬ** (全テスト緑のまま鍵が出る)。tag 側を
// 真とみなして突き合わせる。
func TestJobSecretKeysMatchPayloadTags(t *testing.T) {
	type spec struct {
		typ    reflect.Type
		fields []string
	}
	specs := []spec{
		{reflect.TypeOf(queue.DeliverPayload{}), []string{"KeyPEM", "Ed25519PrivPEM"}},
		{reflect.TypeOf(queue.WebhookPayload{}), []string{"OverrideSecret"}},
	}

	// **別 package の非公開 struct に由来するものは、ここでは名前で持つ。**
	// `internal/server` の `peerJob` は unexported なので reflect で引けない。
	// tag が変わっていないことは `internal/entitycompat` の
	// `TestPeerJobEnvelopeTagIsStable` が固定する (そちらが落ちればここも直す)。
	externalSecretTags := map[string]string{
		"envelope": "internal/server の peerJob.Envelope (プラグイン peer の送信本文)",
	}

	want := map[string]struct{}{}
	for tag := range externalSecretTags {
		want[tag] = struct{}{}
	}
	for _, sp := range specs {
		for _, name := range sp.fields {
			f, ok := sp.typ.FieldByName(name)
			require.Truef(t, ok, "%s.%s が無い (rename した? jobSecretKeys も直すこと)",
				sp.typ.Name(), name)
			tag := f.Tag.Get("json")
			if i := strings.IndexByte(tag, ','); i >= 0 {
				tag = tag[:i]
			}
			require.NotEmptyf(t, tag, "%s.%s に json tag が無い", sp.typ.Name(), name)
			want[tag] = struct{}{}
		}
	}

	for tag := range want {
		_, ok := jobSecretKeys[tag]
		require.Truef(t, ok,
			"%q が jobSecretKeys に無い。payload の json tag を変えたなら、"+
				"redact の一覧も直すこと (放置すると秘密が API から読める)", tag)
	}
	// 逆向き: 一覧に実在しない key が残っていないか (守っているつもりで
	// 何も伏せていない状態を作らない)。
	for key := range jobSecretKeys {
		_, ok := want[key]
		require.Truef(t, ok,
			"jobSecretKeys の %q に対応する payload field が無い。rename / 削除した?", key)
	}
}

// **API が実際に返す形で鍵が出ないこと。**
//
// `admin/queue/jobs` (search 経路を含む) / `show-job` のレスポンスは `data.body` とは
// **別に** mk-go 独自の `payload` フィールドでも payload を返す。`packJobData`
// だけを見ていると、そちらの redact を外す変更が緑で通る (実測で確認した)。
// この PR が塞ごうとしている露出そのものなので、到達点で固定する。
func TestPackTaskSummary_RedactsSecretsInAllFields(t *testing.T) {
	raw := []byte(`{"inbox":"https://remote.example/inbox","keyId":"https://x/users/u1#main-key",` +
		`"keyPem":"-----BEGIN RSA PRIVATE KEY-----RSA-SECRET-----END RSA PRIVATE KEY-----",` +
		`"ed25519PrivPem":"ED-SECRET","signerUserId":"u1"}`)

	got := packTaskSummary(&QueueTaskSummary{
		ID: "j1", Queue: "deliver", Type: "deliver", State: "waiting", Payload: raw,
	})
	require.NotNil(t, got)
	encoded, err := json.Marshal(got)
	require.NoError(t, err)

	for _, secret := range []string{"RSA-SECRET", "ED-SECRET"} {
		require.NotContainsf(t, string(encoded), secret,
			"レスポンスから署名鍵が読める (%s): %s", secret, encoded)
	}
	// 運用で job を追える程度の情報は残す。
	require.Contains(t, string(encoded), "https://remote.example/inbox")
	require.Contains(t, string(encoded), "signerUserId")

	// `payload` と `data` の**両方**を個別に見る (片方だけ直すのを防ぐ)。
	require.NotContainsf(t, got["payload"].(string), "RSA-SECRET",
		"payload フィールドから鍵が読める")
	dataJSON, err := json.Marshal(got["data"])
	require.NoError(t, err)
	require.NotContainsf(t, string(dataJSON), "RSA-SECRET", "data フィールドから鍵が読める")
}

// webhook の上書き secret も同じ経路で出ないこと。
// `admin/queue/jobs` は queue 名に allowlist を持たないので、**他人のものも
// 同じ endpoint から読める**。
func TestPackTaskSummary_RedactsWebhookSecret(t *testing.T) {
	raw := []byte(`{"webhookId":"w1","overrideUrl":"https://example/hook","overrideSecret":"HOOK-SECRET"}`)
	got := packTaskSummary(&QueueTaskSummary{ID: "j2", Queue: "webhook", Type: "webhook", Payload: raw})
	require.NotNil(t, got)
	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContainsf(t, string(encoded), "HOOK-SECRET",
		"レスポンスから webhook の secret が読める: %s", encoded)
	require.Contains(t, string(encoded), "https://example/hook")
}

// **プラグイン peer の送信本文が API から読めないこと (#3037)。**
//
// 中身を決めるのはプラグインで、本体はそれが何かを知らない。peer は本来
// 「そのプラグイン同士」の通信で、モデレーション画面に出す前提のものでは
// ない。Redis に再送のあいだ残り、`admin/queue/jobs` から (moderator +
// `read:admin:queue` で) 読めていた。
func TestPeerEnvelopeIsRedacted(t *testing.T) {
	raw := []byte(`{"host":"peer.example","sendId":"s1","envelope":{"secret":"plugin data"}}`)

	// mk-go 独自の `payload` フィールド側。
	out := redactPayloadSecrets(raw)
	assert.NotContains(t, out, "plugin data", "peer の送信本文がそのまま出ている")
	assert.Contains(t, out, redactedPlaceholder)
	// **伏せるのは envelope だけ。** host / sendId は追跡に要る。
	assert.Contains(t, out, "peer.example")
	assert.Contains(t, out, "s1")

	// `data.body` 側。
	body := redactJobSecrets(map[string]any{
		"host": "peer.example", "envelope": map[string]any{"secret": "plugin data"},
	})
	m, ok := body.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, redactedPlaceholder, m["envelope"])
	assert.Equal(t, "peer.example", m["host"])
}
