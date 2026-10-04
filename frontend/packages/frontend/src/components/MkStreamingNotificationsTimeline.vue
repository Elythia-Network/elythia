<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<component :is="prefer.s.enablePullToRefresh ? MkPullToRefresh : 'div'" :refresher="() => reload()">
	<MkLoading v-if="paginator.fetching.value"/>

	<!--
		**mk-go: MkError より前に置く (#2955)。**
		v-if チェーンは排他で、init() の catch は error も立てる。後ろに置くと
		初回取得の 429 で必ず MkError (=「何かがおかしいようです」) が選ばれ、
		理由が画面に出ない。自走が止まった直後に人が押すもの (F5 / reload /
		pull-to-refresh / MkError の retry) はほぼ全部 init() なので、この
		経路に入りやすい。
	
		**条件は error。** items の件数で見ると、streaming が 1 件 prepend した
		瞬間に枝から外れて MkError に戻る (realtimeMode は既定 true で、init の
		成否と無関係に繋ぐ)。error は init の失敗でしか立たないので「初回取得が
		失敗した」を正確に表す。429 以外の失敗では Paginator 側が印を落とす。
	-->
	<MkRateLimitedNotice v-else-if="paginator.rateLimited.value && paginator.error.value" :paginator="paginator"/>

	<MkError v-else-if="paginator.error.value" @retry="paginator.init()"/>

	<div v-else-if="paginator.items.value.length === 0" key="_empty_">
		<slot name="empty"><MkResult type="empty" :text="i18n.ts.noNotifications"/></slot>
	</div>

	<div v-else ref="rootEl">
		<component
			:is="prefer.s.animation ? TransitionGroup : 'div'" :class="[$style.notifications]"
			:enterActiveClass="$style.transition_x_enterActive"
			:leaveActiveClass="$style.transition_x_leaveActive"
			:enterFromClass="$style.transition_x_enterFrom"
			:leaveToClass="$style.transition_x_leaveTo"
			:moveClass="$style.transition_x_move"
			tag="div"
		>
			<div v-for="(notification, i) in paginator.items.value" :key="notification.id" :data-scroll-anchor="notification.id" :class="$style.item">
				<div v-if="i > 0 && isSeparatorNeeded(paginator.items.value[i -1].createdAt, notification.createdAt)" :class="$style.date">
					<span><i class="ti ti-chevron-up"></i> {{ getSeparatorInfo(paginator.items.value[i -1].createdAt, notification.createdAt)?.prevText }}</span>
					<span style="height: 1em; width: 1px; background: var(--MI_THEME-divider);"></span>
					<span>{{ getSeparatorInfo(paginator.items.value[i -1].createdAt, notification.createdAt)?.nextText }} <i class="ti ti-chevron-down"></i></span>
				</div>
				<MkNote v-if="['reply', 'quote', 'mention'].includes(notification.type) && 'note' in notification" :class="$style.content" :note="notification.note" :withHardMute="true"/>
				<XNotification v-else :class="$style.content" :notification="notification" :withTime="true" :full="true"/>
			</div>
		</component>
		<!-- mk-go: レート制限の理由と再試行 (#2955)。落とすだけだと「これで全部」に見える。 -->
		<MkRateLimitedNotice :paginator="paginator"/>
		<button v-show="paginator.canFetchOlder.value" key="_more_" v-appear="prefer.s.enableInfiniteScroll ? paginator.fetchOlder : null" :disabled="paginator.fetchingOlder.value" class="_button" :class="$style.more" @click="paginator.fetchOlder">
			<div v-if="!paginator.fetchingOlder.value">{{ i18n.ts.loadMore }}</div>
			<MkLoading v-else/>
		</button>
	</div>
</component>
</template>

<script lang="ts" setup>
import { onUnmounted, onMounted, computed, useTemplateRef, TransitionGroup, markRaw, watch } from 'vue';
import * as Misskey from 'misskey-js';
import { notificationTypes } from 'misskey-js';
import { useInterval } from '@@/js/use-interval.js';
import { useDocumentVisibility } from '@@/js/use-document-visibility.js';
import { getScrollContainer, scrollToTop } from '@@/js/scroll.js';
import XNotification from '@/components/MkNotification.vue';
import MkRateLimitedNotice from '@/components/MkRateLimitedNotice.vue';
import MkNote from '@/components/MkNote.vue';
import { useStream } from '@/stream.js';
import { i18n } from '@/i18n.js';
import MkPullToRefresh from '@/components/MkPullToRefresh.vue';
import { prefer } from '@/preferences.js';
import { store } from '@/store.js';
import { isSeparatorNeeded, getSeparatorInfo } from '@/utility/timeline-date-separate.js';
import { Paginator } from '@/utility/paginator.js';
import { createReconnectResync, paginatorResyncOptions, NOTIFICATION_RESYNC_PARAMS } from '@/utility/reconnect-resync.js';

const props = defineProps<{
	excludeTypes?: typeof notificationTypes[number][] | null;
}>();

const rootEl = useTemplateRef('rootEl');

const paginator = prefer.s.useGroupedNotifications ? markRaw(new Paginator('i/notifications-grouped', {
	limit: 20,
	computedParams: computed(() => ({
		excludeTypes: props.excludeTypes ?? undefined,
	})),
})) : markRaw(new Paginator('i/notifications', {
	limit: 20,
	computedParams: computed(() => ({
		excludeTypes: props.excludeTypes ?? undefined,
	})),
}));

const MIN_POLLING_INTERVAL = 1000 * 10;
const POLLING_INTERVAL =
	prefer.s.pollingInterval === 1 ? MIN_POLLING_INTERVAL * 1.5 * 1.5 :
	prefer.s.pollingInterval === 2 ? MIN_POLLING_INTERVAL * 1.5 :
	prefer.s.pollingInterval === 3 ? MIN_POLLING_INTERVAL :
	MIN_POLLING_INTERVAL;

if (!store.s.realtimeMode) {
	useInterval(async () => {
		paginator.fetchNewer({
			toQueue: false,
		});
	}, POLLING_INTERVAL, {
		immediate: false,
		afterMounted: true,
	});
}

function isTop() {
	if (scrollContainer == null) return true;
	if (rootEl.value == null) return true;
	const scrollTop = scrollContainer.scrollTop;
	const tlTop = rootEl.value.offsetTop - scrollContainer.offsetTop;
	return scrollTop <= tlTop;
}

function releaseQueue() {
	paginator.releaseQueue();
	scrollToTop(rootEl.value!);
}

let scrollContainer: HTMLElement | null = null;

function onScrollContainerScroll() {
	if (isTop()) {
		paginator.releaseQueue();
	}
}

watch(rootEl, (el) => {
	if (el && scrollContainer == null) {
		scrollContainer = getScrollContainer(el);
		if (scrollContainer == null) return;
		scrollContainer.addEventListener('scroll', onScrollContainerScroll, { passive: true }); // ほんとはscrollendにしたいけどiosが非対応
	}
}, { immediate: true });

const visibility = useDocumentVisibility();
let isPausingUpdate = false;

watch(visibility, () => {
	if (visibility.value === 'hidden') {
		isPausingUpdate = true;
	} else { // 'visible'
		isPausingUpdate = false;
		if (isTop()) {
			releaseQueue();
		}
	}
});

function onNotification(notification: Misskey.entities.Notification) {
	const isMuted = props.excludeTypes ? props.excludeTypes.includes(notification.type as typeof notificationTypes[number]) : false;
	if (isMuted || window.document.visibilityState === 'visible') {
		if (store.s.realtimeMode) {
			useStream().send('readNotification');
		}
	}

	if (!isMuted) {
		if (isTop() && !isPausingUpdate) {
			paginator.prepend(notification);
		} else {
			paginator.enqueue(notification);
		}
	}
}

function reload() {
	return paginator.reload();
}

let connection: Misskey.IChannelConnection<Misskey.Channels['main']> | null = null;

// **切断していた間の通知は再送されない。** ストリーミングは pub/sub なので、
// 再接続しただけでは抜けた分が埋まらず、リロードするまで一覧に出てこない。
// `boot/main-boot.ts` が再接続時に揃えているのは未読バッジの件数だけで、
// 中身はここで取りに行く必要がある。
// realtimeMode でないときは `useInterval` が同じ役割を果たしているので不要。
//
// 切断していた間の取りこぼしを再接続時に拾う。起点をいつ読むかと間隔制御は
// `createReconnectResync` の仕事 (理由はあちらの doc)。
const reconnectResync = createReconnectResync({
	// 判断は `paginatorResyncOptions` 側 (SFC に置くとテストから触れない)。
	...paginatorResyncOptions(paginator, {
		// このコンポーネントは queue の件数を描画しないので、積んでも見えない。
		// 非 realtimeMode のポーリングが `toQueue: false` なのと揃える。
		toQueue: () => false,
		params: NOTIFICATION_RESYNC_PARAMS,
	}),
});

onMounted(() => {
	paginator.init();

	if (paginator.computedParams) {
		watch(paginator.computedParams, () => {
			paginator.reload();
		}, { immediate: false, deep: true });
	}

	if (store.s.realtimeMode) {
		connection = useStream().useChannel('main');
		connection.on('notification', onNotification);
		connection.on('notificationFlushed', reload);
		useStream().on('_connected_', reconnectResync.onConnected);
	}
});

onUnmounted(() => {
	// connection が非 null なら realtimeMode で購読済み = ストリームは初期化
	// されている。ここで `useStream()` を無条件に呼ぶと、realtimeMode を切って
	// いる利用者にまで WebSocket を張ってしまう。
	if (connection) {
		connection.dispose();
		useStream().off('_connected_', reconnectResync.onConnected);
	}
	reconnectResync.dispose();
	if (scrollContainer != null) {
		scrollContainer.removeEventListener('scroll', onScrollContainerScroll);
	}
});

defineExpose({
	reload,
});
</script>

<style lang="scss" module>
.transition_x_move {
	transition: transform 0.7s cubic-bezier(0.23, 1, 0.32, 1);
}

.transition_x_enterActive {
	transition: transform 0.7s cubic-bezier(0.23, 1, 0.32, 1), opacity 0.7s cubic-bezier(0.23, 1, 0.32, 1);

	&.content,
	.content {
		/* Skip Note Rendering有効時、TransitionGroupで通知を追加するときに一瞬がくっとなる問題を抑制する */
		content-visibility: visible !important;
	}
}

.transition_x_leaveActive {
	transition: height 0.2s cubic-bezier(0,.5,.5,1), opacity 0.2s cubic-bezier(0,.5,.5,1);
}

.transition_x_enterFrom {
	opacity: 0;
	transform: translateY(max(-64px, -100%));
}

@supports (interpolate-size: allow-keywords) {
	.transition_x_enterFrom {
		interpolate-size: allow-keywords; // heightのtransitionを動作させるために必要
		height: 0;
	}
}

.transition_x_leaveTo {
	opacity: 0;
}

.notifications {
	container-type: inline-size;
	background: var(--MI_THEME-panel);
}

.item {
	border-bottom: solid 0.5px var(--MI_THEME-divider);
}

.date {
	display: flex;
	font-size: 85%;
	align-items: center;
	justify-content: center;
	gap: 1em;
	padding: 8px 8px;
	margin: 0 auto;
	border-bottom: solid 0.5px var(--MI_THEME-divider);
}

.more {
	display: block;
	width: 100%;
	box-sizing: border-box;
	padding: 16px;
	background: var(--MI_THEME-panel);
	border-top: solid 0.5px var(--MI_THEME-divider);
}
</style>
