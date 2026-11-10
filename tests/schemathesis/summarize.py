#!/usr/bin/env python3
"""Summarizes a schemathesis run (#4118) as Markdown for the job summary.

Reads the newest NDJSON report under out/report/ and lists the failures by
check and operation. Exits 1 when the run tested fewer operations than
MIN_OPERATIONS, so that a run which tested nothing does not look clean.
"""

import base64
import collections
import glob
import json
import os
import sys

# 外す一覧を除いて約 500 の口を叩く (2026-11-10)。抽出や起動が壊れて口を叩かなかった
# 実行を、失敗が無い実行と取り違えないための下限。口を絞って回すときは
# SCHEMATHESIS_MIN_OPERATIONS で下げる
MIN_OPERATIONS = int(os.environ.get("SCHEMATHESIS_MIN_OPERATIONS", "400"))

CHECK_TITLES = {
    "not_a_server_error": "5xx",
    "status_code_conformance": "宣言に無い status",
    "response_schema_conformance": "応答が宣言の型に合わない",
}


def text(b64):
    if not b64:
        return ""
    return base64.b64decode(b64).decode("utf-8", "replace")


def main():
    here = os.path.dirname(os.path.abspath(__file__))
    reports = sorted(glob.glob(os.path.join(here, "out", "report", "*.ndjson")))
    if not reports:
        print("schemathesis の報告が無い (out/report/*.ndjson)")
        return 1
    operations = set()
    failures = collections.defaultdict(dict)
    # 401/403 が出たら、検査に使う管理者が途中で締め出された (token の失効、凍結、
    # ロールのポリシー) かもしれない。外す一覧を見直す手がかりにする
    denied = collections.Counter()
    with open(reports[-1], encoding="utf-8") as f:
        for line in f:
            scenario = json.loads(line).get("ScenarioFinished")
            if not scenario:
                continue
            recorder = scenario["recorder"]
            label = recorder["label"]
            if label.startswith(("POST ", "GET ")):
                operations.add(label)
            if scenario.get("status") != "failure":
                continue
            for interaction in recorder.get("interactions", {}).values():
                status = interaction.get("response", {}).get("status_code")
                if status in (401, 403):
                    denied[status] += 1
            for case_id, checks in recorder.get("checks", {}).items():
                # stateful の段の失敗は、口ではなく "Stateful tests" の label にまとめられる。
                # どの口で落ちたかが表から読めるよう、その case の method と path で書く
                case = recorder.get("cases", {}).get(case_id, {}).get("value", {})
                case_label = f"{case['method']} {case['path']}" if case.get("method") and case.get("path") else label
                for check in checks:
                    if check.get("status") != "failure":
                        continue
                    response = recorder.get("interactions", {}).get(case_id, {}).get("response", {})
                    failure = check.get("failure_info", {}).get("failure", {})
                    message = (failure.get("message") or text(response.get("content", {}).get("$base64")))
                    failures[check["name"]].setdefault(case_label, (response.get("status_code"), message.splitlines()[0][:160] if message else ""))

    total = sum(len(v) for v in failures.values())
    print("## /api.json から入力を作る検査 (schemathesis)\n")
    print(f"叩いた口: {len(operations)}、失敗のある口と検査の組: {total}\n")
    if denied:
        print(f"401/403 の応答: {dict(denied)}。検査に使う管理者が途中で締め出されていないか、外す一覧を見直す\n")
    for name, title in CHECK_TITLES.items():
        rows = failures.get(name, {})
        print(f"<details><summary>{title} ({len(rows)})</summary>\n")
        if rows:
            print("| 口 | status | 内容 |\n|---|---|---|")
            for label, (status, message) in sorted(rows.items()):
                print(f"| `{label}` | {status} | {message.replace('|', '¦')} |")
        print("\n</details>\n")
    if len(operations) < MIN_OPERATIONS:
        print(f"叩いた口が {len(operations)} で、下限の {MIN_OPERATIONS} に届かない")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
