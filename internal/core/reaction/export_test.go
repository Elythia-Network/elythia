package reaction

// 本番は NewService の必須の引数で渡す (#3825)。以下は、外部パッケージの
// テストが補助関数で作った service に後から入れるための口で、テストのビルドで
// だけ見える。

// SetUserRolesProviderForTest replaces the user roles provider after construction.
func (s *Service) SetUserRolesProviderForTest(p UserRolesProvider) { s.userRoles = p }

// SetMediaSilenceCheckerForTest replaces the media-silence checker after construction.
func (s *Service) SetMediaSilenceCheckerForTest(c MediaSilenceChecker) { s.mediaSilence = c }

// SetBlockingCheckerForTest replaces the blocking checker after construction.
func (s *Service) SetBlockingCheckerForTest(c BlockingChecker) { s.blockingChecker = c }
