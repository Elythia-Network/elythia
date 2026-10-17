// NODE_ENV=developmentでMisskeyのレートリミットを無効化しているため、
// 高負荷プロファイルで処理性能を比較する。

// 読み取り系: 20→50 VUにランプアップして20秒定速。
export const readStages = [
  { duration: "5s", target: 20 },
  { duration: "20s", target: 50 },
  { duration: "5s", target: 0 },
];

// 書き込み系: 10→20 VU。notes/createは重い処理なので控えめ。
export const writeStages = [
  { duration: "5s", target: 10 },
  { duration: "20s", target: 20 },
  { duration: "5s", target: 0 },
];

// Duration of one scenario block (seconds).
export const SCENARIO_DURATION = 31;

// base / head の比較と本家との比較 (tests/bench/ab/k6/scenarios.js) の、エンドポイントごとの
// 到着率 (1 秒あたりの件数)。k6 の constant-arrival-rate で、応答の速さによらず同じ件数を
// 同じ間隔で送る (#3656)。
//
// 以前の ramping-vus (待ち時間なしの closed-loop) では、サーバーが速いほど k6 が先に CPU の
// 上限 (0.75) で飽和し、k6 の待ちを測っていた。率は次の 2 つより十分低くする:
//
// - k6 の上限: ping を 0.75 コアの k6 で送ると毎秒約 5000 件で頭打ちになった
//   (bench-upstream の run 39012182949)。ping / meta はその 2 割にする
// - 遅い側 (本家) が捌ける量: 同じ run で、本家が 30 秒の区間 (平均 VU 約 29) に捌いた件数は
//   local-timeline が約 3600 件 (毎秒約 120)、home-timeline が約 2650 件 (毎秒約 88)。
//   どちらもその半分以下にする。i / users-show は k6 の頭打ちで本家の上限が分かっていない
//   (毎秒約 150 件で、本家の postgres が上限 0.75 の約 6 割) ので、100 にとどめる
//
// どちらかを超えると、benchcompare が「k6 が律速」か「落ちた iteration がある」として、その
// エンドポイントを比較不能にする。値は PR の上の A/A で見直す。base と head (本家と Elythia)
// は必ず同じ率で流す (このファイルを両側で共有する)。
export const arrivalRates = {
  // 1000 では PR #3658 の上の実行で、k6 が止められた区切りが本家・Elythia とも最大 2〜5%
  // になった (meta は応答が大きい分 ping より多い)。半分にして 2% の線から離す
  ping: 500,
  meta: 500,
  "local-timeline": 50,
  "users-show": 100,
  i: 100,
  // 書き込みも同じ率で送る。closed-loop では速い側ほど多くのノートを作り、後に流す
  // home-timeline の DB の中身が両側で違っていた。prod-like の利用者ごとの上限
  // (notes/create は 1 時間に 300、50 人に均して 15000) に対し、1 回のウォームアップと
  // 計測で (5 + 30) 秒 × 30 = 1050 件
  "notes-create": 30,
  "home-timeline": 40,
};

// 到着率のシナリオ 1 つの長さ (秒)。シナリオの間隔は SCENARIO_DURATION (31 秒) で、
// 残りの 1 秒は gracefulStop (終わりに走っている iteration を待つ上限) に使う。
export const ARRIVAL_DURATION = 30;
