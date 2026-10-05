package captcha

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/elythia-network/elythia/internal/safehttp"
)

type mcaptchaVerifier struct {
	instanceURL string
	siteKey     string
	secretKey   string
	client      *http.Client
}

type mcaptchaRequest struct {
	Key    string `json:"key"`
	Secret string `json:"secret"`
	Token  string `json:"token"`
}

type mcaptchaResponse struct {
	Valid bool `json:"valid"`
}

// NewMcaptcha returns a Verifier for mCaptcha.
func NewMcaptcha(instanceURL, siteKey, secretKey string) Verifier {
	return &mcaptchaVerifier{
		instanceURL: strings.TrimRight(instanceURL, "/"),
		siteKey:     siteKey,
		secretKey:   secretKey,
	}
}

// NewMcaptchaWithClient allows injecting an *http.Client for tests.
func NewMcaptchaWithClient(instanceURL, siteKey, secretKey string, client *http.Client) Verifier {
	return &mcaptchaVerifier{
		instanceURL: strings.TrimRight(instanceURL, "/"),
		siteKey:     siteKey,
		secretKey:   secretKey,
		client:      client,
	}
}

func (v *mcaptchaVerifier) Verify(ctx context.Context, token string) error {
	if token == "" {
		return ErrNoResponse
	}

	payload, _ := json.Marshal(mcaptchaRequest{
		Key:    v.siteKey,
		Secret: v.secretKey,
		Token:  token,
	})

	url := v.instanceURL + "/api/v1/pow/siteverify"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRequestFailed, err)
	}
	req.Header.Set("Content-Type", "application/json")

	cl := v.client
	if cl == nil {
		cl = http.DefaultClient
	}
	resp, err := cl.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRequestFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// upstream verifyMcaptcha は非200を requestFailed として扱う
		// (verification の不成立ではなく instance への到達失敗扱い)。
		return fmt.Errorf("%w: status %d", ErrRequestFailed, resp.StatusCode)
	}

	data, err := safehttp.ReadAllLimit(resp.Body, safehttp.DefaultThirdPartyAPILimit)
	if err != nil {
		return fmt.Errorf("%w: read body: %v", ErrRequestFailed, err)
	}

	var result mcaptchaResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("%w: parse response: %v", ErrRequestFailed, err)
	}
	if !result.Valid {
		return fmt.Errorf("%w: invalid token", ErrVerificationFail)
	}
	return nil
}
