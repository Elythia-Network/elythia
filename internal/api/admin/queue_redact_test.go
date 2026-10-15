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
	got := redactPayloadSecrets(queue.TaskTypeDeliver, raw)
	require.NotContains(t, got, "SECRET-RSA")
	require.NotContains(t, got, "SECRET-ED")
	require.Contains(t, got, "https://remote.example/inbox")

	// 鍵を持たない payload は素通し (無駄な再シリアライズをしない)。
	plain := []byte(`{"inbox":"https://remote.example/inbox"}`)
	require.Equal(t, string(plain), redactPayloadSecrets(queue.TaskTypeDeliver, plain))

	// JSON でないものはそのまま (非 JSON payload の job がありうる)。
	require.Equal(t, "not-json", redactPayloadSecrets(queue.TaskTypeWebPush, []byte("not-json")))
	require.Equal(t, "", redactPayloadSecrets(queue.TaskTypeWebPush, nil))
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
		{reflect.TypeOf(queue.WebhookPayload{}), []string{"OverrideSecret", "OverrideURL"}},
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
	// テスト送信の送信先も伏せる (Discord などは URL が資格情報)。
	require.NotContainsf(t, string(encoded), "example/hook",
		"レスポンスから webhook の送信先が読める: %s", encoded)
	require.Contains(t, string(encoded), "w1")
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
	out := redactPayloadSecrets("plugin:demo:_peer", raw)
	assert.NotContains(t, out, "plugin data", "peer の送信本文がそのまま出ている")
	assert.Contains(t, out, redactedPlaceholder)
	// **伏せるのは envelope だけ。** host / sendId は追跡に要る。
	assert.Contains(t, out, "peer.example")
	assert.Contains(t, out, "s1")

	// `data.body` 側。
	body := redactJobSecrets("plugin:demo:_peer", map[string]any{
		"host": "peer.example", "envelope": map[string]any{"secret": "plugin data"},
	})
	m, ok := body.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, redactedPlaceholder, m["envelope"])
	assert.Equal(t, "peer.example", m["host"])
}

// **webhook の失敗の文面に載った送信先 URL が出ないこと。**
//
// processor は *url.Error の URL を落とすようにしたが、それより前に失敗した job は
// URL 入りの文面を持っている。failedReason / lastErr / stacktrace のどれからも
// 読めず、検索 (packTaskSummary の結果に対して行う) でも当たらないこと。
func TestPackTaskSummary_RedactsWebhookErrorURL(t *testing.T) {
	const reason = `Post "https://hook.example/api/webhooks/1/SECRET-TOKEN": dial tcp 192.0.2.1:443: connect: connection refused`
	for _, typ := range []string{queue.TaskTypeUserWebhook, queue.TaskTypeSystemWebhook} {
		t.Run(typ, func(t *testing.T) {
			got := packTaskSummary(&QueueTaskSummary{
				ID: "j1", Queue: "webhook", Type: typ, State: "failed",
				Payload:    []byte(`{"webhookId":"w1"}`),
				LastErr:    reason,
				Stacktrace: []string{reason},
			})
			encoded, err := json.Marshal(got)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), "SECRET-TOKEN")
			assert.NotContains(t, string(encoded), "hook.example")
			// 原因は残す。
			assert.Equal(t, `Post [redacted]: dial tcp 192.0.2.1:443: connect: connection refused`, got["failedReason"])
			assert.Equal(t, got["failedReason"], got["lastErr"])
			assert.Equal(t, []string{`Post [redacted]: dial tcp 192.0.2.1:443: connect: connection refused`}, got["stacktrace"])
			assert.False(t, jobMatchesSearch(got, searchTerms("secret-token")), "URL で検索に当たってはいけない")
		})
	}

	// webhook 以外の job の文面は変えない (配送先の inbox は資格情報ではなく、追跡に要る)。
	other := `Post "https://remote.example/inbox": context deadline exceeded`
	got := packTaskSummary(&QueueTaskSummary{ID: "j2", Queue: "deliver", Type: queue.TaskTypeDeliver, LastErr: other})
	assert.Equal(t, other, got["failedReason"])
}

// **Web Push の通知の中身が admin/queue から読めないこと。**
//
// チャットやダイレクトのノートの本文が入り、宛先の本人にしか見せないもの。
// data / payload のどちらからも読めず、検索 (packTaskSummary の結果に対して
// 行う) でも当たらないこと。宛先と種類は残す。
func TestPackTaskSummary_RedactsWebPushBody(t *testing.T) {
	raw, err := json.Marshal(queue.WebPushPayload{
		UserID: "u1", Type: "newChatMessage",
		Body: json.RawMessage(`{"type":"newChatMessage","body":{"text":"PRIVATE-CHAT-TEXT"}}`),
	})
	require.NoError(t, err)

	got := packTaskSummary(&QueueTaskSummary{ID: "j1", Queue: "webpush", Type: queue.TaskTypeWebPush, Payload: raw})
	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "PRIVATE-CHAT-TEXT")
	assert.NotContains(t, got["payload"].(string), "PRIVATE-CHAT-TEXT", "payload フィールドから本文が読める")
	dataJSON, err := json.Marshal(got["data"])
	require.NoError(t, err)
	assert.NotContains(t, string(dataJSON), "PRIVATE-CHAT-TEXT", "data フィールドから本文が読める")
	assert.Contains(t, string(encoded), `"userId":"u1"`)
	assert.Contains(t, string(encoded), "newChatMessage")
	assert.False(t, jobMatchesSearch(got, searchTerms("private-chat-text")), "本文で検索に当たってはいけない")
	assert.True(t, jobMatchesSearch(got, searchTerms("u1")), "宛先では検索できる")

	// 同じ key でも webhook の送信本文は伏せない (upstream も job data に持つ)。
	hook := packTaskSummary(&QueueTaskSummary{
		ID: "j2", Queue: "webhook", Type: queue.TaskTypeUserWebhook,
		Payload: []byte(`{"webhookId":"w1","body":{"text":"hook body"}}`),
	})
	hookJSON, err := json.Marshal(hook)
	require.NoError(t, err)
	assert.Contains(t, string(hookJSON), "hook body")
}

// jobSecretKeysByType の key が payload の json tag と一致していること
// (tag を変えると黙って伏せなくなるため)。
func TestJobSecretKeysByTypeMatchPayloadTags(t *testing.T) {
	f, ok := reflect.TypeOf(queue.WebPushPayload{}).FieldByName("Body")
	require.True(t, ok, "WebPushPayload.Body が無い (rename した? jobSecretKeysByType も直すこと)")
	tag := strings.SplitN(f.Tag.Get("json"), ",", 2)[0]
	assert.Equal(t, map[string]map[string]struct{}{queue.TaskTypeWebPush: {tag: {}}}, jobSecretKeysByType)
}
