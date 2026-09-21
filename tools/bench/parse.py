#!/usr/bin/env python3
"""ISUCON14 ベンチマーカーの標準出力(INFO/WARN/ERROR)を要約して score.json にする。

ベンチの出力はログ形式で、最後に
    msg=結果 pass=true スコア=1097 種別エラー数=map[26:1]
が出る。スコア以外にも改善の判断に効く情報が出ているので全部拾う:

  - 不満率: 「X%のライドは椅子がマッチされるまでの時間、Y%のライドは…乗車地点までに掛かる時間、
            Z%のライドは椅子の実移動時間に不満がありました」
    ユーザー満足度 -> 新規登録数 -> 負荷量 -> スコア、という連鎖の入口。
  - 新規登録数 / 離脱数
  - 最終地域情報・最終オーナー情報（売上と椅子数）
  - WARN / ERROR の内訳（200件でFAIL打ち切り）

usage: parse.py <bench.log> > score.json
"""
import json
import re
import sys
from collections import Counter

path = sys.argv[1]
lines = open(path, encoding="utf-8", errors="replace").read().splitlines()

out = {
    "pass": None,
    "score": 0,
    "error_codes": {},
    "dissatisfied": None,
    "registered": None,
    "left": 0,
    "regions": [],
    "owners": [],
    "warn_count": 0,
    "error_count": 0,
    "warnings": [],
    "errors": [],
}

warn = Counter()
err = Counter()

for ln in lines:
    m = re.search(r"msg=結果 pass=(\w+) スコア=(-?\d+) 種別エラー数=map\[([^\]]*)\]", ln)
    if m:
        out["pass"] = m.group(1) == "true"
        out["score"] = int(m.group(2))
        codes = {}
        for kv in m.group(3).split():
            k, v = kv.split(":")
            codes[k] = int(v)
        out["error_codes"] = codes
        continue

    m = re.search(r"([\d.]+)%のライドは椅子がマッチされるまでの時間、([\d.]+)%のライドはマッチされた椅子が乗車地点までに掛かる時間、([\d.]+)%のライドは椅子の実移動時間に不満", ln)
    if m:
        out["dissatisfied"] = {
            "matching": float(m.group(1)),
            "pickup": float(m.group(2)),
            "ride": float(m.group(3)),
        }
        continue

    m = re.search(r"地域内の評判によって(\d+)人、既存ユーザーの招待経由で(\d+)人が新規登録", ln)
    if m:
        out["registered"] = {"reputation": int(m.group(1)), "invitation": int(m.group(2))}
        continue

    m = re.search(r"低評価なライドによって(\d+)人が利用をやめました", ln)
    if m:
        out["left"] = int(m.group(1))
        continue

    m = re.search(r"最終地域情報 名前=(\S+) ユーザー登録数=(\d+) アクティブユーザー数=(\d+)", ln)
    if m:
        out["regions"].append({"name": m.group(1), "users": int(m.group(2)), "active": int(m.group(3))})
        continue

    m = re.search(r'最終オーナー情報 名前=("[^"]*"|\S+) 売上=(\d+) 椅子数=(\d+)', ln)
    if m:
        out["owners"].append({"name": m.group(1).strip('"'), "sales": int(m.group(2)), "chairs": int(m.group(3))})
        continue

    m = re.search(r"level=(WARN|ERROR) msg=(.*)", ln)
    if m:
        # IDなどの可変部分を潰して種類ごとに数える
        msg = re.sub(r"\b[0-9A-Z]{26}\b", "<ID>", m.group(2))
        msg = re.sub(r"\d+", "N", msg)
        (warn if m.group(1) == "WARN" else err)[msg] += 1

out["warn_count"] = sum(warn.values())
out["error_count"] = sum(err.values())
out["warnings"] = [{"count": c, "msg": m[:300]} for m, c in warn.most_common(15)]
out["errors"] = [{"count": c, "msg": m[:300]} for m, c in err.most_common(15)]
out["total_sales"] = sum(o["sales"] for o in out["owners"])
out["total_chairs"] = sum(o["chairs"] for o in out["owners"])

json.dump(out, sys.stdout, ensure_ascii=False, indent=2)
print()
