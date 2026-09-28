package server

import (
	"context"
	"errors"
	"time"

	coredeliveryhealth "github.com/shiroha-a/mk/internal/core/deliveryhealth"
	corefederation "github.com/shiroha-a/mk/internal/core/federation"
	coreinstance "github.com/shiroha-a/mk/internal/core/instance"
	"github.com/shiroha-a/mk/internal/core/remotecheck"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// remoteCheckHTTPTimeout bounds each request of the remote diagnosis.
const remoteCheckHTTPTimeout = 10 * time.Second

// remoteCheckAdapter diagnoses federation with one remote host (#3055).
type remoteCheckAdapter struct {
	deps remotecheck.Deps
}

func (a *remoteCheckAdapter) CheckRemoteHost(ctx context.Context, req remotecheck.Request) remotecheck.Report {
	return remotecheck.Run(ctx, a.deps, req)
}

// remoteCheckSources are the parts of the server the diagnosis reads.
// テストで差し替えられるよう、必要なメソッドだけの interface で受ける。
type remoteCheckSources struct {
	fetcher   remotecheck.Fetcher
	instances remoteCheckInstances
	users     remoteCheckUsers
	sigCaps   remoteCheckSigCaps
	degraded  func(ctx context.Context, host string) (bool, error)
	delivery  remoteCheckDelivery
	breaker   remoteCheckBreaker
}

type remoteCheckInstances interface {
	FederationHostLists() (coreinstance.FederationHostSets, error)
	FindByHost(host string) (*model.Instance, error)
}

type remoteCheckUsers interface {
	FindRecentByHost(host string) (*model.User, error)
}

type remoteCheckSigCaps interface {
	FindByHost(host string) (*model.InstanceSignatureCapability, error)
}

type remoteCheckDelivery interface {
	Query(ctx context.Context, window time.Duration) ([]coredeliveryhealth.HostHealth, error)
}

type remoteCheckBreaker interface {
	List(ctx context.Context) ([]coredeliveryhealth.BreakerState, error)
}

// newRemoteCheckAdapter wires the diagnosis.
//
// **通信は outboundClient (SSRF-safe transport) だけを使う。** 宛先を管理者が
// 指定する口なので、self-check の client (ガード無し) を渡してはいけない。
// 署名付きの取得に使う fetcher も同じ transport の上に作られている。
func (s *Server) newRemoteCheckAdapter(src remoteCheckSources) *remoteCheckAdapter {
	deps := remotecheck.Deps{
		Client:          s.outboundClient(remoteCheckHTTPTimeout),
		Fetcher:         src.fetcher,
		Ed25519Degraded: src.degraded,
	}
	if src.instances != nil {
		deps.Policy = func(_ context.Context, host string) (remotecheck.Policy, error) {
			sets, err := src.instances.FederationHostLists()
			if err != nil {
				return remotecheck.Policy{}, err
			}
			// IsAllowed と同じ判定 (連合モード → 許可リスト → ブロック)。
			p := remotecheck.Policy{
				FederationDisabled: sets.Federation == "none",
				NotAllowed:         sets.Federation == "specified" && !coreinstance.HostMatchesAllowList(sets.FederationHosts, host),
				Blocked:            coreinstance.HostMatchesAny(sets.Blocked, host),
				Silenced:           coreinstance.HostMatchesAny(sets.Silenced, host),
				Suspension:         model.SuspensionStateNone,
			}
			inst, err := src.instances.FindByHost(host)
			switch {
			case errors.Is(err, coreinstance.ErrInstanceNotFound):
			case err != nil:
				return p, err
			default:
				p.Suspension = inst.SuspensionState
				if p.Suspension == model.SuspensionStateNone &&
					corefederation.MatchSuspendedSoftware(inst.SoftwareName, inst.SoftwareVersion, sets.SuspendedSoftware) {
					p.Suspension = "softwareSuspended"
				}
			}
			return p, nil
		}
	}
	if src.users != nil {
		deps.KnownUser = func(_ context.Context, host string) (*remotecheck.KnownUser, error) {
			u, err := src.users.FindRecentByHost(host)
			if errors.Is(err, repository.ErrNotFound) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			return &remotecheck.KnownUser{Username: u.Username, URI: deref(u.URI)}, nil
		}
	}
	if src.sigCaps != nil {
		deps.Capability = func(_ context.Context, host string) (*model.InstanceSignatureCapability, error) {
			c, err := src.sigCaps.FindByHost(host)
			if errors.Is(err, repository.ErrNotFound) {
				return nil, nil
			}
			return c, err
		}
	}
	if src.delivery != nil {
		deps.Delivery = func(ctx context.Context, host string) (*coredeliveryhealth.HostHealth, error) {
			all, err := src.delivery.Query(ctx, remotecheck.DeliveryWindow)
			if err != nil {
				return nil, err
			}
			for i := range all {
				if all[i].Host == host {
					return &all[i], nil
				}
			}
			return nil, nil
		}
	}
	if src.breaker != nil {
		deps.Breaker = func(ctx context.Context, host string) (*coredeliveryhealth.BreakerState, error) {
			list, err := src.breaker.List(ctx)
			if err != nil {
				return nil, err
			}
			for i := range list {
				if list[i].Host == host {
					return &list[i], nil
				}
			}
			return nil, nil
		}
	}
	return &remoteCheckAdapter{deps: deps}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

var _ remotecheck.Fetcher = (*corefederation.APFetcher)(nil)
