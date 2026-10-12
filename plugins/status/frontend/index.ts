/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { definePlugin } from '@/plugin-api.js';
import { initApi } from './api.js';
import StatusCard from './StatusCard.vue';
import StatusSettings from './StatusSettings.vue';
import AdminStats from './AdminStats.vue';
import RecentPage from './RecentPage.vue';

export default definePlugin({
	name: 'status',

	// **ページは setup ではなくここで宣言する。** ルーターはモジュール読み込み時に
	// 現在の URL を解決するので、setup で登録すると直接アクセスが 404 になる。
	pages: [
		// 通常のページ。navTitle を付けると「もっと」(ランチパッド) に出て、
		// 利用者が設定でサイドバーに常設できる。
		//
		// **既定のサイドバーには入れない。** あそこは利用者の持ち物で、
		// 運営者が入れたプラグインが勝手に常設されるのは筋が悪い。
		{
			path: '/',
			component: RecentPage,
			navTitle: 'みんなのステータス',
			navIcon: 'ti ti-message-dots',
		},

		// 管理画面。navTitle を付けるとコントロールパネルのメニューに出る
		// (付けないと URL 直打ちでしか辿り着けない)。
		{
			path: '/',
			component: AdminStats,
			admin: true,
			navTitle: 'ステータス',
			navIcon: 'ti ti-message-dots',
		},
	],

	setup(host) {
		initApi(host.api);

		// Vue コンポーネント形式で登録すると、ホストのアプリ内で描画されるので
		// MkInput などが本体と同じ見た目・挙動で動く。
		host.slot('profile:info', { component: StatusCard });

		// 未ログインでは設定画面自体が出ないが、念のため。
		if (host.me != null) {
			host.slot('settings:profile', { component: StatusSettings });
		}

	},
});
