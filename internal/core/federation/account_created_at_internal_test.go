package federation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

func TestParseAccountPublished(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		raw  string
		want *time.Time
	}{
		{"mastodon date at midnight", "2017-04-08T00:00:00Z", ptrTime(time.Date(2017, 4, 8, 0, 0, 0, 0, time.UTC))},
		{"fractional seconds", "2023-01-02T03:04:05.678Z", ptrTime(time.Date(2023, 1, 2, 3, 4, 5, 678000000, time.UTC))},
		{"offset is normalized to UTC", "2023-01-02T09:00:00+09:00", ptrTime(time.Date(2023, 1, 2, 0, 0, 0, 0, time.UTC))},
		{"date only", "2019-12-31", ptrTime(time.Date(2019, 12, 31, 0, 0, 0, 0, time.UTC))},
		{"surrounding spaces", " 2019-12-31 ", ptrTime(time.Date(2019, 12, 31, 0, 0, 0, 0, time.UTC))},
		{"within clock skew", "2026-10-08T12:04:00Z", ptrTime(time.Date(2026, 10, 8, 12, 4, 0, 0, time.UTC))},
		{"empty", "", nil},
		{"unparsable", "yesterday", nil},
		{"no timezone", "2023-01-02T03:04:05", nil},
		{"epoch zero", "1970-01-01T00:00:00Z", nil},
		{"epoch seconds read as milliseconds", "1970-01-18T08:40:00Z", nil},
		{"just before floor", "1999-12-31T23:59:59Z", nil},
		{"at floor", "2000-01-01T00:00:00Z", ptrTime(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))},
		{"before epoch", "1969-12-31T00:00:00Z", nil},
		{"go zero time", "0001-01-01T00:00:00Z", nil},
		{"future beyond skew", "2026-10-08T12:06:00Z", nil},
		{"far future", "2099-01-01", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseAccountPublished(tc.raw, now)
			if tc.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.True(t, tc.want.Equal(*got), "want %s, got %s", tc.want, got)
			assert.Equal(t, time.UTC, got.Location())
		})
	}
}

func TestIsMisskeyFamilySoftware(t *testing.T) {
	for _, name := range []string{"misskey", "Misskey", " sharkey ", "cherrypick", "elythia", "mk-go", "firefish", "iceshrimp"} {
		assert.Truef(t, IsMisskeyFamilySoftware(name), "%q should be Misskey-family", name)
	}
	for _, name := range []string{"", "mastodon", "pleroma", "akkoma", "iceshrimp.net", "gotosocial"} {
		assert.Falsef(t, IsMisskeyFamilySoftware(name), "%q should not be Misskey-family", name)
	}
}

func newCreatedAtServer(t *testing.T, body string, status int) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.URL.Path != "/api/users/show" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestRemoteStatsFetcher_FetchAccountCreatedAt_Success(t *testing.T) {
	srv, hits := newCreatedAtServer(t, `{"username":"Alice","createdAt":"2019-05-06T07:08:09.123Z","notesCount":1}`, http.StatusOK)
	f := newRemoteStatsFetcherWithTransport(redirectTransport{target: srv.URL})

	got := f.FetchAccountCreatedAt(context.Background(), "remote.example", "alice")
	require.NotNil(t, got)
	assert.True(t, time.Date(2019, 5, 6, 7, 8, 9, 123000000, time.UTC).Equal(*got))
	assert.Equal(t, int32(1), atomic.LoadInt32(hits))

	// 2 回目はキャッシュから返り、相手へは出さない。
	again := f.FetchAccountCreatedAt(context.Background(), "remote.example", "alice")
	require.NotNil(t, again)
	assert.Equal(t, int32(1), atomic.LoadInt32(hits))

	// 期限切れのキャッシュは取り直す。
	f.createdCache.Add("remote.example|alice", cachedAccountCreatedAt{fetched: time.Now().Add(-2 * remoteStatsTTL), ttl: remoteStatsTTL})
	require.NotNil(t, f.FetchAccountCreatedAt(context.Background(), "remote.example", "alice"))
	assert.Equal(t, int32(2), atomic.LoadInt32(hits))
}

// 同じホストの別の人は別々に引く (キャッシュのキーに username を含める)。
func TestRemoteStatsFetcher_FetchAccountCreatedAt_CachesPerUser(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		var req struct {
			Username string `json:"username"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		created := map[string]string{"alice": "2019-01-01T00:00:00.000Z", "bob": "2021-06-07T00:00:00.000Z"}[req.Username]
		_, _ = w.Write([]byte(`{"username":"` + req.Username + `","createdAt":"` + created + `"}`))
	}))
	t.Cleanup(srv.Close)
	f := newRemoteStatsFetcherWithTransport(redirectTransport{target: srv.URL})

	for range 2 {
		alice := f.FetchAccountCreatedAt(context.Background(), "remote.example", "alice")
		bob := f.FetchAccountCreatedAt(context.Background(), "remote.example", "bob")
		require.NotNil(t, alice)
		require.NotNil(t, bob)
		assert.True(t, time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC).Equal(*alice))
		assert.True(t, time.Date(2021, 6, 7, 0, 0, 0, 0, time.UTC).Equal(*bob), "別の人の作成日時を返さないこと")
	}
	assert.Equal(t, int32(2), atomic.LoadInt32(&hits), "2 回目はそれぞれキャッシュから返る")
}

func TestRemoteStatsFetcher_FetchAccountCreatedAt_FailuresAreNegativelyCached(t *testing.T) {
	srv, hits := newCreatedAtServer(t, `{}`, http.StatusInternalServerError)
	f := newRemoteStatsFetcherWithTransport(redirectTransport{target: srv.URL})

	assert.Nil(t, f.FetchAccountCreatedAt(context.Background(), "remote.example", "alice"))
	assert.Nil(t, f.FetchAccountCreatedAt(context.Background(), "remote.example", "alice"))
	assert.Equal(t, int32(1), atomic.LoadInt32(hits), "失敗も負キャッシュに入り、続けて取りに行かない")
	entry, ok := f.createdCache.Get("remote.example|alice")
	require.True(t, ok)
	assert.Nil(t, entry.createdAt)
	assert.Equal(t, remoteStatsNegativeTTL, entry.ttl)
	// Mastodon の API へは fallback しない (users/show の 1 回だけ)。
}

func TestRemoteStatsFetcher_FetchAccountCreatedAt_RejectsBadPayloads(t *testing.T) {
	cases := map[string]string{
		"other user":        `{"username":"bob","createdAt":"2019-05-06T07:08:09.123Z"}`,
		"missing createdAt": `{"username":"alice"}`,
		"bad createdAt":     `{"username":"alice","createdAt":"long ago"}`,
		"malformed json":    `{"username":`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv, hits := newCreatedAtServer(t, body, http.StatusOK)
			f := newRemoteStatsFetcherWithTransport(redirectTransport{target: srv.URL})
			assert.Nil(t, f.FetchAccountCreatedAt(context.Background(), "remote.example", "alice"))
			assert.Equal(t, int32(1), atomic.LoadInt32(hits))
		})
	}
}

func TestRemoteStatsFetcher_FetchAccountCreatedAt_Guards(t *testing.T) {
	srv, hits := newCreatedAtServer(t, `{"username":"alice","createdAt":"2019-05-06T07:08:09.123Z"}`, http.StatusOK)
	f := newRemoteStatsFetcherWithTransport(redirectTransport{target: srv.URL})
	f.SetHostAllowedChecker(func(host string) bool { return host != "blocked.example" })

	var nilFetcher *RemoteStatsFetcher
	assert.Nil(t, nilFetcher.FetchAccountCreatedAt(context.Background(), "remote.example", "alice"))
	assert.Nil(t, f.FetchAccountCreatedAt(context.Background(), "", "alice"))
	assert.Nil(t, f.FetchAccountCreatedAt(context.Background(), "remote.example", ""))
	assert.Nil(t, f.FetchAccountCreatedAt(context.Background(), "evil.example/x?", "alice"))
	assert.Nil(t, f.FetchAccountCreatedAt(context.Background(), "blocked.example", "alice"),
		"連合を切った相手へは取りに行かない")
	assert.Equal(t, int32(0), atomic.LoadInt32(hits))

	// 許可された相手には取りに行く (「常に nil」の実装で緑にならないように)。
	assert.NotNil(t, f.FetchAccountCreatedAt(context.Background(), "ok.example", "alice"))
	assert.Equal(t, int32(1), atomic.LoadInt32(hits))
}

// --- AccountCreatedAtFiller ---

type fakeCreatedAtSource struct {
	result *time.Time
	calls  []string
}

func (s *fakeCreatedAtSource) FetchAccountCreatedAt(ctx context.Context, host, username string) *time.Time {
	s.calls = append(s.calls, host+"|"+username)
	if _, ok := ctx.Deadline(); !ok {
		panic("FetchAccountCreatedAt must be called with a deadline")
	}
	return s.result
}

type fakeInstanceLookup struct {
	software map[string]string
	err      error
}

func (l *fakeInstanceLookup) FindByHost(host string) (*model.Instance, error) {
	if l.err != nil {
		return nil, l.err
	}
	name, ok := l.software[host]
	if !ok {
		return nil, errors.New("not found")
	}
	if name == "" {
		return &model.Instance{Host: host}, nil
	}
	return &model.Instance{Host: host, SoftwareName: &name}, nil
}

type fakeCreatedAtWriter struct {
	err      error
	occupied bool
	updates  []time.Time
}

func (w *fakeCreatedAtWriter) SetAccountCreatedAtIfNull(_ string, createdAt time.Time) (bool, error) {
	w.updates = append(w.updates, createdAt)
	if w.err != nil {
		return false, w.err
	}
	return !w.occupied, nil
}

func remoteUser(host string) *model.User {
	return &model.User{ID: "u1", Username: "alice", Host: &host}
}

func newTestFiller(source *fakeCreatedAtSource, writer *fakeCreatedAtWriter) *AccountCreatedAtFiller {
	f := NewAccountCreatedAtFiller(source, &fakeInstanceLookup{software: map[string]string{
		"misskey.example":  "misskey",
		"mastodon.example": "mastodon",
		"unknown.example":  "",
	}}, writer)
	f.now = func() time.Time { return time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC) }
	return f
}

func TestAccountCreatedAtFiller_StoresForMisskeyFamily(t *testing.T) {
	created := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	source := &fakeCreatedAtSource{result: &created}
	writer := &fakeCreatedAtWriter{}
	u := remoteUser("misskey.example")

	newTestFiller(source, writer).Fill(context.Background(), u)

	assert.Equal(t, []string{"misskey.example|alice"}, source.calls)
	require.Len(t, writer.updates, 1)
	assert.True(t, created.Equal(writer.updates[0]))
	require.NotNil(t, u.AccountCreatedAt)
	assert.True(t, created.Equal(*u.AccountCreatedAt))
}

func TestAccountCreatedAtFiller_SkipsWhenNotNeeded(t *testing.T) {
	known := time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := map[string]*model.User{
		"nil user":        nil,
		"local user":      {ID: "l1", Username: "local"},
		"already known":   {ID: "u1", Username: "alice", Host: ptrString("misskey.example"), AccountCreatedAt: &known},
		"not misskey":     remoteUser("mastodon.example"),
		"no softwareName": remoteUser("unknown.example"),
		"unknown host":    remoteUser("nowhere.example"),
	}
	for name, u := range cases {
		t.Run(name, func(t *testing.T) {
			created := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
			source := &fakeCreatedAtSource{result: &created}
			writer := &fakeCreatedAtWriter{}
			newTestFiller(source, writer).Fill(context.Background(), u)
			assert.Empty(t, source.calls, "取りに行かないこと")
			assert.Empty(t, writer.updates)
		})
	}
}

func TestAccountCreatedAtFiller_FailuresLeaveColumnNull(t *testing.T) {
	future := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	created := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	t.Run("fetch failed", func(t *testing.T) {
		source := &fakeCreatedAtSource{}
		writer := &fakeCreatedAtWriter{}
		u := remoteUser("misskey.example")
		newTestFiller(source, writer).Fill(context.Background(), u)
		assert.Len(t, source.calls, 1)
		assert.Empty(t, writer.updates)
		assert.Nil(t, u.AccountCreatedAt)
	})
	t.Run("future value", func(t *testing.T) {
		source := &fakeCreatedAtSource{result: &future}
		writer := &fakeCreatedAtWriter{}
		u := remoteUser("misskey.example")
		newTestFiller(source, writer).Fill(context.Background(), u)
		assert.Empty(t, writer.updates, "未来の作成日時は保存しない")
		assert.Nil(t, u.AccountCreatedAt)
	})
	t.Run("store failed", func(t *testing.T) {
		source := &fakeCreatedAtSource{result: &created}
		writer := &fakeCreatedAtWriter{err: errors.New("db down")}
		u := remoteUser("misskey.example")
		newTestFiller(source, writer).Fill(context.Background(), u)
		assert.Len(t, writer.updates, 1)
		assert.Nil(t, u.AccountCreatedAt, "保存できなかった値をメモリにだけ残さない")
	})
	t.Run("stored by another path meanwhile", func(t *testing.T) {
		source := &fakeCreatedAtSource{result: &created}
		writer := &fakeCreatedAtWriter{occupied: true}
		u := remoteUser("misskey.example")
		newTestFiller(source, writer).Fill(context.Background(), u)
		assert.Len(t, writer.updates, 1)
		assert.Nil(t, u.AccountCreatedAt, "書かなかった値をメモリにだけ残さない")
	})
	t.Run("instance lookup failed", func(t *testing.T) {
		source := &fakeCreatedAtSource{result: &created}
		writer := &fakeCreatedAtWriter{}
		f := NewAccountCreatedAtFiller(source, &fakeInstanceLookup{err: errors.New("db down")}, writer)
		f.Fill(context.Background(), remoteUser("misskey.example"))
		assert.Empty(t, source.calls)
	})
}

func TestAccountCreatedAtFiller_NilDependenciesAreNoop(t *testing.T) {
	created := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	source := &fakeCreatedAtSource{result: &created}
	var nilFiller *AccountCreatedAtFiller
	nilFiller.Fill(context.Background(), remoteUser("misskey.example"))
	NewAccountCreatedAtFiller(nil, &fakeInstanceLookup{}, &fakeCreatedAtWriter{}).Fill(context.Background(), remoteUser("misskey.example"))
	NewAccountCreatedAtFiller(source, nil, &fakeCreatedAtWriter{}).Fill(context.Background(), remoteUser("misskey.example"))
	NewAccountCreatedAtFiller(source, &fakeInstanceLookup{software: map[string]string{"misskey.example": "misskey"}}, nil).Fill(context.Background(), remoteUser("misskey.example"))
	assert.Empty(t, source.calls)
}

func ptrTime(t time.Time) *time.Time { return &t }

func ptrString(s string) *string { return &s }

// 応答しない (接続失敗・時間切れ) ホストへは、5 分のあいだ誰の分も取りに
// 行かない。人ごとの負キャッシュだけだと、Misskey を名乗る相手が知られた人の
// 数だけフォローの処理を待たせられる (#3465)。
func TestRemoteStatsFetcher_FetchAccountCreatedAt_UnreachableHostIsSkipped(t *testing.T) {
	type behavior func(w http.ResponseWriter, r *http.Request)
	dropConn := func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}
	// 本文を読み切らないとサーバー側は切断に気付かない (r.Context() が終わら
	// ない) ので、読んでから止まる。テストの終わりには release で必ず返す。
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	wait := func(r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
	stall := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		wait(r)
	}
	stallBody := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"username":`))
		w.(http.Flusher).Flush()
		wait(r)
	}
	for name, handler := range map[string]behavior{"connection dropped": dropConn, "no response": stall, "body stalls": stallBody} {
		t.Run(name, func(t *testing.T) {
			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				handler(w, r)
			}))
			t.Cleanup(srv.Close)
			f := newRemoteStatsFetcherWithTransport(redirectTransport{target: srv.URL})
			fetch := func(username string) *time.Time {
				ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				defer cancel()
				return f.FetchAccountCreatedAt(ctx, "hostile.example", username)
			}

			assert.Nil(t, fetch("alice"))
			assert.Equal(t, int32(1), atomic.LoadInt32(&hits))
			assert.Nil(t, fetch("bob"), "同じホストの別の人も取りに行かない")
			assert.Equal(t, int32(1), atomic.LoadInt32(&hits), "応答しなかったホストへ続けて出している")

			// 期間が過ぎたら、また取りに行く。
			f.createdHostDown.Add("hostile.example", time.Now().Add(-remoteCreatedAtHostDownTTL-time.Second))
			assert.Nil(t, fetch("bob"))
			assert.Equal(t, int32(2), atomic.LoadInt32(&hits))
		})
	}
}

// 404 や別人の応答は「応答しない」ではないので、同じホストの別の人は取りに行く。
func TestRemoteStatsFetcher_FetchAccountCreatedAt_HTTPErrorsDoNotBlockHost(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		var req struct {
			Username string `json:"username"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Username {
		case "gone":
			w.WriteHeader(http.StatusNotFound)
		case "other":
			_, _ = w.Write([]byte(`{"username":"someone","createdAt":"2019-01-01T00:00:00.000Z"}`))
		default:
			_, _ = w.Write([]byte(`{"username":"` + req.Username + `","createdAt":"2019-01-01T00:00:00.000Z"}`))
		}
	}))
	t.Cleanup(srv.Close)
	f := newRemoteStatsFetcherWithTransport(redirectTransport{target: srv.URL})

	assert.Nil(t, f.FetchAccountCreatedAt(context.Background(), "remote.example", "gone"))
	assert.Nil(t, f.FetchAccountCreatedAt(context.Background(), "remote.example", "other"))
	assert.NotNil(t, f.FetchAccountCreatedAt(context.Background(), "remote.example", "alice"))
	assert.Equal(t, int32(3), atomic.LoadInt32(&hits))
}
