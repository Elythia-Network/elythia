import http from 'k6/http';
import exec from 'k6/execution';
import { Counter, Gauge } from 'k6/metrics';
const rateLimited = new Counter('bench_rate_limited');
const timeouts = new Counter('bench_timeouts');
const windowStart = new Gauge('bench_window_start_ms');
const windowEnd = new Gauge('bench_window_end_ms');
// ping は 200、limited は 429 を返す (busybox httpd の静的ファイルと CGI)
const endpoints = { ping: '/ping', limited: '/cgi-bin/limited' };
const thresholds = {};
for (const e of Object.keys(endpoints)) {
  thresholds[`http_req_duration{endpoint:${e},expected_response:true}`] = ['p(95)>=0'];
  thresholds[`http_reqs{endpoint:${e}}`] = ['count>=0'];
  thresholds[`http_req_failed{endpoint:${e}}`] = ['rate>=0'];
  thresholds[`bench_rate_limited{endpoint:${e}}`] = ['count>=0'];
  thresholds[`bench_timeouts{endpoint:${e}}`] = ['count>=0'];
  thresholds[`bench_window_start_ms{endpoint:${e}}`] = ['value>=0'];
  thresholds[`bench_window_end_ms{endpoint:${e}}`] = ['value>=0'];
}
export const options = { iterations: 20, vus: 2, summaryTrendStats: ['med', 'p(95)', 'p(99)'], thresholds };
export default function () {
  for (const [e, path] of Object.entries(endpoints)) {
    if (exec.vu.iterationInScenario === 0) {
      windowStart.add(exec.scenario.startTime, { endpoint: e });
      windowEnd.add(exec.scenario.startTime + 30000, { endpoint: e });
    }
    const r = http.get(`http://127.0.0.1:18765${path}`, { tags: { endpoint: e }, timeout: '2s' });
    if (r.status === 429) rateLimited.add(1, { endpoint: e });
    if (r.error_code === 1050) timeouts.add(1, { endpoint: e });
  }
}
