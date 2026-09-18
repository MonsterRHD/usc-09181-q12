# 制裁筛查案件台

为跨境金融服务提供的制裁筛查案件工作台：接收批量命中、名单版本与客户主数据，
按**可解释的确定性规则**聚合为案件并分配给合规官，覆盖误报、真阳性、重复命中、
名单回溯撤回、升级时限等状态机；任何解除都必须引用复核人与证据摘要。

## 核心设计

- **事件溯源（append-only）**：所有状态变更都是追加事件；`internal/screening/events.go`。
  事件带 SHA-256 哈希链（`PrevHash/Hash`），审计重放可发现任何篡改或删除
  （`VerifyChain`）。故障恢复 = 重新加载事件流并重放，状态一致。
- **读模型投影**：`Projection` 从零 `Apply` 全部事件重建案件、名单、提醒等读状态。
- **乐观并发**：命令以"重放 → 校验 → 按期望版本追加"执行，冲突自动重试。
  名单撤回与重新发布并发处理时不会丢失更新（`ErrConflict`）。
- **文件事件日志**：`EVENT_STORE_PATH` 指向 JSON Lines 文件，追加写、启动回放并校验哈希链；
  不设置时使用内存存储。

### 案件边界与可解释匹配（`matcher.go`）

硬边界：**客户主数据 ID 不同的命中永不聚合/合并**，即使姓名完全相同、地址缺失——
这正是手工并案最容易把两个客户材料放到一起的情形；被边界隔开的相似命中对记录在
`KeptApart`（规则 `BOUNDARY_DIFFERENT_CUSTOMER`）供审计。

可合并规则（每条合并边都带规则名与细节，写入案件 `Rules`）：

| 规则 | 含义 |
|---|---|
| `SAME_CUSTOMER_ID` | 两条命中指向同一客户主数据记录 |
| `CUSTOMER_ALIAS_CHAIN` | 同一人以法定名/不同别名同时命中 |
| `NATIONAL_ID_EXACT` | 归一化证件号一致且非空 |
| `LIST_ALIAS_CHAIN` | 两个不同姓名形式经同一名单条目别名集合连通 |
| `NAME_ADDRESS_EXACT` | 归一化姓名与非空归一化地址均一致 |

### 案件合并：只追加，不覆盖

`MergeCases` 把来源案件的命中**追加**进目标案件并记录 `SourceCases`，来源案件保留、
标记 `MERGED`，原命中与审计历史绝不删除或覆盖。错聚的材料可用 `DetachHit` 分离
（同样追加分离关系，原命中保留），且必须引用复核人与证据。

### 时限、暂停与跨时区

- 截止时间在入队时换算为 **UTC 绝对时刻**；`PushDue(asOf)` 严格按发生顺序推动，
  已触发的不重推（不重不漏），崩溃后重放补偿安全。
- 暂停期间不推动任何提醒；恢复时按暂停时长整体顺延（`RemindersDeferred`），
  升级时钟不被暂停消耗。升级触发后案件自动置 `ESCALATED`。

### 处置状态与解除管控

`PENDING / FALSE_POSITIVE / TRUE_POSITIVE / DUPLICATE / LIST_WITHDRAWN_CLEARED /
ESCALATED / MERGED`。误报、真阳性、重复、回溯解除等任何解除动作：

1. 必须给出复核人（具备 `reviewer` 角色）与证据摘要；
2. 名单回溯解除仅在案件内命中所依据的名单条目**全部已撤回**时允许；
   名单重新发布（新版本）后新命中不得回溯解除。

### 权限

`agent`（客服）看不到命中原始姓名，读接口返回掩码（`John Smith → J*** S****`），
且无权执行任何处置；`officer` 可见原名并调查；`reviewer` 可作为解除的引用复核人。

## HTTP API（最小演示）

演示用 `X-Actor: officer|reviewer|agent` 头表示身份。

| 方法/路径 | 说明 |
|---|---|
| `GET  /health` | 健康检查 |
| `POST /customers` | 导入/更新客户主数据 |
| `POST /list-entries` | 导入名单条目（带版本） |
| `POST /list/{id}/withdraw` | 名单撤回（回溯） |
| `POST /list/{id}/republish` | 撤回后以新版本重新发布 |
| `POST /hits` | 批量接收命中并自动聚合建案，返回案件 ID 与可解释匹配计划 |
| `GET  /cases` | 案件列表（按角色掩码姓名） |
| `POST /cases/{id}/merge` | 追加式并案 |
| `POST /cases/{id}/assign` | 分配合规官，登记 reminder/escalation 截止时刻（RFC3339，含时区） |
| `POST /cases/{id}/suspend` / `resume` | 暂停 / 恢复（顺延提醒） |
| `POST /cases/{id}/disposition` | 记录处置结论（须 reviewer + evidence） |
| `POST /cases/{id}/retroactive-clear` | 名单回溯解除 |
| `POST /cases/{id}/detach` | 追加式分离错聚命中 |
| `POST /cases/{id}/notes` | 调查笔记 |
| `POST /push-due?asOf=...` | 按发生顺序推动到期提醒/升级 |

## 运行与测试

```bash
EVENT_STORE_PATH=./data/events.jsonl PORT=8080 go run .
go test ./...
```

测试覆盖：相似姓名不同客户的边界、同一人多别名聚合、追加式并案、跨时区截止顺序、
暂停顺延、升级状态、解除强制复核、名单撤回/重发、并发乐观锁、客服姓名掩码、
重复命中、哈希链篡改检测与文件存储故障恢复。

## 目录

```
main.go/server.go            HTTP 入口与最小 JSON API
internal/screening/
  events.go                  事件定义与 EventStore 接口
  store.go                   内存/文件存储、乐观锁、哈希链校验
  models.go                  客户、名单、命中、案件、处置状态
  matcher.go                 可解释匹配/聚合规则与案件边界
  projection.go              事件重放读模型、到期提醒调度
  service.go                 应用服务（命令、权限、并发重试）
  view.go                    按角色掩码的只读视图与审计轨迹
  names.go                   姓名/地址/证件归一化与掩码
```
