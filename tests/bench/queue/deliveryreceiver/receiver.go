package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/elythia-network/elythia/internal/activitypub"
)

const (
	maxActivityBodyBytes = 4 << 20
	maxExpectedBodyBytes = 8 << 20
	maxPublicKeysBody    = 1 << 20
)

type config struct {
	host       string
	publicKeys map[string]string
}

type deliveryKey struct {
	activityID string
	inbox      string
}

type expectedDelivery struct {
	ActivityID string `json:"activityId"`
	Inbox      string `json:"inbox"`
	BodySHA256 string `json:"bodySha256"`
}

type expectedManifest struct {
	Deliveries []expectedDelivery `json:"deliveries"`
}

// stats is the finalized run summary. Valid requests match both the expected
// Activity ID/inbox pair and its expected canonical JSON body. Unexpected is
// a count of distinct unlisted pairs; UnexpectedAttempts counts their requests.
type stats struct {
	Finalized          bool   `json:"finalized"`
	Expected           uint64 `json:"expected"`
	Attempts           uint64 `json:"attempts"`
	Verified           uint64 `json:"verified"`
	Pending            uint64 `json:"pending"`
	Valid              uint64 `json:"valid"`
	Unique             uint64 `json:"unique"`
	Duplicates         uint64 `json:"duplicates"`
	Invalid            uint64 `json:"invalid"`
	ContentMismatch    uint64 `json:"contentMismatch"`
	Unexpected         uint64 `json:"unexpected"`
	UnexpectedAttempts uint64 `json:"unexpectedAttempts"`
	Missing            uint64 `json:"missing"`
}

type activity struct {
	ID string `json:"id"`
}

type receiver struct {
	host string
	mu   sync.RWMutex

	publicKeys map[string]string
	finalized  bool
	expected   map[deliveryKey]string
	// observed retains body digests until the run manifest is supplied. Note
	// creation returns the Activity ID only after enqueueing delivery, so the
	// outbound driver finalizes this set after posting its synthetic notes.
	observed map[deliveryKey]map[string]uint64
	attempts uint64
	verified uint64
	invalid  uint64
}

func newReceiver(cfg config) (*receiver, error) {
	if strings.TrimSpace(cfg.host) == "" {
		return nil, errors.New("receiver host is required")
	}
	r := &receiver{
		host:       cfg.host,
		publicKeys: make(map[string]string, len(cfg.publicKeys)),
		observed:   make(map[deliveryKey]map[string]uint64),
	}
	if len(cfg.publicKeys) > 0 {
		if err := r.addPublicKeys(cfg.publicKeys); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/healthz":
		if req.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	case "/keys":
		if req.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.handleKeys(w, req)
		return
	case "/expect":
		if req.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.handleExpected(w, req)
		return
	case "/stats":
		if req.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(r.stats())
		return
	case "/reset":
		if req.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.reset()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.receive(w, req)
}

func (r *receiver) handleKeys(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Keys map[string]string `json:"keys"`
	}
	if err := decodeBoundedJSON(req.Body, maxPublicKeysBody, &body); err != nil {
		http.Error(w, "invalid public key configuration", http.StatusBadRequest)
		return
	}
	if err := r.addPublicKeys(body.Keys); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (r *receiver) handleExpected(w http.ResponseWriter, req *http.Request) {
	var manifest expectedManifest
	if err := decodeBoundedJSON(req.Body, maxExpectedBodyBytes, &manifest); err != nil {
		http.Error(w, "invalid expected manifest", http.StatusBadRequest)
		return
	}
	if err := r.setExpected(manifest.Deliveries); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeBoundedJSON(body io.Reader, limit int64, out any) error {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("JSON body exceeds %d bytes", limit)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return err
	}
	return nil
}

func (r *receiver) addPublicKeys(keys map[string]string) error {
	if len(keys) == 0 {
		return errors.New("at least one public key is required")
	}
	for keyID, publicPEM := range keys {
		if strings.TrimSpace(keyID) == "" {
			return errors.New("public key ID must not be empty")
		}
		if _, _, err := activitypub.ParsePublicKey(publicPEM); err != nil {
			return fmt.Errorf("invalid public key for %q: %w", keyID, err)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for keyID, publicPEM := range keys {
		if existing, ok := r.publicKeys[keyID]; ok && existing != publicPEM {
			return fmt.Errorf("public key ID %q is already configured with different key material", keyID)
		}
	}
	for keyID, publicPEM := range keys {
		r.publicKeys[keyID] = publicPEM
	}
	return nil
}

func (r *receiver) setExpected(deliveries []expectedDelivery) error {
	if len(deliveries) == 0 {
		return errors.New("expected manifest must contain at least one delivery")
	}
	expected := make(map[deliveryKey]string, len(deliveries))
	for _, delivery := range deliveries {
		if strings.TrimSpace(delivery.ActivityID) == "" {
			return errors.New("expected activity ID must not be empty")
		}
		if err := validateInboxURL(delivery.Inbox); err != nil {
			return err
		}
		digest, err := normalizeDigest(delivery.BodySHA256)
		if err != nil {
			return fmt.Errorf("expected body SHA-256: %w", err)
		}
		key := deliveryKey{activityID: delivery.ActivityID, inbox: delivery.Inbox}
		if _, duplicate := expected[key]; duplicate {
			return fmt.Errorf("duplicate expected tuple (%q, %q)", delivery.ActivityID, delivery.Inbox)
		}
		expected[key] = digest
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finalized {
		return errors.New("expected manifest is already finalized; reset before replacing it")
	}
	r.expected = expected
	r.finalized = true
	return nil
}

func validateInboxURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return fmt.Errorf("expected inbox %q must be an absolute HTTP(S) URL without userinfo", raw)
	}
	return nil
}

func normalizeDigest(raw string) (string, error) {
	b, err := hex.DecodeString(raw)
	if err != nil || len(b) != sha256.Size {
		return "", errors.New("want 64 hexadecimal characters")
	}
	return hex.EncodeToString(b), nil
}

func (r *receiver) receive(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.attempts++
	publicKeys := make(map[string]string, len(r.publicKeys))
	for keyID, pem := range r.publicKeys {
		publicKeys[keyID] = pem
	}
	r.mu.Unlock()
	defer req.Body.Close()
	if len(publicKeys) == 0 {
		r.reject(w, http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(io.LimitReader(req.Body, maxActivityBodyBytes+1))
	if err != nil {
		r.reject(w, http.StatusBadRequest)
		return
	}
	if len(body) > maxActivityBodyBytes {
		r.reject(w, http.StatusRequestEntityTooLarge)
		return
	}

	parsed, err := activitypub.ParseSignatureHeader(req.Header.Get("Signature"))
	if err != nil {
		r.reject(w, http.StatusUnauthorized)
		return
	}
	publicPEM, ok := publicKeys[parsed.KeyID]
	if !ok {
		r.reject(w, http.StatusUnauthorized)
		return
	}
	if err := activitypub.VerifyInboxAdmission(parsed, req.Host, r.host,
		req.Header.Get("Date"), req.Header.Get("Digest"), body); err != nil {
		r.reject(w, http.StatusUnauthorized)
		return
	}

	// VerifyRequest needs the same method, target and headers that were signed.
	req.Body = io.NopCloser(bytes.NewReader(body))
	if err := activitypub.VerifyRequest(req, publicPEM); err != nil {
		r.reject(w, http.StatusUnauthorized)
		return
	}

	var act activity
	if err := json.Unmarshal(body, &act); err != nil || strings.TrimSpace(act.ID) == "" {
		r.reject(w, http.StatusBadRequest)
		return
	}
	bodyDigest, err := activityBodyDigest(body)
	if err != nil {
		r.reject(w, http.StatusBadRequest)
		return
	}

	key := deliveryKey{activityID: act.ID, inbox: inboxURL(req)}
	r.mu.Lock()
	if r.observed[key] == nil {
		r.observed[key] = make(map[string]uint64)
	}
	r.observed[key][bodyDigest]++
	r.verified++
	r.mu.Unlock()

	// A correctly signed delivery is acknowledged even when later manifest
	// reconciliation marks it unexpected or content-mismatched. This fixture
	// measures delivery and does not turn those findings into retry load.
	w.WriteHeader(http.StatusNoContent)
}

func activityBodyDigest(body []byte) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return "", errors.New("multiple JSON values in activity body")
		}
		return "", err
	}
	var canonical bytes.Buffer
	encoder := json.NewEncoder(&canonical)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	compact := bytes.TrimSuffix(canonical.Bytes(), []byte("\n"))
	digest := sha256.Sum256(compact)
	return hex.EncodeToString(digest[:]), nil
}

func inboxURL(req *http.Request) string {
	scheme := req.URL.Scheme
	if scheme == "" {
		if req.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	return scheme + "://" + host + req.URL.RequestURI()
}

func (r *receiver) reject(w http.ResponseWriter, status int) {
	r.mu.Lock()
	r.invalid++
	r.mu.Unlock()
	http.Error(w, http.StatusText(status), status)
}

func (r *receiver) stats() stats {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := stats{
		Finalized: r.finalized,
		Attempts:  r.attempts,
		Verified:  r.verified,
		Invalid:   r.invalid,
	}
	if !r.finalized {
		out.Pending = r.verified
		return out
	}
	out.Expected = uint64(len(r.expected))
	for key, bodies := range r.observed {
		expectedDigest, exists := r.expected[key]
		if !exists {
			out.Unexpected++
			for _, count := range bodies {
				out.UnexpectedAttempts += count
			}
			continue
		}
		matching := bodies[expectedDigest]
		if matching > 0 {
			out.Unique++
			out.Valid += matching
		}
		for bodyDigest, count := range bodies {
			if bodyDigest != expectedDigest {
				out.ContentMismatch += count
				out.Invalid += count
			}
		}
	}
	if out.Valid > out.Unique {
		out.Duplicates = out.Valid - out.Unique
	}
	if out.Expected > out.Unique {
		out.Missing = out.Expected - out.Unique
	}
	return out
}

func (r *receiver) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finalized = false
	r.expected = nil
	r.observed = make(map[deliveryKey]map[string]uint64)
	r.attempts = 0
	r.verified = 0
	r.invalid = 0
}
