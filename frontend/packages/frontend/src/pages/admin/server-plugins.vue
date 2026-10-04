<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<!--
	mk-go: 組み込み済みサーバープラグインの一覧 (#2497)。
	backend の admin/server-plugins (構成・状態) と、クライアント側の生成物
	serverPlugins (frontend の有無・宣言ページ) を突き合わせて表示する。
-->
<template>
<PageWithHeader :actions="headerActions" :tabs="headerTabs">
	<div class="_spacer" style="--MI_SPACER-w: 700px; --MI_SPACER-min: 16px; --MI_SPACER-max: 32px;">
		<MkSuspense v-slot="{ result }" :p="fetchPlugins">
			<div class="_gaps">
				<!-- null は「確認できなかった」。空配列 (= 残存なし) と混同させない -->
				<MkInfo v-if="result.orphanSchemas == null" warn>
					プラグインの残存データを確認できませんでした。サーバーログを参照してください。
				</MkInfo>
				<MkInfo v-else-if="result.orphanSchemas.length > 0" warn>
					使われていないプラグインのデータが残っています: {{ result.orphanSchemas.join(', ') }}
					<br>不要なら手動で削除してください。
				</MkInfo>

				<MkInfo v-if="result.plugins.length === 0">
					組み込まれているサーバープラグインはありません。プラグインはビルド時に組み込まれます。
				</MkInfo>

				<MkFolder v-for="p in result.plugins" :key="p.name" :defaultOpen="true">
					<template #icon><i class="ti ti-puzzle"></i></template>
					<template #label>{{ p.name }}</template>
					<template #suffix>{{ p.enabled ? '有効' : '無効' }}</template>

					<div class="_gaps_s">
						<MkKeyValue oneline>
							<template #key>バージョン</template>
							<template #value>{{ p.version || '(未設定)' }} / API v{{ p.apiVersion }}</template>
						</MkKeyValue>
						<MkKeyValue oneline>
							<template #key>状態</template>
							<template #value>{{ p.enabled ? '有効' : '無効（設定で止められています。ビルドには含まれています）' }}</template>
						</MkKeyValue>
						<MkKeyValue oneline>
							<template #key>機能</template>
							<template #value>{{ capabilities(p) }}</template>
						</MkKeyValue>
						<MkKeyValue oneline>
							<template #key>データ</template>
							<template #value>{{ p.schema }}（migration {{ p.migrations }}件）</template>
						</MkKeyValue>
						<MkKeyValue v-if="p.configKeys.length > 0" oneline>
							<template #key>設定キー</template>
							<template #value>{{ p.configKeys.join(', ') }}（値は表示されません）</template>
						</MkKeyValue>
						<MkKeyValue v-if="pagesOf(p.name).length > 0">
							<template #key>ページ</template>
							<template #value>
								<div class="_gaps_s">
									<MkA v-for="pg in pagesOf(p.name)" :key="pg.to" :to="pg.to" class="_link">
										<!--
											プラグインが navIcon を宣言していればそれを出す。
											アイコンがあるとリンクだと分かりやすい (mk-go #2543)。
										-->
										<i :class="[pg.icon, $style.pageIcon]"></i>{{ pg.label }}
									</MkA>
								</div>
							</template>
						</MkKeyValue>
					</div>
				</MkFolder>
			</div>
		</MkSuspense>
	</div>
</PageWithHeader>
</template>

<script lang="ts" setup>
import { computed } from 'vue';
import MkKeyValue from '@/components/MkKeyValue.vue';
import MkFolder from '@/components/MkFolder.vue';
import MkInfo from '@/components/MkInfo.vue';
import { misskeyApi } from '@/utility/misskey-api.js';
import { definePage } from '@/page.js';
import { collectPages } from '@/plugin-api.js';
import { serverPlugins } from '@/server-plugins.generated.js';

type ServerPluginInfo = {
	name: string;
	version: string;
	apiVersion: number;
	enabled: boolean;
	routes: boolean;
	jobs: boolean;
	migrations: number;
	schema: string;
	configKeys: string[];
};

type ServerPluginsResponse = {
	plugins: ServerPluginInfo[];
	orphanSchemas: string[] | null;
};

// mk-go 独自のエンドポイントなので misskey-js の型集合には無い。
// plugin-api.ts の api() と同じ理由の cast。
const fetchPlugins = () => misskeyApi('admin/server-plugins' as never, {} as never) as unknown as Promise<ServerPluginsResponse>;

function hasFrontend(name: string): boolean {
	return serverPlugins.some(sp => sp.name === name);
}

function capabilities(p: ServerPluginInfo): string {
	const caps: string[] = [];
	if (p.routes) caps.push('API');
	if (p.jobs) caps.push('ジョブ');
	if (hasFrontend(p.name)) caps.push('フロントエンド');
	return caps.length > 0 ? caps.join(' / ') : '(なし)';
}

// navIcon を宣言していないプラグインもあるので既定を用意する。
const defaultPageIcon = 'ti ti-external-link';

function pagesOf(name: string): { to: string; label: string; icon: string }[] {
	const normal = collectPages(serverPlugins, false)
		.filter(pg => pg.plugin === name)
		.map(pg => ({ to: pg.fullPath, label: pg.navTitle ?? pg.fullPath, icon: pg.navIcon ?? defaultPageIcon }));
	const admin = collectPages(serverPlugins, true)
		.filter(pg => pg.plugin === name)
		.map(pg => ({
			to: `/admin${pg.fullPath}`,
			label: `${pg.navTitle ?? pg.fullPath}（管理画面）`,
			icon: pg.navIcon ?? defaultPageIcon,
		}));
	return [...normal, ...admin];
}

const headerActions = computed(() => []);

const headerTabs = computed(() => []);

definePage(() => ({
	title: 'サーバープラグイン',
	icon: 'ti ti-puzzle',
}));
</script>

<style lang="scss" module>
.pageIcon {
	margin-right: 6px;
	opacity: 0.8;
}
</style>
