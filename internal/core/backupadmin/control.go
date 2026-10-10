package backupadmin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/backup"
)

// Errors returned by Control.
var (
	// ErrServiceNotConfigured: backup.server.serviceUrl is empty.
	ErrServiceNotConfigured = errors.New("backupadmin: backup service is not configured")
	// ErrServiceBusy: the backup service is already taking or verifying.
	ErrServiceBusy = errors.New("backupadmin: backup service is busy")
	// ErrServiceFailed: the backup service could not be reached or refused.
	ErrServiceFailed = errors.New("backupadmin: backup service request failed")
)

// Job is a take or verify the backup service is running or ran.
type Job struct {
	// Kind is "take" or "verify".
	Kind string `json:"kind"`
	// Trigger is "schedule" or "api".
	Trigger      string    `json:"trigger"`
	GenerationID string    `json:"generationId,omitempty"`
	StartedAt    time.Time `json:"startedAt"`
}

// JobResult is a finished Job.
type JobResult struct {
	Job
	FinishedAt time.Time `json:"finishedAt"`
	OK         bool      `json:"ok"`
	// Stage is where it failed: "take", "verify" or "prune".
	Stage string `json:"stage,omitempty"`
	Error string `json:"error,omitempty"`
	// Verify is the result of the verification, when one ran.
	Verify *backup.VerifyResult `json:"verify,omitempty"`
	// Deleted are the generations pruned after this job.
	Deleted []string `json:"deleted,omitempty"`
}

// UsableGeneration is the newest generation the service counts as usable.
type UsableGeneration struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
}

// Status is GET /status of the backup service (#3460). The state lives only
// in the service's memory and is lost when it restarts. Unknown fields are
// ignored so that the service can add more.
type Status struct {
	Now          time.Time         `json:"now"`
	Running      *Job              `json:"running"`
	NextRunAt    *time.Time        `json:"nextRunAt"`
	LastTake     *JobResult        `json:"lastTake"`
	LastVerify   *JobResult        `json:"lastVerify"`
	LatestUsable *UsableGeneration `json:"latestUsable"`
	// Overdue is true when no usable generation was made within delayAfter.
	Overdue         bool   `json:"overdue"`
	LastNotifyError string `json:"lastNotifyError,omitempty"`
}

// Control is the control API of `elythia backup daemon` (#3460).
//
// clientIP is the address of the administrator who asked. The service uses it
// for the per-client-range budget of passwordguard (#3463).
type Control interface {
	Take(ctx context.Context, clientIP string) (*Job, error)
	Verify(ctx context.Context, id, clientIP string) (*Job, error)
	Status(ctx context.Context) (*Status, error)
}

// Doer sends HTTP requests. *http.Client implements it.
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// HTTPControl calls the control API over HTTP.
//
// 形は #3460 の制御 API: `POST /take` → 202 `{"job": Job}`、`POST /verify`
// (body `{"id": "..."}`) → 202 `{"job": Job}`、`GET /status` → Status。認証は
// `Authorization: Bearer <backup.server.serviceToken>`、操作した人の接続元は
// `X-Elythia-Client-IP`。取る・確かめるは非同期で、結果は /status で見る。
// 実行中は 409 `busy`、世代が無ければ 404 `no_such_generation`、ID の形が違えば
// 400 `invalid_id`、止まりかけなら 503 `not_running`。
type HTTPControl struct {
	base  string
	token string
	doer  Doer
}

// controlTimeout bounds one control request. 受け付けるだけの API なので短くてよい。
const controlTimeout = 10 * time.Second

// controlBodyLimit caps how much of a response is read.
const controlBodyLimit = 1 << 20

// NewHTTPControl returns a Control for the service at baseURL. doer nil means
// an *http.Client with a short timeout.
//
// サービスの URL は運営者が設定ファイルに書く、同じ compose の中の宛先
// (プライベートアドレス) なので、連合用の SSRF 対策付きの client は使わない。
func NewHTTPControl(baseURL, token string, doer Doer) *HTTPControl {
	if doer == nil {
		doer = &http.Client{Timeout: controlTimeout, Transport: directTransport()}
	}
	return &HTTPControl{base: strings.TrimRight(baseURL, "/"), token: token, doer: doer}
}

// directTransport is http.DefaultTransport without a proxy.
//
// 制御 API は同じ compose の中の宛先なので、HTTP_PROXY / HTTPS_PROXY を通す理由が
// 無い。通すと、Authorization: Bearer の token が proxy に渡る。
func directTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	return t
}

// clientIPHeader carries the administrator's address to the service.
const clientIPHeader = "X-Elythia-Client-IP"

// jobResponse is the 202 body of POST /take and POST /verify.
type jobResponse struct {
	Job *Job `json:"job"`
}

// Take implements Control.
func (c *HTTPControl) Take(ctx context.Context, clientIP string) (*Job, error) {
	return c.job(ctx, "/take", nil, clientIP)
}

// Verify implements Control.
func (c *HTTPControl) Verify(ctx context.Context, id, clientIP string) (*Job, error) {
	return c.job(ctx, "/verify", map[string]string{"id": id}, clientIP)
}

func (c *HTTPControl) job(ctx context.Context, path string, payload any, clientIP string) (*Job, error) {
	body, err := c.do(ctx, http.MethodPost, path, payload, clientIP)
	if err != nil {
		return nil, err
	}
	var res jobResponse
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("%w: decode %s: %v", ErrServiceFailed, path, err)
	}
	return res.Job, nil
}

// Status implements Control.
func (c *HTTPControl) Status(ctx context.Context) (*Status, error) {
	body, err := c.do(ctx, http.MethodGet, "/status", nil, "")
	if err != nil {
		return nil, err
	}
	var st Status
	if err := json.Unmarshal(body, &st); err != nil {
		return nil, fmt.Errorf("%w: decode status: %v", ErrServiceFailed, err)
	}
	return &st, nil
}

// serviceError is the error body of the control API.
type serviceError struct {
	Error string `json:"error"`
}

func (c *HTTPControl) do(ctx context.Context, method, path string, payload any, clientIP string) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrServiceFailed, err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if clientIP != "" {
		req.Header.Set(clientIPHeader, clientIP)
	}
	res, err := c.doer.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrServiceFailed, err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, controlBodyLimit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read response: %v", ErrServiceFailed, err)
	}
	if len(data) > controlBodyLimit {
		return nil, fmt.Errorf("%w: %s %s: response is larger than %d bytes", ErrServiceFailed, method, path, controlBodyLimit)
	}
	if res.StatusCode >= 200 && res.StatusCode <= 299 {
		return data, nil
	}
	var se serviceError
	_ = json.Unmarshal(data, &se)
	switch {
	case res.StatusCode == http.StatusConflict:
		return nil, ErrServiceBusy
	case res.StatusCode == http.StatusNotFound && se.Error == "no_such_generation":
		return nil, ErrNotFound
	case res.StatusCode == http.StatusBadRequest && se.Error == "invalid_id":
		return nil, ErrInvalidID
	}
	// 応答の本文は運営者の調査用にログへ出すだけなので、長さを絞って載せる。
	msg := strings.TrimSpace(string(data))
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return nil, fmt.Errorf("%w: %s %s returned %d: %s", ErrServiceFailed, method, path, res.StatusCode, msg)
}
