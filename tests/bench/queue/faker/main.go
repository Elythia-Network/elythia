// Package main implements a controllable AP HTTP-signature sender used
// by the queue bench (#563) to drive receiver inbox throughput
// measurements. The faker is intentionally Misskey-implementation
// agnostic so that inbound bench numbers reflect pure receiver
// performance instead of the sending stack's overhead.
//
// The binary runs two listeners:
//
//   - HTTPS on :443 — serves the AP actor JSON and key material so a
//     receiver can fetch them if it ever wants to (we also pre-seed
//     receiver DBs to avoid the round-trip in steady state).
//   - HTTP control API on :8081 — `/send` triggers a pre-sign + blast
//     run against the supplied target inbox URLs and returns aggregate
//     timing.
package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultActorPath = "/users/bench-sender"
	keyIDFragment    = "#main-key"
)

type config struct {
	hostname  string
	httpsAddr string
	ctrlAddr  string
	certFile  string
	keyFile   string
	stateDir  string
}

type faker struct {
	cfg        config
	actorURI   string
	keyID      string
	privKey    *rsa.PrivateKey
	pubPEM     string
	actorJSON  []byte
	httpClient *http.Client
}

func main() {
	cfg := config{}
	flag.StringVar(&cfg.hostname, "host", envOr("FAKER_HOST", "faker"), "hostname (used in actor URI)")
	flag.StringVar(&cfg.httpsAddr, "https-addr", ":443", "HTTPS listen for actor JSON")
	flag.StringVar(&cfg.ctrlAddr, "ctrl-addr", ":8081", "HTTP control API listen")
	flag.StringVar(&cfg.certFile, "cert", "/certs/faker.crt", "TLS cert for HTTPS")
	flag.StringVar(&cfg.keyFile, "key", "/certs/faker.key", "TLS key for HTTPS")
	flag.StringVar(&cfg.stateDir, "state", "/state", "directory for persisted RSA key")
	flag.Parse()

	f, err := newFaker(cfg)
	if err != nil {
		log.Fatal(err)
	}

	go f.runHTTPS()
	f.runControl()
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func newFaker(cfg config) (*faker, error) {
	if err := os.MkdirAll(cfg.stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir state: %w", err)
	}
	priv, pub, err := loadOrCreateKey(filepath.Join(cfg.stateDir, "actor.pem"))
	if err != nil {
		return nil, err
	}

	actorURI := "https://" + cfg.hostname + defaultActorPath
	keyID := actorURI + keyIDFragment

	actor := map[string]any{
		"@context": []any{
			"https://www.w3.org/ns/activitystreams",
			"https://w3id.org/security/v1",
		},
		"type":              "Person",
		"id":                actorURI,
		"preferredUsername": "bench-sender",
		"name":              "Queue Bench Sender",
		"inbox":             actorURI + "/inbox",
		"outbox":            actorURI + "/outbox",
		"publicKey": map[string]any{
			"id":           keyID,
			"owner":        actorURI,
			"publicKeyPem": pub,
		},
	}
	actorJSON, err := json.Marshal(actor)
	if err != nil {
		return nil, err
	}

	// Receiver側 verify がボトルネックになる構成なので sender 側は大量に
	// keep-alive コネクションを張りっぱなしにして TLS handshake のコスト
	// を sender 側に被せない。
	tr := &http.Transport{
		MaxIdleConns:        1024,
		MaxIdleConnsPerHost: 256,
		MaxConnsPerHost:     0,
		IdleConnTimeout:     90 * time.Second,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // bench only
		ForceAttemptHTTP2:   false,
	}
	return &faker{
		cfg:        cfg,
		actorURI:   actorURI,
		keyID:      keyID,
		privKey:    priv,
		pubPEM:     pub,
		actorJSON:  actorJSON,
		httpClient: &http.Client{Transport: tr, Timeout: 30 * time.Second},
	}, nil
}

func loadOrCreateKey(path string) (*rsa.PrivateKey, string, error) {
	if data, err := os.ReadFile(path); err == nil {
		blk, _ := pem.Decode(data)
		if blk == nil {
			return nil, "", errors.New("invalid pem")
		}
		k, err := x509.ParsePKCS1PrivateKey(blk.Bytes)
		if err != nil {
			return nil, "", fmt.Errorf("parse key: %w", err)
		}
		return k, encodePublicPEM(&k.PublicKey), nil
	}
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, "", fmt.Errorf("gen key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		return nil, "", err
	}
	return k, encodePublicPEM(&k.PublicKey), nil
}

func encodePublicPEM(pub *rsa.PublicKey) string {
	der, _ := x509.MarshalPKIXPublicKey(pub)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func (f *faker) runHTTPS() {
	mux := http.NewServeMux()
	mux.HandleFunc(defaultActorPath, f.handleActor)
	mux.HandleFunc("/.well-known/webfinger", f.handleWebfinger)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Addr: f.cfg.httpsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("faker HTTPS listening on %s", f.cfg.httpsAddr)
	if err := srv.ListenAndServeTLS(f.cfg.certFile, f.cfg.keyFile); err != nil {
		log.Fatalf("https: %v", err)
	}
}

func (f *faker) handleActor(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/activity+json")
	_, _ = w.Write(f.actorJSON)
}

func (f *faker) handleWebfinger(w http.ResponseWriter, r *http.Request) {
	resource := r.URL.Query().Get("resource")
	expected := "acct:bench-sender@" + f.cfg.hostname
	if resource != expected {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	resp := map[string]any{
		"subject": resource,
		"links": []map[string]any{
			{"rel": "self", "type": "application/activity+json", "href": f.actorURI},
		},
	}
	w.Header().Set("Content-Type", "application/jrd+json")
	_ = json.NewEncoder(w).Encode(resp)
}

// sendRequest is the input to /send.
type sendRequest struct {
	Targets     []string `json:"targets"`     // inbox URLs
	Count       int      `json:"count"`       // requests per target (total = count*len(targets))
	Concurrency int      `json:"concurrency"` // parallel workers per target

	// ActivityType selects payload shape:
	//   "" / "create"  → Create(Note) (default、後方互換)
	//   "announce"     → Announce(object=Objects[target])
	ActivityType string `json:"activityType,omitempty"`
	// Objects maps inbox URL → AS object URI used by Announce mode.
	// e.g. `{"https://mk-mkq/inbox": "https://mk-mkq/notes/<id>"}`
	// announce mode で全 target に同じ note URI を Announce すると receiver 側で
	// (act.ID dedup 経由でない場合) 重複 renote が大量に作られて bench にならない
	// ため、receiver-local の note URI を target ごとに分けて渡す。
	Objects map[string]string `json:"objects,omitempty"`
}

type sendStats struct {
	Target     string  `json:"target"`
	Sent       int     `json:"sent"`
	OK         int     `json:"ok"`
	Failed     int     `json:"failed"`
	DurationMs float64 `json:"durationMs"`
	RPS        float64 `json:"rps"`
}

type sendResponse struct {
	PreSignMs  float64     `json:"preSignMs"`
	TotalMs    float64     `json:"totalMs"`
	Stats      []sendStats `json:"stats"`
	Concurrent int         `json:"concurrent"`
}

func (f *faker) runControl() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/send", f.handleSend)
	srv := &http.Server{Addr: f.cfg.ctrlAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("faker control listening on %s", f.cfg.ctrlAddr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("ctrl: %v", err)
	}
}

func (f *faker) handleSend(w http.ResponseWriter, r *http.Request) {
	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Count <= 0 || len(req.Targets) == 0 {
		http.Error(w, "count and targets required", http.StatusBadRequest)
		return
	}
	if req.Concurrency <= 0 {
		req.Concurrency = 64
	}

	resp, err := f.runSend(r.Context(), req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// signedReq is a fully pre-signed HTTP request that the send phase can
// blast verbatim. We pre-compute on the sender side so faker never
// becomes a bottleneck (#563).
type signedReq struct {
	target  string
	body    []byte
	headers map[string]string
}

func (f *faker) runSend(ctx context.Context, req sendRequest) (*sendResponse, error) {
	preStart := time.Now()
	signed, err := f.preSign(req)
	if err != nil {
		return nil, err
	}
	preSignMs := float64(time.Since(preStart).Microseconds()) / 1000.0

	totalStart := time.Now()
	stats := make([]sendStats, len(req.Targets))
	var wg sync.WaitGroup
	for i, target := range req.Targets {
		wg.Add(1)
		go func(idx int, target string, batch []signedReq) {
			defer wg.Done()
			stats[idx] = f.blast(ctx, target, batch, req.Concurrency)
		}(i, target, signed[target])
	}
	wg.Wait()
	totalMs := float64(time.Since(totalStart).Microseconds()) / 1000.0

	return &sendResponse{
		PreSignMs:  preSignMs,
		TotalMs:    totalMs,
		Stats:      stats,
		Concurrent: req.Concurrency,
	}, nil
}

// preSign fans out RSA signing across all CPU cores so signing never
// becomes the bottleneck during the blast phase. With RSA-2048 a single
// core does ~500-1000 signs/sec; 16 cores gives ~10-15k/sec which is
// already plenty since the bench targets ~10k jobs total.
func (f *faker) preSign(req sendRequest) (map[string][]signedReq, error) {
	type job struct {
		target string
		index  int
	}
	jobs := make(chan job, 1024)
	out := make(map[string][]signedReq, len(req.Targets))
	for _, t := range req.Targets {
		out[t] = make([]signedReq, req.Count)
	}

	workerCount := req.Concurrency
	if workerCount < 8 {
		workerCount = 8
	}
	var wg sync.WaitGroup
	var firstErr atomic.Value
	// Worker error 発生時に producer goroutine を停止させるための cancel。
	// 全 worker が同時に error で抜けると producer が channel send で block
	// したまま leak するため (#564 Devin BUG-1)、worker 側で cancel() を呼び
	// producer は ctx.Done() を select する。
	stopCtx, stop := context.WithCancel(context.Background())
	defer stop()
	for range workerCount {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				sr, err := f.buildSigned(req, j.target, j.index)
				if err != nil {
					firstErr.CompareAndSwap(nil, err)
					stop()
					return
				}
				out[j.target][j.index] = sr
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, target := range req.Targets {
			for i := range req.Count {
				select {
				case jobs <- job{target: target, index: i}:
				case <-stopCtx.Done():
					return
				}
			}
		}
	}()
	wg.Wait()

	if v := firstErr.Load(); v != nil {
		return nil, v.(error)
	}
	return out, nil
}

func (f *faker) buildSigned(req sendRequest, target string, index int) (signedReq, error) {
	now := time.Now().UTC()
	pub := []string{"https://www.w3.org/ns/activitystreams#Public"}

	// Activity type を req.ActivityType で分岐。default は Create。
	// announce mode は req.Objects[target] が宛先の note URI を保持する前提。
	// 受信側 mk の handleAnnounce は act.ID != "" の場合に FindByURI で dedup
	// するため、bench では activity.ID を per-(target, index) でユニーク化する。
	var activity map[string]any
	switch req.ActivityType {
	case "announce":
		objectURI := req.Objects[target]
		if objectURI == "" {
			return signedReq{}, fmt.Errorf("announce mode requires Objects[%q] target object URI", target)
		}
		// activity.id は per-(target, index) で完全ユニーク化する。target の
		// hash を含めないと「同じ index で別 target」が同じ ID を生成して
		// しまい、receiver 側 (handleAnnounce の act.ID ベース dedup) で
		// 1 件目以降が誤って drop される可能性がある。
		tHash := sha256.Sum256([]byte(target))
		announceID := fmt.Sprintf("%s/activities/announce-%d-%s-%d",
			f.actorURI, time.Now().UnixNano(), hex.EncodeToString(tHash[:4]), index)
		activity = map[string]any{
			"@context":  []any{"https://www.w3.org/ns/activitystreams"},
			"id":        announceID,
			"type":      "Announce",
			"actor":     f.actorURI,
			"object":    objectURI,
			"to":        pub,
			"published": now.Format(time.RFC3339),
		}
	case "", "create":
		noteID := fmt.Sprintf("%s/notes/bench-%d-%d", f.actorURI, time.Now().UnixNano(), index)
		activityID := noteID + "/activity"
		activity = map[string]any{
			"@context": []any{"https://www.w3.org/ns/activitystreams"},
			"id":       activityID,
			"type":     "Create",
			"actor":    f.actorURI,
			"to":       pub,
			"object": map[string]any{
				"id":           noteID,
				"type":         "Note",
				"attributedTo": f.actorURI,
				"to":           pub,
				"content":      fmt.Sprintf("queue-bench note %d", index),
				"published":    now.Format(time.RFC3339),
			},
		}
	default:
		return signedReq{}, fmt.Errorf("unknown activityType %q (want create | announce)", req.ActivityType)
	}
	body, err := json.Marshal(activity)
	if err != nil {
		return signedReq{}, err
	}

	digestB64 := sha256.Sum256(body)
	digestHeader := "SHA-256=" + base64.StdEncoding.EncodeToString(digestB64[:])
	host, path, err := splitHostPath(target)
	if err != nil {
		return signedReq{}, err
	}
	dateHeader := now.Format(http.TimeFormat)

	signingString := strings.Join([]string{
		"(request-target): post " + path,
		"host: " + host,
		"date: " + dateHeader,
		"digest: " + digestHeader,
	}, "\n")

	hash := sha256.Sum256([]byte(signingString))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.privKey, crypto.SHA256, hash[:])
	if err != nil {
		return signedReq{}, err
	}
	signature := fmt.Sprintf(
		`keyId="%s",algorithm="rsa-sha256",headers="(request-target) host date digest",signature="%s"`,
		f.keyID, base64.StdEncoding.EncodeToString(sig),
	)

	return signedReq{
		target: target,
		body:   body,
		headers: map[string]string{
			"Content-Type": "application/activity+json",
			"Host":         host,
			"Date":         dateHeader,
			"Digest":       digestHeader,
			"Signature":    signature,
		},
	}, nil
}

func splitHostPath(rawURL string) (host, path string, err error) {
	rest := strings.TrimPrefix(strings.TrimPrefix(rawURL, "https://"), "http://")
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return rest, "/", nil
	}
	return rest[:slash], rest[slash:], nil
}

func (f *faker) blast(ctx context.Context, target string, batch []signedReq, concurrency int) sendStats {
	start := time.Now()
	jobs := make(chan int, concurrency*2)
	var ok, fail atomic.Int64

	var wg sync.WaitGroup
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if f.send(ctx, batch[idx]) {
					ok.Add(1)
				} else {
					fail.Add(1)
				}
			}
		}()
	}

	for i := range batch {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	dur := time.Since(start)
	durMs := float64(dur.Microseconds()) / 1000.0
	rps := 0.0
	if dur > 0 {
		rps = float64(len(batch)) / dur.Seconds()
	}
	return sendStats{
		Target:     target,
		Sent:       len(batch),
		OK:         int(ok.Load()),
		Failed:     int(fail.Load()),
		DurationMs: durMs,
		RPS:        rps,
	}
}

func (f *faker) send(ctx context.Context, sr signedReq) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sr.target, strings.NewReader(string(sr.body)))
	if err != nil {
		return false
	}
	for k, v := range sr.headers {
		req.Header.Set(k, v)
	}
	resp, err := f.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}
