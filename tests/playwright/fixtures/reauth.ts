/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// 再認証 (パスワードと TOTP) が要る管理画面の API を、API から叩くための管理者を作る (#4093)。
// admin/backup/* は、要求のたびにパスワードと 2 つ目の要素で再認証する。

import { type APIRequestContext, expect } from '@playwright/test';
import { authenticator } from 'otplib';
import { callApi } from './api';
import { type Principal, DEFAULT_TEST_PASSWORD, randomUsername, signupUser } from './auth';
import type { RootFixture } from './ui_auth';

const STEP_SECONDS = 30;

/** An administrator with TOTP registered, and the codes to re-authenticate. */
export interface TotpAdmin {
  admin: Principal;
  /** The id of the role that makes `admin` an administrator. Delete it after the test. */
  roleId: string;
  /** The password to send with each re-authentication. */
  password: string;
  /**
   * The TOTP code of the step `offset` from the one used to register (-1 or
   * +1). The server accepts the step before and after its own, and each code
   * only once, so a test has these two codes for two requests. The code of
   * the step before stops working when the server moves to the next step,
   * so use it first.
   */
  code(offset: -1 | 1): string;
}

/**
 * Makes an administrator with TOTP registered. It waits for the next step when
 * the current one is about to end, so that the codes of the steps before and
 * after stay valid during the test.
 */
export async function totpAdmin(request: APIRequestContext, root: RootFixture, prefix: string): Promise<TotpAdmin> {
  // 登録に使った段の前後の 2 つのコードを使う。サーバーの段が進むと前の段のコードが
  // 通らなくなるので、残りが短ければ次の段の頭まで待つ
  const left = STEP_SECONDS - ((Date.now() / 1000) % STEP_SECONDS);
  if (left < 15) await new Promise((resolve) => setTimeout(resolve, left * 1000 + 500));

  const admin = await signupUser(request, randomUsername(prefix));
  const role = await callApi(request, 'admin/roles/create', {
    i: root.token, name: `pw-reauth-${Date.now()}`, description: '', color: null, iconUrl: null,
    target: 'manual', condFormula: {}, isPublic: false, isExplorable: false,
    isAdministrator: true, isModerator: false, asBadge: false,
    canEditMembersByModerator: false, displayOrder: 0, policies: {},
  });
  expect(role.status(), await role.text()).toBe(200);
  const roleId = ((await role.json()) as { id: string }).id;
  expect((await callApi(request, 'admin/roles/assign', { i: root.token, roleId, userId: admin.id })).status()).toBe(204);

  const registered = await callApi(request, 'i/2fa/register', { i: admin.token, password: DEFAULT_TEST_PASSWORD });
  expect(registered.status(), await registered.text()).toBe(200);
  const { secret } = (await registered.json()) as { secret: string };
  const epoch = Date.now();
  expect((await callApi(request, 'i/2fa/done', { i: admin.token, token: authenticator.clone({ epoch }).generate(secret) })).status()).toBe(200);
  return {
    admin,
    roleId,
    password: DEFAULT_TEST_PASSWORD,
    code: (offset) => authenticator.clone({ epoch: epoch + offset * STEP_SECONDS * 1000 }).generate(secret),
  };
}
