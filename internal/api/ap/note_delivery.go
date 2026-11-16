package ap

import (
	"log/slog"
	"net/url"
	"regexp"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/core/notedelivery"
	"github.com/elythia-network/elythia/internal/misc/idnhost"
	"github.com/elythia-network/elythia/internal/model"
)

// fetchHostKey memoizes the host of the remote user who signed this request.
const fetchHostKey = "ap.fetchHost"

// SetNoteDeliveryRules wires the plugins that narrow which remote hosts may
// fetch local notes (#4199). nil で絞らない (初期状態)。
func (h *Handler) SetNoteDeliveryRules(r *notedelivery.Registry) {
	h.noteDelivery = r
}

// userAgentHostPatterns pick the requesting server's URL out of the User-Agent
// of an ActivityPub fetch. CherryPick の ActivityPubAccessControlService と同じ
// 並びで、先頭から順に試す。
var userAgentHostPatterns = []*regexp.Regexp{
	// Mastodon: "http.rb/5.1.1 (Mastodon/4.3.0; +https://remote.example/)"
	regexp.MustCompile(`(?i)http\.rb/[\d.]+\s+\(Mastodon/[\w.+-]+;\s+\+(https?://[^/)\s]+)`),
	// Pleroma / Akkoma: "Pleroma 2.5.0; https://remote.example <admin@...>"
	regexp.MustCompile(`(?i)Pleroma\s+[\w.+-]+;\s+(https?://[^/\s<]+)`),
	// Misskey とその派生、Elythia: "Misskey/2025.1.0 (https://remote.example)"
	regexp.MustCompile(`(?i)[\w-]+/[\w.+-]+\s+\((https?://[^/)\s]+)`),
	// それ以外: User-Agent のどこかに書かれた URL
	regexp.MustCompile(`(?i)(https?://[a-z0-9.-]+[a-z0-9](?::\d+)?)`),
}

// userAgentHost returns the host a User-Agent names, in the form remote users'
// hosts are stored (lower case, punycode, no default port), or "".
func userAgentHost(userAgent string) string {
	for _, re := range userAgentHostPatterns {
		m := re.FindStringSubmatch(userAgent)
		if m == nil {
			continue
		}
		u, err := url.Parse(m[1])
		if err != nil || u.Host == "" {
			return ""
		}
		return idnhost.HostPort(u)
	}
	return ""
}

// fetchHost returns the host of the server that is fetching, or "" when it
// cannot be told.
//
//  1. 署名を本体が持っている鍵で確かめられれば、署名した利用者のホスト
//     (FetchSignerAuthenticator、#3914)。署名した利用者が凍結・削除されているか、
//     こちらが配送しない相手 (signerHostAllowed) なら "" にする
//  2. 署名が無いか、確かめられなければ、User-Agent に書かれたホスト。Misskey と
//     Mastodon は自分の URL を User-Agent に入れる (CherryPick と同じ判定)
//
// **User-Agent は相手が自由に書ける。** 外したホストは User-Agent を書き換えれば
// 読め、許したホストを名乗れば include のノートも読める。利用者が信頼しない
// サーバーへ配送しないための機能で、照会の判定はその補助という位置付けなので、
// 既知の制約として受け入れている (#4199)。署名で確かめられた
// ときは、User-Agent を見ない。
//
// 自分自身を名乗る User-Agent は "" にする (自分のホストを指定する意味は無い)。
func (h *Handler) fetchHost(c echo.Context) string {
	if v, ok := c.Get(fetchHostKey).(string); ok {
		return v
	}
	host, signed := "", false
	req := c.Request()
	if h.fetchSigner != nil && req.Header.Get("Signature") != "" {
		signer, err := h.fetchSigner.AuthenticateFetch(req)
		switch {
		case err != nil:
			slog.Debug("ap: signed fetch not authenticated", "error", err)
		case signer == nil:
		case signer.IsSuspended || signer.IsDeleted || !h.signerHostAllowed(signer):
			// 確かめられたが見せない相手。User-Agent で名乗り直させない。
			signed = true
		default:
			host, signed = *signer.Host, true
		}
	}
	if !signed {
		host = userAgentHost(req.UserAgent())
		if h.localHost != "" && strings.EqualFold(host, h.localHost) {
			host = ""
		}
	}
	c.Set(fetchHostKey, host)
	return host
}

// hiddenByDeliveryRules returns the IDs of the local notes in notes that the
// requester may not fetch because a plugin narrowed their delivery.
//
// 絞られたノートだけが対象。相手が分からない照会は、include のノートには
// 見せず、exclude のノートには見せる (外したホストかどうかを決められないので、
// 利用者が選んだ側の既定に倒す)。署名を確かめるのは、絞られたノートがあった
// ときだけにする。リモートのノートは原本のサーバーへ転送するものなので見ない。
func (h *Handler) hiddenByDeliveryRules(c echo.Context, notes []*model.Note) map[string]bool {
	if h.noteDelivery.Empty() {
		return nil
	}
	ids := make([]string, 0, len(notes))
	for _, n := range notes {
		if n != nil && n.UserHost == nil {
			ids = append(ids, n.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rules := h.noteDelivery.Rules(c.Request().Context(), ids)
	var hidden map[string]bool
	for _, id := range ids {
		rule := rules[id]
		if !rule.Restricted() || rule.Allows(h.fetchHost(c)) {
			continue
		}
		if hidden == nil {
			hidden = make(map[string]bool)
		}
		hidden[id] = true
	}
	return hidden
}

// hiddenByDeliveryRule reports whether the requester may not fetch n.
func (h *Handler) hiddenByDeliveryRule(c echo.Context, n *model.Note) bool {
	return h.hiddenByDeliveryRules(c, []*model.Note{n})[n.ID]
}
