# elythia-js

Elythia 独自の API の型を、本家の [misskey-js](../misskey-js) の型に重ねて出すパッケージ (#3417)。frontend の中だけで使い、npm には公開しない (公開できる形にはしてある)。

- 本家の misskey-js には手を入れない。本家への追従 (`make upstream-sync`) とぶつからないように、独自の分はここに書く
- `Endpoints` は `misskey-js` の `Endpoints` に、次の 3 つを重ねたもの。frontend の `misskeyApi` / `misskeyApiGet` / `os.apiWithDialog` はこれを使う
  - `ElythiaEndpoints` (`src/endpoints.ts`): Elythia 独自のエンドポイント
  - `ElythiaRequestExtensions` (`src/extensions.ts`): 本家のエンドポイントに Elythia が足した引数 (`admin/emoji/copy` / `admin/update-meta`)。本家の req に交差で重ねるので、足せるのは省略できる項目だけ。キーは本家にあるエンドポイントに限る (型が弾く)
  - `ResponseOverrides` (`src/extensions.ts`): misskey-js の型が実際の応答と違うものの差し替え (`i/registry/get`)
- `misskeyApi` / `misskeyApiGet` / `os.apiWithDialog` は、次の 2 つを型で検査する (#3439)
  - **引数の型に無いキーを渡すと型エラーになる** (`ExcessKeys`)。変数や spread で渡した値も対象になる。引数の型が和集合なら、どれかの要素にあるキーは通す。`EmptyRequest` のエンドポイントはどのキーも通す
  - **応答の型は代入先から推論されない** (戻り値を `NoInfer` で包んである)。`x.value = await misskeyApi(...)` でエンドポイントを取り違えると型エラーになる。応答の型を変えたいときは `misskeyApi<T>(...)` と明示する (このときは引数の型の検査が効かなくなる)。エンドポイントを型引数のまま受け渡す包み関数は、明示しないとコンパイルできない。引数の検査を残したいなら `os.apiWithDialog` のように `P & ExcessKeys<E, P>` で受けて `misskeyApi<void, E, P>(...)` と渡す。引数の型を組み立てる汎用の呼び出し (`utility/paginator.ts`) は `misskeyApi<T>(...)` にする
- 型は手で書く。Go に登録された Elythia 独自の POST のエンドポイントと `ElythiaEndpoints` のキーは、`internal/entitycompat` のゲート (`TestElythiaJS_EndpointsMatchRouter`) が突き合わせる。まだ型の無いものは [`pending-endpoints.txt`](pending-endpoints.txt) に 1 行 1 件で並べ、型を足したらそこから外す
