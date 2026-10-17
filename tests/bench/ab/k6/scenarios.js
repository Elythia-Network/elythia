// base / head の比較 (#3544) で流す k6 のシナリオ。
//
// tests/bench/http と同じ 7 つのエンドポイントを、1 つずつ順に流す。結果は
// tools/benchcompare が読む。
//
// 負荷は constant-arrival-rate (open-loop) で、エンドポイントごとの率 (lib/config.js の
// arrivalRates) で送る (#3656)。応答が遅れても送る間隔は変わらないので、両側で同じ件数を
// 同じ間隔で送り、その件数を捌く時間を比べる。VU が足りずに送れなかった分は k6 が
// dropped_iterations に数え、benchcompare はそのエンドポイントを比較不能にする。
//
// **エンドポイントごとに、benchcompare が要求する 8 つの sub-metric すべてに
// threshold を置く。** k6 の --summary-export は threshold の付いた sub-metric しか
// 出さない。1 つでも欠けると benchcompare はその回を読めないとして exit 1 で止まる
// (失敗率を 0 と読んで比較を続けると、失敗の差で速く見える側を見逃すため)。
// threshold はどれも必ず通る形にして、k6 自身は止めない。dropped_iterations には
// シナリオの tags (endpoint) が付かないので、scenario タグ (シナリオ名 = エンドポイント名)
// で絞る。
//
// SHORT=1 のときは、同じエンドポイントを同じ率で 5 秒ずつ流す。ウォームアップ (結果は
// 捨てる) と、run.sh --smoke (手順が通るかだけを見る) で使う。
//
// START_DELAY (秒) は、最初のシナリオを始めるまでの待ち。run.sh は k6 を起動してから
// そのコンテナを資源の記録 (sampler) に加えるので、その間に負荷をかけ始めないよう待たせる。
//
// 各エンドポイントの区間 (予定の開始と終了、Unix ミリ秒) を gauge
// (bench_window_start_ms / bench_window_end_ms) に書く。benchcompare は、この区間で k6 の
// コンテナが CPU の上限で止められた割合を出し、k6 が律速になったエンドポイントを比較不能に
// する (#3656)。
import exec from "k6/execution";
import { check } from "k6";
import { SharedArray } from "k6/data";
import { Counter, Gauge } from "k6/metrics";
import { MisskeyClient } from "../lib/client.js";
import { arrivalRates, ARRIVAL_DURATION, SCENARIO_DURATION } from "../lib/config.js";

const SHORT = __ENV.SHORT === "1";
const SHORT_DURATION = 5;
const START_DELAY = Number(__ENV.START_DELAY || 0);

const client = new MisskeyClient(__ENV.TARGET_URL);
// 既定の 60 秒だと、詰まった回が 1 分ずつ伸びる。10 秒で打ち切って timeout として数える
client.params.timeout = "10s";

const rateLimited = new Counter("bench_rate_limited");
const timeouts = new Counter("bench_timeouts");
const windowStart = new Gauge("bench_window_start_ms");
const windowEnd = new Gauge("bench_window_end_ms");

const seed = new SharedArray("seed", function () {
  return [JSON.parse(open("/seed/seed-data.json"))];
});
// SharedArray は添字で読むたびに要素を JSON.parse し直す。要素は seed 全体 (約 63KB、
// 1 回約 4ms) なので、iteration ごとに seed[0] を読むと k6 (CPU 0.75) がそれだけで
// 飽和し、毎秒約 160 件で頭打ちになっていた (#3656)。使う値は VU の初期化で 1 回だけ
// 取り出し、iteration の中では SharedArray に触れない
const tokens = seed[0].tokens;
const username = seed[0].username;

// 認証の要るエンドポイントは、iteration ごとに利用者を順に替える。VU ごとに
// 固定すると、notes/create の利用者ごとの上限 (1 時間に 300) に数人だけが当たり、
// prod-like で 429 を測ることになる。
//
// 50 人に均すと、上限は 50 × 300 = 15000 件。seed は制限を外して流すので数に入らず、
// 使うのはウォームアップと計測だけになる (到着率で 1050 件、lib/config.js)。上限に当たると、
// 429 の率の差で benchcompare が「比較不能」にするので、黙って誤った判定にはならない
function token() {
  return tokens[exec.scenario.iterationInTest % tokens.length];
}

const endpoints = [
  { name: "ping", exec: "ping" },
  { name: "meta", exec: "meta" },
  { name: "local-timeline", exec: "localTimeline" },
  { name: "users-show", exec: "usersShow" },
  { name: "i", exec: "i" },
  { name: "notes-create", exec: "notesCreate" },
  { name: "home-timeline", exec: "homeTimeline" },
];

// 1 つのシナリオの長さ (秒)。区間の gauge にも使う
const duration = SHORT ? SHORT_DURATION : ARRIVAL_DURATION;
// VU は最初に maxVUs まで作っておく。k6 は送る時刻に空いた VU が無いと、その iteration を
// dropped_iterations に数えて落としてから VU を足す (毎秒 100 件、1 件 0.5 秒で 50 VU が要る
// ところを preAllocatedVUs 10 で流すと、5 秒で 41 件落ちた。100 にすると 0 件)。途中で VU を
// 足すと、その初期化 (スクリプトの評価と seed の取り出し) の CPU も計測の区間に入る。
// VU はシナリオの間で使い回すので、作るのは初期化のときの 100 個だけ。100 個が全部応答を
// 待っているほど遅いときは、送れなかった分が dropped_iterations に数えられる
const PRE_ALLOCATED_VUS = 100;
const MAX_VUS = 100;

const scenarios = {};
const thresholds = {};
endpoints.forEach((e, idx) => {
  const rate = arrivalRates[e.name];
  if (!(rate > 0)) throw new Error(`no arrival rate for ${e.name} in lib/config.js`);
  scenarios[e.name] = {
    executor: "constant-arrival-rate",
    rate,
    timeUnit: "1s",
    duration: `${duration}s`,
    preAllocatedVUs: PRE_ALLOCATED_VUS,
    maxVUs: MAX_VUS,
    // 次のシナリオまでの 1 秒の間に終わらせ、区間を重ねない
    gracefulStop: "1s",
    exec: e.exec,
    tags: { endpoint: e.name },
    startTime: `${START_DELAY + idx * (SHORT ? SHORT_DURATION + 1 : SCENARIO_DURATION)}s`,
  };
  thresholds[`dropped_iterations{scenario:${e.name}}`] = ["count>=0"];
  thresholds[`http_req_duration{endpoint:${e.name},expected_response:true}`] = ["p(95)>=0"];
  thresholds[`http_reqs{endpoint:${e.name}}`] = ["count>=0"];
  thresholds[`http_req_failed{endpoint:${e.name}}`] = ["rate>=0"];
  thresholds[`bench_rate_limited{endpoint:${e.name}}`] = ["count>=0"];
  thresholds[`bench_timeouts{endpoint:${e.name}}`] = ["count>=0"];
  thresholds[`bench_window_start_ms{endpoint:${e.name}}`] = ["value>=0"];
  thresholds[`bench_window_end_ms{endpoint:${e.name}}`] = ["value>=0"];
});

export const options = {
  scenarios,
  thresholds,
  summaryTrendStats: ["med", "p(95)", "p(99)"],
};

function record(endpoint, res) {
  // 区間はシナリオごとに決まった値なので、VU ごとに最初の iteration でだけ書く
  if (exec.vu.iterationInScenario === 0) {
    const start = exec.scenario.startTime;
    windowStart.add(start, { endpoint });
    windowEnd.add(start + duration * 1000, { endpoint });
  }
  if (res.status === 429) rateLimited.add(1, { endpoint });
  // 1050 は k6 の request timeout
  if (res.error_code === 1050) timeouts.add(1, { endpoint });
  check(res, { [`${endpoint} 200`]: (r) => r.status === 200 });
}

export function ping() {
  record("ping", client.ping());
}

export function meta() {
  record("meta", client.meta());
}

export function localTimeline() {
  record("local-timeline", client.localTimeline(20));
}

export function usersShow() {
  record("users-show", client.usersShow(username));
}

export function i() {
  record("i", client.i(token()));
}

export function notesCreate() {
  const text = `bench ${exec.scenario.iterationInTest}-${__VU}-${__ITER}`;
  record("notes-create", client.createNote(text, token()));
}

export function homeTimeline() {
  record("home-timeline", client.homeTimeline(token(), 20));
}
