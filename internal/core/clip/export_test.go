package clip

import "github.com/elythia-network/elythia/internal/core/role"

// SetRolePolicyProviderForTest replaces the role policy source after
// construction. 本番は NewService の必須の引数で渡す (#3825)。外部パッケージの
// テストが補助関数で作った service に後から入れるための口で、テストのビルドで
// だけ見える。
func (s *Service) SetRolePolicyProviderForTest(p role.PolicyProvider) { s.rolePolicyProvider = p }
