package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"

	"github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/emojiapplication"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/safehttp"
)

// MaxEmojiImageBytes caps the response read from a remote emoji image so a
// hostile / misconfigured server cannot drown the drive backend in arbitrary
// bytes. Real-world custom emoji rarely exceed a few hundred KB; 8 MiB leaves
// generous headroom for animated PNG / APNG / WebP.
const MaxEmojiImageBytes int64 = 8 << 20

// MaxEmojiCopyBytes caps the local drive file duplicated on approval (#2966).
//
// **定義は core に 1 つだけ置く (2 周目レビュー M2)。** 申請側 (`checkFile`) と
// 承認側でこの値が食い違うと、「申請はできたのに承認だけが恒久的に失敗する」
// サイズ帯が生まれる。別名にして、片方だけ動かせないようにする。
const MaxEmojiCopyBytes = emojiapplication.MaxEmojiCopyBytes

// DefaultEmojiCopyAccept matches the headers Misskey TS sends when fetching
// emoji images via DriveService.uploadFromUrl. */* fallback is required since
// some CDN-fronted emoji use generic image/* responses.
const DefaultEmojiCopyAccept = "image/*, */*"

// EmojiImageFetcherImpl is the default EmojiImageFetcher used by router.go.
// httpClient must be wired with safehttp.NewSSRFSafeTransport(...) so private
// IPs and non-http(s) schemes never leak through this fetch path.
type EmojiImageFetcherImpl struct {
	httpClient *http.Client
	driveSvc   *drive.Service
	userAgent  string
}

// NewEmojiImageFetcher builds a fetcher bound to an SSRF-safe HTTP client and
// the local drive service. userAgent should normally be cfg.UserAgent so feed
// servers see the same `mk-go/<ver>` identification used everywhere else.
func NewEmojiImageFetcher(httpClient *http.Client, driveSvc *drive.Service, userAgent string) *EmojiImageFetcherImpl {
	return &EmojiImageFetcherImpl{
		httpClient: httpClient,
		driveSvc:   driveSvc,
		userAgent:  userAgent,
	}
}

// FetchAndStore downloads the image at imageURL and stores it as a drive file.
// Errors are wrapped so the caller can log a redacted reason while returning
// a generic INTERNAL_ERROR to the client. SSRF / dial / parse / oversized
// failures all collapse into a single error here — the SSRF-safe transport
// blocks redirects to private IPs as well, so this layer does not need its
// own redirect handling.
func (f *EmojiImageFetcherImpl) FetchAndStore(ctx context.Context, imageURL string, user *model.User, name string) (*model.DriveFile, error) {
	if f.httpClient == nil || f.driveSvc == nil {
		return nil, fmt.Errorf("emoji image fetcher not wired")
	}
	u, err := url.Parse(imageURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid image url")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", DefaultEmojiCopyAccept)
	if f.userAgent != "" {
		req.Header.Set("User-Agent", f.userAgent)
	}

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("upstream status %d", resp.StatusCode)
	}

	body, err := safehttp.ReadAllLimit(resp.Body, MaxEmojiImageBytes)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	// Misskey TS は uploadFromUrl で `name = path.basename(url) || randomString()`
	// 相当を行う。ここでは emoji name (例: "smile") を渡しているのでそのまま
	// drive file 名として使うが、空なら URL の最終 path セグメントを fallback
	// に取って TS と挙動を揃える。
	driveName := name
	if driveName == "" {
		driveName = path.Base(u.Path)
	}

	// Force: true は upstream Misskey TS の uploadFromUrl({force: true}) と
	// 同じく明示的に指定する。EmojiCopy 経路では user==nil が渡されるため
	// drive.Service.Upload は md5 dedup を既に skip していて Force は no-op
	// になるが、user 付きで本 fetcher を再利用する将来経路 (例: emoji 単発
	// 追加 UI) で「重複しても新しいファイルを作る」契約を読み手に明示する。
	df, err := f.driveSvc.Upload(ctx, drive.UploadInput{
		User:  user,
		Body:  body,
		Name:  driveName,
		Force: true,
	})
	if err != nil {
		return nil, fmt.Errorf("upload to drive: %w", err)
	}
	return df, nil
}

// CopyToSystemFile implements EmojiImageFetcher (#2966).
//
// 承認した絵文字が申請者の drive ファイルに依存し続けるのを断つための複製。
// 申請者がファイルを消しても、アカウントを消しても、絵文字は生き残る。
//
// **実体は `drive.Service` が持つ。** 既存データを直すバッチ (#2990) が
// `cmd/` から同じ複製を回すので、ここに置いたままだと api 層を import させるか、
// 同じ処理をもう 1 つ書くことになる。後者は「片方だけ直して気付かない」形。
func (f *EmojiImageFetcherImpl) CopyToSystemFile(ctx context.Context, src *model.DriveFile, name string, sensitive bool) (*model.DriveFile, error) {
	if f.driveSvc == nil {
		return nil, fmt.Errorf("emoji image fetcher not wired")
	}
	return f.driveSvc.CopyToSystemFile(ctx, src, name, sensitive, MaxEmojiCopyBytes)
}

// DeleteSystemFile implements EmojiImageFetcher (#2966).
func (f *EmojiImageFetcherImpl) DeleteSystemFile(_ context.Context, fileID string) error {
	if f.driveSvc == nil {
		return fmt.Errorf("emoji image fetcher not wired")
	}
	return f.driveSvc.DeleteSystemFile(fileID)
}
