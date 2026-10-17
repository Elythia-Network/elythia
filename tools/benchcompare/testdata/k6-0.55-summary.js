// benchcompare の TestLoad_RealK6Output が読む k6-0.55-summary.json を作るスクリプト。
// busybox httpd を 127.0.0.1:18765 で動かし (ping は静的ファイル、limited は 429 を、slow は
// 1 秒待ってから 200 を返す CGI)、k6 をそのネットワークに入れて流す。
// slow は 1 VU で毎秒 10 件を求めるので、送れなかった分が dropped_iterations に数えられる。
import http from 'k6/http';
import exec from 'k6/execution';
import { Counter, Gauge } from 'k6/metrics';
const rateLimited = new Counter('bench_rate_limited');
const timeouts = new Counter('bench_timeouts');
const windowStart = new Gauge('bench_window_start_ms');
const windowEnd = new Gauge('bench_window_end_ms');
const paths = { ping: '/ping', limited: '/cgi-bin/limited', slow: '/cgi-bin/slow' };
const scenarios = {
  ping: { executor: 'shared-iterations', iterations: 20, vus: 2 },
  limited: { executor: 'shared-iterations', iterations: 20, vus: 2 },
  slow: { executor: 'constant-arrival-rate', rate: 10, timeUnit: '1s', duration: '2s', preAllocatedVUs: 1, maxVUs: 1 },
};
const thresholds = {};
for (const e of Object.keys(paths)) {
  Object.assign(scenarios[e], { exec: 'hit', tags: { endpoint: e } });
  thresholds[`http_req_duration{endpoint:${e},expected_response:true}`] = ['p(95)>=0'];
  thresholds[`http_reqs{endpoint:${e}}`] = ['count>=0'];
  thresholds[`http_req_failed{endpoint:${e}}`] = ['rate>=0'];
  thresholds[`bench_rate_limited{endpoint:${e}}`] = ['count>=0'];
  thresholds[`bench_timeouts{endpoint:${e}}`] = ['count>=0'];
  thresholds[`bench_window_start_ms{endpoint:${e}}`] = ['value>=0'];
  thresholds[`bench_window_end_ms{endpoint:${e}}`] = ['value>=0'];
  thresholds[`dropped_iterations{scenario:${e}}`] = ['count>=0'];
}
export const options = { scenarios, summaryTrendStats: ['med', 'p(95)', 'p(99)'], thresholds };
export function hit() {
  const e = exec.scenario.name;
  if (exec.vu.iterationInScenario === 0) {
    windowStart.add(exec.scenario.startTime, { endpoint: e });
    windowEnd.add(exec.scenario.startTime + 30000, { endpoint: e });
  }
  const r = http.get(`http://127.0.0.1:18765${paths[e]}`, { tags: { endpoint: e }, timeout: '2s' });
  if (r.status === 429) rateLimited.add(1, { endpoint: e });
  if (r.error_code === 1050) timeouts.add(1, { endpoint: e });
}
