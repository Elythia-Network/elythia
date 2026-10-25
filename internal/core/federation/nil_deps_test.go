package federation

import (
	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/bgwork"
	coreblocking "github.com/elythia-network/elythia/internal/core/blocking"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	corereaction "github.com/elythia-network/elythia/internal/core/reaction"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/repository"
)

// NewResolverNilDeps builds a Resolver for tests from the arguments most of
// them vary, leaving every other constructor argument unset.
//
// テストの組み立てを 1 か所に寄せておき、構築子の引数が増えたときに直すのを
// ここだけにする (#3833)。依存を渡したいテストは NewResolver を直接呼ぶ。
func NewResolverNilDeps(
	userRepo repository.UserRepository,
	noteRepo repository.NoteRepository,
	urls *activitypub.URLBuilder,
	fetcher HTTPFetcher,
	idGen id.Generator,
) *Resolver {
	return NewResolver(userRepo, noteRepo, urls, fetcher, idGen)
}

// NewProcessorNilDeps builds a Processor for tests from the arguments most of
// them vary, leaving every other constructor argument unset.
//
// NewResolverNilDeps と同じく、構築子の引数が増えたときに直すのをここだけにする
// (#3833)。localBaseURL は空文字、blockingService と background は nil (processor の
// 中の nil の分岐を通る)。依存を渡したいテストは NewProcessor を直接呼ぶか、下の
// …ForTest で入れる。
func NewProcessorNilDeps(
	resolver *Resolver,
	followingService *corefollowing.Service,
	reactionService *corereaction.Service,
	noteDeleteSvc *corenote.DeleteService,
	userRepo repository.UserRepository,
	noteRepo repository.NoteRepository,
) *Processor {
	return NewProcessor(resolver, followingService, reactionService, noteDeleteSvc, userRepo, noteRepo,
		"", nil, nil)
}

// SetLocalBaseURLForTest sets the local base URL after construction.
//
// 外部テスト (federation_test) は作った後でフィールドへ直接代入できないので、その
// 代わり (#3833)。本番の組み立ては構築子で渡す。_test.go にあるので本番のコードからは
// 呼べない。テストの途中で入れ替えて、空のときと入れたときの両方を見るものがある。
func (p *Processor) SetLocalBaseURLForTest(baseURL string) { p.localBaseURL = baseURL }

// SetBlockingServiceForTest sets the blocking service after construction
// (see SetLocalBaseURLForTest).
func (p *Processor) SetBlockingServiceForTest(svc *coreblocking.Service) { p.blockingService = svc }

// SetBackgroundForTest sets the background group after construction (see
// SetLocalBaseURLForTest).
func (p *Processor) SetBackgroundForTest(g *bgwork.Group) { p.background = g }
