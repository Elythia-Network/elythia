package following

import (
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/repository"
)

// 本番は NewService の必須の引数で渡す (#3825)。以下は、外部パッケージの
// テストが補助関数で作った service に後から入れるための口で、テストのビルドで
// だけ見える。

// SetEntityPackerForTest replaces the Packer after construction.
func (s *Service) SetEntityPackerForTest(p *entity.Packer) { s.entityPacker = p }

// SetSilencedHostCheckerForTest replaces the silenced-host lookup after construction.
func (s *Service) SetSilencedHostCheckerForTest(c SilencedHostChecker) { s.silencedChecker = c }

// SetSilentFollowRepoForTest replaces the silent follow store after construction.
func (s *Service) SetSilentFollowRepoForTest(r repository.SilentFollowRepository) {
	s.silentFollowRepo = r
}

// SetUserPackerForTest replaces the user packer after construction.
func (s *Service) SetUserPackerForTest(p UserPacker) { s.userPacker = p }
