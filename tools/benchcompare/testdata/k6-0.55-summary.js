import http from 'k6/http';
import { Counter } from 'k6/metrics';
const rateLimited = new Counter('bench_rate_limited');
const timeouts = new Counter('bench_timeouts');
const endpoints = ['ping', 'limited'];
const thresholds = {};
for (const e of endpoints) {
  thresholds[`http_req_duration{endpoint:${e},expected_response:true}`] = ['p(95)>=0'];
  thresholds[`http_reqs{endpoint:${e}}`] = ['count>=0'];
  thresholds[`http_req_failed{endpoint:${e}}`] = ['rate>=0'];
  thresholds[`bench_rate_limited{endpoint:${e}}`] = ['count>=0'];
  thresholds[`bench_timeouts{endpoint:${e}}`] = ['count>=0'];
}
export const options = { iterations: 20, vus: 2, summaryTrendStats: ['med', 'p(95)', 'p(99)'], thresholds };
export default function () {
  for (const e of endpoints) {
    const r = http.get(`http://127.0.0.1:18765/${e}`, { tags: { endpoint: e }, timeout: '2s' });
    if (r.status === 429) rateLimited.add(1, { endpoint: e });
    if (r.error_code === 1050) timeouts.add(1, { endpoint: e });
  }
}
