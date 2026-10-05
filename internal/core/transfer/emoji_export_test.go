package transfer_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/core/transfer"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeEmojiSource struct {
	emojis []*model.Emoji
	err    error
}

func (f *fakeEmojiSource) ListLocal() ([]*model.Emoji, error) { return f.emojis, f.err }

type fakeEmojiFetcher struct {
	byURL  map[string][]byte
	errURL map[string]bool
}

func (f *fakeEmojiFetcher) Fetch(_ context.Context, url string) ([]byte, error) {
	if f.errURL[url] {
		return nil, errors.New("fetch failed")
	}
	if b, ok := f.byURL[url]; ok {
		return b, nil
	}
	return nil, errors.New("not found")
}

func unzip(t *testing.T, body []byte) map[string][]byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	require.NoError(t, err)
	out := make(map[string][]byte, len(zr.File))
	for _, f := range zr.File {
		rc, err := f.Open()
		require.NoError(t, err)
		b, err := io.ReadAll(rc)
		require.NoError(t, rc.Close())
		require.NoError(t, err)
		out[f.Name] = b
	}
	return out
}

func strptr(s string) *string { return &s }

func TestExport_CustomEmojis(t *testing.T) {
	saver, notifier, deps, user := newExportDeps(t)
	deps.EmojiRepo = &fakeEmojiSource{emojis: []*model.Emoji{
		{ID: "e1", Name: "smile", PublicURL: "https://x/e/smile.png", Type: strptr("image/png"), Category: strptr("fun"), Aliases: model.StringArray{"happy"}, IsSensitive: true, LocalOnly: false},
		{ID: "e2", Name: "broken", PublicURL: "https://x/e/broken.png", Type: strptr("image/png")},
	}}
	deps.EmojiImageFetcher = &fakeEmojiFetcher{
		byURL:  map[string][]byte{"https://x/e/smile.png": []byte("PNGDATA")},
		errURL: map[string]bool{"https://x/e/broken.png": true},
	}
	exporter := transfer.NewExporter(deps)

	file, err := exporter.Export(context.Background(), user.ID, transfer.ExportCustomEmojis)
	require.NoError(t, err)
	require.NotNil(t, file)
	require.Len(t, saver.uploads, 1)
	up := saver.uploads[0]
	assert.True(t, strings.HasSuffix(up.Name, ".zip"))
	assert.Contains(t, up.Name, "custom-emojis")

	entries := unzip(t, up.Body)
	// 画像取得に成功した emoji だけ zip entry を持つ。
	require.Contains(t, entries, "smile.png")
	assert.Equal(t, []byte("PNGDATA"), entries["smile.png"])
	_, hasBroken := entries["broken.png"]
	assert.False(t, hasBroken, "fetch 失敗 emoji は画像 entry を持たない")

	require.Contains(t, entries, "meta.json")
	var meta struct {
		MetaVersion int    `json:"metaVersion"`
		ExportedAt  string `json:"exportedAt"`
		Emojis      []struct {
			FileName   string `json:"fileName"`
			Downloaded bool   `json:"downloaded"`
			Emoji      struct {
				Name        string   `json:"name"`
				Category    *string  `json:"category"`
				Aliases     []string `json:"aliases"`
				IsSensitive bool     `json:"isSensitive"`
				LocalOnly   bool     `json:"localOnly"`
			} `json:"emoji"`
		} `json:"emojis"`
	}
	require.NoError(t, json.Unmarshal(entries["meta.json"], &meta))
	assert.Equal(t, 2, meta.MetaVersion)
	// exportedAt は ISO8601 ms 精度 (upstream toISOString 互換)。
	_, perr := time.Parse("2006-01-02T15:04:05.000Z", meta.ExportedAt)
	assert.NoError(t, perr, "exportedAt=%q should be ISO8601 ms", meta.ExportedAt)
	require.Len(t, meta.Emojis, 2)

	assert.Equal(t, "smile.png", meta.Emojis[0].FileName)
	assert.True(t, meta.Emojis[0].Downloaded)
	assert.Equal(t, "smile", meta.Emojis[0].Emoji.Name)
	require.NotNil(t, meta.Emojis[0].Emoji.Category)
	assert.Equal(t, "fun", *meta.Emojis[0].Emoji.Category)
	assert.Equal(t, []string{"happy"}, meta.Emojis[0].Emoji.Aliases)
	assert.True(t, meta.Emojis[0].Emoji.IsSensitive)

	assert.Equal(t, "broken.png", meta.Emojis[1].FileName)
	assert.False(t, meta.Emojis[1].Downloaded, "fetch 失敗は downloaded=false")

	// exportCompleted 通知が custom-emojis entity で発火すること。
	require.Len(t, notifier.calls, 1)
	assert.Equal(t, notification.TypeExportCompleted, notifier.calls[0].Type)
	// exportedEntity は misskey enum 値 (custom-emojis → customEmoji) に変換 (#1249)。
	assert.Equal(t, "customEmoji", notifier.calls[0].Extra["exportedEntity"])
	assert.Equal(t, file.ID, notifier.calls[0].Extra["fileId"])
}

func TestExport_CustomEmojis_NoFetcherProducesMetaOnly(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	deps.EmojiRepo = &fakeEmojiSource{emojis: []*model.Emoji{
		{ID: "e1", Name: "smile", PublicURL: "https://x/e/smile.png", Type: strptr("image/png")},
	}}
	// EmojiImageFetcher 未配線 → 画像なし meta.json のみ。
	exporter := transfer.NewExporter(deps)

	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportCustomEmojis)
	require.NoError(t, err)
	entries := unzip(t, saver.uploads[0].Body)
	require.Contains(t, entries, "meta.json")
	_, hasImg := entries["smile.png"]
	assert.False(t, hasImg)
}

func TestExport_CustomEmojis_NoRepoErrors(t *testing.T) {
	_, _, deps, user := newExportDeps(t)
	// EmojiRepo 未配線 → エラー。
	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportCustomEmojis)
	require.Error(t, err)
}

func TestExport_CustomEmojis_ListError(t *testing.T) {
	_, _, deps, user := newExportDeps(t)
	deps.EmojiRepo = &fakeEmojiSource{err: errors.New("db down")}
	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportCustomEmojis)
	require.Error(t, err)
}

func TestExport_CustomEmojis_ExtFallback(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	// Type なし + URL の拡張子から推定 (.gif)。
	deps.EmojiRepo = &fakeEmojiSource{emojis: []*model.Emoji{
		{ID: "e1", Name: "dance", PublicURL: "https://x/e/dance.gif"},
	}}
	deps.EmojiImageFetcher = &fakeEmojiFetcher{byURL: map[string][]byte{"https://x/e/dance.gif": []byte("GIF")}}
	exporter := transfer.NewExporter(deps)

	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportCustomEmojis)
	require.NoError(t, err)
	entries := unzip(t, saver.uploads[0].Body)
	assert.Contains(t, entries, "dance.gif")
}

func TestExport_CustomEmojis_ExtVariants(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	emojis := []*model.Emoji{
		{ID: "1", Name: "a", PublicURL: "https://x/a", Type: strptr("image/png")},
		{ID: "2", Name: "b", PublicURL: "https://x/b", Type: strptr("image/gif")},
		{ID: "3", Name: "c", PublicURL: "https://x/c", Type: strptr("image/jpeg")},
		{ID: "4", Name: "d", PublicURL: "https://x/d", Type: strptr("image/webp")},
		{ID: "5", Name: "e", PublicURL: "https://x/e", Type: strptr("image/apng")},
		{ID: "6", Name: "f", PublicURL: "https://x/f", Type: strptr("image/avif")},
		// Type が未知/nil で URL にも拡張子が無い → 最終フォールバック .png。
		{ID: "7", Name: "g", PublicURL: "https://x/g", Type: strptr("application/octet-stream")},
	}
	fetch := &fakeEmojiFetcher{byURL: map[string][]byte{}}
	for _, em := range emojis {
		fetch.byURL[em.PublicURL] = []byte("DATA")
	}
	deps.EmojiRepo = &fakeEmojiSource{emojis: emojis}
	deps.EmojiImageFetcher = fetch
	exporter := transfer.NewExporter(deps)

	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportCustomEmojis)
	require.NoError(t, err)
	entries := unzip(t, saver.uploads[0].Body)
	for _, name := range []string{"a.png", "b.gif", "c.jpg", "d.webp", "e.apng", "f.avif", "g.png"} {
		assert.Contains(t, entries, name)
	}
}

func TestExport_CustomEmojis_EmptyBytesNotDownloaded(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	deps.EmojiRepo = &fakeEmojiSource{emojis: []*model.Emoji{
		{ID: "e1", Name: "empty", PublicURL: "https://x/empty.png", Type: strptr("image/png")},
	}}
	// fetch は成功するが 0 byte → entry を作らず downloaded=false。
	deps.EmojiImageFetcher = &fakeEmojiFetcher{byURL: map[string][]byte{"https://x/empty.png": {}}}
	exporter := transfer.NewExporter(deps)

	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportCustomEmojis)
	require.NoError(t, err)
	entries := unzip(t, saver.uploads[0].Body)
	_, hasImg := entries["empty.png"]
	assert.False(t, hasImg)
}

func TestExport_CustomEmojis_OriginalURLFallback(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	// PublicURL 空 → OriginalURL から取得 + 拡張子推定。
	deps.EmojiRepo = &fakeEmojiSource{emojis: []*model.Emoji{
		{ID: "e1", Name: "orig", PublicURL: "", OriginalURL: "https://x/orig.webp"},
	}}
	deps.EmojiImageFetcher = &fakeEmojiFetcher{byURL: map[string][]byte{"https://x/orig.webp": []byte("W")}}
	exporter := transfer.NewExporter(deps)

	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportCustomEmojis)
	require.NoError(t, err)
	entries := unzip(t, saver.uploads[0].Body)
	assert.Contains(t, entries, "orig.webp")
}

func TestExport_CustomEmojis_ContextCancelled(t *testing.T) {
	_, _, deps, user := newExportDeps(t)
	deps.EmojiRepo = &fakeEmojiSource{emojis: []*model.Emoji{
		{ID: "e1", Name: "x", PublicURL: "https://x/x.png", Type: strptr("image/png")},
	}}
	deps.EmojiImageFetcher = &fakeEmojiFetcher{byURL: map[string][]byte{"https://x/x.png": []byte("D")}}
	exporter := transfer.NewExporter(deps)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := exporter.Export(ctx, user.ID, transfer.ExportCustomEmojis)
	require.Error(t, err)
}

func TestHTTPEmojiImageFetcher(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("IMG"))
	}))
	defer srv.Close()

	f := transfer.NewHTTPEmojiImageFetcher(http.DefaultClient, "mk-go-test", 0)
	b, err := f.Fetch(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, []byte("IMG"), b)
}

func TestHTTPEmojiImageFetcher_StatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	f := transfer.NewHTTPEmojiImageFetcher(http.DefaultClient, "", 0)
	_, err := f.Fetch(context.Background(), srv.URL)
	require.Error(t, err)
}

func TestHTTPEmojiImageFetcher_NilClient(t *testing.T) {
	f := transfer.NewHTTPEmojiImageFetcher(nil, "", 0)
	_, err := f.Fetch(context.Background(), "https://example.com/x.png")
	require.Error(t, err)
}
