-- #3369: ワードミュート (`user_profile."mutedWords"` / `"hardMutedWords"`) の要素のうち、
-- `/.../` の形でない文字列を、同じ語の組 (`["foo"]`) に書き換える。
--
-- 本家は文字列の要素を必ず正規表現として読み、`/.../` の形でなければ何にも当てない
-- (i/update も INVALID_REGEXP で拒否する)。#3369 より前の Elythia は i/update で
-- 受け付け、ハードミュートではサーバーが部分一致の語として当てていた。#3369 で判定と
-- i/update の検査を本家に揃えたので、そのままだと黙って効かなくなり、同じ一覧を
-- 送り直すと拒否される。1 語の組にすれば、hardMutedWords は部分一致の語として
-- 効き続ける (大文字と小文字は区別するようになる)。mutedWords はサーバーが判定せず、
-- frontend は `/.../` でない文字列を当てないので、以前から効いておらず、ここで
-- 効き始める (設定画面で保存し直したときと同じく語の組になる)。
--
-- - 正規表現として残す形は、#3369 より前にその列を正規表現として読んでいた側に合わせる。
--   hardMutedWords は Elythia のサーバー (Go の `^/(.+)/([gimsuy]*)$`) と frontend
--   (JavaScript の `^\/(.+)\/(.*)$`。`.` は改行類に当たらない) の両方が読んでいたので、
--   どちらかの形なら残す。mutedWords は frontend だけが読んでいたので JavaScript の形。
--   本家の i/update は JavaScript の形しか受け付けないので、TS 版が書いた行は変わらない
-- - 空の文字列と、JavaScript の `trim()` が除く文字だけの文字列は消す。本家の frontend は
--   保存するときに各行を trim して空の行を捨てるので作らない。語の組にしても何にも
--   当たらない。文字の集合は明示する (`\s` は DB の locale で変わるため)
-- - 配列の要素 (語の組) と、正規表現の文字列はそのまま残す。順序も保つ
-- - 一覧が配列でない行には触らない
UPDATE "user_profile" SET "hardMutedWords" = (
    SELECT COALESCE(jsonb_agg(
        CASE WHEN jsonb_typeof(t.e) = 'string' AND NOT (((t.e #>> '{}') ~ '^/[^\n]+/[gimsuy]*$' OR (t.e #>> '{}') ~ '^/[^\n\r\u2028\u2029]+/[^\n\r\u2028\u2029]*$'))
            THEN jsonb_build_array(t.e) ELSE t.e END
        ORDER BY t.ord), '[]'::jsonb)
    FROM jsonb_array_elements("hardMutedWords") WITH ORDINALITY AS t(e, ord)
    WHERE NOT (jsonb_typeof(t.e) = 'string' AND (t.e #>> '{}') ~ '^[\u0009-\u000D\u0020\u00A0\u1680\u2000-\u200A\u2028\u2029\u202F\u205F\u3000\uFEFF]*$')
)
WHERE jsonb_typeof("hardMutedWords") = 'array'
  AND EXISTS (
    SELECT 1 FROM jsonb_array_elements("hardMutedWords") AS x(e)
    WHERE jsonb_typeof(x.e) = 'string' AND NOT (((x.e #>> '{}') ~ '^/[^\n]+/[gimsuy]*$' OR (x.e #>> '{}') ~ '^/[^\n\r\u2028\u2029]+/[^\n\r\u2028\u2029]*$'))
  );

UPDATE "user_profile" SET "mutedWords" = (
    SELECT COALESCE(jsonb_agg(
        CASE WHEN jsonb_typeof(t.e) = 'string' AND NOT ((t.e #>> '{}') ~ '^/[^\n\r\u2028\u2029]+/[^\n\r\u2028\u2029]*$')
            THEN jsonb_build_array(t.e) ELSE t.e END
        ORDER BY t.ord), '[]'::jsonb)
    FROM jsonb_array_elements("mutedWords") WITH ORDINALITY AS t(e, ord)
    WHERE NOT (jsonb_typeof(t.e) = 'string' AND (t.e #>> '{}') ~ '^[\u0009-\u000D\u0020\u00A0\u1680\u2000-\u200A\u2028\u2029\u202F\u205F\u3000\uFEFF]*$')
)
WHERE jsonb_typeof("mutedWords") = 'array'
  AND EXISTS (
    SELECT 1 FROM jsonb_array_elements("mutedWords") AS x(e)
    WHERE jsonb_typeof(x.e) = 'string' AND NOT ((x.e #>> '{}') ~ '^/[^\n\r\u2028\u2029]+/[^\n\r\u2028\u2029]*$')
  );
