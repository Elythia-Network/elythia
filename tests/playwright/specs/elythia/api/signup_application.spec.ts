/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 承認制の登録 (#2568 / #2569) の独自の API 7 件を叩き、応答を /api.json の宣言で照合する
// (#4093)。画面の spec (signup_application_flow) で 2xx までは通っているが、応答の型は
// 見ていなかった。登録の受け付け方は instance meta なので、始める前の値を控えて終わったら
// 戻す (この spec は admin/update-meta を書くので meta の project で流れる)。

import { type APIRequestContext, expect, test } from '@playwright/test';
import { expectApiOk, rootFixture } from '../../../fixtures/admin_table';
import { callApi } from '../../../fixtures/api';
import { callChecked } from '../../../fixtures/api_schema';
import { DEFAULT_TEST_PASSWORD, randomUsername } from '../../../fixtures/auth';
import { isTsBackend } from '../../../fixtures/backend';
import { resetRateLimit } from '../../../fixtures/rate_limit';

const KEYS = ['registrationClosed', 'approvalRequiredForSignup', 'disableRegistration', 'signupApplicationForm', 'emailRequiredForSignup'] as const;

// 申請・状況・審査の一覧の日時は、毎回 Go の書式で返る。#4091 で直す
const DATES = { knownBug: '#4091' };

interface Applied { claimCode: string; application: { status: string } }
interface AdminApplication { id: string; answers: { value: string }[] }

/**
 * Finds the pending application whose answer is `answer` in the review list.
 * The applicant's view has no id, so the reviewer finds it by the answer.
 */
async function pendingId(request: APIRequestContext, token: string, answer: string): Promise<string> {
  const listed = await callChecked<{ applications: AdminApplication[] }>(request, 'admin/signup-application/list', { i: token, filter: 'pending', limit: 100 }, DATES);
  const found = listed.applications.find((a) => a.answers.some((x) => x.value === answer));
  expect(found, `the review list has the application answered ${answer}`).toBeDefined();
  return found!.id;
}

/** Gets a form token and applies with `answer` after the minimum wait. */
async function apply(request: APIRequestContext, answer: string): Promise<Applied> {
  const form = await callChecked<{ token: string; minWaitSeconds: number }>(request, 'signup-application/form-token', {});
  // 最短の滞在時間より早く送ると FORM_TOKEN_TOO_SOON で断られる
  if (form.minWaitSeconds > 0) await new Promise((resolve) => setTimeout(resolve, form.minWaitSeconds * 1000 + 200));
  return callChecked<Applied>(request, 'signup-application/apply', { answers: [answer], formToken: form.token }, DATES);
}

test.describe('signup applications: the Elythia endpoints answer as declared', () => {
  test.skip(isTsBackend, '承認制の登録は Elythia だけにある');
  test.setTimeout(90_000);

  let original: Record<string, unknown> | undefined;
  test.beforeEach(async ({ request }) => {
    resetRateLimit();
    original = undefined;
    const root = rootFixture();
    const meta = await expectApiOk(request, 'admin/meta', { i: root.token });
    original = Object.fromEntries(KEYS.map((k) => [k, meta[k]]));
    await expectApiOk(request, 'admin/update-meta', {
      i: root.token, registrationClosed: false, approvalRequiredForSignup: true, disableRegistration: false,
      signupApplicationForm: [{ label: 'pw reason', type: 'text', required: false }],
      // メールの確認を挟むと、登録が 204 になって照合の対象 (200) と変わる
      emailRequiredForSignup: false,
    });
  });
  test.afterEach(async ({ request }) => {
    if (original === undefined) return;
    const restored = await callApi(request, 'admin/update-meta', { i: rootFixture().token, ...original });
    expect.soft(restored.ok(), '登録の受け付け方を元に戻せなかった').toBe(true);
  });

  test('apply, see the status, get approved and register', async ({ request }) => {
    const { token } = rootFixture();
    const answer = `pw-api-approve-${Date.now().toString(36)}`;
    const applied = await apply(request, answer);
    expect(applied.application.status).toBe('pending');
    const status = await callChecked<Applied>(request, 'signup-application/status', { claimCode: applied.claimCode }, DATES);
    expect(status.application.status).toBe('pending');

    const id = await pendingId(request, token, answer);
    expect(await callChecked(request, 'admin/signup-application/approve', { i: token, applicationId: id })).toEqual({ ok: true });
    expect((await callChecked<Applied>(request, 'signup-application/status', { claimCode: applied.claimCode }, DATES)).application.status).toBe('approved');

    const username = randomUsername('pwsar');
    const registered = await callChecked<{ username: string; token: string }>(request, 'signup-application/register', { claimCode: applied.claimCode, username, password: DEFAULT_TEST_PASSWORD });
    expect(registered.username).toBe(username);
    expect((await callApi(request, 'i', { i: registered.token })).status()).toBe(200);
    expect((await callChecked<Applied>(request, 'signup-application/status', { claimCode: applied.claimCode }, DATES)).application.status).toBe('completed');
  });

  test('apply and get rejected', async ({ request }) => {
    const { token } = rootFixture();
    const answer = `pw-api-reject-${Date.now().toString(36)}`;
    const applied = await apply(request, answer);
    const id = await pendingId(request, token, answer);
    expect(await callChecked(request, 'admin/signup-application/reject', { i: token, applicationId: id })).toEqual({ ok: true });
    const status = await callChecked<Applied>(request, 'signup-application/status', { claimCode: applied.claimCode }, DATES);
    expect(status.application.status).toBe('rejected');
  });
});
