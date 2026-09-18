# 制裁筛查案件台

跨境金融制裁筛查的案件工作台。接收批量筛查命中、名单版本、客户主数据与调查笔记，
按**可解释的匹配规则**聚合为案件、分配给合规官，并对误报、真阳性、重复命中、
名单回溯与升级时限给出显式状态。任何解除都必须引用复核人与证据摘要。

## 为什么这样设计

合规官每天面对三类高风险混淆，系统在数据模型层面逐一设防：

| 业务痛点 | 设计对策 |
| --- | --- |
| 姓名相似、地址缺失，手工并案会把两个客户的材料混在一起 | 聚合键以**客户主体 + 名单**为边界；地址永不参与同一性判断；无客户号的游离命中永不互相并案 |
| 案件合并会覆盖或搬移原始命中 | 事件溯源 + 仅追加：合并只新增一条 `cases.merged` 关系，原命中永远保留在最初案件上 |
| 跨时区截止、故障恢复后重复/漏发提醒 | 截止时间以 UTC 绝对时刻存储，按案件时区展示；暂停冻结 SLA；提醒按到期顺序推送，`(案件,纪元,里程碑)` 幂等，重启后重放恢复 |
| 同一人多别名同时命中 | 同一客户主数据下法定名与全部别名归入同一案件，每条命中保留实际触发匹配的原始姓名 |
| 名单撤回后重新发布 | 新版本发布即对存量客户**回溯重扫**；提供版本化新证据且案件曾以误报关闭时，案件重开并进入新的 SLA 纪元 |
| 客服权限不足却能看到原始姓名 | 角色化视图：客服看到的原始姓名、别名、规则明细、笔记、证据一律以固定占位符遮蔽 |
| 解除缺乏问责 | 仅复核人可解除；证据摘要必填；重复命中必须引用原案件；全部动作进入哈希链审计日志 |

## 匹配规则（可解释）

每条命中都带 `reasons` 列表，说明分数来源：

- `exact_name`（规范化后姓名/别名完全一致，权重 0.90）、`fuzzy_name`（Jaro–Winkler ≥ 阈值，默认 0.85，支持词序无关的 token-set）
- `date_of_birth` +0.15、`nationality` +0.05、`id_document` +0.20 为佐证加分
- `date_of_birth_veto`：双方都有生日但**冲突**时直接判定非同一人，姓名再像也不命中
- `weak_demographics`、`address_missing` 为标注项（权重 0），向合规官解释证据强弱，不改变命中结论

匹配器在 `internal/match`，可独立使用；外部筛查引擎也可通过批量接口直接送入已算分的命中。

## 状态机

```
open ──pause──▶ paused ──resume──▶ open
  │                │
  ├─ SLA 预警到期（仅提醒，状态不变）
  ├─ SLA 升级到期 ──▶ escalated ──解除──▶ 终态
  └─ 复核解除 ──▶ resolved_false_positive
               ──▶ resolved_true_positive
               ──▶ resolved_duplicate（必须引用原案件）

误报终态 ──名单重发+版本化新证据──▶ open（新提醒纪元，旧处置保留在审计历史中）
任意案件 ──合并──▶ 被吸收方 merged_away（只追加关系，操作须落到幸存根案件）
```

## 事件溯源与审计重放

所有写操作都翻译为不可变事件，追加写入 `data/events.jsonl`（路径由 `EVENT_LOG` 指定）：

- 每条记录含 `seq` 与前一条记录的 SHA-256（`prev_hash`/`hash`），形成哈希链；
- 启动时重放日志恢复全部读模型（案件、合并树、暂停累计、提醒幂等记录）；
- `GET /v1/audit/replay` 用**独立投影**重放全量日志，校验完整性并产出：
  人读时间线、案件合并边界（含原命中归属核对）、缺少复核人/证据的解除清单。

## HTTP 接口

鉴权：请求头 `X-Officer-ID`（演示用；生产应由网关注入并校验签名令牌）。
角色：`agent`（客服，只读且遮蔽）、`officer`（合规官）、`reviewer`（复核人）。

| 方法 & 路径 | 角色 | 说明 |
| --- | --- | --- |
| `POST /v1/admin/officers` | - | 登记人员与角色 |
| `PUT /v1/customers` | officer+ | 客户主数据（姓名、别名、地址、时区） |
| `POST /v1/lists/{id}/publish` | officer+ | 首次发布名单版本 |
| `POST /v1/lists/{id}/withdraw?version=n` | officer+ | 撤回版本 |
| `POST /v1/lists/{id}/republish` | officer+ | 撤回版本以新版本重发并触发回溯重扫 |
| `POST /v1/hits/batch` | officer+ | 接收外部引擎的批量命中 |
| `POST /v1/screen` | officer+ | 用内置匹配引擎对客户实时筛查 |
| `GET /v1/cases` / `GET /v1/cases/{id}` | 全部角色 | 案件列表/详情（按角色裁剪字段） |
| `POST /v1/cases/{id}/assign` | officer+ | 分配承办人 |
| `POST /v1/cases/{id}/merge` | officer+ | 追加合并关系 |
| `POST /v1/cases/{id}/pause` / `resume` | officer+ | 暂停/恢复（SLA 冻结/顺延） |
| `POST /v1/cases/{id}/notes` | officer+ | 追加调查笔记 |
| `POST /v1/cases/{id}/resolve` | **reviewer** | 解除：误报/真阳性/重复命中（证据必填） |
| `POST /v1/reminders/pump` | officer+ | 推动到期提醒（按 DueAt 升序、幂等） |
| `GET /v1/audit/replay` | **reviewer** | 完整性校验与审计时间线 |

错误码：`400` 输入非法、`401` 身份未知、`403` 角色不足、`404` 资源不存在、
`409` 状态冲突（终态/暂停中/已合并/版本状态等）。

## 运行

```bash
go run .                      # 默认 :8080，事件日志 data/events.jsonl
PORT=8080 \
EVENT_LOG=data/events.jsonl \
SLA_DURATION=24h \
SLA_WARNING_LEAD=4h \
  go run .

go test ./...                 # 匹配、聚合边界、合并、SLA、回溯、权限、审计篡改检测
```

## 代码结构

```
internal/domain   领域模型、状态机、不可变事件、错误
internal/match    姓名规范化、Jaro–Winkler、佐证加分与生日强否决
internal/store    哈希链仅追加事件日志 + 事件重放投影（案件边界/暂停/提醒幂等）
internal/service  命令处理：批量落点、聚合、合并、回溯、解除守卫、SLA 提醒泵、审计重放
internal/view     按角色裁剪的案件视图（字段级遮蔽）
internal/httpapi  JSON HTTP 接口
```
