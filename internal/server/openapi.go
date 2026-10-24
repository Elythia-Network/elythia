package server

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/elythia-network/elythia/internal/api/openapi"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// OpenAPISpec serves /api.json.
//
// 本家のgen-spec.tsと同じ形で、本家互換のエンドポイントには引数・応答・エラーの
// 例を付ける(internal/api/openapi、#3810)。載せるのはEchoに登録したルートに
// あるエンドポイントとメソッドだけにする。本家にあってもElythiaが持たない
// ものは載らず、Elythia独自のものは型を付けずに別のタグで一覧だけ載せる。
//
// ルートは組み立ての後に変わらないので、文書は初回に1度だけ作る。本文は約1.3MB
// あり、全体のgzip middlewareに任せるとリクエストのたびに圧縮し直す(認証なしで
// 叩ける)。gzipした本文も1度だけ作り、ここでContent-Encodingを決める
// (selfEncodedRoutes。/api/emojisと同じ書き方)。
func (s *Server) OpenAPISpec(c echo.Context) error {
	spec, err := s.apiSpec.get(s.buildOpenAPISpec)
	if err != nil {
		slog.Error("failed to build /api.json", "error", err)
		return c.JSON(http.StatusInternalServerError, map[string]any{
			"error": map[string]any{
				"message": "Internal error occurred. Please contact us if the error persists.",
				"code":    "INTERNAL_ERROR",
				"id":      "5d37dbcb-891e-41ca-a3d6-e690c97775ac",
			},
		})
	}
	res := c.Response()
	// 符号化を選ばなかった応答(Accept-Encodingが無い、q=0)も、同じURLで
	// 符号化が変わるので常にVaryを付ける。
	res.Header().Add(echo.HeaderVary, echo.HeaderAcceptEncoding)
	res.Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	res.Header().Set(echo.HeaderCacheControl, "public, max-age=600")
	payload := spec.plain
	if middleware.AcceptsEncoding(c.Request().Header.Get(echo.HeaderAcceptEncoding), "gzip") {
		res.Header().Set(echo.HeaderContentEncoding, "gzip")
		payload = spec.gzip
	}
	res.Header().Set(echo.HeaderContentLength, strconv.Itoa(len(payload)))
	res.WriteHeader(http.StatusOK)
	_, err = res.Write(payload)
	return err
}

// encodedSpec is the /api.json body, plain and gzip-compressed.
type encodedSpec struct {
	plain []byte
	gzip  []byte
}

// apiSpecCache holds /api.json once it has been built.
//
// **本文だけを持つ。** 組み立てに使ったopenapi.Generatorは十数MBあるので、
// 本文を作ったら捨てる(#3810)。
type apiSpecCache struct {
	once sync.Once
	spec *encodedSpec
	err  error
}

// get returns the cached document, building it with build on the first call.
// A panic in build, or a build that returns neither a document nor an error,
// is kept as the error.
//
// sync.Onceはpanicしても完了扱いになる。recoverしないと、以後はspecもerrも
// nilのまま返り、呼ぶたびにnilの参照でpanicする。
func (a *apiSpecCache) get(build func() (*encodedSpec, error)) (*encodedSpec, error) {
	a.once.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				a.spec, a.err = nil, fmt.Errorf("build /api.json: panic: %v", r)
			}
		}()
		a.spec, a.err = build()
		if a.spec == nil && a.err == nil {
			a.err = errors.New("build /api.json: no document")
		}
	})
	return a.spec, a.err
}

func (s *Server) buildOpenAPISpec() (*encodedSpec, error) {
	gen, err := openapi.NewDefault()
	if err != nil {
		return nil, err
	}
	var routes []openapi.Route
	for _, r := range s.echo.Routes() {
		name, ok := strings.CutPrefix(r.Path, "/api/")
		if !ok {
			continue
		}
		routes = append(routes, openapi.Route{Method: r.Method, Name: name})
	}
	plain, err := gen.Spec(openapi.Options{
		Version:        config.MkGoVersion,
		MisskeyVersion: config.MisskeyVersion,
		RepositoryURL:  config.MkGoRepositoryURL,
		APIURL:         strings.TrimSuffix(s.config.URL, "/") + "/api",
		Routes:         routes,
	})
	if err != nil {
		return nil, err
	}
	return &encodedSpec{plain: plain, gzip: gzipBytes(plain)}, nil
}

// gzipBytes compresses b with the best compression. It cannot fail: it
// writes to memory with a constant level.
//
// 1度だけ作って使い回すので、最大の圧縮率で縮める(/api/emojisと同じ)。
func gzipBytes(b []byte) []byte {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	_, _ = zw.Write(b)
	_ = zw.Close()
	return buf.Bytes()
}
