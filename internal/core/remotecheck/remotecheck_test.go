package remotecheck

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/deliveryhealth"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/core/selfcheck"
	"github.com/elythia-network/elythia/internal/model"
)

// remote is a fake remote server. webfinger は resource ごとの応答を持つ。
type remote struct {
	srv       *httptest.Server
	host      string
	webfinger map[string]int // resource -> status (200 なら self link を返す)
	nodeinfo  int
	inbox     int
	gotInbox  string
}

func newRemote(t *testing.T) *remote {
	t.Helper()
	r := &remote{webfinger: map[string]int{}, nodeinfo: http.StatusOK, inbox: http.StatusMethodNotAllowed}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		base := "https://" + r.host
		switch req.URL.Path {
		case "/.well-known/nodeinfo":
			if r.nodeinfo != http.StatusOK {
				w.WriteHeader(r.nodeinfo)
				return
			}
			fmt.Fprintf(w, `{"links":[{"href":"%s/nodeinfo/2.0"},{"href":"%s/nodeinfo/2.1"}]}`, base, base)
		case "/nodeinfo/2.1":
			w.WriteHeader(http.StatusNotFound)
		case "/nodeinfo/2.0":
			fmt.Fprint(w, `{"software":{"name":"misskey","version":"2026.9.1"}}`)
		case "/.well-known/webfinger":
			res := req.URL.Query().Get("resource")
			st, ok := r.webfinger[res]
			if !ok {
				st = http.StatusNotFound
			}
			if st != http.StatusOK {
				w.WriteHeader(st)
				return
			}
			fmt.Fprintf(w, `{"subject":%q,"links":[{"rel":"self","type":"application/activity+json","href":"%s/users/%s"}]}`,
				res, base, strings.TrimSuffix(strings.TrimPrefix(res, "acct:"), "@"+r.host))
		case "/inbox", "/users/alice/inbox":
			r.gotInbox = req.Method + " " + req.URL.Path
			w.WriteHeader(r.inbox)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(r.srv.Close)
	r.host = strings.TrimPrefix(r.srv.URL, "https://")
	return r
}

// fakeFetcher answers actor fetches.
type fakeFetcher struct {
	signed, unsigned       []byte
	signedErr, unsignedErr error
	gotSigned, gotUnsigned string
}

func (f *fakeFetcher) FetchObjectSignedOnly(uri string) ([]byte, error) {
	f.gotSigned = uri
	return f.signed, f.signedErr
}

func (f *fakeFetcher) FetchObjectUnsigned(uri string) ([]byte, error) {
	f.gotUnsigned = uri
	return f.unsigned, f.unsignedErr
}

func actorJSON(host string, ed25519 bool) []byte {
	am := ""
	if ed25519 {
		am = fmt.Sprintf(`,"assertionMethod":[{"id":"https://%s/users/alice#ed25519-key","type":"Multikey","publicKeyMultibase":%q}]`, host, testMultikey)
	}
	return []byte(fmt.Sprintf(`{"id":"https://%s/users/alice","inbox":"https://%s/users/alice/inbox","endpoints":{"sharedInbox":"https://%s/inbox"},"publicKey":{"id":"k","publicKeyPem":"PEM"}%s}`,
		host, host, host, am))
}

// testMultikey is a valid Ed25519 public key in Multikey form.
var testMultikey = func() string {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	s, err := activitypub.EncodeEd25519Multikey(pub)
	if err != nil {
		panic(err)
	}
	return s
}()

func healthyDeps(r *remote) (Deps, *fakeFetcher) {
	body := actorJSON(r.host, false)
	f := &fakeFetcher{signed: body, unsigned: body}
	return Deps{
		Client:  r.srv.Client(),
		Fetcher: f,
		Policy: func(context.Context, string) (Policy, error) {
			return Policy{Suspension: model.SuspensionStateNone}, nil
		},
		KnownUser: func(context.Context, string) (*KnownUser, error) {
			return &KnownUser{Username: "alice", URI: "https://" + r.host + "/users/alice"}, nil
		},
		Capability:      func(context.Context, string) (*model.InstanceSignatureCapability, error) { return nil, nil },
		Ed25519Degraded: func(context.Context, string) (bool, error) { return false, nil },
		Delivery: func(context.Context, string) (*deliveryhealth.HostHealth, error) {
			return &deliveryhealth.HostHealth{Success: 10}, nil
		},
		Breaker: func(context.Context, string) (*deliveryhealth.BreakerState, error) { return nil, nil },
	}, f
}

func byName(t *testing.T, rep Report, name string) selfcheck.Result {
	t.Helper()
	for _, r := range rep.Results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no result %q in %+v", name, rep.Results)
	return selfcheck.Result{}
}

func TestRun_Healthy(t *testing.T) {
	r := newRemote(t)
	r.webfinger["acct:alice@"+r.host] = http.StatusOK
	deps, f := healthyDeps(r)

	rep := Run(context.Background(), deps, Request{Host: r.host})
	for _, res := range rep.Results {
		assert.Equal(t, selfcheck.StatusOK, res.Status, "%s: %s", res.Name, res.Detail)
	}
	assert.True(t, rep.OK)
	assert.Equal(t, []string{"policy", "nodeinfo", "webfinger", "actor", "authorized-fetch", "inbox", "signature", "delivery"},
		names(rep))
	assert.Equal(t, "misskey 2026.9.1", byName(t, rep, "nodeinfo").Detail, "falls back from the last link to an earlier one")
	assert.Contains(t, byName(t, rep, "webfinger").Detail, "既知のユーザー")
	assert.Equal(t, "https://"+r.host+"/users/alice", f.gotSigned, "the actor comes from the WebFinger self link")
	assert.Equal(t, "GET /inbox", r.gotInbox, "the shared inbox is probed with GET, never POST")
	assert.Contains(t, byName(t, rep, "signature").Detail, "RSA")
}

func names(rep Report) []string {
	out := make([]string, len(rep.Results))
	for i, r := range rep.Results {
		out[i] = r.Name
	}
	return out
}

// 既知のユーザーがいなければインスタンス actor の候補を順に試す。
func TestRun_WebFingerTriesInstanceActorCandidates(t *testing.T) {
	r := newRemote(t)
	r.webfinger["acct:"+r.host+"@"+r.host] = http.StatusOK
	deps, _ := healthyDeps(r)
	deps.KnownUser = func(context.Context, string) (*KnownUser, error) { return nil, nil }

	res := byName(t, Run(context.Background(), deps, Request{Host: r.host}), "webfinger")
	assert.Equal(t, selfcheck.StatusOK, res.Status, res.Detail)
	assert.Contains(t, res.Detail, "インスタンス actor")
	assert.Contains(t, res.Detail, "acct:"+r.host+"@"+r.host)
}

// 指定したアカウントは既知のユーザーより優先する。
func TestRun_WebFingerPrefersTheGivenAccount(t *testing.T) {
	r := newRemote(t)
	r.webfinger["acct:bob@"+r.host] = http.StatusOK
	deps, f := healthyDeps(r)

	res := byName(t, Run(context.Background(), deps, Request{Host: r.host, Username: "bob"}), "webfinger")
	assert.Equal(t, selfcheck.StatusOK, res.Status, res.Detail)
	assert.Contains(t, res.Detail, "指定したアカウント")
	assert.Equal(t, "https://"+r.host+"/users/bob", f.gotSigned)
}

// WebFinger が失敗しても、既知のユーザーの actor は検査する。
func TestRun_WebFingerFailureStillChecksTheKnownActor(t *testing.T) {
	r := newRemote(t)
	deps, f := healthyDeps(r)

	rep := Run(context.Background(), deps, Request{Host: r.host})
	assert.Equal(t, selfcheck.StatusFail, byName(t, rep, "webfinger").Status)
	assert.Equal(t, selfcheck.StatusOK, byName(t, rep, "actor").Status)
	assert.Equal(t, "https://"+r.host+"/users/alice", f.gotSigned)
	assert.False(t, rep.OK)
}

// 何も分からなければ actor 以降は実行できない (skip)。
func TestRun_NoActorSkipsDependentChecks(t *testing.T) {
	r := newRemote(t)
	deps, f := healthyDeps(r)
	deps.KnownUser = func(context.Context, string) (*KnownUser, error) { return nil, nil }

	rep := Run(context.Background(), deps, Request{Host: r.host})
	for _, n := range []string{"actor", "authorized-fetch", "inbox"} {
		assert.Equal(t, selfcheck.StatusSkip, byName(t, rep, n).Status, n)
	}
	assert.Empty(t, f.gotSigned)
}

// 渡された client (SSRF-safe) を通す。既定の client では自己署名の相手に
// 接続できないので、ここが緑なら渡された client を使っている。
func TestRun_UsesTheGivenClient(t *testing.T) {
	r := newRemote(t)
	deps, _ := healthyDeps(r)
	deps.Client = &http.Client{Timeout: time.Second}

	rep := Run(context.Background(), deps, Request{Host: r.host})
	assert.Equal(t, selfcheck.StatusFail, byName(t, rep, "nodeinfo").Status)
}

func TestRun_NodeInfoFailures(t *testing.T) {
	r := newRemote(t)
	r.nodeinfo = http.StatusNotFound
	deps, _ := healthyDeps(r)
	res := byName(t, Run(context.Background(), deps, Request{Host: r.host}), "nodeinfo")
	assert.Equal(t, selfcheck.StatusFail, res.Status)
	assert.NotEmpty(t, res.Hint)
}

func TestAuthorizedFetchResult(t *testing.T) {
	denied := &activitypub.StatusError{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized"}
	forbidden := &activitypub.StatusError{StatusCode: http.StatusForbidden, Status: "403 Forbidden"}
	gone := &activitypub.StatusError{StatusCode: http.StatusGone, Status: "410 Gone"}
	for _, tc := range []struct {
		name             string
		signed, unsigned error
		status           selfcheck.Status
		detailContains   string
	}{
		{"both ok", nil, nil, selfcheck.StatusOK, "署名なしでも"},
		{"secure mode", nil, denied, selfcheck.StatusOK, "必須"},
		{"signed ok, unsigned other", nil, gone, selfcheck.StatusOK, "署名付きで取得できた"},
		{"our signature rejected", denied, nil, selfcheck.StatusFail, "署名なしなら"},
		{"rejected either way", forbidden, denied, selfcheck.StatusFail, "どちらでも"},
		{"other failure", gone, gone, selfcheck.StatusFail, "410"},
		{"no signer", federation.ErrNoSigner, nil, selfcheck.StatusSkip, "鍵"},
		{"wrapped no signer", fmt.Errorf("x: %w", federation.ErrNoSigner), nil, selfcheck.StatusSkip, "鍵"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := authorizedFetchResult("authorized-fetch", tc.signed, tc.unsigned)
			assert.Equal(t, tc.status, res.Status, res.Detail)
			assert.Contains(t, strings.ReplaceAll(res.Detail, "署名付きでも署名なしでも", "どちらでも"), tc.detailContains)
			if tc.status == selfcheck.StatusFail {
				assert.NotEmpty(t, res.Hint)
			}
		})
	}
}

// 署名付きが拒否されても、署名なしで読めれば actor の中身は検査する。
func TestRun_ActorReadFromUnsignedWhenSignedIsDenied(t *testing.T) {
	r := newRemote(t)
	r.webfinger["acct:alice@"+r.host] = http.StatusOK
	deps, f := healthyDeps(r)
	f.signed, f.signedErr = nil, &activitypub.StatusError{StatusCode: http.StatusUnauthorized, Status: "401"}

	rep := Run(context.Background(), deps, Request{Host: r.host})
	assert.Equal(t, selfcheck.StatusOK, byName(t, rep, "actor").Status)
	assert.Equal(t, selfcheck.StatusFail, byName(t, rep, "authorized-fetch").Status)
}

func TestRun_ActorContent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		status selfcheck.Status
	}{
		{"broken json", `{`, selfcheck.StatusFail},
		{"no key", `{"id":"https://%[1]s/u","inbox":"https://%[1]s/inbox"}`, selfcheck.StatusFail},
		{"no inbox", `{"id":"https://%[1]s/u","publicKey":{"publicKeyPem":"P"}}`, selfcheck.StatusFail},
		{"other host", `{"id":"https://elsewhere.example/u","inbox":"https://%[1]s/inbox","publicKey":{"publicKeyPem":"P"}}`, selfcheck.StatusWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRemote(t)
			r.webfinger["acct:alice@"+r.host] = http.StatusOK
			deps, f := healthyDeps(r)
			body := []byte(tc.body)
			if strings.Contains(tc.body, "%[1]s") {
				body = []byte(fmt.Sprintf(tc.body, r.host))
			}
			f.signed, f.unsigned = body, body
			res := byName(t, Run(context.Background(), deps, Request{Host: r.host}), "actor")
			assert.Equal(t, tc.status, res.Status, res.Detail)
			assert.NotEmpty(t, res.Hint)
		})
	}
}

func TestRun_ActorUnreachable(t *testing.T) {
	r := newRemote(t)
	r.webfinger["acct:alice@"+r.host] = http.StatusOK
	deps, f := healthyDeps(r)
	f.signedErr, f.unsignedErr = errors.New("dial tcp: refused"), errors.New("dial tcp: refused")
	rep := Run(context.Background(), deps, Request{Host: r.host})
	assert.Equal(t, selfcheck.StatusFail, byName(t, rep, "actor").Status)
	assert.Equal(t, selfcheck.StatusSkip, byName(t, rep, "inbox").Status)
}

func TestRun_Inbox(t *testing.T) {
	t.Run("5xx warns", func(t *testing.T) {
		r := newRemote(t)
		r.webfinger["acct:alice@"+r.host] = http.StatusOK
		r.inbox = http.StatusBadGateway
		deps, _ := healthyDeps(r)
		assert.Equal(t, selfcheck.StatusWarn, byName(t, Run(context.Background(), deps, Request{Host: r.host}), "inbox").Status)
	})
	t.Run("personal inbox when no shared inbox", func(t *testing.T) {
		r := newRemote(t)
		r.webfinger["acct:alice@"+r.host] = http.StatusOK
		deps, f := healthyDeps(r)
		body := []byte(fmt.Sprintf(`{"id":"https://%[1]s/users/alice","inbox":"https://%[1]s/users/alice/inbox","publicKey":{"publicKeyPem":"P"}}`, r.host))
		f.signed, f.unsigned = body, body
		res := byName(t, Run(context.Background(), deps, Request{Host: r.host}), "inbox")
		assert.Equal(t, selfcheck.StatusOK, res.Status, res.Detail)
		assert.Equal(t, "GET /users/alice/inbox", r.gotInbox)
	})
	t.Run("unreachable fails", func(t *testing.T) {
		r := newRemote(t)
		r.webfinger["acct:alice@"+r.host] = http.StatusOK
		deps, f := healthyDeps(r)
		body := []byte(fmt.Sprintf(`{"id":"https://%[1]s/users/alice","inbox":"https://127.0.0.1:1/inbox","publicKey":{"publicKeyPem":"P"}}`, r.host))
		f.signed, f.unsigned = body, body
		res := byName(t, Run(context.Background(), deps, Request{Host: r.host}), "inbox")
		assert.Equal(t, selfcheck.StatusFail, res.Status, res.Detail)
		assert.NotEmpty(t, res.Hint)
	})
}

func TestRun_Signature(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	ed := model.SignatureAlgEd25519
	t.Run("degraded warns and says RSA", func(t *testing.T) {
		r := newRemote(t)
		deps, _ := healthyDeps(r)
		deps.Ed25519Degraded = func(context.Context, string) (bool, error) { return true, nil }
		res := byName(t, Run(context.Background(), deps, Request{Host: r.host}), "signature")
		assert.Equal(t, selfcheck.StatusWarn, res.Status)
		assert.Contains(t, res.Detail, "RSA")
		assert.NotEmpty(t, res.Hint)
	})
	t.Run("observed capability says Ed25519", func(t *testing.T) {
		r := newRemote(t)
		deps, _ := healthyDeps(r)
		deps.Capability = func(context.Context, string) (*model.InstanceSignatureCapability, error) {
			return &model.InstanceSignatureCapability{Ed25519AcceptedAt: &now, InboundAlg: &ed}, nil
		}
		res := byName(t, Run(context.Background(), deps, Request{Host: r.host}), "signature")
		assert.Equal(t, selfcheck.StatusOK, res.Status)
		assert.Contains(t, res.Detail, "こちらが使う方式: Ed25519")
		assert.Contains(t, res.Detail, "受理")
		assert.Contains(t, res.Detail, "ed25519")
	})
	t.Run("declared in this actor says Ed25519", func(t *testing.T) {
		r := newRemote(t)
		r.webfinger["acct:alice@"+r.host] = http.StatusOK
		deps, f := healthyDeps(r)
		f.signed = actorJSON(r.host, true)
		res := byName(t, Run(context.Background(), deps, Request{Host: r.host}), "signature")
		assert.Contains(t, res.Detail, "今回の actor に Ed25519 の宣言あり")
		assert.Contains(t, res.Detail, "こちらが使う方式: Ed25519")
	})
}

func TestRun_Delivery(t *testing.T) {
	until := time.Now().Add(time.Minute)
	for _, tc := range []struct {
		name    string
		breaker *deliveryhealth.BreakerState
		health  *deliveryhealth.HostHealth
		status  selfcheck.Status
	}{
		{"breaker open", &deliveryhealth.BreakerState{Open: true, ConsecutiveFailures: 5}, &deliveryhealth.HostHealth{Success: 3}, selfcheck.StatusFail},
		{"throttled", &deliveryhealth.BreakerState{ThrottledUntil: &until}, &deliveryhealth.HostHealth{Success: 3}, selfcheck.StatusWarn},
		{"draining", &deliveryhealth.BreakerState{ReservedUntil: &until}, &deliveryhealth.HostHealth{Success: 3}, selfcheck.StatusWarn},
		{"no deliveries", nil, nil, selfcheck.StatusSkip},
		{"all failed", nil, &deliveryhealth.HostHealth{Failure: 4, LastError: &deliveryhealth.LastError{Message: "boom"}}, selfcheck.StatusFail},
		{"some failed", nil, &deliveryhealth.HostHealth{Success: 4, Failure: 1}, selfcheck.StatusWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRemote(t)
			deps, _ := healthyDeps(r)
			deps.Breaker = func(context.Context, string) (*deliveryhealth.BreakerState, error) { return tc.breaker, nil }
			deps.Delivery = func(context.Context, string) (*deliveryhealth.HostHealth, error) { return tc.health, nil }
			res := byName(t, Run(context.Background(), deps, Request{Host: r.host}), "delivery")
			assert.Equal(t, tc.status, res.Status, res.Detail)
			if tc.status == selfcheck.StatusFail || tc.status == selfcheck.StatusWarn {
				assert.NotEmpty(t, res.Hint)
			}
		})
	}
}

func TestRun_Policy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		p      Policy
		status selfcheck.Status
		detail string
	}{
		{"blocked", Policy{Blocked: true, Suspension: model.SuspensionStateNone}, selfcheck.StatusWarn, "ブロック"},
		{"suspended", Policy{Suspension: "autoSuspendedForNotResponding"}, selfcheck.StatusWarn, "autoSuspendedForNotResponding"},
		{"silenced", Policy{Silenced: true, Suspension: model.SuspensionStateNone}, selfcheck.StatusOK, "サイレンス"},
		{"none", Policy{Suspension: model.SuspensionStateNone}, selfcheck.StatusOK, "制限なし"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRemote(t)
			deps, _ := healthyDeps(r)
			deps.Policy = func(context.Context, string) (Policy, error) { return tc.p, nil }
			res := byName(t, Run(context.Background(), deps, Request{Host: r.host}), "policy")
			assert.Equal(t, tc.status, res.Status)
			assert.Contains(t, res.Detail, tc.detail)
		})
	}
}

// 未配線の依存は skip にし、他の検査は動かす。
func TestRun_UnwiredDepsSkip(t *testing.T) {
	r := newRemote(t)
	rep := Run(context.Background(), Deps{Client: r.srv.Client()}, Request{Host: r.host})
	require.Len(t, rep.Results, 8)
	for _, n := range []string{"policy", "actor", "authorized-fetch", "inbox", "delivery"} {
		assert.Equal(t, selfcheck.StatusSkip, byName(t, rep, n).Status, n)
	}
	assert.Equal(t, selfcheck.StatusOK, byName(t, rep, "nodeinfo").Status)
}

// 相手が返す nodeinfo の links は、このホストを指すものを maxNodeInfoLinks 件まで
// しか引かない。上限が無いと、診断させたホストが links を並べるだけでこちらから
// 大量の GET が飛ぶ。
func TestRun_NodeInfoLinksAreBounded(t *testing.T) {
	var hits atomic.Int64
	var host string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		if req.URL.Path == "/.well-known/nodeinfo" {
			var links []string
			for i := range 3000 {
				links = append(links, fmt.Sprintf(`{"href":"https://%s/n/%d"}`, host, i))
			}
			links = append(links, `{"href":"https://elsewhere.example/n"}`)
			fmt.Fprintf(w, `{"links":[%s]}`, strings.Join(links, ","))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	host = strings.TrimPrefix(srv.URL, "https://")

	r := runner{deps: Deps{Client: srv.Client()}, host: host, client: limitRedirects(srv.Client())}
	res := r.checkNodeInfo(context.Background())
	assert.Equal(t, selfcheck.StatusFail, res.Status)
	assert.Equal(t, int64(1+maxNodeInfoLinks), hits.Load())
}

// 別ホストを指す link は引かない。
func TestRun_NodeInfoIgnoresOtherHosts(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"links":[{"href":"https://elsewhere.example/nodeinfo/2.0"}]}`)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "https://")
	r := runner{host: host, client: limitRedirects(srv.Client())}
	res := r.checkNodeInfo(context.Background())
	assert.Equal(t, selfcheck.StatusFail, res.Status)
	assert.Contains(t, res.Detail, "このホストを指すものが無い")
	assert.Equal(t, int64(1), hits.Load())
}

// redirect は maxRedirects 回までしか辿らない。
func TestLimitRedirects(t *testing.T) {
	var hits atomic.Int64
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		http.Redirect(w, req, srv.URL+"/again", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	r := runner{client: limitRedirects(srv.Client())}
	_, _, err := r.get(context.Background(), srv.URL+"/start", "application/json")
	require.Error(t, err)
	assert.Equal(t, int64(1+maxRedirects), hits.Load())
	assert.Nil(t, limitRedirects(nil))
}

// 診断の制限時間を過ぎたら、残りは失敗ではなく「確かめられず」にする。時間切れを
// 相手の障害や拒否として読ませない。AP の取得は ctx を受け取らないので、待つのを
// やめることで期限を守る。
func TestRun_TimeoutIsNotReportedAsTheRemotesFault(t *testing.T) {
	r := newRemote(t)
	r.webfinger["acct:alice@"+r.host] = http.StatusOK
	deps, _ := healthyDeps(r)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	deps.Fetcher = &slowFetcher{block: block}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	rep := Run(ctx, deps, Request{Host: r.host})
	assert.Less(t, time.Since(start), 5*time.Second, "does not wait for a fetch that ignores ctx")
	for _, n := range []string{"actor", "authorized-fetch", "inbox"} {
		res := byName(t, rep, n)
		assert.Equal(t, selfcheck.StatusSkip, res.Status, n)
		assert.Contains(t, res.Detail, "制限時間", n)
	}
}

type slowFetcher struct{ block chan struct{} }

func (f *slowFetcher) FetchObjectSignedOnly(string) ([]byte, error) {
	<-f.block
	return nil, errors.New("late")
}

func (f *slowFetcher) FetchObjectUnsigned(string) ([]byte, error) {
	<-f.block
	return nil, errors.New("late")
}

func TestRun_TimedOutBeforeNetworkChecks(t *testing.T) {
	r := newRemote(t)
	deps, f := healthyDeps(r)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep := Run(ctx, deps, Request{Host: r.host})
	for _, n := range []string{"nodeinfo", "webfinger", "actor", "authorized-fetch"} {
		assert.Equal(t, selfcheck.StatusSkip, byName(t, rep, n).Status, n)
	}
	assert.Empty(t, f.gotSigned)
}

// 格下げの状態を読めないときに「落としていない」と断定しない。
func TestRun_SignatureDegradeUnreadable(t *testing.T) {
	r := newRemote(t)
	deps, _ := healthyDeps(r)
	deps.Ed25519Degraded = func(context.Context, string) (bool, error) { return false, errors.New("redis down") }
	res := byName(t, Run(context.Background(), deps, Request{Host: r.host}), "signature")
	assert.Equal(t, selfcheck.StatusWarn, res.Status)
	assert.Contains(t, res.Detail, "読めない")
	assert.NotContains(t, res.Detail, "こちらが使う方式")
}

func TestRun_PolicyFederationMode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		p      Policy
		detail string
	}{
		{"none", Policy{FederationDisabled: true, Suspension: model.SuspensionStateNone}, "federation: none"},
		{"specified", Policy{NotAllowed: true, Suspension: model.SuspensionStateNone}, "federation: specified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRemote(t)
			deps, _ := healthyDeps(r)
			deps.Policy = func(context.Context, string) (Policy, error) { return tc.p, nil }
			res := byName(t, Run(context.Background(), deps, Request{Host: r.host}), "policy")
			assert.Equal(t, selfcheck.StatusWarn, res.Status)
			assert.Contains(t, res.Detail, tc.detail)
			assert.NotEmpty(t, res.Hint)
		})
	}
}

func TestParseActor_Shapes(t *testing.T) {
	key := fmt.Sprintf(`{"id":"https://h.example/users/a#k","type":"Multikey","publicKeyMultibase":%q}`, testMultikey)
	mk := fmt.Sprintf("%q", testMultikey)
	for _, tc := range []struct {
		name          string
		body          string
		key, declared bool
	}{
		{"arrays", `{"id":"https://h.example/users/a","publicKey":[{"publicKeyPem":"P"}],"assertionMethod":[` + key + `]}`, true, true},
		{"single objects", `{"id":"https://h.example/users/a","publicKey":{"publicKeyPem":"P"},"assertionMethod":` + key + `}`, true, true},
		{"refs mixed with keys", `{"id":"https://h.example/users/a","publicKey":["https://h.example/k",{"publicKeyPem":"P"}],"assertionMethod":["https://h.example/users/a#ref",` + key + `]}`, true, true},
		{"type as array", `{"id":"https://h.example/users/a","assertionMethod":[{"id":"https://h.example/users/a#k","type":["Multikey"],"publicKeyMultibase":` + mk + `}]}`, false, true},
		{"not multikey", `{"id":"https://h.example/users/a","assertionMethod":[{"id":"https://h.example/users/a#k","type":"JsonWebKey2020","publicKeyMultibase":` + mk + `}]}`, false, false},
		// 値が Ed25519 の鍵として読めないものは、resolver が採らないので宣言と見なさない。
		{"undecodable key", `{"id":"https://h.example/users/a","assertionMethod":[{"id":"https://h.example/users/a#k","type":"Multikey","publicKeyMultibase":"z6Mk"}]}`, false, false},
		{"no key material", `{"id":"https://h.example/users/a","assertionMethod":[{"id":"https://h.example/users/a#k","type":"Multikey"}]}`, false, false},
		{"no key id", `{"id":"https://h.example/users/a","assertionMethod":[{"type":"Multikey","publicKeyMultibase":` + mk + `}]}`, false, false},
		// resolver は actor と別ホストの鍵を採らない。採らない鍵を見て「Ed25519 で
		// 送る」と表示しない。
		{"key on another host", `{"id":"https://h.example/users/a","assertionMethod":[{"id":"https://evil.example/k","type":"Multikey","publicKeyMultibase":` + mk + `}]}`, false, false},
		{"string references only", `{"id":"https://h.example/users/a","publicKey":"https://h.example/key","assertionMethod":["https://h.example/key2"]}`, false, false},
		{"missing", `{}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, err := parseActor([]byte(tc.body))
			require.NoError(t, err)
			assert.Equal(t, tc.key, a.HasPublicKey)
			assert.Equal(t, tc.declared, a.DeclaresEd25519)
		})
	}
}

// actor.id の host は対象と同じ規則で正規化して比べる (既定ポートや大文字で
// 別ホスト扱いしない)。
func TestRun_ActorIDHostIsNormalized(t *testing.T) {
	r := newRemote(t)
	r.webfinger["acct:alice@"+r.host] = http.StatusOK
	deps, f := healthyDeps(r)
	body := []byte(fmt.Sprintf(`{"id":"https://%s/users/alice","inbox":"https://%s/inbox","publicKey":{"publicKeyPem":"P"}}`,
		strings.ToUpper(r.host), r.host))
	f.signed, f.unsigned = body, body
	res := byName(t, Run(context.Background(), deps, Request{Host: r.host}), "actor")
	assert.Equal(t, selfcheck.StatusOK, res.Status, res.Detail)
}

// Run が redirect の制限を掛けた client を使う (制限の関数だけでなく配線も見る)。
func TestRun_LimitsRedirects(t *testing.T) {
	var hits atomic.Int64
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		http.Redirect(w, req, srv.URL+"/again", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "https://")
	Run(context.Background(), Deps{Client: srv.Client()}, Request{Host: host})
	// nodeinfo と WebFinger (接続エラーで次の候補は試さない) の 2 本、それぞれ
	// 最初の 1 回 + redirect maxRedirects 回。
	assert.Equal(t, int64(2*(1+maxRedirects)), hits.Load())
}

func TestHostOf_Normalizes(t *testing.T) {
	assert.Equal(t, "remote.example", hostOf("https://Remote.Example:443/users/a"))
	assert.Equal(t, "xn--bcher-kva.example", hostOf("https://bücher.example/users/a"))
	assert.Equal(t, "remote.example:8443", hostOf("https://remote.example:8443/users/a"))
}

// 既知のユーザーを読めなかったことを黙って「知らない」にしない。
func TestRun_KnownUserUnreadable(t *testing.T) {
	r := newRemote(t)
	r.webfinger["acct:instance.actor@"+r.host] = http.StatusOK
	deps, _ := healthyDeps(r)
	deps.KnownUser = func(context.Context, string) (*KnownUser, error) { return nil, errors.New("db down") }
	res := byName(t, Run(context.Background(), deps, Request{Host: r.host}), "webfinger")
	assert.Equal(t, selfcheck.StatusOK, res.Status)
	assert.Contains(t, res.Detail, "既知のユーザーを読めない")
}

// 署名付きで読めた後に期限が切れたら、actor は検査し、突き合わせだけを
// 「確かめられず」にする。
func TestRun_SignedFetchKeptWhenOnlyTheSecondTimesOut(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	host := "remote.example"
	f := &splitFetcher{signed: actorJSON(host, false), block: block}
	// WebFinger などの往復を挟まず actor の検査だけを見る (期限までの時間を
	// 取得以外に使わせない)。
	r := runner{deps: Deps{Fetcher: f}, host: host}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	actorRes, fetch, actor := r.checkActor(ctx, "https://"+host+"/users/alice")
	assert.Equal(t, selfcheck.StatusOK, actorRes.Status, actorRes.Detail)
	require.NotNil(t, actor)
	assert.Equal(t, selfcheck.StatusSkip, fetch.Status)
	assert.Contains(t, fetch.Detail, "制限時間")
}

// 期限切れの後は署名なしの取得を始めない (相手へ余計なリクエストを出さない)。
func TestRun_NoFetchStartedAfterTheDeadline(t *testing.T) {
	r := newRemote(t)
	r.webfinger["acct:alice@"+r.host] = http.StatusOK
	deps, _ := healthyDeps(r)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	f := &splitFetcher{blockSigned: true, block: block}
	deps.Fetcher = f
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	Run(ctx, deps, Request{Host: r.host})
	// 取得は裏の goroutine で始まるので、始まる猶予を置いてから数える。
	time.Sleep(200 * time.Millisecond)
	assert.Zero(t, f.unsignedCalls.Load())
}

// splitFetcher answers the signed fetch (or blocks it) and blocks the
// unsigned one.
type splitFetcher struct {
	signed        []byte
	blockSigned   bool
	block         chan struct{}
	unsignedCalls atomic.Int64
}

func (f *splitFetcher) FetchObjectSignedOnly(string) ([]byte, error) {
	if f.blockSigned {
		<-f.block
		return nil, errors.New("late")
	}
	return f.signed, nil
}

func (f *splitFetcher) FetchObjectUnsigned(string) ([]byte, error) {
	f.unsignedCalls.Add(1)
	<-f.block
	return nil, errors.New("late")
}

// 署名付きの失敗が拒否 (401/403) でないなら、相手のせいにしない。
func TestAuthorizedFetchResult_NonDenialSignedError(t *testing.T) {
	res := authorizedFetchResult("authorized-fetch", errors.New("key load failed"), nil)
	assert.Equal(t, selfcheck.StatusWarn, res.Status)
	assert.Contains(t, res.Hint, "相手の拒否ではない")
}

// 同じホストの link でも、redirect で別ホストへ移った先の情報は採らない。
func TestRun_NodeInfoRedirectToAnotherHostIsIgnored(t *testing.T) {
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"software":{"name":"elsewhere","version":"1"}}`)
	}))
	t.Cleanup(other.Close)
	var host string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/.well-known/nodeinfo" {
			fmt.Fprintf(w, `{"links":[{"href":"https://%s/nodeinfo/2.0"}]}`, host)
			return
		}
		http.Redirect(w, req, "https://localhost:"+strings.Split(other.Listener.Addr().String(), ":")[1]+"/n", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	host = strings.TrimPrefix(srv.URL, "https://")
	client := srv.Client()
	client.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify = true
	r := runner{host: host, client: limitRedirects(client)}
	res := r.checkNodeInfo(context.Background())
	assert.Equal(t, selfcheck.StatusFail, res.Status, res.Detail)
	assert.NotContains(t, res.Detail, "elsewhere")
}

// 相手が応答せず診断の期限を使い切っても、自サーバーの状態 (ブレーカー・格下げ) は
// ネットワークの検査の後に作る別の期限で読む。
func TestRun_LocalChecksSurviveTheNetworkDeadline(t *testing.T) {
	r := newRemote(t)
	r.webfinger["acct:alice@"+r.host] = http.StatusOK
	deps, _ := healthyDeps(r)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	deps.Fetcher = &slowFetcher{block: block}
	withOpenBreaker(&deps)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	rep := Run(ctx, deps, Request{Host: r.host})
	assertLocalFactsReported(t, rep)
}

// **ネットワークに localTimeout 以上かかっても**自サーバーの状態は読める。期限を
// 冒頭から数えると、ここで期限切れになってブレーカーの fail が消える
// (3 周目のレビューで見つかった回帰の形)。
func TestRun_LocalChecksSurviveASlowNetwork(t *testing.T) {
	prev := localTimeout
	localTimeout = 100 * time.Millisecond
	t.Cleanup(func() { localTimeout = prev })

	r := newRemote(t)
	r.webfinger["acct:alice@"+r.host] = http.StatusOK
	deps, _ := healthyDeps(r)
	deps.Fetcher = &sleepyFetcher{d: 300 * time.Millisecond, body: actorJSON(r.host, false)}
	withOpenBreaker(&deps)

	rep := Run(context.Background(), deps, Request{Host: r.host})
	assert.Equal(t, selfcheck.StatusOK, byName(t, rep, "actor").Status, "the network part finished, just slowly")
	assertLocalFactsReported(t, rep)
}

func withOpenBreaker(deps *Deps) {
	deps.Breaker = func(ctx context.Context, _ string) (*deliveryhealth.BreakerState, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return &deliveryhealth.BreakerState{Open: true, ConsecutiveFailures: 9}, nil
	}
	deps.Ed25519Degraded = func(ctx context.Context, _ string) (bool, error) { return false, ctx.Err() }
}

func assertLocalFactsReported(t *testing.T, rep Report) {
	t.Helper()
	assert.Equal(t, selfcheck.StatusFail, byName(t, rep, "delivery").Status, "the open breaker is still reported")
	sig := byName(t, rep, "signature")
	assert.Equal(t, selfcheck.StatusOK, sig.Status, sig.Detail)
	assert.False(t, rep.OK)
}

// sleepyFetcher answers after d.
type sleepyFetcher struct {
	d    time.Duration
	body []byte
}

func (f *sleepyFetcher) FetchObjectSignedOnly(string) ([]byte, error) {
	time.Sleep(f.d)
	return f.body, nil
}

func (f *sleepyFetcher) FetchObjectUnsigned(string) ([]byte, error) { return f.body, nil }

func TestRun_BreakerUnreadable(t *testing.T) {
	r := newRemote(t)
	deps, _ := healthyDeps(r)
	deps.Breaker = func(context.Context, string) (*deliveryhealth.BreakerState, error) {
		return nil, errors.New("redis down")
	}
	res := byName(t, Run(context.Background(), deps, Request{Host: r.host}), "delivery")
	assert.Equal(t, selfcheck.StatusWarn, res.Status)
	assert.Contains(t, res.Detail, "読めない")
}
