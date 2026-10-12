/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// notification spec 共通の helper / 型 (#823)。reaction / mention / follow
// 等の notification round-trip spec は WS push 受信後に
// /api/i/notifications で永続化を確認する pattern を共有する。

import { expect, type APIRequestContext } from '@playwright/test';
import { callApi } from './api';

// NotificationBody は WS notification event / /api/i/notifications で
// 共通する notification の最小 shape。spec 固有の field (reaction の絵文字
// 等) は optional で含め、spec 側で必要に応じて narrow する。
export interface NotificationBody {
  id: string;
  type: string;
  userId?: string;
  reaction?: string;
}

// pollForNotification polls /api/i/notifications until a notification matching
// the given predicate appears, then returns it. upstream Misskey TS は
// notification の write を queue 経由で行うので reaction / mention 等の
// trigger 直後は GET が空配列を返すことがある。`expect.poll` で 5s 範囲で
// 段階的に retry し、見つからない場合は assertion 失敗で reject する。
export async function pollForNotification(
  request: APIRequestContext,
  token: string,
  predicate: (n: NotificationBody) => boolean,
): Promise<NotificationBody> {
  let found: NotificationBody | undefined;
  await expect
    .poll(
      async () => {
        const resp = await callApi(request, 'i/notifications', { i: token });
        if (resp.status() !== 200) return false;
        const list = (await resp.json()) as NotificationBody[];
        found = list.find(predicate);
        return found !== undefined;
      },
      { timeout: 5000, intervals: [100, 200, 500, 1000] },
    )
    .toBe(true);
  // expect.poll が toBe(true) を満たした iteration で必ず found が set
  // される。predicate match を guard で再表現することで、!-assertion を
  // 使わずに型を narrow する (= 意図 = "match と found 設定は同期" を
  // 読み手に明示)。
  if (!found) {
    throw new Error('pollForNotification: matched but `found` was not set (unreachable)');
  }
  return found;
}
