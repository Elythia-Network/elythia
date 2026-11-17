package ap

import (
	"log/slog"
	"net/http"
	"net/url"
	"regexp"

	"github.com/labstack/echo/v5"

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

// fetchSignerKey memoizes the result of checking this request's signature.
const fetchSignerKey = "ap.fetchSigner"

type fetchSignerResult struct {
	user *model.User
	err  error
}

// authenticateFetch checks the request's signature once per request.
//
// 見せてよいかの判定 (noteForSigner) と、配送先の絞り込みの判定 (fetchHost) が
// 同じ要求で両方とも署名を確かめるので、結果を共有する (#4199)。呼ぶ側が
// fetchSigner と Signature ヘッダーの有無を確かめておく。
func (h *Handler) authenticateFetch(c *echo.Context) (*model.User, error) {
	if v, ok := c.Get(fetchSignerKey).(fetchSignerResult); ok {
		return v.user, v.err
	}
	user, err := h.fetchSigner.AuthenticateFetch(c.Request())
	c.Set(fetchSignerKey, fetchSignerResult{user: user, err: err})
	return user, err
}

// fetchRequester is who fetched, as fetchHost tells it.
type fetchRequester struct {
	host     string
	rejected bool
}

// fetchHost returns the host of the server that is fetching, or "" when it
// cannot be told. rejected is true when the signature verified but the
// signer is one this server does not serve.
//
//  1. 署名を本体が持っている鍵で確かめられれば、署名した利用者のホスト
//     (FetchSignerAuthenticator、#3914)。署名した利用者が凍結・削除されているか、
//     こちらが配送しない相手 (signerHostAllowed) なら rejected にする。"" を返すだけ
//     だと、exclude のノートは「相手が分からない」として見せてしまい、外したホストが
//     署名を付けるだけで照会できる
//  2. 署名が無いか、確かめられなければ、User-Agent に書かれたホスト。Misskey と
//     Mastodon は自分の URL を User-Agent に入れる (CherryPick と同じ判定)
//
// **User-Agent は相手が自由に書ける。** 外したホストは User-Agent を書き換えれば
// 読め、許したホストを名乗れば include のノートも読める。利用者が信頼しない
// サーバーへ配送しないための機能で、照会の判定はその補助という位置付けなので、
// 既知の制約として受け入れている (#4199)。署名で確かめられた
// ときは、User-Agent を見ない。
//
// 自分のホストを名乗る User-Agent も、そのまま返す。分からない相手 ("") は、判定で
// 匿名の閲覧者として自分のホストに置き換える (hiddenByDeliveryRules、#4203) ので、
// どちらも自分のホストとして判定される。自分のホストは配送先の条件のレジストリが
// 持つ1つだけを使い、ここでは比べない (綴りの揃え方を2か所に持たないため)。
func (h *Handler) fetchHost(c *echo.Context) (host string, rejected bool) {
	if v, ok := c.Get(fetchHostKey).(fetchRequester); ok {
		return v.host, v.rejected
	}
	signed := false
	req := c.Request()
	if h.fetchSigner != nil && req.Header.Get("Signature") != "" {
		signer, err := h.authenticateFetch(c)
		switch {
		case err != nil:
			slog.Debug("ap: signed fetch not authenticated", "error", err)
		case signer == nil:
		case signer.IsSuspended || signer.IsDeleted || !h.signerHostAllowed(signer):
			// 確かめられたが見せない相手。User-Agent で名乗り直させない。
			signed, rejected = true, true
		default:
			host, signed = *signer.Host, true
		}
	}
	if !signed {
		host = userAgentHost(req.UserAgent())
	}
	c.Set(fetchHostKey, fetchRequester{host: host, rejected: rejected})
	return host, rejected
}

// hiddenByDeliveryRules returns the IDs of the local notes in notes that the
// requester may not fetch because a plugin narrowed their delivery.
// unavailable is true when a plugin could not report the targets of any of
// them; the caller answers 503 then (#4202).
//
// 絞られたノートだけが対象。相手が分からない照会は、ログインしていない閲覧者と
// 同じく、自分のホストからの照会として判定する (#4203)。自分のホストを条件に
// 入れていなければ、include のノートには見せず、exclude のノートには見せる
// (外したホストかどうかを決められないので、利用者が選んだ側の既定に倒す)。
// **匿名の Web と同じ相手として扱う。** 分けると、exclude に自分のホストを入れた
// ノートが、Web (notes/show・埋め込み・フィード) では伏せられ、同じ URL を
// activity+json で署名も User-Agent も無しに取ると本文ごと読める。
//
// 署名を確かめたうえで断った相手には、絞られたノートを1つも見せない。署名を
// 確かめるのは、絞られたノートがあったときだけにする。リモートのノートは原本の
// サーバーへ転送するものなので見ない。
//
// **条件を読めないときは、404ではなく503にさせる。** 404を「消えた」と扱う相手
// では、障害が直っても戻らない。プラグインの障害は、そのとき渡したノートの
// 全部 (条件を付けていないものも含む) に同じく出るので、503にしても、どの
// ノートを絞っているかは漏れない。
func (h *Handler) hiddenByDeliveryRules(c *echo.Context, notes []*model.Note) (hidden map[string]bool, unavailable bool) {
	if h.noteDelivery.Empty() {
		return nil, false
	}
	ids := make([]string, 0, len(notes))
	for _, n := range notes {
		if n != nil && n.UserHost == nil {
			ids = append(ids, n.ID)
		}
	}
	if len(ids) == 0 {
		return nil, false
	}
	rules := h.noteDelivery.Rules(c.Request().Context(), ids)
	for _, id := range ids {
		rule := rules[id]
		if rule.Unavailable() {
			return nil, true
		}
		if !rule.Restricted() {
			continue
		}
		host, rejected := h.fetchHost(c)
		if host == "" {
			host = h.noteDelivery.LocalHost()
		}
		if !rejected && rule.Allows(host) {
			continue
		}
		if hidden == nil {
			hidden = make(map[string]bool)
		}
		hidden[id] = true
	}
	return hidden, false
}

// deliveryRuleStatus returns the status to answer instead of n, or 0 when the
// requester may fetch it: 404 when a plugin narrowed n away from the
// requester, 503 when the targets could not be read.
func (h *Handler) deliveryRuleStatus(c *echo.Context, n *model.Note) int {
	hidden, unavailable := h.hiddenByDeliveryRules(c, []*model.Note{n})
	switch {
	case unavailable:
		return http.StatusServiceUnavailable
	case hidden[n.ID]:
		return http.StatusNotFound
	}
	return 0
}
