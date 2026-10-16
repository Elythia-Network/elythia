# #3464 次段階の Issue 追記案

2026-10-16 時点のローカル草案。GitHub への Issue 作成・編集はしていない。

## 起票状況と最小スコープ

- 配送は既存の [#3569](https://github.com/Elythia-Network/elythia/issues/3569) が open。[#3630](https://github.com/Elythia-Network/elythia/pull/3630) は merge 済みで、隔離 receiver の署名・actor/key・expected tuple・canonical digest 検証と、件数の妥当性判定を追加している。したがって receiver や比較基盤を作り直さず、per-run artifact と一意到着 throughput の不足分だけを #3569 の続きとして扱う。
- リモート画像は既存の [#3570](https://github.com/Elythia-Network/elythia/issues/3570) が open で、隔離 origin、cold/warm、サイズ・形式、失敗系、resource 計測の相談を含む。新規起票で重ねず、以下の対象ファイルと安全条件を実装前の追記候補にする。
- base/head 交互実行、resource 時系列、bench-compare workflow とレポートは既存の `tests/bench/ab`、`tools/benchcompare`、[`docs/bench-compare.md`](bench-compare.md) を再利用する。nightly 混合・長時間負荷と性能判定基準は今回の対象外。

## 配送: #3569 への追記案

### 件名案

配送 run ごとの一意到着数と処理量を bench-compare artifact に記録する

### 背景・対象

[#3630](https://github.com/Elythia-Network/elythia/pull/3630) で受信側の署名・本文検証と一意 ID 集計が入った。次は既存 outbound driver の base/head run からその結果を run 単位で保存し、enqueue や再送試行ではなく、receiver が確定した一意な正常到着数を処理量として参照表示したい。

変更候補は `tests/bench/ab/run.sh`、`tests/bench/queue/compose.yml`、`tests/bench/queue/common/seed.py`、`tests/bench/queue/common/driver_outbound.py`、`tools/benchcompare/{load.go,judge.go,render.go}` と対応する単体試験。比較器、HTTP stack、resource sampler は既存実装を拡張する。

### 最小範囲案

- 明示的な配送モードだけで、各 `rN-side/` に receiver の raw counters、status/reasons、`throughputJobs`、`drainSeconds`、`throughputJobsPerSecond` を含む `delivery.json` を保存する。
- 合成 sender・follower と Compose 内 receiver のみを使う。正常系の計測と、不正署名・内容不一致・重複・未到着の correctness 確認を分ける。
- throughput は `unique / drainSeconds` と照合する。重複 attempt は `verified` に残しても処理量へ重ねず、性能の判定閾値は設けず「参考値」として表示する。
- 準備失敗、manifest 未確定、受信統計の欠測、期待数不足は成功扱いにしない。配送モード有効時に side/round の artifact が欠けた比較は比較不能とする。

### 完了条件案

1. 2 notes × 2 followers の合成 fixture で期待 tuple が 4 件となり、署名・内容検証済みの `unique=4`、`missing=0` を記録する。
2. 重複 attempt がある試験では `duplicates` を別記録し、throughput は `unique / drainSeconds` と一致する。
3. invalid、content mismatch、unexpected、timeout、準備失敗、未確定、欠測は raw counters と理由を残し、valid な性能比較として扱わない。
4. 配送を有効にした全 side/round に artifact が必要であることを単体試験で確認する。配送 artifact を持たない既存 run は、新しい throughput 項目がないことだけを理由に比較不能にしない。
5. 同一 commit の小規模 A/A で成果物と表示を確認する。throughput は参考値で、merge blocker や新しい閾値にはしない。

### 対象外

nightly/長時間・混合負荷、性能劣化の閾値、実インスタンスへの配送、認証情報や実ユーザーデータの利用。

## リモート画像: #3570 への追記案

### 件名案

隔離 fixture で remote image の cold/warm 取得と失敗を測る

### 背景・対象

既存の [#3570](https://github.com/Elythia-Network/elythia/issues/3570) の相談を維持し、実装を始める前に対象経路と隔離方法を決める。候補経路の根拠は `internal/core/mediaproxy/service.go` と `internal/core/mediaproxy/service_test.go`。既存の比較・resource 記録は `tests/bench/ab` と `tools/benchcompare` を再利用する。

### 最小範囲案

- 実サーバーへ接続しない Compose 内 origin と、サイズ・形式・内容 digest を固定した合成画像を用意する。
- 初回は対象経路を一つに絞る。cold は専用 fixture cache を未取得にし、warm は同じ画像を事前取得して状態を明示する。origin request 数で cache 状態を検証する。
- 画像サイズ・形式・同時取得数を固定し、取得成功/失敗、HTTP status、再試行数、origin request/bytes、取得遅延、app の CPU・memory 時系列を記録する。
- 遅延・timeout・429/5xx・破損画像などの異常系は正常系の性能値と分け、失敗を速い成功として集計しない。
- SSRF、DNS/redirect 検証、allowlist、認可、取得 byte 上限、画素数上限、CPU 制限を維持する。fixture 接続に private-network allowlist の例外が必要なら、変更前に明示して相談する。今回の準備では例外も保護緩和も実装しない。

### 完了条件案

1. fixture の Content-Type・bytes・digest（変換経路は定義した変換後の期待値）を照合し、不一致を失敗として識別できる。
2. cold/warm それぞれで origin request 数を記録し、cache 状態が想定どおりだったことを検証できる。
3. 各取得操作の成功/失敗・status と、origin request/retry 数を区別して記録する。
4. 固定条件、commit、画像サイズ・形式、各指標、resource 時系列を Summary/artifact に残し、小規模 A/A で再現性を確認する。p99 は参考値とする。
5. 本番 URL、実ユーザーデータ、credential を使わず、SSRF 等の保護設定を弱めない。

### 相談してから決める項目

remote attachment の取り込み・cache と media proxy のどちらを初回対象にするか、cache reset 方法、fixture の private-network 許可要否、画像サイズ・同時取得数、異常系を初回に含めるか、手動 workflow の実行時間。

nightly 混合・長時間負荷と性能判定基準は配送・画像の小規模測定後に別途相談する。
