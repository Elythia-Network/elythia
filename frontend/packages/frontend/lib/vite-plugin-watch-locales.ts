/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import path from 'node:path';
import locales from 'i18n';
import type { Plugin } from 'vite';

const localesDir = path.resolve(__dirname, '../../../locales')
// Elythia: 本家の語に重ねる Elythia の語も監視する (#4071)
const elythiaLocalesDir = path.resolve(__dirname, '../../../locales-elythia');

/**
 * 外部ファイルを監視し、必要に応じてwebSocketでメッセージを送るViteプラグイン
 */
export default function pluginWatchLocales(): Plugin {
	return {
		name: 'watch-locales',

		configureServer(server) {
			const localeYmlPaths = Object.keys(locales).flatMap(locale => [localesDir, elythiaLocalesDir].map(dir => path.join(dir, `${locale}.yml`)));

			// watcherにパスを追加
			server.watcher.add(localeYmlPaths);

			const notifyLocaleUpdate = (filePath: string) => {
				if (localeYmlPaths.includes(filePath)) {
					server.ws.send({
						type: 'custom',
						event: 'locale-update',
						data: filePath.match(/([^\/]+)\.yml$/)?.[1] || null,
					})
				}
			};

			server.watcher.on('change', notifyLocaleUpdate);
			// Elythia: 起動後に作った locales-elythia のファイルも拾う (#4071)
			server.watcher.on('add', notifyLocaleUpdate);
		},
	};
}
