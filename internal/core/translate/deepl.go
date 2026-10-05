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
