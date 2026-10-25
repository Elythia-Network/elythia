package federation

import (
	"github.com/elythia-network/elythia/internal/activitypub"
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
// (#3833)。依存を渡したいテストは NewProcessor を直接呼ぶ。
func NewProcessorNilDeps(
	resolver *Resolver,
	followingService *corefollowing.Service,
	reactionService *corereaction.Service,
	noteDeleteSvc *corenote.DeleteService,
	userRepo repository.UserRepository,
	noteRepo repository.NoteRepository,
) *Processor {
	return NewProcessor(resolver, followingService, reactionService, noteDeleteSvc, userRepo, noteRepo)
}
