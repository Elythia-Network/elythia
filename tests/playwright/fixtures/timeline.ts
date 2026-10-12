/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Phase 2 #819 timeline spec helper.
//
// timeline 系 endpoint (notes/timeline / local-timeline / global-timeline /
// hybrid-timeline) で共通の fetch / poll を提供する。upstream Misskey TS と
// mk-go は両方とも fanout を Redis 経由で行うので、note 投稿直後は該当
// timeline に未反映なことがある。positive 側 (= 出るはず) は expect.poll
// で 5s 範囲で retry し、negative 側 (= 出ないはず) は positive 用 poll が
// 通った直後 (= fanout が settle した状態) に同 fetch 結果を再利用する形で
// 確認する。

import { expect, type APIRequestContext } from '@playwright/test';
import { callApi } from './api';

export interface TimelineNote {
  id: string;
  userId: string;
  text?: string | null;
  visibility: string;
}

// fetchTimelineNotes returns the timeline as an array of minimal note shapes.
// 4 つの timeline endpoint で共通の request body を受け付けるので、endpoint
// だけ切り替える。token は null 可 (= local / global は 未ログイン user 用
// path もあるが、本 helper は spec から token 付きで呼ぶ前提)。
//
// 注意: token 引数を渡すと body.i は上書きされる (= 認証 token は引数側で
// 一元管理する設計)。一方 body.limit は明示があれば尊重し、未指定時のみ
// default 100 を補う (= caller が limit を絞りたい spec を書ける)。
export async function fetchTimelineNotes(
  request: APIRequestContext,
  endpoint: string,
  token: string | null,
  body: Record<string, unknown> = {},
): Promise<TimelineNote[]> {
  const payload: Record<string, unknown> = { ...body };
  if (token) payload.i = token;
  if (payload.limit === undefined) payload.limit = 100;
  const resp = await callApi(request, endpoint, payload);
  if (resp.status() !== 200) {
    throw new Error(
      `${endpoint} failed: ${resp.status()} ${await resp.text()}`,
    );
  }
  return (await resp.json()) as TimelineNote[];
}

// pollForTimelineNote polls the given timeline endpoint until noteId appears
// or the timeout elapses. timeline fanout は async (Redis) なので note 投稿
// 直後は反映が遅れる可能性があり、5s 範囲で段階的に retry する (notification
// 用 pollForNotification と同 pattern)。見つからなければ assertion 失敗。
//
// body は endpoint 固有の追加 param (= 例: user-list-timeline の listId、
// channel-timeline の channelId) を渡したい場合に使う。fetchTimelineNotes
// と同じく body.i / body.limit は helper 管理。
export async function pollForTimelineNote(
  request: APIRequestContext,
  endpoint: string,
  token: string | null,
  noteId: string,
  body: Record<string, unknown> = {},
): Promise<void> {
  await expect
    .poll(
      async () => {
        const notes = await fetchTimelineNotes(request, endpoint, token, body);
        return notes.some((n) => n.id === noteId);
      },
      { timeout: 5000, intervals: [100, 200, 500, 1000] },
    )
    .toBe(true);
}
