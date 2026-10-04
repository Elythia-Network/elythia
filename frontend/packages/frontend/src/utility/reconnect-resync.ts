/*
 * SPDX-FileCopyrightText: mk-go project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

/**
 * Recovers the items that a pub/sub stream dropped while it was disconnected.
 *
 * ストリーミングで届くイベントは pub/sub なので、切断していた間に発行された分は
 * 再送されない。realtimeMode の一覧 (通知 / タイムライン) はイベントだけで中身を
 * 更新するため、再接続したら自分で差分を取りに行かないと、リロードするまで
 * 抜けたままになる。非 realtimeMode は `useInterval` の `fetchNewer` が同じ役割を
 * 果たしているので、この穴は realtimeMode にだけある。
 *
 * **起点は `_connected_` を受けた瞬間に読む。** `Stream.onOpen` はイベントを
 * emit した**後**にチャンネルを張り直すので、この時点ではまだ 1 件も届いて
 * いない = 手持ちの最新がそのまま「切断前の最新」になる。ジッターを待ってから
 * 読むと、その間に流れ込んだぶんだけ起点が進み、埋めたい穴を飛び越える。
 *
 * **`_disconnected_` は使えない。** `Stream.reconnect()` 経由の張り直し
 * (`stream.ts` がバックグラウンド 20 秒超の復帰で呼ぶ = モバイルでいちばん多い
 * 経路) は `intentionalReconnect` で抑止されるので emit されない。
 * `boot/main-boot.ts` が同じ理由でこのイベントを判定に使っていない。
 *
 * 呼び出し制御は `boot/main-boot.ts` の未読バッジ再同期と同じ理屈:
 *
 *  - **mount 直後の接続は数えない。** 生成から `initialDelay` の間に来た
 *    `_connected_` は起点も控えない (そこは `paginator.init()` が取っている)。
 *    **間隔 (30 秒) とは分けてある** — 以前は生成時刻を「前回」に数えて 30 秒
 *    抑止しており、iOS の PWA で「裏で自動リロード → 20 秒後に復帰」の
 *    張り直しが必ず見送られた (#3195)
 *  - **間隔を空ける。** 再接続は連続しうるので、そのたびに API を叩かない。
 *    **見送った回は捨てずに、間隔が明けた時点で走らせ直す** (#3195)。
 *    以前は次の `_connected_` まで誰も拾わず、タイムラインが古いまま残った
 *  - **ジッターを入れる。** サーバーの再起動で全クライアントが同時に再接続する
 *    ので散らす。**ただし表示に戻った直後は入れない** — 復帰による張り直しは
 *    利用者ごとにばらけるので散らす意味が無く、待つ間 (最大 10 秒) 古い一覧を
 *    見せるだけになる。実機では待っている間に裏へ戻され、取得が裏で止まった
 *  - **裏にいる間は投げない。** iOS は裏のページの通信を止めるので、投げた取得が
 *    戻るまで宙に浮く。控えは残し、表示に戻ったら走らせる (購読はこの関数が
 *    持つ。呼び出し側ごとに配線すると片側が漏れる — ノート側に口が無かった)
 *  - **多重実行しない。** 待っている間に次の再接続が来ても 1 本に畳む。
 *    飛行中に控えた分は、終わった時点で走らせ直す
 *
 * **取りに行った後の `reload()` とは競合しうる。** 投げる直前まで世代を見るので
 * 「reload が先」は避けられるが、こちらのリクエストが飛んでいる間に reload が
 * 始まると、取り直した最新ページが穴埋め分の下に入る。`:key` の重複は
 * `pushItems` 側で落とすが、並びまでは揃わない。
 *
 * **埋められるのは 1 ページぶんまで。** `fetchNewer` は 30 件までしか取らず、
 * 呼び出し側は `canFetchDetection` を設定していないので `canFetchNewer` も
 * 立たない。それを超える断では穴の古い側だけが埋まり、残りはリロードするまで
 * 出てこない。ページングして埋め切るか、埋め切れなかったことを UI に出すかは
 * 別途の判断。
 *
 * **「いま決められない」と「本当に空」は分ける。** `init()` / `reload()` の最中も
 * 一覧は空だが、そこで控えると「空だった」という起点が入れ替えを跨いで生き残る
 * (世代は `init()` の冒頭で進むので、入れ替え中に控えた値は完了後の世代と一致
 * してしまい弾けない)。前者は控えず (`undefined`)、後者は `null` を控えて
 * 呼び出し側に委ねる。**空の一覧にも穴は空く** — バッジだけ増えて中身が出ない
 * 状態になるので、埋めない判断はしない。
 *
 * **起点は接続した瞬間に控え、走るまで持ち越す。** 見送った回 (裏タブ、init /
 * reload 中、レート制限中) のあとに呼び直すとき、その場で読み直すと、見送って
 * いる間に届いたぶんだけ起点が進んで穴を飛び越える。控えを捨てるのは「実際に
 * 取りに行った」ときと「一覧が総入れ替えされた」とき (`getGeneration`) だけ。
 * 世代を見ないと、持ち越した起点が `reload()` 後に古びて、過去のページが最新の
 * 上に積まれる。
 *
 * **裏をまたいだ取得は見捨てて投げ直す** (#3195)。表示に戻った時点で飛行中なら、
 * それは裏へ行く前に投げたもの (裏では投げない) で、iOS に止められて決着しない
 * ことがある。見捨てた回が後から返ってきても重複は id で落ちる。代わりに、
 * 裏と表を往復するたびに決着しない取得が 1 本ずつ残りうる (利用者の操作でしか
 * 増えない)。**一覧が空のときの `reload()` は救えない** — 冒頭で世代を進めて
 * 取得中になるので、戻した控えは世代違いで止まり、復帰時の接続も起点を控えない。
 * 空の一覧に限った穴として残っている。
 *
 * **レート制限に当たったら通常の取得と同じ扱いになる。** `Paginator` は 429 を
 * 受けると追い読みを止めて通知を出すので、背景で撃ったぶんでもそれが起きる。
 * 背景だからと握り潰すと、実際に制限されている事実が利用者から見えなくなる
 * ほうが害が大きいので、そのままにしてある (既に制限中の回は `canResync` で
 * 見送るので、立て続けに撃つことはない)。
 */
export type ReconnectResyncOptions = {
	/**
	 * 取りこぼしの起点。`_connected_` を受けた瞬間に呼ばれる。
	 *
	 *  - 文字列: その id より新しいものを取りに行く
	 *  - `null`: 一覧が空 = 起点が無い。控えたうえで `resync(null)` を呼ぶので、
	 *    呼び出し側が「最初から取る」形に倒す
	 *  - `undefined`: いま起点を決められない (`init()` / `reload()` の最中)。
	 *    控えないので、この接続ぶんの穴はリロードするまで残る
	 */
	getNewestId: () => string | null | undefined;

	/** 起点より新しいものを取りに行く。null は「起点なし」。 */
	resync: (sinceId: string | null) => unknown;

	/**
	 * 一覧の世代。`Paginator.generation` をそのまま渡す。控えた起点は、この値が
	 * 変わった時点で無効になる (`init()` / `reload()` で中身が総入れ替えされた)。
	 */
	getGeneration?: () => number;

	/**
	 * 走ってよいかの判定。**ジッターの前後で 2 回呼ばれる** — 待っている間に
	 * 条件が変わる (タブが裏に戻る、`reload()` が始まる) ため。
	 *
	 * 前段で false を返したときは間隔を消費しない。控えは残るので、表示に
	 * 戻ったとき (本体が自分で拾う) か次の `_connected_` で走る。
	 */
	canResync?: () => boolean;

	/** 再同期の最短間隔 (ms)。 */
	interval?: number;
	/** 表示に戻った直後の最短間隔 (ms)。 */
	resumeInterval?: number;
	/** 生成からこの間に来た接続は mount 時のものとして数えない (ms)。 */
	initialDelay?: number;
	/** 実行前に待つ最大時間 (ms)。0 なら待たない。 */
	jitter?: number;
	/** テスト用の時計。 */
	now?: () => number;
	/** テスト用の乱数。 */
	random?: () => number;
	/** テスト用の待ち。 */
	sleep?: (ms: number) => Promise<void>;
	/** テスト用の timer。戻り値は取り消し。 */
	schedule?: (fn: () => void, ms: number) => () => void;
	/** テスト用。表示中か。 */
	isVisible?: () => boolean;
	/** テスト用。表示に戻ったら呼ぶ。戻り値は購読の解除。 */
	onVisible?: (fn: () => void) => () => void;
	/** テスト用。表示に戻った直後か (ジッターを入れない)。 */
	resumedRecently?: () => boolean;
};

export type ReconnectResync = {
	/** `_connected_` に渡すハンドラ。起点を控えたうえで実行を試みる。 */
	onConnected: () => Promise<void>;
	/**
	 * 控えてある起点があるときだけ実行を試みる。見送る条件が解けたタイミングで
	 * 呼ぶ。**表示に戻ったときは本体が自分で呼ぶ**ので、呼び出し側が配線する
	 * 必要は無い。**控えが無ければ何もしない** — 切断が無かったのに取りに行くと、
	 * 通知一覧では取得そのものが既読化を伴うので害になる。
	 */
	retry: () => Promise<void>;
	/**
	 * 以降の再同期を止める。unmount 時に呼ぶ。
	 *
	 * **既に `resync` に入ったものは止まらない。** 止められるのはまだ
	 * ジッターを待っている分まで。
	 */
	dispose: () => void;
};

const DEFAULT_INTERVAL = 30 * 1000;
const DEFAULT_INITIAL_DELAY = 5 * 1000;
const DEFAULT_JITTER = 10 * 1000;
// 表示に戻った直後の最短間隔。
const DEFAULT_RESUME_INTERVAL = 2 * 1000;

// 表示に戻ってからこの間の接続は「復帰による張り直し」とみなす。ジッターの
// 上限 (10 秒) と揃えてある — 実機では復帰から `_connected_` まで 0.3 秒程度だが、
// 回線が悪いと張り直しに数秒かかる。
const RESUME_WINDOW = 10 * 1000;

// 表示に戻った時刻。モジュールで 1 つ持つ (一覧ごとに購読する必要が無い)。
let lastBecameVisibleAt = Number.NEGATIVE_INFINITY;
if (typeof window !== 'undefined') {
	window.document.addEventListener('visibilitychange', () => {
		if (window.document.visibilityState === 'visible') lastBecameVisibleAt = Date.now();
	});
}

const defaultIsVisible = () => window.document.visibilityState !== 'hidden';

const defaultOnVisible = (fn: () => void) => {
	const listener = () => {
		if (window.document.visibilityState === 'visible') fn();
	};
	window.document.addEventListener('visibilitychange', listener);
	return () => window.document.removeEventListener('visibilitychange', listener);
};

const defaultSchedule = (fn: () => void, ms: number) => {
	const id = window.setTimeout(fn, ms);
	return () => window.clearTimeout(id);
};

export function createReconnectResync(options: ReconnectResyncOptions): ReconnectResync {
	const interval = options.interval ?? DEFAULT_INTERVAL;
	const jitter = options.jitter ?? DEFAULT_JITTER;
	const now = options.now ?? (() => Date.now());
	const random = options.random ?? Math.random;
	const sleep = options.sleep ?? ((ms: number) => new Promise<void>(resolve => { window.setTimeout(resolve, ms); }));
	const canResync = options.canResync ?? (() => true);
	const generation = options.getGeneration ?? (() => 0);
	const schedule = options.schedule ?? defaultSchedule;
	const isVisible = options.isVisible ?? defaultIsVisible;
	const resumedRecently = options.resumedRecently ?? (() => Date.now() - lastBecameVisibleAt < RESUME_WINDOW);
	const resumeInterval = options.resumeInterval ?? DEFAULT_RESUME_INTERVAL;
	const currentInterval = () => (resumedRecently() ? Math.min(interval, resumeInterval) : interval);

	// mount 直後の `_connected_` を数えない期限。**間隔とは別に持つ** (理由は冒頭)。
	const ignoreUntil = now() + (options.initialDelay ?? DEFAULT_INITIAL_DELAY);
	let lastAt = Number.NEGATIVE_INFINITY;
	let pending: { since: string | null; generation: number } | null = null;
	// 飛行中の 1 本。**見捨てることがある** (表示に戻ったときの処理を参照) ので、
	// 真偽値ではなく自分自身と比べられる形で持つ。
	let flight: { since: string | null; generation: number; lastAtBefore: number } | null = null;
	let disposed = false;
	let cancelScheduled: (() => void) | null = null;

	// 間隔が明ける時刻に 1 本だけ張る。**既にあっても、今回のほうが早く明けるなら
	// 張り直す** — 明ける時刻は `lastAt` だけでなく「表示に戻った直後か」でも動く
	// (`currentInterval()`)。窓の外で張った 30 秒の timer を残すと、復帰直後に
	// 2 秒で走るはずの回がそれに吸われて待たされる (3 周目のレビューで実測)。
	// 遅いほうへは張り直さない (窓が切れた後の判断は発火時の `run()` がやり直す)。
	// 張ってある timer が明ける時刻。timer があるときだけ意味を持つ。
	let scheduledAt = Number.POSITIVE_INFINITY;
	const scheduleRun = () => {
		const due = lastAt + currentInterval();
		if (cancelScheduled != null) {
			if (scheduledAt <= due) return;
			cancelScheduled();
		}
		scheduledAt = due;
		cancelScheduled = schedule(() => {
			cancelScheduled = null;
			void run();
		}, Math.max(0, due - now()));
	};

	const run = async (): Promise<void> => {
		if (disposed) return;
		if (flight != null) return;
		if (pending == null) return;
		if (!canResync()) return;
		// 裏にいる間は投げない。控えは残し、表示に戻ったら `onVisible` から来る。
		if (!isVisible()) return;
		// **表示に戻った直後は間隔を縮める。** 30 秒のままだと、復帰直後の取得の
		// 飛行中に張り直しが来た回が 30 秒待たされる。**無くしはしない** — 復帰の
		// 直後に接続が暴れると (misskey-js は間髪入れず張り直す)、そのたびに撃つ。
		if (now() - lastAt < currentInterval()) {
			scheduleRun();
			return;
		}
		// 控えてから一覧が総入れ替えされていたら、その起点はもう意味を持たない。
		// **捨てるのは `onConnected` の仕事。** あちらが「古ければ捨てて控え直す」
		// を一手に持つので、ここで捨てると二重管理になる (捨て漏れると、以後
		// 「控えはあるが世代が合わない」で回り続けて二度と走らなくなる)。
		// **その代わり `retry()` は次の `_connected_` まで不活性になる** — 控えは
		// 残っているが世代が合わないので、ここで毎回 return する。
		if (pending.generation !== generation()) return;
		const { since, generation: gen } = pending;
		const me = { since, generation: gen, lastAtBefore: lastAt };
		flight = me;
		try {
			if (jitter > 0 && !resumedRecently()) await sleep(random() * jitter);
			// 待っている間に見捨てられた。後を継いだ回が走っている。
			if (flight !== me) return;
			// 待っている間に unmount された / 条件が変わった / 総入れ替えされた。
			if (disposed) return;
			// **控えは消さずに戻る。** 条件が解けてから `retry()` で拾い直せる。
			// 裏へ戻されたときも同じ (待っている間に戻されるのは実機で起きた)。
			if (!canResync() || !isVisible()) return;
			// **控え直された新しい世代ではなく、自分が持ち出した `gen` と比べる。**
			// ジッターを待つ間に `onConnected` が走って控え直していることがあり、
			// `pending` を見ると「一致」してしまう (= 古い起点のまま投げる)。
			if (gen !== generation()) return;
			// **投げると決まってから控えを捨てる。** ここより前で捨てると、
			// 見送った回の穴が失われる。
			pending = null;
			// **投げる直前に更新する。** ここまでの見送りは「試行しなかった」ので
			// 間隔を消費させない (条件が解けた瞬間に呼び直せば待たされない)。
			// 失敗しても間隔は空けたいので、成功後ではなく直前に置く。
			lastAt = now();
			await options.resync(since);
		} catch {
			// `fetchNewer` は reject しないので、ここへ来るのは想定外の例外だけ。
			// unhandled rejection にしないために握る。間隔は据え置き (= 失敗しても
			// 連打しない)。
		} finally {
			// 見捨てられた回は、後を継いだ回の飛行中の印を消さない。
			if (flight === me) flight = null;
		}
		// 飛行中に来た接続の控えは、飛行中で弾かれたまま残っている。
		// ここで拾わないと次の `_connected_` まで誰も走らせない (後を継いだ回が
		// まだ飛んでいれば入口で弾かれ、その回の終わりに拾われる)。
		if (pending != null && !disposed) void run();
	};

	const onConnected = async (): Promise<void> => {
		if (disposed) return;
		// mount 直後の接続は `paginator.init()` が取っているので起点も控えない。
		// 控えると、期限が明けた時点で走らせ直しが init と同じ範囲を取りに行く。
		if (now() < ignoreUntil) return;
		// **古びた控えは先に捨てる。** 残したままだと「控えがある」と見なして
		// この接続の起点を取らず、ここで生じた穴を誰も埋めない。
		if (pending != null && pending.generation !== generation()) pending = null;
		// **起点はここで控える** (理由はファイル冒頭)。見送る回もここを通るので、
		// 条件が解けてから `retry()` で呼び直しても起点は動かない。
		if (pending == null) {
			const since = options.getNewestId();
			// **`undefined` は「いま決められない」だけを意味する** (理由は冒頭)。
			// `null` (本当に空) は控える。
			if (since !== undefined) pending = { since, generation: generation() };
		}
		await run();
	};

	// **その場で走らせず、timer を 1 段挟む。** 呼び出し側は「利用者が見ていない
	// 間は queue へ積む」を Vue の watch で持っており (ノート側の `isPausingUpdate`)、
	// watch はこのリスナーより後に走る。同期で投げると `toQueue` が「まだ裏」と
	// 答え、取れたノートが queue に入ったまま、先に済んだ `releaseQueue()` の後ろに
	// 取り残される。
	const unsubscribeVisible = (options.onVisible ?? defaultOnVisible)(() => {
		// **裏の期間をまたいだ飛行は見捨てる。** 表示に戻った時点で飛行中なら、
		// 裏へ行く前に投げたか待ち始めたものだけ (裏では投げない)。iOS は裏の
		// ページの通信を止めるので、決着するまで (実機で 1.5 分) 何も走らなくなる。
		// 控えは戻す (投げた回は捨てているので)。間隔も数えない。後から返ってきた
		// 結果は id で重複を落として並べ直すので、二重に入っても壊れない。
		if (flight != null) {
			const abandoned = flight;
			flight = null;
			lastAt = abandoned.lastAtBefore;
			// 見捨てた回のほうが古い起点を持つ (控え直しは飛行中にしか起きない) ので、
			// 穴を広く覆う。**ただし世代が変わっていたら戻さない。** 裏にいる間に
			// 総入れ替えされ、そのあと来た接続が新しい世代で控えていることがある。
			// 古い世代で上書きすると、その控えは `run()` の入口で止まり続け、その
			// 接続で空いた穴が二度と埋まらない (一度この確認を外して踏んだ)。
			if (abandoned.generation === generation()) {
				pending = { since: abandoned.since, generation: abandoned.generation };
			}
		}
		schedule(() => {
			void run();
		}, 0);
	});

	return {
		onConnected,
		retry: run,
		dispose: () => {
			disposed = true;
			unsubscribeVisible();
			cancelScheduled?.();
			cancelScheduled = null;
		},
	};
}

/**
 * Builds the `createReconnectResync` options for a `Paginator`-backed list.
 *
 * **判断を `.vue` に置かない。** ここに書いてある述語 (入れ替え中かどうか、
 * 見送る条件、どこへ入れるか、どのパラメータを足すか) は過去に何度も取り違えて
 * いる箇所で、SFC の中にあるとテストから触れない。
 */
export function paginatorResyncOptions(paginator: {
	fetching: { value: boolean };
	rateLimited: { value: boolean };
	error: { value: boolean };
	generation: number;
	getNewestId: () => string | null | undefined;
	fetchNewer: (options: { toQueue?: boolean; sinceId?: string; params?: Record<string, unknown> }) => Promise<void>;
	reload: (params?: Record<string, unknown>) => Promise<void>;
}, opts: {
	/** 取得したものを queue へ積むか (= 利用者が先頭を見ていないか)。 */
	toQueue: () => boolean;
	/** この取得にだけ足すパラメータ。通知一覧は `markAsRead: false` を渡す。 */
	params?: Record<string, unknown>;
}): Pick<ReconnectResyncOptions, 'getNewestId' | 'getGeneration' | 'canResync' | 'resync'> {
	return {
		// `init()` / `reload()` の最中は「いま決められない」。一覧が空なのは
		// 一時的で、そこで控えると誤った起点が入れ替えを跨いで生き残る。
		getNewestId: () => paginator.fetching.value ? undefined : (paginator.getNewestId() ?? null),
		getGeneration: () => paginator.generation,
		// init / reload 中とレート制限中は見送る (どちらも撃つ意味が無い)。控えは
		// 残るので、次の `_connected_` か `retry()` で拾い直せる。
		// **`error` も見る。** `init()` が失敗した一覧は一度も中身を持っていない
		// ので、「切断中の穴」を定義できない。撃つと最古のページをエラー表示の
		// 裏に積むだけになる (`error` は `init()` 成功でしか降りない)。
		canResync: () => !paginator.fetching.value && !paginator.rateLimited.value && !paginator.error.value,
		// **起点が無い (一覧が空だった) ときは取り直す。** `fetchNewer` では表現
		// できない: 起点を省くとサーバーが降順で返して `toReversed()` を前提に
		// した差し込みが逆順になり、`sinceId: '0'` だと**最古のページ**を取って
		// しまう (「一覧が空なら履歴も無い」は `removeItem` で空になったときに
		// 崩れる)。空の一覧は失うものが無いので `reload()` が素直。
		resync: (sinceId) => sinceId == null
			// **`params` はこちらにも渡す。** 通知一覧の `markAsRead: false` が
			// 片方の分岐にしか効かないと、一覧が空のときだけ背景の穴埋めが
			// 全既読にする (`init()` は既定の true で撃つ)。
			? paginator.reload(opts.params)
			: paginator.fetchNewer({
				sinceId,
				toQueue: opts.toQueue(),
				...(opts.params ? { params: opts.params } : {}),
			}),
	};
}

/**
 * 通知一覧ぶんの `params`。**取得そのものが既読化を伴う** ので、背景の穴埋めでは
 * 切る (`markAsRead` 既定 true → `readAllNotifications` を publish)。あれは
 * 「利用者が一覧を開いた/更新した」ことを既読の合図とする設計で、再接続は
 * 利用者の操作ではない。そのまま撃つと、見ていない通知までバッジごと消える。
 */
export const NOTIFICATION_RESYNC_PARAMS = { markAsRead: false } as const;
