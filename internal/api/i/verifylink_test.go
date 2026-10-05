package i

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"gorm.io/datatypes"
)

const testMyLink = "https://example.com/@alice"

// serveHTML returns a test server responding with the given HTML body.
func serveHTML(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestVerifyRelMe_AnchorHref(t *testing.T) {
	// `<a href=myLink>` は rel 不問で backlink とみなす (upstream includesMyLink)。
	srv := serveHTML(t, `<html><body><a href="`+testMyLink+`">me</a></body></html>`)
	assert.True(t, verifyRelMe(http.DefaultClient, srv.URL, testMyLink))
}

func TestVerifyRelMe_LinkRelMe(t *testing.T) {
	srv := serveHTML(t, `<html><head><link rel="me" href="`+testMyLink+`"></head></html>`)
	assert.True(t, verifyRelMe(http.DefaultClient, srv.URL, testMyLink))
}

func TestVerifyRelMe_AnchorRelMeMultiToken(t *testing.T) {
	srv := serveHTML(t, `<html><body><a rel="noopener me" href="`+testMyLink+`">x</a></body></html>`)
	assert.True(t, verifyRelMe(http.DefaultClient, srv.URL, testMyLink))
}

func TestVerifyRelMe_NoBacklink(t *testing.T) {
	srv := serveHTML(t, `<html><body><a href="https://example.com/@bob">other</a></body></html>`)
	assert.False(t, verifyRelMe(http.DefaultClient, srv.URL, testMyLink))
}

func TestVerifyRelMe_LinkWithoutRelMe(t *testing.T) {
	// <link> は rel=me が無ければ href 一致でも backlink とみなさない。
	srv := serveHTML(t, `<html><head><link rel="stylesheet" href="`+testMyLink+`"></head></html>`)
	assert.False(t, verifyRelMe(http.DefaultClient, srv.URL, testMyLink))
}

func TestVerifyRelMe_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	assert.False(t, verifyRelMe(http.DefaultClient, srv.URL, testMyLink))
}

func TestVerifyRelMe_Guards(t *testing.T) {
	assert.False(t, verifyRelMe(nil, "https://x", testMyLink))
	assert.False(t, verifyRelMe(http.DefaultClient, "", testMyLink))
	assert.False(t, verifyRelMe(http.DefaultClient, "https://x", ""))
}

func TestVerifyLinks_FiltersVerified(t *testing.T) {
	good := serveHTML(t, `<a rel="me" href="`+testMyLink+`">me</a>`)
	bad := serveHTML(t, `<a href="https://example.com/@bob">x</a>`)
	out := verifyLinks(http.DefaultClient, testMyLink, []string{good.URL, bad.URL})
	assert.Equal(t, []string{good.URL}, out)
}

func TestHTTPSFieldValues(t *testing.T) {
	p := &model.UserProfile{Fields: datatypes.JSON(`[
		{"name":"site","value":"https://a.example"},
		{"name":"plain","value":"not a url"},
		{"name":"http","value":"http://insecure.example"},
		{"name":"site2","value":"https://b.example"}
	]`)}
	assert.Equal(t, []string{"https://a.example", "https://b.example"}, httpsFieldValues(p))

	assert.Nil(t, httpsFieldValues(nil))
	assert.Nil(t, httpsFieldValues(&model.UserProfile{}))
}
