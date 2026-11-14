-- 000143 の down は **意図的に no-op**。
--
-- 列の DEFAULT は、up の前の値を記録していない (TS 版の DB では設定ファイル次第で false
-- だった) ので戻せない。true のままでも、Elythia の model (internal/model/meta.go の
-- default:true) と食い違わない。seed した行は 000135 と同じ理由で残す: Misskey TS が自分で
-- 書いた同名の行と区別できず、消すと TS 2026.11.0 以降へ繋ぎ直したときにその migration が
-- 未実行と判定されて再実行される。
SELECT 1;
