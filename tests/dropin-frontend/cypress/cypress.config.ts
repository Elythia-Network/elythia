import { defineConfig } from 'cypress';

// Phase 14-1 (#381) / Phase 14-2 (#387) drop-in frontend e2e 向け cypress 設定。
//
// - spec は `e2e/*.cy.ts` にフラットに置く (階層化は 14-2.5 以降で検討)
// - baseUrl は設定しない (spec 内で 3 instance の URL を個別に叩く)
// - self-signed cert は `NODE_TLS_REJECT_UNAUTHORIZED=0` を compose で
//   cypress-runner に渡して node 側 (= cy.request の backing runtime) に
//   許容させる。
// - signin rate limit 対策として plugin task (`tokenCache:get` / `tokenCache:set`)
//   で spec 間共有を実現する。plugin process は cypress run 全体で 1 つだけ
//   起動されるので、memory 上の変数が spec をまたいで persistent。

export default defineConfig({
  e2e: {
    specPattern: 'e2e/**/*.cy.ts',
    supportFile: 'support/e2e.ts',

    viewportWidth: 1280,
    viewportHeight: 720,

    // Misskey の起動・連合配信 poll のため緩めに。
    defaultCommandTimeout: 15_000,
    requestTimeout: 30_000,
    responseTimeout: 30_000,

    setupNodeEvents(on) {
      const cache: Record<string, unknown> = {};
      on('task', {
        // unknown な key は null を返す (cypress task は undefined を許さない)。
        'tokenCache:get': (key: string) => {
          return cache[key] ?? null;
        },
        'tokenCache:set': ({ key, value }: { key: string; value: unknown }) => {
          cache[key] = value;
          return null;
        },
      });
    },
  },
});
