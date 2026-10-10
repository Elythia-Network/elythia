package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/safehttp"
)

// hookServer records the bodies posted to it.
type hookServer struct {
	*httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	types  []string
	status []int // responses in order; 204 after they run out
}

func newHookServer(t *testing.T, status ...int) *hookServer {
	h := &hookServer{status: status}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.bodies = append(h.bodies, b)
		h.types = append(h.types, r.Header.Get("Content-Type"))
		code := http.StatusNoContent
		if len(h.status) > 0 {
			code, h.status = h.status[0], h.status[1:]
		}
		h.mu.Unlock()
		if code == http.StatusFound {
			http.Redirect(w, r, "/elsewhere", code)
			return
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *hookServer) received() [][]byte {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]byte(nil), h.bodies...)
}

func testWebhook(t *testing.T, url, format string) *Webhook {
	t.Helper()
	w, err := NewWebhook(url, format, NewWebhookClient(http.DefaultTransport))
	require.NoError(t, err)
	w.retryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	return w
}

var mismatchEvent = Event{
	Kind: EventMismatch, Instance: "https://example.tld", OccurredAt: t0, GenerationID: backup.NewID(t0),
	Message:      "the backup did not pass verification",
	FailedStages: []backup.StageResult{{Stage: backup.StageRestorable, Error: "row counts differ"}},
	Mismatches:   []backup.RowMismatch{{Table: "public.note", Expected: 10, Actual: 9}},
}

func TestEventText(t *testing.T) {
	assert.Equal(t, "[Elythia backup] https://example.tld: verification found a broken backup (generation 20261001T040000Z): the backup did not pass verification\n- stage restorable: row counts differ\n- public.note: expected 10 rows, got 9", mismatchEvent.Text())
	assert.Equal(t, "[Elythia backup] i: take failed: boom", Event{Kind: EventFailure, Instance: "i", Stage: "take", Message: "boom"}.Text())
	assert.Equal(t, "[Elythia backup] i: no new backup: late", Event{Kind: EventDelay, Instance: "i", Message: "late"}.Text())
}

func TestWebhookGeneric(t *testing.T) {
	h := newHookServer(t)
	require.NoError(t, testWebhook(t, h.URL, "").Notify(context.Background(), mismatchEvent))
	got := h.received()
	require.Len(t, got, 1)
	assert.Equal(t, "application/json", h.types[0])
	var body map[string]any
	require.NoError(t, json.Unmarshal(got[0], &body))
	assert.Equal(t, "mismatch", body["event"])
	assert.Equal(t, "https://example.tld", body["instance"])
	assert.Equal(t, "20261001T040000Z", body["generationId"])
	assert.Equal(t, mismatchEvent.Text(), body["text"])
	assert.Equal(t, []any{map[string]any{"table": "public.note", "expected": float64(10), "actual": float64(9)}}, body["mismatches"])
	assert.NotContains(t, body, "lastUsableAt")
}

func TestWebhookDiscord(t *testing.T) {
	h := newHookServer(t)
	e := Event{Kind: EventFailure, Instance: "i", Stage: "take", Message: "@everyone " + strings.Repeat("あ", 3000)}
	require.NoError(t, testWebhook(t, h.URL, FormatDiscord).Notify(context.Background(), e))
	var body struct {
		Content         string         `json:"content"`
		AllowedMentions map[string]any `json:"allowed_mentions"`
	}
	require.NoError(t, json.Unmarshal(h.received()[0], &body))
	assert.Len(t, []rune(body.Content), discordContentLimit)
	assert.True(t, strings.HasSuffix(body.Content, "…"))
	assert.Equal(t, map[string]any{"parse": []any{}}, body.AllowedMentions, "mentions in the error text must not ping anyone")
}

func TestWebhookSlack(t *testing.T) {
	h := newHookServer(t)
	e := Event{Kind: EventFailure, Instance: "i", Stage: "take", Message: "<!channel> a & b > c"}
	require.NoError(t, testWebhook(t, h.URL, FormatSlack).Notify(context.Background(), e))
	var body map[string]string
	require.NoError(t, json.Unmarshal(h.received()[0], &body))
	assert.Equal(t, "[Elythia backup] i: take failed: &lt;!channel&gt; a &amp; b &gt; c", body["text"])

	long := Event{Kind: EventFailure, Message: strings.Repeat("x", 5000)}
	p, err := testWebhook(t, h.URL, FormatSlack).Payload(long)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(p, &body))
	assert.Len(t, []rune(body["text"]), slackTextLimit)
}

func TestWebhookRetries(t *testing.T) {
	h := newHookServer(t, http.StatusInternalServerError, http.StatusBadGateway)
	require.NoError(t, testWebhook(t, h.URL, "").Notify(context.Background(), mismatchEvent))
	assert.Len(t, h.received(), 3, "two failures, then success")

	h = newHookServer(t, 500, 500, 500, 500)
	err := testWebhook(t, h.URL, "").Notify(context.Background(), mismatchEvent)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 500")
	assert.Len(t, h.received(), 3, "gives up after three attempts")
}

func TestWebhookDoesNotFollowRedirects(t *testing.T) {
	h := newHookServer(t, http.StatusFound, http.StatusFound, http.StatusFound)
	err := testWebhook(t, h.URL, "").Notify(context.Background(), mismatchEvent)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 302")
	assert.Len(t, h.received(), 3, "only the configured URL is posted to")
}

func TestWebhookErrorDoesNotLeakURL(t *testing.T) {
	h := newHookServer(t)
	url := h.URL + "/api/webhooks/123/secret-token"
	h.Close()
	err := testWebhook(t, url, FormatDiscord).Notify(context.Background(), mismatchEvent)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret-token")
}

// failingTransport answers every request with 500 and counts them.
type failingTransport struct {
	mu sync.Mutex
	n  int
}

func (f *failingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.n++
	f.mu.Unlock()
	return &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

func (f *failingTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

func TestWebhookStopsRetryingOnCancel(t *testing.T) {
	// 1 回目の応答が届く前に cancel すると、送信そのものが context canceled で
	// 終わり、500 が誤りに残らない。応答を返してから数える transport で、cancel を
	// 必ず再送の待ちの間に入れる。
	ft := &failingTransport{}
	w, err := NewWebhook("https://hooks.example.tld/x", "", &http.Client{Transport: ft})
	require.NoError(t, err)
	w.retryDelays = []time.Duration{time.Hour, time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Notify(ctx, mismatchEvent) }()
	require.Eventually(t, func() bool { return ft.count() == 1 }, 5*time.Second, time.Millisecond)
	cancel()
	err = <-done
	require.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), "status 500")
	assert.Equal(t, 1, ft.count(), "no retry after cancel")
}

func TestNewWebhookValidates(t *testing.T) {
	for _, u := range []string{"", "ftp://x/", "https://", "::"} {
		_, err := NewWebhook(u, "", http.DefaultClient)
		assert.Error(t, err, u)
	}
	_, err := NewWebhook("https://example.tld/hook", "teams", http.DefaultClient)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "teams")
	w, err := NewWebhook("https://example.tld/hook", "", http.DefaultClient)
	require.NoError(t, err)
	assert.Equal(t, FormatGeneric, w.format)
}

func TestWebhookPostBadRequest(t *testing.T) {
	w := testWebhook(t, "https://example.tld/hook", "")
	w.url = "http://in valid/"
	require.Error(t, w.post(context.Background(), nil))
}

// 本番と同じ safehttp の transport では、loopback の通知先は
// allowedPrivateNetworks で許したときだけ届く。
func TestWebhookWithSafeTransport(t *testing.T) {
	h := newHookServer(t)
	blocked := testWebhook(t, h.URL, "")
	blocked.client = NewWebhookClient(safehttp.NewSSRFSafeTransport(nil))
	blocked.retryDelays = nil
	err := blocked.Notify(context.Background(), mismatchEvent)
	require.Error(t, err)
	assert.Empty(t, h.received())

	allowed := testWebhook(t, h.URL, "")
	allowed.client = NewWebhookClient(safehttp.NewSSRFSafeTransport([]string{"127.0.0.0/8", "::1/128"}))
	require.NoError(t, allowed.Notify(context.Background(), mismatchEvent))
	assert.Len(t, h.received(), 1)
}

func TestTruncateRunes(t *testing.T) {
	assert.Equal(t, "abc", truncateRunes("abc", 3))
	assert.Equal(t, "ab…", truncateRunes("abcd", 3))
}
