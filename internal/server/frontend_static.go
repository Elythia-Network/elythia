package server

import (
	"compress/gzip"
	"io"
	"io/fs"
	"mime"
	"net/http"
	urlpkg "net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"

	"github.com/elythia-network/elythia/internal/misc/headreq"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// hashedAssetCacheControl is sent with the content-hashed frontend build
// files. The value is upstream's (`ClientServerService.ts` serves `/vite/`
// and `/embed_vite/` with `maxAge: 30 days, immutable`), the same as
// staticAssetCacheControl.
const hashedAssetCacheControl = staticAssetCacheControl

// hashedAssetNameRe matches the file names vite gives build output
// (`<localesHash>-[hash:8].js`, `assets/<localesHash>-[hash:8][extname]`,
// worker chunks `<name>-[hash:8].js`). The hash alphabet is base64url.
//
// **hash の付かないファイルには immutable を付けない。** 本家は `/vite/` の全体に
// 付けるが、`manifest.json`・`preloads.json`・`loader/boot.js` は名前が
// 変わらずに中身だけ変わるので、immutable にすると更新の後も 30 日間古いものを
// 使い続けるブラウザが出る (docs/divergence/operations.md §5.8)。
var hashedAssetNameRe = regexp.MustCompile(`^.+-[A-Za-z0-9_-]{8}\.[A-Za-z0-9]+$`)

// precompressedEncodings lists the precompressed encodings, with the file
// suffix frontend/scripts/precompress.mjs writes. On equal q-values the
// earlier one wins.
var precompressedEncodings = []struct{ coding, suffix string }{
	{"br", ".br"},
	{"gzip", ".gz"},
}

// frontendDynamicGzip compresses frontend responses that have no
// precompressed variant, with the settings of the global gzip middleware
// (which skips the frontend routes, see selfEncodedRoutes).
func frontendDynamicGzip() echo.MiddlewareFunc {
	return echomw.GzipWithConfig(echomw.GzipConfig{
		Level:     gzip.DefaultCompression,
		MinLength: 1024,
	})
}

// serveFrontendBuild returns the handler for `<prefix>/*` that serves the
// frontend build in dir.
//
//   - A content-hashed file gets `Cache-Control: public, max-age=2592000,
//     immutable`, so a returning browser sends no request at all.
//   - For a content-hashed file, when `<file>.br` / `<file>.gz` exists and
//     the client accepts that encoding, it is sent as is with
//     Content-Encoding, so nothing is compressed per request. Otherwise (and
//     always for files without a hash) the file is gzip-compressed on the fly
//     as before. Hashed files always carry `Vary: Accept-Encoding`.
//
// ファイルの解決 (パスの検査、ディレクトリの扱い、404) は echo の
// StaticDirectoryHandler に任せ、ここでは同じ名前の事前圧縮のファイルを
// 探すだけにする。
func serveFrontendBuild(dir string) echo.HandlerFunc {
	fsys := os.DirFS(dir)
	fallback := frontendDynamicGzip()(echo.StaticDirectoryHandler(fsys, false))
	return func(c echo.Context) error {
		name, ok := frontendAssetName(c.Param("*"))
		if !ok {
			return fallback(c)
		}
		fi, err := fs.Stat(fsys, name)
		if err != nil || !fi.Mode().IsRegular() {
			return fallback(c)
		}
		if !hashedAssetNameRe.MatchString(path.Base(name)) {
			// hash の無いファイルは事前圧縮を使わない。名前を変えずに中身だけが
			// 変わるので、作り直されなかった古い .br / .gz を返しうる (#3593)。
			return fallback(c)
		}
		res := c.Response()
		res.Before(func() {
			// 応答を書く直前に status を見て付ける。Stat の後でも、読み出しの失敗や
			// Range の誤りで 4xx / 5xx になりうるので、それには付けない。
			if (res.Status >= 200 && res.Status < 300) || res.Status == http.StatusNotModified {
				res.Header().Set(echo.HeaderCacheControl, hashedAssetCacheControl)
			}
			// 変種を選ばなかった応答 (Accept-Encoding が無い、q=0) も、同じ URL で
			// 符号化が変わりうるので常に Vary を付ける。その場の gzip が付けた分とは
			// 重ねない。
			if !varyHasAcceptEncoding(res.Header()) {
				res.Header().Add(echo.HeaderVary, echo.HeaderAcceptEncoding)
			}
		})
		if servePrecompressed(c, fsys, name, fi) {
			return nil
		}
		return fallback(c)
	}
}

// frontendAssetName turns the wildcard param into a cleaned fs.FS name.
// It reports false for anything StaticDirectoryHandler would treat
// specially (bad escapes, encoded separators), leaving that to it.
func frontendAssetName(param string) (string, bool) {
	lower := strings.ToLower(param)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
		return "", false
	}
	p, err := urlpkg.PathUnescape(param)
	if err != nil {
		return "", false
	}
	// `.`・`..`・空のセグメントを含むパスは、path.Clean で正規化して配信しない。
	// echo の静的配信はこの形を 404 にするので、自前の経路だけが事前圧縮の変種と
	// immutable を付けて返すと、方針がずれる (#3369)。
	for seg := range strings.SplitSeq(strings.TrimPrefix(p, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}
	name := path.Clean(strings.TrimPrefix(p, "/"))
	if !fs.ValidPath(name) || name == "." {
		return "", false
	}
	return name, true
}

// servePrecompressed writes name's precompressed variant when one exists
// and the request accepts its encoding. Among the existing variants it takes
// the one with the highest q-value (q=0 refuses), the earlier one on a tie.
// It reports whether it wrote a response.
func servePrecompressed(c echo.Context, fsys fs.FS, name string, orig fs.FileInfo) bool {
	ctype := mime.TypeByExtension(path.Ext(name))
	if ctype == "" {
		return false
	}
	accept := c.Request().Header.Get(echo.HeaderAcceptEncoding)
	var (
		best    io.ReadSeeker
		bestF   fs.File
		bestEnc string
		bestQ   float64
		bestLen int64
	)
	for _, enc := range precompressedEncodings {
		q := middleware.EncodingQ(accept, enc.coding)
		if q <= bestQ {
			continue
		}
		f, err := fsys.Open(name + enc.suffix)
		if err != nil {
			continue
		}
		fi, err := f.Stat()
		rs, seekable := f.(io.ReadSeeker)
		if err != nil || !fi.Mode().IsRegular() || !seekable {
			_ = f.Close()
			continue
		}
		if bestF != nil {
			_ = bestF.Close()
		}
		best, bestF, bestEnc, bestQ, bestLen = rs, f, enc.coding, q, fi.Size()
	}
	if bestF == nil {
		return false
	}
	defer bestF.Close()
	req := headreq.Request(c)
	h := c.Response().Header()
	h.Set(echo.HeaderContentType, ctype)
	h.Set(echo.HeaderContentEncoding, bestEnc)
	res := c.Response()
	res.Before(func() {
		// ServeContent は Content-Encoding があると、Range の無い応答に
		// Content-Length を付けない。200 は本文がファイルそのものなので、ここで
		// 付ける。先に付けると、本文の無い 304 / 412 にも残ってしまう。
		if res.Status == http.StatusOK && res.Header().Get(echo.HeaderContentLength) == "" {
			res.Header().Set(echo.HeaderContentLength, strconv.FormatInt(bestLen, 10))
		}
		// 本文の無いエラー (412 など) に Content-Encoding を残さない。
		if (res.Status < 200 || res.Status >= 300) && res.Status != http.StatusNotModified {
			res.Header().Del(echo.HeaderContentEncoding)
		}
	})
	// Last-Modified は元のファイルの値にする。符号化ごとに値が違うと、
	// 同じ URL の条件付き要求の答えが Accept-Encoding で変わってしまう。
	http.ServeContent(c.Response(), req, name, orig.ModTime(), best)
	return true
}

// varyHasAcceptEncoding reports whether h's Vary already names
// Accept-Encoding.
func varyHasAcceptEncoding(h http.Header) bool {
	for _, v := range h.Values(echo.HeaderVary) {
		for f := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(f), echo.HeaderAcceptEncoding) {
				return true
			}
		}
	}
	return false
}
