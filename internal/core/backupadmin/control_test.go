package backupadmin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recorded struct {
	method, path, auth, contentType, clientIP string
	body                                      map[string]string
}

func controlServer(t *testing.T, status int, resp string) (*httptest.Server, *[]recorded) {
	t.Helper()
	var reqs []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recorded{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"), clientIP: r.Header.Get("X-Elythia-Client-IP")}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &rec.body)
		}
		reqs = append(reqs, rec)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

// The bodies below follow the control API of #3460.
const (
	jobBody    = `{"job":{"kind":"verify","trigger":"api","generationId":"20261001T040000Z","startedAt":"2026-10-10T04:00:00Z"}}`
	statusBody = `{"now":"2026-10-10T05:00:00Z","schedule":{"interval":"24h","at":"04:00","timezone":"Asia/Tokyo","keep":7,"verify":true,"delayAfter":"36h"},` +
		`"running":{"kind":"take","trigger":"schedule","startedAt":"2026-10-10T04:59:00Z"},"pending":0,"nextRunAt":"2026-10-11T04:00:00Z",` +
		`"lastTake":{"kind":"take","trigger":"schedule","generationId":"20261009T190000Z","startedAt":"2026-10-09T19:00:00Z","finishedAt":"2026-10-09T19:03:00Z","ok":false,"stage":"prune","error":"denied","deleted":["20261001T040000Z"]},` +
		`"lastVerify":{"kind":"verify","trigger":"api","generationId":"20261009T190000Z","startedAt":"2026-10-09T19:03:00Z","finishedAt":"2026-10-09T19:05:00Z","ok":true,"verify":{"id":"20261009T190000Z","ok":true,"stages":[{"stage":"readable","ok":true}]}},` +
		`"latestUsable":{"id":"20261009T190000Z","at":"2026-10-09T19:00:00Z"},"overdue":true,"lastNotifyError":"webhook returned 500"}`
)

func TestHTTPControl_Requests(t *testing.T) {
	srv, reqs := controlServer(t, http.StatusAccepted, jobBody)
	c := NewHTTPControl(srv.URL+"/", "secret", nil)

	job, err := c.Take(context.Background(), "192.0.2.1")
	require.NoError(t, err)
	assert.Equal(t, "verify", job.Kind)
	job, err = c.Verify(context.Background(), gen1, "192.0.2.2")
	require.NoError(t, err)
	assert.Equal(t, gen1, job.GenerationID)
	assert.Equal(t, "api", job.Trigger)
	assert.Equal(t, time.Date(2026, 10, 10, 4, 0, 0, 0, time.UTC), job.StartedAt.UTC())

	require.Len(t, *reqs, 2)
	assert.Equal(t, recorded{method: "POST", path: "/take", auth: "Bearer secret", clientIP: "192.0.2.1"}, (*reqs)[0])
	assert.Equal(t, recorded{method: "POST", path: "/verify", auth: "Bearer secret", contentType: "application/json", clientIP: "192.0.2.2", body: map[string]string{"id": gen1}}, (*reqs)[1])
}

func TestHTTPControl_Status(t *testing.T) {
	srv, reqs := controlServer(t, http.StatusOK, statusBody)
	st, err := NewHTTPControl(srv.URL, "secret", nil).Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "GET", (*reqs)[0].method)
	assert.Equal(t, "/status", (*reqs)[0].path)
	assert.Empty(t, (*reqs)[0].clientIP)
	require.NotNil(t, st.Running)
	assert.Equal(t, "schedule", st.Running.Trigger)
	require.NotNil(t, st.NextRunAt)
	assert.Equal(t, time.Date(2026, 10, 11, 4, 0, 0, 0, time.UTC), st.NextRunAt.UTC())
	require.NotNil(t, st.LastTake)
	assert.False(t, st.LastTake.OK)
	assert.Equal(t, "prune", st.LastTake.Stage)
	assert.Equal(t, []string{gen1}, st.LastTake.Deleted)
	require.NotNil(t, st.LastVerify)
	require.NotNil(t, st.LastVerify.Verify)
	assert.True(t, st.LastVerify.Verify.OK)
	assert.Equal(t, "20261009T190000Z", st.LatestUsable.ID)
	assert.True(t, st.Overdue)
	assert.Equal(t, "webhook returned 500", st.LastNotifyError)
}

func TestHTTPControl_StatusIdle(t *testing.T) {
	srv, _ := controlServer(t, http.StatusOK, `{"now":"2026-10-10T05:00:00Z","schedule":null,"running":null,"pending":0,"nextRunAt":null,"lastTake":null,"lastVerify":null,"latestUsable":null,"overdue":false}`)
	st, err := NewHTTPControl(srv.URL, "s", nil).Status(context.Background())
	require.NoError(t, err)
	assert.Nil(t, st.Running)
	assert.Nil(t, st.NextRunAt)
	assert.Nil(t, st.LastTake)
}

func TestHTTPControl_NoToken(t *testing.T) {
	srv, reqs := controlServer(t, http.StatusAccepted, jobBody)
	_, err := NewHTTPControl(srv.URL, "", nil).Take(context.Background(), "")
	require.NoError(t, err)
	assert.Empty(t, (*reqs)[0].auth)
	assert.Empty(t, (*reqs)[0].clientIP)
}

func TestHTTPControl_Errors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"busy", http.StatusConflict, `{"error":"busy","running":{"kind":"take"}}`, ErrServiceBusy},
		{"no such generation", http.StatusNotFound, `{"error":"no_such_generation"}`, ErrNotFound},
		{"invalid id", http.StatusBadRequest, `{"error":"invalid_id"}`, ErrInvalidID},
		{"other 404", http.StatusNotFound, `not found`, ErrServiceFailed},
		{"unauthorized", http.StatusUnauthorized, `{"error":"unauthorized"}`, ErrServiceFailed},
		{"storage error", http.StatusBadGateway, `{"error":"storage_error"}`, ErrServiceFailed},
		{"not running", http.StatusServiceUnavailable, `{"error":"not_running"}`, ErrServiceFailed},
		{"bad job json", http.StatusAccepted, `nope`, ErrServiceFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := controlServer(t, tc.status, tc.body)
			_, err := NewHTTPControl(srv.URL, "s", nil).Verify(context.Background(), gen1, "")
			assert.ErrorIs(t, err, tc.want)
		})
	}
	t.Run("long body is truncated", func(t *testing.T) {
		srv, _ := controlServer(t, http.StatusInternalServerError, strings.Repeat("x", 500))
		_, err := NewHTTPControl(srv.URL, "s", nil).Take(context.Background(), "")
		assert.ErrorIs(t, err, ErrServiceFailed)
		assert.Contains(t, err.Error(), "500")
		assert.Less(t, len(err.Error()), 400)
	})
	t.Run("bad status json", func(t *testing.T) {
		srv, _ := controlServer(t, http.StatusOK, `nope`)
		_, err := NewHTTPControl(srv.URL, "s", nil).Status(context.Background())
		assert.ErrorIs(t, err, ErrServiceFailed)
	})
	t.Run("status error", func(t *testing.T) {
		srv, _ := controlServer(t, http.StatusUnauthorized, `{"error":"unauthorized"}`)
		_, err := NewHTTPControl(srv.URL, "s", nil).Status(context.Background())
		assert.ErrorIs(t, err, ErrServiceFailed)
	})
	t.Run("unreachable", func(t *testing.T) {
		_, err := NewHTTPControl("http://127.0.0.1:1", "s", nil).Take(context.Background(), "")
		assert.ErrorIs(t, err, ErrServiceFailed)
	})
	t.Run("bad url", func(t *testing.T) {
		_, err := NewHTTPControl("http://bad host", "s", nil).Take(context.Background(), "")
		assert.ErrorIs(t, err, ErrServiceFailed)
	})
	t.Run("body read error", func(t *testing.T) {
		_, err := NewHTTPControl("http://svc", "s", doerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(errReader{})}, nil
		})).Take(context.Background(), "")
		assert.ErrorIs(t, err, ErrServiceFailed)
	})
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("reset") }

func TestRedisTokens(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	tk := NewRedisTokens(rdb)
	ctx := context.Background()

	tok, err := tk.Issue(ctx, DownloadGrant{Key: "generations/x/dump.pgc", FileName: "f"}, time.Minute)
	require.NoError(t, err)
	assert.Len(t, tok, 64)
	g, err := tk.Resolve(ctx, tok)
	require.NoError(t, err)
	assert.Equal(t, "generations/x/dump.pgc", g.Key)
	// 期限内は何度でも使える (Range での再開)。
	_, err = tk.Resolve(ctx, tok)
	require.NoError(t, err)

	mr.FastForward(time.Minute + time.Second)
	_, err = tk.Resolve(ctx, tok)
	assert.ErrorIs(t, err, ErrTokenInvalid, "expired")

	for _, bad := range []string{"", "short", strings.Repeat("z", 64)} {
		_, err = tk.Resolve(ctx, bad)
		assert.ErrorIs(t, err, ErrTokenInvalid, bad)
	}
	other := strings.Repeat("a", 64)
	require.NoError(t, mr.Set(downloadTokenPrefix+other, "{"))
	_, err = tk.Resolve(ctx, other)
	assert.ErrorIs(t, err, ErrTokenInvalid)

	mr.SetError("down")
	_, err = tk.Resolve(ctx, other)
	assert.ErrorContains(t, err, "down")
	_, err = tk.Issue(ctx, DownloadGrant{Key: "k"}, time.Minute)
	assert.ErrorContains(t, err, "down")
}
