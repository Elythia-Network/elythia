// Misskey API を cypress から叩くための薄い helper。
//
// Phase 14-1 (#381) では cy.request のみ使い、ブラウザ UI 操作は Phase 14-2
// 以降で追加する。self-signed cert は cypress の browser launch 時に
// `--ignore-certificate-errors` を渡すことで許容する設定 (cypress.config.ts)。

export interface InstanceInfo {
  url: string; // "https://a"
  domain: string; // "a"
}

export const INSTANCES = {
  a: { url: Cypress.env('A_URL') as string, domain: Cypress.env('A_DOMAIN') as string },
  b: { url: Cypress.env('B_URL') as string, domain: Cypress.env('B_DOMAIN') as string },
  c: { url: Cypress.env('C_URL') as string, domain: Cypress.env('C_DOMAIN') as string },
};

// 各インスタンスがまだ上がりきっていない可能性があるため、API 叩く前に
// `POST /api/ping` が 200 を返すのを待つ。
//
// cypress は `failOnStatusCode: false` と `retryOnStatusCodeFailure: true` を
// 同時指定できない制約があるので、自力で retry loop を組む。
export function waitForInstance(inst: InstanceInfo, retries = 60): Cypress.Chainable {
  const attempt = (left: number): Cypress.Chainable =>
    cy
      .request({
        method: 'POST',
        url: `${inst.url}/api/ping`,
        body: {},
        failOnStatusCode: false,
      })
      .then((resp) => {
        if (resp.status === 200) {
          return resp;
        }
        if (left <= 0) {
          throw new Error(`waitForInstance: ${inst.url} never became healthy`);
        }
        return cy.wait(3_000, { log: false }).then(() => attempt(left - 1));
      });
  return attempt(retries);
}

// 初回ユーザーを root として作成する。既存ならそのユーザーで signin する。
// Misskey TS の `admin/accounts/create` は root 作成時のみ開く。以降は 400/403
// が返るので signin-flow にフォールバックする。
//
// signin-flow のレスポンスは `{finished: true, i: <token>}` で `id` を含まない
// ため、/api/i で hydrate する (Phase 13 の `_ensure_user_id` ヘルパ相当)。
// Devin #385 #1。
export function createRootOrSignin(
  inst: InstanceInfo,
  username: string,
  password: string,
): Cypress.Chainable<{ id: string; token: string }> {
  return cy
    .request({
      method: 'POST',
      url: `${inst.url}/api/admin/accounts/create`,
      body: { username, password },
      failOnStatusCode: false,
    })
    .then((resp) => {
      if (resp.status === 200 && resp.body?.token) {
        return cy.wrap({ id: resp.body.id, token: resp.body.token });
      }
      return cy
        .request({
          method: 'POST',
          url: `${inst.url}/api/signin-flow`,
          body: { username, password },
        })
        .then((signin) => {
          const token = signin.body.i ?? signin.body.token;
          if (signin.body.id) {
            return cy.wrap({ id: signin.body.id as string, token });
          }
          // signin-flow が id を返さない場合は /api/i で自分の user info を
          // 引いて id を埋める。
          return cy
            .request({
              method: 'POST',
              url: `${inst.url}/api/i`,
              body: { i: token },
            })
            .then((me) => cy.wrap({ id: me.body.id as string, token }));
        });
    });
}

// 認証付き API 呼び出し。Misskey は body に `i: token` を載せる convention。
export function api<T = any>(
  inst: InstanceInfo,
  endpoint: string,
  body: Record<string, unknown> & { i?: string } = {},
): Cypress.Chainable<Cypress.Response<T>> {
  return cy.request({
    method: 'POST',
    url: `${inst.url}/api/${endpoint}`,
    body,
    failOnStatusCode: false,
  });
}

// retry wrapper。connect 成立まで 60s 待つ。
export function retryUntil<T>(
  fn: () => Cypress.Chainable<T>,
  predicate: (v: T) => boolean,
  opts: { retries?: number; interval?: number } = {},
): Cypress.Chainable<T> {
  const retries = opts.retries ?? 20;
  const interval = opts.interval ?? 3_000;
  const attempt = (left: number): Cypress.Chainable<T> =>
    fn().then((v) => {
      if (predicate(v)) {
        return v;
      }
      if (left <= 0) {
        throw new Error('retryUntil: predicate never matched');
      }
      return cy.wait(interval, { log: false }).then(() => attempt(left - 1));
    });
  return attempt(retries);
}
