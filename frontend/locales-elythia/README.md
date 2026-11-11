# locales-elythia

Elythiaで足した語と、本家の語を書き換えた文言を、言語ごとに`<言語>.yml`で置くディレクトリです(#4071)。`frontend/locales/`は本家のファイルをそのまま置く場所なので、Elythiaの語はここに書きます。

- `packages/i18n`の`build()`は、各言語の本家の語(`frontend/locales/<言語>.yml`)の上に、ここにある同じ言語のファイルを重ねます。節の中は語ごとに重なります。言語をまたぐフォールバック(ja-JP → en-US → その言語、など)は、重ねた後に今までどおり行われます
- 本家の語を消して上位の言語の語に落としたいときは、その語に空文字(`''`)を書きます
- 型(`packages/i18n/src/autogen/locale.ts`)は、本家のja-JPにここのja-JPを重ねたものから作ります
- 本家と同じキーを書くと、本家の文言を上書きします。本家の語を書き換える・消すときは、`overrides.tsv`にキーと理由を書きます。一覧に無い書き換え、本家と同じ文の行、何も消さない空文字、文字列でない値(空の節、配列、`~`)は、`scripts/check-locale-overrides.mjs`(`make frontend-check`とCIの`frontend`)が落とします
- `frontend/locales/`は本家の版とバイト単位で同じに保ちます。`make upstream-check`(CIは`apicompat`)が見ます。`frontend/locales/README.md`(本家のファイル)は`ja-JP.yml`だけは直してよいと書いていますが、Elythiaでは`ja-JP.yml`も直しません
- ビルドが読むのは`packages/i18n/src/const.ts`の`languages`にある言語だけです。それ以外の言語のファイルを置くと、検査が落とします
- ja-JPに足した語は、en-USにも英訳を足します。無いと英語のUIでも日本語のまま出るので、`scripts/check-locale-coverage.mjs`が落とします
