// Package translate provides note translation via external APIs.
// Currently supports DeepL (Free and Pro).
package translate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/elythia-network/elythia/internal/safehttp"
)

var (
	ErrNotConfigured = errors.New("translate: translator not configured")
	ErrRequestFailed = errors.New("translate: request failed")
	ErrNoResult      = errors.New("translate: no translation result")
	// ErrSettingsUnavailable means the translator settings could not be read
	// (e.g. the meta row). DeepL への要求の失敗 (ErrRequestFailed) と分ける。
	ErrSettingsUnavailable = errors.New("translate: settings unavailable")
)

const (
	deeplFreeURL = "https://api-free.deepl.com/v2/translate"
	deeplProURL  = "https://api.deepl.com/v2/translate"
)

// DeepLClient translates text via the DeepL API.
type DeepLClient struct {
	authKey string
	apiURL  string
	client  *http.Client
}

// NewDeepL creates a DeepL translator. isPro selects the Pro endpoint.
//
// client は production では SSRF-safe transport + forward proxy 経由の
// outbound client を渡す (#638)。nil のときは http.DefaultClient に
// フォールバックするが、本番経路では使わないこと (origin IP が DeepL に
// 直接漏れる)。
func NewDeepL(authKey string, isPro bool, client *http.Client) *DeepLClient {
	apiURL := deeplFreeURL
	if isPro {
		apiURL = deeplProURL
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &DeepLClient{authKey: authKey, apiURL: apiURL, client: client}
}

// Translator translates note text. *DeepLClient and *MetaDeepL implement it.
type Translator interface {
	Translate(ctx context.Context, text, targetLang string) (*TranslateResult, error)
}

// DeepLSettings returns the DeepL auth key and whether it is a Pro key. An
// empty key means translation is not configured.
type DeepLSettings func() (authKey string, isPro bool, err error)

// MetaDeepL is a DeepL translator that reads its key and plan on every call,
// so changes in the admin settings apply without rebuilding the server (#3663).
//
// 起動時の値で DeepLClient を作ると、キーを入れても外しても Pro を切り替えても、
// プロセスか世代を作り直すまで変わらなかった。本家は `meta` を
// `metaUpdated` で差し替え、翻訳のたびにそこから読む。ここでは settings に
// meta のキャッシュ (metaUpdated で捨てられる) を読む関数を渡す。
type MetaDeepL struct {
	settings DeepLSettings
	client   *http.Client
	freeURL  string
	proURL   string
}

// NewMetaDeepL creates a translator that resolves the DeepL settings per call.
// client は NewDeepL と同じく、本番では SSRF-safe な outbound client を渡す。
func NewMetaDeepL(settings DeepLSettings, client *http.Client) *MetaDeepL {
	if client == nil {
		client = http.DefaultClient
	}
	return &MetaDeepL{settings: settings, client: client, freeURL: deeplFreeURL, proURL: deeplProURL}
}

// Configured reports whether an auth key is currently set. err is
// ErrSettingsUnavailable when the settings could not be read.
func (m *MetaDeepL) Configured() (bool, error) {
	key, _, err := m.settings()
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrSettingsUnavailable, err)
	}
	return key != "", nil
}

// Translate reads the current settings and translates text. It returns
// ErrNotConfigured when no auth key is set.
func (m *MetaDeepL) Translate(ctx context.Context, text, targetLang string) (*TranslateResult, error) {
	key, isPro, err := m.settings()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSettingsUnavailable, err)
	}
	if key == "" {
		return nil, ErrNotConfigured
	}
	apiURL := m.freeURL
	if isPro {
		apiURL = m.proURL
	}
	return NewDeepLWithClient(key, apiURL, m.client).Translate(ctx, text, targetLang)
}

// NewDeepLWithClient allows injecting a custom http.Client (for tests).
func NewDeepLWithClient(authKey, apiURL string, client *http.Client) *DeepLClient {
	return &DeepLClient{authKey: authKey, apiURL: apiURL, client: client}
}

type deeplResponse struct {
	Translations []struct {
		DetectedSourceLanguage string `json:"detected_source_language"`
		Text                   string `json:"text"`
	} `json:"translations"`
}

// TranslateResult holds a single translation result.
type TranslateResult struct {
	SourceLang string `json:"sourceLang"`
	Text       string `json:"text"`
}

// Translate sends text to DeepL and returns the translated text.
func (c *DeepLClient) Translate(ctx context.Context, text, targetLang string) (*TranslateResult, error) {
	body := url.Values{
		"text":        {text},
		"target_lang": {strings.ToUpper(targetLang)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL,
		strings.NewReader(body.Encode()))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRequestFailed, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "DeepL-Auth-Key "+c.authKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRequestFailed, err)
	}
	defer resp.Body.Close()

	// status check を body 読込より先に行う。non-200 で body が巨大だと
	// ReadAllLimit が ErrResponseTooLarge を返して status 情報が error
	// message に出ず debug が難しくなるため (#404 Devin 指摘)。
	if resp.StatusCode != http.StatusOK {
		// error body は full に読まず 8 KiB snippet だけ引いてメッセージに含める。
		// io.LimitReader は cap 超過時に error を返さずに切り詰めるので、
		// 8 KiB 超のbodyでも先頭snippetが確実に取れる (ReadAllLimitだと
		// 超過時に nil を返してしまう、Devin指摘)。
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		return nil, fmt.Errorf("%w: status %d: %s", ErrRequestFailed, resp.StatusCode, string(snippet))
	}

	data, err := safehttp.ReadAllLimit(resp.Body, safehttp.DefaultThirdPartyAPILimit)
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %v", ErrRequestFailed, err)
	}

	var result deeplResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("%w: parse: %v", ErrRequestFailed, err)
	}
	if len(result.Translations) == 0 {
		return nil, ErrNoResult
	}
	return &TranslateResult{
		SourceLang: strings.ToLower(result.Translations[0].DetectedSourceLanguage),
		Text:       result.Translations[0].Text,
	}, nil
}
