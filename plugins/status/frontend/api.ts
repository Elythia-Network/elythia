/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

/*
 * バックエンド呼び出しの薄いラッパ。
 *
 * host.api は POST 固定 (misskeyApi と同じ) なので、バックエンド側も POST で
 * 揃えてある。
 */

let call: <T>(endpoint: string, params?: Record<string, unknown>) => Promise<T>;

/** Called once from the plugin's setup. */
export function initApi(fn: typeof call): void {
	call = fn;
}

export function api<T>(path: string, params: Record<string, unknown> = {}): Promise<T> {
	return call<T>(`plugin/status/${path}`, params);
}

export type Duration = '1h' | '1d' | '1w' | '';

export type MeResponse = {
	text: string | null;
	expiresAt?: string | null;
	maxLength: number;
};

export type ShowResponse =
	| { set: false }
	| { set: true; text: string; expiresAt: string | null; updatedAt: string };
