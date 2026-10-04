import { check } from "k6";
import { SharedArray } from "k6/data";
import { MisskeyClient } from "./lib/client.js";
import { readStages, writeStages, SCENARIO_DURATION } from "./lib/config.js";

const BASE_URL = __ENV.TARGET_URL;
const client = new MisskeyClient(BASE_URL);

// seed.pyが出力したtokenを読み込む
const seedData = new SharedArray("seed", function () {
  try {
    return [JSON.parse(open("/seed/seed-data.json"))];
  } catch (_) {
    return [{ tokens: [], username: "benchadmin" }];
  }
});

function getToken() {
  const tokens = seedData[0].tokens;
  if (tokens.length === 0) return null;
  return tokens[__VU % tokens.length];
}

function getUsername() {
  return seedData[0].username || "benchadmin";
}

// 各シナリオをstartTimeでずらして逐次実行する
export const options = {
  insecureSkipTLSVerify: true,
  scenarios: {
    ping: {
      executor: "ramping-vus",
      startVUs: 0,
      stages: readStages,
      exec: "pingScenario",
      tags: { endpoint: "ping" },
      startTime: "0s",
    },
    meta: {
      executor: "ramping-vus",
      startVUs: 0,
      stages: readStages,
      exec: "metaScenario",
      tags: { endpoint: "meta" },
      startTime: `${SCENARIO_DURATION}s`,
    },
    localTimeline: {
      executor: "ramping-vus",
      startVUs: 0,
      stages: readStages,
      exec: "localTimelineScenario",
      tags: { endpoint: "local-timeline" },
      startTime: `${SCENARIO_DURATION * 2}s`,
    },
    usersShow: {
      executor: "ramping-vus",
      startVUs: 0,
      stages: readStages,
      exec: "usersShowScenario",
      tags: { endpoint: "users-show" },
      startTime: `${SCENARIO_DURATION * 3}s`,
    },
    iEndpoint: {
      executor: "ramping-vus",
      startVUs: 0,
      stages: readStages,
      exec: "iScenario",
      tags: { endpoint: "i" },
      startTime: `${SCENARIO_DURATION * 4}s`,
    },
    notesCreate: {
      executor: "ramping-vus",
      startVUs: 0,
      stages: writeStages,
      exec: "notesCreateScenario",
      tags: { endpoint: "notes-create" },
      startTime: `${SCENARIO_DURATION * 5}s`,
    },
    homeTimeline: {
      executor: "ramping-vus",
      startVUs: 0,
      stages: readStages,
      exec: "homeTimelineScenario",
      tags: { endpoint: "home-timeline" },
      startTime: `${SCENARIO_DURATION * 6}s`,
    },
  },
  thresholds: {
    "http_req_duration{endpoint:ping}": ["p(95)<200"],
    "http_req_duration{endpoint:meta}": ["p(95)<500"],
    "http_req_duration{endpoint:local-timeline}": ["p(95)<1000"],
    "http_req_duration{endpoint:users-show}": ["p(95)<500"],
    "http_req_duration{endpoint:i}": ["p(95)<500"],
    "http_req_duration{endpoint:notes-create}": ["p(95)<2000"],
    // home-timeline は計測専用 (gate しない)。ただし k6 は threshold を持つ tagged
    // sub-metric のみ summary-export に出すため、percentile を report に残すには
    // threshold 定義が必須。authed home timeline は TS 側で 1s を超えうるので、
    // 常に pass する tautology (p95>=0) を置いて「abort せず計測だけする」。
    "http_req_duration{endpoint:home-timeline}": ["p(95)>=0"],
  },
};

export function pingScenario() {
  const res = client.ping();
  check(res, { "ping 200": (r) => r.status === 200 });
}

export function metaScenario() {
  const res = client.meta();
  check(res, { "meta 200": (r) => r.status === 200 });
}

export function localTimelineScenario() {
  const res = client.localTimeline(20);
  check(res, { "local-timeline 200": (r) => r.status === 200 });
}

export function usersShowScenario() {
  const res = client.usersShow(getUsername());
  check(res, { "users-show 200": (r) => r.status === 200 });
}

export function iScenario() {
  const token = getToken();
  if (!token) return;
  const res = client.i(token);
  check(res, { "i 200": (r) => r.status === 200 });
}

export function notesCreateScenario() {
  const token = getToken();
  if (!token) return;
  const text = `bench ${Date.now()}-${__VU}-${__ITER}`;
  const res = client.createNote(text, token);
  check(res, { "notes-create 200": (r) => r.status === 200 });
}

export function homeTimelineScenario() {
  const token = getToken();
  if (!token) return;
  const res = client.homeTimeline(token, 20);
  check(res, { "home-timeline 200": (r) => r.status === 200 });
}
