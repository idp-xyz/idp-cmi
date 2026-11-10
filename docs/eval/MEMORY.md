# 记忆评测（2026-11-09.memory）

数据集 `tests/eval/dataset_memory.json`（`gen_memory.py` 生成）：25 个人物，每人约 70 轮对话，6 个个人属性（部分后来被更新一到两次），夹杂闲聊和干扰（同事的属性），之后用多种说法提问。同一份对话有两种存法：**原样存回合**（idp-host 现在的做法，一轮一条"用户说了…助手答了…"）和**抽取成事实**（一个事实一条，被更新的旧事实关闭有效期；这里抽取是精确的，所以是上限）。

指标：当前值的记录是否排第一、是否在前 5、前 5 里是否混进了**过期的旧值或同事的属性**（越低越好）、问没说过的事时是否什么都不返回。

## hybrid-qwen3-0.6b-instruct-f0.40-b0.05-ex-qwen3.8-flash-next-iq3-xxs-p2

| 存法 | 问题类型 | 条数 | 当前值第一 | 当前值在前 5 | 前 5 含过期值 | 什么都不返回 |
|---|---|---|---|---|---|---|
| raw | single | 130 | 0.52 | 0.92 | 0.51 | — |
| raw | updated | 170 | 0.45 | 0.92 | 0.92 | — |
| raw | unknown | 50 | — | — | — | 0.04 |
| facts | single | 130 | 0.95 | 1.00 | 0.00 | — |
| facts | updated | 170 | 0.96 | 0.99 | 0.00 | — |
| facts | unknown | 50 | — | — | — | 0.34 |
| extracted | single | 130 | 0.75 | 0.92 | 0.03 | — |
| extracted | updated | 170 | 0.79 | 0.89 | 0.06 | — |
| extracted | unknown | 50 | — | — | — | 0.34 |

## hybrid-qwen3-0.6b-instruct-f0.40-b0.05-ex-qwen3.8-flash-next-iq3-xxs

| 存法 | 问题类型 | 条数 | 当前值第一 | 当前值在前 5 | 前 5 含过期值 | 什么都不返回 |
|---|---|---|---|---|---|---|
| raw | single | 130 | 0.52 | 0.92 | 0.51 | — |
| raw | updated | 170 | 0.45 | 0.92 | 0.93 | — |
| raw | unknown | 50 | — | — | — | 0.04 |
| facts | single | 130 | 0.95 | 1.00 | 0.00 | — |
| facts | updated | 170 | 0.96 | 0.99 | 0.00 | — |
| facts | unknown | 50 | — | — | — | 0.34 |
| extracted | single | 130 | 0.70 | 0.85 | 0.03 | — |
| extracted | updated | 170 | 0.71 | 0.80 | 0.20 | — |
| extracted | unknown | 50 | — | — | — | 0.50 |

## lexical-ex-qwen3.8-flash-next-iq3-xxs-p2

| 存法 | 问题类型 | 条数 | 当前值第一 | 当前值在前 5 | 前 5 含过期值 | 什么都不返回 |
|---|---|---|---|---|---|---|
| raw | single | 130 | 0.10 | 0.19 | 0.15 | — |
| raw | updated | 170 | 0.13 | 0.21 | 0.31 | — |
| raw | unknown | 50 | — | — | — | 0.08 |
| facts | single | 130 | 0.49 | 0.54 | 0.00 | — |
| facts | updated | 170 | 0.50 | 0.54 | 0.00 | — |
| facts | unknown | 50 | — | — | — | 0.88 |
| extracted | single | 130 | 0.16 | 0.16 | 0.02 | — |
| extracted | updated | 170 | 0.18 | 0.21 | 0.03 | — |
| extracted | unknown | 50 | — | — | — | 0.76 |

## lexical-ex-qwen3.8-flash-next-iq3-xxs

| 存法 | 问题类型 | 条数 | 当前值第一 | 当前值在前 5 | 前 5 含过期值 | 什么都不返回 |
|---|---|---|---|---|---|---|
| raw | single | 130 | 0.10 | 0.19 | 0.15 | — |
| raw | updated | 170 | 0.13 | 0.21 | 0.31 | — |
| raw | unknown | 50 | — | — | — | 0.08 |
| facts | single | 130 | 0.49 | 0.54 | 0.00 | — |
| facts | updated | 170 | 0.50 | 0.54 | 0.00 | — |
| facts | unknown | 50 | — | — | — | 0.88 |
| extracted | single | 130 | 0.15 | 0.15 | 0.02 | — |
| extracted | updated | 170 | 0.17 | 0.21 | 0.06 | — |
| extracted | unknown | 50 | — | — | — | 0.82 |

## lexical

| 存法 | 问题类型 | 条数 | 当前值第一 | 当前值在前 5 | 前 5 含过期值 | 什么都不返回 |
|---|---|---|---|---|---|---|
| raw | single | 130 | 0.10 | 0.19 | 0.15 | — |
| raw | updated | 170 | 0.13 | 0.21 | 0.31 | — |
| raw | unknown | 50 | — | — | — | 0.08 |
| facts | single | 130 | 0.49 | 0.54 | 0.00 | — |
| facts | updated | 170 | 0.50 | 0.54 | 0.00 | — |
| facts | unknown | 50 | — | — | — | 0.88 |

