"""Schemathesis hooks for the /api.json check (#4118).

Loaded through SCHEMATHESIS_HOOKS (compose.yml).
"""

import schemathesis
from hypothesis import strategies as st

# /api.json は ID を `format: misskey:id` で宣言する。schemathesis はこの書式を知らないので、
# 何でも入る文字列を作り、半分以上の入力が引数の検証 (INVALID_PARAM) で弾かれていた。
# aidx の ID (16 文字の [0-9a-z]) の形で作り、検証を越えて処理まで届かせる
schemathesis.openapi.format("misskey:id", st.from_regex(r"\A[0-9a-z]{16}\Z"))
