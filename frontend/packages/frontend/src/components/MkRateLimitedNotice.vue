<!--
SPDX-FileCopyrightText: mk-go project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div v-if="paginator.rateLimited.value" :class="$style.root">
	<MkInfo warn>{{ i18n.ts.rateLimitExceeded }}</MkInfo>
	<MkButton rounded :disabled="!paginator.canRetryAfterRateLimit.value" @click="retry">
		{{ paginator.canRetryAfterRateLimit.value ? i18n.ts.retry : i18n.ts.rateLimitCooldown }}
	</MkButton>
</div>
</template>

<script lang="ts" setup>
import MkInfo from '@/components/MkInfo.vue';
import MkButton from '@/components/MkButton.vue';
import { i18n } from '@/i18n.js';
import type { IPaginator } from '@/utility/paginator.js';

/*
 * mk-go: レート制限に当たったことを利用者に伝える (#2955)。
 *
 * **自動追い読みを持つすべての場所で使う。** 当初は `MkPagination` にだけ
 * 置いていたが、`i/notifications` は `MkStreamingNotificationsTimeline` が
 * 独自のボタンを持ち `MkPagination` を経由しない。**そちらでは理由が出ず、
 * ボタンが黙って消えるだけ**になっていた。利用者は「これで全部」と誤解する。
 *
 * **state は一切持たない。** この notice は親の v-if の枝なので、再試行で
 * `fetching` が立つと**アンマウントされる**。冷却をここに置くと消えてしまい、
 * 429 で戻ったときに新しいインスタンスが冷却なしで生える (実測)。判断も冷却も
 * `Paginator` 側にあり、ここは表示するだけ。
 *
 * **待ち時間は出せない。** `Retry-After` は HTTP ヘッダにしかなく、
 * `misskeyApi` は `Response` を捨てて `body.error` だけを reject する。
 */
const props = defineProps<{
	paginator: IPaginator;
}>();

function retry(): void {
	props.paginator.retryAfterRateLimit().catch(() => { /* 失敗は notice が出たままになる */ });
}
</script>

<style lang="scss" module>
.root {
	display: flex;
	flex-direction: column;
	align-items: center;
	gap: 8px;
	margin: 8px 0;
}
</style>
