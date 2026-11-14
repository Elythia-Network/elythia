-- #4164: meta の 3 列の既定値を true に揃え、本家 2026.11.0 で増えた TypeORM migration を
-- migrations テーブルへ seed する。
--
-- 本家の旧 migration (migrateSomeConfigFileSettingsToMeta1746949539915) は、設定ファイルの
-- 値を列の DEFAULT に焼き込む形で入った版 (2025.5.1〜2025.11.x) があり、その版で流した
-- TS 版の DB は、proxyRemoteFiles を設定していなければ DEFAULT false のまま残る。Elythia は
-- meta の行を作るときに DB の既定値に頼る (internal/repository/meta.go は ID と URL の列だけを
-- 書く) ので、TS 版から移した DB で行を作り直すと false で作られる。本家 2026.11.0 の
-- FixMetaMigratedConfigColumnDefaults1794385801836 と同じく、既存の行の値は変えずに、列の
-- DEFAULT だけを true にする。Elythia が作った DB は 000001 / 000029 で元から true なので、
-- 何も変わらない。
ALTER TABLE "meta" ALTER COLUMN "proxyRemoteFiles" SET DEFAULT true;
ALTER TABLE "meta" ALTER COLUMN "signToActivityPubGet" SET DEFAULT true;
ALTER TABLE "meta" ALTER COLUMN "allowExternalApRedirect" SET DEFAULT true;

-- seed の形は 000135 と同じ (000067 / 000135 は書き換えない)。上の DDL が本家の migration と
-- 同じ変更なので、seed しても schema はずれない。name 一致で存在確認するので、本家が既に
-- 書いた行は二重挿入しない。
INSERT INTO "migrations" ("timestamp", "name")
SELECT v.ts, v.n FROM (VALUES
    (1794385801836::bigint, 'FixMetaMigratedConfigColumnDefaults1794385801836')
) AS v(ts, n)
WHERE NOT EXISTS (SELECT 1 FROM "migrations" m WHERE m."name" = v.n);
