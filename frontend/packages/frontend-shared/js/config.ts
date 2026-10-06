/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

// eslint-disable-next-line @typescript-eslint/prefer-nullish-coalescing
const address = new URL(window.document.querySelector<HTMLMetaElement>('meta[property="instance_url"]')?.content || window.location.href);
const siteName = window.document.querySelector<HTMLMetaElement>('meta[property="og:site_name"]')?.content;

export const host = address.host;
export const hostname = address.hostname;
export const url = address.origin;
export const port = address.port;
export const apiUrl = window.location.origin + '/api';
export const wsOrigin = window.location.origin;
export const lang = localStorage.getItem('lang') ?? 'en-US';
export const langs = _LANGS_;
export const version = _VERSION_;
// Elythia: サイト名が未設定のとき、サーバーは og:site_name に既定の名前を入れる (2.0.0 から Elythia、それより前と
// 本家は Misskey)。どちらも「未設定」として扱い、ホスト名を出す
export const instanceName = (siteName === 'Elythia' || siteName === 'Misskey' || siteName == null) ? host : siteName;
export const ui = localStorage.getItem('ui');
export const debug = localStorage.getItem('debug') === 'true';
export const isSafeMode = localStorage.getItem('isSafeMode') === 'true';
export const prefersReducedMotion = window.matchMedia('(prefers-reduced-motion)').matches;
