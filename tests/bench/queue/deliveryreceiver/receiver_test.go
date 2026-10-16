package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testActivityID = "https://sender.example/notes/n1/activity"

func newTestReceiver(t *testing.T) (*receiver, *activitypub.PrivateKey) {
	t.Helper()
	privatePEM, publicPEM, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	keyID := "https://sender.example/users/bench#main-key"
	privateKey, err := activitypub.NewPrivateKey(keyID, privatePEM)
	require.NoError(t, err)
	r, err := newReceiver(config{
		host: "receiver.example",
		publicKeys: map[string]string{
			keyID: publicPEM,
		},
	})
	require.NoError(t, err)
	return r, privateKey
}

func signedRequest(t *testing.T, key *activitypub.PrivateKey, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://receiver.example"+path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/activity+json")
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest([]byte(body)),
		[]string{"(request-target)", "host", "date", "digest"}))
	return req
}

func activityBody(id, content string) string {
	return fmt.Sprintf(`{"id":%q,"type":"Create","actor":"https://sender.example/users/bench","object":{"id":"https://sender.example/notes/n1","type":"Note","content":%q}}`, id, content)
}

func expectedTuple(t *testing.T, id, inbox, body string) expectedDelivery {
	t.Helper()
	digest, err := activityBodyDigest([]byte(body))
	require.NoError(t, err)
	return expectedDelivery{ActivityID: id, Inbox: inbox, BodySHA256: digest}
}

func finalizeExpected(t *testing.T, r *receiver, deliveries ...expectedDelivery) {
	t.Helper()
	require.NoError(t, r.setExpected(deliveries))
}

func send(t *testing.T, r *receiver, req *http.Request) int {
	t.Helper()
	response := httptest.NewRecorder()
	r.ServeHTTP(response, req)
	return response.Code
}

func TestReceiverMatchesExpectedActivityInboxPairsAndCountsDuplicates(t *testing.T) {
	r, key := newTestReceiver(t)
	body1 := activityBody(testActivityID, "expected body")
	otherID := "https://sender.example/notes/n2/activity"
	body2 := activityBody(otherID, "second body")
	finalizeExpected(t, r,
		expectedTuple(t, testActivityID, "http://receiver.example/inbox/one", body1),
		expectedTuple(t, testActivityID, "http://receiver.example/inbox/two", body1),
		expectedTuple(t, otherID, "http://receiver.example/inbox/one", body2),
	)

	for _, req := range []*http.Request{
		signedRequest(t, key, "/inbox/one", body1),
		signedRequest(t, key, "/inbox/one", body1),
		signedRequest(t, key, "/inbox/two", body1),
	} {
		assert.Equal(t, http.StatusNoContent, send(t, r, req))
	}

	got := r.stats()
	assert.True(t, got.Finalized)
	assert.Equal(t, uint64(3), got.Expected)
	assert.Equal(t, uint64(3), got.Attempts)
	assert.Equal(t, uint64(3), got.Verified)
	assert.Equal(t, uint64(3), got.Valid)
	assert.Equal(t, uint64(2), got.Unique)
	assert.Equal(t, uint64(1), got.Duplicates)
	assert.Zero(t, got.Invalid)
	assert.Zero(t, got.Unexpected)
	assert.Equal(t, uint64(1), got.Missing)
}

func TestReceiverDoesNotLetUnexpectedIDFillMissingExpectedPair(t *testing.T) {
	r, key := newTestReceiver(t)
	expectedBody := activityBody(testActivityID, "expected")
	finalizeExpected(t, r, expectedTuple(t, testActivityID, "http://receiver.example/inbox/one", expectedBody))

	unexpectedID := "https://sender.example/notes/other/activity"
	unexpectedBody := activityBody(unexpectedID, "unexpected")
	assert.Equal(t, http.StatusNoContent, send(t, r, signedRequest(t, key, "/inbox/one", unexpectedBody)))

	got := r.stats()
	assert.Equal(t, uint64(1), got.Expected)
	assert.Equal(t, uint64(1), got.Attempts)
	assert.Equal(t, uint64(1), got.Verified)
	assert.Zero(t, got.Valid)
	assert.Zero(t, got.Unique)
	assert.Equal(t, uint64(1), got.Unexpected)
	assert.Equal(t, uint64(1), got.UnexpectedAttempts)
	assert.Equal(t, uint64(1), got.Missing)
}

func TestReceiverRejectsSameExpectedIDWithDifferentSignedBody(t *testing.T) {
	r, key := newTestReceiver(t)
	expectedBody := activityBody(testActivityID, "expected body")
	wrongBody := activityBody(testActivityID, "different body")
	finalizeExpected(t, r, expectedTuple(t, testActivityID, "http://receiver.example/inbox/one", expectedBody))

	assert.Equal(t, http.StatusNoContent, send(t, r, signedRequest(t, key, "/inbox/one", expectedBody)))
	// The second request is correctly signed for its altered body. Signature
	// validity alone must not make it an accepted duplicate.
	assert.Equal(t, http.StatusNoContent, send(t, r, signedRequest(t, key, "/inbox/one", wrongBody)))

	got := r.stats()
	assert.Equal(t, uint64(2), got.Attempts)
	assert.Equal(t, uint64(2), got.Verified)
	assert.Equal(t, uint64(1), got.Valid)
	assert.Equal(t, uint64(1), got.Unique)
	assert.Zero(t, got.Duplicates)
	assert.Equal(t, uint64(1), got.Invalid)
	assert.Equal(t, uint64(1), got.ContentMismatch)
	assert.Equal(t, uint64(0), got.Missing)
}

func TestReceiverLeavesSignedArrivalsPendingUntilManifestIsSet(t *testing.T) {
	r, key := newTestReceiver(t)
	body := activityBody(testActivityID, "expected")
	assert.Equal(t, http.StatusNoContent, send(t, r, signedRequest(t, key, "/inbox/one", body)))

	before := r.stats()
	assert.False(t, before.Finalized)
	assert.Equal(t, uint64(1), before.Pending)
	assert.Zero(t, before.Valid, "a signed request is not confirmed as expected before manifest reconciliation")

	finalizeExpected(t, r, expectedTuple(t, testActivityID, "http://receiver.example/inbox/one", body))
	after := r.stats()
	assert.True(t, after.Finalized)
	assert.Equal(t, uint64(1), after.Valid)
	assert.Zero(t, after.Missing)
}

func TestReceiverCountsTamperedBodyAsInvalid(t *testing.T) {
	r, key := newTestReceiver(t)
	body := activityBody(testActivityID, "expected")
	finalizeExpected(t, r, expectedTuple(t, testActivityID, "http://receiver.example/inbox/one", body))
	req := signedRequest(t, key, "/inbox/one", body)
	req.Body = http.NoBody
	assert.Equal(t, http.StatusUnauthorized, send(t, r, req))

	got := r.stats()
	assert.Equal(t, uint64(1), got.Attempts)
	assert.Zero(t, got.Verified)
	assert.Zero(t, got.Valid)
	assert.Equal(t, uint64(1), got.Invalid)
	assert.Equal(t, uint64(1), got.Missing)
}

func TestReceiverCountsInvalidCryptographicSignature(t *testing.T) {
	r, key := newTestReceiver(t)
	body := activityBody(testActivityID, "expected")
	finalizeExpected(t, r, expectedTuple(t, testActivityID, "http://receiver.example/inbox/one", body))
	req := signedRequest(t, key, "/inbox/one", body)
	parsed, err := activitypub.ParseSignatureHeader(req.Header.Get("Signature"))
	require.NoError(t, err)
	req.Header.Set("Signature", fmt.Sprintf(`keyId=%q,algorithm=%q,headers=%q,signature=%q`,
		parsed.KeyID, parsed.Algorithm, strings.Join(parsed.Headers, " "), base64.StdEncoding.EncodeToString([]byte{1})))
	assert.Equal(t, http.StatusUnauthorized, send(t, r, req))

	got := r.stats()
	assert.Equal(t, uint64(1), got.Attempts)
	assert.Zero(t, got.Verified)
	assert.Equal(t, uint64(1), got.Invalid)
	assert.Equal(t, uint64(1), got.Missing)
}

func TestReceiverCountsValidSignatureWithoutActivityIDAsInvalid(t *testing.T) {
	r, key := newTestReceiver(t)
	expectedBody := activityBody(testActivityID, "expected")
	finalizeExpected(t, r, expectedTuple(t, testActivityID, "http://receiver.example/inbox/one", expectedBody))
	req := signedRequest(t, key, "/inbox/one", `{"type":"Create"}`)
	assert.Equal(t, http.StatusBadRequest, send(t, r, req))

	got := r.stats()
	assert.Equal(t, uint64(1), got.Attempts)
	assert.Zero(t, got.Verified)
	assert.Equal(t, uint64(1), got.Invalid)
}

func TestReceiverResetClearsManifestAndObservationsButKeepsKeys(t *testing.T) {
	r, key := newTestReceiver(t)
	body := activityBody(testActivityID, "expected")
	finalizeExpected(t, r, expectedTuple(t, testActivityID, "http://receiver.example/inbox/one", body))
	r.ServeHTTP(httptest.NewRecorder(), signedRequest(t, key, "/inbox/one", body))

	r.reset()
	got := r.stats()
	assert.False(t, got.Finalized)
	assert.Zero(t, got.Attempts)
	assert.Zero(t, got.Verified)
	assert.Zero(t, got.Expected)

	// The configured fixture key remains available after a per-run reset.
	assert.Equal(t, http.StatusNoContent, send(t, r, signedRequest(t, key, "/inbox/one", body)))
}

func TestReceiverRejectsDuplicateManifestTuple(t *testing.T) {
	r, _ := newTestReceiver(t)
	body := activityBody(testActivityID, "expected")
	delivery := expectedTuple(t, testActivityID, "http://receiver.example/inbox/one", body)
	err := r.setExpected([]expectedDelivery{delivery, delivery})
	require.ErrorContains(t, err, "duplicate expected tuple")
}

func TestReceiverRejectsEmptyManifest(t *testing.T) {
	r, _ := newTestReceiver(t)
	require.ErrorContains(t, r.setExpected(nil), "at least one delivery")
	assert.False(t, r.stats().Finalized)
}

func TestReceiverAcceptsSeederPublicKeysThroughSetupEndpoint(t *testing.T) {
	privatePEM, publicPEM, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	keyID := "https://sender.example/users/bench#main-key"
	key, err := activitypub.NewPrivateKey(keyID, privatePEM)
	require.NoError(t, err)
	r, err := newReceiver(config{host: "receiver.example"})
	require.NoError(t, err)

	payload, err := json.Marshal(map[string]any{"keys": map[string]string{keyID: publicPEM}})
	require.NoError(t, err)
	setup := httptest.NewRequest(http.MethodPost, "http://receiver.example/keys", bytes.NewReader(payload))
	assert.Equal(t, http.StatusNoContent, send(t, r, setup))

	body := activityBody(testActivityID, "expected")
	finalizeExpected(t, r, expectedTuple(t, testActivityID, "http://receiver.example/inbox/one", body))
	assert.Equal(t, http.StatusNoContent, send(t, r, signedRequest(t, key, "/inbox/one", body)))
	assert.Equal(t, uint64(1), r.stats().Valid)
}

type failingReadCloser struct{}

func (failingReadCloser) Read([]byte) (int, error) { return 0, errors.New("fixture read failure") }
func (failingReadCloser) Close() error             { return nil }

func TestReceiverServeHTTPRoutesAndMethods(t *testing.T) {
	r, _ := newTestReceiver(t)
	body := activityBody(testActivityID, "route test")
	manifest, err := json.Marshal(expectedManifest{Deliveries: []expectedDelivery{
		expectedTuple(t, testActivityID, "http://receiver.example/inbox/one", body),
	}})
	require.NoError(t, err)

	cases := []struct {
		method string
		target string
		body   string
		status int
	}{
		{http.MethodGet, "/healthz", "", http.StatusOK},
		{http.MethodPost, "/healthz", "", http.StatusMethodNotAllowed},
		{http.MethodGet, "/keys", "", http.StatusMethodNotAllowed},
		{http.MethodPost, "/keys", `{`, http.StatusBadRequest},
		{http.MethodPost, "/keys", `{}`, http.StatusBadRequest},
		{http.MethodGet, "/expect", "", http.StatusMethodNotAllowed},
		{http.MethodPost, "/expect", `{`, http.StatusBadRequest},
		{http.MethodGet, "/stats", "", http.StatusOK},
		{http.MethodPost, "/stats", "", http.StatusMethodNotAllowed},
		{http.MethodGet, "/reset", "", http.StatusMethodNotAllowed},
		{http.MethodPost, "/expect", string(manifest), http.StatusNoContent},
		{http.MethodPost, "/expect", string(manifest), http.StatusConflict},
		{http.MethodGet, "/inbox/one", "", http.StatusMethodNotAllowed},
		{http.MethodPost, "/unknown", "{}", http.StatusUnauthorized},
		{http.MethodPost, "/reset", "", http.StatusNoContent},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "http://receiver.example"+tc.target, strings.NewReader(tc.body))
			response := httptest.NewRecorder()
			r.ServeHTTP(response, req)
			assert.Equal(t, tc.status, response.Code)
			if tc.target == "/stats" && tc.method == http.MethodGet {
				assert.Equal(t, "application/json", response.Header().Get("Content-Type"))
				var got stats
				assert.NoError(t, json.Unmarshal(response.Body.Bytes(), &got))
			}
		})
	}
}

func TestReceiverConfigurationAndPublicKeyValidation(t *testing.T) {
	_, err := newReceiver(config{host: "  "})
	require.ErrorContains(t, err, "receiver host is required")
	_, err = newReceiver(config{host: "receiver.example", publicKeys: map[string]string{"key": "not a PEM"}})
	require.ErrorContains(t, err, "invalid public key")

	r, key := newTestReceiver(t)
	publicPEM := r.publicKeys[key.KeyID]
	assert.ErrorContains(t, r.addPublicKeys(nil), "at least one public key")
	assert.ErrorContains(t, r.addPublicKeys(map[string]string{" ": publicPEM}), "ID must not be empty")
	assert.ErrorContains(t, r.addPublicKeys(map[string]string{"new-key": "not a PEM"}), "invalid public key")
	assert.NoError(t, r.addPublicKeys(map[string]string{key.KeyID: publicPEM}), "adding the same key is idempotent")

	_, otherPublicPEM, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	assert.ErrorContains(t, r.addPublicKeys(map[string]string{key.KeyID: otherPublicPEM}), "already configured")
}

func TestDecodeBoundedJSONErrorCases(t *testing.T) {
	var out map[string]string
	assert.Error(t, decodeBoundedJSON(failingReadCloser{}, 16, &out))
	assert.ErrorContains(t, decodeBoundedJSON(strings.NewReader(strings.Repeat("x", 17)), 16, &out), "exceeds")
	assert.Error(t, decodeBoundedJSON(strings.NewReader("{"), 16, &out))
	assert.NoError(t, decodeBoundedJSON(strings.NewReader(`{"ok":"yes"}`), 16, &out))
	assert.Equal(t, "yes", out["ok"])
}

func TestExpectedManifestValidation(t *testing.T) {
	r, _ := newTestReceiver(t)
	digest := strings.Repeat("a", 64)
	valid := expectedDelivery{ActivityID: testActivityID, Inbox: "http://receiver.example/inbox", BodySHA256: digest}
	for _, tc := range []struct {
		name      string
		delivery  expectedDelivery
		wantError string
	}{
		{"empty activity ID", expectedDelivery{Inbox: valid.Inbox, BodySHA256: digest}, "activity ID must not be empty"},
		{"relative inbox", expectedDelivery{ActivityID: testActivityID, Inbox: "/inbox", BodySHA256: digest}, "absolute HTTP(S) URL"},
		{"empty digest", expectedDelivery{ActivityID: testActivityID, Inbox: valid.Inbox}, "body SHA-256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.ErrorContains(t, r.setExpected([]expectedDelivery{tc.delivery}), tc.wantError)
			assert.False(t, r.stats().Finalized)
		})
	}
	require.NoError(t, r.setExpected([]expectedDelivery{valid}))
	assert.ErrorContains(t, r.setExpected([]expectedDelivery{valid}), "already finalized")

	for _, tc := range []struct {
		url       string
		wantError bool
	}{
		{"http://receiver.example/inbox", false},
		{"https://receiver.example/inbox", false},
		{"/relative", true},
		{"ftp://receiver.example/inbox", true},
		{"http:///no-host", true},
		{"http://user:pass@receiver.example/inbox", true},
		{"http://[::1", true},
	} {
		if tc.wantError {
			assert.Error(t, validateInboxURL(tc.url), tc.url)
		} else {
			assert.NoError(t, validateInboxURL(tc.url), tc.url)
		}
	}

	got, err := normalizeDigest(strings.Repeat("A", 64))
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("a", 64), got)
	_, err = normalizeDigest("not-hex")
	assert.Error(t, err)
	_, err = normalizeDigest(strings.Repeat("a", 62))
	assert.Error(t, err)
}

func TestReceiverRejectsUnavailableAndUnreadableRequests(t *testing.T) {
	withoutKeys, err := newReceiver(config{host: "receiver.example"})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "http://receiver.example/inbox", strings.NewReader("{}"))
	assert.Equal(t, http.StatusServiceUnavailable, send(t, withoutKeys, req))

	r, _ := newTestReceiver(t)
	readFailure := httptest.NewRequest(http.MethodPost, "http://receiver.example/inbox", nil)
	readFailure.Body = failingReadCloser{}
	assert.Equal(t, http.StatusBadRequest, send(t, r, readFailure))

	tooLarge := strings.NewReader(strings.Repeat("x", maxActivityBodyBytes+1))
	oversized := httptest.NewRequest(http.MethodPost, "http://receiver.example/inbox", tooLarge)
	assert.Equal(t, http.StatusRequestEntityTooLarge, send(t, r, oversized))
}

func TestReceiverRejectsSignatureAdmissionAndTargetFailures(t *testing.T) {
	r, key := newTestReceiver(t)
	body := activityBody(testActivityID, "expected")
	makeSigned := func() *http.Request { return signedRequest(t, key, "/inbox/one", body) }

	missingSignature := makeSigned()
	missingSignature.Header.Del("Signature")
	assert.Equal(t, http.StatusUnauthorized, send(t, r, missingSignature))

	malformedSignature := makeSigned()
	malformedSignature.Header.Set("Signature", "not-a-signature")
	assert.Equal(t, http.StatusUnauthorized, send(t, r, malformedSignature))

	unknownKey := makeSigned()
	parsed, err := activitypub.ParseSignatureHeader(unknownKey.Header.Get("Signature"))
	require.NoError(t, err)
	unknownKey.Header.Set("Signature", fmt.Sprintf(`keyId=%q,algorithm=%q,headers=%q,signature=%q`,
		"https://sender.example/users/unknown#key", parsed.Algorithm, strings.Join(parsed.Headers, " "), parsed.Signature))
	assert.Equal(t, http.StatusUnauthorized, send(t, r, unknownKey))

	wrongHost := makeSigned()
	wrongHost.Host = "other.example"
	assert.Equal(t, http.StatusUnauthorized, send(t, r, wrongHost))

	wrongDate := makeSigned()
	wrongDate.Header.Set("Date", "not-a-date")
	assert.Equal(t, http.StatusUnauthorized, send(t, r, wrongDate))

	wrongTarget := makeSigned()
	wrongTarget.URL.Path = "/inbox/tampered"
	assert.Equal(t, http.StatusUnauthorized, send(t, r, wrongTarget))
}

func TestActivityBodyDigestRejectsInvalidTrailingJSON(t *testing.T) {
	for _, body := range []string{"not-json", `{} {}`, `{} trailing`} {
		_, err := activityBodyDigest([]byte(body))
		assert.Error(t, err, body)
	}
}

func TestInboxURLUsesRequestSchemeAndHostFallbacks(t *testing.T) {
	explicit := &http.Request{URL: &url.URL{Scheme: "https", Host: "url.example", Path: "/inbox"}, Host: "header.example"}
	assert.Equal(t, "https://header.example/inbox", inboxURL(explicit))

	plain := &http.Request{URL: &url.URL{Host: "url.example", Path: "/inbox", RawQuery: "x=1"}}
	assert.Equal(t, "http://url.example/inbox?x=1", inboxURL(plain))

	tlsRequest := &http.Request{URL: &url.URL{Host: "url.example", Path: "/inbox"}, TLS: &tls.ConnectionState{}}
	assert.Equal(t, "https://url.example/inbox", inboxURL(tlsRequest))
}
