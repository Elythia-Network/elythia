/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// Backend-dependent expectations.
//
// spec は mk-go / Misskey TS の両 backend で走らせて drop-in 互換を検証する
// (tests/playwright/compose.ts.yml)。ただし mk-go には docs/divergence.md に
// 記録済みの意図的な差分があり、そこだけは backend ごとに期待値を変える必要が
// ある。差分そのものを spec から消すと mk-go 側の検証が緩くなるので、
// 「どちらでも通る」ゆるい assert ではなく backend ごとの厳密値を使う。

/**
 * True when the spec runs against the upstream Misskey TS backend.
 *
 * 利用箇所は 2 種類ある。
 *
 * **upstream と意図的に挙動が違うもの** (embed の 2 件、#2289):
 *
 *   - `/embed/notes/:note` が埋め込み不可のとき upstream は**空 body**、mk-go は
 *     文脈なしのシェルを返す (存在の有無を応答の形で区別させないため)
 *   - `/embed/clips/:clip` は upstream が**非公開 clip も埋め込める**のに対し、
 *     mk-go は `isPublic` も見て弾く
 *
 * どちらも「どちらでも通る」ゆるい assert にはしない。差分を消すと mk-go 側の
 * 防御が外れても気付けなくなる。
 *
 * **mk-go にしか無い機能** (TS baseline では画面も field も存在しない):
 * 承認制サインアップ (`admin_moderation_approval_off_dialog` は丸ごと skip、
 * `admin_moderation_email_required_signup_toggle` は field の確認だけ)、
 * 起動画面の夜空 (`smoke/frontend_load`。#2549 の起動スピナーを 2.0.0 で置き換えた)。2026.9.1 の追従で TS baseline を
 * 回したときに、これらが TS で落ちることが分かった。
 */
export const isTsBackend = process.env.MK_BACKEND_TYPE === 'ts';

/**
 * Expected HTTP status for NO_SUCH_* style errors.
 *
 * upstream は `ApiError` の `kind` 既定が `'client'` なので
 * `ApiCallService.#sendApiError` の `statusCode ?? 400` に落ちて、対象が
 * 存在しない場合も 400 を返す。mk-go はかつて意味的に正確な 404 を返して
 * おり backend ごとに期待値を分けていたが、drop-in 互換 (status で分岐する
 * クライアントを壊さないこと) を優先して upstream に合わせたため、現在は
 * 両者とも 400 で一致する。
 */
export const NOT_FOUND_STATUS = 400;
