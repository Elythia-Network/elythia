# Bench profiling (pprof)

`tests/bench/http/compose.yml` で `make bench-up && make bench-run` を実行すると、
k6 シナリオと並走して Elythia の pprof profile が `tests/bench/http/results/profiles/` に
書き出される。`#413` チューニングロードマップの実測根拠として使う。

Misskey TS 側はここでは取らない (Node.js は別ツール、本ロードマップは Go 側が対象)。

## 仕組み

| Component | 役割 |
|-----------|------|
| `app-mkgo` (`MK_ENABLEPPROF=true`) | `internal/server/router.go` の `/debug/pprof/*` を bench 限定で公開 |
| `profile-collector-mkgo` | k6-mkgo と同時に起動し、pprof endpoint を curl で叩いて保存 |
| `compare` | `report.md` に profile ファイル一覧と `go tool pprof` 起動例を追記 |

`profile-collector.sh` は k6 が ramp-up した直後から、6 シナリオ各々の steady-state
(`PROFILE_SECONDS=25` 秒) を CPU profile として撮る。前後で heap / allocs /
goroutine snapshot も取得する。

## 出力ファイル

| File | 内容 |
|------|------|
| `cpu-<scenario>.pb.gz` | 各 k6 シナリオ steady-state 25 秒の CPU profile |
| `heap-pre.pb.gz` / `heap-post.pb.gz` | ベンチ前後の heap snapshot |
| `allocs-post.pb.gz` | ベンチ全体の累積 allocation |
| `goroutine-pre.pb.gz` / `goroutine-post.pb.gz` | goroutine 状態 (leak 検出) |

## 使い方

```sh
make bench-up
make bench-run
ls tests/bench/http/results/profiles/

# scenario ごとの CPU hot path を見る
go tool pprof -http :8080 tests/bench/http/results/profiles/cpu-users-show.pb.gz

# heap の steady-state allocation を見る
go tool pprof -http :8080 tests/bench/http/results/profiles/heap-post.pb.gz

# allocator の累積 hot path
go tool pprof -http :8080 tests/bench/http/results/profiles/allocs-post.pb.gz

make bench-down
```

`-http :8080` で起動するとブラウザで flame graph / top / source view を切り替えられる。

## 環境変数

`profile-collector-mkgo` で上書き可能:

| 変数 | 既定 | 用途 |
|------|------|------|
| `HOST` | `app-mkgo:3000` | pprof endpoint の host:port |
| `SCENARIO_DURATION` | `31` | k6 シナリオ 1 つの長さ (秒)。`tests/bench/http/k6/lib/config.js` と一致させる |
| `PROFILE_SECONDS` | `25` | 1 シナリオあたりの CPU profile 採取秒数 |
| `SCENARIOS` | `ping meta local-timeline users-show i notes-create` | profile 名に使うシナリオ列 |

## 本番では絶対に有効化しないこと

`MK_ENABLEPPROF=true` は profile を取るために `/debug/pprof/*` を公開する。
本番設定ファイル (`.config/default.yml`) では `enablePprof` を設定しない (= false)。
config 読み込み時に有効だと警告ログが出る:

```
config: EnablePprof is enabled; /debug/pprof/* endpoints expose runtime internals.
DO NOT enable this in production.
```
