# 純正 Misskey との差分: 運用・性能

[差分カタログの目次](../divergence.md) にある分類と基準 (Elythia と追従している本家の版) は、このファイルにも当てはまる。

## 5. 運用・性能機能 (Elythia 独自)

| 項目 | 内容 |
|---|---|
| Redis timeline / antenna の宙吊り ID 除去 | 読み取り時に解決できなかった ID を Redis から取り除く (timeline は #2715 / PR #2718、antenna は #2719)。**upstream は取り除かない** — timeline (`FanoutTimelineEndpointService`) も antenna (`server/api/endpoints/antennas/notes.ts`、こちらは `FanoutTimelineEndpointService` を通らず生の `note.id IN (...)`) も、Redis から取った ID を hydrate して引けなかったぶんを黙って落とすだけ。`FanoutTimelineService.remove` の呼び出し元は `antennas/remove-note` (ユーザー操作) しか無い。**antenna で効く理由は DB fallback が無いこと。** timeline は件数不足時に DB へ fallback するが (`meta.enableFanoutTimelineDbFallback` を off にすると止まる、§5.6)、antenna の読み取りは Redis の ID だけで完結する。押し出し自体は両方にある (antenna は `pushNote` が毎回 `ZRemRangeByRank`、timeline は 10% 確率の `LTrim`) が、**マッチが止まった antenna では新着が積まれないので宙吊り ID が残り続ける**。読み取り窓 (`limit*2`) が全部宙吊りだとページが空で返り、クライアントは次の `untilId` を得られず行き止まりになる。解消は 1 リクエストあたり窓 1 つぶん。**除去は filter を掛ける前の集合で判定する** — visibility / mute / block で落ちた note は生きているため。**消す前に primary で存在を確かめる** (`ExistingNoteIDsOnPrimary`、`dbresolver.Write` で primary 固定) — Elythia はリードレプリカを対応しており (`dbReplications`、既定 `false`)、複製前の行は通常の SELECT で引けない。ID に埋め込まれた時刻での猶予判定は使えない: リモート note の ID は AP の `published` から発番されるため「たった今 INSERT されたが ID の時刻は数時間前」が普通に起きる。**timeline 側も同じ確認を通す** (#2757)。DB fallback があるから安全とは言えない — fallback は Hybrid 以外では別メソッドで、`allowPartial: true` を渡すクライアントには 1 件でも解決できたページでは走らず (0 件のときだけ走る、#3448)、`enableFanoutTimelineDbFallback` を off にすれば運用側でも止まり (global を除く、§5.6)、Redis list から消えた ID は戻らない |
| inbox verify-in-worker 化 | HTTP handler は body + signature header を payload 化して即 202、署名 verify / host block / instance touch は worker 側。HTTP 受信 rps が **TS の 2.6〜2.8 倍** |
| mkq queue driver | BullMQ wire 互換の Go 実装で、**Elythia 唯一の queue driver** (legacy の asynq は #2985 で削除)。queue-bench の当時の 3-way 比較では送信 rps が mkq 優位、drain time は asynq 優位だった (詳細は [queue-bench.md](../queue-bench.md)) |
| AIMD auto-scale worker | per-queue の動的 Resize + Prometheus metrics。worker 現在数 / 範囲 / scale 履歴は admin UI にも出す (#2277) |
| 落ちた配送先へのブレーカー | 接続失敗か 5xx が 5 回続いた配送先へは送らず、**試行回数を消費せずに**遅延させる (#3048、mkq の `DelayedError` = BullMQ の `moveToDelayed(skipAttempt)`)。次に試す時刻が来たら 1 件だけ通し (Redis のロックで queue ノードをまたいで 1 件)、何らかの HTTP 応答があれば閉じ、**試行が**失敗したときだけ間隔を倍にする (1 分から 1 時間。開いた瞬間に飛んでいた配送の失敗は倍化に数えない)。**開いている間の失敗も試行回数を消費させない** — 消費させると溜まったジョブが試行のたびに持ち回りで回数を失い、下の上限より前に捨てられる。待たせる時間は 5 分で区切り (試行中は 15 秒。どちらも一斉に起きないよう最大 1 割 / 5 割ずらすので、実際の上限は 5 分 30 秒 / 22.5 秒)、起きるたびに判定し直すので、試行が通ったり手で閉じたりすれば数分以内に流れる。試行には札を持たせ、札が一致する試行の結果だけが枠を外し間隔を倍にする (ロックが切れた後に返ってきた古い試行が、次の試行を邪魔しない)。こちら側の失敗で終わった試行は枠だけ返す。署名鍵の読み込みなど HTTP の送信まで至らない失敗 (こちら側の事情) は数えない。429 を返した相手にはブレーカーを開かず、`Retry-After` の間 (既定 60 秒、上限 1 時間) ほかのジョブを待たせ、待たせるときに「止めている期限」と「最後の予約 + 0.5 秒」の遅い方を送信時刻として予約させる (429 を受けたジョブ自体は従来どおり試行回数を消費する)。予約はジョブごと (payload のハッシュ) に覚え、**待たせる時間はここでも 5 分で区切る** — 起きたジョブは自分の予約まで待ち直す (並び直さない)。区切らないと、予約の列 (溜まった件数 x 0.5 秒、1 万件で 1.4 時間) を手で閉じても遅延中のジョブを起こせない。予約の最後尾 (送り終わる見込み) は `delivery-health` の `breakers` に `reservedUntil` として出し、`close-delivery-breaker` で 429 の停止と予約ごと消せる (自動で閉じる = 成功のときは消さない。送り出し中の配送はどれも成功で終わるので、消すと残りが一斉に起きる)。**予約は絶対時刻で 1 本の列にする** — 429 は並行して何本も返るので、429 のたびに順番を数え直す形だと同じ順番が重なって明けた直後に一斉に起き、送り出しの途中で再発すると古い予約と新しい予約が 2 本並んで流れた (#3048 の敵対的レビュー 3 周目)。**各ジョブは自分の時刻に 1 回だけ起きて送る** — 明けた瞬間に一斉に送るとまた 429 になる。最初は「明けた後の一定時間だけ 0.5 秒に 1 件へ絞る」形にしたが、待たされたジョブが起きては待ち直しを繰り返し、絞る期間が切れた瞬間に残りを一斉に送る回帰を作ったので、予約方式に改めた (#3048 の敵対的レビュー)。明けた後に新しく来た配送は間隔の対象外 (普段どおりの流量)。4xx / 410 は相手が健在なので対象外。**判断は取り出し時にする** — 生成側 (fanout) で止めると、閉じたときに配送が復活しない。Delete も同じく遅延させる (相手が落ちている間は迂回しても届かない)。**独自の上限は置かない** — 開いている間も 1 時間に 1 回は試すので `isNotResponding` の記録が続き、7 日で既存の自動停止 (`autoSuspendedForNotResponding`、upstream 互換) に入って、溜まったジョブは配送時の判定で捨てられる。状態は Redis (`apDeliveryBreaker:*`) に置き、admin の `delivery-health` から見て `close-delivery-breaker` で手で閉じられる。**upstream は落ちた相手へも `deliverJobMaxAttempts` (既定 12) 回まで撃ち続ける**。遅延中のジョブは試行回数が 0 のままなので、`admin/queue/*` では再試行待ちではなく予約 (delayed) として見える。遅延は queue の処理時間・成否の観測 (Prometheus / runtimestats) に数えない |
| Prometheus `/metrics` | `mk_job_workers_active` / `mk_job_queue_pending` / `mk_job_dispatch_wait_seconds` ほか。**無認証公開なので LB/nginx ACL 必須**。admin から読めない分は `admin/queue/*` の `runtime` block が補う (#2277) |
| `admin/server-metrics` | Elythia プロセス自身の統計 (goroutine / heap / GC / uptime / version) を返す Elythia 独自 endpoint (#2395)。upstream に対応物は無い。`admin/server-info` はホストマシンの静的スペックを返すもので別物。control panel のダッシュボードから 10s ポーリングで表示する (`ReadMemStats` が stop-the-world を伴うため間隔を詰めない)。DB / Redis の接続プールは当初含めていたが、常時ほぼ一定で画面のノイズになるため UI ごと落とした |
| `admin/drive/usage` | インスタンス全体のドライブ使用量と内訳を返す Elythia 独自 endpoint (#3053)。upstream は per-user の `driveCapacityMb` しか持たず、**合計を出す口が無い**。返すのは ローカル / リモートの別、種類別 (添付・アバター・バナー・カスタム絵文字・その他)、ホスト別と利用者別の上位 30。**返すのは `drive_file.size` の合計 = 「DB が把握している量」で、object storage に実際に置かれている量ではない** (削除の失敗や孤児があれば必ずずれる)。応答の `source` に `database` と入れ、同梱フロントエンドは同じ趣旨を常時表示する (画面は `source` の値を読まず固定の文言を出しているので、値を増やすときは画面も直すこと)。実ストレージ側を出すなら S3 の API を叩く別経路が要る。**種類は「その file を誰が指しているか」で決め、avatar → banner → emoji → attachment → other の優先順で 1 つに割り当てる** (1 つの file が avatar と banner の両方に指されうるので、順序が無いと二重計上になる)。**`attachment` は「note に添付済み」ではない** — 利用者所有の実体すべてで、一度も添付していない file も入る。note からの参照で絞らないのは、(a) 合成データ 2,005,400 行で GIN 経由の EXISTS が 4.2 秒・note 側から unnest しても 1.1 秒掛かるうえ、(b) drive_file を指すのは note だけではない (`note_draft` / `gallery_post` / `chat_message`) ので、note だけで「未参照」を出すと**消してよい量を過大に見せる**ため。孤児の検出は別 issue。**集計は都度走らせる** — 実測は運用中のインスタンス (`drive_file` 74,757 行 / `user` 38,694 / `emoji` 20,907) で 3 本合計 225 ms、合成データ 2,005,400 行で 1.84 秒。定期集計のジョブは持たない。**emoji の突き合わせを `IN` や `EXISTS` に書き換えないこと** — 同じインスタンスで種類別クエリ 1 本だけを差し替えて測ると、現在の `LEFT JOIN` が 195 ms に対し `IN (SELECT ... UNION ...)` が 56-68 秒、相関 `EXISTS` が 153-167 秒だった。代わりに 5 分の TTL でスナップショットを 1 つ持ち、同時要求は singleflight で 1 本に畳む。`forceRecalc` で明示的に取り直せる。**集計元が未配線なら 500** — 0 バイトを返すと「使っていない」という誤った事実を管理画面に出すことになる。**リモート側は count と linkCount が一致し size は 0 になる** — §5.5 の「リモートメディアをローカルにキャッシュしない」が**インスタンス全体の合計として**読める場所 (`admin/drive/files` でも 1 件ずつの `size` は見えるが、合計は出ない)。TS 由来の DB から引き継いだ実体つきリモート行だけがそこから外れる。**同梱フロントエンドはリモートを `size > 0` のときだけ描く** (`2026.9.0-mk.32a`) が、API は常に返す |
| timeline JSON cache | first-page per-viewer cache (opt-in) |
| mediaproxy のアニメ pass-through | `?emoji` / `?avatar` / `?preview` で gif / apng / **アニメーション WebP** を decode せず raw 返し (Go std の `image.Decode` は 1 frame しか返さず静止画化するため)。**WebP は MIME では判定できない** (アニメーションも静止画も `image/webp`) ので、RIFF コンテナを歩いて `VP8X` の ANIMATION フラグか `ANIM` / `ANMF` チャンクを見る (`imagedecode.IsAnimatedWebP`、#3128)。`static=1` (利用者の「アニメーション画像を再生しない」設定) と `static` / `badge` mode は従来どおり静止化する。**upstream と食い違う向きは mode ごとに違う。** `?emoji` / `?avatar` は upstream も `{animated: !('static' in query)}` で全コマ resize するので、Elythia は raw を返すぶん縮小が効かない (最大 32MiB がそのまま出る)。一方 **`?preview` は upstream も静止画** (`FileServerProxyHandler.ts` の preview 枝は `{animated}` を渡さない) なので、そこだけ Elythia のほうがアニメーションを返す。**APNG に差は無い** — upstream の `sharp-animation-convertible-image-with-bmp` は apng を含まず生返しになる。**animated AVIF はまだ拾えない** — ISOBMFF を歩いて複数フレームを見る必要があり、RIFF 走査とは別物。**ローカル保存の構成では届かない** — `resolveLocal` の `swapToVariant` が resize 系 mode で先に サムネイル (常に静止 WebP) へ差し替えるため、この判定までバイト列が来ない。GIF / APNG も同じで、オブジェクトストレージ構成 (URL が `/files/` で始まらない) でのみ効く |
| アニメーション画像の webpublic | **作らない** (= 他人に見せる側も原本のまま) — `encodeWebP` は 1 枚しか受けないので、作ると 1 コマの静止画になる。upstream も `isAnimated` なら作らない (`DriveService.ts`) が、**判定の仕方が違う**。upstream は `metadata.pages > 1` という中身の判定で、Elythia は GIF / APNG を MIME で、WebP を RIFF コンテナの走査で見る (`imagedecode.IsAnimatedWebP`、#3128)。このため**乖離が 2 方向に出る**: (a) **メタデータを持つものには作る** — アニメーションを保つために撮影情報を残すのは割に合わないので、upstream より厳しい側に倒している (効くのは APNG とアニメーション WebP で、GIF は `hasStrippableMetadata` の対象外なのでこの枝に入らない)。(b) **animated AVIF は拾えず静止画になる** — ISOBMFF を歩く判定が要るうえ、「AVIF は必ず webpublic を作る」枝があるため寸法にもメタデータにも関係なく潰れる。**判定はデコードより前に置いてある** — バイト列だけで決まるので、作らないと決まっている入力をデコードしない |
| `user.avatarUrl` に入れる値 | **プロキシに通さない公開 URL** (`webpublicUrl ?? url`) を保存する。upstream は `getPublicUrl(avatar, 'avatar')` = **プロキシ URL (avatar mode、高さ 320)** を列に入れる (`i/update.ts`)。「原本ではなく公開用」という点は揃えたが、**プロキシ URL は保存しない** — Elythia の `ProxiedURL` は `sig=` に HMAC を付けるので、保存するとプロキシの secret を変えた瞬間に全員のアイコン URL が無効になる (upstream のプロキシは open で署名を持たないため同じ問題が起きない)。リモート origin の包み直しは packer 側 (`entity.PackUserLite` → `ProxyAvatarURL`) が行うので、**応答に出る値の leak-safe 性は変わらない**。結果として**ローカル利用者のアイコンは 320 に縮められず、公開用 (最大 2048px) のまま配られる** — 帯域は upstream より増える。リモートは従来どおり avatar mode のプロキシ URL になる。**`bannerUrl` は差が無い** — upstream も mode 無しの `getPublicUrl(banner)` なので `webpublicUrl ?? url` がそのまま入る (`internal/entity/mediaurl.go` の `ProxyBannerURL` が既に同じことを書いている) |
| URL preview の charset 自動正規化 | Content-Type + `<meta charset>` から UTF-8 化。Shift_JIS / EUC-JP / ISO-2022-JP で文字化けしない (upstream は外部 `summaly` package に委譲しているため同等機能の有無は未確認) |
| URL preview で取得元が決める文字列の長さ | upstream (summaly) が切るのは `title` 100 / `description` 300 だけで、`site_name` と URL 系 (`url` / `thumbnail` / `icon` / `activityPub`) は無制限。Elythia は **`site_name` も 100 で切り、URL 系は 2048 バイトを超えたら落とす** (切らずに落とす — 途中で切った URL は別の場所を指しうる)。**キャッシュの前提が違う** — upstream は 1 時間 / 100 件のメモリキャッシュだが、Elythia は 24 時間 Redis に載せ、クエリ文字列を変えれば URL は無限に作れる。上限が無いと 1 本あたり `urlPreviewMaximumContentLength` (既定 10MiB) までリニアに伸びる (実測) |
| instance touch buffer | 同一 remote host の連続 inbox 受信を集約。**upstream も `CollapsedQueue` で per-host に集約している**。差分は flush 窓が Elythia 1s / upstream 5 分という点だけ |
| instance のフォロー数の起動時再計算 | 起動のたびに `instance.followingCount` / `followersCount` を `following` テーブルから数え直す (`RecomputeFollowCounts`、#421)。**upstream には無い** — 本家は Follow / Unfollow のたびに増減する (`UserFollowingService.insertFollowingDoc` / `decrementFollowing`) だけ。数え方は本家の増減と同じ向きで、`followingCount` はその host の利用者 → ローカルの利用者、`followersCount` はローカルの利用者 → その host の利用者 (#3330 で向きを直した。それまでは逆で、TS 版が積んだ値も起動のたびに入れ替えていた)。**`meta.enableStatsForFederatedInstances` が false でも走る** — 本家ではこの設定を切ると 2 列が止まったまま古くなるが、Elythia は起動時に 1 回だけ実値へ揃える (Follow / Unfollow ごとの増減は本家と同じく止める)。**未登録の host には行を作らない** — 本家は `fetchOrRegister` で instance 行を作ってから増やすが、Elythia の増減は行が無ければ何もしない (リモートの利用者を取り込む時点で行は作られている)。**移行済みのアカウントが絡むフォロー行は数えない**。本家と一致するのは、**ローカルの利用者が移行済みのリモートアカウントをフォローしている行の `followersCount`** だけ — 本家は移行の時点で `adjustFollowingCounts` がその人数ぶんを引き、行は残す。それ以外の次の行は、本家では移行の後も数えたまま残るので、**除外すると Elythia の値が本家より小さくなる**: (a) 移行したリモートの旧アカウントがローカルの利用者をフォローしている行 (本家は利用者の `followersCount` だけを引き、instance の `followingCount` は引かない)、(b) ローカルのアカウントが移行したときの、そのアカウントとリモートの利用者の間の行 (本家は instance の列を調整せず、後で行が消えても `decrementFollowing` の移行済みガードで引かない)、(c) proxy アカウントが移行済みのリモートアカウントをフォローしている行 (本家は proxy を `localFollowerIds` から除くので、そのぶんを引かない)。移行の後に作られた、移行済みが絡むフォローは本家も数えないので一致する。除外を選んだのは、全部数えると (a) とは逆に、本家が引いた分まで起動のたびに戻すことになるため (#3330)。**host は `following` の非正規化列 (`followerHost` / `followeeHost`) で読み、1 本の UPDATE で値が変わる行だけを書く** (#3330)。以前は全行を 0 にしてから数え直していたので、起動のたびに全 instance 行を書き換え、トランザクションのあいだ全行のロックを握っていた。非正規化列は Elythia のフォロー行の作成経路 (`following.Service` の Follow / AcceptRequest) が初版から利用者の host を写しており、本家の instance chart (`tickMajor`) も同じ列で数えている |
| instance の `notesCount` / `usersCount` | 本家と同じく、リモートの投稿の作成・削除で `notesCount` を、リモートの利用者の新規取り込みで `usersCount` を `meta.enableStatsForFederatedInstances` の下で増減し、instance chart の notes / users はさらに `enableChartsForFederatedInstances` を見る (#3330。それまで Elythia は 2 列を動かしておらず、`usersCount` は instance 行を作ったときの 1 のままだった)。**書き込みを 30 秒の窓で合算する** (`instance.CounterBuffer`) — 本家は `notesCount` の加算だけを `CollapsedQueue` (本番 5 分) で合算し、減算と `usersCount` はその場で書く。窓の中の値はプロセスが落ちると失われる (本家の `CollapsedQueue` も同じ)。**数える経路は notes chart と同じ** — 作成は resolver が新しく作った投稿 (inbound の Create、返信元・引用・Announce の対象・featured などを解決のついでに取り込んだもの、リレー由来で ephemeral に置いたもの) と、inbound の Announce が作るリノート行。削除は `DeleteService` を通るもの (inbound の Delete など)。本家は `NoteCreateService` / `NoteDeleteService` を通る全経路で数える (#3330 の途中までは inbox の Create / Announce しか作成に数えず、削除は全部引いていたので値が下がり続けた)。**残る非対称**: (a) **Undo(Announce) によるリノートの取り消しは引かない** — `noteRepo.Delete` を直接呼び `DeleteService` を通らないため (本家は `NoteDeleteService` で引く)。(b) **ephemeral に置いた投稿は数えるが引かない** — DB に行が無いので Delete で見つからず、期限切れで ephemeral から消えたときも引かない。ephemeral だけにある投稿の分は増える一方になる (本家に ephemeral は無い)。後から直接配送で同じ投稿の DB 行ができたときは、ephemeral に入れたときに数えた分で済ませ、数え直さない。(c) **減算は 0 で止める** — 更新前に取り込んだ投稿 (`notesCount` が 0 のままの行) の削除で負にしない (本家は止めない)。chart の notes には下限が無いので、更新前の投稿の削除は chart を負に動かしうる。**instance 行の作成は設定に関係なく行う** — 本家は設定が false だと利用者の取り込みで instance 行を作らない (`fetchOrRegister` を呼ばない) が、Elythia は行を作り、`usersCount` は 0 のまま置く。**過去の値は自動では直さない** — #3330 より前に Elythia が作った instance 行の 2 列は実数と合っていない (`notesCount` は 0、`usersCount` は行を作ったときの 1 のまま)。再計算は note の該当範囲の走査になるので起動時には行わず、同梱バッチ `elythia backfill instance-counts` で運用者が数え直す (docs/deployment.md の「後始末バッチ」)。**数え直した値は本家の累積値ではなく、本家の instance chart の total (`tickMajor`) と同じ「いまの実件数」** — `notesCount` は `note."userHost"` の件数 (renote を含む)、`usersCount` は `user.host` の件数 (削除済み・凍結中を含む)。本家の累積値は、リモートのアカウント削除 (`DeleteAccountProcessorService`) で投稿と利用者の行を直接消しても引かないので実件数より大きくなりうるが、その履歴は DB に残らない。stats の設定にも関係なく数える |
| instance chart の requests | 本家と同じく `enableChartsForFederatedInstances` の下でだけ記録する (#3330。それまでは設定に関係なく記録していた)。**instance 行の有無は見ない** — 本家は `enableStatsForFederatedInstances` が false のとき行を作らずに `fetch` し、行が無い host の受信と配送成功を chart に載せない |
| 移行時の instance の `followersCount` | 本家 `AccountMoveService.adjustFollowingCounts` と同じく、旧アカウントがリモートなら、その instance の `followersCount` をローカルのフォロワー数ぶん減らし (`enableStatsForFederatedInstances` の下)、instance chart の followers を**人数に関わらず 1 だけ**減らす (本家の `updateFollowers(host, false)` 1 回をそのまま写した)。per-user following chart はフォロワーごとに減らすが、旧アカウントがリモートのときは他のフォロー系の chart と同じく `enableChartsForRemoteUser` も見る (本家は見ない) (#3330) |
| chart tick の DB 再集計 | **upstream も同機構を持つ** (`TickChartsProcessorService` / `ResyncChartsProcessorService`)。Elythia は cron 実装が異なるだけで差分ではない |
| VAPID 鍵の自動生成 | Service Worker 有効化時に鍵が両方空なら生成して meta に注入。operator 指定鍵は尊重。明示的な空 / null 送信は「ローテーション指示」として扱い再生成する。fork frontend は保存後に `admin/meta` を引き直して生成鍵を表示する (#2272) |
| `+host` / `-host` sort key | `federation/instances` の host 昇順/降順 |
| `signatureCapability` | `federation/instances` / `federation/show-instance` の additive field (#2393)。相手サーバーが対応する署名方式を「宣言 (actor の assertionMethod)」「受信観測 (verify に成功した鍵種別 / LD-Signature の受信)」「配送観測 (Ed25519 署名の配送が 2xx)」の 3 系統から判定して返す。観測が無い host は null。記録先は Elythia 独自の `instance_signature_capability` テーブルで、TS は本テーブルを認識しない。`federation/stats` は公開エンドポイントなので常に null (追加クエリを撃たない) |
| `notes` の noteIds bulk lookup | upstream の public-note timeline に加え `{noteIds:[...]}` bulk (max 100、visibility filter 付き) を同 endpoint で両立 |
| `webpublicUrl` | drive entity の拡張 field (proxy 化済で IP leak なし) |
| mention による reply filter escape | viewer が `note.mentions` に含まれれば withReplies 設定に関係なく reply gate を pass。streaming と fanout の両方に実装 |
| streaming publish 時の suspended フィルタ | 凍結ユーザー (本人 / reply 先 / renote 先) の note を WebSocket publish から除外する (#2624)。**upstream は streaming に suspended フィルタを持たない** (`packages/backend/src/server/api/stream/` に `isSuspended` の参照が無い)。upstream で顕在化しないのは suspended ユーザーが投稿できないためで、Elythia では**凍結したリモートユーザーの note を対象にした inbound Announce が相手インスタンスから届き続ける**ため、取得経路にしかフィルタが無いと「リアルタイムには流れるがリロードで消える」という食い違いになっていた。gate は `internal/stream` の publish 1 箇所に置く (home / local / global / userList / channel / hashtag / roleTimeline / antenna が全て同じ publisher を通る)。**Redis の timeline list には従来どおり積む** — fanout 側で打ち切ると凍結を解除しても list に ID が無いままになり、取得は list が limit を満たす限り DB へ fallback しないため復活しなくなる。あわせて channel 一覧 (`ListByChannelID`) と hashtag 一覧 (`SearchByTag`) にも同じ 3 author の除外を追加した (これらは `applyTimelineFilter` を通らないため、publish だけ止めると逆向きの食い違いになる) |
| effective-policy provider | build-time pluginがnative role解決へ動的に寄与するElythia独自機構。成功結果は明示的invalidationまでLRUへ保持する。寄与はnative roleやDBへ永続化されないため、plugin停止・buildからの除外・Misskey TSへの切り戻しで消え、利用者の実効権限が変わる。特に制限方向の寄与は切り戻しで権限を緩めうる。停止後も維持すべき判定はnative roleとして永続化し、切り戻し前に`admin/server-plugins`の`effectivePolicies`宣言とnative fallbackを確認する |
| トランザクションメールの l10n | `internal/l10n` で件名・本文・CTA ラベル・HTML wrapper の footer link 文言を出し分ける (#2986)。**upstream は backend のメール文面を locale 化していない** — `SignupApiService` / `request-reset-password` / `i/update-email` / `SigninService` / `CheckModeratorsActivityProcessorService` は英語固定 (`locales/*.yml` にも載っていない)。Elythia は初版で **ja / en** の 2 言語 (`normalizeKnown` が知らない `meta.langs` 値は無視する。`zh` 等を入れても効かない)。解決順は **認証済み利用者向け** (`reset-password` / `i/update-email` / new-login / モデレーター通知): `user_profile.lang` → `meta.langs` の先頭から知っている言語 → **手がかりが無いときは upstream 同様の日英併記** (new-login / モデレーター通知) または **英語** (reset / email 変更確認)。**`profile.lang` 未設定の既存利用者は多い** (client の言語設定から入る値なので空のままのことが多い) — 手がかりが無いとき new-login は併記のまま。**未登録の signup 確認**: `meta.langs` が空なら `Accept-Language` の先頭から知っている言語を直接採用、`meta.langs` があるときは突き合わせ → 上記と同じ fallback (手がかり無しは英語。pending 行に lang は無い)。モデレーター通知は**受信者ごと**に解決する。対象外は `admin/send-email`。`signin` / `resetpassword` の `SetMetaRepo` は `criticalWiring` に載せ、未配線を起動時に検出する。**#2986 以前からあった文面差は残る** — signup 確認は upstream 件名 `Signup` / 本文 `To complete signup…` に対し Elythia は `Confirm your account` / HTML CTA 付き (#600 item 4)、reset は upstream `Password reset requested` に対し Elythia は `Password reset`。明示的な手がかりがあるときだけ単一言語 (ja または en) に絞る。HTML footer の `Email setting` も `EmailSettingsLabel` で l10n する (併記時は `Email setting / メール設定`) |

## 5.5. リモートメディアをローカルにキャッシュしない (意図的)

upstream は `cacheRemoteFiles` が真のとき、連合で流れてきたメディアの実体を自サーバーの
Drive へ保存する。**Elythia はこれを実装しない。** 未実装ではなく意図的な設計判断。

### 挙動

| 対象 | upstream | Elythia |
|---|---|---|
| ノート添付 | `cacheRemoteFiles` が真なら実体を Drive へ保存 | **link 行のみ** (`isLink=true` / `size=0` / `md5=""`)。実 fetch しない |
| リモートの avatar / banner | Drive へ保存しうる | **URL 文字列を `user.avatarUrl` に持つだけ**。drive_file 行を作らない |
| 表示 | ローカルのキャッシュを配信 | メディアプロキシが都度取得して中継 (保存しない) |

`meta.cacheRemoteFiles` / `cacheRemoteSensitiveFiles` の**列と API field は残す**
(drop-in 互換のため)。値は保存・返却されるがダウンロード判定には使わない。
関連する admin UI は無効表示にして理由を出している (fork frontend)。

`admin/drive/clean-remote-files` も実装は残る。**Elythia が作った行に対しては常に 0 件** —
対象 (`isLink=false` の remote file) を作る経路が無いため。**TS 製の DB を引き継いだときだけ
対象がある** (あちらは `cacheRemoteFiles` が真なら実体を保存する)。

**行は消さず link に倒す** (#3102、upstream と同じ)。以前は `isLink=false` のリモート行を
**行ごと DELETE** していたが、upstream の `CleanRemoteFilesProcessorService` は
`deleteFileSync(file, true)` を呼び、`deletePostProcess` の `isExpired` 分岐が
**行を残して link に倒す**。行を消すと `note.fileIds` の指す先が無くなり、packer が引けない ID を
捨てるので**過去の投稿から添付が黙って消える**。実体 (object storage / ローカル FS) は
先に消すので、容量はどちらでも空く。

倒した後の行は **Elythia が普段作る link 行と同じ形**にする (`isLink=true` / `url=uri` /
`size=0` / `storedInternal=false` / access key なし)。**`size` を 0 にするのは upstream と違う** —
あちらは据え置くが、据え置くと実体が無いのに使用量に乗り続けて `admin/drive/usage` が嘘をつく
(upstream 自身も集計では `isLink = FALSE` で絞るので link 行の `size` は読まれない)。
access key を NULL にするのは、実体が消えた後も古い `/files/<key>` の URL を生かしておく意味が
無いため (upstream は新しい UUID を振り直して同じ効果を得ている)。`uri` を持たない行だけは
倒せないので消す (upstream の else 枝と同じ)。

**同梱フロントエンドは対象が実在するときだけボタンを有効にする** (#3102)。以前は無条件に
無効表示だったため、**引き継いだ運用者は消せるはずのものを UI から消せなかった** (文面は
「Elythia が作ったファイルに削除対象はありません。Misskey から引き継いだデータベースにだけ
対象が残ることがあり、その場合は API を直接呼ぶ必要があります」で、回避策は書いてあった)。
判定は `admin/drive/usage` (§5、#3053) の `remote.count - remote.linkCount`。
**`remote.size` では数えない** — あれはリモート行**全部**の合計で、削除対象だけの合計ではない
(純正は `expireOldFile` で link 化するとき `size` を据え置くので、引き継いだ DB には
「実体は無いのに size を持つ link 行」が溜まっている)。削除条件が `isLink = false` である以上、
同じ述語で数えたものしか一致しない。**集計を取れなかったときは押せるままにする** —
一時的な失敗を「対象が無い」と断定しない。「削除対象はありません」と言うのは 0 件と
**確かめたとき**だけ。

**確認ダイアログは件数と、取り消せないことを出す。容量は出さない** — 上の理由で
`admin/drive/usage` からは削除対象だけの容量を出せない (出すなら endpoint 側に
`isLink = false` の合計を足す必要がある)。

### 理由

1. **相手の削除の権利**。キャッシュを持つとそのコピーのライフサイクルを自分が所有する。
   相手が消しても、Delete 配送が届かない・連合が切れている・ノートは残してファイルだけ
   差し替えた等のケースでコピーが残り続ける
2. **リスク**。連合を流れてくる違法コンテンツが自ストレージに保存される。都度取得して
   中継するのとは実務上の立場が違う
3. ストレージ増加の抑制 (上の 2 つに比べれば副次的)

### キャッシュしないことの弱点と、その埋まり方

| 一般的な弱点 | Elythia の状況 |
|---|---|
| クライアントの IP が相手サーバーに漏れる | メディアプロキシが吸収する (drive / avatar / banner 等は server-proxy 経由) |
| 閲覧のたびに再取得して帯域を食う | プロキシ応答に `Cache-Control` (最長 3 日) が付き、CDN / ブラウザがキャッシュする |
| 相手サーバーが消えると表示が壊れる | **埋まらない。** キャッシュしない設計の本質的なトレードオフとして受け入れる |

なおエッジキャッシュにも複製は載るが、性質が違う。TTL で自動失効する一時的なインフラ層で
あって、バックアップにも入る永続レコードではない。「削除の権利」の観点ではこの差が効く。

### 影響する upstream 機能

  - `DriveService.expireOldFile` (容量超過時の LRU 退去) — **不要**。退去すべき実体が
    存在しない。実装しかけたが、キャッシュしない以上 dead code になるため破棄した
  - remote user への `driveCapacityMb` gate — 同様に意味を持たない。`size=0` の link 行は
    使用量に乗らない

この設計の効果は `admin/drive/usage` (§5、#3053) で数字として確かめられる。リモートの行は
`isLink = true` / `size = 0` なので、**件数は積み上がるのに使用量は 0 のまま**という形で出る。
逆に言うと、そこに 0 でない値が出るインスタンスは TS 製の DB を引き継いでおり、
`isLink = false` のリモート行が残っている (消し方は上の `clean-remote-files` の項)。
**ボタンが有効になるかは `size` ではなく `count - linkCount` で決まる** — 過去に
`expireOldFile` で link 化された行は `size` を持ったまま残るので、「使用量に 0 でない値が
出ているのにボタンは無効」も、その逆も成立する。

## 5.6. timeline の DB fallback を止めるつまみ

`meta.enableFanoutTimelineDbFallback` (admin から切り替え、既定 **on**) は
**FTT を止めずに、タイムライン取得の DB fallback だけを止める**つまみ。#2762 まで
Elythia は列を持つだけで読み取り経路から参照しておらず、off にしても何も起きなかった。

`enableFanoutTimeline` とは別物。あちらは push (fanout) と read の両方を止めて
DB 直行にする。**両方 off でも DB は引く** — upstream も
`if (!enableFanoutTimeline) return getFromDb()` を endpoint 側に持ち、そこでは
`useDbFallback` を見ない。

**off は珍しい状態ではない。** 同梱 frontend のセットアップウィザード
(`MkServerSetupWizard.vue`) は用途に「1 人用」以外を選ぶと
`enableFanoutTimelineDbFallback: false` を送る (`q_use === 'single'` が条件)。
**group / open で立てたインスタンスは、誰も設定を触っていなくても off**。

### off にすると何が起きるか

upstream の `FanoutTimelineEndpointService` は `useDbFallback` が偽のとき
`ps.dbFallback` を `() => Promise.resolve([])` に差し替える。Elythia も同じで、
DB では埋めない。足りない分は Redis の窓の奥を読み進めて埋め、読み切っても
足りなければ `limit` に満たないまま返す (下の「件数の埋め方」)。

| 状況 | on (既定) | off |
|---|---|---|
| Redis に `limit` 以上ある | Redis から返す。複数の list を混ぜる timeline では、全ての list が持っている範囲だけ (下の「複数の list を混ぜる timeline」) | Redis から返す (範囲で切らない) |
| Redis から解決できた分が `limit` 未満 | 足りない分を DB で継ぎ足す。ただし `allowPartial: true` (同梱 frontend は常に送る) なら、1 件も残らなかったときだけ | **Redis の窓の奥を読み進め、読み切ったらその分だけ返す** |
| Redis が空 | 全ページを DB から返す | **空を返す** |
| `sinceId` を含むページング | 全ページを DB が処理する | **空を返す** |

最後の行が実際に効く。`sinceId` を含むページングは Redis に十分な ID があっても
必ず DB へ倒れる (`shouldFallbackToDb` が常に真、#2720 で upstream に合わせた)。
frontend の paginator は `fetchNewer` で `sinceId` を投げる
(`packages/frontend/src/utility/paginator.ts`) ので、**off にすると「新しい投稿を
読み込む」操作が空を返す**。timeline JSON cache は cursor 無しのページだけが
対象なので、この経路には効かない。

実際に効くのは **`realtimeMode` を off にした利用者**。
`MkStreamingNotesTimeline.vue` は非 realtime のとき `useInterval` と
`notePosted` イベントから `fetchNewer` を呼ぶので、これが毎回空になり
**新着 (自分の投稿を含む) が一切追加されなくなる**。既定は
`realtimeMode: true` (`packages/frontend/src/store.ts`) で、そちらは streaming が
前置きするため影響は小さい。

負荷を落とすためのつまみであって、無害な最適化ではない。**DB の負荷が実際に
問題になっているときだけ切ること。**

### 「DB を触らなくなる」わけではない

止まるのは fallback クエリだけ。経路によって残るものが違う。

| 経路 | off でも走る DB アクセス |
|---|---|
| 継ぎ足し (Redis に持ち分があり `limit` 未満) | Redis の ID を note に引き直す hydrate (`FindManyByIDsWithUser`) と、解決できなかった ID の primary 確認 (`ExistingNoteIDsOnPrimary`、§5 の宙吊り ID 除去)。窓の奥を読み進めるので、どちらも読み進めた回数だけ走る |
| 全ページ (Redis 空 / `sinceId` 付き) | timeline service は DB を引かない |

ただし**どちらの経路でも handler 側の loader は毎リクエスト走る**。
`internal/api/notes/timeline_handler.go` は service を呼ぶ前に mute / block /
following / channel を読み、いずれもキャッシュを挟んでいない。数え方は
「handler が発行する repository 呼び出しの本数」:

- home / hybrid: **7 本** (フォロー中チャンネルがあれば `loadFollowedChannelIDs`
  が内部で `loadMutedChannelIDs` を呼び直すので 8 本)
- local / global: **5 本**
- `applyMuteBlock` 相当の filter (`newMuteBlockFilter`) が
  `notesfilter.LoadMuteBlockSets` で muting / blocking / channelMuting /
  user_profile を**もう一度** 4 本読み、`blockedHosts` のために meta を 1 回読む。
  Redis のページに通すとき (service の中) か service から戻ったあとの、最初の
  1 回だけ読んで使い回す (#3448)
- 同じ filter のリノート先の入れ子検査 (`FindManyByIDsWithUser`) が、新しく通す
  束ごとに 1 本。Redis の 1 ページ目、窓の奥を読み進めた各回、DB fallback の行が
  それぞれ 1 束で、通過済みの note は数えない。リノートを含まない束や、mute /
  block が空の viewer では走らない

いずれもログイン時の数。匿名 viewer では各 loader が nil を返して 0 本になる。
cursor 付きのページは JSON cache も効かないので、off にしても「timeline が DB を
触らなくなる」とは言えない。

### 件数の埋め方が upstream と違う (off で顕在化する)

upstream は Redis list を `lrange 0 -1` で**丸ごと**取り (`FanoutTimelineService.getMulti`)、
`limit` 件が埋まるまで**窓の奥へ読み進めながら** hydrate を繰り返す
(`FanoutTimelineEndpointService.getMiNotes` の while ループ)。mute や解決不能で
落ちたぶんは、その先の ID を追加で読んで埋める。**この再読み込みは
`useDbFallback` では止まらない。**

Elythia も Redis list を 1 リクエストで 1 回だけ丸ごと読み、1 ページ目
(`limit` 件) に hydrate と filter を通す (`readFanout`、4 経路で共通)。
filter には handler 側にしか無い判定 (meta.blockedHosts、凍結、リノート先まで辿る
mute / block) も含める (`TimelineFilter.PageFilter`、#3448)。足りないときの埋め方は
DB fallback の設定で分かれる。

- **off**: upstream と同じく、窓の奥の ID を読み進めて埋める (`scanOlder`)。
  1 回に hydrate する量は「足りない件数の 3 倍」で、upstream の上限
  (`Math.min(1.1 / lastSuccessfulRate, 3)`) に合わせている。**ただし `limit`
  件を下回らない** — upstream の式のままだと残り 1 件で 3 件ずつしか読まず、
  mute で落ちる ID が続くと、hydrate などの問い合わせの回数が窓の長さに比例して
  増える。1 ページ目も upstream は `ceil(limit * 1.1)` 件を読むが、Elythia は
  `limit` 件ちょうど読む。
- **on**: 窓の奥は読まず、1 ページ目で解決できた最古の note を境界にして DB から
  継ぎ足す。1 件も解決できなければ、呼び出し側の cursor から DB を引き直す。
  **upstream は先に窓の奥を読み進め、読み切ってから最後に読んだ ID を境界にして
  DB へ倒す。** Elythia が読み進めないのは、Redis に無い新しい note が DB にあると
  読み進めた分だけ飛ばしてしまうため (#2715。`fallbackRange` の doc)。

`allowPartial: true` (同梱 frontend は常に送る) は、1 件でも取れたらそのページを
返す。**0 件のときは返さず、上のどちらかで埋める** (#3448)。空のページを返すと
frontend の paginator は終端と判断し、古い note が残っていてもそれ以上読まない。
upstream も `ps.allowPartial ? redisTimeline.length !== 0 : ...` を満たすまで読み進め、
0 件なら DB へ倒す。

### 複数の list を混ぜる timeline (#3449)

social (`hybrid-timeline`) はホーム / ローカル / 返信付きローカル (または自分宛ての
返信) の 3 本、ログイン中の `local-timeline` はローカル + 返信付きローカル (または
自分宛ての返信) の 2 本の list を混ぜて返す。list ごとに上限の件数と流量が違うので、
遡れる深さが違う。以前は浅い list が尽きた先でも深い list の ID だけで 1 ページが
埋まって DB へ倒れず、浅い list にあったはずの期間 (例: フォロー中の人の、
ローカルに流れない投稿) が抜けていた。本家にも同じ問題があり、修正
(misskey-dev/misskey#13495) は未マージ。

Elythia は **DB fallback が on のとき**、各 list の末尾 (最も前に push されて残って
いる ID) のうち一番新しいもの (`coverageCutoff`) より古い ID を Redis から返さず、
そこから先を DB に任せる。1 ページ目がそこで足りなくなれば、Redis から返した最古の
note を境界にして DB で継ぎ足し、cursor がそれより古ければ全ページを DB から返す。
**抜けなくなるのは DB fallback が on のときだけ。** off のときは範囲で切らず、
抜ける期間は従来どおり抜ける (下記)。

**DB fallback のクエリも、list の中身を返すように広げた。** 返信を絞る条件
(「返信ではない or 自己スレッド」) のままだと、list にあった返信が cutoff を
越えた所から消える (以前は Redis から出ていたものが出なくなる)。

| timeline | list にあって、以前の DB クエリが返さなかったもの | 今の DB クエリ |
|---|---|---|
| `local-timeline` (ログイン中、`withReplies: false`) | 自分宛ての返信 (`localTimelineWithReplyTo:<viewer>`) | `replyUserId = viewer` も返す |
| `hybrid-timeline` の local 側 (`withReplies: false`) | 同上 | 同上 |
| `hybrid-timeline` の home 側 (`withReplies: false`) | フォロー中の人からの自分宛ての返信、自分の他人宛ての返信、自分への mention を含む返信、`withReplies` 付きでフォローしている人の返信 (ホームの list の振り分け、`fanoutToFollowersAndStream`)、フォロー中のチャンネルの返信 (チャンネルの投稿は返信も含めてフォロワー全員のホームの list に積まれる、`fanoutToChannelFollowers`) | 上記も返す。ただしフォロー一覧が読めなかったときは返さない (下記) |

可視性・mute・block・凍結・チャンネルの条件はそのまま AND で効く。チャンネルの
返信は、home 側の基本条件がチャンネルの投稿をフォロー中 (mute 済みを除く) の
チャンネルに絞っているので、それ以外のチャンネルのものは出ない。HTL
(`notes/timeline`) は list が 1 本で cutoff が掛からないので変えていない
(upstream の HTL の DB クエリと同じく返信を絞ったまま)。

**home 側の例外は、フォロー一覧が読めたときだけ付ける (fail closed)。** fanout は、
フォローしていない人の followers 限定の投稿への返信を配らない。SQL 側の同じ判定
(`HideFollowersOnlyReplyFromNonFollowee`) はフォロー一覧 (`loadFollowingIDs`) が
読めたときしか掛からないので、読めなかったときに例外だけを広げると、その返信
(`withReplies` 付きでフォローしている人のものなど) が DB から出てしまう。読め
なかったときは、自分宛ての返信 (`KeepRepliesToViewer`) 以外の home 側の例外
(自分の返信、mention を含む返信、`withReplies` の返信、チャンネルの返信) を付けず、
それらの返信は DB からは出ない。

**FTT を切ったとき (DB 直行) の結果も変わる。** DB 直行の経路も同じクエリを使う
ので、ログイン中の `local-timeline` / `hybrid-timeline` は、upstream の
`getFromDb` が返さない返信 (自分宛ての返信と、hybrid の home 側の上の表の返信) も
返す。これは upstream で FTT が on のときに返る結果と同じで、upstream も FTT を
切ったときに返らないことを不具合として扱っている (`test/e2e/timelines.ts` の
Local TL / Social TL にある `FIXME: misskey-dev/misskey#12065` のテストは、
FTT を切ると飛ばされる)。返る note が増えるだけで、レスポンスの形は変わらない。

**`withFiles: true` の local / hybrid も、自分宛ての返信を含む。** Redis の経路は
もともと `withFiles` でも同じ list (`localTimeline` + `localTimelineWithReplyTo:<viewer>`
など) を混ぜてから添付の有無で絞っており、自分宛ての返信の list も混ざっていた。
upstream は `withFiles` のとき `localTimelineWithFiles` だけを読む。今回 DB の経路も
自分宛ての返信を返すようになったので、Redis の経路と揃った (upstream との差は
以前からのもの)。

#13495 も「cutoff + DB クエリの拡張」の組み合わせだが、拡張は hybrid に
`replyUserId = me` を足すだけで、`withReplies: true` のときにフォロー中の人の返信を
絞る条件を足している。Elythia との違いは次のとおり。

| 状況 | #13495 | Elythia |
|---|---|---|
| list はあるが、窓 (`untilId` より古い範囲) には何も無い | DB へ倒す | 同じ (cutoff が `untilId` 以降になり、Redis から何も返さない) |
| list が無い (key が存在しない) | DB へ倒す。DB の結果が `limit` 未満の 1 ページ目でだけ、ダミー ID を積んで「空と確認済み」を印す | **cutoff に数えない** |
| list の深さ | 窓の中の最古 (= 最小値) | **位置としての末尾** (下記) |
| cutoff で切れて 1 ページに満たない | `allowPartial: true` なら、取れた分だけを返す | **`allowPartial` でも DB で埋める** |
| DB クエリの拡張 | hybrid に `replyUserId = me` | 上の表 (local と、hybrid の home 側の振り分けも) |
| DB fallback が off | 切らない | 同じ |

**list が無いのを数えないのは、DB の負荷のため。** 自分宛ての返信の list
(`localTimelineWithReplyTo:<viewer>`) は、返信を受けたことの無い利用者では常に
無い。数えると、その利用者のログイン中の social / local は 1 ページ目から全て DB へ
行く。#13495 のダミー ID は DB の結果が `limit` 未満のとき (= その timeline に
該当する note がほとんど無い小さなインスタンス) にしか積まれないので、実際には
ほとんど効かない。Elythia でダミー ID を積まないのは、Redis の key を TS 版と
共有していること (drop-in) と、解決できない ID を読み取りのたびに除去している
こと (§5 の宙吊り ID 除去) から。代わりに、key だけが失われたとき (Redis の
eviction など) は、他の list が持っている期間にあったその list の note が抜ける。
Redis 全体を消したときは全ての list が同じ時点から積み直されるので普通は抜けないが、
空の key には古い ID も無条件に積まれる (`Push`) ので、最初に積まれたのが遅れて
届いた古い note だと、その list は実際より深く見え、間が抜けうる。

**list の深さを最小値で測らないのは、遅れて届いた ID のため。** `Push` は 3 分の
猶予内の ID を時刻に関係なく先頭へ積むので、遅れて届いた古い ID が list の最小値に
なりうる。最小値で測ると、それより新しく、先に push されて押し出された note の
期間まで「持っている」ことになる。list は push の順に並ぶので、末尾 (最も前に
push されて残っている ID) で測ると、欠けうるのは末尾の note 自身の配送の遅れ
(最大で猶予の 3 分) の間に作られたものだけになる。猶予の分まで新しい側へずらすと
安全側に倒せるが、LTL の深さが 3 分を切るインスタンス (上限は 200 件固定) では
1 ページ目から常に DB へ倒れるので、そこまではしない。

**DB fallback が off のときに範囲で切らないのは、切っても取り戻す先が無いため。**
切ると、深い list にある note まで見えなくなり、timeline が一番浅い list の深さで
終わる。上のとおり off は group / open で立てたインスタンスの既定なので、切ると
影響が広い。

**増える DB の読み取り**は、一番浅い list と一番深い list の間の範囲を遡るページに
限られる。そこは以前 Redis から返していた (抜けを含んだまま) ページで、1 ページ
あたり social は 2 本 (`ListHomeTimeline` + `ListLocalTimeline`)、local は 1 本の
クエリになる。自分宛ての返信の list は上限が 20 件と小さいので、返信を多く受ける
利用者ではこれが一番浅い list になりやすい。返信を受け始めたばかりの利用者も、
その list が若いので一番浅くなる。1 ページ目が増えるのは、一番浅い list の末尾が
1 ページ目の範囲に入るときだけ。list の深さが揃っている範囲や、key の無い list が
あるだけの場合は、従来どおり DB を引かない。広げた DB クエリは、返信を絞る条件に
OR を足しただけで、行の取り方 (`id` の index を降順に読む) は変わらない
(`withReplies` 付きのフォローの副問い合わせは 1 回だけ評価される)。

**cutoff で切れたページを DB で埋めるのは、短いページを返さないため。**
#13495 と同じく取れた分だけを返すと、若い list が 1 本あるだけで (新しい利用者の
ホームの list など) social の 1 ページ目が数件になる。次のページは cutoff より
古いので必ず DB へ行くので、ここで埋めても DB の読み取りの回数は変わらない。
cutoff で切れていないページは、従来どおり `allowPartial` で部分ページを返す。

`sinceId` を含むページングは、もともと全て DB が処理するので変わらない。

### 対象になる timeline

| timeline | upstream | Elythia |
|---|---|---|
| home (`notes/timeline`) | 対象 | 対象 |
| `local-timeline` | 対象 | 対象 |
| `hybrid-timeline` | 対象 | 対象 |
| `global-timeline` | 対象外 (fanout を使わず常に DB) | **対象外** (下記) |
| `user-list-timeline` | 対象 | 対象外 (常に DB) |
| `channels/timeline` / `users/notes` / AP outbox | 対象外 (`useDbFallback: true` 固定) | 対象外 (Service を通らない) |
| `antennas/notes` / `roles/notes` | 対象外 (`FanoutTimelineEndpointService` を通らない) | 対象外 |

`user-list-timeline` だけが逆向きの差。Elythia は list メンバーの visibility を
SQL に push-down している (#1452) ため、Redis の ID から組み立てる経路を持たない。

**`global-timeline` は Elythia では fanout 経路だが、意図的に gate していない**
(#2762)。Elythia が GTL を fanout に載せているのは性能上の拡張であって、upstream
由来のつまみの意味論を変える理由にはならない。gate すると、上記のとおり group /
open で立てた**既定 off のインスタンス**で、誰も設定を触っていないのに GTL が
Redis list の窓 (global は `MaxTimelineLength` 固定の 200 件で、meta では変えられ
ない) を超えて遡れなくなる — `untilId` で窓の外を要求した
時点で Redis が空になり、fallback も止まるため。upstream の同設定のインスタンスは
GTL が無傷なので、この食い違いは Elythia 側の退行として出る。
