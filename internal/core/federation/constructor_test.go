package federation

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/bgwork"
	coreblocking "github.com/elythia-network/elythia/internal/core/blocking"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

// 構築子の引数に渡すだけの値。どれも呼ばれないので、interface を埋め込んで型を満たす。
type (
	ctorSoftwareBlocker     struct{ SoftwareSuspensionChecker }
	ctorHostBlocker         struct{ HostBlockChecker }
	ctorResolverHostBlocker struct{ ResolverHostBlocker }
	ctorSilencedChecker     struct{ SilencedHostChecker }
	ctorRolePolicies        struct{ RolePolicyProvider }
	ctorOriginRepo          struct {
		repository.UserSuspensionOriginRepository
	}
)

// 構築子は受け取った依存を持たせる (#3823)。依存を構築子の必須の引数にしたので、
// 起動時の検査 (criticalWiring) は入れ忘れを見なくなった。構築子が引数を捨てても
// build は通るので、ここで固定する。
func TestRemoteUserResolverAndStatsFetcherKeepDependencies(t *testing.T) {
	blocker := &ctorSoftwareBlocker{}
	r := NewRemoteUserResolver(nil, nil, nil, "", blocker)
	assert.Same(t, blocker, r.softwareBlocker)

	var asked []string
	f := NewRemoteStatsFetcher(nil, "", func(host string) bool {
		asked = append(asked, host)
		return false
	})
	if assert.NotNil(t, f.hostAllowed) {
		assert.False(t, f.hostAllowed("remote.example"))
		assert.Equal(t, []string{"remote.example"}, asked, "渡した判定を持つこと")
	}
}

// 下の構築子も同じ (#3833)。

func TestNewDeliverServiceKeepsDependencies(t *testing.T) {
	blocker := &ctorHostBlocker{}
	s := NewDeliverService(nil, nil, nil, nil, nil, blocker)
	assert.Same(t, blocker, s.hostBlocker)
}

func TestNewNoteDeleteDeliveryHookKeepsDependencies(t *testing.T) {
	users := testutil.NewMockUserRepository()
	notes := testutil.NewMockNoteRepository()
	h := NewNoteDeleteDeliveryHook(nil, nil, nil, users, notes)
	assert.Same(t, users, h.userRepo)
	assert.Same(t, notes, h.noteRepo)
}

func TestNewProcessorKeepsDependencies(t *testing.T) {
	blocking := coreblocking.NewService(nil, nil, nil, nil, nil, nil, nil)
	g := bgwork.New()
	p := NewProcessor(nil, nil, nil, nil, nil, nil, "https://example.test", blocking, g)
	assert.Equal(t, "https://example.test", p.localBaseURL)
	assert.Same(t, blocking, p.blockingService)
	assert.Same(t, g, p.background)
}

func TestNewResolverKeepsDependencies(t *testing.T) {
	hostBlocker := &ctorResolverHostBlocker{}
	software := &ctorSoftwareBlocker{}
	silenced := &ctorSilencedChecker{}
	policies := &ctorRolePolicies{}
	origins := &ctorOriginRepo{}
	g := bgwork.New()
	r := NewResolver(nil, nil, nil, nil, nil, hostBlocker, software, silenced, policies, origins, g)
	assert.Same(t, hostBlocker, r.hostBlocker)
	assert.Same(t, software, r.softwareBlocker)
	assert.Same(t, silenced, r.silencedChecker)
	assert.Same(t, policies, r.rolePolicyProvider)
	assert.Same(t, origins, r.suspensionOriginRepo)
	assert.Same(t, g, r.background)
}
