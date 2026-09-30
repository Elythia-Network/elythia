---
paths:
  - ".github/**"
  - "Makefile"
---

workflowやMakefileを書く・直す前に、`docs/ci.md`をReadで読むこと。
各workflowの設計理由、required checkにしない理由、落ちたときの対処がまとまっている。

<!-- ここで `@docs/...` の取り込みを使わない。ルールファイルの中の `@` は paths: に関係なく
     起動時に展開され、5 本で約 2,200 行が毎回読み込まれた (#3248 で実測)。 -->
