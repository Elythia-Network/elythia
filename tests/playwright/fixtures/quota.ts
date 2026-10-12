/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// per-user quota を使い切らないための spec 側 cleanup helper (#2264)。
//
// antenna / webhook / clip / user list は role policy に上限がある
// (antennaLimit 5 / webhookLimit 3 / clipLimit 10 / avatarDecorationLimit 1)。
// UI spec は root (alice) を共有するので、作りっぱなしにすると **無関係な
// spec** が setup の create で落ちる。落ちる場所が原因から離れるので診断が
// 難しく、実際 #2254 の調査中に spec 側の bug と誤認しかけた。
//
// globalSetup も run の先頭で root の quota を purge するが、そちらは
// 「前回 run の残骸」対策。1 回の run の中で枠を食い潰さないためには spec が
// 自分の作ったものを片付ける必要がある。

import type { APIRequestContext } from '@playwright/test';
import { callApi } from './api';

interface Named {
  id?: string;
  name?: string;
}

async function deleteByNames(
  request: APIRequestContext,
  token: string,
  listPath: string,
  deletePath: string,
  idKey: string,
  names: string[],
): Promise<void> {
  if (names.length === 0) return;
  const wanted = new Set(names);
  const listResp = await callApi(request, listPath, { i: token, limit: 100 });
  if (listResp.status() !== 200) return;
  const rows = (await listResp.json()) as Named[];
  if (!Array.isArray(rows)) return;
  for (const row of rows) {
    if (typeof row.id !== 'string' || !wanted.has(row.name ?? '')) continue;
    await callApi(request, deletePath, { i: token, [idKey]: row.id });
  }
}

// deleteAntennasNamed removes the antennas whose name is in names.
export async function deleteAntennasNamed(
  request: APIRequestContext,
  token: string,
  names: string[],
): Promise<void> {
  await deleteByNames(request, token, 'antennas/list', 'antennas/delete', 'antennaId', names);
}

// deleteWebhooksNamed removes the user webhooks whose name is in names.
export async function deleteWebhooksNamed(
  request: APIRequestContext,
  token: string,
  names: string[],
): Promise<void> {
  await deleteByNames(request, token, 'i/webhooks/list', 'i/webhooks/delete', 'webhookId', names);
}
