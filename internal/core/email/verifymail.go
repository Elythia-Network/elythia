package email

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/elythia-network/elythia/internal/safehttp"
)

type verifymailClient struct {
	authKey string
	client  *http.Client
}

func newVerifymailClient(authKey string, client *http.Client) *verifymailClient {
	if client == nil {
		client = http.DefaultClient
	}
	return &verifymailClient{authKey: authKey, client: client}
}

type verifymailResponse struct {
	Message          *string `json:"message"`
	EmailAddress     *string `json:"email_address"`
	DeliverableEmail *bool   `json:"deliverable_email"`
	Disposable       *bool   `json:"disposable"`
	MX               *bool   `json:"mx"`
}

func (c *verifymailClient) verify(ctx context.Context, email string) error {
	u := "https://verifymail.io/api/" + url.PathEscape(email) + "?key=" + url.QueryEscape(c.authKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNetwork, err)
	}
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

	var r verifymailResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("%w: parse: %v", ErrNetwork, err)
	}

	// API エラー (message のみ返る場合)
	if r.EmailAddress == nil {
		return ErrFormat
	}
	if r.DeliverableEmail != nil && !*r.DeliverableEmail {
		return ErrSMTP
	}
	if r.Disposable != nil && *r.Disposable {
		return ErrDisposable
	}
	if r.MX != nil && !*r.MX {
		return ErrMX
	}
	return nil
}
