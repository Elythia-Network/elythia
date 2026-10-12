/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { definePlugin } from '@/plugin-api.js';
import { initApi } from './api.js';
import AdminPage from './AdminPage.vue';

export default definePlugin({
	name: 'trustlevel',

	// **ページは setup ではなくここで宣言する。** ルーターはモジュール読み込み時に
	// 現在の URL を解決するので、setup で登録すると直接アクセスが 404 になる。
	pages: [
		{
			path: '/',
			component: AdminPage,
			admin: true,
			navTitle: '自動ロール付与',
			navIcon: 'ti ti-arrow-badge-up',
		},
	],

	setup(host) {
		initApi(host.api);
	},
});
