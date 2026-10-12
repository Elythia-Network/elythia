/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// #744 Phase 1: signup / signin helper.
// upstream Misskey TS と互換な /api/signup と /api/signin-flow を叩き、
// 取得した access token を spec から再利用できる形にする。
//
// 注: root account (admin/accounts/create) の作成と
// disableRegistration=false の切替は globalSetup で行う (spec 開始前に
// 1 度だけ)。本 fixture では signup endpoint と signin-flow の通常経路
// のみ提供する。

import { randomBytes } from 'node:crypto';
import type { APIRequestContext } from '@playwright/test';
import { callApi } from './api';

// upstream Misskey TS の username 仕様: 1〜20 文字、`[a-zA-Z0-9_]`。
// 本 helper はこの制約を必ず満たす uniq username を生成する。
const USERNAME_MAX = 20;

// randomUsername returns a uniq, validation-safe username for use in spec.
// 8 文字 hex random suffix + "_" + prefix で max 20 文字に収める。upstream
// の username regex は `^\w{1,20}$` で underscore 1 つは可。suffix は hex
// (= alphanumeric) のみなので追加の sanitize 不要。
//
// prefix が長すぎる場合は **silent に切り捨てず throw** する。spec 作成時
// に意図と違う username に縮められて debug 困難になる事故を防ぐ。
export function randomUsername(prefix: string): string {
  // 8 hex chars = 32 bit random で test 内 uniq には十分 (collision は
  // 1 / 2^32 ~ 10^-10 オーダー)。
  const suffix = randomBytes(4).toString('hex');
  const maxPrefix = USERNAME_MAX - suffix.length - 1;
  if (prefix.length > maxPrefix) {
    throw new Error(
      `randomUsername: prefix '${prefix}' exceeds ${maxPrefix} chars`,
    );
  }
  return `${prefix}_${suffix}`;
}

export interface Principal {
  id: string;
  token: string;
  username: string;
}

// DEFAULT_TEST_PASSWORD is the password used by signupUser when caller does
// not specify one. spec 側で signin-flow / i/change-password / i/update-email
// 等を叩く際にこの定数を再利用すれば signupUser の default が変わっても
// 同期する (= 個別 spec 内で 'password1234' を直書きしない、#827 review)。
export const DEFAULT_TEST_PASSWORD = 'password1234';

// signupUser creates a regular (non-root) account via /api/signup. Phase 1
// では captcha / email 認証は disabled な instance config を使うので
// username + password だけで通る。複数回呼んでも username が違えば成功。
//
// 同 instance で複数 user が必要な spec はこの helper を使う
// (admin/accounts/create は 1 度しか通らないので root 専用)。
export async function signupUser(
  request: APIRequestContext,
  username: string,
  password = DEFAULT_TEST_PASSWORD,
): Promise<Principal> {
  const resp = await callApi(request, 'signup', { username, password });
  if (resp.status() !== 200) {
    throw new Error(`signup failed: ${resp.status()} ${await resp.text()}`);
  }
  const body = await resp.json();
  return { id: body.id, token: body.token, username };
}

// signin posts /api/signin-flow and returns the access token. upstream
// Misskey TS は #705 で本家準拠化されており、レスポンスは
// `{finished: true, i: <token>}`。
//
// signin-flow が 2FA / passkey 経路に分岐するアカウント (= step 1 で
// `next: 'totp' | 'passkey'`) は本 helper では扱わない。Phase 1 では
// password のみの signin path を test する。
export async function signin(
  request: APIRequestContext,
  username: string,
  password: string,
): Promise<string> {
  const resp = await callApi(request, 'signin-flow', { username, password });
  if (resp.status() !== 200) {
    throw new Error(`signin-flow failed: ${resp.status()} ${await resp.text()}`);
  }
  const body = await resp.json();
  if (!body.i) {
    throw new Error(`signin-flow returned no token: ${JSON.stringify(body)}`);
  }
  return body.i as string;
}
