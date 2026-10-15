/*
 * SPDX-FileCopyrightText: Elythia-Network
 * SPDX-License-Identifier: AGPL-3.0-only
 */

import { nextTick, onActivated, onDeactivated, onUnmounted, watch } from 'vue';
import { useDocumentVisibility } from '@@/js/use-document-visibility.js';

// 戻った後、位置の復元 (useScrollPositionKeeper の nextTick + 100ms) が終わってから
// もう一度先頭かどうかを見るまでの時間。
export const RECHECK_AFTER_ACTIVATED_MS = 200;

/**
 * Decides whether a streaming timeline should hold incoming items in its queue
 * instead of rendering them.
 *
 * Updates are paused while the document is hidden (upstream behaviour) and while
 * the component sits deactivated inside a `<KeepAlive>` (Elythia addition).
 */
export function useTimelineUpdatePause(opts: {
	/** Whether the scroll position is at the top of the timeline. Only meaningful while the DOM is attached. */
	isTop: () => boolean;
	/** Called when the document becomes visible again while the timeline is shown and at the top. */
	onVisibleAtTop: () => void;
	/** Called when the timeline is re-activated and it is (or was, when it was left) at the top. */
	onActivatedAtTop: () => void;
}): {
	isPausing: () => boolean;
	/**
	 * Call from the scroll container's `scroll` handler to remember whether the timeline is at the top.
	 * A list that never calls it is released on every re-activation.
	 */
	recordScrollPosition: () => void;
} {
	const visibility = useDocumentVisibility();
	let isDocumentHidden = false;
	let isDeactivated = false;

	// KeepAlive は onDeactivated より先に DOM を切り離すので、そこで isTop() を
	// 読んでも正しい位置が取れない (切り離した要素の scrollTop は 0 になる)。
	// 戻ったときも、位置の復元 (useScrollPositionKeeper) は onActivated の後に
	// 非同期で走る。なので、表示中の最後の scroll で見た位置を覚えておいて使う。
	let wasAtTop = true;

	watch(visibility, () => {
		if (visibility.value === 'hidden') {
			isDocumentHidden = true;
		} else { // 'visible'
			isDocumentHidden = false;
			// 裏に回っている間は DOM が無く isTop() が常に真になるので、ここでは出さない。
			// 戻ったときの onActivated に任せる。
			if (!isDeactivated && opts.isTop()) {
				opts.onVisibleAtTop();
			}
		}
	});

	let recheckTimer: number | null = null;

	function cancelRecheck() {
		if (recheckTimer != null) {
			window.clearTimeout(recheckTimer);
			recheckTimer = null;
		}
	}

	onDeactivated(() => {
		isDeactivated = true;
		cancelRecheck();
	});

	onActivated(() => {
		// 初回の mount でも onActivated は呼ばれる (KeepAlive の中にいるとき)。
		// そのときは queue が空なので、呼んでも何も起きない。
		isDeactivated = false;
		cancelRecheck();
		if (isDocumentHidden) return; // 見えるようになったときの watch に任せる
		if (wasAtTop) {
			opts.onActivatedAtTop();
			return;
		}
		// 離れる前は先頭でなかったと記録されていても、実際には先頭に戻っていることがある
		// (scrollTop が小さくアンカーが取られず、復元が 0 のままになるなど)。そのまま
		// にすると queue が残り続けるので、位置の復元が終わった後に実際の位置で見直す。
		nextTick(() => {
			if (isDeactivated) return;
			recheckTimer = window.setTimeout(() => {
				recheckTimer = null;
				if (isDeactivated || isDocumentHidden) return;
				if (opts.isTop()) opts.onActivatedAtTop();
			}, RECHECK_AFTER_ACTIVATED_MS);
		});
	});

	onUnmounted(cancelRecheck);

	return {
		isPausing: () => isDocumentHidden || isDeactivated,
		recordScrollPosition: () => {
			if (isDeactivated) return;
			wasAtTop = opts.isTop();
		},
	};
}
