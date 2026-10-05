package drive

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"

	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/safehttp"
)

// defaultURLUploadMaxBytes caps a remote download when no explicit limit is
// configured. Mirrors config.defaultMaxFileSize (250 MiB).
const defaultURLUploadMaxBytes int64 = 262144000

// driveUploader is the subset of *coredrive.Service the URL uploader needs.
type driveUploader interface {
	Upload(ctx context.Context, in coredrive.UploadInput) (*model.DriveFile, error)
}

// mainEventPublisher emits a `{type, body}` event to a single user's main
// stream (satisfied by *stream.MainStreamPublisher).
type mainEventPublisher interface {
	PublishMainEvent(userID, eventType string, body any)
}

// URLUploadInput carries the resolved parameters of drive/files/upload-from-url.
type URLUploadInput struct {
	User        *model.User
	URL         string
	FolderID    *string
	Comment     *string
	Marker      *string
	IsSensitive bool
	Force       bool
	RequestIP   string
}

// URLUploadProcessor downloads a remote file and stores it in the drive,
// notifying the requesting user via the main stream when finished.
type URLUploadProcessor interface {
	Process(ctx context.Context, in URLUploadInput)
}

// URLUploader is the production URLUploadProcessor. httpClient must be wired
// with safehttp.NewSSRFSafeTransport(...) so private IPs and non-http(s)
// schemes never leak through this download path (the SSRF-safe transport also
// blocks redirects to private IPs, so no extra redirect handling is needed).
type URLUploader struct {
	httpClient *http.Client
	uploader   driveUploader
	publisher  mainEventPublisher
	idGen      id.Generator
	userAgent  string
	maxBytes   int64
}

// NewURLUploader builds a URLUploader. maxBytes <= 0 falls back to the 250 MiB
// default; pass cfg.MaxFileSize so a download never exceeds what the drive
// would accept anyway.
func NewURLUploader(httpClient *http.Client, uploader driveUploader, publisher mainEventPublisher, idGen id.Generator, userAgent string, maxBytes int64) *URLUploader {
	if maxBytes <= 0 {
		maxBytes = defaultURLUploadMaxBytes
	}
	return &URLUploader{
		httpClient: httpClient,
		uploader:   uploader,
		publisher:  publisher,
		idGen:      idGen,
		userAgent:  userAgent,
		maxBytes:   maxBytes,
	}
}

// Process fetches in.URL and stores it as a drive file owned by in.User, then
// emits `urlUploadFinished` ({marker, file}) to the user's main stream.
//
// upstream Misskey の drive/files/upload-from-url と同じく fire-and-forget で、
// download / 保存に失敗した場合はクライアントに urlUploadFinished を送らない
// (= 呼び出し側 UI は timeout 表示にフォールバックする)。エラーは redacted で
// log に残すだけにする。
func (u *URLUploader) Process(ctx context.Context, in URLUploadInput) {
	df, err := u.fetchAndStore(ctx, in)
	if err != nil {
		uid := ""
		if in.User != nil {
			uid = in.User.ID
		}
		slog.Warn("drive upload-from-url failed", "user", uid, "err", err)
		return
	}
	if u.publisher == nil || in.User == nil {
		return
	}
	// upstream は pack(file, {self:true}) の single pack を payload に使う
	// (= drive/files/create の応答と同 shape)。helper を共有して shape drift
	// を防ぐ (#1564 review)。
	u.publisher.PublishMainEvent(in.User.ID, "urlUploadFinished", map[string]any{
		"marker": in.Marker,
		"file":   packDriveFileSelfSingle(df, u.idGen),
	})
}

func (u *URLUploader) fetchAndStore(ctx context.Context, in URLUploadInput) (*model.DriveFile, error) {
	if u.httpClient == nil || u.uploader == nil {
		return nil, fmt.Errorf("url uploader not wired")
	}
	parsed, err := url.Parse(in.URL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid url")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "*/*")
	if u.userAgent != "" {
		req.Header.Set("User-Agent", u.userAgent)
	}

	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("upstream status %d", resp.StatusCode)
	}

	body, err := safehttp.ReadAllLimit(resp.Body, u.maxBytes)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	// Misskey TS の uploadFromUrl 相当で `name = path.basename(url)` を使う。
	// path が無い (= ルート) URL では fallback 名を当てる。
	name := path.Base(parsed.Path)
	if name == "" || name == "." || name == "/" {
		name = "unknown"
	}

	var ip *string
	if in.RequestIP != "" {
		ip = &in.RequestIP
	}
	return u.uploader.Upload(ctx, coredrive.UploadInput{
		User:        in.User,
		Body:        body,
		Name:        name,
		Comment:     in.Comment,
		FolderID:    in.FolderID,
		IsSensitive: in.IsSensitive,
		Force:       in.Force,
		RequestIP:   ip,
	})
}
