package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/config"
	coredeliveryhealth "github.com/shiroha-a/mk/internal/core/deliveryhealth"
	coreinstance "github.com/shiroha-a/mk/internal/core/instance"
	"github.com/shiroha-a/mk/internal/core/remotecheck"
	"github.com/shiroha-a/mk/internal/core/selfcheck"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// 疎通の診断 (#3055) は宛先を管理者が指定する口なので、配線する client は
// SSRF-safe でなければならない。loopback の相手へは接続そのものが拒否される
// (TLS の検証まで進まない) ことで確かめる。
func TestRemoteCheckAdapter_UsesTheSSRFSafeClient(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "https://")

	s := &Server{config: &config.Config{}}
	rep := s.newRemoteCheckAdapter(remoteCheckSources{}).CheckRemoteHost(context.Background(), remotecheck.Request{Host: host})

	var nodeinfo selfcheck.Result
	for _, r := range rep.Results {
		if r.Name == "nodeinfo" {
			nodeinfo = r
		}
	}
	require.Equal(t, selfcheck.StatusFail, nodeinfo.Status)
	assert.Contains(t, nodeinfo.Detail, "private IP blocked", "the dial to a private address is refused by the SSRF guard")
	assert.Zero(t, hits.Load(), "nothing reaches a private address")
}

// allowedPrivateNetworks で許可した宛先へは接続する (設定を尊重する)。
func TestRemoteCheckAdapter_HonoursAllowedPrivateNetworks(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "https://")

	s := &Server{config: &config.Config{AllowedPrivateNetworks: []string{"127.0.0.0/8"}}}
	rep := s.newRemoteCheckAdapter(remoteCheckSources{}).CheckRemoteHost(context.Background(), remotecheck.Request{Host: host})
	for _, r := range rep.Results {
		if r.Name == "nodeinfo" {
			assert.NotContains(t, r.Detail, "private IP blocked", "an allowed private network is not blocked")
		}
	}
}

type fakeInstances struct {
	sets    coreinstance.FederationHostSets
	setsErr error
	inst    *model.Instance
	instErr error
}

func (f *fakeInstances) FederationHostLists() (coreinstance.FederationHostSets, error) {
	return f.sets, f.setsErr
}

func (f *fakeInstances) FindByHost(string) (*model.Instance, error) { return f.inst, f.instErr }

// 連合モード・許可リスト・ブロック (suffix match)・配送停止・ソフトウェアでの停止を
// インスタンスの設定から読む。IsAllowed と同じ判定でないと、受信を拒否している
// 相手を「制限なし」と出して、下の失敗を相手のせいに見せる。
func TestRemoteCheckAdapter_Policy(t *testing.T) {
	s := &Server{config: &config.Config{}}
	notFound := &fakeInstances{instErr: coreinstance.ErrInstanceNotFound}
	for _, tc := range []struct {
		name string
		src  *fakeInstances
		want remotecheck.Policy
	}{
		{"none", &fakeInstances{sets: coreinstance.FederationHostSets{Federation: "none"}, instErr: coreinstance.ErrInstanceNotFound},
			remotecheck.Policy{FederationDisabled: true, Suspension: model.SuspensionStateNone}},
		{"specified, not listed", &fakeInstances{sets: coreinstance.FederationHostSets{Federation: "specified", FederationHosts: []string{"friend.example"}}, instErr: coreinstance.ErrInstanceNotFound},
			remotecheck.Policy{NotAllowed: true, Suspension: model.SuspensionStateNone}},
		{"specified, listed by suffix", &fakeInstances{sets: coreinstance.FederationHostSets{Federation: "specified", FederationHosts: []string{"example"}}, instErr: coreinstance.ErrInstanceNotFound},
			remotecheck.Policy{Suspension: model.SuspensionStateNone}},
		{"blocked by suffix", &fakeInstances{sets: coreinstance.FederationHostSets{Blocked: []string{"example"}}, instErr: coreinstance.ErrInstanceNotFound},
			remotecheck.Policy{Blocked: true, Suspension: model.SuspensionStateNone}},
		{"silenced", &fakeInstances{sets: coreinstance.FederationHostSets{Silenced: []string{"remote.example"}}, instErr: coreinstance.ErrInstanceNotFound},
			remotecheck.Policy{Silenced: true, Suspension: model.SuspensionStateNone}},
		{"unknown instance", notFound, remotecheck.Policy{Suspension: model.SuspensionStateNone}},
		{"suspended", &fakeInstances{inst: &model.Instance{SuspensionState: model.SuspensionStateAutoSuspendedForNotResponding}},
			remotecheck.Policy{Suspension: model.SuspensionStateAutoSuspendedForNotResponding}},
		{"software suspended", &fakeInstances{
			sets: coreinstance.FederationHostSets{SuspendedSoftware: []model.SuspendedSoftwareEntry{{Software: "badsoft", VersionRange: "*"}}},
			inst: &model.Instance{SuspensionState: model.SuspensionStateNone, SoftwareName: strPtr("badsoft"), SoftwareVersion: strPtr("1.0.0")}},
			remotecheck.Policy{Suspension: "softwareSuspended"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := s.newRemoteCheckAdapter(remoteCheckSources{instances: tc.src})
			got, err := a.deps.Policy(context.Background(), "remote.example")
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	a := s.newRemoteCheckAdapter(remoteCheckSources{instances: &fakeInstances{setsErr: errors.New("meta")}})
	_, err := a.deps.Policy(context.Background(), "remote.example")
	assert.Error(t, err, "an unreadable meta is reported, not read as no restriction")
	a = s.newRemoteCheckAdapter(remoteCheckSources{instances: &fakeInstances{instErr: errors.New("db")}})
	_, err = a.deps.Policy(context.Background(), "remote.example")
	assert.Error(t, err)
}

type fakeUsers struct {
	u   *model.User
	err error
}

func (f fakeUsers) FindRecentByHost(string) (*model.User, error) { return f.u, f.err }

type fakeSigCaps struct {
	c   *model.InstanceSignatureCapability
	err error
}

func (f fakeSigCaps) FindByHost(string) (*model.InstanceSignatureCapability, error) {
	return f.c, f.err
}

type fakeDelivery []coredeliveryhealth.HostHealth

func (f fakeDelivery) Query(context.Context, time.Duration) ([]coredeliveryhealth.HostHealth, error) {
	return f, nil
}

type fakeBreakerList []coredeliveryhealth.BreakerState

func (f fakeBreakerList) List(context.Context) ([]coredeliveryhealth.BreakerState, error) {
	return f, nil
}

// 「無い」は nil で返し (検査は skip / 別経路へ)、障害はエラーで返す。
// 配送とブレーカーは対象のホストの行だけを返す。
func TestRemoteCheckAdapter_Lookups(t *testing.T) {
	s := &Server{config: &config.Config{}}
	ctx := context.Background()
	uri := "https://remote.example/users/alice"

	a := s.newRemoteCheckAdapter(remoteCheckSources{users: fakeUsers{u: &model.User{Username: "alice", URI: &uri}}})
	u, err := a.deps.KnownUser(ctx, "remote.example")
	require.NoError(t, err)
	assert.Equal(t, &remotecheck.KnownUser{Username: "alice", URI: uri}, u)
	a = s.newRemoteCheckAdapter(remoteCheckSources{users: fakeUsers{err: repository.ErrNotFound}})
	u, err = a.deps.KnownUser(ctx, "remote.example")
	assert.NoError(t, err)
	assert.Nil(t, u)
	a = s.newRemoteCheckAdapter(remoteCheckSources{users: fakeUsers{err: errors.New("db")}})
	_, err = a.deps.KnownUser(ctx, "remote.example")
	assert.Error(t, err)

	a = s.newRemoteCheckAdapter(remoteCheckSources{sigCaps: fakeSigCaps{err: repository.ErrNotFound}})
	c, err := a.deps.Capability(ctx, "remote.example")
	assert.NoError(t, err)
	assert.Nil(t, c)
	a = s.newRemoteCheckAdapter(remoteCheckSources{sigCaps: fakeSigCaps{err: errors.New("db")}})
	_, err = a.deps.Capability(ctx, "remote.example")
	assert.Error(t, err)

	a = s.newRemoteCheckAdapter(remoteCheckSources{delivery: fakeDelivery{{Host: "other.example", Failure: 9}, {Host: "remote.example", Success: 3}}})
	h, err := a.deps.Delivery(ctx, "remote.example")
	require.NoError(t, err)
	require.NotNil(t, h)
	assert.Equal(t, int64(3), h.Success)
	h, err = a.deps.Delivery(ctx, "none.example")
	assert.NoError(t, err)
	assert.Nil(t, h)

	a = s.newRemoteCheckAdapter(remoteCheckSources{breaker: fakeBreakerList{{Host: "other.example", Open: true}, {Host: "remote.example"}}})
	b, err := a.deps.Breaker(ctx, "remote.example")
	require.NoError(t, err)
	require.NotNil(t, b)
	assert.False(t, b.Open)
	b, err = a.deps.Breaker(ctx, "none.example")
	assert.NoError(t, err)
	assert.Nil(t, b)

	a = s.newRemoteCheckAdapter(remoteCheckSources{})
	assert.Nil(t, a.deps.Policy)
	assert.Nil(t, a.deps.KnownUser)
	assert.Nil(t, a.deps.Capability)
	assert.Nil(t, a.deps.Delivery)
	assert.Nil(t, a.deps.Breaker)
}
