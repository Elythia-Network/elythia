package entitycompat

import "testing"

// アカウントの作成日時 (#3465) は router で 2 箇所を配線しないと効かない。
//
// どちらも未配線なら黙って何もしない (actor に `published` が出ない / フォローを
// 受けても Misskey 系の作成日時を取りに行かない) ので、行ごと消しても build も
// テストも通る。`internal/server` は CI のカバレッジ対象外なので、router.go を
// AST で照合して固定する。挙動そのものは activitypub の
// TestRenderPerson_PublishedIsAccountCreationTime と、core/federation の
// TestProcess_FollowFillsAccountCreatedAtBestEffort /
// TestAccountCreatedAtFiller_DB_MisskeyFallback が見る。
func TestAccountCreatedAtIsWired(t *testing.T) {
	assertWired(t, routerGo, "apRenderer.SetIDGenerator(idGen)",
		"こちらの actor に published (アカウントの作成日時) が載らなくなる (#3465)")
	assertWired(t, routerGo,
		"federationProcessor.SetAccountCreatedAtFiller(corefederation.NewAccountCreatedAtFiller(remoteStatsFetcher, instanceRepo, userRepo).Fill)",
		"published を送らない Misskey 系の人の作成日時を、フォローを受けても取りに行かなくなる (#3465)")
}
