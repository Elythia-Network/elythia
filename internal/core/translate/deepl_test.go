package translate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeepL_Translate_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "DeepL-Auth-Key testkey", r.Header.Get("Authorization"))
		assert.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))
		_ = r.ParseForm()
		assert.Equal(t, "EN", r.FormValue("target_lang"))

		_ = json.NewEncoder(w).Encode(deeplResponse{
			Translations: []struct {
				DetectedSourceLanguage string `json:"detected_source_language"`
				Text                   string `json:"text"`
			}{
				{DetectedSourceLanguage: "JA", Text: "Hello"},
			},
		})
	}))
	defer srv.Close()

	c := NewDeepLWithClient("testkey", srv.URL, srv.Client())
	result, err := c.Translate(context.Background(), "こんにちは", "en")
	require.NoError(t, err)
	assert.Equal(t, "ja", result.SourceLang)
	assert.Equal(t, "Hello", result.Text)
}

// The region of targetLang is dropped before it is sent, as upstream's
// `targetLang.split('-')[0]` does (#3369).
func TestDeepL_Translate_DropsRegionOfTargetLang(t *testing.T) {
	for in, want := range map[string]string{"ja-JP": "JA", "en-US": "EN", "zh-Hans-CN": "ZH", "ja": "JA", "-x": ""} {
		var got string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			got = r.FormValue("target_lang")
			_, _ = w.Write([]byte(`{"translations":[{"detected_source_language":"JA","text":"x"}]}`))
		}))
		c := NewDeepLWithClient("testkey", srv.URL, srv.Client())
		_, err := c.Translate(context.Background(), "text", in)
		srv.Close()
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
}

func TestDeepL_Translate_NoResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(deeplResponse{Translations: nil})
	}))
	defer srv.Close()

	c := NewDeepLWithClient("key", srv.URL, srv.Client())
	_, err := c.Translate(context.Background(), "text", "en")
	assert.ErrorIs(t, err, ErrNoResult)
}

func TestDeepL_Translate_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("Forbidden"))
	}))
	defer srv.Close()

	c := NewDeepLWithClient("badkey", srv.URL, srv.Client())
	_, err := c.Translate(context.Background(), "text", "en")
	assert.ErrorIs(t, err, ErrRequestFailed)
}

func TestDeepL_Translate_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := NewDeepLWithClient("key", srv.URL, srv.Client())
	_, err := c.Translate(context.Background(), "text", "en")
	assert.ErrorIs(t, err, ErrRequestFailed)
}

func TestDeepL_Translate_NetworkError(t *testing.T) {
	c := NewDeepLWithClient("key", "http://127.0.0.1:1", http.DefaultClient)
	_, err := c.Translate(context.Background(), "text", "en")
	assert.ErrorIs(t, err, ErrRequestFailed)
}

func TestNewDeepL_ProEndpoint(t *testing.T) {
	c := NewDeepL("key", true, nil)
	assert.Equal(t, deeplProURL, c.apiURL)
}

func TestNewDeepL_FreeEndpoint(t *testing.T) {
	c := NewDeepL("key", false, nil)
	assert.Equal(t, deeplFreeURL, c.apiURL)
}

// #340: DeepL fetcher が過大 response を safehttp.ReadAllLimit で弾くこと。
// 1 MiB cap を超える body を返す stub server に対して ErrRequestFailed を返す。
func TestDeepL_Translate_ResponseTooLarge(t *testing.T) {
	oversized := make([]byte, 2<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(oversized)
	}))
	defer srv.Close()

	c := NewDeepLWithClient("key", srv.URL, srv.Client())
	_, err := c.Translate(context.Background(), "text", "en")
	assert.ErrorIs(t, err, ErrRequestFailed)
}

// #404 Devin指摘: non-200 で error body が 8 KiB 超の場合も snippet が
// error message に含まれること (io.LimitReader で cap せず先頭を取り込む)。
func TestDeepL_Translate_NonOKLargeBody_IncludesSnippet(t *testing.T) {
	// 10 KiB の repeating marker — 先頭 8 KiB が snippet に入る想定。
	body := strings.Repeat("x", 10*1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := NewDeepLWithClient("key", srv.URL, srv.Client())
	_, err := c.Translate(context.Background(), "text", "en")
	require.ErrorIs(t, err, ErrRequestFailed)
	// "x" がerror messageに多数含まれているはず (snippetとして)
	assert.Contains(t, err.Error(), "status 400")
	assert.Contains(t, err.Error(), strings.Repeat("x", 100))
}

// fakeDeepLServers starts a Free and a Pro endpoint that record the auth
// header they received.
func fakeDeepLServers(t *testing.T) (free, pro *httptest.Server, got *[]string) {
	t.Helper()
	var calls []string
	handler := func(plan string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, plan+" "+r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(`{"translations":[{"detected_source_language":"JA","text":"Hello"}]}`))
		}
	}
	free = httptest.NewServer(handler("free"))
	pro = httptest.NewServer(handler("pro"))
	t.Cleanup(free.Close)
	t.Cleanup(pro.Close)
	return free, pro, &calls
}

func TestMetaDeepL_ReadsSettingsOnEveryCall(t *testing.T) {
	free, pro, calls := fakeDeepLServers(t)
	key, isPro := "", false
	m := NewMetaDeepL(func() (string, bool, error) { return key, isPro, nil }, free.Client())
	m.freeURL, m.proURL = free.URL, pro.URL

	_, err := m.Translate(context.Background(), "text", "en")
	require.ErrorIs(t, err, ErrNotConfigured)
	assert.Empty(t, *calls)

	key = "free-key"
	res, err := m.Translate(context.Background(), "text", "en")
	require.NoError(t, err)
	assert.Equal(t, "Hello", res.Text)

	key, isPro = "pro-key", true
	_, err = m.Translate(context.Background(), "text", "en")
	require.NoError(t, err)

	key = ""
	_, err = m.Translate(context.Background(), "text", "en")
	require.ErrorIs(t, err, ErrNotConfigured)

	assert.Equal(t, []string{"free DeepL-Auth-Key free-key", "pro DeepL-Auth-Key pro-key"}, *calls)
}

func TestMetaDeepL_SettingsError(t *testing.T) {
	m := NewMetaDeepL(func() (string, bool, error) { return "", false, errors.New("db down") }, nil)
	_, err := m.Translate(context.Background(), "text", "en")
	require.ErrorIs(t, err, ErrSettingsUnavailable)
	assert.Contains(t, err.Error(), "db down")
	assert.NotErrorIs(t, err, ErrNotConfigured)
	assert.NotErrorIs(t, err, ErrRequestFailed)
}

func TestNewMetaDeepL_DefaultEndpoints(t *testing.T) {
	m := NewMetaDeepL(func() (string, bool, error) { return "", false, nil }, nil)
	assert.Equal(t, deeplFreeURL, m.freeURL)
	assert.Equal(t, deeplProURL, m.proURL)
	assert.Same(t, http.DefaultClient, m.client)
}

// Configured reads the current settings: true only when a key is set, and
// ErrSettingsUnavailable when the settings cannot be read (#3847).
func TestMetaDeepL_Configured(t *testing.T) {
	key, settingsErr := "", error(nil)
	m := NewMetaDeepL(func() (string, bool, error) { return key, false, settingsErr }, nil)

	ok, err := m.Configured()
	require.NoError(t, err)
	assert.False(t, ok)

	key = "free-key"
	ok, err = m.Configured()
	require.NoError(t, err)
	assert.True(t, ok)

	settingsErr = errors.New("db down")
	ok, err = m.Configured()
	require.ErrorIs(t, err, ErrSettingsUnavailable)
	assert.Contains(t, err.Error(), "db down")
	assert.False(t, ok)
}
