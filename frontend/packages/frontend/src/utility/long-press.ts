/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

export type LongPressOptions = {
	/** How long the finger must stay down, in ms. */
	delay?: number;
	/** How far the finger may drift before the press is cancelled, in px. */
	moveTolerance?: number;
};

export type LongPressHandler = (ev: TouchEvent) => void;

// 2 つの用途で使う窓。値を変えるときは両方を見ること。
//
// 1. **指を離してから、この時間内に来た click を握り潰す。起点は touchend。**
//    発火時刻を起点にすると、メニューを読んでから指を離す (= この機能の一番普通の
//    使い方) だけで窓が切れ、リアクションが付け外しされる (#2932 のレビュー 2 周目で
//    実測)。実機の click は touchend の数十 ms 後に来るので 1 秒あれば足りる。
// 2. **touchend の後に来た `contextmenu` をタッチ由来と見なす猶予。** 指が触れて
//    いる間は `touching` で判定できるが、離した直後に来る実装もあるため。
//
// **2 の副作用**: タッチの 1 秒以内に来たマウスの右クリックはタッチ由来と誤判定され、
// こちらが撃った分として握り潰される。ハイブリッド端末でしか踏まないので許容する。
const SUPPRESS_WINDOW = 1000;

/**
 * Calls `onLongPress` when the element is touched and held, and swallows the
 * `click` / `contextmenu` that the browser fires afterwards.
 *
 * Returns a function that removes every listener it added.
 *
 * mk-go 独自 (#2932)。**iOS Safari は `<button>` の長押しで `contextmenu` を
 * 発火しない**ので、`@contextmenu` にだけメニューを配線している要素は iOS から
 * 到達できない。タップが別の動作 (リアクションの付け外し) に取られている場合、
 * 代わりの入口が無くなる。
 *
 * **後続の `click` を握り潰すのが要点。** 潰さないと、メニューを開いた指を離した
 * 瞬間にタップとして扱われ、リアクションが付け外しされる。**`capture: true` が
 * 要る** — Vue は `@click` をこの要素自身に張るので、bubble 段階で受けると登録順
 * (Vue の patch が先、`onMounted` のこちらが後) の分だけ相手が先に走ってしまう。
 * capture パスは bubble パスより前に回るので、登録順に関わらず先に取れる。
 *
 * **握り潰すかどうかは「この touch 操作で長押しが成立したか」で持つ** (`armed`)。
 * 真偽値だけだと、メニュー側をタップしてこの要素の `click` が来なかったときに
 * 立ちっぱなしになり、次のマウスクリックやキーボード操作を 1 回食う。かといって
 * 発火時刻からの期限にすると、長く押していただけで抜ける。**touchend で期限へ
 * 変換する**ことで両方を満たす。
 *
 * **`contextmenu` は 2 方向に効かせる。** Android Chrome は長押しで `contextmenu`
 * も発火するので、(a) こちらが先に撃っていたら握り潰し、(b) ブラウザが先に撃った
 * ならこちらのタイマーを取り消す。どちらか一方だけがメニューを開く。**どちらの
 * 経路でも後続の `click` は握り潰す** — ブラウザ側が tap を抑止しない実装に当たると、
 * メニューの裏でリアクションが動く。
 *
 * **タッチ由来かどうかは主に「指が触れているか」で見る** (`touching`)。時刻の差だけで
 * 判定すると、`delay` を長くしたときに Android 経路が黙って無効になる。touchend の
 * 直後に来る分だけ時刻の窓で拾う。
 *
 * **接触点は `targetTouches` で数える。** `touches` は画面全体の接触点なので、別の指が
 * どこかに触れているだけで長押しが黙って死ぬ。
 */
export function bindLongPress(el: HTMLElement, onLongPress: LongPressHandler, options: LongPressOptions = {}): () => void {
	// 500ms は Android Chrome の長押し判定とほぼ同じ。短くするとスクロール開始と
	// 区別が付かず、長くすると Chrome 側の contextmenu に必ず先を越される。
	const delay = options.delay ?? 500;
	// 指は必ず少し動く。8px は「押したまま」と読める範囲で、upstream に対応する
	// 定数は無い (MkSwiper が持つのは 20 / 70 / 120 でスワイプ用)。
	const moveTolerance = options.moveTolerance ?? 8;

	let timer: number | null = null;
	let startX = 0;
	let startY = 0;
	// 指が触れている間 true。contextmenu がタッチ由来かの判定に使う。
	let touching = false;
	// 直近に触られた時刻。touchend の直後に来る contextmenu を拾うための保険。
	let lastTouchAt = 0;
	// 自前のタイマーが撃ったか。ブラウザ側の contextmenu と二重に開かないために使う。
	let pressFired = false;
	// この touch 操作に続く click を握り潰すか。
	let armed = false;
	// touchend で入る期限。0 は「まだ指が離れていない」。
	let suppressClickUntil = 0;

	function cancel() {
		if (timer != null) {
			window.clearTimeout(timer);
			timer = null;
		}
	}

	function onTouchStart(ev: TouchEvent) {
		cancel();
		// **「新しい操作の始まり」で状態を落とす。** 追加の指では落とさない —
		// 長押しが成立した後に 2 本目が触れただけで握り潰しが解けると、離した
		// 瞬間にリアクションが動く。
		//
		// 判定は 2 つの or にしてある。`!touching` だけだと 2 本指で始めた操作に
		// 前回の状態が残り、普通のタップが 1 回食われる。`targetTouches === 1`
		// だけだと、`touchend` が来ない経路で `touching` が固着したときに
		// 状態が二度と落ちない。
		if (!touching || ev.targetTouches.length === 1) {
			pressFired = false;
			armed = false;
			suppressClickUntil = 0;
		}
		touching = true;
		lastTouchAt = Date.now();
		// 2 本指はピンチ / スクロールなので長押しにしない。**`targetTouches` で
		// 見る** — `touches` は画面全体の接触点なので、別の指が画面のどこかに
		// 触れているだけで長押しが黙って死ぬ。
		if (ev.targetTouches.length !== 1) return;
		startX = ev.targetTouches[0].clientX;
		startY = ev.targetTouches[0].clientY;
		timer = window.setTimeout(() => {
			timer = null;
			pressFired = true;
			armed = true;
			onLongPress(ev);
		}, delay);
	}

	function onTouchMove(ev: TouchEvent) {
		lastTouchAt = Date.now();
		if (timer == null) return;
		const touch = ev.targetTouches[0];
		if (touch == null) return;
		// スクロール中の誤発火を止める。
		if (Math.hypot(touch.clientX - startX, touch.clientY - startY) > moveTolerance) cancel();
	}

	function onTouchEnd(ev: TouchEvent) {
		cancel();
		// **この要素に残っている指で見る。** 無条件に false にすると、2 本指の
		// 片方を離しただけで「タッチ由来ではない」と判定するようになり、その後の
		// contextmenu 経路で後続の click を握り潰しそこねる。
		touching = ev.targetTouches.length > 0;
		lastTouchAt = Date.now();
		// 指を離した時点から数え始める。まだ触れているなら操作は続いている。
		if (!touching && armed) suppressClickUntil = lastTouchAt + SUPPRESS_WINDOW;
	}

	function onClick(ev: MouseEvent) {
		if (!armed) return;
		// suppressClickUntil が 0 = まだ指が離れていない (click が touchend より
		// 先に来る実装への保険)。期限切れなら握り潰さないが、armed は落とす。
		const expired = suppressClickUntil !== 0 && Date.now() >= suppressClickUntil;
		armed = false;
		suppressClickUntil = 0;
		if (expired) return;
		ev.preventDefault();
		ev.stopImmediatePropagation();
	}

	function onContextmenu(ev: MouseEvent) {
		// touchend の直後に来る分も拾う。マウスの右クリックは通す。
		const fromTouch = touching || Date.now() - lastTouchAt < SUPPRESS_WINDOW;
		if (pressFired && fromTouch) {
			// こちらが先に撃った。ブラウザ側の分は捨てる。
			pressFired = false;
			ev.preventDefault();
			ev.stopImmediatePropagation();
			return;
		}
		if (!fromTouch) return;
		// ブラウザが先に撃った。こちらは撃たないが、後続の click は同じ理由で潰す。
		cancel();
		armed = true;
		// **contextmenu は touchend より後にも来る。** そのときは touchend が
		// 期限を入れる機会を過ぎているので、ここで入れる。入れないと armed が
		// 期限無しで立ちっぱなしになり、次の click を 1 回食う。
		if (!touching) suppressClickUntil = Date.now() + SUPPRESS_WINDOW;
	}

	el.addEventListener('touchstart', onTouchStart, { passive: true });
	el.addEventListener('touchmove', onTouchMove, { passive: true });
	el.addEventListener('touchend', onTouchEnd, { passive: true });
	el.addEventListener('touchcancel', onTouchEnd, { passive: true });
	el.addEventListener('click', onClick, { capture: true });
	el.addEventListener('contextmenu', onContextmenu, { capture: true });

	return () => {
		cancel();
		el.removeEventListener('touchstart', onTouchStart);
		el.removeEventListener('touchmove', onTouchMove);
		el.removeEventListener('touchend', onTouchEnd);
		el.removeEventListener('touchcancel', onTouchEnd);
		el.removeEventListener('click', onClick, { capture: true });
		el.removeEventListener('contextmenu', onContextmenu, { capture: true });
	};
}
