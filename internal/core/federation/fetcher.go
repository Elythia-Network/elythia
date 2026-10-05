package federation

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/elythia-network/elythia/internal/activitypub"
)

// SignerProvider supplies the parsed private key (typically the
// instance.actor system account) used to sign outgoing AP fetches.
// 戻り値の `*PrivateKey` は parse 済みなので fetcher 側での再 parse は不要。
// instance.actor の row / keypair は実質変わらないので実装側でキャッシュ
// してよい。
//
// 契約:
//   - 成功時: (*PrivateKey, nil) を返す。nil key + nil error は invalid と
//     し、APFetcher は unsigned-only モードにフォールバックする (将来の
//     実装が誤って (nil, nil) を返した場合の defensive default)。
//   - 失敗時: (nil, ErrNoSigner) を返す。実装側で transient / permanent の
//     区別を吸収すること (例: 一定時間 backoff してから再試行)。
type SignerProvider interface {
	Signer() (*activitypub.PrivateKey, error)
}

// APFetcher wraps an activitypub.Client to satisfy HTTPFetcher。
//
// Default behaviour after #419: try signed GET first using the configured
// SignerProvider (instance.actor)。ピアが authorized-fetch を強制する
// (Iceshrimp.NET / Mastodon secure mode 等) ケースを fail せずに通す。
// 署名 GET が 401/403 を返した場合のみ unsigned GET にフォールバックする
// (404/410 等の一過性でないエラーで二重リクエストはしない)。SignerProvider
// が unset なら従来通り unsigned GET 一発で動く。
type APFetcher struct {
	client *activitypub.Client
	signer SignerProvider
}

// NewAPFetcher constructs an APFetcher.
func NewAPFetcher(client *activitypub.Client) *APFetcher {
	return &APFetcher{client: client}
}

// SetSigner attaches a SignerProvider used for default signed AP fetches.
// 未配線なら従来通り未署名 GET のみ。
func (f *APFetcher) SetSigner(s SignerProvider) {
	f.signer = s
}

// FetchObjectWithFinalURL is FetchObject but also returns the final response URL
// (after redirects) so the resolver can bind a fetched object's id host to the
// host that served it (object-spoofing 防止、#1820)。
func (f *APFetcher) FetchObjectWithFinalURL(uri string) ([]byte, string, error) {
	if f.signer != nil {
		key, err := f.signer.Signer()
		switch {
		case err != nil:
			// signer が unavailable な場合の degradation がログ無しで隠れて
			// しまうのを避ける (#419 Devin review)。事象自体の Warn ログは
			// signer 実装側で出すので、ここでは per-fetch の Debug にとどめ
			// て log spam を抑える。
			slog.Debug("ap fetcher: signer unavailable, falling back to unsigned",
				"uri", uri, "err", err)
		case key != nil:
			body, finalURL, ferr := f.client.FetchJSONWithURL(uri, key)
			if ferr == nil {
				return body, finalURL, nil
			}
			// 4xx の中でも 401/403 だけがフォールバック対象。404/410 は
			// 「リソース不在」を意味するので unsigned で再リクエストしても
			// 結果は同じ (#419 Devin review)。
			if !shouldFallbackToUnsigned(ferr) {
				return nil, "", ferr
			}
			slog.Debug("ap fetcher: signed fetch unauthorized, falling back to unsigned",
				"uri", uri, "err", ferr)
		}
	}
	return f.client.FetchUnsignedWithURL(uri)
}

// FetchObject performs a default-signed GET against uri, falling back to
// unsigned GET on 401/403. Resolver でアクター取得やリモート Note 取得に
// 共通で使う (#419)。
func (f *APFetcher) FetchObject(uri string) ([]byte, error) {
	body, _, err := f.FetchObjectWithFinalURL(uri)
	return body, err
}

// FetchObjectSignedOnly performs a signed GET and never falls back to an
// unsigned one. Returns ErrNoSigner when no signer is available.
//
// 疎通の診断 (#3055) 用。FetchObject は 401/403 で署名なしへ落ちるので、相手が
// こちらの署名を拒否しているのか、署名なしでも読めるのかを区別できない。
func (f *APFetcher) FetchObjectSignedOnly(uri string) ([]byte, error) {
	if f.signer == nil {
		return nil, ErrNoSigner
	}
	key, err := f.signer.Signer()
	if err != nil {
		return nil, err
	}
	if key == nil {
		return nil, ErrNoSigner
	}
	body, _, err := f.client.FetchJSONWithURL(uri, key)
	return body, err
}

// FetchObjectUnsigned performs an explicit unsigned GET. nodeinfo /
// .well-known/* など peer 認証を要求しない discovery endpoint 用 (#419
// Devin review)。SignerProvider が wire されていても署名を付けない。
func (f *APFetcher) FetchObjectUnsigned(uri string) ([]byte, error) {
	return f.client.FetchUnsigned(uri)
}

// FetchHTML performs an unsigned GET with Accept: text/html. Instance metadata
// fetcher がリモートトップページから <link rel="icon"> を抜き出す用途で使う。
func (f *APFetcher) FetchHTML(uri string) ([]byte, error) {
	return f.client.FetchHTML(uri)
}

// FetchJSON performs an unsigned GET with Accept: application/json, */*
// for non-AP discovery endpoints (specifically nodeinfo, #474). Iceshrimp.NET
// 等の strict 実装は AP MIME を Accept に渡すと 406 を返すため、nodeinfo
// パスでは plain JSON を要求する必要がある。
func (f *APFetcher) FetchJSON(uri string) ([]byte, error) {
	return f.client.FetchUnsignedJSON(uri)
}

// FetchJSONWithFinalURL is FetchJSON but also returns the URL that actually
// served the response (after redirects), so callers can decide whether to read
// it back (see internal/core/instance).
func (f *APFetcher) FetchJSONWithFinalURL(uri string) ([]byte, string, error) {
	return f.client.FetchUnsignedJSONWithURL(uri)
}

// shouldFallbackToUnsigned reports whether an error from a signed fetch
// warrants retrying without the signature. AP の authorized-fetch 系の peer
// は鍵検証失敗で 401 / 403 を返すので、この 2 つだけフォールバック対象に
// する。それ以外 (network error, 404, 5xx) は signed/unsigned で結果が
// 変わらないので二重リクエストしない (#419 Devin review)。
func shouldFallbackToUnsigned(err error) bool {
	var se *activitypub.StatusError
	if !errors.As(err, &se) {
		return false
	}
	return se.StatusCode == http.StatusUnauthorized ||
		se.StatusCode == http.StatusForbidden
}

// ErrNoSigner is returned by SignerProvider implementations when the local
// instance actor / keypair is not yet provisioned. Callers treat this as
// "skip signing this call" rather than a hard failure.
var ErrNoSigner = errors.New("federation: instance signer not available")
