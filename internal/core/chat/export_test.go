package chat

import (
	"github.com/elythia-network/elythia/internal/core/userpack"
	"github.com/elythia-network/elythia/internal/repository"
)

// 下の …ForTest は、構築子の必須の引数にした依存 (#3823) を、補助関数が作った
// service へ後から入れるための seam。同じパッケージのテストならフィールドへ直接
// 代入するところを、外部パッケージ (chat_test) のテストから行う。ファイル名が
// _test.go で終わるので、本番のビルドには含まれない。

// SetBlockingRepoForTest replaces the blocking repository.
func (s *Service) SetBlockingRepoForTest(r repository.BlockingRepository) { s.blockingRepo = r }

// SetLiteFillerForTest replaces the lite filler.
func (s *Service) SetLiteFillerForTest(f userpack.LiteFiller) { s.lites = f }
