package drive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"path"
	"sync"
	"time"

	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// defaultURLUploadMaxBytes caps a remote download when no explicit limit is
// configured. Mirrors config.defaultMaxFileSize (250 MiB).
const defaultURLUploadMaxBytes int64 = 262144000

// driveUploader is the subset of *coredrive.Service the URL uploader needs.
type driveUploader interface {
	Upload(ctx context.Context, in coredrive.UploadInput) (*model.DriveFile, error)
}

// uploadLimiter reports a user's maxFileSizeMb policy in bytes before the body
// is read (*coredrive.Service implements it).
type uploadLimiter interface {
	MaxUploadBytes(user *model.User) (int64, bool)
}

var _ uploadLimiter = (*coredrive.Service)(nil)

// Concurrency limits of upload-from-url.
//
// **取得は全部メモリに載せるので、同時に走る数を絞る。** 応答は即 204 で、取得は
// 裏の goroutine が行う。上限が無いと、rate limit (1 時間 60 回) の範囲で並べた
// 取得が同時に本文を抱える。待つ側は本文を持たないので、待たせるだけでよい。
const (
	maxConcurrentURLUploads        = 4
	maxConcurrentURLUploadsPerUser = 1
	// urlUploadWaitTimeout bounds how long a request waits for a slot.
	urlUploadWaitTimeout = 10 * time.Minute
)

// errURLUploadTooLarge reports a body over the effective size limit.
var errURLUploadTooLarge = errors.New("max file size exceeded")

// errURLUploadAbandoned reports a download that finished after the server
// generation started stopping, so the file is not saved (#3661).
var errURLUploadAbandoned = errors.New("abandoned before saving because the server generation is stopping")

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

	slots     chan struct{}
	userMu    sync.Mutex
	userSlots map[string]*userSlot
	// entityPacker は entity を包むときの依存 (role 由来の値、メディアのプロキシ等) を
	// 持つ (#3796)。組み立てで渡し、既定の Packer を読まない。
	entityPacker *entity.Packer
}

// SetEntityPacker wires the Packer this URLUploader packs entities with (#3796).
func (u *URLUploader) SetEntityPacker(p *entity.Packer) { u.entityPacker = p }

// HasEntityPacker reports whether SetEntityPacker received a Packer that
// rewrites media URLs. 無いとファイルの url などを生のまま返し、閲覧者の IP が
// リモートへ漏れる。
func (u *URLUploader) HasEntityPacker() bool { return u.entityPacker.HasMediaURL() }

// userSlot is the per-user semaphore and the number of requests holding or
// waiting for it (the entry is dropped when that reaches zero).
type userSlot struct {
	ch   chan struct{}
	refs int
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
		slots:      make(chan struct{}, maxConcurrentURLUploads),
		userSlots:  make(map[string]*userSlot),
	}
}

// acquire waits for a per-user slot and then a global slot. The returned
// function releases both.
func (u *URLUploader) acquire(ctx context.Context, userID string) (func(), error) {
	u.userMu.Lock()
	us, ok := u.userSlots[userID]
	if !ok {
		us = &userSlot{ch: make(chan struct{}, maxConcurrentURLUploadsPerUser)}
		u.userSlots[userID] = us
	}
	us.refs++
	u.userMu.Unlock()

	unref := func() {
		u.userMu.Lock()
		us.refs--
		if us.refs == 0 {
			delete(u.userSlots, userID)
		}
		u.userMu.Unlock()
	}
	select {
	case us.ch <- struct{}{}:
	case <-ctx.Done():
		unref()
		return nil, ctx.Err()
	}
	select {
	case u.slots <- struct{}{}:
	case <-ctx.Done():
		<-us.ch
		unref()
		return nil, ctx.Err()
	}
	return func() {
		<-u.slots
		<-us.ch
		unref()
	}, nil
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
		"file":   packDriveFileSelfSingle(u.entityPacker, df, u.idGen),
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

	// **読む上限は利用者の maxFileSizeMb で絞る。** `Upload` も同じ判定をするが、
	// そこに届くのは本文を全部読んだ後なので、30MB しか保存できない利用者でも
	// config.maxFileSize (既定 250MiB) まで確保させられた。drive/files/create は
	// 読む前に同じ値を見ている (#3037)。
	limit := u.maxBytes
	if lim, ok := u.uploader.(uploadLimiter); ok {
		if pl, ok := lim.MaxUploadBytes(in.User); ok {
			if pl < 0 {
				return nil, errURLUploadTooLarge
			}
			if pl < limit {
				limit = pl
			}
		}
	}

	waitCtx, cancelWait := context.WithTimeout(ctx, urlUploadWaitTimeout)
	defer cancelWait()
	uid := ""
	if in.User != nil {
		uid = in.User.ID
	}
	release, err := u.acquire(waitCtx, uid)
	if err != nil {
		return nil, fmt.Errorf("wait for a download slot: %w", err)
	}
	defer release()

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

	// 宣言された長さが上限を超えるなら読まずに断る。
	if resp.ContentLength > limit {
		return nil, errURLUploadTooLarge
	}
	// limit+1 が溢れると LimitReader が空の本文を返すので、上の端で止める。
	readCap := limit
	if readCap < math.MaxInt64 {
		readCap++
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, readCap))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, errURLUploadTooLarge
	}

	// Misskey TS の uploadFromUrl 相当で `name = path.basename(url)` を使う。
	// path が無い (= ルート) URL では fallback 名を当てる。
	name := path.Base(parsed.Path)
	if name == "" || name == "." || name == "/" {
		name = "unknown"
	} else if !coredrive.ValidateFileName(name) {
		// **名前の形を確かめる。** URL の path は percent-decode されるので、
		// NUL や `\`、200 文字を超える名前が来る。そのまま保存すると drive_file.name
		// の INSERT が落ちる (SQLSTATE 22021 / 22001)。upstream の addFile は
		// validateFileName に通らない名前を 'untitled' にする。
		name = "untitled"
	}

	var ip *string
	if in.RequestIP != "" {
		ip = &in.RequestIP
	}
	// **取得が終わった後は、ctx の取り消しで保存を止めない (#3661)。** ctx は世代の
	// 停止で切れる。Upload の途中で切れると、サムネイルの生成が枠を取れずに飛び、
	// センシティブの判定が fail-open で通って、劣化したファイルが確定する。打ち切って
	// よいのは外向きの取得だけにする。取得の後に既に切れていたら、保存を始めずに
	// 諦める。利用者には urlUploadFinished を送らない (取得に失敗したときと同じ)。
	// 保存には期限を付けない。付けると、期限が来たときに同じ fail-open で劣化する。
	// 世代の停止が待ちきれずに DB を閉じれば、保存は失敗して確定しない。
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", errURLUploadAbandoned, err)
	}
	return u.uploader.Upload(context.WithoutCancel(ctx), coredrive.UploadInput{
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
