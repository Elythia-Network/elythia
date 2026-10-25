package user

import "github.com/elythia-network/elythia/internal/entity"

// SetEntityPackerForTest replaces the Packer after construction. 本番は
// NewService の必須の引数で渡す (#3825)。外部パッケージのテストが補助関数で作った
// service に後から入れるための口で、テストのビルドでだけ見える。
func (s *Service) SetEntityPackerForTest(p *entity.Packer) { s.entityPacker = p }
