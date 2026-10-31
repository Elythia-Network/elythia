package entitycompat

import (
	"sort"
	"strings"
	"testing"
)

// TestSecureDrift checks that every Misskey `secure: true` endpoint that mk-go
// implements applies middleware.RequireSecure on its route. Misskey's secure
// endpoints (password / 2FA / data export-import / authorized-apps) are
// reachable only via the native session token; without RequireSecure a
// third-party OAuth app holding a valid access token could drive them.
func TestSecureDrift(t *testing.T) {
	// 本家の secure は upstream_paramdefs.json の meta から作る (upstreamMetas)。
	golden := upstreamSecureEndpoints(upstreamMetas(t))
	secure := make(map[string]bool, len(golden))
	for _, ep := range golden {
		secure[ep] = true
	}
	// 拾えなくなったら落とす。件数の下限に加えて、secure の endpoint を名指しで要求する。
	if len(golden) < 30 || !secure["i/change-password"] || !secure["i/export-notes"] {
		t.Fatalf("upstream secure endpoints look broken (%d endpoints, without i/change-password or i/export-notes); regenerate with `make paramdef-gen`", len(golden))
	}

	regs := parseRouteRegistrations(t, serverSourcePaths(t)...)

	var missing []string
	for ep, reg := range regs {
		if !secure[ep] || excludedEndpoint(ep) {
			continue
		}
		if !strings.Contains(reg, "RequireSecure") {
			missing = append(missing, ep)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		var b strings.Builder
		b.WriteString("secure drift: Misskey `secure: true` endpoint lacks middleware.RequireSecure (app token could drive it).\n")
		for _, ep := range missing {
			b.WriteString("  " + ep + "\n")
		}
		t.Fatal(b.String())
	}

	// **逆向きも見る。** golden にない endpoint に RequireSecure が付いていると、
	// upstream ではサードパーティアプリから叩けるものが mk-go だけ 403 になる。
	// 片方向のままだと、upstream が secure を外したとき (2026.9.0 の
	// `i/revoke-token` がまさにそれ) に golden から消えるだけで、mk-go 側に
	// RequireSecure が残っていても緑のまま通る = 機能が丸ごと死んでいても
	// 検出できない。
	//
	// mk-go が意図的に upstream より厳しくする場合は、理由を添えて
	// secureStricterThanUpstream に登録すること。
	var extra []string
	for ep, reg := range regs {
		if secure[ep] || excludedEndpoint(ep) || secureStricterThanUpstream[ep] != "" {
			continue
		}
		if strings.Contains(reg, "RequireSecure") {
			extra = append(extra, ep)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		var b strings.Builder
		b.WriteString("secure drift: mk-go applies RequireSecure to an endpoint Misskey does NOT mark `secure: true`.\n")
		b.WriteString("third-party apps can drive it upstream but get 403 here. Remove it, or register the reason in secureStricterThanUpstream.\n")
		for _, ep := range extra {
			b.WriteString("  " + ep + "\n")
		}
		t.Fatal(b.String())
	}
}

// secureStricterThanUpstream lists endpoints where mk-go deliberately requires a
// native session token even though Misskey does not mark them `secure: true`.
// The value is the reason. Add an entry only with a documented reason
// (docs/divergence.md).
var secureStricterThanUpstream = map[string]string{
	// DB のバックアップの管理画面 (#3462、Elythia 独自で本家に無い)。バックアップには
	// 利用者の秘密鍵・token・パスワードの hash が入るので、アプリの token では
	// 使えないようにする (docs/divergence/api.md)。
	"admin/backup/delete":           backupSecureReason,
	"admin/backup/download":         backupSecureReason,
	"admin/backup/list":             backupSecureReason,
	"admin/backup/reauth-challenge": backupSecureReason,
	"admin/backup/take":             backupSecureReason,
	"admin/backup/verify":           backupSecureReason,
	// 画面からのリストア (#3463 (6c))。DB を入れ替える操作なので同じ扱い。
	"admin/backup/restore/abort":   backupSecureReason,
	"admin/backup/restore/confirm": backupSecureReason,
	"admin/backup/restore/prepare": backupSecureReason,
	// 退避した DB の一覧 (#3463 (6d))。一覧 (admin/backup/list) と同じ扱い。
	"admin/backup/restore/candidates": backupSecureReason,
	// 外部公開や既存投稿のlocalOnly化を伴うアカウント自身の連合状態変更。
	"i/update-federation": federationStateSecureReason,
}

const backupSecureReason = "Elythia-only backup admin (#3462): backups hold private keys, tokens and password hashes, so app tokens must not reach them"

const federationStateSecureReason = "Elythia-only federation state change (#3852): publishing an account or making its existing posts local-only is consequential and can be irreversible, so only the owner's native session may change it (docs/divergence/api.md)"

// parseRouteRegistrations returns endpoint path -> the full router registration
// text (balanced parens), so middleware on a multi-line inline handler's closing
// line is included.
//
// **コメントを先に落とす (#3037 レビュー 3 周目)。** 深さ数えは文字列にも
// コメントにも入るので、引数の途中に**括弧を含むコメントを 1 行足すだけ**で
// 対応が崩れ、後続の route を丸ごと飲み込む。飲み込まれた route の
// middleware が手前の判定に混ざるので、
//
//   - app-token gate: `RejectAppToken()` を外した route が、後続 route の
//     文字列に紛れて「付いている」と判定される
//   - privileged policy gate: 後続の `RequireModerator` が混ざって
//     「ロールで守られている」と読まれ、検査対象から外れる
//
// の 2 形が実測で素通りした。**2 周目はこのパーサを「自前の行畳みと違って
// 文字列・コメントを数えない」と書いて採用したが、事実と逆だった。**
// `stripGoComments` は rune / 文字列リテラルを保つので、route の path に
// 含まれる `(` が消えることはない。
//
// **ファイルごとに読む (#3672)。** offset はファイルの中でだけ意味を持つ。
// 同じ endpoint を 2 つのファイルが登録していたら落とす (routeRegistrationsOf)。
func parseRouteRegistrations(t *testing.T, paths ...string) map[string]string {
	t.Helper()
	return routeRegistrationsOf(t, "parseRouteRegistrations", paths)
}
