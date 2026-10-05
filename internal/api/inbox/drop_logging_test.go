package inbox

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureLogs swaps the default slog handler for the duration of the test.
//
// **JSON で取る。** text 形式だと `err` の中に偶然含まれる `keyId=...` と属性
// としての `keyId=...` を区別できず、専用の属性を落とす変異を見逃す (#2716 の
// worker 側テストと同じ理由)。
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

// admission で捨てたときに相手が判ること (#2725)。
//
// 本番と同じ配線 (enqueuer あり = fast write path) で、digest が body と
// 合わない = 署名を保ったまま body を差し替えられたリクエストを送る。401 で
// 捨てるのは従来どおりで、ログに host / keyId が乗ることだけを足した。
func TestInbox_AdmissionRejectLogIdentifiesTheSender(t *testing.T) {
	buf := captureLogs(t)
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	h, _, followingRepo := newHandler(t, pub)
	h.SetExpectedHost("example.com")
	enq := &recordingEnqueuer{}
	h.SetEnqueuer(enq)

	original := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)
	tampered := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/eve"}`)

	c, rec := newPost(t, tampered)
	req := c.Request()
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest(original),
		[]string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))

	// **判定は変えていない。** digest の合わない body は従来どおり 401 で捨てる。
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Empty(t, enq.calls, "捨てるはずの activity が enqueue されている")
	assert.Empty(t, followingRepo.Followings, "捨てるはずの activity が処理されている")

	logRec := findLogRecord(t, buf, "inbox admission rejected")
	assert.Equal(t, "remote.example", logRec["host"], "keyId 由来の host が出ていない")
	assert.Equal(t, "https://remote.example/users/alice#main-key", logRec["keyId"], "keyId が出ていない")
	assert.NotEmpty(t, logRec["err"], "破棄の理由が落ちている")
}

// actor 欠落で捨てたときに相手が判ること (#2725)。
//
// admission は通っているので parsed は非 nil。body の actor は読めないから
// 捨てているので、相手を示せるのは署名側だけ。
func TestInbox_NoActorRejectLogIdentifiesTheSender(t *testing.T) {
	buf := captureLogs(t)
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	h, _, _ := newHandler(t, pub)
	h.SetExpectedHost("example.com")
	enq := &recordingEnqueuer{}
	h.SetEnqueuer(enq)

	body := []byte(`{"type":"Create","object":{"id":"https://remote.example/notes/1","type":"Note"}}`)
	c, rec := newPost(t, body)
	req := c.Request()
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest(body),
		[]string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))

	// **判定は変えていない。** actor 無しは従来どおり 400 で、enqueue しない。
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, enq.calls, "actor 無しの activity が enqueue されている")

	logRec := findLogRecord(t, buf, "activity has no actor")
	assert.Equal(t, "remote.example", logRec["host"], "keyId 由来の host が出ていない")
	assert.Equal(t, "https://remote.example/users/alice#main-key", logRec["keyId"], "keyId が出ていない")
}

// Signature ヘッダが欠落 / malformed でも panic せず、属性は空で出ること。
//
// parsed が nil になるのはこの経路だけ。nil のまま host / keyId を導出するので、
// ここを守らないと**無認証で誰でも到達できる経路で nil deref する**。
//
// この場合は相手が判らないままになる (keyId だけ書いた header も、signature が
// 無いと ParseSignatureHeader が落ちるので拾わない)。header を 2 回 parse すれば
// 拾えるが、**署名として成立していない申告を相手として記録する**ことになるので
// しない。worker 側の hostFromSignatureKeyID も parse 失敗時は空を返す。
func TestInbox_AdmissionRejectLogWithoutParsableSignature(t *testing.T) {
	cases := []struct {
		name      string
		signature string
	}{
		{"absent", ""},
		{"malformed", "garbage"},
		{"keyId only", `keyId="https://remote.example/users/alice#main-key"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLogs(t)
			_, pub, err := activitypub.GenerateRSAKeypair()
			require.NoError(t, err)
			h, _, _ := newHandler(t, pub)
			h.SetExpectedHost("example.com")
			enq := &recordingEnqueuer{}
			h.SetEnqueuer(enq)

			body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice"}`)
			c, rec := newPost(t, body)
			if tc.signature != "" {
				c.Request().Header.Set("Signature", tc.signature)
			}
			c.Request().Host = "example.com"

			require.NoError(t, h.Inbox(c))
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Empty(t, enq.calls)

			logRec := findLogRecord(t, buf, "inbox admission rejected")
			assert.Equal(t, "", logRec["host"])
			assert.Equal(t, "", logRec["keyId"])
		})
	}
}

// keyId は署名検証を通る前の申告値なので、長さを相手に決めさせない (#2725)。
//
// ヘッダ上限 (1MiB) までいくらでも伸ばせるため、切らないとリクエスト 1 本で
// ログを膨らませられる。
func TestSignerLogAttrsAreBounded(t *testing.T) {
	long := "https://" + strings.Repeat("a", 4096) + ".example/users/alice#main-key"
	parsed := &activitypub.ParsedSignature{KeyID: long}

	assert.LessOrEqual(t, len([]rune(signerKeyIDOf(parsed))), maxLoggedSignerLen,
		"keyId が切られていない")
	assert.LessOrEqual(t, len([]rune(signerHostOf(parsed))), maxLoggedSignerLen,
		"host が切られていない")
}

// nil 安全性を単体でも固定する。
func TestSignerLogAttrsNilParsed(t *testing.T) {
	assert.Equal(t, "", signerHostOf(nil))
	assert.Equal(t, "", signerKeyIDOf(nil))
}

// keyId が URL として解釈できないときは host を空にする (keyId 側だけ出す)。
func TestSignerHostOf_UnparsableKeyID(t *testing.T) {
	parsed := &activitypub.ParsedSignature{KeyID: "://not a url"}
	assert.Equal(t, "", signerHostOf(parsed))
	assert.Equal(t, "://not a url", signerKeyIDOf(parsed))
}
