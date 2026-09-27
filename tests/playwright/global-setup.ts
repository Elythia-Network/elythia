/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #744 Phase 1 PR-2: Playwright globalSetup.
//
// 全 spec が start する前に 1 度だけ実行される。本 globalSetup は instance
// を test runnable な状態に整える 3 つの仕事を行う:
//
//   1. Redis FLUSHDB — mk-go の signup endpoint は IP base 1h 5 回の rate
//      limit が hardcoded (internal/server/middleware/ratelimit_defs.go)
//      で、test 累積で 429 になるため毎 run で counter をゼロから始める
//   2. root (alice) を idempotent に確保 — admin/accounts/create は初回のみ
//      通り、再実行時は signin-flow で既存 alice の token を再取得する
//   3. admin/update-meta で disableRegistration=false に切替 — model/meta.go
//      の default は true で、initial state の signup endpoint は invitation
//      コード必須。Phase 1 spec は signup-flow で fresh user を作るので
//      registration を open にしておく
//   4. root の per-user quota を purge — antenna / webhook / clip / user list
//      は role policy に上限があり (antennaLimit 5 / webhookLimit 3 /
//      clipLimit 10 / avatarDecorationLimit 1)、spec が後始末しない分が
//      DB に残る。同じ stack へ 2 回目を流すと枠を使い切って**無関係な
//      spec** が setup の create で落ちる (#2264)。run の先頭で空にする
//
// root の credentials は `.auth/root.json` に書き出して spec から読める
// ようにする。複数 spec で root token を再利用するための fixture。

import { mkdirSync, writeFileSync } from 'node:fs';
import { type APIRequestContext, request as createRequest } from '@playwright/test';
import { DEFAULT_TEST_PASSWORD } from './fixtures/auth';
import { isTsBackend } from './fixtures/backend';
import { resetRateLimit } from './fixtures/rate_limit';

const baseURL = process.env.MK_BASE_URL ?? 'http://mkgo:3000';

// root credentials は globalSetup と spec 全体で共有する。username / password
// 値は spec 内で hardcode されている箇所 (= signin form 入力等) があるが、
// 本 file では「create / signin の payload」と「.auth/root.json の username」
// の整合性を維持する責務として 1 箇所に集約する。password は fixtures/auth.ts
// の DEFAULT_TEST_PASSWORD を再利用 (= signupUser / uiSigninAsRoot と source
// of truth を一本化、#827 review)。
const ROOT_USERNAME = 'alice';
const ROOT_PASSWORD = DEFAULT_TEST_PASSWORD;

interface RootCreds {
  id: string;
  token: string;
  username: string;
}

// admin/accounts/create を試みる。初回 (alice 未存在) は 200 で credentials
// を返す。既存の場合は null を返して caller に signin fallback を委譲する。
async function tryCreateRoot(ctx: APIRequestContext): Promise<RootCreds | null> {
  const resp = await ctx.post(`${baseURL}/api/admin/accounts/create`, {
    data: { username: ROOT_USERNAME, password: ROOT_PASSWORD },
    failOnStatusCode: false,
  });
  if (resp.status() === 200) {
    const body = await resp.json();
    if (typeof body.id !== 'string' || typeof body.token !== 'string') {
      throw new Error(
        `globalSetup admin/accounts/create returned malformed body: ${JSON.stringify(body)}`,
      );
    }
    return { id: body.id, token: body.token, username: ROOT_USERNAME };
  }
  // 既存 alice (= ACCESS_DENIED) は idempotent re-run の正常系。status は
  // backend で割れる: upstream は kind:'client' の既定にあたる 400 を返し
  // (ApiCallService.#sendApiError)、mk-go は 403 を返す。どちらの backend で
  // 走らせても re-run できるよう、status ではなく error code で判定する。
  // それ以外 (5xx 等) は本当に失敗しているので throw する (#2276)。
  const body = await resp.text();
  if ((resp.status() === 403 || resp.status() === 400) && body.includes('ACCESS_DENIED')) {
    return null;
  }
  throw new Error(
    `globalSetup admin/accounts/create unexpected status: ${resp.status()} ${body}`,
  );
}

// 既存 alice として signin-flow で token を再取得する。fresh DB から alice
// が作られた直後の re-run / postgres volume が残っているケースで使う。
async function signinAsExistingRoot(ctx: APIRequestContext): Promise<RootCreds> {
  // 2FA なし user は signin-flow に { username, password } を送ると
  // { finished: true, id, i: token } が返る (internal/api/signin/handler.go:118)。
  const resp = await ctx.post(`${baseURL}/api/signin-flow`, {
    data: { username: ROOT_USERNAME, password: ROOT_PASSWORD },
    failOnStatusCode: false,
  });
  if (resp.status() !== 200) {
    throw new Error(
      `globalSetup signin-flow failed for existing alice: ${resp.status()} ${await resp.text()}`,
    );
  }
  const body = await resp.json();
  if (!body.finished || typeof body.id !== 'string' || typeof body.i !== 'string') {
    throw new Error(
      `globalSetup signin-flow returned non-final / malformed state: ${JSON.stringify(body)}`,
    );
  }
  return { id: body.id, token: body.i, username: ROOT_USERNAME };
}

// purgeRootQuotas deletes the per-user-limited resources root accumulated in
// previous runs. これをやらないと 2 回目以降の run で antenna / webhook 系の
// spec が「作れない」ことによる謎の失敗を起こす (#2264)。
//
// nightly CI は毎回 stack を作り直すので影響しないが、ローカルで
// `make playwright-up` した stack を使い回すと必ず踏む。診断のノイズになる
// (#2254 の調査中、実際に spec 側の bug と誤認しかけた)。
async function purgeRootQuotas(ctx: APIRequestContext, token: string): Promise<void> {
  // list endpoint / delete endpoint / delete の id param 名。
  const resources: Array<{ name: string; list: string; del: string; idKey: string }> = [
    { name: 'antenna', list: 'antennas/list', del: 'antennas/delete', idKey: 'antennaId' },
    { name: 'webhook', list: 'i/webhooks/list', del: 'i/webhooks/delete', idKey: 'webhookId' },
    { name: 'clip', list: 'clips/list', del: 'clips/delete', idKey: 'clipId' },
    { name: 'userList', list: 'users/lists/list', del: 'users/lists/delete', idKey: 'listId' },
  ];

  for (const r of resources) {
    const listResp = await ctx.post(`${baseURL}/api/${r.list}`, {
      data: { i: token, limit: 100 },
      failOnStatusCode: false,
    });
    if (listResp.status() !== 200) {
      throw new Error(
        `globalSetup ${r.list} failed: ${listResp.status()} ${await listResp.text()}`,
      );
    }
    const rows = (await listResp.json()) as Array<{ id?: string }>;
    if (!Array.isArray(rows)) {
      throw new Error(`globalSetup ${r.list} returned non-array: ${JSON.stringify(rows)}`);
    }
    for (const row of rows) {
      if (typeof row.id !== 'string') continue;
      const delResp = await ctx.post(`${baseURL}/api/${r.del}`, {
        data: { i: token, [r.idKey]: row.id },
        failOnStatusCode: false,
      });
      if (delResp.status() !== 200 && delResp.status() !== 204) {
        throw new Error(
          `globalSetup ${r.del} failed for ${row.id}: ${delResp.status()} ${await delResp.text()}`,
        );
      }
    }
    if (rows.length > 0) {
      // eslint-disable-next-line no-console
      console.log(`[globalSetup] purged ${rows.length} leftover ${r.name}(s)`);
    }
  }

  // avatarDecorationLimit は既定 1 なので、装着済みが残っていると
  // avatar_decoration_attach spec が「Remaining: 0」で attach できない。
  const decoResp = await ctx.post(`${baseURL}/api/i/update`, {
    data: { i: token, avatarDecorations: [] },
    failOnStatusCode: false,
  });
  if (decoResp.status() !== 200) {
    throw new Error(
      `globalSetup i/update avatarDecorations reset failed: ${decoResp.status()} ${await decoResp.text()}`,
    );
  }
}

export default async function globalSetup(): Promise<void> {
  // 1. Redis flush (#744 PR-2). 失敗は throw して fail-fast にする —
  // silent warn だと後段の signup spec が 429 で fail して原因が見えない。
  resetRateLimit();
  // eslint-disable-next-line no-console
  console.log('[globalSetup] redis FLUSHDB done');

  // self-signed cert を accept する。playwright.config.ts の use 設定は
  // spec scope のみなので globalSetup には届かない (= globalSetup は
  // request context を独立に作るため明示指定が必要、#817 part2 で HTTPS 化)。
  const ctx = await createRequest.newContext({ ignoreHTTPSErrors: true });
  try {
    // 2. Root を idempotent に確保。create が通ればそれを使い、既に alice
    // が存在する場合 (= postgres volume が残った状態の re-run) は signin-flow
    // で token を再取得する。
    let root = await tryCreateRoot(ctx);
    if (root === null) {
      root = await signinAsExistingRoot(ctx);
      // eslint-disable-next-line no-console
      console.log('[globalSetup] reusing existing root (signin-flow path)');
    }

    // 3. disableRegistration=false に切り替え (再実行時も idempotent)
    //
    // mk-go では「受け付けない」(#3186) も外す。前の run が閉じたまま終わっていると、
    // サーバーは disableRegistration=false を閉じる側で上書きし、以降の signup が全滅
    // する。TS はこの field を知らないので送らない。
    const metaResp = await ctx.post(`${baseURL}/api/admin/update-meta`, {
      data: {
        i: root.token,
        disableRegistration: false,
        ...(isTsBackend ? {} : { registrationClosed: false, approvalRequiredForSignup: false }),
      },
      failOnStatusCode: false,
    });
    if (metaResp.status() !== 200 && metaResp.status() !== 204) {
      throw new Error(
        `globalSetup admin/update-meta failed: ${metaResp.status()} ${await metaResp.text()}`,
      );
    }

    // 4. accountSetupWizard を -1 に設定して MkUserSetupDialog (初回 wizard
    // modal) の自動表示を抑止する。pizzax store 'base' の `where: 'account'`
    // entry は i/registry に scope=['client', 'base'] で persist される。
    // wizard が出ると programmatic click でも modal overlay 上の MkButton
    // (= 別 z-index) を踏んで原 page の Save / Follow button click が
    // intercept される (#744 batch3 で発覚)。
    const wizardResp = await ctx.post(`${baseURL}/api/i/registry/set`, {
      data: {
        i: root.token,
        scope: ['client', 'base'],
        key: 'accountSetupWizard',
        value: -1,
      },
      failOnStatusCode: false,
    });
    if (wizardResp.status() !== 200 && wizardResp.status() !== 204) {
      throw new Error(
        `globalSetup i/registry/set accountSetupWizard failed: ${wizardResp.status()} ${await wizardResp.text()}`,
      );
    }

    // 5. 前回 run の残骸で per-user quota を使い切らないよう purge (#2264)
    await purgeRootQuotas(ctx, root.token);

    // root credentials を spec から読めるように file 出力
    mkdirSync('.auth', { recursive: true });
    writeFileSync('.auth/root.json', JSON.stringify(root));
    // eslint-disable-next-line no-console
    console.log('[globalSetup] root account ready, registration opened');
  } finally {
    await ctx.dispose();
  }
}
