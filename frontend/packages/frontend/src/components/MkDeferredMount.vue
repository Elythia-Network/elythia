<!--
SPDX-FileCopyrightText: Elythia-Network
SPDX-License-Identifier: AGPL-3.0-only
-->

<script lang="ts">
import { defineComponent, h, onUnmounted, ref, useCssModule } from 'vue';
import { mountScheduler } from '@/utility/mount-scheduler.js';

/**
 * Renders its slot, optionally a few frames later.
 *
 * Without `defer`, it renders its single slot child as is. With `defer`, it renders a
 * wrapper that holds a light placeholder until `mountScheduler` gives it a turn, and then
 * the slot. Attributes fall through to the slot child or to the wrapper.
 *
 * The slot must render exactly one element or component. Without `defer` only the first
 * vnode is rendered, so other children would be dropped.
 */
export default defineComponent({
	props: {
		/** Read once on mount. */
		defer: { type: Boolean, default: false },
	},
	setup(props, { slots }) {
		const $style = useCssModule();

		if (!props.defer) {
			// 属性 (class や data-scroll-anchor) を中身に渡すため、slot を配列のまま返さず
			// 1 つの vnode にする。配列 (Fragment) を返すと、属性が中身へ渡らない
			return () => slots.default?.()[0];
		}

		const ready = ref(false);
		const cancel = mountScheduler.request(() => {
			ready.value = true;
		});
		onUnmounted(cancel);

		// 中身に替えるときに根の要素を替えない。TransitionGroup の子だと、根が替わるのを
		// 退場と入場として扱い、替えるたびに強制レイアウトと入場のアニメーションが走る
		// (試作の計測で、強制レイアウトだけで 2.8 秒増えた)
		return () => h('div', { class: ready.value ? undefined : $style.placeholder }, ready.value ? slots.default?.() : undefined);
	},
});
</script>

<style lang="scss" module>
.placeholder {
	// 中身が入るまでの仮の高さ。「もっと見る」がすぐに画面へ入って、次の読み足しを
	// 始めない程度にあればよい
	height: 160px;
}
</style>
