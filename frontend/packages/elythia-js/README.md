# elythia-js

Elythia 独自の API の型を、本家の [misskey-js](../misskey-js) の型に重ねて出すパッケージ (#3417)。frontend の中だけで使い、npm には公開しない (公開できる形にはしてある)。

- 本家の misskey-js には手を入れない。本家への追従 (`make upstream-sync`) とぶつからないように、独自の分はここに書く
- `Endpoints` は `misskey-js` の `Endpoints` に `ElythiaEndpoints` を重ねたもの。frontend の `misskeyApi` / `misskeyApiGet` はこれを使う
- 型は手で書く。Go に登録された Elythia 独自の POST のエンドポイントと `ElythiaEndpoints` のキーは、`internal/entitycompat` のゲート (`TestElythiaJS_EndpointsMatchRouter`) が突き合わせる。まだ型の無いものは [`pending-endpoints.txt`](pending-endpoints.txt) に 1 行 1 件で並べ、型を足したらそこから外す
