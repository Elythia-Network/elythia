package email

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Format ---

func TestValidateFormat(t *testing.T) {
	assert.True(t, ValidateFormat("user@example.com"))
	assert.True(t, ValidateFormat("a+b@sub.domain.org"))
	assert.False(t, ValidateFormat(""))
	assert.False(t, ValidateFormat("noatsign"))
	assert.False(t, ValidateFormat("@missing-local.com"))
}

// --- MX ---

func TestCheckMX_ValidDomain(t *testing.T) {
	old := lookupMX
	defer func() { lookupMX = old }()
	lookupMX = func(_ string) ([]*net.MX, error) {
		return []*net.MX{{Host: "mx.example.com", Pref: 10}}, nil
	}
	assert.NoError(t, CheckMX("example.com"))
}

func TestCheckMX_NoRecords(t *testing.T) {
	old := lookupMX
	defer func() { lookupMX = old }()
	lookupMX = func(_ string) ([]*net.MX, error) {
		return nil, nil
	}
	assert.ErrorIs(t, CheckMX("no-mx.example"), ErrMX)
}

func TestCheckMX_DNSError(t *testing.T) {
	old := lookupMX
	defer func() { lookupMX = old }()
	lookupMX = func(_ string) ([]*net.MX, error) {
		return nil, &net.DNSError{Err: "timeout"}
	}
	assert.NoError(t, CheckMX("timeout.example"))
}

// --- Banned domains ---

func TestIsBannedDomain(t *testing.T) {
	banned := []string{"example.com", "spam.org"}
	assert.True(t, IsBannedDomain("example.com", banned))
	assert.True(t, IsBannedDomain("mail.example.com", banned))
	assert.True(t, IsBannedDomain("spam.org", banned))
	assert.False(t, IsBannedDomain("notbanned.com", banned))
	assert.False(t, IsBannedDomain("", banned))
	assert.False(t, IsBannedDomain("example.com", nil))
}

func TestIsBannedDomain_EmptyEntries(t *testing.T) {
	assert.False(t, IsBannedDomain("anything.com", []string{"", "  "}))
}

// --- Verifymail ---

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func verifymailWithStub(t *testing.T, handler http.HandlerFunc) *verifymailClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &verifymailClient{authKey: "key", client: &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			req.URL.Scheme = "http"
			req.URL.Host = srv.Listener.Addr().String()
			return http.DefaultTransport.RoundTrip(req)
		}),
	}}
}

func TestVerifymail_Success(t *testing.T) {
	c := verifymailWithStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"email_address": "user@ok.com", "deliverable_email": true, "disposable": false, "mx": true,
		})
	})
	require.NoError(t, c.verify(context.Background(), "user@ok.com"))
}

func TestVerifymail_Disposable(t *testing.T) {
	c := verifymailWithStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"email_address": "user@tmp.com", "deliverable_email": true, "disposable": true, "mx": true,
		})
	})
	assert.ErrorIs(t, c.verify(context.Background(), "user@tmp.com"), ErrDisposable)
}

func TestVerifymail_Undeliverable(t *testing.T) {
	c := verifymailWithStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"email_address": "user@fail.com", "deliverable_email": false, "disposable": false, "mx": true,
		})
	})
	assert.ErrorIs(t, c.verify(context.Background(), "user@fail.com"), ErrSMTP)
}

func TestVerifymail_NoMX(t *testing.T) {
	c := verifymailWithStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"email_address": "user@nomx.com", "deliverable_email": true, "disposable": false, "mx": false,
		})
	})
	assert.ErrorIs(t, c.verify(context.Background(), "user@nomx.com"), ErrMX)
}

func TestVerifymail_APIError(t *testing.T) {
	c := verifymailWithStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "rate limit"})
	})
	assert.ErrorIs(t, c.verify(context.Background(), "user@x.com"), ErrFormat)
}

func TestVerifymail_InvalidJSON(t *testing.T) {
	c := verifymailWithStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	})
	assert.ErrorIs(t, c.verify(context.Background(), "user@x.com"), ErrNetwork)
}

func TestVerifymail_NetworkError(t *testing.T) {
	c := &verifymailClient{authKey: "key", client: &http.Client{
		Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			return nil, assert.AnError
		}),
	}}
	assert.ErrorIs(t, c.verify(context.Background(), "user@x.com"), ErrNetwork)
}

// --- Truemail ---

func TestTruemail_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "mykey", r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(truemailResponse{Email: "user@ok.com", Success: true})
	}))
	defer srv.Close()
	c := &truemailClient{instanceURL: srv.URL, authKey: "mykey", client: srv.Client()}
	require.NoError(t, c.verify(context.Background(), "user@ok.com"))
}

func TestTruemail_FormatError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		regex := "invalid"
		_ = json.NewEncoder(w).Encode(truemailResponse{Email: "", Success: false, Errors: &truemailErrors{Regex: &regex}})
	}))
	defer srv.Close()
	c := &truemailClient{instanceURL: srv.URL, authKey: "k", client: srv.Client()}
	assert.ErrorIs(t, c.verify(context.Background(), "bad"), ErrFormat)
}

func TestTruemail_MXError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mx := "no mx"
		_ = json.NewEncoder(w).Encode(truemailResponse{Email: "u@x.com", Success: false, Errors: &truemailErrors{MX: &mx}})
	}))
	defer srv.Close()
	c := &truemailClient{instanceURL: srv.URL, authKey: "k", client: srv.Client()}
	assert.ErrorIs(t, c.verify(context.Background(), "u@x.com"), ErrMX)
}

func TestTruemail_SMTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s := "fail"
		_ = json.NewEncoder(w).Encode(truemailResponse{Email: "u@x.com", Success: false, Errors: &truemailErrors{SMTP: &s}})
	}))
	defer srv.Close()
	c := &truemailClient{instanceURL: srv.URL, authKey: "k", client: srv.Client()}
	assert.ErrorIs(t, c.verify(context.Background(), "u@x.com"), ErrSMTP)
}

func TestTruemail_BlacklistError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		lm := "listed"
		_ = json.NewEncoder(w).Encode(truemailResponse{Email: "u@x.com", Success: false, Errors: &truemailErrors{ListMatch: &lm}})
	}))
	defer srv.Close()
	c := &truemailClient{instanceURL: srv.URL, authKey: "k", client: srv.Client()}
	assert.ErrorIs(t, c.verify(context.Background(), "u@x.com"), ErrBlacklist)
}

func TestTruemail_SuccessFalseNoErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(truemailResponse{Email: "u@x.com", Success: false})
	}))
	defer srv.Close()
	c := &truemailClient{instanceURL: srv.URL, authKey: "k", client: srv.Client()}
	assert.ErrorIs(t, c.verify(context.Background(), "u@x.com"), ErrBlacklist)
}

func TestTruemail_NetworkError(t *testing.T) {
	c := &truemailClient{instanceURL: "http://127.0.0.1:1", authKey: "k", client: http.DefaultClient}
	assert.ErrorIs(t, c.verify(context.Background(), "u@x.com"), ErrNetwork)
}

func TestTruemail_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()
	c := &truemailClient{instanceURL: srv.URL, authKey: "k", client: srv.Client()}
	assert.ErrorIs(t, c.verify(context.Background(), "u@x.com"), ErrNetwork)
}

// --- Service ---

func TestService_NoChecksEnabled(t *testing.T) {
	svc := NewService(&model.Meta{})
	require.NoError(t, svc.Validate(context.Background(), "user@example.com"))
}

func TestService_FormatFails(t *testing.T) {
	svc := NewService(&model.Meta{})
	assert.ErrorIs(t, svc.Validate(context.Background(), "not-email"), ErrFormat)
}

func TestService_BannedDomain(t *testing.T) {
	svc := NewService(&model.Meta{BannedEmailDomains: []string{"banned.org"}})
	assert.ErrorIs(t, svc.Validate(context.Background(), "user@banned.org"), ErrBanned)
	assert.ErrorIs(t, svc.Validate(context.Background(), "user@sub.banned.org"), ErrBanned)
	require.NoError(t, svc.Validate(context.Background(), "user@ok.com"))
}

func TestService_ActiveMXCheck(t *testing.T) {
	old := lookupMX
	defer func() { lookupMX = old }()
	lookupMX = func(_ string) ([]*net.MX, error) { return nil, nil }
	svc := NewService(&model.Meta{EnableActiveEmailValidation: true})
	assert.ErrorIs(t, svc.Validate(context.Background(), "user@no-mx.example"), ErrMX)
}

func TestService_VerifymailEnabled(t *testing.T) {
	key := "vkey"
	svc := NewService(&model.Meta{EnableVerifymailAPI: true, VerifymailAuthKey: &key})
	assert.NotNil(t, svc.verifymail)
}

func TestService_TruemailEnabled(t *testing.T) {
	inst := "http://truemail.example"
	key := "k"
	svc := NewService(&model.Meta{EnableTruemailAPI: true, TruemailInstance: &inst, TruemailAuthKey: &key})
	assert.NotNil(t, svc.truemail)
}

func TestService_VerifymailPriorityOverTruemail(t *testing.T) {
	vkey := "vkey"
	inst := "http://truemail.example"
	tkey := "tkey"
	svc := NewService(&model.Meta{
		EnableVerifymailAPI: true, VerifymailAuthKey: &vkey,
		EnableTruemailAPI: true, TruemailInstance: &inst, TruemailAuthKey: &tkey,
	})
	assert.NotNil(t, svc.verifymail)
	assert.Nil(t, svc.truemail)
}

func TestDomainOf(t *testing.T) {
	assert.Equal(t, "example.com", domainOf("user@example.com"))
	assert.Equal(t, "sub.domain.org", domainOf("a@sub.domain.org"))
	assert.Equal(t, "", domainOf("noatsign"))
}

// #340: verifymail / truemail fetcher が過大 response を safehttp.ReadAllLimit
// で弾くこと。1 MiB cap を超えるbodyを返す stub server に対して error が
// 伝播することを検証する。
func TestVerifymail_ResponseTooLarge(t *testing.T) {
	oversized := make([]byte, 2<<20)
	c := verifymailWithStub(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(oversized)
	})
	err := c.verify(context.Background(), "a@example.com")
	assert.Error(t, err)
}

func TestTruemail_ResponseTooLarge(t *testing.T) {
	oversized := make([]byte, 2<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(oversized)
	}))
	defer srv.Close()

	c := &truemailClient{instanceURL: srv.URL, authKey: "key", client: srv.Client()}
	err := c.verify(context.Background(), "a@example.com")
	assert.Error(t, err)
}
