# locales-elythia

Elythiaで足した語と、本家の語を書き換えた文言を、言語ごとに`<言語>.yml`で置くディレクトリです(#4071)。`frontend/locales/`は本家のファイルをそのまま置く場所なので、Elythiaの語はここに書きます。

- `packages/i18n`の`build()`は、各言語の本家の語(`frontend/locales/<言語>.yml`)の上に、ここにある同じ言語のファイルを重ねます。節の中は語ごとに重なります。言語をまたぐフォールバック(ja-JP → en-US → その言語、など)は、重ねた後に今までどおり行われます
- 本家の語を消して上位の言語の語に落としたいときは、その語に空文字(`''`)を書きます
- 型(`packages/i18n/src/autogen/locale.ts`)は、本家のja-JPにここのja-JPを重ねたものから作ります
