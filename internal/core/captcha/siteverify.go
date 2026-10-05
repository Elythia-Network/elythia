package captcha

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/elythia-network/elythia/internal/safehttp"
)

// siteVerifyResponse is the common JSON shape returned by hCaptcha,
// reCAPTCHA, and Turnstile siteverify endpoints.
type siteVerifyResponse struct {
	Success    bool     `json:"success"`
	ErrorCodes []string `json:"error-codes"`
}

// siteVerifier performs the standard form-urlencoded siteverify POST used
// by hCaptcha, reCAPTCHA, and Turnstile. Each differs only in URL and
// secret; the wire format is identical.
type siteVerifier struct {
	verifyURL string
	secret    string
	client    *http.Client
}

func (v *siteVerifier) Verify(ctx context.Context, token string) error {
	if token == "" {
		return ErrNoResponse
	}

	body := url.Values{
		"secret":   {v.secret},
		"response": {token},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.verifyURL,
		strings.NewReader(body.Encode()))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRequestFailed, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	cl := v.client
	if cl == nil {
		cl = http.DefaultClient
	}
	resp, err := cl.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRequestFailed, err)
	}
	defer resp.Body.Close()

	data, err := safehttp.ReadAllLimit(resp.Body, safehttp.DefaultThirdPartyAPILimit)
	if err != nil {
		return fmt.Errorf("%w: read body: %v", ErrRequestFailed, err)
	}

	var result siteVerifyResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("%w: parse response: %v", ErrRequestFailed, err)
	}
	if !result.Success {
		codes := strings.Join(result.ErrorCodes, ", ")
		if codes == "" {
			codes = "unknown"
		}
		return fmt.Errorf("%w: %s", ErrVerificationFail, codes)
	}
	return nil
}
