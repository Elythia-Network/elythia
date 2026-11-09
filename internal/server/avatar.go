package server

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/labstack/echo/v4"

	apiusers "github.com/elythia-network/elythia/internal/api/users"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/remoteurl"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// avatarStaticFallback is the path the frontend treats as the
// "user not found" placeholder. Lives under /static-assets which the
// router maps to the bundled frontend static directory.
const avatarStaticFallback = "/static-assets/user-unknown.png"

// avatarUserLookup is the minimal interface needed to resolve an acct
// string to a model.User row. Implemented by repository.UserRepository,
// abstracted here so handler tests can stub without standing up the
// full repository.
type avatarUserLookup interface {
	FindByUsernameLower(username string, host *string) (*model.User, error)
}

// avatarHandler implements `GET /avatar/@:acct` (#462). The frontend
// `MkMention.vue` constructs `<img src="/avatar/@user@host">` directly
// rather than fetching API metadata, so without this endpoint mention
// chips render with no avatar image.
//
// Behaviour mirrors Misskey TS upstream `ServerService.ts:211`:
//   - acct = "username" → local user (host == nil filter)
//   - acct = "username@host" → remote user; if the host matches the
//     running instance's host it is treated as local (host == nil)
//     since upstream stores local users with host=NULL.
//   - User found → 302 redirect to user.avatarUrl, or to the fallback the
//     API returns as avatarUrl ((*entity.Packer).FallbackAvatarURL) when
//     avatarUrl is unset.
//   - User missing or suspended → 302 redirect to
//     /static-assets/user-unknown.png so the existence of an account
//     is not leaked via mention probes.
//
// Sets Cache-Control: public, max-age=86400 so browsers do not run
// the DB lookup for every mention chip (matches upstream).
func avatarHandler(pk *entity.Packer, userRepo avatarUserLookup, localHost string) echo.HandlerFunc {
	return func(c echo.Context) error {
		// 304 / cache-hit でも返したい header なので Redirect 前に書く。
		c.Response().Header().Set(echo.HeaderCacheControl, "public, max-age=86400")

		acct := c.Param("acct")
		// **実装は users.ParseAcct に一本化してある** (#2791)。
		// pinned-users も同じ形で acct を解く。
		username, host := apiusers.ParseAcct(acct, localHost)
		if username == "" {
			return c.Redirect(http.StatusFound, avatarStaticFallback)
		}

		user, err := userRepo.FindByUsernameLower(strings.ToLower(username), host)
		if err != nil || user == nil {
			return c.Redirect(http.StatusFound, avatarStaticFallback)
		}
		if user.IsSuspended {
			// upstream は suspended な user も `where: { isSuspended: false }`
			// で除外する。一致させて mention 経由で生存確認できないようにする。
			return c.Redirect(http.StatusFound, avatarStaticFallback)
		}

		target := user.AvatarURL
		// リモートの人の、プロキシで包めない形の avatarUrl は使わない ((*entity.Packer).IdenticonURL と同じ)。
		if target != nil && user.Host != nil && *user.Host != "" {
			if clean, ok := remoteurl.Media(*target); ok {
				target = &clean
			} else {
				target = nil
			}
		}
		if target == nil || *target == "" {
			// API の avatarUrl と同じ fallback にする。以前は `/identicon/<user.id>` で、
			// 種が違うので API の avatarUrl とは別の絵になっていた (#4095)
			target = strPtr(pk.FallbackAvatarURL(user))
		}
		// リモートの avatar はメディアプロキシ経由に書き換える (#2425)。
		//
		// **ここだけ生の URL を Location に入れていた。** API 応答側は
		// `entity.ProxyAvatarURL` で既に leak-safe にしてあり (UserLite /
		// chat packUser / stream)、その doc comment も「every avatar-bearing
		// response」と言っているのに、この endpoint は素通しだった。結果、
		// mention chip を 1 つ表示するだけで閲覧者の IP とリファラが相手
		// インスタンスへ渡っていた。
		//
		// **`?static=1` は静止画にして返す (#2908)。** 利用者の「アニメーション
		// 画像を再生しない」設定 (disableShowingAnimatedImages / dataSaver.avatar)
		// が frontend からこの形で届く。ここで受けないと frontend は
		// `<mediaProxy>/static.webp?url=<instance>/avatar/@u@h&static=1` を組み立てるが、
		// **その URL は allowlist のどの列にも無い**ので 403 + `max-age=86400` に
		// なり、静止画になるどころか 1 日壊れる (#2905 のレビューで実測)。
		//
		// **同一オリジンでもプロキシへ回す。** ローカルの drive アバターも
		// GIF / APNG / animated WebP になりうるので、リモートに限ると静止画設定が
		// ローカルユーザーにだけ効かない。allowlist は判断材料にならない —
		// `StaticAvatarProxyURL` は必ず `sig` を付け、`Authorize` は allowlist より
		// **先に** HMAC を見る (`/emoji/:path` が無条件にプロキシへ回しているのと
		// 同じ形、#2905)。
		if _, wantsStatic := c.QueryParams()["static"]; wantsStatic && proxyableAvatarTarget(pk, *target) {
			if proxied := pk.StaticAvatarProxyURL(*target); proxied != "" {
				return c.Redirect(http.StatusFound, proxied)
			}
			// context 未配線 (テスト等) なら従来どおり raw へ 302 する。
		}
		// 同一オリジン・相対 URL (identicon fallback を含む) は wrap されない。
		return c.Redirect(http.StatusFound, pk.ProxyAvatarURLString(*target))
	}
}

// proxyableAvatarTarget reports whether the media proxy can fetch the avatar
// URL at all (#2908).
//
// 除外するのは、自分の identicon だけ。identicon は PNG を生成して返すので
// アニメーションせず、回す理由が無い。`mediaproxy.Fetch` は同一オリジン判定
// (`instanceURL + "/files/"` の接頭一致) を外れたものを `fetchRemote` に回すので、
// 回すと自分自身へ HTTP で取りに行く。自分のホストが private IP に解決される
// 構成 (`allowedPrivateNetworks` で許していないもの) では SSRF の防御で 403 になり、
// そうでなくても往復が 1 回増える。
//
// #4095 より前の identicon は相対 URL で、回していれば `fetchRemote` の入口で
// 400 + `max-age=86400` になるところを、この関数の scheme の検査で外していた。本家と同じ
// 絶対 URL にしたので、自分の identicon を名指しで外す。自分かどうかは、URL を
// 組んだのと同じ Packer のホストで見る。他のインスタンスの /identicon/ は
// 向こうの生成物なので、従来どおり回す。
func proxyableAvatarTarget(pk *entity.Packer, rawURL string) bool {
	if pk.IsOwnIdenticonURL(rawURL) {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	// proxy が実際に取得できるのは http(s) だけ。
	return u.Scheme == "http" || u.Scheme == "https"
}

func strPtr(s string) *string { return &s }

// 静的アサーション: repository.UserRepository は avatarUserLookup を
// 満たす (handler 配線時の型不一致を build-time で検出する)。
var _ avatarUserLookup = (repository.UserRepository)(nil)
