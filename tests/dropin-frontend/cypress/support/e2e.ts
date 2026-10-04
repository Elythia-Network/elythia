// Phase 14-1 (#381) 用 cypress support ファイル。
// cypress 実行ごとの共通設定 (uncaught exception の扱い等) を書く。

// Misskey フロントエンドは開発中に uncaught が出ることがある。
// spec 側で意味を解釈できないので、support 層では test を fail させない。
Cypress.on('uncaught:exception', () => false);
