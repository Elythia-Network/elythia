<!--
SPDX-FileCopyrightText: Elythia-Network
SPDX-License-Identifier: AGPL-3.0-only
-->

<script lang="ts">
import { defineComponent, h, inject, onMounted, onUnmounted, provide, ref, useTemplateRef } from 'vue';
import { mountScheduler } from '@/utility/mount-scheduler.js';
import { FAR_ITEM_ATTR, FAR_ITEMS, ITEM_DISPOSE, ITEM_STATE } from '@/utility/far-items.js';

// 中身が入るまでの仮の高さ (px)。「もっと見る」がすぐに画面へ入って、次の読み足しを
// 始めない程度にあればよい
const PLACEHOLDER_HEIGHT = 160;

/**
 * Renders its slot, optionally a few frames later, and optionally only while it is near
 * the visible area.
 *
 * Without `defer` and `unmountWhenFar`, it renders its single slot child as is. Otherwise it
 * renders a wrapper (attributes fall through to it) that holds:
 * - with `defer`, a light placeholder until `mountScheduler` gives it a turn;
 * - with `unmountWhenFar`, an empty box of the last height while the list's `FarItems`
 *   reports it far, unless the user touched it, it has the focus, or it is playing media.
 *
 * The slot must render exactly one element or component. Without the wrapper only the
 * first vnode is rendered, so other children would be dropped.
 */
export default defineComponent({
	props: {
		/** Read once on mount. */
		defer: { type: Boolean, default: false },
		/** Read once on mount. Needs `FAR_ITEMS` provided by the list. */
		unmountWhenFar: { type: Boolean, default: false },
	},
	setup(props, { slots }) {
		if (!props.defer && !props.unmountWhenFar) {
			// 属性 (class や data-scroll-anchor) を中身に渡すため、slot を配列のまま返さず
			// 1 つの vnode にする。配列 (Fragment) を返すと、属性が中身へ渡らない
			return () => slots.default?.()[0];
		}

		const rootEl = useTemplateRef<HTMLElement>('rootEl');
		const ready = ref(!props.defer);
		if (!ready.value) {
			const cancel = mountScheduler.request(() => {
				ready.value = true;
			});
			onUnmounted(cancel);
		}

		// Elythia: 遠くなったら中身を外す (#3941)。外側の div は残すので、ここに置いた状態は
		// 中身を外しても残る
		const farHeight = ref<number | null>(null);
		if (props.unmountWhenFar) {
			provide(ITEM_STATE, new Map());
			const disposers: (() => void)[] = [];
			provide(ITEM_DISPOSE, (dispose) => disposers.push(dispose));
			onUnmounted(() => {
				for (const dispose of disposers) dispose();
			});
			const farItems = inject(FAR_ITEMS, null);
			let cancelRemount: (() => void) | null = null;
			let unobserve: (() => void) | null = null;
			onMounted(() => {
				if (farItems == null || rootEl.value == null) return;
				const el = rootEl.value;
				unobserve = farItems.observe(el, (far, height) => {
					if (far) {
						cancelRemount?.();
						cancelRemount = null;
						if (farHeight.value != null || !ready.value || height <= 0) return;
						if (farItems.isTouched(el) || el.contains(window.document.activeElement) || isPlayingMedia(el)) return;
						// 高さは交差の通知に入っている値を使い、レイアウトを読み直さない
						farHeight.value = height;
					} else if (farHeight.value != null && cancelRemount == null) {
						// 近付いたら mount し直す。上へ戻るときに一度に組み立てないよう、読み足しと
						// 同じく数件ずつにする
						cancelRemount = mountScheduler.request(() => {
							cancelRemount = null;
							farHeight.value = null;
						});
					}
				});
			});
			onUnmounted(() => {
				unobserve?.();
				cancelRemount?.();
			});
		}

		// 中身に替えるときに根の要素を替えない。TransitionGroup の子だと、根が替わるのを
		// 退場と入場として扱い、替えるたびに強制レイアウトと入場のアニメーションが走る
		// (試作の計測で、強制レイアウトだけで 2.8 秒増えた)
		return () => {
			const far = farHeight.value != null;
			return h('div', {
				ref: 'rootEl',
				[FAR_ITEM_ATTR]: props.unmountWhenFar ? '' : undefined,
				// 中身が入るまでの仮の高さは、インラインで付ける (#3950)。useCssModule は使わない
				// (eslint で禁じている)。この部品は描画関数の形のおかげで本番のビルドでも
				// CSS Modules の表が残っていたが、それは unwind のプラグインが処理を飛ばした
				// 偶然で、テンプレートを持つ部品では空を返す
				style: far ? { height: `${farHeight.value}px` } : ready.value ? undefined : { height: `${PLACEHOLDER_HEIGHT}px` },
			}, ready.value && !far ? slots.default?.() : undefined);
		};
	},
});

function isPlayingMedia(el: Element): boolean {
	for (const media of el.querySelectorAll('video, audio')) {
		if (!(media as HTMLMediaElement).paused) return true;
	}
	return false;
}
</script>
