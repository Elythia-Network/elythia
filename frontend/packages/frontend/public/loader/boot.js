/*
 * SPDX-FileCopyrightText: syuilo and misskey-project
 * SPDX-License-Identifier: AGPL-3.0-only
 */

'use strict';

// ブロックの中に入れないと、定義した変数がブラウザのグローバルスコープに登録されてしまい邪魔なので
(async () => {
	window.onerror = (e) => {
		console.error(e);
		renderError('SOMETHING_HAPPENED', e);
	};
	window.onunhandledrejection = (e) => {
		console.error(e);
		renderError('SOMETHING_HAPPENED_IN_PROMISE', e.reason || e);
	};

	let forceError = localStorage.getItem('forceError');
	if (forceError != null) {
		renderError('FORCED_ERROR', 'This error is forced by having forceError in local storage.');
		return;
	}

	//#region Detect language
	const supportedLangs = LANGS;
	/** @type { string } */
	let lang = localStorage.getItem('lang');
	if (lang == null || !supportedLangs.includes(lang)) {
		if (supportedLangs.includes(navigator.language)) {
			lang = navigator.language;
		} else {
			lang = supportedLangs.find(x => x.split('-')[0] === navigator.language);

			// Fallback
			if (lang == null) lang = 'en-US';
		}
	}

	// for https://github.com/misskey-dev/misskey/issues/10202
	if (lang == null || lang.toString == null || lang.toString() === 'null') {
		console.error('invalid lang value detected!!!', typeof lang, lang);
		lang = 'en-US';
	}

	localStorage.setItem('lang', lang);
	//#endregion

	//#region Script
	async function importAppScript() {
		await import(CLIENT_ENTRY ? `/vite/${CLIENT_ENTRY.replace('scripts', lang)}` : '/vite/src/_boot_.ts')
			.catch(async e => {
				console.error(e);
				renderError('APP_IMPORT', e);
			});
	}

	// タイミングによっては、この時点でDOMの構築が済んでいる場合とそうでない場合とがある
	if (document.readyState !== 'loading') {
		importAppScript();
	} else {
		window.addEventListener('DOMContentLoaded', () => {
			importAppScript();
		});
	}
	//#endregion

	let isSafeMode = (localStorage.getItem('isSafeMode') === 'true');

	if (!isSafeMode) {
		const urlParams = new URLSearchParams(window.location.search);

		if (urlParams.has('safemode') && urlParams.get('safemode') === 'true') {
			localStorage.setItem('isSafeMode', 'true');
			isSafeMode = true;
		}
	}

	//#region Theme
	if (!isSafeMode) {
		const theme = localStorage.getItem('theme');
		if (theme) {
			for (const [k, v] of Object.entries(JSON.parse(theme))) {
				document.documentElement.style.setProperty(`--MI_THEME-${k}`, v.toString());

				// HTMLの theme-color 適用
				if (k === 'htmlThemeColor') {
					for (const tag of document.head.children) {
						if (tag.tagName === 'META' && tag.getAttribute('name') === 'theme-color') {
							tag.setAttribute('content', v);
							break;
						}
					}
				}
			}
		}
	}

	const colorScheme = localStorage.getItem('colorScheme');
	if (colorScheme) {
		document.documentElement.style.setProperty('color-scheme', colorScheme);
	}
	//#endregion

	const fontSize = localStorage.getItem('fontSize');
	if (fontSize) {
		document.documentElement.classList.add('f-' + fontSize);
	}

	const useSystemFont = localStorage.getItem('useSystemFont');
	if (useSystemFont) {
		document.documentElement.classList.add('useSystemFont');
	}

	if (!isSafeMode) {
		const customCss = localStorage.getItem('customCss');
		if (customCss && customCss.length > 0) {
			const style = document.createElement('style');
			style.innerHTML = customCss;
			document.head.appendChild(style);
		}
	}

	async function addStyle(styleText) {
		let css = document.createElement('style');
		css.appendChild(document.createTextNode(styleText));
		document.head.appendChild(css);
	}

	async function renderError(code, details) {
		// Cannot set property 'innerHTML' of null を回避
		if (document.readyState === 'loading') {
			await new Promise(resolve => window.addEventListener('DOMContentLoaded', resolve));
		}

		let messages = null;
		const bootloaderLocales = localStorage.getItem('bootloaderLocales');
		if (bootloaderLocales) {
			messages = JSON.parse(bootloaderLocales);
		}
		if (!messages) {
			// older version of misskey does not store bootloaderLocales, stores locale as a whole
			const legacyLocale = localStorage.getItem('locale');
			if (legacyLocale) {
				const parsed = JSON.parse(legacyLocale);
				messages = {
					...(parsed._bootErrors ?? {}),
					reload: parsed.reload,
				};
			}
		}
		if (!messages) messages = {};

		messages = Object.assign({
			title: 'Failed to initialize Misskey',
			solution: 'The following actions may solve the problem.',
			solution1: 'Update your os and browser',
			solution2: 'Disable an adblocker',
			solution3: 'Clear the browser cache',
			solution4: '(Tor Browser) Set dom.webaudio.enabled to true',
			otherOption: 'Other options',
			otherOption1: 'Clear preferences and cache',
			otherOption2: 'Start the simple client',
			otherOption3: 'Start the repair tool',
			otherOption4: 'Start Misskey in safe mode',
			reload: 'Reload',
		}, messages);

		const safeModeUrl = new URL(window.location.href);
		safeModeUrl.searchParams.set('safemode', 'true');

		let errorsElement = document.getElementById('errors');

		if (!errorsElement) {
			document.body.innerHTML = `
			<svg class="icon-warning" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" fill="none" stroke-linecap="round" stroke-linejoin="round">
				<path stroke="none" d="M0 0h24v24H0z" fill="none"></path>
				<path d="M12 9v2m0 4v.01"></path>
				<path d="M5 19h14a2 2 0 0 0 1.84 -2.75l-7.1 -12.25a2 2 0 0 0 -3.5 0l-7.1 12.25a2 2 0 0 0 1.75 2.75"></path>
			</svg>
			<h1>${messages.title}</h1>
			<button id="mkBootReload" class="button-big">
				<span class="button-label-big">${messages?.reload}</span>
			</button>
			<p><b>${messages.solution}</b></p>
			<p>${messages.solution1}</p>
			<p>${messages.solution2}</p>
			<p>${messages.solution3}</p>
			<p>${messages.solution4}</p>
			<details style="color: #86b300;">
				<summary>${messages.otherOption}</summary>
				<a href="${safeModeUrl}">
					<button class="button-small">
						<span class="button-label-small">${messages.otherOption4}</span>
					</button>
				</a>
				<br>
				<a href="/flush">
					<button class="button-small">
						<span class="button-label-small">${messages.otherOption1}</span>
					</button>
				</a>
				<br>
				<a href="/cli">
					<button class="button-small">
						<span class="button-label-small">${messages.otherOption2}</span>
					</button>
				</a>
				<br>
				<a href="/bios">
					<button class="button-small">
						<span class="button-label-small">${messages.otherOption3}</span>
					</button>
				</a>
			</details>
			<br>
			<div id="errors"></div>
			`;
			// mk-go: onclick 属性から addEventListener へ移した。inline event handler は
			// CSP の script-src で hash を登録しても通らず ('unsafe-hashes' が要る)、
			// mk-go は #2786 で script-src から 'unsafe-inline' を外したため、属性の
			// ままだとこの復旧ボタンだけが押しても反応しなくなる。
			document.getElementById('mkBootReload')?.addEventListener('click', () => location.reload(true));
			errorsElement = document.getElementById('errors');
		}
		const detailsElement = document.createElement('details');
		detailsElement.id = 'errorInfo';
		detailsElement.innerHTML = `
		<br>
		<summary>
			<code>ERROR CODE: ${code}</code>
		</summary>
		<code>${details.toString()} ${JSON.stringify(details)}</code>`;
		errorsElement.appendChild(detailsElement);
		addStyle(`
		* {
			font-family: BIZ UDGothic, Roboto, HelveticaNeue, Arial, sans-serif;
		}

		#misskey_app,
		#splash {
			display: none !important;
		}

		body,
		html {
			background-color: #222;
			color: #dfddcc;
			justify-content: center;
			margin: auto;
			padding: 10px;
			text-align: center;
		}

		button {
			border-radius: 999px;
			padding: 0px 12px 0px 12px;
			border: none;
			cursor: pointer;
			margin-bottom: 12px;
		}

		.button-big {
			background: linear-gradient(90deg, rgb(134, 179, 0), rgb(74, 179, 0));
			line-height: 50px;
		}

		.button-big:hover {
			background: rgb(153, 204, 0);
		}

		.button-small {
			background: #444;
			line-height: 40px;
		}

		.button-small:hover {
			background: #555;
		}

		.button-label-big {
			color: #222;
			font-weight: bold;
			font-size: 1.2em;
			padding: 12px;
		}

		.button-label-small {
			color: rgb(153, 204, 0);
			font-size: 16px;
			padding: 12px;
		}

		a {
			color: rgb(134, 179, 0);
			text-decoration: none;
		}

		p,
		li {
			font-size: 16px;
		}

		.icon-warning {
			color: #dec340;
			height: 4rem;
			padding-top: 2rem;
		}

		h1 {
			font-size: 1.5em;
			margin: 1em;
		}

		code {
			font-family: Fira, FiraCode, monospace;
		}

		#errorInfo {
			background: #333;
			margin-bottom: 2rem;
			padding: 0.5rem 1rem;
			width: 40rem;
			border-radius: 10px;
			justify-content: center;
			margin: auto;
		}

		#errorInfo summary {
			cursor: pointer;
		}

		#errorInfo summary > * {
			display: inline;
		}

		@media screen and (max-width: 500px) {
			#errorInfo {
				width: 50%;
			}
		}`);
	}
})();

// Elythia: 起動画面の星空。背景・星雲・アイコンは style.css が描き、ここでは
// #splashSky (canvas) に星と流れ星を描く。
//
// **例外を外へ出さない。** 上のブロックが window.onerror で renderError を
// 呼ぶので、飾りの描画が 1 回失敗しただけで起動失敗の画面に化ける。描画で
// 失敗したら、星空を諦めて止まるだけにする。
(() => {
	// 星白・淡紫・真珠。白を多めにして、色の付いた星が混ざる程度にする
	const COLORS = ['253,241,235', '253,241,235', '253,241,235', '204,195,247', '253,221,207'];

	function animationDisabled() {
		try {
			if (window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches) return true;
			// アプリの「アニメーション」設定。preferences は [scope, value, meta] の
			// 記録の並びで、どのアカウントの記録かはここでは決められない。どれか
			// 1 つでも切られていれば止める方に倒す
			const raw = localStorage.getItem('preferences');
			if (raw == null) return false;
			const records = JSON.parse(raw)?.preferences?.animation;
			return Array.isArray(records) && records.some(r => Array.isArray(r) && r[1] === false);
		} catch {
			return false;
		}
	}

	function start() {
		const splash = document.getElementById('splash');
		const canvas = document.getElementById('splashSky');
		if (splash == null || canvas == null || typeof canvas.getContext !== 'function') return;
		const g = canvas.getContext('2d');
		if (g == null) return;

		const still = animationDisabled();
		if (still) splash.classList.add('splashStill');

		let w = 1;
		let h = 1;
		let stars = [];
		let meteors = [];
		let t0 = performance.now();
		let last = t0;
		let nextMeteor = 1.5;
		let stopped = false;

		function stop() {
			stopped = true;
			window.removeEventListener('resize', onResize);
		}

		function fit() {
			const rect = splash.getBoundingClientRect();
			w = Math.max(1, rect.width);
			h = Math.max(1, rect.height);
			// 高精細の画面でも 2 倍まで、広い画面では 1.5 倍までにする。起動処理と
			// 同じ時間帯に描くので、画素数をそのまま追うと読み込みそのものを遅くする
			const dpr = Math.min(window.devicePixelRatio || 1, w * h > 1600000 ? 1.5 : 2);
			canvas.width = Math.round(w * dpr);
			canvas.height = Math.round(h * dpr);
			g.setTransform(dpr, 0, 0, dpr, 0, 0);
		}

		function build() {
			fit();
			const n = Math.min(260, Math.round(w * h / 2600));
			stars = [];
			for (let i = 0; i < n; i++) {
				const big = Math.random() < 0.06;
				stars.push({
					x: Math.random() * w,
					y: Math.random() * h,
					r: big ? 1.1 + Math.random() * 0.9 : 0.3 + Math.random() * 0.8,
					a: 0.25 + Math.random() * 0.6,
					sp: 0.4 + Math.random() * 1.6,
					ph: Math.random() * Math.PI * 2,
					c: COLORS[(Math.random() * COLORS.length) | 0],
					big,
					// 1 秒あたりの横の移動量 (px)
					vx: -(2 + Math.random() * 6),
				});
			}
		}

		function sparkle(x, y, s, a, c) {
			g.strokeStyle = `rgba(${c},${a * 0.7})`;
			g.lineWidth = 0.6;
			g.beginPath();
			g.moveTo(x - s, y);
			g.lineTo(x + s, y);
			g.moveTo(x, y - s);
			g.lineTo(x, y + s);
			g.stroke();
		}

		function drawStar(s, a) {
			g.fillStyle = `rgba(${s.c},${a})`;
			g.beginPath();
			g.arc(s.x, s.y, s.r, 0, Math.PI * 2);
			g.fill();
			if (s.big) {
				const glow = g.createRadialGradient(s.x, s.y, 0, s.x, s.y, s.r * 5);
				glow.addColorStop(0, `rgba(${s.c},${a * 0.35})`);
				glow.addColorStop(1, `rgba(${s.c},0)`);
				g.fillStyle = glow;
				g.beginPath();
				g.arc(s.x, s.y, s.r * 5, 0, Math.PI * 2);
				g.fill();
				sparkle(s.x, s.y, s.r * 3.2, a, s.c);
			}
		}

		function drawMeteors(t, dt) {
			if (t > nextMeteor) {
				const fromLeft = Math.random() < 0.5;
				meteors.push({
					x: fromLeft ? w * (0.05 + Math.random() * 0.35) : w * (0.6 + Math.random() * 0.35),
					y: h * Math.random() * 0.35,
					vx: (fromLeft ? 1 : -1) * w * 0.55,
					vy: h * 0.32,
					life: 0,
					dur: 0.9 + Math.random() * 0.4,
				});
				nextMeteor = t + 3.5 + Math.random() * 4;
			}
			for (let i = meteors.length - 1; i >= 0; i--) {
				const m = meteors[i];
				m.life += dt;
				const p = m.life / m.dur;
				if (p >= 1) {
					meteors.splice(i, 1);
					continue;
				}
				const hx = m.x + m.vx * p;
				const hy = m.y + m.vy * p;
				const tx = hx - m.vx * 0.18;
				const ty = hy - m.vy * 0.18;
				const al = Math.sin(Math.PI * p);
				const grad = g.createLinearGradient(hx, hy, tx, ty);
				grad.addColorStop(0, `rgba(253,241,235,${0.95 * al})`);
				grad.addColorStop(0.3, `rgba(204,195,247,${0.45 * al})`);
				grad.addColorStop(1, 'rgba(166,155,251,0)');
				g.strokeStyle = grad;
				g.lineWidth = 1.4;
				g.lineCap = 'round';
				g.beginPath();
				g.moveTo(hx, hy);
				g.lineTo(tx, ty);
				g.stroke();
				sparkle(hx, hy, 3.5, al, '253,241,235');
			}
		}

		function frame(now) {
			if (stopped) return;
			// 読み込みが終わると common.ts が #splash を外す。外れたら描くのをやめる
			if (!splash.isConnected) {
				stop();
				return;
			}
			try {
				const t = (now - t0) / 1000;
				// タブが裏にあった間の経過をまとめて進めない
				const dt = Math.min(0.05, Math.max(0, (now - last) / 1000));
				last = now;
				const fadeIn = Math.min(1, t / 1.2);
				g.clearRect(0, 0, w, h);
				for (const s of stars) {
					s.x += s.vx * dt;
					if (s.x < -2) s.x = w + 2;
					drawStar(s, s.a * (0.55 + 0.45 * Math.sin(t * s.sp + s.ph)) * fadeIn);
				}
				drawMeteors(t, dt);
			} catch (e) {
				console.error(e);
				stop();
				return;
			}
			window.requestAnimationFrame(frame);
		}

		function drawStill() {
			g.clearRect(0, 0, w, h);
			for (const s of stars) drawStar(s, s.a * 0.8);
		}

		let resizeTimer = null;
		function onResize() {
			window.clearTimeout(resizeTimer);
			resizeTimer = window.setTimeout(() => {
				if (stopped) return;
				// 静止画のときは描画の繰り返しが無いので、外れたことにここで気付く
				if (!splash.isConnected) {
					stop();
					return;
				}
				try {
					// 星を作り直さず、新しい大きさへ比率で写す。スマートフォンでは
					// アドレスバーの出入りのたびに resize が来るので、作り直すと
					// 星の配置がそのたびに入れ替わって見える
					const pw = w;
					const ph = h;
					fit();
					if (pw < 2 || ph < 2) {
						// 大きさが一瞬 0 になった後は比率が意味を持たないので作り直す
						build();
					} else {
						for (const s of stars) {
							s.x = s.x * w / pw;
							s.y = s.y * h / ph;
						}
					}
					if (still) drawStill();
				} catch (e) {
					console.error(e);
					stop();
				}
			}, 120);
		}

		try {
			build();
			window.addEventListener('resize', onResize);
			if (still) {
				drawStill();
			} else {
				t0 = performance.now();
				last = t0;
				window.requestAnimationFrame(frame);
			}
		} catch (e) {
			console.error(e);
			stop();
		}
	}

	if (document.readyState !== 'loading') {
		start();
	} else {
		window.addEventListener('DOMContentLoaded', start);
	}
})();
