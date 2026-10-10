-- #3479: Page の公開範囲を public / private の 2 値にする。
--
-- 本家の page_visibility_enum は ('public', 'followers', 'specified') だが、本家は
-- 作成時に必ず 'public' を書き、公開範囲を選ぶ API も画面も持たない。Elythia は
-- 派生版の一つ (この enum を ('public', 'private') に作り替えている) に揃えて、
-- 作者だけが見られる 'private' を足す。
--
-- 既存の 'followers' / 'specified' の行は 'private' にする (見える範囲が狭くなる
-- 向き)。enum が既に 2 値 (その派生版から移行した DB) なら何もしない。2 値の DB で
-- 変換を流しても値は同じだが、表の書き換えと排他ロックを避けるために分岐する。
--
-- この DB を TS へ戻すと、本家の型 ('public' / 'followers' / 'specified') を前提に
-- した migration や読み書きと合わなくなる。復路は保証しない (#3191)。
DO $$
DECLARE
    had_default boolean;
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_enum
        WHERE enumtypid = 'page_visibility_enum'::regtype AND enumlabel = 'followers'
    ) THEN
        -- 既定値は元からあったときだけ付け直す。Elythia の 000009 は 'public' を
        -- 既定にするが、本家 (TypeORM) が作った列には既定値が無い。
        SELECT EXISTS (
            SELECT 1 FROM pg_attrdef d
            JOIN pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum
            WHERE d.adrelid = '"page"'::regclass AND a.attname = 'visibility'
        ) INTO had_default;
        ALTER TYPE page_visibility_enum RENAME TO page_visibility_enum_old;
        CREATE TYPE page_visibility_enum AS ENUM ('public', 'private');
        ALTER TABLE "page" ALTER COLUMN "visibility" DROP DEFAULT;
        ALTER TABLE "page" ALTER COLUMN "visibility" TYPE page_visibility_enum
            USING (CASE WHEN "visibility"::text = 'public' THEN 'public' ELSE 'private' END)::page_visibility_enum;
        IF had_default THEN
            ALTER TABLE "page" ALTER COLUMN "visibility" SET DEFAULT 'public';
        END IF;
        DROP TYPE page_visibility_enum_old;
    END IF;
END $$;
