package signup

import "github.com/elythia-network/elythia/internal/core/role"

// SetUserPolicyResolverForTest replaces the effective-policy source.
//
// 構築子の必須の引数にした依存 (#3823) を、補助関数が作った handler へ後から
// 入れるための seam。同じパッケージのテストならフィールドへ直接代入するところを、
// 外部パッケージ (signup_test) のテストから行う。ファイル名が _test.go で終わるので、
// 本番のビルドには含まれない。
func (h *Handler) SetUserPolicyResolverForTest(r role.PolicyProvider) { h.userPolicies = r }
