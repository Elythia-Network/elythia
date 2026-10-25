package note

import (
	"time"

	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/repository"
)

// SafeGoForTest exposes safeGo to package-external tests.
func SafeGoForTest(fn func()) { safeGo(nil, newHookSlots(0), fn) }

// ResolveMentionUserIDsForTest runs the mention → userID mapping the create
// path uses (DB lookup, then the WebFinger fetch) so the host rules (#2704,
// #3330) can be pinned without building a full note.
func (s *CreateService) ResolveMentionUserIDsForTest(mentions []Mention, authorHost *string) []string {
	r := s.lookupMentionsInDB(mentions, authorHost)
	s.fetchRemoteMentions(r)
	return r.userIDs()
}

// SetRemoteMentionFetchTimeoutForTest overrides the overall deadline of the
// remote mention fetches.
func (s *CreateService) SetRemoteMentionFetchTimeoutForTest(d time.Duration) {
	s.remoteMentionTimeout = d
}

// RemoteMentionFetchConcurrency exposes the fetch concurrency for tests.
const RemoteMentionFetchConcurrency = remoteMentionFetchConcurrency

// 本番は NewCreateService の必須の引数で渡す (#3825)。以下は、外部パッケージの
// テストが補助関数で作った service に後から入れるための口で、テストのビルドで
// だけ見える。

// SetBackgroundForTest replaces the background group after construction.
func (s *CreateService) SetBackgroundForTest(g *bgwork.Group) { s.background = g }

// SetEntityPackerForTest replaces the Packer after construction.
func (s *CreateService) SetEntityPackerForTest(p *entity.Packer) { s.entityPacker = p }

// SetBlockingRepoForTest replaces the blocking repository after construction.
func (s *CreateService) SetBlockingRepoForTest(r repository.BlockingRepository) { s.blockingRepo = r }

// SetMetaRepoForTest replaces the meta repository after construction.
func (s *CreateService) SetMetaRepoForTest(r repository.MetaRepository) { s.metaRepo = r }

// SetSilencingProviderForTest replaces the silencing provider after construction.
func (s *CreateService) SetSilencingProviderForTest(p SilencingProvider) { s.silencingProvider = p }

// SetDriveFileRepoForTest replaces the drive file repository after construction.
func (s *CreateService) SetDriveFileRepoForTest(r repository.DriveFileRepository) {
	s.driveFileRepo = r
}
