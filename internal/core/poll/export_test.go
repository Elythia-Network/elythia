package poll

// SetBlockingCheckerForTest replaces the blocking checker after construction.
// 本番は NewService の必須の引数で渡す (#3825)。外部パッケージのテストが補助関数で
// 作った service に後から入れるための口で、テストのビルドでだけ見える。
func (s *Service) SetBlockingCheckerForTest(c BlockingChecker) { s.blockingChecker = c }
