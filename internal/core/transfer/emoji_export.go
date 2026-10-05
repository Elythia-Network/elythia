package transfer

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/safehttp"
)

// ExportCustomEmojis is the export type for the custom-emoji ZIP archive
// (upstream `export-custom-emojis`). Produces a ZIP of every local emoji image
// plus a meta.json that round-trips through emojiimport's import-zip flow.
const ExportCustomEmojis = "custom-emojis"

// emojiExportMetaVersion matches the version emitted by Misskey's
// ExportCustomEmojisProcessorService. emojiimport は値を gate しないが upstream
// 互換のため 2 を出す。
const emojiExportMetaVersion = 2

// defaultEmojiImageMaxBytes caps a single emoji image download (8 MiB, same as
// the admin emoji copy path).
const defaultEmojiImageMaxBytes int64 = 8 << 20

// EmojiSource lists the local custom emojis to include in an export.
// Satisfied by repository.EmojiRepository (CachedEmojiRepository).
type EmojiSource interface {
	ListLocal() ([]*model.Emoji, error)
}

// EmojiImageFetcher downloads an emoji image by URL. Production wires an
// SSRF-safe HTTP client; nil disables image embedding (meta.json only,
// downloaded=false for every entry).
type EmojiImageFetcher interface {
	Fetch(ctx context.Context, url string) ([]byte, error)
}

// emojiMetaDoc / emojiMetaRecord / emojiMetaEmoji mirror the meta.json shape
// consumed by emojiimport (and produced by upstream). Duplicated here (rather
// than shared) so the two packages stay decoupled; the JSON contract is what
// matters and the round-trip is covered by tests.
type emojiMetaDoc struct {
	MetaVersion int               `json:"metaVersion"`
	Host        *string           `json:"host"`
	ExportedAt  string            `json:"exportedAt"`
	Emojis      []emojiMetaRecord `json:"emojis"`
}

type emojiMetaRecord struct {
	FileName   string         `json:"fileName"`
	Downloaded bool           `json:"downloaded"`
	Emoji      emojiMetaEmoji `json:"emoji"`
}

type emojiMetaEmoji struct {
	Name        string   `json:"name"`
	Category    *string  `json:"category"`
	Aliases     []string `json:"aliases"`
	License     *string  `json:"license"`
	IsSensitive bool     `json:"isSensitive"`
	LocalOnly   bool     `json:"localOnly"`
}

// exportCustomEmojis builds a Misskey-compatible custom-emoji ZIP: one entry
// per emoji image named `<emojiName>.<ext>` plus a meta.json describing every
// emoji. Per-image download failures are tolerated (downloaded=false, no zip
// entry) so a single unreachable image does not abort the whole export, matching
// the import side which skips records with downloaded=false / missing entries.
func (e *Exporter) exportCustomEmojis(ctx context.Context) ([]byte, error) {
	if e.deps.EmojiRepo == nil {
		return nil, fmt.Errorf("emoji source not configured")
	}
	emojis, err := e.deps.EmojiRepo.ListLocal()
	if err != nil {
		return nil, fmt.Errorf("list local emojis: %w", err)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	records := make([]emojiMetaRecord, 0, len(emojis))
	for _, em := range emojis {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fileName := em.Name + emojiImageExt(em)
		downloaded := false
		if e.deps.EmojiImageFetcher != nil {
			url := em.PublicURL
			if url == "" {
				url = em.OriginalURL
			}
			if img, ferr := e.deps.EmojiImageFetcher.Fetch(ctx, url); ferr != nil {
				slog.Warn("emoji export: image fetch failed", "emoji", em.Name, "err", ferr)
			} else if len(img) > 0 {
				w, werr := zw.Create(fileName)
				if werr != nil {
					return nil, fmt.Errorf("zip entry %s: %w", fileName, werr)
				}
				if _, werr := w.Write(img); werr != nil {
					return nil, fmt.Errorf("write entry %s: %w", fileName, werr)
				}
				downloaded = true
			}
		}
		records = append(records, emojiMetaRecord{
			FileName:   fileName,
			Downloaded: downloaded,
			Emoji: emojiMetaEmoji{
				Name:        em.Name,
				Category:    em.Category,
				Aliases:     append([]string{}, em.Aliases...),
				License:     em.License,
				IsSensitive: em.IsSensitive,
				LocalOnly:   em.LocalOnly,
			},
		})
	}

	meta := emojiMetaDoc{
		MetaVersion: emojiExportMetaVersion,
		Host:        nil,
		// upstream は new Date().toISOString() (ms 付き `...000Z`) を出す。
		// codebase 内の他 ISO timestamp とも形式を揃える。
		ExportedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Emojis:     records,
	}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("marshal meta.json: %w", err)
	}
	mw, err := zw.Create("meta.json")
	if err != nil {
		return nil, fmt.Errorf("zip meta.json: %w", err)
	}
	if _, err := mw.Write(metaBytes); err != nil {
		return nil, fmt.Errorf("write meta.json: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("close zip: %w", err)
	}
	return buf.Bytes(), nil
}

// emojiImageExt derives the archive filename extension from the emoji's MIME
// type, falling back to the URL path extension and finally ".png".
func emojiImageExt(em *model.Emoji) string {
	if em.Type != nil {
		switch *em.Type {
		case "image/png":
			return ".png"
		case "image/gif":
			return ".gif"
		case "image/jpeg":
			return ".jpg"
		case "image/webp":
			return ".webp"
		case "image/apng":
			return ".apng"
		case "image/avif":
			return ".avif"
		}
	}
	if ext := path.Ext(em.PublicURL); ext != "" {
		return ext
	}
	if ext := path.Ext(em.OriginalURL); ext != "" {
		return ext
	}
	return ".png"
}

// HTTPEmojiImageFetcher is the production EmojiImageFetcher. client must be an
// SSRF-safe http.Client so emoji URLs pointing at private IPs cannot be abused.
type HTTPEmojiImageFetcher struct {
	client    *http.Client
	userAgent string
	maxBytes  int64
}

// NewHTTPEmojiImageFetcher builds a fetcher. maxBytes <= 0 falls back to 8 MiB.
func NewHTTPEmojiImageFetcher(client *http.Client, userAgent string, maxBytes int64) *HTTPEmojiImageFetcher {
	if maxBytes <= 0 {
		maxBytes = defaultEmojiImageMaxBytes
	}
	return &HTTPEmojiImageFetcher{client: client, userAgent: userAgent, maxBytes: maxBytes}
}

// Fetch downloads the image at url, capping the body at maxBytes.
func (f *HTTPEmojiImageFetcher) Fetch(ctx context.Context, url string) ([]byte, error) {
	if f.client == nil || url == "" {
		return nil, fmt.Errorf("emoji image fetcher not usable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "image/*, */*")
	if f.userAgent != "" {
		req.Header.Set("User-Agent", f.userAgent)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	return safehttp.ReadAllLimit(resp.Body, f.maxBytes)
}
