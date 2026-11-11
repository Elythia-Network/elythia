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
					{{ i18n.ts._mkgoServerPlugins.orphanCheckFailed }}
				</MkInfo>
				<MkInfo v-else-if="result.orphanSchemas.length > 0" warn>
					{{ i18n.tsx._mkgoServerPlugins.orphanData({ schemas: result.orphanSchemas.join(', ') }) }}
					<br>{{ i18n.ts._mkgoServerPlugins.orphanDataRemove }}
				</MkInfo>

				<MkInfo v-if="result.plugins.length === 0" data-testid="server-plugins-empty">
					{{ i18n.ts._mkgoServerPlugins.none }}
				</MkInfo>

				<MkFolder v-for="p in result.plugins" :key="p.name" :defaultOpen="true">
					<template #icon><i class="ti ti-puzzle"></i></template>
					<template #label>{{ p.name }}</template>
					<template #suffix>{{ p.enabled ? i18n.ts._mkgoServerPlugins.enabled : i18n.ts._mkgoServerPlugins.disabled }}</template>

					<div class="_gaps_s">
						<MkKeyValue oneline>
							<template #key>{{ i18n.ts._mkgoServerPlugins.version }}</template>
							<template #value>{{ p.version || i18n.ts._mkgoServerPlugins.notSet }} / API v{{ p.apiVersion }}</template>
						</MkKeyValue>
						<MkKeyValue oneline>
							<template #key>{{ i18n.ts._mkgoServerPlugins.status }}</template>
							<template #value>{{ p.enabled ? i18n.ts._mkgoServerPlugins.enabled : i18n.ts._mkgoServerPlugins.disabledByConfig }}</template>
						</MkKeyValue>
						<MkKeyValue oneline>
							<template #key>{{ i18n.ts._mkgoServerPlugins.capabilities }}</template>
							<template #value>{{ capabilities(p) }}</template>
						</MkKeyValue>
						<MkKeyValue oneline>
							<template #key>{{ i18n.ts._mkgoServerPlugins.data }}</template>
							<template #value>{{ i18n.tsx._mkgoServerPlugins.schemaWithMigrations({ schema: p.schema, n: p.migrations }) }}</template>
						</MkKeyValue>
						<MkKeyValue v-if="p.configKeys.length > 0" oneline>
							<template #key>{{ i18n.ts._mkgoServerPlugins.configKeys }}</template>
							<template #value>{{ i18n.tsx._mkgoServerPlugins.configKeysHidden({ keys: p.configKeys.join(', ') }) }}</template>
						</MkKeyValue>
						<!--
							秘密の値 (#3470)。状態も管理者にしか返らないので、管理者にだけ出す。
							入力口は有効なプラグインにしか張られない。
						-->
						<MkFolder v-if="p.secrets.length > 0 && $i?.isAdmin">
							<template #icon><i class="ti ti-key"></i></template>
							<template #label>{{ i18n.ts._mkgoPluginSecrets.title }}</template>
							<template #suffix>{{ i18n.tsx._mkgoServerPlugins.count({ n: p.secrets.length }) }}</template>
							<MkPluginSecrets v-if="p.enabled" :plugin="p.name"/>
							<MkInfo v-else>{{ i18n.ts._mkgoPluginSecrets.disabledPlugin }}</MkInfo>
						</MkFolder>
						<MkKeyValue v-if="pagesOf(p.name).length > 0">
							<template #key>{{ i18n.ts._mkgoServerPlugins.pages }}</template>
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
import type * as Elythia from 'elythia-js';
import MkKeyValue from '@/components/MkKeyValue.vue';
import MkFolder from '@/components/MkFolder.vue';
import MkInfo from '@/components/MkInfo.vue';
import MkPluginSecrets from '@/components/MkPluginSecrets.vue';
import { $i } from '@/i.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { definePage } from '@/page.js';
import { i18n } from '@/i18n.js';
import { collectPages } from '@/plugin-api.js';
import { serverPlugins } from '@/server-plugins.generated.js';

type ServerPluginInfo = Elythia.ServerPluginInfo;

async function fetchPlugins() {
	const res = await misskeyApi('admin/server-plugins', {});
	return res;
}

function hasFrontend(name: string): boolean {
	return serverPlugins.some(sp => sp.name === name);
}

function capabilities(p: ServerPluginInfo): string {
	const caps: string[] = [];
	if (p.routes) caps.push('API');
	if (p.jobs) caps.push(i18n.ts._mkgoServerPlugins.capabilityJobs);
	if (hasFrontend(p.name)) caps.push(i18n.ts._mkgoServerPlugins.capabilityFrontend);
	return caps.length > 0 ? caps.join(' / ') : i18n.ts._mkgoServerPlugins.capabilityNone;
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
			label: i18n.tsx._mkgoServerPlugins.adminPage({ name: pg.navTitle ?? pg.fullPath }),
			icon: pg.navIcon ?? defaultPageIcon,
		}));
	return [...normal, ...admin];
}

const headerActions = computed(() => []);

const headerTabs = computed(() => []);

definePage(() => ({
	title: i18n.ts._mkgoServerPlugins.title,
	icon: 'ti ti-puzzle',
}));
</script>

<style lang="scss" module>
.pageIcon {
	margin-right: 6px;
	opacity: 0.8;
}
</style>
