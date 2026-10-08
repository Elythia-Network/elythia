package plugintest_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/plugin"
	"github.com/elythia-network/elythia/plugin/plugintest"
)

// secretPlugin reads its API key on every request and stores a token it
// receives at runtime.
var secretPlugin = plugin.Definition{
	Name:       "secretive",
	APIVersion: plugin.APIVersion,
	Secrets:    []plugin.SecretSpec{{Name: "apiKey"}},
	Routes: func(ctx plugin.Context, r plugin.Router) error {
		r.POST("/ready", func(req plugin.Request) (any, error) {
			key, err := ctx.Secrets().Get(req.Context(), "apiKey")
			switch {
			case errors.Is(err, plugin.ErrSecretsUnavailable):
				return map[string]any{"state": "unavailable"}, nil
			case errors.Is(err, plugin.ErrSecretNotSet):
				return map[string]any{"state": "notSet"}, nil
			case err != nil:
				return nil, err
			}
			return map[string]any{"state": "ready", "length": len(key)}, nil
		})
		r.POST("/store", func(req plugin.Request) (any, error) {
			var body struct{ Token string }
			if err := req.Bind(&body); err != nil {
				return nil, err
			}
			return nil, ctx.Secrets().Set(req.Context(), "oauthToken", body.Token)
		})
		return nil
	},
}

func TestHarness_WithSecrets(t *testing.T) {
	h := plugintest.New(t).WithName("secretive").WithSecrets(map[string]string{"apiKey": "sk-123"})
	routes := h.Routes(secretPlugin)

	res, err := routes.Call(t, "POST /ready", plugintest.Request{})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"state": "ready", "length": 6}, res)

	_, err = routes.Call(t, "POST /store", plugintest.Request{Body: `{"Token":"tok-1"}`})
	require.NoError(t, err)
	got, ok := h.Secret("oauthToken")
	require.True(t, ok)
	assert.Equal(t, "tok-1", got)
	assert.Equal(t, []string{"apiKey", "oauthToken"}, h.SecretNames())

	_, ok = h.Secret("missing")
	assert.False(t, ok)
}

func TestHarness_SecretsNotSetByDefault(t *testing.T) {
	h := plugintest.New(t).WithName("secretive")
	res, err := h.Routes(secretPlugin).Call(t, "POST /ready", plugintest.Request{})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"state": "notSet"}, res)
	assert.Empty(t, h.SecretNames())
}

// 鍵の無いインスタンスでも、プラグインが壊れずに動くかを試せる。
func TestHarness_WithoutSecretKey(t *testing.T) {
	h := plugintest.New(t).WithName("secretive").WithoutSecretKey()
	routes := h.Routes(secretPlugin)
	res, err := routes.Call(t, "POST /ready", plugintest.Request{})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"state": "unavailable"}, res)

	_, err = routes.Call(t, "POST /store", plugintest.Request{Body: `{"Token":"tok"}`})
	assert.ErrorIs(t, err, plugin.ErrSecretsUnavailable)
	_, ok := h.Secret("oauthToken")
	assert.False(t, ok)
	assert.ErrorIs(t, h.Context().Secrets().Delete(context.Background(), "oauthToken"), plugin.ErrSecretsUnavailable)
}

// **本番と同じ検査を通す。** 本番で弾かれる名前や値をテストが通さない。
func TestHarness_SecretsUseProductionValidation(t *testing.T) {
	h := plugintest.New(t).WithName("secretive")
	s := h.Context().Secrets()
	ctx := context.Background()
	assert.Error(t, s.Set(ctx, "bad name", "v"))
	assert.Error(t, s.Set(ctx, "apiKey", ""))
	assert.Error(t, s.Set(ctx, "apiKey", strings.Repeat("x", 8*1024+1)))
	require.NoError(t, s.Set(ctx, "apiKey", "v"))
	require.NoError(t, s.Delete(ctx, "apiKey"))
	_, err := s.Get(ctx, "apiKey")
	assert.ErrorIs(t, err, plugin.ErrSecretNotSet)
}

// プラグイン名が違えば値は見えない (本番と同じく名前で分かれる)。
func TestHarness_SecretsAreScopedToPluginName(t *testing.T) {
	h := plugintest.New(t).WithName("first").WithSecrets(map[string]string{"apiKey": "first-key"})
	h.WithName("second")
	_, err := h.Context().Secrets().Get(context.Background(), "apiKey")
	assert.ErrorIs(t, err, plugin.ErrSecretNotSet)
	_, ok := h.Secret("apiKey")
	assert.False(t, ok)
	h.WithName("first")
	v, ok := h.Secret("apiKey")
	require.True(t, ok)
	assert.Equal(t, "first-key", v)
}
