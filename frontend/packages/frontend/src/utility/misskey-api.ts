/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { ref } from 'vue';
import { apiUrl } from '@@/js/config.js';
import type * as Elythia from 'elythia-js';
import { $i } from '@/i.js';
export const pendingApiRequestsCount = ref(0);

/**
 * The resolved response type: `ResT` when given explicitly, otherwise the one
 * the endpoint (and its arguments) decide.
 */
type ApiResponse<ResT, E extends keyof Elythia.Endpoints, P extends Elythia.Endpoints[E]['req']> =
	ResT extends void ? Elythia.SwitchCaseResponseType<E, P> : ResT;

// Elythia: 型引数について (#3417 / #3439)
// - E は本家の Endpoints に Elythia 独自のエンドポイントを重ねたものから取る
// - P は渡した値から推論する (本家の $switch による応答の出し分けに要る)。推論した P は
//   req に無いキーを持てるので、ExcessKeys を交差させて弾く
// - 戻り値は NoInfer で包む。包まないと、応答の型が代入先から推論され、
//   `x.value = await misskeyApi(...)` でエンドポイントを取り違えても型エラーにならない

// Implements Misskey.api.ApiClient.request
export function misskeyApi<
	ResT = void,
	E extends keyof Elythia.Endpoints = keyof Elythia.Endpoints,
	P extends Elythia.Endpoints[E]['req'] = Elythia.Endpoints[E]['req'],
>(
	endpoint: E,
	data: P & Elythia.ExcessKeys<E, P, 'i'> & { i?: string | null; } = {} as any,
	token?: string | null | undefined,
	signal?: AbortSignal,
): Promise<NoInfer<ApiResponse<ResT, E, P>>> {
	type Res = ApiResponse<ResT, E, P>;
	if (endpoint.includes('://')) throw new Error('invalid endpoint');
	pendingApiRequestsCount.value++;

	const onFinally = () => {
		pendingApiRequestsCount.value--;
	};

	const promise = new Promise<Res>((resolve, reject) => {
		// Append a credential
		if ($i) data.i = $i.token;
		if (token !== undefined) data.i = token;

		// Send request
		window.fetch(`${apiUrl}/${endpoint}`, {
			method: 'POST',
			body: JSON.stringify(data),
			credentials: 'omit',
			cache: 'no-cache',
			headers: {
				'Content-Type': 'application/json',
			},
			signal,
		}).then(async (res) => {
			const body = res.status === 204 ? null : await res.json();

			if (res.status === 200) {
				resolve(body);
			} else if (res.status === 204) {
				resolve(undefined as Res); // void -> undefined
			} else {
				reject(body.error);
			}
		}).catch(reject);
	});

	promise.then(onFinally, onFinally);

	return promise;
}

// Implements Misskey.api.ApiClient.request
// Elythia: 型引数の扱いは misskeyApi と同じ (#3439)
export function misskeyApiGet<
	ResT = void,
	E extends keyof Elythia.Endpoints = keyof Elythia.Endpoints,
	P extends Elythia.Endpoints[E]['req'] = Elythia.Endpoints[E]['req'],
>(
	endpoint: E,
	data: P & Elythia.ExcessKeys<E, P> = {} as any,
): Promise<NoInfer<ApiResponse<ResT, E, P>>> {
	type Res = ApiResponse<ResT, E, P>;
	pendingApiRequestsCount.value++;

	const onFinally = () => {
		pendingApiRequestsCount.value--;
	};

	const query = new URLSearchParams(data as any);

	const promise = new Promise<Res>((resolve, reject) => {
		// Send request
		window.fetch(`${apiUrl}/${endpoint}?${query}`, {
			method: 'GET',
			credentials: 'omit',
			cache: 'default',
		}).then(async (res) => {
			const body = res.status === 204 ? null : await res.json();

			if (res.status === 200) {
				resolve(body);
			} else if (res.status === 204) {
				resolve(undefined as Res); // void -> undefined
			} else {
				reject(body.error);
			}
		}).catch(reject);
	});

	promise.then(onFinally, onFinally);

	return promise;
}
