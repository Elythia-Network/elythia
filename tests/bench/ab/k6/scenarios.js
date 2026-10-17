// base / head の比較 (#3544) で流す k6 のシナリオ。
//
// tests/bench/http と同じ 7 つのエンドポイントを、同じ負荷の形 (lib/config.js) で
// 順に流す。結果は tools/benchcompare が読む。
//
// **エンドポイントごとに、benchcompare が要求する 5 つの sub-metric すべてに
// threshold を置く。** k6 の --summary-export は threshold の付いた sub-metric しか
// 出さない。1 つでも欠けると benchcompare はその回を読めないとして exit 1 で止まる
// (失敗率を 0 と読んで比較を続けると、失敗の差で速く見える側を見逃すため)。
// threshold はどれも必ず通る形にして、k6 自身は止めない。
//
// SHORT=1 のときは、同じエンドポイントを短く弱く流す。ウォームアップ (結果は捨てる) と、
// run.sh --smoke (手順が通るかだけを見る) で使う。
import exec from "k6/execution";
import { check } from "k6";
import { SharedArray } from "k6/data";
import { Counter } from "k6/metrics";
import { MisskeyClient } from "../lib/client.js";
import { readStages, writeStages, SCENARIO_DURATION } from "../lib/config.js";

const SHORT = __ENV.SHORT === "1";
const SHORT_DURATION = 5;

const client = new MisskeyClient(__ENV.TARGET_URL);
// 既定の 60 秒だと、詰まった回が 1 分ずつ伸びる。10 秒で打ち切って timeout として数える
client.params.timeout = "10s";

const rateLimited = new Counter("bench_rate_limited");
const timeouts = new Counter("bench_timeouts");

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
// 使うのはウォームアップと計測だけになる。tests/bench/results の過去の実測では計測が
// 6000〜7500 件で、2 倍ほどの余裕がある。上限に当たると、速い側ほど 429 が増えて
// benchcompare が「比較不能」にするので、黙って誤った判定にはならない
function token() {
  return tokens[exec.scenario.iterationInTest % tokens.length];
}

const endpoints = [
  { name: "ping", exec: "ping", stages: readStages },
  { name: "meta", exec: "meta", stages: readStages },
  { name: "local-timeline", exec: "localTimeline", stages: readStages },
  { name: "users-show", exec: "usersShow", stages: readStages },
  { name: "i", exec: "i", stages: readStages },
  { name: "notes-create", exec: "notesCreate", stages: writeStages },
  { name: "home-timeline", exec: "homeTimeline", stages: readStages },
];

const scenarios = {};
const thresholds = {};
endpoints.forEach((e, idx) => {
  scenarios[e.name] = {
    executor: "ramping-vus",
    startVUs: 0,
    stages: SHORT ? [{ duration: `${SHORT_DURATION}s`, target: 5 }] : e.stages,
    exec: e.exec,
    tags: { endpoint: e.name },
    startTime: `${idx * (SHORT ? SHORT_DURATION + 1 : SCENARIO_DURATION)}s`,
  };
  thresholds[`http_req_duration{endpoint:${e.name},expected_response:true}`] = ["p(95)>=0"];
  thresholds[`http_reqs{endpoint:${e.name}}`] = ["count>=0"];
  thresholds[`http_req_failed{endpoint:${e.name}}`] = ["rate>=0"];
  thresholds[`bench_rate_limited{endpoint:${e.name}}`] = ["count>=0"];
  thresholds[`bench_timeouts{endpoint:${e.name}}`] = ["count>=0"];
});

export const options = {
  scenarios,
  thresholds,
  summaryTrendStats: ["med", "p(95)", "p(99)"],
};

function record(endpoint, res) {
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
