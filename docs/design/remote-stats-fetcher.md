# RemoteStatsFetcher: リモートユーザー counts の origin fetch (mk-go 独自拡張)

**Status**: Active (#943 で実装、#945 で LRU cache 化、2026-05-09) / **Scope**: `internal/core/federation/remote_stats.go`

---

## 1. 背景

Misskey TS (および mk-go の元実装) は `users/show` で remote user の `notesCount` / `followersCount` / `followingCount` を **自インスタンスで観測した範囲のみ** 集計する。具体的には:

- `notesCount`: 自インスタンスが受信した同 user 由来の Note 数
- `followersCount`: 自インスタンスから観測できる follower (= 自 instance の local user で fan-out 経由で関係を持っているもの) の数
- `followingCount`: 同様

→ remote user のプロフィール画面では「実体より小さい / ゼロ」になる。フォローしていないユーザーは Inbox に流れてこないので 0 表示が当然。

UDS production の運用報告 (#943): リモートユーザーの数値が実体より小さく見えて UX が悪い。「リモートサーバー上の実値を表示してほしい」という要望。

## 2. 設計判断: origin instance の `/api/users/show` を fetch して上書き

mk-go 独自拡張として、 `users/show` で remote user (= `user.Host != nil`) の場合に origin instance の `/api/users/show` を https POST して 3 つの count を取得 → `(*entity.Packer).PackUserDetailed` の戻り値に上書きする。

```
[users/show 呼び出し]
        │
        ▼
[(*entity.Packer).PackUserDetailed]  ← 自インスタンス観測値
        │
        ▼
[remote user?]  ── no ──► そのまま返す
        │ yes
        ▼
[RemoteStatsFetcher.Fetch(host, username)]
        │
        ├─ cache hit (LRU + per-entry TTL) ──► 上書き
        │
        ├─ cache miss
        │     │
        │     ▼
        │   [singleflight: 並行 fetch を fold]
        │     │
        │     ▼
        │   [https POST origin/api/users/show]
        │     │
        │     ▼
        │   [SSRF guard (safehttp): private IP / metadata 弾く]
        │     │
        │     ▼
        │   [host validation: URL injection 弾く]
        │     │
        │     ▼
        │   [3 counts を抽出して LRU に store]
        │     │
        ▼     ▼
       上書き or fallback (= local 観測値を維持)
```

### 維持する制約 (TS と同じ)

- フォロー一覧 (`users/followers`) / フォロー中一覧 (`users/following`) は **自インスタンス上に存在する関係のみ** 返す
- 「数値だけ remote から、一覧は local」という非対称設計

理由: 一覧を origin から取ると pagination / privacy / federation 仕様の都合で複雑化、本来必要なのは数値表示のみ。

## 3. なぜ mk-go 独自拡張か

upstream Misskey TS は 2026.5.1 時点でこの機能を持たない。mk-go が独自に追加した理由:

- UDS production で実観測された UX 問題
- 既存 federation API 経由で軽量に実装可能 (= 新 protocol 不要、Misskey 互換 instance に対して `/api/users/show` を叩くだけ)
- failure 時の silent fallback で従来 UX を破壊しない

upstream にも提案できるが、frontend 側 (TS) に対しても drop-in で動くため mk-go 単独で先行実装する判断。

## 4. 実装の trade-off

### Cache (LRU, size cap 10000)

- positive TTL 1h、negative TTL 5min (#945 で per-entry TTL に変更)
- `hashicorp/golang-lru/v2` (既存依存) で size eviction
- ~80 byte/entry × 10K = 上限 ~800KB memory footprint
- Counter は数分単位で動くが毎 request 取りに行くと remote 負荷が大きい trade-off

### SSRF guard

- `safehttp.NewSSRFSafeTransport` 経由 (urlpreview / mediaproxy と同 pattern)
- private IP / loopback / metadata service を DNS resolve 段階で reject
- `allowedPrivateNetworks` config で開発時の self-loop を許可可能

### Host validation

- `url.Parse("https://" + host + "/")` で `parsed.Host == host` を確認
- `/`, `?`, `#`, `@`, ` `, NUL byte 等の混入を reject
- federation の webfinger 経由で sanitize されているはずだが二重 check

### Failure handling

- HTTP 4xx/5xx / timeout / malformed JSON: silent fallback (= local 観測値を維持)
- log は `slog.Debug` レベル (= 大量 remote から fetch 試行する性質上、production で noisy にならないよう抑制)

### Singleflight

- 同 `(host, username)` への並行 fetch を fold
- 同 user のプロフィールが複数 frontend session から同時アクセスされても remote に投げる request は 1 本

## 5. 利用フロー

1. mk-go の `internal/api/users/handler.go` の `Show` 関数で `(*entity.Packer).PackUserDetailed` 直後に呼ぶ
2. `bundle.User.Host != nil && *bundle.User.Host != ""` なら `h.remoteStatsFetcher.Fetch(ctx, host, username)` 呼び出し
3. 戻り値 `*RemoteUserStatsView` が non-nil なら `detailed.NotesCount` / `FollowersCount` / `FollowingCount` を上書き

`internal/server/wiring_accounts.go` の wiring (#3901 より前は `router.go`) で `corefederation.NewRemoteStatsFetcher(s.config.AllowedPrivateNetworks, s.config.UserAgent, s.outboundOpts()...)` を生成して adapter で `users.Handler` に注入。

## 6. 関連

- PR: #944 (#943 RemoteStatsFetcher 本体), #946 (#945 LRU cache 化)
- 関連 doc: [docs/federation.md](../federation.md) の「RemoteStatsFetcher」section
- 関連 issue: #949 (ドキュメント更新親 tracker)
- safehttp pattern: [docs/architecture.md](../architecture.md) の `internal/safehttp/`
