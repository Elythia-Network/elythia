package federation_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/activitypub/ld"

	corefederation "github.com/shiroha-a/mk/internal/core/federation"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLDSignatureVerifier_NoSignature_NoOp(t *testing.T) {
	repo := testutil.NewMockUserPublickeyRepository()
	v := corefederation.NewLDSignatureVerifier(repo)

	// signature field 無し → skip。HTTP Signature 経由でしか認証していない
	// 既存挙動の activity でも本 verifier は素通しすること (= 後方互換)。
	err := v.VerifyIfPresent([]byte(`{
		"@context": "https://www.w3.org/ns/activitystreams",
		"type": "Note",
		"id": "https://example.com/notes/n1"
	}`))
	require.NoError(t, err)
}

func TestLDSignatureVerifier_ForbiddenDirective_Rejected(t *testing.T) {
	repo := testutil.NewMockUserPublickeyRepository()
	// pubkey は適当に登録しておく (= forbidden directive で先に落ちて pubkey
	// resolve まで到達しないことを確認したい)。
	v := corefederation.NewLDSignatureVerifier(repo)

	err := v.VerifyIfPresent([]byte(`{
		"@context": "https://www.w3.org/ns/activitystreams",
		"type": "Note",
		"@graph": [{"type": "Note"}],
		"signature": {
			"type": "RsaSignature2017",
			"creator": "https://example.com/users/alice#main-key",
			"signatureValue": "AAAA"
		}
	}`))
	require.Error(t, err)
	// hardening の forbidden directive で reject される (= ld.ErrForbiddenDirective)。
	assert.Contains(t, err.Error(), "forbidden")
}

func TestLDSignatureVerifier_MissingCreator_Rejected(t *testing.T) {
	repo := testutil.NewMockUserPublickeyRepository()
	v := corefederation.NewLDSignatureVerifier(repo)

	err := v.VerifyIfPresent([]byte(`{
		"type": "Note",
		"signature": {
			"type": "RsaSignature2017",
			"signatureValue": "AAAA"
		}
	}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "creator missing")
}

// signature field がオブジェクトでない (例: 文字列) 場合は reject。present=true
// を報告しつつ error を返す (VerifyAndCompact 経路)。
func TestLDSignatureVerifier_SignatureNotObject_Rejected(t *testing.T) {
	repo := testutil.NewMockUserPublickeyRepository()
	v := corefederation.NewLDSignatureVerifier(repo)

	verified, present, err := v.VerifyAndCompact([]byte(`{"type":"Note","signature":"not-an-object"}`))
	require.Error(t, err)
	assert.True(t, present, "signature field exists, so present must be true")
	assert.Empty(t, verified.Creator)
	assert.Nil(t, verified.Body)
	assert.Contains(t, err.Error(), "not an object")
}

func TestLDSignatureVerifier_UnknownCreator_Rejected(t *testing.T) {
	repo := testutil.NewMockUserPublickeyRepository()
	v := corefederation.NewLDSignatureVerifier(repo)

	err := v.VerifyIfPresent([]byte(`{
		"type": "Note",
		"signature": {
			"type": "RsaSignature2017",
			"creator": "https://example.com/users/unknown#main-key",
			"created": "` + ldFreshCreated() + `",
			"signatureValue": "AAAA"
		}
	}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "public key not found")
}

func TestLDSignatureVerifier_BadSignatureValueRejected(t *testing.T) {
	repo := testutil.NewMockUserPublickeyRepository()
	// 本物の RSA キーを生成して登録するが、signatureValue は invalid base64
	// なので verify は必ず失敗する shape。
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	require.NoError(t, err)
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	keyID := "https://example.com/users/alice#main-key"
	repo.Keys["alice"] = &model.UserPublickey{
		UserID: "alice",
		KeyID:  keyID,
		KeyPEM: pubPEM,
	}

	v := corefederation.NewLDSignatureVerifier(repo)
	err = v.VerifyIfPresent([]byte(`{
		"@context": "https://www.w3.org/ns/activitystreams",
		"type": "Note",
		"id": "https://example.com/notes/n1",
		"signature": {
			"type": "RsaSignature2017",
			"creator": "https://example.com/users/alice#main-key",
			"created": "` + ldFreshCreated() + `",
			"signatureValue": "AAAA"
		}
	}`))
	require.Error(t, err)
	// rsa.VerifyPKCS1v15 経由の verify mismatch。
	assert.NotContains(t, err.Error(), "public key not found",
		"public key は resolve できた状態で signature verify で fail することを確認")
	assert.ErrorIs(t, err, ld.ErrSignatureMismatch, "created の窓より手前で落ちていないこと")
}

func TestLDSignatureVerifier_NilRepo_NoOp(t *testing.T) {
	// 防御的: pubkeyRepo nil でも panic せず素通し (= verify 経路を skip)。
	v := corefederation.NewLDSignatureVerifier(nil)
	err := v.VerifyIfPresent([]byte(`{"type":"Note"}`))
	require.NoError(t, err)
}

// LDSignatureVerifier interface 互換性 (= inbox processor 経由で wire される
// shape) を最低限 confirm する。
func TestLDSignatureVerifier_InterfaceShape(t *testing.T) {
	repo := testutil.NewMockUserPublickeyRepository()
	v := corefederation.NewLDSignatureVerifier(repo)
	// メソッド存在 (compile time check)
	_ = v.VerifyIfPresent
	assert.NotNil(t, v, "verifier must be non-nil")
}

// #2106 N26: CheckForbiddenDirectivesIfPresent は signature 無しを素通しする。
func TestLDSignatureVerifier_CheckForbidden_NoSignature(t *testing.T) {
	v := corefederation.NewLDSignatureVerifier(testutil.NewMockUserPublickeyRepository())
	require.NoError(t, v.CheckForbiddenDirectivesIfPresent([]byte(`{"type":"Note","id":"https://example.com/n1"}`)))
}

// #2106 N26: forbidden directive は CheckForbiddenDirectivesIfPresent でも reject される。
func TestLDSignatureVerifier_CheckForbidden_ForbiddenDirective(t *testing.T) {
	v := corefederation.NewLDSignatureVerifier(testutil.NewMockUserPublickeyRepository())
	err := v.CheckForbiddenDirectivesIfPresent([]byte(`{
		"type":"Note","@graph":[{"type":"Note"}],
		"signature":{"type":"RsaSignature2017","creator":"https://example.com/users/alice#main-key","signatureValue":"AAAA"}
	}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "forbidden")
}

// #2106 N26: forbidden directive が無ければ、creator 鍵が未解決でも (= VerifyIfPresent なら
// verify 失敗で drop する状況でも) CheckForbiddenDirectivesIfPresent は nil を返す。
func TestLDSignatureVerifier_CheckForbidden_UnresolvableKeyStillPasses(t *testing.T) {
	v := corefederation.NewLDSignatureVerifier(testutil.NewMockUserPublickeyRepository())
	body := []byte(`{
		"@context":"https://www.w3.org/ns/activitystreams","type":"Note",
		"signature":{"type":"RsaSignature2017","creator":"https://example.com/users/alice#main-key","signatureValue":"AAAA"}
	}`)
	require.Error(t, v.VerifyIfPresent(body), "VerifyIfPresent は creator 鍵未解決で error")
	require.NoError(t, v.CheckForbiddenDirectivesIfPresent(body), "forbidden 無し + 鍵不要なので nil")
}

// #2106 N26: 不正 JSON は CheckForbiddenDirectivesIfPresent でも error。
func TestLDSignatureVerifier_CheckForbidden_BadJSON(t *testing.T) {
	v := corefederation.NewLDSignatureVerifier(testutil.NewMockUserPublickeyRepository())
	require.Error(t, v.CheckForbiddenDirectivesIfPresent([]byte(`{not json`)))
}

// #2680: **正しい署名が通ることを確認する。**
//
// これまで本ファイルには test 関数が 12 本あったが、**署名検証が成功する経路を
// 通るものが 1 本も無かった** (拒否を確認するものが 9 本、nil を返すことを
// 確認するものが 3 本 — いずれも verify 本体に到達しないか、失敗を期待する)。
// そのため `loadDocument` が preload まで freeze で塞いで LD-Signature 検証が
// 常に失敗するようになっても、バグも「拒否」を返す以上テストは緑のままだった。
//
// verifier は `NewProcessor()` の空 cache のまま `Freeze()` してから verify する
// ので、本テストは production と同じ順序を通る。
func TestLDSignatureVerifier_ValidSignatureAccepted(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	require.NoError(t, err)
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
	privPEM := string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv),
	}))

	keyID := "https://example.com/users/alice#main-key"
	repo := testutil.NewMockUserPublickeyRepository()
	repo.Keys["alice"] = &model.UserPublickey{UserID: "alice", KeyID: keyID, KeyPEM: pubPEM}

	signed, err := ld.NewProcessor().SignRsaSignature2017(map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"type":     "Note",
		"id":       "https://example.com/notes/n1",
		"content":  "hello",
	}, privPEM, keyID, time.Now())
	require.NoError(t, err)

	body, err := json.Marshal(signed)
	require.NoError(t, err)

	require.NoError(t, corefederation.NewLDSignatureVerifier(repo).VerifyIfPresent(body),
		"正しい LD-Signature が検証を通らない")
}

// #2680: **verifier が Freeze すること自体を固定する。**
//
// この PR は「freeze は維持したまま preload だけ通す」ものなので、freeze が
// 外れると主旨が失われる。ところが `proc.Freeze()` を消しても他のテストは
// すべて緑のままだった。context 問題に当たった人が Freeze() を消して緑にする、
// という直し方を塞ぐ。
//
// 現状 mk-go に remote fetch 経路は無いので即時の security 影響は無いが、
// fetch fallback を足したときに freeze が唯一の防壁になる。
func TestLDSignatureVerifier_NonPreloadedContextRejectedByFreeze(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	require.NoError(t, err)
	pubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))

	keyID := "https://example.com/users/alice#main-key"
	repo := testutil.NewMockUserPublickeyRepository()
	repo.Keys["alice"] = &model.UserPublickey{UserID: "alice", KeyID: keyID, KeyPEM: pubPEM}

	// preload に無い remote context を参照する activity。署名の正否によらず、
	// context を引けない時点で reject されなければならない。
	body := []byte(`{
		"@context": ["https://www.w3.org/ns/activitystreams", "https://litepub.social/litepub/context.jsonld"],
		"type": "Create",
		"id": "https://example.com/notes/n1/activity",
		"signature": {
			"type": "RsaSignature2017",
			"creator": "https://example.com/users/alice#main-key",
			"signatureValue": "AAAA"
		}
	}`)

	err = corefederation.NewLDSignatureVerifier(repo).VerifyIfPresent(body)
	require.Error(t, err, "preload 外の context は freeze で弾かれること")
	assert.True(t, errors.Is(err, ld.ErrCacheFrozen),
		"freeze による拒否であること (got %v)", err)
}

// #3121: 鍵が引けなかったのを「鍵が無い」に潰さない。潰すと呼び出し側が
// LD-Signature の検証失敗として activity を drop するので、DB 障害のあいだ
// 届いた転送 activity がまるごと失われる。
func TestLDSignatureVerifier_KeyLookupFailureIsLookupUnavailable(t *testing.T) {
	repo := testutil.NewMockUserPublickeyRepository()
	boom := errors.New("connection refused")
	repo.FindByKeyIDErr = boom
	v := corefederation.NewLDSignatureVerifier(repo)

	body := []byte(`{
		"type": "Note",
		"signature": {
			"type": "RsaSignature2017",
			"creator": "https://example.com/users/alice#main-key",
			"created": "` + ldFreshCreated() + `",
			"signatureValue": "AAAA"
		}
	}`)
	err := v.VerifyIfPresent(body)
	require.ErrorIs(t, err, corefederation.ErrLookupUnavailable, "DB 障害を「鍵が無い」に潰している")

	// **逆向き。** not-found は従来どおり「鍵が無い」として drop する。
	repo.FindByKeyIDErr = nil
	err = v.VerifyIfPresent(body)
	require.Error(t, err)
	assert.NotErrorIs(t, err, corefederation.ErrLookupUnavailable)
	assert.Contains(t, err.Error(), "public key not found")
}

// ldFreshCreated returns a `signature.created` value inside the accepted
// window, for fixtures that must get past the window check.
func ldFreshCreated() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// ldTestKey registers a fresh RSA key for alice and returns its private PEM.
func ldTestKey(t *testing.T, repo *testutil.MockUserPublickeyRepository) (keyID, privPEM string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	require.NoError(t, err)
	keyID = "https://example.com/users/alice#main-key"
	repo.Keys["alice"] = &model.UserPublickey{
		UserID: "alice", KeyID: keyID,
		KeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})),
	}
	privPEM = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)}))
	return keyID, privPEM
}

// **署名が覆っていないキーは compact 後の Body に短い名前で残らない。**
// Mastodon の context は `_misskey_content` を定義しないので、AS2 の
// `@vocab: "_:"` で blank node IRI になり署名に含まれない。署名は通るが、
// Body の object に `_misskey_content` キーがあってはならない。
func TestLDSignatureVerifier_CompactDropsUnsignedKeys(t *testing.T) {
	repo := testutil.NewMockUserPublickeyRepository()
	keyID, privPEM := ldTestKey(t, repo)

	signed, err := ld.NewProcessor().SignRsaSignature2017(map[string]any{
		"@context": []any{"https://www.w3.org/ns/activitystreams", map[string]any{"sensitive": "as:sensitive"}},
		"id":       "https://example.com/notes/n1/activity",
		"type":     "Create",
		"actor":    "https://example.com/users/alice",
		"to":       []any{"https://www.w3.org/ns/activitystreams#Public"},
		"object": map[string]any{
			"id":        "https://example.com/notes/n1",
			"type":      "Note",
			"content":   "<p>signed</p>",
			"sensitive": false,
		},
	}, privPEM, keyID, time.Now())
	require.NoError(t, err)
	signed["object"].(map[string]any)["_misskey_content"] = "forged"
	signed["object"].(map[string]any)["quoteUrl"] = "https://evil.example/notes/x"
	body, err := json.Marshal(signed)
	require.NoError(t, err)

	verified, present, err := corefederation.NewLDSignatureVerifier(repo).VerifyAndCompact(body)
	require.NoError(t, err, "署名外のキーを足しても LD-Signature 自体は通る (だから compact が要る)")
	require.True(t, present)
	assert.Equal(t, keyID, verified.Creator)

	var got map[string]any
	require.NoError(t, json.Unmarshal(verified.Body, &got))
	obj, ok := got["object"].(map[string]any)
	require.True(t, ok, "object が残っていない: %s", verified.Body)
	assert.NotContains(t, obj, "_misskey_content", "署名外の _misskey_content が残っている")
	assert.NotContains(t, obj, "quoteUrl", "署名外の quoteUrl が残っている")
	assert.Equal(t, "<p>signed</p>", obj["content"])
	assert.Equal(t, false, obj["sensitive"])
	assert.Equal(t, "as:Public", got["to"], "upstream と同じ compact 形になること")
	assert.Equal(t, "https://example.com/users/alice", got["actor"])
	sig, ok := got["signature"].(map[string]any)
	require.True(t, ok, "signature が付け直されていない")
	assert.Equal(t, keyID, sig["creator"])
}

// 署名した語彙 (context で定義された `_misskey_content`) は compact 後も残る。
func TestLDSignatureVerifier_CompactKeepsSignedExtensionKeys(t *testing.T) {
	repo := testutil.NewMockUserPublickeyRepository()
	keyID, privPEM := ldTestKey(t, repo)

	signed, err := ld.NewProcessor().SignRsaSignature2017(map[string]any{
		"@context": []any{
			"https://www.w3.org/ns/activitystreams",
			map[string]any{"misskey": "https://misskey-hub.net/ns#", "_misskey_content": "misskey:_misskey_content"},
		},
		"id":               "https://example.com/notes/n1",
		"type":             "Note",
		"_misskey_content": "signed **mfm**",
	}, privPEM, keyID, time.Now())
	require.NoError(t, err)
	body, err := json.Marshal(signed)
	require.NoError(t, err)

	verified, _, err := corefederation.NewLDSignatureVerifier(repo).VerifyAndCompact(body)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(verified.Body, &got))
	assert.Equal(t, "signed **mfm**", got["_misskey_content"])
}

// signature.created の窓 (replay 対策)。
func TestLDSignatureVerifier_CreatedWindow(t *testing.T) {
	repo := testutil.NewMockUserPublickeyRepository()
	keyID, privPEM := ldTestKey(t, repo)
	v := corefederation.NewLDSignatureVerifier(repo)
	doc := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       "https://example.com/notes/n1",
		"type":     "Note",
		"content":  "hello",
	}
	sign := func(t *testing.T, created time.Time) []byte {
		t.Helper()
		signed, err := ld.NewProcessor().SignRsaSignature2017(doc, privPEM, keyID, created)
		require.NoError(t, err)
		b, err := json.Marshal(signed)
		require.NoError(t, err)
		return b
	}

	tests := []struct {
		name    string
		created time.Time
		wantErr bool
	}{
		{name: "just signed", created: time.Now()},
		{name: "six days old", created: time.Now().Add(-6 * 24 * time.Hour)},
		{name: "slightly ahead", created: time.Now().Add(30 * time.Minute)},
		{name: "eight days old", created: time.Now().Add(-8 * 24 * time.Hour), wantErr: true},
		{name: "two hours ahead", created: time.Now().Add(2 * time.Hour), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := v.VerifyIfPresent(sign(t, tc.created))
			if tc.wantErr {
				require.ErrorIs(t, err, corefederation.ErrLDSignatureExpired)
				return
			}
			require.NoError(t, err)
		})
	}

	// created が文字列でない / 読めない形式は、窓の判定ができないので拒否する。
	for name, created := range map[string]string{
		"not a string": `12345`,
		"unparseable":  `"yesterday"`,
	} {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{
				"@context": "https://www.w3.org/ns/activitystreams",
				"type": "Note",
				"signature": {
					"type": "RsaSignature2017",
					"creator": "` + keyID + `",
					"created": ` + created + `,
					"signatureValue": "AAAA"
				}
			}`)
			require.ErrorIs(t, v.VerifyIfPresent(body), corefederation.ErrLDSignatureExpired)
		})
	}
}

// **created は署名された RDF から読む。** `created` を `dc:created` (型付き) や
// 完全 IRI へ移しても RDF は同じなので署名は通る。JSON のキーだけを見る判定は
// これを「欠落」と読み、古い署名の窓を外せた。
func TestLDSignatureVerifier_CreatedWindowSeesAliasedKeys(t *testing.T) {
	repo := testutil.NewMockUserPublickeyRepository()
	keyID, privPEM := ldTestKey(t, repo)
	v := corefederation.NewLDSignatureVerifier(repo)
	doc := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       "https://example.com/notes/n1",
		"type":     "Note",
		"content":  "hello",
	}
	aliases := map[string]func(sig map[string]any, created string){
		"dc:created": func(sig map[string]any, created string) {
			sig["dc:created"] = map[string]any{"@value": created, "@type": "xsd:dateTime"}
		},
		"absolute IRI": func(sig map[string]any, created string) {
			sig["http://purl.org/dc/terms/created"] = map[string]any{
				"@value": created, "@type": "http://www.w3.org/2001/XMLSchema#dateTime",
			}
		},
	}
	for name, move := range aliases {
		for _, tc := range []struct {
			age     time.Duration
			wantErr bool
		}{
			{age: 0},
			{age: 400 * 24 * time.Hour, wantErr: true},
		} {
			t.Run(fmt.Sprintf("%s/age=%s", name, tc.age), func(t *testing.T) {
				signed, err := ld.NewProcessor().SignRsaSignature2017(doc, privPEM, keyID, time.Now().Add(-tc.age))
				require.NoError(t, err)
				sig := signed["signature"].(map[string]any)
				created := sig["created"].(string)
				delete(sig, "created")
				move(sig, created)
				body, err := json.Marshal(signed)
				require.NoError(t, err)

				err = v.VerifyIfPresent(body)
				if tc.wantErr {
					require.ErrorIs(t, err, corefederation.ErrLDSignatureExpired)
					return
				}
				require.NoError(t, err, "RDF として同じ署名を落としている")
			})
		}
	}
}

// 転送経路では created を必須にする。created の無い署名は鮮度を持たず、
// 一度受け取れば無期限に再送できる。複数ある / 日時として読めない形も拒否する。
func TestLDSignatureVerifier_CreatedRequiredAndUnambiguous(t *testing.T) {
	repo := testutil.NewMockUserPublickeyRepository()
	keyID, _ := ldTestKey(t, repo)
	v := corefederation.NewLDSignatureVerifier(repo)
	fresh := ldFreshCreated()
	tests := map[string]string{
		"missing": ``,
		"two values": `"created": "` + fresh + `",
					"dc:created": {"@value": "2020-01-01T00:00:00Z", "@type": "xsd:dateTime"},`,
		"node reference": `"created": {"@id": "https://example.com/t"},`,
	}
	for name, created := range tests {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{
				"@context": "https://www.w3.org/ns/activitystreams",
				"type": "Note",
				"signature": {
					"type": "RsaSignature2017",
					"creator": "` + keyID + `",
					` + created + `
					"signatureValue": "AAAA"
				}
			}`)
			require.ErrorIs(t, v.VerifyIfPresent(body), corefederation.ErrLDSignatureExpired)
		})
	}

	// options を正規化できない形は窓の判定に進めず拒否する。
	body := []byte(`{
		"@context": "https://www.w3.org/ns/activitystreams",
		"type": "Note",
		"signature": {
			"type": "RsaSignature2017",
			"creator": "` + keyID + `",
			"created": {"@value": {"nested": true}},
			"signatureValue": "AAAA"
		}
	}`)
	err := v.VerifyIfPresent(body)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "signature options")
}

// **compact 後の文書にも forbidden directive の検査を掛ける。** 生の文書の検査は
// キー名しか見ないので、inline context で `"g": "@graph"` のような別名を付けると
// 素通りする。compact は別名を InboxCompactContext の形 (= `@graph` /
// `@included` そのもの) に戻すので、そこで捕まえる。署名は正しく付けてある —
// compact 後の検査を外すと verify まで進んで受理される形にしてある。
func TestLDSignatureVerifier_ForbiddenDirectiveUnderAliasRejectedAfterCompact(t *testing.T) {
	repo := testutil.NewMockUserPublickeyRepository()
	keyID, privPEM := ldTestKey(t, repo)
	v := corefederation.NewLDSignatureVerifier(repo)

	tests := map[string]map[string]any{
		"@graph": {
			"@context": []any{"https://www.w3.org/ns/activitystreams", map[string]any{"g": "@graph"}},
			"g": []any{
				map[string]any{"id": "https://example.com/notes/a", "type": "Note", "content": "a"},
				map[string]any{"id": "https://example.com/notes/b", "type": "Note", "content": "b"},
			},
		},
		"@included": {
			"@context": []any{"https://www.w3.org/ns/activitystreams", map[string]any{"inc": "@included"}},
			"id":       "https://example.com/notes/a",
			"type":     "Note",
			"content":  "a",
			"inc": []any{
				map[string]any{"id": "https://example.com/notes/b", "type": "Note", "content": "b"},
			},
		},
	}
	for name, doc := range tests {
		t.Run(name, func(t *testing.T) {
			signed, err := ld.NewProcessor().SignRsaSignature2017(doc, privPEM, keyID, time.Now())
			require.NoError(t, err)
			body, err := json.Marshal(signed)
			require.NoError(t, err)
			require.NotContains(t, string(body), `"`+name+`":`,
				"生の文書に directive がキーとして出ていると別名の検査にならない")

			require.ErrorIs(t, v.VerifyIfPresent(body), ld.ErrForbiddenDirective)
		})
	}
}
