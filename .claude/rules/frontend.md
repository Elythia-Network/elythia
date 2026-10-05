---
paths:
  - "frontend/**"
---

`frontend/`を書く・直す前に、`docs/contributing.md`の「fork frontend (`frontend/`) を触るとき」をReadで読むこと。
手元での確認の手順、本番を壊すビルドの注意、frontendのコーディング規約がまとまっている。

<!-- ここで `@docs/...` の取り込みを使わない。ルールファイルの中の `@` は paths: に関係なく
     起動時に展開され、5 本で約 2,200 行が毎回読み込まれた (#3248 で実測)。 -->
