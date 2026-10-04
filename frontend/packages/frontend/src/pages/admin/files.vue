<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<PageWithHeader v-model:tab="tab" :actions="headerActions" :tabs="headerTabs">
	<div class="_spacer" style="--MI_SPACER-w: 900px;">
		<!--
			インスタンス全体のストレージ使用量 (mk-go #3053)。純正 backend には
			endpoint ごと無いので、取得に失敗したらタブの中で理由を出す。
		-->
		<XUsage v-if="tab === 'usage'"/>
		<div v-else class="_gaps">
			<div class="inputs" style="display: flex; gap: var(--MI-margin); flex-wrap: wrap;">
				<MkSelect v-model="origin" :items="originDef" style="margin: 0; flex: 1;">
					<template #label>{{ i18n.ts.instance }}</template>
				</MkSelect>
				<MkInput v-model="searchHost" :debounce="true" type="search" style="margin: 0; flex: 1;" :disabled="paginator.computedParams?.value?.origin === 'local'">
					<template #label>{{ i18n.ts.host }}</template>
				</MkInput>
			</div>
			<div class="inputs" style="display: flex; gap: var(--MI-margin); flex-wrap: wrap;">
				<MkInput v-model="userId" :debounce="true" type="search" style="margin: 0; flex: 1;">
					<template #label>User ID</template>
				</MkInput>
				<MkInput v-model="type" :debounce="true" type="search" style="margin: 0; flex: 1;">
					<template #label>MIME type</template>
				</MkInput>
			</div>
			<MkFileListForAdmin :paginator="paginator" :viewMode="viewMode"/>
		</div>
	</div>
</PageWithHeader>
</template>

<script lang="ts" setup>
import { computed, markRaw, onMounted, ref } from 'vue';
import * as Misskey from 'misskey-js';
import MkInput from '@/components/MkInput.vue';
import MkSelect from '@/components/MkSelect.vue';
import MkFileListForAdmin from '@/components/MkFileListForAdmin.vue';
import XUsage from '@/pages/admin/files.usage.vue';
import * as os from '@/os.js';
import { lookupFile } from '@/utility/admin-lookup.js';
import { i18n } from '@/i18n.js';
import { definePage } from '@/page.js';
import { useMkSelect } from '@/composables/use-mkselect.js';
import { Paginator } from '@/utility/paginator.js';
import { misskeyApi } from '@/utility/misskey-api.js';
import { cachedRemoteFileCount, cleanRemoteFilesState } from '@/utility/remote-cache-cleanup.js';
import type { RemoteUsageBucket } from '@/utility/remote-cache-cleanup.js';
import number from '@/filters/number.js';

const {
	model: origin,
	def: originDef,
} = useMkSelect({
	items: [
		{ label: i18n.ts.all, value: 'combined' },
		{ label: i18n.ts.local, value: 'local' },
		{ label: i18n.ts.remote, value: 'remote' },
	],
	initialValue: 'local',
});
const tab = ref<'files' | 'usage'>('files');
const type = ref<string | null>(null);
const searchHost = ref('');
const userId = ref('');
const viewMode = ref<'grid' | 'list'>('grid');
const paginator = markRaw(new Paginator('admin/drive/files', {
	limit: 10,
	computedParams: computed(() => ({
		type: (type.value && type.value !== '') ? type.value : null,
		userId: (userId.value && userId.value !== '') ? userId.value : null,
		origin: origin.value,
		hostname: (searchHost.value && searchHost.value !== '') ? searchHost.value : null,
	})),
}));

// **削除対象が実在するときだけ押せるようにする** (#3102)。
//
// mk-go はリモートメディアをキャッシュしないので、mk-go が作った行に対象は無い
// (docs/divergence.md 5.5)。**しかし純正 Misskey から引き継いだ DB には、
// `cacheRemoteFiles` が有効だった時期の実体つきの行が残る。** 以前は無条件に
// disabled にしていたので、その運用者は消せるはずのものを UI から消せなかった。
//
// 判定材料は `admin/drive/usage` (#3053) が既に返している。**取れなかったときは
// 押せるままにする** — あれは mk-go 独自の endpoint なので純正 backend に向けると
// 必ず失敗し、そちらは実際にキャッシュするため押せなくするのは誤り。
const remoteUsage = ref<RemoteUsageBucket | null>(null);

onMounted(async () => {
	try {
		const usage = await misskeyApi('admin/drive/usage' as never, {} as never) as unknown as { remote?: RemoteUsageBucket };
		remoteUsage.value = usage.remote ?? null;
	} catch {
		// 取れなくてもボタンは出す (上記の理由)。使用量タブ側が理由を表示する。
		remoteUsage.value = null;
	}
});

const cleanState = computed(() => cleanRemoteFilesState(remoteUsage.value));

function clear() {
	// **件数だけを出す。容量は出さない。** `admin/drive/usage` の `remote.size` は
	// リモート行**全部**の合計で、削除対象 (`isLink = false`) の合計ではない。
	// 純正は `expireOldFile` で link 化するとき `size` を据え置くので、
	// 引き継いだ DB には「実体は無いのに size を持つ link 行」が普通に溜まっており、
	// そのまま出すと**破壊的操作の確認画面で実際より大きい数字を断定する**
	// (敵対的レビューで指摘)。正しい値を出すには endpoint 側に
	// `isLink = false` の合計を足す必要がある。
	const detail = remoteUsage.value == null
		? i18n.ts._mkgoCleanRemoteFiles.unknownTarget
		: i18n.tsx._mkgoCleanRemoteFiles.target({
			n: number(cachedRemoteFileCount(remoteUsage.value)),
		});
	os.confirm({
		type: 'warning',
		title: i18n.ts.clearCachedFilesConfirm,
		// **不可逆であることを読み取れるようにする。** 相手サーバーが既に消した
		// ファイルは取り戻せない (再取得できるとは限らない)。
		text: `${detail}\n\n${i18n.ts._mkgoCleanRemoteFiles.irreversible}`,
	}).then(({ canceled }) => {
		if (canceled) return;

		os.apiWithDialog('admin/drive/clean-remote-files', {});
	});
}

const headerActions = computed(() => [{
	text: i18n.ts.lookup,
	icon: 'ti ti-search',
	handler: lookupFile,
}, {
	// **無効な理由を分けて出す。** 「対象がありません」は対象が 0 と
	// **確かめたとき**にだけ言う。
	text: cleanState.value === 'none'
		? `${i18n.ts.clearCachedFiles} (${i18n.ts._mkgoCleanRemoteFiles.noTarget})`
		: i18n.ts.clearCachedFiles,
	icon: 'ti ti-trash',
	disabled: cleanState.value === 'none',
	handler: clear,
}]);

const headerTabs = computed(() => [{
	key: 'files',
	title: i18n.ts.files,
	icon: 'ti ti-cloud',
}, {
	key: 'usage',
	title: i18n.ts._driveUsage.tab,
	icon: 'ti ti-chart-pie',
}]);

definePage(() => ({
	title: i18n.ts.files,
	icon: 'ti ti-cloud',
}));
</script>
