// Package remotecheck actively diagnoses federation with one remote host
// (#3055): whether WebFinger, nodeinfo, the actor and the inbox are reachable,
// whether our signed fetches are accepted, and what we have observed about
// the host's signature support and our deliveries to it.
//
// **通信は必ず SSRF-safe な client を通す。** 宛先を管理者が指定する口なので、
// 自ホスト向けの self-check (#2463) の client (ガード無し、宛先を config に固定
// することを安全性の前提にしている) を流用してはいけない。
package remotecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/deliveryhealth"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/core/selfcheck"
	"github.com/elythia-network/elythia/internal/model"
)

// maxBodyBytes bounds every response body the checks read.
const maxBodyBytes = 1 << 20

// DeliveryWindow is how far back the delivery check looks.
const DeliveryWindow = time.Hour

// maxNodeInfoLinks bounds how many nodeinfo documents are tried.
//
// **相手が返した links を全部は引かない。** 件数に上限が無いと、診断させた
// ホストが links を数千件返すだけで、こちらから第三者へ大量の GET が飛ぶ
// (実測で 1 回の診断が 3001 リクエストになった)。
const maxNodeInfoLinks = 4

// maxRedirects bounds redirects per request (net/http の既定は 10)。
const maxRedirects = 3

// localTimeout bounds the checks that only read this server's own state
// after the network checks (署名方式の観測・格下げ・ブレーカー・配送の集計)。
//
// **ネットワークの検査が終わってから数え始める。** 相手が応答しないと診断全体の
// 期限を使い切るので、同じ ctx で読むと自分の Redis / DB の読み取りまで期限切れで
// 失敗し、「ブレーカーが開いている」のような、相手が遅いときに一番要る事実を
// 落とす。一方で冒頭から数えると、ネットワークに localTimeout 以上かかっただけで
// 同じことが起きる (#3055 の敵対的レビュー 3 周目で実測)。待つ時間の上限は
// 診断全体の期限 + localTimeout。変数なのはテストで短くするため。
var localTimeout = 5 * time.Second

// instanceActorCandidates are tried through WebFinger when neither an account
// was given nor a user of the host is known. Misskey 系は `instance.actor`、
// Mastodon 系はドメイン名そのものをユーザー名に持つ。
var instanceActorCandidates = []string{"instance.actor", "%host%"}

// Fetcher fetches AP objects signed as our instance actor or unsigned.
type Fetcher interface {
	FetchObjectSignedOnly(uri string) ([]byte, error)
	FetchObjectUnsigned(uri string) ([]byte, error)
}

// Policy is how this server treats the host.
type Policy struct {
	// FederationDisabled は連合モードが `none` (全ホストと連合しない)。
	FederationDisabled bool
	// NotAllowed は連合モードが `specified` で、許可リストに入っていない。
	NotAllowed bool
	Blocked    bool
	Silenced   bool
	// Suspension は配送停止の状態 (`none` 以外なら止めている)。
	Suspension model.SuspensionState
}

// KnownUser is a remote user of the host we already have.
type KnownUser struct {
	Username string
	URI      string
}

// Deps wires the checks to the rest of the server. nil の関数は該当の検査を
// skip にする (未配線の構成でも他の検査は動かす)。
type Deps struct {
	// Client は SSRF-safe な client (nodeinfo / WebFinger / inbox に使う)。
	Client  *http.Client
	Fetcher Fetcher

	Policy          func(ctx context.Context, host string) (Policy, error)
	KnownUser       func(ctx context.Context, host string) (*KnownUser, error)
	Capability      func(ctx context.Context, host string) (*model.InstanceSignatureCapability, error)
	Ed25519Degraded func(ctx context.Context, host string) (bool, error)
	Delivery        func(ctx context.Context, host string) (*deliveryhealth.HostHealth, error)
	Breaker         func(ctx context.Context, host string) (*deliveryhealth.BreakerState, error)
}

// Request is what the admin asked to check.
type Request struct {
	// Host は正規化済み (punycode、小文字) のホスト名。
	Host string
	// Username は actor の検査に使うアカウント。空なら既知のユーザー、無ければ
	// インスタンス actor の候補を試す。
	Username string
}

// Report is the full run. Results は self-check と同じ形。
type Report struct {
	Host    string             `json:"host"`
	Results []selfcheck.Result `json:"results"`
	// OK は fail が 1 つも無いとき true (warn は数えない)。
	OK bool `json:"ok"`
}

// Run performs every check against req.Host.
func Run(ctx context.Context, deps Deps, req Request) Report {
	r := runner{deps: deps, host: req.Host, client: limitRedirects(deps.Client)}
	results := []selfcheck.Result{
		r.checkPolicy(ctx),
		r.checkNodeInfo(ctx),
	}
	wf, actorURL := r.checkWebFinger(ctx, req.Username)
	results = append(results, wf)
	actorRes, fetchRes, actor := r.checkActor(ctx, actorURL)
	results = append(results, actorRes, fetchRes, r.checkInbox(ctx, actor))
	local, cancel := context.WithTimeout(context.WithoutCancel(ctx), localTimeout)
	defer cancel()
	results = append(results,
		r.checkSignature(local, actor),
		r.checkDelivery(local),
	)
	ok := true
	for _, res := range results {
		if res.Status == selfcheck.StatusFail {
			ok = false
			break
		}
	}
	return Report{Host: req.Host, Results: results, OK: ok}
}

type runner struct {
	deps   Deps
	host   string
	client *http.Client
	// knownURI はこのホストの既知のユーザーの actor URI。WebFinger が失敗しても
	// actor の検査を続けられるように持っておく。
	knownURI string
}

func (r *runner) checkPolicy(ctx context.Context) selfcheck.Result {
	const name = "policy"
	if r.deps.Policy == nil {
		return skip(name, "未配線")
	}
	p, err := r.deps.Policy(ctx, r.host)
	if err != nil {
		return skip(name, "インスタンスの設定を読めない: "+err.Error())
	}
	switch {
	case p.FederationDisabled:
		return warn(name, "連合を無効にしている (federation: none)",
			"どのホストとも連合しない設定なので、相手はこのサーバーの actor も取得できない。下の結果の失敗はこのためで、相手の問題ではない")
	case p.NotAllowed:
		return warn(name, "連合を許可したホストに入っていない (federation: specified)",
			"許可リストに無いホストからの受信は拒否し、配送もしない。連合するなら許可リストに加える")
	case p.Blocked:
		return warn(name, "ブロックしている",
			"ブロック中は配送も受信もしない。連合できないのはこのためなので、意図したものか確認する")
	case p.Suspension != "" && p.Suspension != model.SuspensionStateNone:
		return warn(name, "配送を停止している ("+string(p.Suspension)+")",
			"停止中はこのホストへ配送しない。応答が無いための自動停止なら、相手の復旧を確かめてから連合の画面で再開する")
	case p.Silenced:
		return ok(name, "サイレンスしている (連合はする)")
	}
	return ok(name, "制限なし")
}

func (r *runner) checkNodeInfo(ctx context.Context) selfcheck.Result {
	const name = "nodeinfo"
	body, status, err := r.get(ctx, "https://"+r.host+"/.well-known/nodeinfo", "application/json")
	if err != nil {
		if res, out := r.timedOut(ctx, name); out {
			return res
		}
		return fail(name, "取得できない: "+err.Error(),
			"相手のサーバーに接続できない。落ちているか、DNS / TLS / 経路に問題がある")
	}
	if status != http.StatusOK {
		return fail(name, fmt.Sprintf("discovery が status %d", status),
			"相手が nodeinfo を配信していない。ActivityPub を話すサーバーでない可能性もある")
	}
	var disc struct {
		Links []struct {
			Href string `json:"href"`
		} `json:"links"`
	}
	if err := json.Unmarshal(body, &disc); err != nil || len(disc.Links) == 0 {
		return fail(name, "discovery の links が空", "相手の nodeinfo discovery の応答が壊れている")
	}
	// 最後の link が最新版を指す慣習。後ろから、**このホストを指すものだけ**を
	// maxNodeInfoLinks 件まで試す (別ホストを指す link は引かない)。
	tried := 0
	for i := len(disc.Links) - 1; i >= 0 && tried < maxNodeInfoLinks; i-- {
		if hostOf(disc.Links[i].Href) != r.host {
			continue
		}
		tried++
		docBody, docStatus, final, derr := r.getFinal(ctx, disc.Links[i].Href, "application/json")
		// redirect で別ホストへ移った先の情報を、このホストのものとして出さない。
		if derr != nil || docStatus != http.StatusOK || hostOf(final) != r.host {
			continue
		}
		var doc struct {
			Software struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"software"`
		}
		if json.Unmarshal(docBody, &doc) == nil && doc.Software.Name != "" {
			return ok(name, strings.TrimSpace(doc.Software.Name+" "+doc.Software.Version))
		}
	}
	if tried == 0 {
		return fail(name, "discovery の links にこのホストを指すものが無い",
			"相手の nodeinfo discovery が別ホストを指している。このサーバーも同じホストの nodeinfo しか読まないので、相手のソフトウェアを判別できない (アカウントのドメインと配信のドメインが別の構成なら、配信側のホストで診断する)")
	}
	return fail(name, "discovery が指す nodeinfo 本体を読めない", "相手の nodeinfo の配信が壊れている")
}

// checkWebFinger resolves an account and returns the actor URL it points to.
func (r *runner) checkWebFinger(ctx context.Context, username string) (selfcheck.Result, string) {
	const name = "webfinger"
	var candidates []string
	source := ""
	switch {
	case username != "":
		candidates, source = []string{username}, "指定したアカウント"
	default:
		note := ""
		if r.deps.KnownUser != nil {
			u, err := r.deps.KnownUser(ctx, r.host)
			switch {
			case err != nil:
				// 読めなかったことを黙って「知らない」にしない。
				note = " (既知のユーザーを読めない: " + err.Error() + ")"
			case u != nil:
				candidates, source = []string{u.Username}, "既知のユーザー"
				r.knownURI = u.URI
			}
		}
		if len(candidates) == 0 {
			source = "インスタンス actor" + note
			for _, c := range instanceActorCandidates {
				candidates = append(candidates, strings.ReplaceAll(c, "%host%", r.host))
			}
		}
	}

	var last string
	for _, u := range candidates {
		resource := "acct:" + u + "@" + r.host
		target := "https://" + r.host + "/.well-known/webfinger?resource=" + url.QueryEscape(resource)
		body, status, err := r.get(ctx, target, "application/jrd+json, application/json")
		if err != nil {
			if res, out := r.timedOut(ctx, name); out {
				return res, r.knownURI
			}
			return fail(name, "取得できない: "+err.Error(),
				"相手のサーバーに接続できない。nodeinfo も失敗していれば相手が落ちている"), r.knownURI
		}
		if status != http.StatusOK {
			last = fmt.Sprintf("%s: status %d", resource, status)
			continue
		}
		var jrd struct {
			Links []struct {
				Rel  string `json:"rel"`
				Type string `json:"type"`
				Href string `json:"href"`
			} `json:"links"`
		}
		if err := json.Unmarshal(body, &jrd); err != nil {
			last = resource + ": JSON として読めない"
			continue
		}
		for _, l := range jrd.Links {
			if l.Rel == "self" && (strings.Contains(l.Type, "activity+json") || strings.Contains(l.Type, "ld+json")) && l.Href != "" {
				return ok(name, source+" "+resource+" → "+l.Href), l.Href
			}
		}
		last = resource + ": self link (activity+json) が無い"
	}
	hint := "アカウント名を指定して試す。相手が WebFinger を配信していないと、その相手のユーザーをこちらから検索・メンションできない"
	if strings.HasPrefix(source, "インスタンス actor") {
		hint = "このホストのユーザーをまだ知らないため、インスタンス actor の候補だけを試した。相手のアカウント名を指定して試す"
	}
	return fail(name, last, hint), r.knownURI
}

// actorDoc is the part of an actor the checks read.
type actorDoc struct {
	ID          string
	Inbox       string
	SharedInbox string
	// HasPublicKey は publicKey (単体でも配列でも) に PEM があるか。
	HasPublicKey bool
	// DeclaresEd25519 は assertionMethod に Multikey の鍵があるか。
	DeclaresEd25519 bool
}

// parseActor reads an actor leniently.
//
// **publicKey / assertionMethod は単体でも配列でも受け、要素ごとに読む。** JSON-LD
// では単体の値と 1 要素の配列が同じ意味で、配列には参照 (IRI の文字列) も混ざり
// うる。形を決め打ちして全体を Unmarshal すると、読める鍵があるのに actor ごと
// 「壊れている」と誤って診断する。
func parseActor(body []byte) (*actorDoc, error) {
	var raw struct {
		ID        string `json:"id"`
		Inbox     string `json:"inbox"`
		Endpoints struct {
			SharedInbox string `json:"sharedInbox"`
		} `json:"endpoints"`
		PublicKey       json.RawMessage          `json:"publicKey"`
		AssertionMethod activitypub.MultikeyList `json:"assertionMethod"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	a := &actorDoc{ID: raw.ID, Inbox: raw.Inbox, SharedInbox: raw.Endpoints.SharedInbox}
	for _, el := range elements(raw.PublicKey) {
		var k struct {
			PublicKeyPem string `json:"publicKeyPem"`
		}
		if json.Unmarshal(el, &k) == nil && k.PublicKeyPem != "" {
			a.HasPublicKey = true
		}
	}
	// resolver が Ed25519 鍵として採るのと同じ条件 (Multikey で、鍵の id が actor と
	// 同じホスト (id が無ければ一致しないので外れる)、値が Ed25519 の鍵として読める)。
	// 列の長さの検査だけは見ない (resolver の内部事情で、相手の宣言の有無ではない)。条件が緩いと、resolver が採らない鍵を見て
	// 「Ed25519 で送る」と誤って表示する。
	actorHost := hostOf(raw.ID)
	for _, m := range raw.AssertionMethod.Keys {
		if m.Type != activitypub.MultikeyType || actorHost == "" || hostOf(m.ID) != actorHost {
			continue
		}
		if _, err := activitypub.DecodeEd25519Multikey(m.PublicKeyMultibase); err == nil {
			a.DeclaresEd25519 = true
		}
	}
	return a, nil
}

// elements splits a JSON value that may be a single value or an array.
func elements(raw json.RawMessage) []json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var many []json.RawMessage
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	return []json.RawMessage{raw}
}

// checkActor fetches the actor signed and unsigned. Returns the actor check,
// the authorized-fetch check and the parsed actor (nil when unreadable).
func (r *runner) checkActor(ctx context.Context, actorURL string) (selfcheck.Result, selfcheck.Result, *actorDoc) {
	const name, fetchName = "actor", "authorized-fetch"
	if res, out := r.timedOut(ctx, name); out {
		fres, _ := r.timedOut(ctx, fetchName)
		return res, fres, nil
	}
	if actorURL == "" {
		return skip(name, "WebFinger で actor が見つからないため実行できない"),
			skip(fetchName, "actor が無いため実行できない"), nil
	}
	if r.deps.Fetcher == nil {
		return skip(name, "未配線"), skip(fetchName, "未配線"), nil
	}
	// 相手が指した URL をそのまま引く。宛先の検査は SSRF-safe transport が持つ。
	signedBody, signedErr := withContext(ctx, func() ([]byte, error) { return r.deps.Fetcher.FetchObjectSignedOnly(actorURL) })
	// 期限を過ぎていたら次の取得を始めない (相手へ余計なリクエストを出さない)。
	var unsignedBody []byte
	unsignedErr := ctx.Err()
	if unsignedErr == nil {
		unsignedBody, unsignedErr = withContext(ctx, func() ([]byte, error) { return r.deps.Fetcher.FetchObjectUnsigned(actorURL) })
	}

	var fetchRes selfcheck.Result
	switch {
	case ctx.Err() != nil && signedErr == nil:
		// 署名付きで読めた actor は検査する。突き合わせだけが時間切れ。
		fetchRes, _ = r.timedOut(ctx, fetchName)
		unsignedErr = nil
	case ctx.Err() != nil:
		// 時間切れで失敗したものを、相手の拒否や障害として読ませない。
		res, _ := r.timedOut(ctx, name)
		fres, _ := r.timedOut(ctx, fetchName)
		return res, fres, nil
	default:
		fetchRes = authorizedFetchResult(fetchName, signedErr, unsignedErr)
	}

	body := signedBody
	if signedErr != nil {
		body = unsignedBody
	}
	if signedErr != nil && unsignedErr != nil {
		err := signedErr
		if errors.Is(err, federation.ErrNoSigner) {
			err = unsignedErr
		}
		return fail(name, actorURL+": "+err.Error(),
			"actor を取得できない。相手が落ちているか、こちらを拒否している (下の authorized-fetch を見る)"), fetchRes, nil
	}
	actor, err := parseActor(body)
	if err != nil {
		return fail(name, "JSON として読めない", "相手の actor の応答が壊れている"), fetchRes, nil
	}
	if !actor.HasPublicKey {
		return fail(name, "publicKey が無い",
			"相手の署名を検証できないので、相手からの配送を受け付けられない"), fetchRes, actor
	}
	if actor.Inbox == "" {
		return fail(name, "inbox が無い", "相手へ配送できない"), fetchRes, actor
	}
	if idHost := hostOf(actor.ID); idHost != "" && idHost != r.host {
		return warn(name, fmt.Sprintf("actor.id の host が %q", idHost),
			"WebFinger と actor が別ホストで配信されている。意図した構成なら問題ない"), fetchRes, actor
	}
	return ok(name, actor.ID), fetchRes, actor
}

// withContext runs fetch but stops waiting when ctx is done.
//
// **AP の取得は ctx を受け取らない** (client の timeout だけで切れる) ので、
// 診断全体の期限を越えて待たないよう、待つのをやめて ctx のエラーを返す。
// 取得そのものは client の timeout で必ず終わる (goroutine は残り続けない)。
func withContext(ctx context.Context, fetch func() ([]byte, error)) ([]byte, error) {
	type result struct {
		body []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := fetch()
		ch <- result{b, err}
	}()
	select {
	case res := <-ch:
		return res.body, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// timedOut reports a skip when the whole diagnosis ran out of time.
// 時間切れを相手の障害として読ませないため、失敗ではなく「確かめられず」にする。
func (r *runner) timedOut(ctx context.Context, name string) (selfcheck.Result, bool) {
	if ctx.Err() == nil {
		return selfcheck.Result{}, false
	}
	return skip(name, "診断の制限時間を過ぎたため確かめられなかった (相手の応答が遅い)"), true
}

// limitRedirects returns a copy of c that follows at most maxRedirects
// redirects. nil はそのまま返す。
func limitRedirects(c *http.Client) *http.Client {
	if c == nil {
		return nil
	}
	cp := *c
	cp.CheckRedirect = func(_ *http.Request, via []*http.Request) error {
		if len(via) > maxRedirects {
			return fmt.Errorf("redirect が %d 回を超えた", maxRedirects)
		}
		return nil
	}
	return &cp
}

// authorizedFetchResult compares a signed and an unsigned fetch of the actor.
//
// **こちらの署名が拒否されているかを切り分けるのが目的。** 相手が署名付きの
// 取得を必須にしている (Mastodon の secure mode 等) 構成では、署名が拒否されると
// 相手のユーザーやノートを一切取得できない。署名なしで読めるなら、拒否の原因は
// 経路ではなく署名 (こちらの鍵を相手が取得できない、古い鍵を持っている) にある。
func authorizedFetchResult(name string, signedErr, unsignedErr error) selfcheck.Result {
	if errors.Is(signedErr, federation.ErrNoSigner) {
		return skip(name, "インスタンス actor の鍵が未作成のため署名付きの取得を試せない")
	}
	signedDenied, unsignedDenied := denied(signedErr), denied(unsignedErr)
	switch {
	case signedErr == nil && unsignedErr == nil:
		return ok(name, "署名なしでも取得できる")
	case signedErr == nil && unsignedDenied:
		return ok(name, "署名付きの取得を必須にしている。こちらの署名は受理された")
	case signedErr == nil:
		return ok(name, "署名付きで取得できた (署名なしは "+unsignedErr.Error()+")")
	case !signedDenied && unsignedErr == nil:
		return warn(name, "署名付きの取得に失敗した ("+signedErr.Error()+")。署名なしなら取得できる",
			"相手の拒否ではない。こちらの署名鍵を読めないか、一時的な通信の失敗。もう一度診断する")
	case signedDenied && unsignedErr == nil:
		return fail(name, "署名付きだと拒否され ("+signedErr.Error()+")、署名なしなら取得できる",
			"相手がこちらの署名を検証できていない。相手からこちらのインスタンス actor が取得できるか (こちらの self-check)、相手が古い鍵を持っていないかを確かめる")
	case signedDenied && unsignedDenied:
		return fail(name, "署名付きでも署名なしでも拒否された",
			"相手がこのサーバーを拒否している (ブロック・許可制の連合) か、署名付きの取得を必須にしたうえでこちらの署名を検証できていない")
	}
	return fail(name, "署名付き: "+signedErr.Error(), "actor の取得そのものが失敗している。上の actor の結果を見る")
}

func denied(err error) bool {
	var se *activitypub.StatusError
	return errors.As(err, &se) && (se.StatusCode == http.StatusUnauthorized || se.StatusCode == http.StatusForbidden)
}

// checkInbox confirms the inbox answers over HTTP. **投函はしない。** GET を
// 送り、何らかの HTTP 応答が返れば到達できると見なす (inbox は POST 専用なので
// 404 / 405 が普通)。
func (r *runner) checkInbox(ctx context.Context, actor *actorDoc) selfcheck.Result {
	const name = "inbox"
	if res, out := r.timedOut(ctx, name); out {
		return res
	}
	if actor == nil || actor.Inbox == "" {
		return skip(name, "actor が読めないため実行できない")
	}
	target := actor.Inbox
	if actor.SharedInbox != "" {
		target = actor.SharedInbox
	}
	_, status, err := r.get(ctx, target, "application/activity+json")
	if err != nil {
		if res, out := r.timedOut(ctx, name); out {
			return res
		}
		return fail(name, target+": 到達できない: "+err.Error(),
			"配送先に接続できない。actor は取得できて inbox だけ届かないなら、相手の inbox が別ホストや別経路にある")
	}
	if status >= 500 {
		return warn(name, fmt.Sprintf("%s: status %d", target, status),
			"接続はできるが、相手がエラーを返している。配送も同じく失敗している可能性がある (下の delivery を見る)")
	}
	return ok(name, fmt.Sprintf("%s: 到達できる (GET に status %d)", target, status))
}

// checkSignature reports what we know about the host's signature support and
// which method deliveries would use.
func (r *runner) checkSignature(ctx context.Context, actor *actorDoc) selfcheck.Result {
	const name = "signature"
	var parts []string
	declared := actor != nil && actor.DeclaresEd25519
	if declared {
		parts = append(parts, "今回の actor に Ed25519 の宣言あり")
	}
	var capability *model.InstanceSignatureCapability
	if r.deps.Capability != nil {
		c, err := r.deps.Capability(ctx, r.host)
		if err != nil {
			return skip(name, "観測を読めない: "+err.Error())
		}
		capability = c
	}
	if capability != nil {
		if capability.Ed25519DeclaredAt != nil {
			parts = append(parts, "Ed25519 の宣言を観測 ("+formatTime(*capability.Ed25519DeclaredAt)+")")
		}
		if capability.Ed25519AcceptedAt != nil {
			parts = append(parts, "Ed25519 で送った配送が受理された ("+formatTime(*capability.Ed25519AcceptedAt)+")")
		}
		if capability.InboundAlg != nil {
			parts = append(parts, "相手からの受信は "+*capability.InboundAlg)
		}
	}
	degraded := false
	if r.deps.Ed25519Degraded != nil {
		d, err := r.deps.Ed25519Degraded(ctx, r.host)
		if err != nil {
			// 読めないときに「落としていない」と断定しない。
			parts = append(parts, "Ed25519 を一時的に落としているかは読めない ("+err.Error()+")")
			return warn(name, strings.Join(parts, " / "),
				"Redis に接続できるか確認する。配送が Ed25519 と RSA のどちらで署名されているかは、この状態では分からない")
		}
		degraded = d
	}
	if degraded {
		parts = append(parts, "こちらが使う方式: RSA (Ed25519 が拒否されたため一時的に落としている)")
		return warn(name, strings.Join(parts, " / "),
			"Ed25519 で署名した配送が 4xx で拒否された。RSA で送り直しているので配送は続く。しばらくすると自動で Ed25519 を試し直す")
	}
	if declared || capability.SupportsEd25519() {
		parts = append(parts, "こちらが使う方式: Ed25519 (相手の Ed25519 鍵が分かっている配送)、それ以外は RSA")
	} else {
		parts = append(parts, "こちらが使う方式: RSA")
	}
	return ok(name, strings.Join(parts, " / "))
}

func (r *runner) checkDelivery(ctx context.Context) selfcheck.Result {
	const name = "delivery"
	if r.deps.Breaker != nil {
		b, err := r.deps.Breaker(ctx, r.host)
		if err != nil {
			return warn(name, "配送を止めているかを読めない: "+err.Error(),
				"Redis に接続できるか確認する。この状態では、ブレーカーが開いているかも直近の配送結果も分からない")
		}
		if b != nil {
			if b.Open {
				return fail(name, fmt.Sprintf("配送を止めている (連続 %d 回失敗)", b.ConsecutiveFailures),
					"相手が接続失敗か 5xx を返し続けている。ときどき 1 件だけ試しており、応答があれば自動で再開する。連合の状態の画面から手で再開もできる")
			}
			if b.ThrottledUntil != nil || b.ReservedUntil != nil {
				return warn(name, "429 を受けて配送の間隔を空けている",
					"相手がこちらからの配送を速すぎると言っている。待たせた配送は間隔を空けて送る")
			}
		}
	}
	if r.deps.Delivery == nil {
		return skip(name, "未配線")
	}
	h, err := r.deps.Delivery(ctx, r.host)
	if err != nil {
		return skip(name, "配送の集計を読めない: "+err.Error())
	}
	if h == nil || h.Success+h.Failure == 0 {
		return skip(name, "直近 1 時間の配送なし")
	}
	detail := fmt.Sprintf("直近 1 時間: 成功 %d / 失敗 %d", h.Success, h.Failure)
	if h.LastError != nil {
		detail += " / 直近のエラー: " + h.LastError.Message
	}
	switch {
	case h.Failure > 0 && h.Success == 0:
		return fail(name, detail, "配送が 1 件も成功していない。上の inbox と authorized-fetch の結果とあわせて見る")
	case h.Failure > 0:
		return warn(name, detail, "一部の配送が失敗している。一時的なものなら再試行で届く")
	}
	return ok(name, detail)
}

// get performs one SSRF-safe GET.
func (r *runner) get(ctx context.Context, target, accept string) ([]byte, int, error) {
	body, status, _, err := r.getFinal(ctx, target, accept)
	return body, status, err
}

// getFinal is get that also returns the URL that finally answered (after
// redirects).
func (r *runner) getFinal(ctx context.Context, target, accept string) ([]byte, int, string, error) {
	if r.client == nil {
		return nil, 0, "", errors.New("client が未配線")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, "", err
	}
	req.Header.Set("Accept", accept)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	final := target
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	if err != nil {
		return nil, resp.StatusCode, final, err
	}
	return body, resp.StatusCode, final, nil
}

// hostOf normalizes a URL's host the way the host under test was normalized
// (小文字化・punycode 化・既定ポートの除去)。揃えないと同じホストを別ホストと
// 判定する。
func hostOf(raw string) string {
	return federation.NormalizeGateHost(raw)
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func ok(name, detail string) selfcheck.Result {
	return selfcheck.Result{Name: name, Status: selfcheck.StatusOK, Detail: detail}
}

func fail(name, detail, hint string) selfcheck.Result {
	return selfcheck.Result{Name: name, Status: selfcheck.StatusFail, Detail: detail, Hint: hint}
}

func warn(name, detail, hint string) selfcheck.Result {
	return selfcheck.Result{Name: name, Status: selfcheck.StatusWarn, Detail: detail, Hint: hint}
}

func skip(name, detail string) selfcheck.Result {
	return selfcheck.Result{Name: name, Status: selfcheck.StatusSkip, Detail: detail}
}
