---
paths:
  - "**/*_test.go"
  - "internal/testutil/**"
  - "migration/**"
---

テストやmigrationを書く・直す前に、`docs/testing.md`をReadで読むこと。
DBを使うテストの分離、schemaが壊れたときの復旧手順、過去に踏んだ罠がまとまっている。

<!-- ここで `@docs/...` の取り込みを使わない。ルールファイルの中の `@` は paths: に関係なく
     起動時に展開され、5 本で約 2,200 行が毎回読み込まれた (#3248 で実測)。 -->
