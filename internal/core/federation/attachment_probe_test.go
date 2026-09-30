package federation

import (
	"archive/zip"
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// renderTestPNG returns a minimal valid PNG of the given dimensions so
// the probe can decode real image bytes (rather than relying on a
// hand-rolled fixture).
func renderTestPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func renderTestJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, nil))
	return buf.Bytes()
}

func renderTestZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("readme.txt")
	require.NoError(t, err)
	_, err = w.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func serveBytes(t *testing.T, contentType, disposition string, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		if disposition != "" {
			w.Header().Set("Content-Disposition", disposition)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchAttachmentProbe_ImageDimensionsAndType(t *testing.T) {
	srv := serveBytes(t, "image/png", "", renderTestPNG(t, 1280, 720))

	p, err := fetchAttachmentProbe(context.Background(), srv.Client(), srv.URL+"/cat.png")
	require.NoError(t, err)
	assert.Equal(t, "image/png", p.MIME)
	assert.Equal(t, 1280, p.Width)
	assert.Equal(t, 720, p.Height)
	assert.Empty(t, p.FileName)
}

// 応答の Content-Type を信じない。Misskey 系のオブジェクトストレージは
// octet-stream しか返さないので、見ていると画像が画像にならない。
func TestFetchAttachmentProbe_SniffsContentNotHeader(t *testing.T) {
	srv := serveBytes(t, "application/octet-stream", "", renderTestJPEG(t, 8, 6))

	p, err := fetchAttachmentProbe(context.Background(), srv.Client(), srv.URL+"/x")
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", p.MIME)
	assert.Equal(t, 8, p.Width)
	assert.Equal(t, 6, p.Height)
}

// 画像と申告された HTML は HTML として扱い、寸法も作らない。
func TestFetchAttachmentProbe_HTMLIsNotAnImage(t *testing.T) {
	srv := serveBytes(t, "image/png", "", []byte("<html><body>error page</body></html>"))

	p, err := fetchAttachmentProbe(context.Background(), srv.Client(), srv.URL+"/x.png")
	require.NoError(t, err)
	assert.Equal(t, "text/html", p.MIME)
	assert.Zero(t, p.Width)
	assert.Zero(t, p.Height)
}

func TestFetchAttachmentProbe_RequestsOnlyTheHead(t *testing.T) {
	var gotRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRange = r.Header.Get("Range")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(renderTestPNG(t, 3, 3))
	}))
	defer srv.Close()

	p, err := fetchAttachmentProbe(context.Background(), srv.Client(), srv.URL+"/x")
	require.NoError(t, err, "206 を失敗扱いにしている")
	assert.Equal(t, "image/png", p.MIME)
	assert.Equal(t, "bytes=0-65535", gotRange)
}

// countingBody hands out 2MiB of zero bytes and records how much the caller
// consumed. 有限にしてあるのは、上限を外す変異でテストが止まらずに落ちるように
// するため。
type countingBody struct{ read atomic.Int64 }

const countingBodySize = 2 * 1024 * 1024

func (b *countingBody) Read(p []byte) (int, error) {
	left := countingBodySize - b.read.Load()
	if left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > left {
		p = p[:left]
	}
	for i := range p {
		p[i] = 0
	}
	b.read.Add(int64(len(p)))
	return len(p), nil
}

func (b *countingBody) Close() error { return nil }

type fixedTransport struct{ body *countingBody }

func (f fixedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: f.body, Request: req}, nil
}

// Range を無視して全体を返すサーバでも、読むのは上限まで。
func TestFetchAttachmentProbe_BoundsBodyWhenRangeIgnored(t *testing.T) {
	body := &countingBody{}
	_, err := fetchAttachmentProbe(context.Background(), &http.Client{Transport: fixedTransport{body}}, "https://media.example/big")
	require.NoError(t, err)
	assert.LessOrEqual(t, body.read.Load(), attachmentFetchMaxBytes+32*1024, "上限を超えて読んでいる")
}

func TestFetchAttachmentProbe_Errors(t *testing.T) {
	t.Run("status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "not found", http.StatusNotFound)
		}))
		defer srv.Close()
		_, err := fetchAttachmentProbe(context.Background(), srv.Client(), srv.URL+"/x")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "404")
	})
	t.Run("empty body", func(t *testing.T) {
		srv := serveBytes(t, "image/png", "", nil)
		_, err := fetchAttachmentProbe(context.Background(), srv.Client(), srv.URL+"/x")
		require.Error(t, err)
	})
	t.Run("bad url", func(t *testing.T) {
		_, err := fetchAttachmentProbe(context.Background(), http.DefaultClient, "://invalid")
		require.Error(t, err)
	})
}

func TestFetchAttachmentProbe_NilClientUsesDefault(t *testing.T) {
	srv := serveBytes(t, "image/png", "", renderTestPNG(t, 10, 20))
	p, err := fetchAttachmentProbe(context.Background(), nil, srv.URL+"/x.png")
	require.NoError(t, err)
	assert.Equal(t, 10, p.Width)
	assert.Equal(t, 20, p.Height)
}

// SSRF 対策 (PR #464 review): probeAttachment に nil client を渡すと
// http.DefaultClient へフォールバックせず即座に skip する。
//
// 宛先を実在しないホストにするだけだと、フォールバックしても名前解決で失敗して
// 同じ結果になる (空振りする)。http.DefaultClient に届いたかを直接見る。
func TestProbeAttachment_NilClientReturnsFalse(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(renderTestPNG(t, 2, 2))
	}))
	defer srv.Close()

	p, ok := probeAttachment(context.Background(), nil, srv.URL+"/x.png")
	assert.False(t, ok, "nil client should not perform any HTTP request")
	assert.Equal(t, attachmentProbe{}, p)
	assert.Zero(t, hits.Load(), "nil client で既定の client にフォールバックしている")
}

func TestProbeAttachment_FailureReturnsFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p, ok := probeAttachment(context.Background(), srv.Client(), srv.URL+"/x.png")
	assert.False(t, ok)
	assert.Equal(t, attachmentProbe{}, p)
}

func TestProbeAttachment_Success(t *testing.T) {
	srv := serveBytes(t, "image/png", "", renderTestPNG(t, 800, 600))
	p, ok := probeAttachment(context.Background(), srv.Client(), srv.URL+"/cat.png")
	require.True(t, ok)
	assert.Equal(t, 800, p.Width)
	assert.Equal(t, 600, p.Height)
}

func TestDispositionFileName(t *testing.T) {
	cases := []struct {
		name, header, want string
	}{
		{"empty", "", ""},
		{"no filename", "inline", ""},
		{"plain", `attachment; filename="report.pdf"`, "report.pdf"},
		// upstream の content-disposition パッケージと同じく filename* を優先する。
		{"extended wins", `inline; filename="__.zip"; filename*=UTF-8''%E3%82%A2%E3%83%B3.zip`, "アン.zip"},
		{"extended only", `attachment; filename*=UTF-8''a%20b.png`, "a b.png"},
		{"path separator", `attachment; filename="a/b.png"`, ""},
		{"traversal", `attachment; filename=".."`, ""},
		{"unparsable", `attachment; filename=x y.png`, ""},
		{"raw control char", "attachment; filename=\"a\x01b.png\"", ""},
		{"encoded newline", `attachment; filename*=UTF-8''a%0Ab.png`, ""},
		{"rlo spoof", `attachment; filename*=UTF-8''photo%E2%80%AEgnp.exe`, ""},
		{"isolate", `attachment; filename*=UTF-8''a%E2%81%A6b.png`, ""},
		{"lrm", `attachment; filename*=UTF-8''a%E2%80%8Eb.png`, ""},
		{"rlm", `attachment; filename*=UTF-8''a%E2%80%8Fb.png`, ""},
		{"alm", `attachment; filename*=UTF-8''a%D8%9Cb.png`, ""},
		{"plain non-ascii is fine", `attachment; filename*=UTF-8''%E5%86%99%E7%9C%9F.png`, "写真.png"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, dispositionFileName(tc.header))
		})
	}
}

func TestResolveAttachmentMIME(t *testing.T) {
	cases := []struct {
		name, sniffed, declared, apType, url, want string
	}{
		{"sniffed wins over declared", "image/jpeg", "application/octet-stream", "Document", "https://r/x", "image/jpeg"},
		{"sniffed wins over conflicting media", "image/png", "image/jpeg", "Image", "https://r/x.jpg", "image/png"},
		// 200 で返ったエラーページを読んだ形。メディアを名乗るなら判定を捨てる。
		{"error page under image declaration", "text/html", "image/png", "Image", "https://r/x.png", "image/png"},
		{"json error under video type", "application/json", "", "Video", "https://r/v.mp4", "video/mp4"},
		{"html error under image declaration only", "text/html", "image/png", "Document", "https://r/x", "image/png"},
		{"html error under video declaration", "text/html", "video/mp4", "Document", "https://r/v", "video/mp4"},
		{"json error under audio type only", "application/json", "", "Audio", "https://r/a", "application/octet-stream"},
		{"xml error under audio declaration", "text/xml", "audio/mpeg", "Document", "https://r/a", "audio/mpeg"},
		{"document without media claim keeps sniffed", "text/html", "", "Document", "https://r/x", "text/html"},
		{"document without media claim keeps sniffed json", "application/json", "application/octet-stream", "Document", "https://r/x", "application/json"},
		{"unknown content keeps declaration", "application/octet-stream", "audio/midi", "Document", "https://r/x", "audio/midi"},
		{"vague text keeps declaration", "text/plain", "application/json", "Document", "https://r/x", "application/json"},
		{"vague text without anything else", "text/plain", "", "Document", "https://r/x", "text/plain"},
		{"no probe uses declaration", "", "video/mp4", "Video", "https://r/x", "video/mp4"},
		{"no probe no declaration infers from extension", "", "", "Image", "https://r/s.jpg", "image/jpeg"},
		{"octet-stream declaration still infers", "", "application/octet-stream", "Video", "https://r/a.mp4", "video/mp4"},
		{"oversized declaration is ignored", "", "image/" + strings.Repeat("x", 200), "Document", "https://r/a.bin", "application/octet-stream"},
		{"nothing to go on", "", "", "Image", "https://r/xrpc/com.atproto.sync.getBlob?cid=1", "application/octet-stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveAttachmentMIME(tc.sniffed, tc.declared, tc.apType, tc.url))
		})
	}
}

func TestInferAttachmentMIME(t *testing.T) {
	cases := []struct {
		name, apType, url, want string
	}{
		{"image", "Image", "https://r/1.PNG", "image/png"},
		{"document takes any class", "Document", "https://r/v.webm", "video/webm"},
		{"untyped takes any class", "", "https://r/a.mp3", "audio/mpeg"},
		{"class mismatch", "Audio", "https://r/v.webm", ""},
		{"image type with video ext", "Image", "https://r/v.mp4", ""},
		{"video", "Video", "https://r/v.mov", "video/quicktime"},
		{"page is not guessed", "Page", "https://r/x.png", ""},
		// 拡張子だけで SVG と名乗らせない。
		{"svg is never guessed", "Image", "https://r/x.svg", ""},
		{"query is ignored", "Image", "https://r/x?name=a.png", ""},
		{"unknown ext", "Document", "https://r/x.bin", ""},
		{"bad url", "Image", "://bad", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, inferAttachmentMIME(tc.apType, tc.url))
		})
	}
}

func newProbeTestResolver(t *testing.T, client *http.Client) (*Resolver, *testutil.MockDriveFileRepository) {
	t.Helper()
	drive := testutil.NewMockDriveFileRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := NewResolver(testutil.NewMockUserRepository(), testutil.NewMockNoteRepository(),
		activitypub.NewURLBuilder("https://example.com"), nopFetcher{}, idGen)
	r.SetDriveFileRepo(drive)
	r.SetAttachmentProbeClient(client)
	return r, drive
}

func upsertOne(t *testing.T, r *Resolver, drive *testutil.MockDriveFileRepository, doc activitypub.Document) *model.DriveFile {
	t.Helper()
	userID, host := "ru", "remote.example"
	ids := r.upsertAttachments([]activitypub.Document{doc}, &userID, &host)
	require.Len(t, ids, 1)
	return drive.Files[ids[0]]
}

// #3243 の報告で実際に見えていた 3 つの形。
func TestUpsertAttachments_UsesProbedTypeAndName(t *testing.T) {
	t.Run("misskey style: declared octet-stream, name only in Content-Disposition", func(t *testing.T) {
		srv := serveBytes(t, "application/octet-stream",
			`inline; filename="____.zip"; filename*=UTF-8''%E8%B3%87%E6%96%99.zip`, renderTestZip(t))
		r, drive := newProbeTestResolver(t, srv.Client())
		f := upsertOne(t, r, drive, activitypub.Document{
			Type: "Document", MediaType: "application/octet-stream",
			URL: srv.URL + "/io/665a1f93-a708-4bab-bf88-25432332705c",
		})
		assert.Equal(t, "application/zip", f.Type)
		assert.Equal(t, "資料.zip", f.Name)
	})
	t.Run("bridgy web style: no mediaType", func(t *testing.T) {
		srv := serveBytes(t, "image/jpeg", "", renderTestJPEG(t, 40, 30))
		r, drive := newProbeTestResolver(t, srv.Client())
		f := upsertOne(t, r, drive, activitypub.Document{Type: "Image", URL: srv.URL + "/img/s.jpg"})
		assert.Equal(t, "image/jpeg", f.Type)
		assert.Equal(t, "s.jpg", f.Name)
		assert.JSONEq(t, `{"width":40,"height":30}`, string(f.Properties))
	})
	t.Run("bridgy bluesky style: no mediaType, no extension", func(t *testing.T) {
		srv := serveBytes(t, "image/png", "", renderTestPNG(t, 5, 5))
		r, drive := newProbeTestResolver(t, srv.Client())
		f := upsertOne(t, r, drive, activitypub.Document{
			Type: "Image", URL: srv.URL + "/xrpc/com.atproto.sync.getBlob?did=d&cid=c",
		})
		assert.Equal(t, "image/png", f.Type)
	})
}

// 取得できなければ申告と推測に倒す。添付そのものは捨てない。
func TestUpsertAttachments_ProbeFailureFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	r, drive := newProbeTestResolver(t, srv.Client())

	f := upsertOne(t, r, drive, activitypub.Document{Type: "Image", URL: srv.URL + "/img/s.jpg"})
	assert.Equal(t, "image/jpeg", f.Type, "拡張子から推測していない")
	assert.Equal(t, "s.jpg", f.Name)
	assert.Nil(t, f.Properties)

	f = upsertOne(t, r, drive, activitypub.Document{Type: "Document", MediaType: "audio/midi", URL: srv.URL + "/a/b"})
	assert.Equal(t, "audio/midi", f.Type, "申告を捨てている")
}

// 200 で返ったエラーページで、画像を恒久的に文書扱いにしない。
func TestUpsertAttachments_ErrorPageDoesNotOverrideMedia(t *testing.T) {
	srv := serveBytes(t, "text/html", "", []byte("<!DOCTYPE html><html><body>maintenance</body></html>"))
	r, drive := newProbeTestResolver(t, srv.Client())

	f := upsertOne(t, r, drive, activitypub.Document{Type: "Image", URL: srv.URL + "/img/s.jpg"})
	assert.Equal(t, "image/jpeg", f.Type)
}

// エラーページが名前を付けてきても、その名前は採らない。
func TestUpsertAttachments_ErrorPageNameIsDiscarded(t *testing.T) {
	srv := serveBytes(t, "text/html", `inline; filename="error.html"`, []byte("<!DOCTYPE html><html><body>blocked</body></html>"))
	r, drive := newProbeTestResolver(t, srv.Client())

	f := upsertOne(t, r, drive, activitypub.Document{Type: "Document", MediaType: "image/png", URL: srv.URL + "/img/a.png"})
	assert.Equal(t, "image/png", f.Type)
	assert.Equal(t, "a.png", f.Name)

	// AP の type だけでメディアを名乗る形も同じ。
	f = upsertOne(t, r, drive, activitypub.Document{Type: "Image", URL: srv.URL + "/img/b"})
	assert.Equal(t, "b", f.Name)
}

// 形式・名前・寸法を 1 回の GET で読む (寸法のために 2 回目を撃たない)。
func TestUpsertAttachments_OneRequestPerAttachment(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Disposition", `attachment; filename="cat.png"`)
		_, _ = w.Write(renderTestPNG(t, 12, 34))
	}))
	defer srv.Close()
	r, drive := newProbeTestResolver(t, srv.Client())

	f := upsertOne(t, r, drive, activitypub.Document{Type: "Document", URL: srv.URL + "/f/1"})
	assert.Equal(t, int32(1), hits.Load())
	assert.Equal(t, "image/png", f.Type)
	assert.Equal(t, "cat.png", f.Name)
	assert.JSONEq(t, `{"width":12,"height":34}`, string(f.Properties))
}

// AP が寸法を持っていればそちらを使う (probe の値で上書きしない)。
func TestUpsertAttachments_DeclaredDimensionsWin(t *testing.T) {
	srv := serveBytes(t, "image/png", "", renderTestPNG(t, 12, 34))
	r, drive := newProbeTestResolver(t, srv.Client())

	f := upsertOne(t, r, drive, activitypub.Document{
		Type: "Image", MediaType: "image/png", URL: srv.URL + "/a.png", Width: 100, Height: 200,
	})
	assert.JSONEq(t, `{"width":100,"height":200}`, string(f.Properties))
}
