package captcha_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elythia-network/elythia/internal/core/captcha"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- siteVerify providers (hCaptcha, reCAPTCHA, Turnstile) ---

func newSiteVerifyServer(t *testing.T, wantSecret string, respond func(secret, token string) any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		resp := respond(r.FormValue("secret"), r.FormValue("response"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestHcaptcha_Success(t *testing.T) {
	srv := newSiteVerifyServer(t, "sec", func(_, _ string) any {
		return map[string]any{"success": true}
	})
	defer srv.Close()

	v := captcha.NewHcaptchaWithClient("sec", srv.Client())
	// siteVerifier は verifyURL を上書きできないので、内部テストは Turnstile 経由で代理する。
	// ここでは NewHcaptchaWithClient が生成できることだけ確認。
	assert.NotNil(t, v)
}

func TestTurnstile_Success(t *testing.T) {
	srv := newSiteVerifyServer(t, "sec", func(secret, token string) any {
		if secret == "sec" && token == "ok-token" {
			return map[string]any{"success": true}
		}
		return map[string]any{"success": false, "error-codes": []string{"invalid-input-response"}}
	})
	defer srv.Close()

	v := captcha.NewTurnstileWithURL("sec", srv.URL, srv.Client())
	require.NoError(t, v.Verify(context.Background(), "ok-token"))
}

func TestTurnstile_Failure(t *testing.T) {
	srv := newSiteVerifyServer(t, "sec", func(_, _ string) any {
		return map[string]any{"success": false, "error-codes": []string{"bad-request"}}
	})
	defer srv.Close()

	v := captcha.NewTurnstileWithURL("sec", srv.URL, srv.Client())
	err := v.Verify(context.Background(), "bad-token")
	assert.ErrorIs(t, err, captcha.ErrVerificationFail)
}

func TestSiteVerify_EmptyToken(t *testing.T) {
	v := captcha.NewHcaptchaWithClient("sec", http.DefaultClient)
	err := v.Verify(context.Background(), "")
	assert.ErrorIs(t, err, captcha.ErrNoResponse)
}

// --- mCaptcha ---

func TestMcaptcha_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["key"] == "site" && body["secret"] == "sec" && body["token"] == "good" {
			_ = json.NewEncoder(w).Encode(map[string]any{"valid": true})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"valid": false})
		}
	}))
	defer srv.Close()

	v := captcha.NewMcaptchaWithClient(srv.URL, "site", "sec", srv.Client())
	require.NoError(t, v.Verify(context.Background(), "good"))
}

func TestMcaptcha_InvalidToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"valid": false})
	}))
	defer srv.Close()

	v := captcha.NewMcaptchaWithClient(srv.URL, "site", "sec", srv.Client())
	err := v.Verify(context.Background(), "wrong")
	assert.ErrorIs(t, err, captcha.ErrVerificationFail)
}

func TestMcaptcha_EmptyToken(t *testing.T) {
	v := captcha.NewMcaptcha("http://localhost", "site", "sec")
	err := v.Verify(context.Background(), "")
	assert.ErrorIs(t, err, captcha.ErrNoResponse)
}

func TestMcaptcha_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	v := captcha.NewMcaptchaWithClient(srv.URL, "site", "sec", srv.Client())
	err := v.Verify(context.Background(), "token")
	// 非200は instance への到達失敗扱い (upstream verifyMcaptcha と同じ requestFailed)。
	assert.ErrorIs(t, err, captcha.ErrRequestFailed)
}

// --- testcaptcha ---

func TestTestcaptcha_Success(t *testing.T) {
	v := captcha.NewTestcaptcha()
	require.NoError(t, v.Verify(context.Background(), "testcaptcha-passed"))
}

func TestTestcaptcha_WrongToken(t *testing.T) {
	v := captcha.NewTestcaptcha()
	err := v.Verify(context.Background(), "wrong-value")
	assert.ErrorIs(t, err, captcha.ErrVerificationFail)
}

func TestTestcaptcha_EmptyToken(t *testing.T) {
	v := captcha.NewTestcaptcha()
	err := v.Verify(context.Background(), "")
	assert.ErrorIs(t, err, captcha.ErrNoResponse)
}

// --- Service ---

func TestService_NoProviderEnabled(t *testing.T) {
	svc := captcha.NewService(&model.Meta{})
	require.NoError(t, svc.Verify(context.Background(), captcha.CaptchaTokens{}))
}

func TestService_TestcaptchaEnabled(t *testing.T) {
	svc := captcha.NewService(&model.Meta{EnableTestcaptcha: true})
	require.NoError(t, svc.Verify(context.Background(), captcha.CaptchaTokens{Testcaptcha: "testcaptcha-passed"}))

	err := svc.Verify(context.Background(), captcha.CaptchaTokens{Testcaptcha: "wrong"})
	assert.ErrorIs(t, err, captcha.ErrVerificationFail)
}

func TestService_HcaptchaEnabledButNoSecret(t *testing.T) {
	// secret 未設定なら provider は構築されない → captcha スキップ
	svc := captcha.NewService(&model.Meta{EnableHcaptcha: true})
	require.NoError(t, svc.Verify(context.Background(), captcha.CaptchaTokens{}))
}

func TestService_HcaptchaEnabled(t *testing.T) {
	secret := "hcap-secret"
	svc := captcha.NewService(&model.Meta{
		EnableHcaptcha:    true,
		HcaptchaSecretKey: &secret,
	})
	// token が空なので ErrNoResponse
	err := svc.Verify(context.Background(), captcha.CaptchaTokens{})
	assert.ErrorIs(t, err, captcha.ErrNoResponse)
}

func TestService_PriorityOrder(t *testing.T) {
	// 複数有効でも最初の provider (hcaptcha) が使われる。
	secret := "s"
	svc := captcha.NewService(&model.Meta{
		EnableHcaptcha:    true,
		HcaptchaSecretKey: &secret,
		EnableTestcaptcha: true,
	})
	// testcaptcha token は送るが hcaptcha が優先されるため token 空でエラー。
	err := svc.Verify(context.Background(), captcha.CaptchaTokens{Testcaptcha: "testcaptcha-passed"})
	assert.ErrorIs(t, err, captcha.ErrNoResponse)
}

func TestService_RecaptchaEnabled(t *testing.T) {
	secret := "recap-sec"
	svc := captcha.NewService(&model.Meta{
		EnableRecaptcha:    true,
		RecaptchaSecretKey: &secret,
	})
	err := svc.Verify(context.Background(), captcha.CaptchaTokens{})
	assert.ErrorIs(t, err, captcha.ErrNoResponse)
}

func TestService_TurnstileEnabled(t *testing.T) {
	secret := "ts-sec"
	svc := captcha.NewService(&model.Meta{
		EnableTurnstile:    true,
		TurnstileSecretKey: &secret,
	})
	err := svc.Verify(context.Background(), captcha.CaptchaTokens{})
	assert.ErrorIs(t, err, captcha.ErrNoResponse)
}

func TestService_McaptchaEnabled(t *testing.T) {
	secret := "mc-sec"
	inst := "http://mcaptcha.example"
	siteKey := "site"
	svc := captcha.NewService(&model.Meta{
		EnableMcaptcha:      true,
		McaptchaSecretKey:   &secret,
		McaptchaInstanceURL: &inst,
		McaptchaSiteKey:     &siteKey,
	})
	err := svc.Verify(context.Background(), captcha.CaptchaTokens{})
	assert.ErrorIs(t, err, captcha.ErrNoResponse)
}

func TestRecaptcha_Constructor(t *testing.T) {
	v := captcha.NewRecaptcha("sec")
	assert.NotNil(t, v)
	vw := captcha.NewRecaptchaWithClient("sec", http.DefaultClient)
	assert.NotNil(t, vw)
}

func TestTurnstile_Constructor(t *testing.T) {
	v := captcha.NewTurnstile("sec")
	assert.NotNil(t, v)
	vw := captcha.NewTurnstileWithClient("sec", http.DefaultClient)
	assert.NotNil(t, vw)
}

func TestSiteVerify_ServerDown(t *testing.T) {
	// 到達不能サーバー
	v := captcha.NewTurnstileWithURL("sec", "http://127.0.0.1:1", http.DefaultClient)
	err := v.Verify(context.Background(), "token")
	assert.ErrorIs(t, err, captcha.ErrRequestFailed)
}

func TestSiteVerify_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	v := captcha.NewTurnstileWithURL("sec", srv.URL, srv.Client())
	err := v.Verify(context.Background(), "token")
	assert.ErrorIs(t, err, captcha.ErrRequestFailed)
}

func TestSiteVerify_NoErrorCodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false})
	}))
	defer srv.Close()

	v := captcha.NewTurnstileWithURL("sec", srv.URL, srv.Client())
	err := v.Verify(context.Background(), "token")
	assert.ErrorIs(t, err, captcha.ErrVerificationFail)
	assert.Contains(t, err.Error(), "unknown")
}

func TestMcaptcha_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("broken"))
	}))
	defer srv.Close()

	v := captcha.NewMcaptchaWithClient(srv.URL, "site", "sec", srv.Client())
	err := v.Verify(context.Background(), "token")
	assert.ErrorIs(t, err, captcha.ErrRequestFailed)
}

// #340: captcha fetcher が safehttp.ReadAllLimit (1 MiB cap) で過大 response
// を弾くこと。1 MiB 超を返すサーバをsimulate して ErrRequestFailed が返る
// ことを検証する。
func TestMcaptcha_ResponseTooLarge(t *testing.T) {
	oversized := make([]byte, 2<<20) // 2 MiB > 1 MiB cap
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(oversized)
	}))
	defer srv.Close()

	v := captcha.NewMcaptchaWithClient(srv.URL, "site", "sec", srv.Client())
	err := v.Verify(context.Background(), "token")
	assert.ErrorIs(t, err, captcha.ErrRequestFailed)
}

func TestTurnstile_ResponseTooLarge(t *testing.T) {
	oversized := make([]byte, 2<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(oversized)
	}))
	defer srv.Close()

	v := captcha.NewTurnstileWithURL("sec", srv.URL, srv.Client())
	err := v.Verify(context.Background(), "token")
	// Mcaptcha と同じく ErrRequestFailed が返ることを明示 (TestMcaptcha_ResponseTooLarge
	// と揃える、Devin #404 指摘)。
	assert.ErrorIs(t, err, captcha.ErrRequestFailed)
}

// testcaptcha は実 provider として数えない (#2806)。数えると「マジック文字列
// 一致だけが効いていて、フォームトークンは要求されない」という最悪の組み合わせに
// なる。
func TestService_HasRealProvider(t *testing.T) {
	secret := "s"
	url := "https://mcaptcha.example"
	empty := ""
	tests := []struct {
		name string
		meta *model.Meta
		want bool
	}{
		{name: "none", meta: &model.Meta{}, want: false},
		{name: "testcaptcha only", meta: &model.Meta{EnableTestcaptcha: true}, want: false},
		{name: "hcaptcha", meta: &model.Meta{EnableHcaptcha: true, HcaptchaSecretKey: &secret}, want: true},
		{name: "recaptcha", meta: &model.Meta{EnableRecaptcha: true, RecaptchaSecretKey: &secret}, want: true},
		{name: "turnstile", meta: &model.Meta{EnableTurnstile: true, TurnstileSecretKey: &secret}, want: true},
		{name: "mcaptcha", meta: &model.Meta{EnableMcaptcha: true, McaptchaSecretKey: &secret, McaptchaSiteKey: &secret, McaptchaInstanceURL: &url}, want: true},
		{
			// **upstream `SignupApiService.ts:86` は sitekey も要求する
			// (#3037 レビュー 2 周目)。** 見ないと、sitekey が NULL の meta 行で
			// トークンを要求するのにウィジェットが描画されず、signup /
			// signin / 申請が全滅する。
			name: "mcaptcha without sitekey",
			meta: &model.Meta{EnableMcaptcha: true, McaptchaSecretKey: &secret, McaptchaInstanceURL: &url},
			want: false,
		},
		{
			// **空文字は upstream では falsy。** `nil` だけを見ていると、
			// `admin/update-meta` で空文字を書くだけで provider を有効にでき、
			// 検証が必ず失敗する = 登録とサインインを止められる。
			name: "hcaptcha with an empty secret",
			meta: &model.Meta{EnableHcaptcha: true, HcaptchaSecretKey: &empty},
			want: false,
		},
		{
			name: "hcaptcha + testcaptcha",
			meta: &model.Meta{EnableHcaptcha: true, HcaptchaSecretKey: &secret, EnableTestcaptcha: true},
			want: true,
		},
		{
			// 有効フラグだけで secret が無いものは provider が組み立たない。
			name: "hcaptcha without secret",
			meta: &model.Meta{EnableHcaptcha: true},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, captcha.NewService(tt.meta).HasRealProvider())
		})
	}
	var nilSvc *captcha.Service
	assert.False(t, nilSvc.HasRealProvider())
}

// Reload が provider 集合を差し替えること。
//
// **起動時のスナップショットのままだと、運営者が管理画面で captcha を有効に
// しても再起動まで一切検証されない。** `/api/meta` は DB を読むのでフロントは
// captcha を描画し、**運営者からは ON に見える**。provider が 1 つも無いときの
// `Verify` は成功を返すので、有効化したつもりのまま signup / signin が素通りする。
func TestService_Reload(t *testing.T) {
	ctx := context.Background()

	t.Run("無効から有効へ", func(t *testing.T) {
		s := captcha.NewService(&model.Meta{})
		require.NoError(t, s.Verify(ctx, captcha.CaptchaTokens{}), "provider が無ければ成功")
		assert.False(t, s.IsEnabled())

		s.Reload(&model.Meta{EnableTestcaptcha: true})
		assert.True(t, s.IsEnabled())
		assert.Error(t, s.Verify(ctx, captcha.CaptchaTokens{}), "有効化後はトークン無しを拒否")
		assert.NoError(t, s.Verify(ctx, captcha.CaptchaTokens{Testcaptcha: "testcaptcha-passed"}))
	})

	t.Run("有効から無効へ", func(t *testing.T) {
		s := captcha.NewService(&model.Meta{EnableTestcaptcha: true})
		assert.Error(t, s.Verify(ctx, captcha.CaptchaTokens{}))

		s.Reload(&model.Meta{})
		assert.False(t, s.IsEnabled())
		assert.NoError(t, s.Verify(ctx, captcha.CaptchaTokens{}))
	})

	// **nil は据え置く。** 読めなかったことを理由に運営者の設定を消さない。
	t.Run("nil meta は無視", func(t *testing.T) {
		s := captcha.NewService(&model.Meta{EnableTestcaptcha: true})
		s.Reload(nil)
		assert.True(t, s.IsEnabled(), "設定が消えている")
	})

	t.Run("nil レシーバでも落ちない", func(t *testing.T) {
		var s *captcha.Service
		assert.NotPanics(t, func() { s.Reload(&model.Meta{EnableTestcaptcha: true}) })
	})
}

// **有効な provider は全部検証する (#3037)。**
//
// 以前は最初の 1 つで return していたので、運営者が 2 つ有効にすると
// **後ろの 1 つは素通り**だった。設定画面は両方を「有効」と表示し、フォームも
// 両方のウィジェットを出すので、効いていない側があることに気付けない。
// upstream (`SignupApiService.ts:80-108`) は `if` を 5 つ並べて全部検証する。
//
// mcaptcha と testcaptcha を使うのは、**外部 URL を差し替えられる provider が
// この 2 つだけ**のため (他の 3 つは verify URL が定数)。判定の形は共通なので、
// この組で足りる。
func TestVerify_ChecksEveryEnabledProvider(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"valid":true}`))
	}))
	defer srv.Close()

	svc := captcha.NewServiceWithClient(mcaptchaAndTestcaptchaMeta(srv.URL), srv.Client())

	// 両方正しいときは通る。
	require.NoError(t, svc.Verify(context.Background(), captcha.CaptchaTokens{
		Mcaptcha: "m", Testcaptcha: "testcaptcha-passed",
	}))
	assert.Equal(t, 1, hits, "mcaptcha が検証されていない")

	// **testcaptcha だけ間違っている。** mcaptcha (先に評価される) が成功する
	// ので、最初の 1 つで return する実装ではここが通ってしまう。
	err := svc.Verify(context.Background(), captcha.CaptchaTokens{
		Mcaptcha: "m", Testcaptcha: "wrong",
	})
	assert.Error(t, err, "2 つ目の provider が素通りしている")
}

// **手前の provider が失敗したら、その時点で落とす。** 後ろが成功しても
// 結果は失敗。
func TestVerify_FailsOnTheFirstRejectingProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"valid":false}`))
	}))
	defer srv.Close()

	svc := captcha.NewServiceWithClient(mcaptchaAndTestcaptchaMeta(srv.URL), srv.Client())

	err := svc.Verify(context.Background(), captcha.CaptchaTokens{
		Mcaptcha: "m", Testcaptcha: "testcaptcha-passed",
	})
	assert.Error(t, err)
}

func mcaptchaAndTestcaptchaMeta(instanceURL string) *model.Meta {
	secret := "s"
	site := "sk"
	return &model.Meta{
		EnableMcaptcha:      true,
		McaptchaSecretKey:   &secret,
		McaptchaSiteKey:     &site,
		McaptchaInstanceURL: &instanceURL,
		EnableTestcaptcha:   true,
	}
}
