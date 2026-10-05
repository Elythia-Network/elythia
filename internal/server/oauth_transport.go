package server

import (
	"net/http"
	"os"

	"github.com/elythia-network/elythia/internal/safehttp"
)

// checkIPRangeEnvKey mirrors the environment variable 本家 OAuth2ProviderService
// uses to re-enable the client_id IP range check inside tests.
const checkIPRangeEnvKey = "MISSKEY_TEST_CHECK_IP_RANGE"

// oauthDiscoveryTransport returns the HTTP transport used to fetch
// attacker-suppliable client_id URLs during OAuth client information discovery.
//
// 本番では必ず SSRF-guarded transport を返す。TestMode のときだけ素の
// transport に落として private IP への fetch を許可する。本家の e2e は
// `http://127.0.0.1:<port>/` を client_id にしてローカルサーバーから
// クライアント情報を取得させるため、ガードしたままでは成立しない。
//
// 緩めるのは **この経路だけ** に留める。本家は HttpRequestService の接続時
// ガード自体を `NODE_ENV === 'production'` でしか動かしておらず、ActivityPub
// の fetch や URL プレビューまで開発・テスト環境では素通しになるが、mk-go は
// それらを常にガードしたまま維持する (#2233 / #2234 / #2236 の監査資産)。
//
// 本家同様、MISSKEY_TEST_CHECK_IP_RANGE=1 でガードを戻せる。oauth.ts が
// 「IP range チェックが効くこと」を検証するケースで使う。
//
// **`opts` を必ず渡すこと (#3037)。** `config.proxy` /
// `outgoingAddress` / `outgoingAddressFamily` は `safehttp.Option` として
// transport に載るので、渡し忘れると**この経路だけがサーバーの素の IP で
// 外へ出る**。#638 が全 outbound を proxy 経由に揃えた不変条件の破れで、
// 運営者は proxy を設定したつもりのまま OAuth クライアントの discovery
// でだけ origin IP を晒すことになる。
func oauthDiscoveryTransport(allowedPrivateNetworks []string, testMode bool, opts ...safehttp.Option) http.RoundTripper {
	if testMode && os.Getenv(checkIPRangeEnvKey) != "1" {
		return http.DefaultTransport
	}
	return safehttp.NewSSRFSafeTransport(allowedPrivateNetworks, opts...)
}
