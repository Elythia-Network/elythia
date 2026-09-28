package admin

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/core/federation"
	"github.com/shiroha-a/mk/internal/core/remotecheck"
	"github.com/shiroha-a/mk/internal/core/selfcheck"
)

// remoteCheckTimeout bounds one diagnosis. 検査は 8 つで、相手が応答しない
// ときに管理画面を待たせ続けない。
const remoteCheckTimeout = 60 * time.Second

// RemoteChecker diagnoses federation with one remote host (#3055).
// 実装は server の adapter (SSRF-safe な client と観測を束ねる)。
type RemoteChecker interface {
	CheckRemoteHost(ctx context.Context, req remotecheck.Request) remotecheck.Report
}

// SetRemoteChecker wires the remote diagnosis. selfHost is this server's
// host (normalized like the request), which is refused.
func (h *Handler) SetRemoteChecker(r RemoteChecker, selfHost string) {
	h.remoteCheck = r
	h.remoteCheckSelfHost = selfHost
}

// FederationCheckHost handles POST /api/admin/federation/check-host.
//
// **mk-go 独自 endpoint** (#3055)。upstream に対応物は無い。管理者が指定した
// ホストとの疎通を 1 回だけ能動的に検査する。
//
// **自ホストは断る。** 自ホストの検査 (admin/self-check) は、自分が loopback /
// private IP に解決されうるため SSRF ガードを通さない client を使い、宛先を
// config に固定することを安全性の前提にしている。こちらは宛先を外から受け取る
// ので、あちらの経路へ分岐させない (入力によってガードの無い経路に入る口になる)。
func (h *Handler) FederationCheckHost(c echo.Context) error {
	var req struct {
		Host    string `json:"host"`
		Account string `json:"account"`
	}
	_ = c.Bind(&req)
	host := normalizeRequestHost(req.Host)
	if host == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "host is required.", "345fdd87-c6e3-46ad-b768-1c08eebd726e"))
	}
	if h.remoteCheckSelfHost != "" && host == h.remoteCheckSelfHost {
		return c.JSON(http.StatusBadRequest, apierr.Error("CANNOT_CHECK_SELF", "Use admin/self-check to check this server.", "ff8a5980-5b4b-4d8d-90a5-25be0751f1f9"))
	}
	username, ok := parseCheckAccount(req.Account, host)
	if !ok {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_ACCOUNT", "account must be a username of the host (user or user@host).", "d5b1a8fa-e904-489b-acfa-2433663ca1cb"))
	}
	if h.remoteCheck == nil {
		return c.JSON(http.StatusOK, remotecheck.Report{Host: host, Results: []selfcheck.Result{}, OK: true})
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), remoteCheckTimeout)
	defer cancel()
	return c.JSON(http.StatusOK, h.remoteCheck.CheckRemoteHost(ctx, remotecheck.Request{Host: host, Username: username}))
}

// normalizeRequestHost normalizes a host (or URL) given by an admin the way
// the delivery side keys hosts. 空なら "" を返す。
func normalizeRequestHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	// **末尾のドットを落とす。** `example.com.` は DNS 上は同じホストだが、
	// 正規化は残すので、配送・ブレーカー・既知ユーザーのキーと一致しなくなり
	// (診断が「配送なし」「WebFinger 404」と誤る)、自ホストの判定もすり抜ける。
	if u, err := url.Parse(raw); err == nil {
		if h := strings.TrimRight(u.Hostname(), "."); h != u.Hostname() {
			if p := u.Port(); p != "" {
				u.Host = net.JoinHostPort(h, p)
			} else {
				u.Host = h
			}
			raw = u.String()
		}
	}
	return federation.NormalizeGateHost(raw)
}

// parseCheckAccount accepts "", "user", "@user" or "user@host" / "@user@host"
// and returns the username. host 部分があれば対象のホストと一致しなければならない
// (別ホストのアカウントを渡すと、診断対象と違う相手を検査して結果を誤読させる)。
func parseCheckAccount(raw, host string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", true
	}
	raw = strings.TrimPrefix(raw, "@")
	username := raw
	if i := strings.Index(raw, "@"); i >= 0 {
		username = raw[:i]
		if normalizeRequestHost(raw[i+1:]) != host {
			return "", false
		}
	}
	if username == "" || len(username) > 128 || strings.ContainsAny(username, "@/?#% \t\r\n\x00") {
		return "", false
	}
	return username, true
}
