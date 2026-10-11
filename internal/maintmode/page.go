package maintmode

// pageHTML is the maintenance page. It depends on nothing but StatusPath:
// no database, no meta, no frontend build. "{{nonce}}" is replaced per
// response to match the Content-Security-Policy.
//
// 管理者が書いた理由はサーバー側で埋め込まず、StatusPath から読んで
// textContent で入れる (HTML として解釈させない)。文言は日本語と英語を
// 両方置く。locale のファイルは frontend の成果物にあり、それが壊れている
// 場合にも出したいため (設計 §2.3)。
const pageHTML = `<!DOCTYPE html>
<html lang="ja">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex">
<title>メンテナンス中 / Under maintenance</title>
<style nonce="{{nonce}}">
:root{color-scheme:light dark;--bg:#f4f6f5;--fg:#1f2523;--muted:#5b6662;--card:#fff;--accent:#5a8f00}
@media (prefers-color-scheme:dark){:root{--bg:#0c1210;--fg:#dee7e4;--muted:#93a29c;--card:#151d1a;--accent:#b4e900}}
*{box-sizing:border-box}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:24px 16px;background:var(--bg);color:var(--fg);font-family:"Hiragino Maru Gothic Pro","BIZ UDGothic",Roboto,"Helvetica Neue",Arial,sans-serif;line-height:1.6}
main{max-width:480px;width:100%;background:var(--card);border-radius:16px;padding:28px 24px;text-align:center}
h1{font-size:20px;margin:0 0 4px}
.en{color:var(--muted);font-size:14px;margin:0}
.phase{margin:20px 0 4px;font-weight:700;color:var(--accent)}
dl{margin:16px 0 0;text-align:left;font-size:14px}
dt{color:var(--muted)}
dd{margin:0 0 8px;overflow-wrap:anywhere;white-space:pre-wrap}
.note{margin-top:20px;font-size:13px;color:var(--muted)}
[hidden]{display:none}
</style>
</head>
<body>
<main>
<h1>メンテナンス中です</h1>
<p class="en">The server is under maintenance.</p>
<p class="phase" id="phase"></p>
<dl>
<div id="started-row" hidden><dt>開始 / Started</dt><dd id="started"></dd></div>
<div id="reason-row" hidden><dt>理由 / Reason</dt><dd id="reason"></dd></div>
</dl>
<p class="note">終わると自動で再読み込みします。<br>This page reloads itself when the maintenance is over.</p>
</main>
<script nonce="{{nonce}}">
(function () {
	var labels = {
		entering: '準備中 / Preparing',
		held: 'メンテナンス中 / In maintenance',
		resuming: '再開中 / Resuming'
	};
	function show(id, rowId, text) {
		var row = document.getElementById(rowId);
		if (!text) { row.hidden = true; return; }
		document.getElementById(id).textContent = text;
		row.hidden = false;
	}
	function render(s) {
		document.getElementById('phase').textContent = labels[s.phase] || labels.held;
		var started = '';
		if (s.startedAt) {
			var d = new Date(s.startedAt);
			if (!isNaN(d.getTime())) started = d.toLocaleString();
		}
		show('started', 'started-row', started);
		show('reason', 'reason-row', s.reason || '');
	}
	function poll() {
		fetch('/maintenance/status', { cache: 'no-store', credentials: 'same-origin' })
			.then(function (res) {
				if (!res.ok) return null;
				// 本文が JSON でない (前段の proxy のページなど) ときは、終わったとは
				// 読まずに次の周を待つ。終わったと読むと、再読み込みを 5 秒ごとに繰り返す。
				return res.json().catch(function () { return null; });
			})
			.then(function (s) {
				if (!s) return;
				if (s.maintenance === false) { location.reload(); return; }
				render(s);
			})
			.catch(function () { /* 一時的に繋がらないときは次の周で読み直す */ })
			.then(function () { setTimeout(poll, 5000); });
	}
	render({ phase: 'held' });
	poll();
})();
</script>
</body>
</html>
`
