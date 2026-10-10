-- #3479 の戻し。page_visibility_enum を本家の ('public', 'followers', 'specified') に
-- 戻し、'private' の行は 'specified' にする (visibleUserIds は空のままなので、作者
-- 以外には見えない)。
-- data loss: up の前に 'followers' だった行と 'specified' だった行の区別は戻らない
-- (どちらも 'specified' になる)。
DO $$
DECLARE
    had_default boolean;
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_enum
        WHERE enumtypid = 'page_visibility_enum'::regtype AND enumlabel = 'private'
    ) THEN
        -- 既定値は元からあったときだけ付け直す。Elythia の 000009 は 'public' を
        -- 既定にするが、本家 (TypeORM) が作った列には既定値が無い。
        SELECT EXISTS (
            SELECT 1 FROM pg_attrdef d
            JOIN pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum
            WHERE d.adrelid = '"page"'::regclass AND a.attname = 'visibility'
        ) INTO had_default;
        ALTER TYPE page_visibility_enum RENAME TO page_visibility_enum_old;
        CREATE TYPE page_visibility_enum AS ENUM ('public', 'followers', 'specified');
        ALTER TABLE "page" ALTER COLUMN "visibility" DROP DEFAULT;
        ALTER TABLE "page" ALTER COLUMN "visibility" TYPE page_visibility_enum
            USING (CASE WHEN "visibility"::text = 'public' THEN 'public' ELSE 'specified' END)::page_visibility_enum;
        IF had_default THEN
            ALTER TABLE "page" ALTER COLUMN "visibility" SET DEFAULT 'public';
        END IF;
        DROP TYPE page_visibility_enum_old;
    END IF;
END $$;
