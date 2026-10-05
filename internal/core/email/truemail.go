package email

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/elythia-network/elythia/internal/safehttp"
)

type truemailClient struct {
	instanceURL string
	authKey     string
	client      *http.Client
}

func newTruemailClient(instanceURL, authKey string, client *http.Client) *truemailClient {
	if client == nil {
		client = http.DefaultClient
	}
	return &truemailClient{
		instanceURL: strings.TrimRight(instanceURL, "/"),
		authKey:     authKey,
		client:      client,
	}
}

type truemailResponse struct {
	Email   string          `json:"email"`
	Success bool            `json:"success"`
	Errors  *truemailErrors `json:"errors"`
}

type truemailErrors struct {
	ListMatch *string `json:"list_match"`
	Regex     *string `json:"regex"`
	MX        *string `json:"mx"`
	SMTP      *string `json:"smtp"`
}

func (c *truemailClient) verify(ctx context.Context, emailAddr string) error {
	u := c.instanceURL + "?email=" + url.QueryEscape(emailAddr)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNetwork, err)
	}
	req.Header.Set("Authorization", c.authKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNetwork, err)
	}
	defer resp.Body.Close()

	data, err := safehttp.ReadAllLimit(resp.Body, safehttp.DefaultThirdPartyAPILimit)
	if err != nil {
		return fmt.Errorf("%w: read body: %v", ErrNetwork, err)
	}

	var r truemailResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("%w: parse: %v", ErrNetwork, err)
	}

	if r.Email == "" || (r.Errors != nil && r.Errors.Regex != nil) {
		return ErrFormat
	}
	if r.Errors != nil && r.Errors.SMTP != nil {
		return ErrSMTP
	}
	if r.Errors != nil && r.Errors.MX != nil {
		return ErrMX
	}
	if !r.Success {
		if r.Errors != nil && r.Errors.ListMatch != nil {
			return ErrBlacklist
		}
		return ErrBlacklist
	}
	return nil
}
