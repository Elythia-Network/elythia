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
