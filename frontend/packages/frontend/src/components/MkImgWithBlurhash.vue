<!--
SPDX-FileCopyrightText: syuilo and misskey-project
SPDX-License-Identifier: AGPL-3.0-only
-->

<template>
<div :class="['chromatic-ignore', $style.root, { [$style.cover]: cover }]" :title="title ?? ''">
	<!--
		Elythia: 切り替えに TransitionGroup を使わない (#3935)。
		TransitionGroup は画像のデコードのたびに canvas の退場・img の出現・canvas の
		移動の遷移を走らせ、そのたびに getComputedStyle でページ全体のスタイル
		再計算を強制していた (画像 1 枚につき数回)。アバターもこの部品なので、タイムラインのノート 1 件ごとに
		起きる。クラス名に undefined を渡しても既定の v- のクラスで同じ処理をするので、
		「アニメーションを減らす」でも止まらなかった。
		遷移は、呼び出し側が transition を渡したとき (ギャラリーのセンシティブの切り替え)
		だけ canvas と img に掛ける。それ以外は css を切った Transition にして、Vue に
		getComputedStyle を読ませない。
	-->
	<Transition v-bind="transitionProps" :leaveActiveClass="leaveActiveClass($style.transition_leaveActive)">
		<MkBlurhash
			:class="$style.canvas"
			:blurhash="hash ?? null"
			:height="props.height"
			:width="props.width"
			:onlyAvgColor="props.onlyAvgColor"
			:show="hide"
		/>
	</Transition>
	<!--
		Elythia: 隠している間も display: none にしない (#3935)。loading="lazy" の画像は、
		箱が無いと画面に近付いても読み込まれない。隠している間は canvas の上に重ねて
		見えなくしておき、大きさは canvas が決める (以前と同じ並び)。
		遷移を掛けるとき (ギャラリー) だけは、以前と同じく v-show で消し、退場の遷移を
		見せる。display: none の間は lazy だと読み込まれないので、そのときは eager にする。
	-->
	<Transition v-bind="transitionProps" :leaveActiveClass="leaveActiveClass($style.transition_leaveActive)">
		<img
			v-show="!cssTransition || !hide"
			ref="img"
			:height="imgHeight"
			:width="imgWidth"
			:class="[$style.img, { [$style.imgHidden]: hide && !cssTransition }]"
			:src="src ?? undefined"
			:title="title ?? undefined"
			:alt="alt ?? undefined"
			:data-marker="marker ?? undefined"
			:loading="cssTransition ? 'eager' : 'lazy'"
			decoding="async"
			draggable="false"
			tabindex="-1"
			style="-webkit-user-drag: none;"
			@load="onImgLoad"
		/>
	</Transition>
</div>
</template>

<script lang="ts" setup>
import { computed, useTemplateRef, watch, ref } from 'vue';
import { prefer } from '@/preferences.js';
import MkBlurhash from '@/components/MkBlurhash.vue';

const props = withDefaults(defineProps<{
	transition?: {
		duration?: number | { enter: number; leave: number; };
		enterActiveClass?: string;
		leaveActiveClass?: string;
		enterFromClass?: string;
		leaveToClass?: string;
		enterToClass?: string;
		leaveFromClass?: string;
	} | null;
	src?: string | null;
	hash?: string | null;
	alt?: string | null;
	title?: string | null;
	height?: number;
	width?: number;
	cover?: boolean;
	forceBlurhash?: boolean;
	onlyAvgColor?: boolean; // 軽量化のためにBlurhashを使わずに平均色だけを描画
	marker?: string;
}>(), {
	transition: null,
	src: null,
	alt: '',
	title: null,
	height: 64,
	width: 64,
	cover: true,
	forceBlurhash: false,
	onlyAvgColor: false,
});

const img = useTemplateRef('img');
const loaded = ref(false);
const hide = computed(() => !loaded.value || props.forceBlurhash);
const cssTransition = computed(() => prefer.s.animation && props.transition != null);

// Elythia: $style は script で読まない (useCssModule を使わない)。本番のビルドでは
// rollup-plugin-unwind-css-module-class-name が、描画関数に `_ctx.$style` の参照が
// 1 つも残らない部品から CSS Modules の表を外す。script の最上位で $style を宣言すると
// テンプレートの $style もそれを指すので、必ずこの条件に当たり、useCssModule() が空を
// 返してクラスが全部 undefined になる (#3950。アバターの画像が枠より大きく描かれ、
// 拡大されて見えた)。開発時と vitest ではこの処理が走らないので気付けない。退場の
// クラスは、既定のクラスと合わせてテンプレートから渡す (transitionProps には入れない)
function leaveActiveClass(defaultClass: string): string | undefined {
	if (!cssTransition.value) return undefined;
	return props.transition?.leaveActiveClass ?? defaultClass;
}

const transitionProps = computed(() => {
	if (!cssTransition.value) return { css: false };
	const t = props.transition;
	return {
		css: true,
		duration: t?.duration,
		enterActiveClass: t?.enterActiveClass,
		enterFromClass: t?.enterFromClass,
		leaveToClass: t?.leaveToClass,
		enterToClass: t?.enterToClass,
		leaveFromClass: t?.leaveFromClass,
	};
});

// Elythia: 縦横比は受け取った幅と高さから決める (#3935)。以前は mount のたびに
// clientWidth を読んでいて、読み込みの途中で DOM が汚れているとレイアウトを強制した。
// img は CSS で幅も高さも 100% にしているので、属性は縦横比を伝えるだけで、
// 値が実寸でも表示の幅でも見た目は変わらない。
const validSize = computed(() => Number.isFinite(props.width) && Number.isFinite(props.height) && props.width > 0 && props.height > 0);
const imgWidth = computed(() => validSize.value ? props.width : undefined);
const imgHeight = computed(() => validSize.value ? props.height : undefined);

// Elythia: デコードは load の後に行う (#3935)。loading="lazy" の画像は画面に近付く
// まで読み込まれないので、src が変わった直後に decode() を待つと、遅延の扱いが
// ブラウザで異なる。load が来てから decode() を待てば、どのブラウザでも同じになる。
function onImgLoad() {
	const el = img.value;
	if (el == null) return;
	const src = props.src;
	el.decode().then(() => {
		// 待っている間に src が変わっていたら、古い画像の完了で表示を切り替えない
		if (props.src === src) loaded.value = true;
	}, error => {
		// Elythia: 要素そのものは渡さない。DevTools を開いているとコンソールが要素を
		// 保持し続け、外れた DOM が残る (#3935 の計測で見つけた)。
		console.log('Error occurred during decoding image', src, error);
	});
}

watch(() => props.src, (src) => {
	if (src == null || src === '') loaded.value = false;
});
</script>

<style lang="scss" module>
.transition_leaveActive {
	position: absolute;
	top: 0;
	left: 0;
}
.root {
	position: relative;
	width: 100%;
	height: 100%;

	&.cover {
		> .canvas,
		> .img {
			object-fit: cover;
		}
	}
}

.canvas,
.img {
	display: block;
	width: 100%;
	height: 100%;
}

.canvas {
	object-fit: contain;
}

.img {
	object-fit: contain;
}

.imgHidden {
	position: absolute;
	top: 0;
	left: 0;
	visibility: hidden;
}
</style>
