# Policy数値の下流表現境界設計

Issue: #2672

## 目的

native role policyから受け取った数値を`time.Duration`、byte数、件数、ID timestampなどの固定幅表現へ変換するとき、overflowによる符号反転、上限checkの迂回、表現範囲外factorの実質的な無期限拒否化、ID生成panicを防ぐ。

policy集約時のhost `int`精度、小数値、通常範囲の単位と結果は変更しない。effective-policy provider、`chunkedUploadEnabled`、policyを返す3経路のserver cap整合は本設計の対象外とする。

## 作業単位

#2672を1 Issue・1 PRで実装する。分離元commit `468cf08c`と`4209e9c4`はforkの`wip/2672-arith`へ退避済みだが、そのbranchをそのままmergeまたはcherry-pickしない。最新`develop`基点の専用branchへ、数値境界に必要な変更とtestだけを依存順に再構成する。

## 境界

`internal/safemath`はpolicyの集約には使わない。次の固定幅表現へ変換する直前だけで使う。

- `time.Duration`
- byte数と容量合計
- 件数合計と残数
- rate limitの`int`と`time.Duration`
- ID形式が持つtimestamp field

これにより、native role policyの比較精度と型は既存契約のまま保ち、consumer固有の表現限界だけを局所化する。

## safemath API

### float変換と乗算

`Float64ToInt`と`Float64ToInt64`は有限値をzero方向へtruncateする。上限以上と正のinfinityは対応する最大値、下限以下と負のinfinityは対応する最小値へ飽和する。NaNは`0`へ倒す。

`MulFloat64(value, unit)`はfloat64の積へ同じ規則を適用して`int64`を返す。`unit`の符号は限定しない。積がNaNなら`0`、±infinityまたは範囲外なら符号に対応する境界へ飽和する。

### 加算と符号反転

`NegateInt64(MinInt64)`は、正方向に表現できる最も近い値である`MaxInt64`を返す。それ以外は通常の符号反転とする。

`AddInt64`は引数を順に加算し、最初に範囲を超えた方向の境界へ飽和する。以後wrapさせない。

`SumExceedsInt64(limit, values...)`はbyte数など非負値の合計専用とする。加算途中の正方向overflowは、すべての表現可能なlimitより大きいため、必ず`true`を返す。負値が渡された場合もcall-site contract違反として`true`へ倒し、負値による上限迂回を許さない。

## consumer別設計

### Role時間

`role.PolicyMinutes`は、正規化済みpolicy数値を分から`time.Duration`へ変換するときに`MulFloat64`を使う。通常値、小数分、既存の`(duration, ok)`契約は維持する。

### Driveと分割upload

`maxFileSizeMb`と`driveCapacityMb`は`policyLimitBytes`で、`chunkedUploadMaxPendingMb`は`policyMegabytes`でbyteへ変換する。policyが有効な正数なら、範囲外の大値は`MaxInt64`へ飽和する。0以下の扱いは2つで違う。`maxFileSizeMb`と`driveCapacityMb`は本家と同じく、0は空でない本体を、負の値は0バイトの本体も拒む (`policyLimitBytes`は0なら0、負なら-1を返す)。上限なしになるのはpolicyが無いときだけ。Elythia独自の`chunkedUploadMaxPendingMb`は0以下を上限なしと読み、インスタンスの上限があればその値に丸める。

容量判定では`usage + pending + request size`を直接加算せず、`SumExceedsInt64`で上限超過を判定する。overflowはupload許可ではなく既存の容量超過errorへ合流させる。

multipart partsは`(size + chunkSize - 1) / chunkSize`を使わない。商を求め、剰余が非zeroの場合だけ1を加える。これによりceil division前の加算overflowを防ぐ。

### Invite

invite cycleを現在時刻から引く処理は、すべて`NegateInt64`を経由して同じ表現に統一する。

負の`inviteLimitCycle`はupstream互換のまま許容し、未来のcutoffになる。0へのclampは通常の負値に対する既存挙動とupstream互換性を変えるため、本設計では行わない。

招待上限の`float64`から`int64`への変換は`Float64ToInt64`を使う。`MulFloat64(value, 1)`を変換APIとして流用しない。上限から既存件数を引く処理は、件数の符号反転と`AddInt64`を使い、overflow後に残数が増えないようにする。最終結果の0下限は維持する。

### Import件数

現在件数とimport予定件数は`AddInt64`で加算する。正方向overflowは最大値へ飽和するため、limit checkを迂回しない。

### ID timestamp

各generatorはtimestampをwire表現の範囲へclampしてから符号化する。

| ID | 最小 | 最大 |
|---|---:|---:|
| AID / AIDX | 2000-01-01 epoch | base36 8桁の最大millisecond |
| MEID | `-(1 << 47)` ms | `(1 << 47) - 1` ms |
| ObjectID | Unix 0秒 | `(1 << 32) - 1`秒 |
| ULID | Unix 0 ms | `(1 << 48) - 1` ms |

範囲外入力でも固定桁を維持し、末尾切り捨て、符号化wrap、`ulid.MustNew`のtimestamp panicを起こさない。範囲内のID形式とparse結果は変更しない。

AID/AIDXの上限超過を最大値へ飽和する挙動は、安全側の意図的なupstream乖離とする。upstream AIDは固定長を超え、AIDXはtimestamp下位8桁へwrapするため、mk-goは固定長と時系列順序の維持を優先する。MEIDはupstreamの負時刻0 clampではなく、既存mk-goのsigned-offset round-tripを維持し、その12桁timestamp fieldの表現限界で飽和する。

### Rate limit

`scaledMax(base, factor)`は`base / factor`を`Float64ToInt`で変換する。極小のpositive factorで結果がhost `int`を超える場合は`MaxInt`へ飽和し、factorが小さいほど緩和される除数semanticsを維持する。計算結果が1未満なら既存どおり1へclampする。

`scaledMinInterval(base, factor)`は正方向overflowを`MaxInt64` durationへ変えない。乗算前に表現可能性を確認し、表現範囲を超える場合は`base`へ戻す。これは表現範囲外factorがoverflowまたは`MaxInt64` durationへの飽和によって実質永久拒否へ変わる経路を防ぐ。

一方、`base=1s`、`factor=9223372036`のように表現可能範囲内で約292年になるintervalは、このguardの対象外である。運用上許容する最大windowは既存endpointの`limit.Duration`との関係を含む別の契約決定が必要なため、follow-up Issue #2679で上限を定義する。

factorが0以下の場合は両関数とも既存どおり`base`を返す。NaNも不正値として`base`へ戻す。通常のpositive finite factorでは既存の除算・乗算結果を維持する。

## Error処理

新しいclient-facing errorは追加しない。overflowは既存consumerのerrorへ合流させる。

- file size: `ErrMaxFileSizeExceeded`
- drive capacity: `ErrNoFreeSpace`
- pending chunked upload: `ErrPendingUploadLimitExceeded`
- multipart parts: `ErrInvalidUploadSize`

DB、repository、API errorの分類は変更しない。内部数値や入力値を新たにlogまたはclient responseへ出さない。

## Plugin向け文書

`docs/plugins/authoring.md`へ、pluginが返すpolicy数値はconsumer固有の固定幅表現へ変換する時点で飽和することを記録する。providerの公開型、policy集約規則、通常範囲の値は変更しない。

## Test戦略

実装はTDDで進める。各consumerで、既存native role policyだけから問題へ到達するtestを先に追加し、旧実装で期待した理由により失敗することを確認する。providerはsetupにも使用しない。

### safemath

table-driven testで次を固定する。

- `MinInt64`、`MaxInt64`と境界直前
- NaN、正負infinity、floatのhost int / int64境界
- 加算の正負overflowと通常値
- 非負sumのoverflow、limit一致、負値防御

### consumer

- role: 分からdurationへの通常値、小数、正負境界
- drive: file size、容量、pending合計の通常値とoverflow
- chunked upload: 最大size付近のparts ceil division
- invite: handler requestからrepository cutoffとresponseまで。直接handler testでcycle、limit、`MinInt64` negationを検証
- import: 大きなcurrent countとimport件数でlimitを迂回しない
- ID: 各形式のmin/max直前、境界、範囲外、parse round-trip
- rate limit: 通常factor、極小factor、正方向overflow、NaN、0以下

mutationとして、飽和を通常演算へ戻す、`MinInt64`特例を削る、rate limit overflowを`MaxInt64`へ変える、ID clampを削る変更が対応testで失敗することを確認する。

## Review対応

PR #2675のreviewではproduction behaviorを追加変更せず、公開面、説明、portabilityを設計どおりに揃える。

- callerのない`MulInt`と`MulInt64`および専用testを削除する
- `internal/safemath`へpackage commentと、残るexported関数6個の英語GoDocを追加する
- `internal/misc/id`へwire timestamp幅、変換前に`time.Time`で比較する理由、MEIDのsigned-offset維持理由を記録する
- AID/AIDXの上限飽和を`docs/divergence.md`の安全側乖離へ追加する
- `DriveFile.Size`へ入れるtest fixtureはhost幅の`math.MaxInt`を使い、`GOARCH=386`でもcompileできるようにする
- `policyMegabytes`のboolが有効な正の上限を表すことを説明し、chunked uploadの古い`policyNumber`参照を直す
- `scaledMax`のコメントへNaNを含め、同じhunkで外した既存の強調表記を戻す

これらは通常範囲、境界値、client-facing error、upstream互換の挙動を変えない。

## Verification

- affected packagesのLinux race testとcoverage
- repository-wide Linux buildとvet
- repository-wide gofmt
- `GOARCH=386`の`safemath`、role、drive、ID test
- repository gates
- 各commitのLinux build

PostgreSQLを使うtestはfresh PostgreSQL 18へ接続し、検証後にcontainer、network、volumeを撤去する。

## Commit構成

各commitを単体build可能にし、次の依存順で分ける。

1. safemath APIと直接test
2. ID timestamp clampとtest
3. role / drive / chunked upload境界とtest
4. invite / import境界とtest
5. rate limit契約、consumer向けdoc、最終verification

## 対象外

- effective-policy providerとPR #2619の履歴
- `chunkedUploadEnabled`のpolicy反映
- `/api/signup`、`/api/meta`、`/api/i`のserver cap整合 (#2673)
- role persistence error分類 (#2625)
- schemaまたはmigration
