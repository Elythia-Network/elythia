package server

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/misc/headreq"
	"github.com/elythia-network/elythia/internal/model"
)

// rawResponse is a response read off the wire, with whatever bytes the server
// sent after it.
type rawResponse struct {
	*http.Response
	trailing []byte
}

// doRaw sends one request with Connection: close over a raw TCP connection to
// the server at base and reads the response. For HEAD the response carries no
// body, so trailing holds every byte the server wrote after the header: a
// server that leaks the GET body into a HEAD response fails here, which an
// http.Client (it never reads a HEAD body) would not notice.
func doRaw(t *testing.T, base, method, path string, header http.Header) rawResponse {
	t.Helper()
	u, err := url.Parse(base)
	require.NoError(t, err)
	conn, err := net.Dial("tcp", u.Host)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(30*time.Second)))

	req, err := http.NewRequest(method, base+path, nil)
	require.NoError(t, err)
	for k, vs := range header {
		req.Header[k] = vs
	}
	req.Close = true
	require.NoError(t, req.Write(conn))

	br := bufio.NewReader(conn)
	res, err := http.ReadResponse(br, req)
	require.NoError(t, err)
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	_ = res.Body.Close()
	res.Body = io.NopCloser(bytes.NewReader(body))
	trailing, err := io.ReadAll(br)
	require.NoError(t, err)
	return rawResponse{Response: res, trailing: trailing}
}

// assertHEADLikeGET sends GET and HEAD for path to the server at base and
// checks that HEAD gets GET's status and the given headers, and no body bytes.
func assertHEADLikeGET(t *testing.T, base, path string, header http.Header, compare ...string) {
	t.Helper()
	get := doRaw(t, base, http.MethodGet, path, header)
	head := doRaw(t, base, http.MethodHead, path, header)
	getBody, _ := io.ReadAll(get.Body)
	headBody, _ := io.ReadAll(head.Body)

	assert.NotEqual(t, http.StatusMethodNotAllowed, head.StatusCode, "HEAD %s", path)
	assert.Equal(t, get.StatusCode, head.StatusCode, "HEAD %s status", path)
	assert.Empty(t, headBody, "HEAD %s body", path)
	assert.Empty(t, head.trailing, "HEAD %s: bytes after the header", path)
	for _, k := range compare {
		assert.Equal(t, get.Header.Values(k), head.Header.Values(k), "HEAD %s header %s", path, k)
	}
	if get.StatusCode == http.StatusOK && len(getBody) > 0 && get.Header.Get(echo.HeaderContentEncoding) == "" &&
		get.Header.Get(echo.HeaderContentLength) != "" {
		assert.Equal(t, strconv.Itoa(len(getBody)), head.Header.Get(echo.HeaderContentLength), "HEAD %s Content-Length", path)
	}
}

// headRoutesFixture is a bare Echo with headRoutes wired the way newServer
// wires it (hook and outermost middleware first, then the access log, then
// presentAsGET), so the routing rules can be checked route shape by route
// shape.
type headRoutesFixture struct {
	e   *echo.Echo
	log *lockedBuffer

	mu   sync.Mutex
	seen map[string]seenRequest
}

type seenRequest struct {
	method string
	head   bool
}

func newHeadRoutesFixture(t *testing.T) (*headRoutesFixture, string) {
	t.Helper()
	f := &headRoutesFixture{e: echo.New(), log: &lockedBuffer{}, seen: map[string]seenRequest{}}
	h := installHeadRoutes(f.e)
	f.e.Use(echomw.RequestLoggerWithConfig(accessLogConfig(f.log)))
	f.e.Use(h.presentAsGET)

	e := f.e
	e.GET("/page", f.handler("page", http.StatusOK, "<html>page</html>"))
	e.GET("/users/:id", f.handler("user", http.StatusOK, `{"id":"u"}`))
	// net/http の書き込みの buffer (4KiB) を超える本文。handler の途中で
	// 書き出されるので、元のリクエストが HEAD に戻っていないと本文が漏れる。
	e.GET("/large", f.handler("large", http.StatusOK, strings.Repeat("large body ", 10000)))
	e.GET("/missing", func(c echo.Context) error {
		f.record("missing", c)
		return echo.NewHTTPError(http.StatusNotFound, "no such thing")
	})
	// group の middleware は、group の下へ 404 用の catch-all を足す。HEAD は
	// これに当たって 404 になるので、GET のルートへ振り分けられることを見る。
	g := e.Group("/grp", func(next echo.HandlerFunc) echo.HandlerFunc { return next })
	g.GET("/item", f.handler("grp-item", http.StatusOK, "item"))
	// /api と同じ形。HEAD も持つ catch-all と、それより具体的な GET のルート。
	api := e.Group("/api", func(next echo.HandlerFunc) echo.HandlerFunc { return next })
	api.GET("/emojis", f.handler("api-emojis", http.StatusOK, `{"emojis":[]}`))
	api.Any("/*", f.handler("api-catchall", http.StatusOK, "{}"))
	// HEAD を明示したルートは、そのまま HEAD として handler に届く。
	e.Any("/any/*", f.handler("any", http.StatusOK, "any"))
	e.GET("/*", f.handler("spa", http.StatusOK, "<html>spa</html>"))
	e.HEAD("/probe", f.handler("probe-head", http.StatusNoContent, ""))
	e.GET("/blob", func(c echo.Context) error {
		f.record("blob", c)
		c.Response().Header().Set(echo.HeaderContentType, "application/octet-stream")
		http.ServeContent(c.Response(), headreq.Request(c), "blob", time.Time{}, newCountingSeeker(blobPayload))
		return nil
	})

	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *headRoutesFixture) record(name string, c echo.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen[name] = seenRequest{method: c.Request().Method, head: headreq.Is(c)}
}

func (f *headRoutesFixture) last(name string) (seenRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.seen[name]
	return s, ok
}

func (f *headRoutesFixture) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = map[string]seenRequest{}
}

func (f *headRoutesFixture) handler(name string, status int, body string) echo.HandlerFunc {
	return func(c echo.Context) error {
		f.record(name, c)
		c.Response().Header().Set("X-Route", name)
		if body == "" {
			return c.NoContent(status)
		}
		return c.Blob(status, echo.MIMETextHTMLCharsetUTF8, []byte(body))
	}
}

var blobPayload = bytes.Repeat([]byte("0123456789"), 10000)

// countingSeeker is an io.ReadSeeker over data that counts the bytes read.
type countingSeeker struct {
	*bytes.Reader
	read *int64
	mu   *sync.Mutex
}

var blobRead struct {
	sync.Mutex
	n int64
}

func newCountingSeeker(data []byte) io.ReadSeeker {
	return &countingSeeker{Reader: bytes.NewReader(data), read: &blobRead.n, mu: &blobRead.Mutex}
}

func (s *countingSeeker) Read(p []byte) (int, error) {
	n, err := s.Reader.Read(p)
	s.mu.Lock()
	*s.read += int64(n)
	s.mu.Unlock()
	return n, err
}

func blobBytesRead() int64 {
	blobRead.Lock()
	defer blobRead.Unlock()
	return blobRead.n
}

func resetBlobBytesRead() {
	blobRead.Lock()
	defer blobRead.Unlock()
	blobRead.n = 0
}

func TestHeadRoutes_GETRouteAnswersHEADLikeGET(t *testing.T) {
	f, base := newHeadRoutesFixture(t)

	for _, path := range []string{"/page", "/users/abc", "/large", "/grp/item", "/api/emojis", "/somewhere/else", "/"} {
		t.Run(path, func(t *testing.T) {
			assertHEADLikeGET(t, base, path, nil, echo.HeaderContentType, "X-Route", echo.HeaderContentLength)
		})
	}

	t.Run("the handler sees GET, and headreq.Is tells it the client sent HEAD", func(t *testing.T) {
		f.reset()
		res := doRaw(t, base, http.MethodHead, "/page", nil)
		require.Equal(t, http.StatusOK, res.StatusCode)
		got, ok := f.last("page")
		require.True(t, ok)
		assert.Equal(t, seenRequest{method: http.MethodGet, head: true}, got)

		f.reset()
		doRaw(t, base, http.MethodGet, "/page", nil)
		got, _ = f.last("page")
		assert.Equal(t, seenRequest{method: http.MethodGet, head: false}, got)
	})

	t.Run("a GET route more specific than a catch-all that has HEAD wins", func(t *testing.T) {
		f.reset()
		res := doRaw(t, base, http.MethodHead, "/api/emojis", nil)
		assert.Equal(t, "api-emojis", res.Header.Get("X-Route"))
		_, hitCatchAll := f.last("api-catchall")
		assert.False(t, hitCatchAll)
	})
}

func TestHeadRoutes_ErrorStatusIsKeptWithoutBody(t *testing.T) {
	_, base := newHeadRoutesFixture(t)
	assertHEADLikeGET(t, base, "/missing", nil, echo.HeaderContentType)
	res := doRaw(t, base, http.MethodHead, "/missing", nil)
	assert.Equal(t, http.StatusNotFound, res.StatusCode)
}

func TestHeadRoutes_ExplicitHEADRoutesAreNotRewritten(t *testing.T) {
	f, base := newHeadRoutesFixture(t)

	t.Run("catch-all registered with Any", func(t *testing.T) {
		f.reset()
		res := doRaw(t, base, http.MethodHead, "/api/unknown", nil)
		assert.Equal(t, "api-catchall", res.Header.Get("X-Route"))
		got, ok := f.last("api-catchall")
		require.True(t, ok)
		assert.Equal(t, seenRequest{method: http.MethodHead, head: true}, got)
	})

	t.Run("route registered with Any", func(t *testing.T) {
		f.reset()
		res := doRaw(t, base, http.MethodHead, "/any/x", nil)
		assert.Equal(t, "any", res.Header.Get("X-Route"))
		got, _ := f.last("any")
		assert.Equal(t, http.MethodHead, got.method)
	})

	t.Run("HEAD-only route under a GET catch-all", func(t *testing.T) {
		res := doRaw(t, base, http.MethodHead, "/probe", nil)
		assert.Equal(t, http.StatusNoContent, res.StatusCode)
		assert.Equal(t, "probe-head", res.Header.Get("X-Route"))
	})
}

// GET を持たないルートの HEAD は、今までどおり Echo が答える (405 / 404)。
func TestHeadRoutes_RoutesWithoutGETAreUnchanged(t *testing.T) {
	e := echo.New()
	h := installHeadRoutes(e)
	e.Use(h.presentAsGET)
	ok := func(c echo.Context) error { return c.String(http.StatusOK, "x") }
	e.POST("/post-only", ok)
	g := e.Group("/grp", func(next echo.HandlerFunc) echo.HandlerFunc { return next })
	g.POST("/post-only", ok)
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)

	for path, want := range map[string]int{
		"/post-only":     http.StatusMethodNotAllowed,
		"/grp/post-only": http.StatusNotFound,
		"/nothing":       http.StatusNotFound,
	} {
		res := doRaw(t, srv.URL, http.MethodHead, path, nil)
		assert.Equal(t, want, res.StatusCode, path)
		assert.Empty(t, res.trailing, path)
	}
}

func TestHeadRoutes_AccessLogRecordsHEAD(t *testing.T) {
	f, base := newHeadRoutesFixture(t)

	doRaw(t, base, http.MethodHead, "/page", nil)
	doRaw(t, base, http.MethodHead, "/missing", nil)

	log := f.log.String()
	assert.Contains(t, log, " HEAD /page 200 ")
	assert.Contains(t, log, " HEAD /missing 404 ")
	assert.NotContains(t, log, " GET ")
}

// http.ServeContent に HEAD を渡し直すので、本文を読まずに済む。
func TestHeadRoutes_HeadAwareRequestSkipsTheBodyCopy(t *testing.T) {
	_, base := newHeadRoutesFixture(t)

	resetBlobBytesRead()
	head := doRaw(t, base, http.MethodHead, "/blob", nil)
	assert.Equal(t, http.StatusOK, head.StatusCode)
	assert.Equal(t, strconv.Itoa(len(blobPayload)), head.Header.Get(echo.HeaderContentLength))
	assert.Empty(t, head.trailing)
	assert.Zero(t, blobBytesRead())

	resetBlobBytesRead()
	get := doRaw(t, base, http.MethodGet, "/blob", nil)
	assert.Equal(t, http.StatusOK, get.StatusCode)
	assert.Equal(t, int64(len(blobPayload)), blobBytesRead())
}

// **`/files` の HEAD は、ルーターを通しても先頭だけを読む分岐に届く (#3925)。**
// 以前は GET だけのルートだったので、本番では HEAD が 405 になり、この分岐は
// handler を直接呼ぶテストでしか通っていなかった。
func TestHeadRoutes_FilesHEADUsesTheSniffOnlyBranch(t *testing.T) {
	const size = 1 << 20
	f := newRangedFixture(t, "k", pngObject(size))
	e := echo.New()
	h := installHeadRoutes(e)
	e.Use(h.presentAsGET)
	e.GET("/files/:accessKey", f.handler)
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)

	res := doRaw(t, srv.URL, http.MethodHead, "/files/k", nil)

	assert.Equal(t, http.StatusOK, res.StatusCode)
	assert.Equal(t, strconv.Itoa(size), res.Header.Get(echo.HeaderContentLength))
	assert.Equal(t, "image/png", res.Header.Get(echo.HeaderContentType))
	assert.Empty(t, res.trailing)
	assert.Equal(t, []string{sniffRangeSpec}, f.s3.ranges)
	assert.LessOrEqual(t, f.s3.read.Load(), int64(coredrive.MIMESniffLen))
}

// Echo が HEAD を GET へ広げないことを前提にしている。Echo が広げるように
// なったら、この仕組みは要らなくなるので、気づけるようにする。
func TestHeadRoutes_EchoAloneAnswersHEADWith405(t *testing.T) {
	e := echo.New()
	e.GET("/page", func(c echo.Context) error { return c.String(http.StatusOK, "page") })
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/page", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.True(t, strings.Contains(rec.Header().Get(echo.HeaderAllow), http.MethodGet), fmt.Sprint(rec.Header()))
}

// Upgrade を求める HEAD は振り分けない。handler には GET に見えるので、
// 振り分けると /streaming が WebSocket へ切り替えてしまう (#3925)。
func TestHeadRoutes_UpgradeHEADIsNotRouted(t *testing.T) {
	for name, upgrade := range map[string][]string{
		"websocket": {"websocket"},
		// Header.Get は最初の値しか見ない。
		"empty value first": {"", "websocket"},
	} {
		t.Run(name, func(t *testing.T) {
			f, base := newHeadRoutesFixture(t)

			res := doRaw(t, base, http.MethodHead, "/page", http.Header{"Connection": {"Upgrade"}, "Upgrade": upgrade})

			assert.Equal(t, http.StatusMethodNotAllowed, res.StatusCode)
			_, called := f.last("page")
			assert.False(t, called, "the GET handler is not run")
		})
	}
}

// seekStorage is a coredrive.Storage whose Get returns a seekable body that
// counts the bytes read, the shape of the local storage's *os.File.
type seekStorage struct {
	memStorage
	data []byte
}

type nopSeekCloser struct{ io.ReadSeeker }

func (nopSeekCloser) Close() error { return nil }

func (s *seekStorage) Get(string) (io.ReadCloser, error) {
	return nopSeekCloser{newCountingSeeker(s.data)}, nil
}

// `/files` のローカルの経路 (seek できる本文) も、HEAD では先頭だけを読む。
func TestHeadRoutes_FilesLocalHEADSkipsTheBodyCopy(t *testing.T) {
	data := pngObject(1 << 20)
	lookup := &stubFilesLookup{byKey: map[string]*model.DriveFile{"k": {ID: "f1", StoredInternal: true}}}
	e := echo.New()
	h := installHeadRoutes(e)
	e.Use(h.presentAsGET)
	e.GET("/files/:accessKey", filesHandler(lookup, &memStorage{byKey: map[string]string{}}, &seekStorage{data: data}))
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)

	resetBlobBytesRead()
	res := doRaw(t, srv.URL, http.MethodHead, "/files/k", nil)

	assert.Equal(t, http.StatusOK, res.StatusCode)
	assert.Equal(t, strconv.Itoa(len(data)), res.Header.Get(echo.HeaderContentLength))
	assert.Equal(t, "image/png", res.Header.Get(echo.HeaderContentType))
	assert.Empty(t, res.trailing)
	assert.LessOrEqual(t, blobBytesRead(), int64(coredrive.MIMESniffLen))
}
