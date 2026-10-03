# IMAP CONDSTORE / QRESYNC 增量同步 - 产品需求文档

## Overview
- **Summary**: 为现有 IMAP 服务补齐 RFC 7162 CONDSTORE 与 QRESYNC 扩展能力：每个邮箱维护持久单调递增的最高变更序号（HIGHESTMODSEQ），每封邮件的新增、标记变更、复制/移动、删除都产生可比较的变更序号（MODSEQ），已 expunge 的 UID 留下可按同步点查询的删除历史；协议层支持 SELECT/EXAMINE 返回 HIGHESTMODSEQ、FETCH 的 CHANGEDSINCE、STORE 的 UNCHANGEDSINCE/MODIFIED 以及 QRESYNC 重选返回变化邮件与 VANISHED UID。
- **Purpose**: 移动邮件客户端离线一段时间后重新选择 Daptin 邮箱时，无需重新抓取整箱即可判断哪些邮件标记变化、哪些邮件已被删除；减少重连延迟、流量与电量消耗。
- **Target Users**: 使用 CONDSTORE/QRESYNC 感知 IMAP 客户端（移动端优先）的最终用户；多会话/多客户端协同访问同一邮箱的用户。

## Goals
- 每个邮箱有持久、单调递增、跨服务重启保留的 HIGHESTMODSEQ。
- 每封邮件维护可比较的 MODSEQ：新增、flags 变更、复制/移动、删除均产生新序号。
- 已删除（expunge）邮件的 UID + MODSEQ 形成删除历史，可按同步点查询。
- 协商扩展后：
  - SELECT/EXAMINE 返回 HIGHESTMODSEQ，STATUS 支持 HIGHESTMODSEQ；
  - FETCH/UID FETCH 支持 MODSEQ 数据项与 CHANGEDSINCE 修饰符；
  - STORE/UID STORE 支持 UNCHANGEDSINCE，冲突邮件以 MODIFIED 报告并拒绝覆盖；
  - ENABLE QRESYNC 后带 QRESYNC 参数重选邮箱，单次往返返回变化邮件（UID/FLAGS/MODSEQ）与 VANISHED (EARLIER) UID；
  - UID FETCH 支持 VANISHED 修饰符；QRESYNC 连接上 EXPUNGE 使用无 EARLIER 的 VANISHED。
- UIDVALIDITY 不匹配时要求（客户端侧）完整同步：服务器忽略 QRESYNC 增量参数并执行普通选择。
- 删除历史不足以覆盖客户端同步点时，服务器必须明确拒绝，不得声称增量结果完整。
- 覆盖所有邮件生产入口：SMTP 收信、IMAP APPEND、COPY、MOVE、STORE、EXPUNGE、JSON:API 新增邮件/修改 flags。

## Non-Goals
- 不实现 RFC 7162 之外的 IMAP 扩展（ANNOTATE、SORT/THREAD、ESEARCH、UIDPLUS UID EXPUNGE 等的新语义），但既有 MOVE/UNSELECT/IDLE 行为必须保持。
- 不提供删除历史的主动裁剪/过期策略（实现不产生无法覆盖的记录缺口；watermark 机制仅用于升级前遗留缺口）。
- 不改变 JSON:API 邮件资源的字段、状态码与读写语义（MODSEQ 等状态为内部持久化数据，可按需暴露为只读属性，但不改变现有请求/响应结构）。
- 不实现私有/共享（priv/shared）分 metadata item 的独立 modseq；每邮件单一 MODSEQ。
- 不重写 SMTP、IMAP 之外的其它协议适配器。

## Background & Context
- IMAP 服务端基于 `github.com/artpar/go-imap v1.0.5`，该 fork 不含 CONDSTORE/QRESYNC 包，但提供完整的扩展点：`server.Extension`（Capabilities/Command 工厂覆盖）、`server.ConnExtension`、ENABLE 命令解析器（`commands.Enable`）。
- 邮箱状态持久化于 SQL 数据库（SQLite/MySQL/PostgreSQL）：`mail_box`（uidvalidity、nextuid）、`mail`（uid、flags、seen/recent/deleted、mail_box_id）。
- UID 分配已有成熟跨库模式：事务内锁定 mailbox 行（SQLite 用 UPDATE 行锁，MySQL/PG 用 SELECT … FOR UPDATE）→ 读 nextuid → 写 nextuid+1（`AllocateMailBoxUid`/`lockMailboxForUidAllocation`）。
- 邮件操作核心路径：
  - 创建：`createWithoutFilter`（IMAP APPEND `CreateMessage`、COPY `CopyMessages`、SMTP 收信 `CreateInboundMailWithTransaction`、发信存 Sent `AppendSentMailForSender`、JSON:API POST 均收敛于此）；
  - flags 更新：IMAP STORE/FETCH 隐式 \Seen → `UpdateMailFlags`；JSON:API PATCH → `updateWithoutFilters`；
  - 删除：EXPUNGE（`ExpungeMailBox`）、JSON:API DELETE、邮箱级联删除均收敛于 `deleteWithoutFiltersWithData`。
- IMAP MOVE：服务端在 backend 实现 `MoveMailbox` 接口时走后端原子移动；当前 `DaptinImapMailBox` 未实现 `MoveMessages`，需补齐。
- 服务器未内置 ENABLE 命令处理器（命令表无 ENABLE），需由扩展注册。
- 协议关键语义（RFC 7162）：
  - modseq 为无符号 63 位正值；同一邮箱每次成功 STORE 必须获得不同且对同会话后完成者严格更大的值；不得直接使用系统时钟；
  - flags 实际变化才必须更新 MODSEQ；设置已存在/清除未设置的 flag 不应更新；
  - CHANGEDSINCE 隐式添加 MODSEQ 数据项；UNCHANGEDSINCE 失败邮件以 tagged `OK [MODIFIED <set>]`（或 NO）报告，成功邮件即使 .SILENT 也必须回送含 MODSEQ 的 FETCH；
  - QRESYNC 参数未经 `ENABLE QRESYNC` 正响应 → tagged BAD；QRESYNC 参数 UIDVALIDITY 不匹配 → 忽略剩余参数、执行普通 SELECT；
  - QRESYNC 连接的 EXPUNGE 必须使用无 EARLIER 的 VANISHED 并在 tagged OK 携带 HIGHESTMODSEQ；VANISHED (EARLIER) 必须先于 FETCH 返回。

## Functional Requirements
- **FR-1 邮箱 MODSEQ 状态**：`mail_box` 持久化 `highestmodseq`（63 位）；提供事务内原子的分配方法，返回严格大于当前值的新值并持久化；任何路径不得回退该值。
- **FR-2 邮件级 MODSEQ**：`mail` 持久化每邮件 `modseq`；新邮件写入时由服务器分配；flags 实际变化时更新；服务器为 modseq 的唯一权威，忽略调用方提供的 modseq。
- **FR-3 删除历史**：新增 Daptin 资源（`mail_deletion`，belongs_to `mail_box`，字段 uid、modseq）；邮件被永久删除前写入一条记录；可按 (mail_box, modseq > since) 查询并按 UID 聚合为序列集合。
- **FR-4 历史完整性水位**：`mail_box` 持久化删除历史水位（deletion_history_modseq，可空）：所有 modseq > 水位的删除记录保证完整；新邮箱水位为 0；升级前已存在、从未记录过删除历史的邮箱在首次写入删除历史时将水位设为当时的 highestmodseq；水位为空且尚未初始化时视为“当前 highestmodseq（不保证任何更早记录）”。
- **FR-5 协议通告与 ENABLE**：连接通告 `CONDSTORE` 与 `QRESYNC` 能力；提供 ENABLE 命令处理器，按 RFC 5161 返回 ENABLED 响应并在连接上下文记录已启用扩展；QRESYNC 必须且只能通过 ENABLE 激活；连接关闭时清理连接状态（有界所有权）。
- **FR-6 SELECT/EXAMINE**：普通选择行为不变；CONDSTORE 感知连接上成功 SELECT/EXAMINE 必须返回 `OK [HIGHESTMODSEQ n]`；支持 `(CONDSTORE)` 参数；STATUS 支持 HIGHESTMODSEQ 项；带 QRESYNC 参数时按 FR-8 处理。
- **FR-7 FETCH**：支持 MODSEQ 数据项（回 `MODSEQ (n)`）；CHANGEDSINCE x 仅返回 modseq > x 的邮件并自动携带 MODSEQ；UID FETCH 支持 `(CHANGEDSINCE x VANISHED)`：先返回 VANISHED (EARLIER)（仅经 ENABLE QRESYNC 时允许），再返回变化邮件。
- **FR-8 STORE UNCHANGEDSINCE**：仅对 modseq <= 值的邮件执行修改并分配新 MODSEQ；冲突邮件不修改并在 tagged 响应以 `MODIFIED <set>`（STORE 为序号集合，UID STORE 为 UID 集合）报告；成功邮件即使 SILENT 也回送含 MODSEQ（按 QRESYNC 状态含 UID）的 FETCH。
- **FR-9 QRESYNC 重选**：`ENABLE QRESYNC` 后 `SELECT/EXAMINE (QRESYNC (uidvalidity modseq [known-uid-set]) [seq-match uid-match])`：UIDVALIDITY 匹配且历史充分时，在常规选择响应之外，返回 modseq 之后的变化邮件 FETCH（至少 UID、FLAGS、MODSEQ；限制在 known-uid-set 内）与 VANISHED (EARLIER)；无变化时不产生对应响应。
- **FR-10 失效与不完整处理**：
  - QRESYNC 参数 UIDVALIDITY 与当前不匹配：忽略 QRESYNC 剩余参数，执行普通 SELECT/EXAMINE，不返回 VANISHED/变化 FETCH；客户端可通过比较 UIDVALIDITY 发现；
  - 删除历史不覆盖同步点（since <= 有效水位）或参数未经 ENABLE：不得返回“看似完整”的增量结果；前者返回 tagged NO（邮箱未被增量选择，需普通重选/完整同步），后者 tagged BAD；
  - VANISHED UID FETCH 修饰符用于非 UID FETCH、未经 ENABLE QRESYNC、或缺少 CHANGEDSINCE：tagged BAD。
- **FR-11 EXPUNGE**：QRESYNC 已启用连接上，EXPUNGE 以无 EARLIER 的 VANISHED 报告被删 UID，tagged OK 携带更新后的 HIGHESTMODSEQ；未启用连接保持 EXPUNGE 响应。
- **FR-12 MOVE**：`DaptinImapMailBox` 实现 MOVE：目标邮箱逐封获得新 UID、新 MODSEQ 与 Recent（flags/internal date 保留）；源邮件在同一事务内永久删除并产生删除历史；目标邮箱不存在时返回错误且不自动创建。
- **FR-13 入口覆盖**：SMTP 收信、IMAP APPEND、COPY、MOVE、STORE、EXPUNGE、JSON:API POST/PATCH（flags）均通过同一核心路径产生 MODSEQ/历史；无第二条写入路径。

## Non-Functional Requirements
- **NFR-1 持久化**：所有 MODSEQ/水位/删除历史状态存储于配置的 SQL 数据库，服务重启后保持且不回退；不依赖内存或 Olric 作为持久权威。
- **NFR-2 并发正确性**：多连接/多节点并发下，modseq 分配由数据库行/事务保证原子与全邮箱严格单调；并发 STORE 获得不同值。
- **NFR-3 性能**：增量同步查询应按 (mail_box_id, modseq) 索引完成，不得全箱扫描；CHANGEDSINCE/QRESYNC 请求只触碰变化行与历史行；无 N+1（批量读取沿用现有批次机制）。
- **NFR-4 跨库兼容**：在 SQLite、PostgreSQL、MySQL 上行为一致，仅复用现有锁/方言抽象，不引入库特定旁路。
- **NFR-5 向后兼容**：未协商扩展的 IMAP 客户端收到的响应与现状逐行等价（不出现 HIGHESTMODSEQ/MODSEQ/VANISHED）；JSON:API 邮件读写请求/响应行为不变。
- **NFR-6 最小改动**：不引入第二套身份、权限、邮箱/邮件模型或并行写入子系统；新生产代码与受影响文件最小化。

## Constraints
- **Technical**：Go；`github.com/artpar/go-imap v1.0.5`（只能通过其扩展接口接入，不修改依赖源码）；`api2go` 资源模型；SQL 持久化。
- **Business**：不得破坏现有移动/桌面客户端与 JSON:API 集成；上线即可对既有部署滚动升级。
- **Dependencies**：schema 同步会自动为既有库 ADD COLUMN、为新资源 CREATE TABLE；连接状态生命周期跟随 go-imap server。

## Assumptions
- 既有部署中 `mail_box` 每行可通过 `highestmodseq`（新增列默认 0）开始追踪：首次变更后为正；未被任何变更触碰的邮箱，HIGHESTMODSEQ 0 由 NOMODSEQ 之外的普通路径处理——CONDSTORE 感知选择时始终返回 HIGHESTMODSEQ（含 0 值邮箱）。
- JSON:API 直接 PATCH `mail_box_id` 的移动场景按 FR-4/FR-2 在源邮箱写删除历史、在目标邮箱分配新 MODSEQ；UID 保持不变（API 现有行为）。
- 删除历史记录由资源创建路径写入，owner 为邮箱属主，权限沿用 mail 的属主读写模型（768）。

## Acceptance Criteria

### AC-1: 邮箱 HIGHESTMODSEQ 持久且原子单调
- **Type**: `rule`
- **Given**: 任一已存在的 mail_box 行
- **When**: 在事务内调用 MODSEQ 分配（任意变更路径）
- **Then**: 返回值严格大于该行此前 highestmodseq 并已持久化；并发调用均获得互不相同、严格递增的值；重启服务后再次分配得到的值大于重启前已提交值。
- **Pass Condition**: 连续/并发分配序列严格单调；重启后不回退（测试中断言并在重启后复读）。
- **Evidence**: `server/resource` 单元测试（SQLite，含并发分配与“关闭后重开 DB”用例）。

### AC-2: 所有邮件新增入口产生邮件 MODSEQ
- **Type**: `rule`
- **Given**: 空/非空邮箱
- **When**: 分别经 SMTP 收信、IMAP APPEND、COPY、MOVE（目标箱）、JSON:API POST 创建邮件
- **Then**: 新 mail 行 modseq 为正且等于创建时邮箱新 highestmodseq；不接受客户端提供的 modseq 值。
- **Pass Condition**: 五种入口创建后，mail.modseq > 0、mail_box.highestmodseq >= mail.modseq，且两次新增间序号递增。
- **Evidence**: resource 单元测试（各入口）+ server 包协议级 E2E（APPEND/COPY/MOVE）。

### AC-3: flags 实际变化才推进 MODSEQ
- **Type**: `rule`
- **Given**: 带已知 flags/modseq 的邮件
- **When**: 经 IMAP STORE/UID STORE、FETCH 非 PEEK（隐式 \Seen）、JSON:API PATCH 修改 flags
- **Then**: flags 实际变化时邮件获得新 modseq 且邮箱 highestmodseq 前进；设置已存在 flag/清除未设置 flag 时 modseq 不变。
- **Pass Condition**: 变化后 modseq 单调增加；无变化操作前后 modseq 相等。
- **Evidence**: resource 单元测试（UpdateMailFlags 与通用 PATCH 路径）。

### AC-4: 删除产生删除历史且可按同步点查询
- **Type**: `rule`
- **Given**: 邮箱中若干邮件
- **When**: EXPUNGE（含 UID STORE/STORE 标 \Deleted 后 EXPUNGE）、JSON:API DELETE、邮箱删除级联
- **Then**: 每封被永久删除的邮件在删除前写入 mail_deletion（mail_box、uid、modseq），其 modseq 为该删除事件序号；按 `modseq > since` 查询可得到对应 UID 集合并聚合为 UID 序列集。
- **Pass Condition**: 删除后历史行数与删除邮件数一致；查询 since 取旧同步点返回全部新 UID，取高水位返回空。
- **Evidence**: resource 单元测试。

### AC-5: 协商后 SELECT/EXAMINE/STATUS 返回 HIGHESTMODSEQ
- **Type**: `rule`
- **Given**: CONDSTORE 感知连接（经 ENABLE CONDSTORE 或任一 CONDSTORE enabling 命令）
- **When**: SELECT/EXAMINE 邮箱；STATUS 请求 HIGHESTMODSEQ
- **Then**: 成功响应包含 `OK [HIGHESTMODSEQ n]`，n 等于当前持久值；STATUS 返回对应项；支持 `(CONDSTORE)` 参数。
- **Pass Condition**: 协议响应中出现/不出现该响应码与连接协商状态一致，且数值正确。
- **Evidence**: server 包协议级 E2E。

### AC-6: FETCH CHANGEDSINCE 与 MODSEQ 数据项
- **Type**: `rule`
- **Given**: 箱内存在 modseq 各异的邮件
- **When**: `FETCH/UID FETCH <set> (MODSEQ)` 或 `(...) (CHANGEDSINCE x)`
- **Then**: CHANGEDSINCE 仅返回 modseq > x 的邮件，且每条携带 `MODSEQ (n)`；MODSEQ 项返回所请求邮件的当前 modseq。
- **Pass Condition**: 返回集合恰好等于 modseq > x 的邮件（UID/序号正确），每条含正确 MODSEQ。
- **Evidence**: server 包协议级 E2E + resource 单元测试。

### AC-7: STORE UNCHANGEDSINCE 冲突拒绝并报告 MODIFIED
- **Type**: `rule`
- **Given**: 部分邮件已被另一会话修改（modseq > v）
- **When**: `STORE/UID STORE <set> ... (UNCHANGEDSINCE v) ...`
- **Then**: 冲突邮件不被修改并在 tagged OK（或 NO）以 `MODIFIED <set>` 报告（STORE 序号、UID STORE UID）；未冲突邮件被修改、获得新 MODSEQ，即使 SILENT 也回送含 MODSEQ（及按 QRESYNC 状态含 UID）的 FETCH。
- **Pass Condition**: 冲突邮件 flags 与 modseq 保持不变；MODIFIED 集合精确；其余邮件 flags/modseq 按预期更新。
- **Evidence**: server 包协议级 E2E。

### AC-8: QRESYNC 重选返回变化邮件与 VANISHED (EARLIER)
- **Type**: `rule`
- **Given**: 已 `ENABLE CONDSTORE QRESYNC` 的连接；客户端持有 (uidvalidity, since, known-uids)
- **When**: `SELECT/EXAMINE <box> (QRESYNC (uidvalidity since [known-uids]))`
- **Then**: 常规选择响应之外，返回 HIGHESTMODSEQ；仅含 modseq > since 的邮件 FETCH（至少 UID/FLAGS/MODSEQ，限制 known-uids）；VANISHED (EARLIER) 列出该窗口删除的 UID；VANISHED 先于 FETCH；无变化时无对应响应。
- **Pass Condition**: FETCH 与 VANISHED 的 UID 集合分别与独立查询结果完全一致且顺序正确。
- **Evidence**: server 包协议级 E2E。

### AC-9: UIDVALIDITY 不匹配时忽略 QRESYNC 增量参数
- **Type**: `rule`
- **Given**: 客户端提供的 QRESYNC uidvalidity 与邮箱当前值不同
- **When**: 带 QRESYNC 参数 SELECT/EXAMINE
- **Then**: 执行普通选择：返回当前 UIDVALIDITY/HIGHESTMODSEQ，但无 VANISHED、无变化 FETCH；响应信息可指出 UIDVALIDITY 不匹配。
- **Pass Condition**: 响应中无 VANISHED 与增量 FETCH；当前 UIDVALIDITY 正常返回。
- **Evidence**: server 包协议级 E2E。

### AC-10: 删除历史不足时不声称增量完整
- **Type**: `rule`
- **Given**: 邮箱有效历史水位 w（或未初始化视为 +inf），客户端同步点 since <= w
- **When**: 带 QRESYNC 参数选择，或 UID FETCH (CHANGEDSINCE since VANISHED)
- **Then**: 选择返回 tagged NO 且邮箱未被增量选择（无“完整 VANISHED”响应）；QRESYNC 参数未经 ENABLE 正响应时选择返回 tagged BAD；客户端必须普通重选并完整同步。
- **Pass Condition**: since <= w 时不存在任何声称完整的增量成功响应；since > w 时正常增量成功。
- **Evidence**: resource 单元测试（水位判定）+ server 包协议级 E2E。

### AC-11: QRESYNC 连接 EXPUNGE 使用 VANISHED
- **Type**: `rule`
- **Given**: ENABLE QRESYNC 的连接，箱内有 \Deleted 邮件
- **When**: EXPUNGE
- **Then**: 以 `VANISHED <uids>`（无 EARLIER）报告，tagged OK 携带 `HIGHESTMODSEQ`；未协商 QRESYNC 的连接仍收到逐序号 EXPUNGE。
- **Pass Condition**: 两种连接的删除响应形态各自正确，计数一致。
- **Evidence**: server 包协议级 E2E。

### AC-12: MOVE 原子语义
- **Type**: `rule`
- **Given**: 源邮箱与已存在的目标邮箱
- **When**: `MOVE/UID MOVE <set> <dest>`
- **Then**: 目标箱新邮件具有新 UID、新 MODSEQ、Recent 且保留原 flags/internal date；源邮件同事务删除并写入源箱删除历史；dest 不存在时报错且无副作用。
- **Pass Condition**: 源/目标箱邮件数、UID、MODSEQ、历史均符合预期；失败路径无部分写入。
- **Evidence**: resource 单元测试 + server 包协议级 E2E。

### AC-13: 未协商客户端与 JSON:API 完全向后兼容
- **Type**: `rule`
- **Given**: 不发送 ENABLE/扩展参数的 IMAP 客户端，以及 JSON:API 邮件调用方
- **When**: 执行 SELECT/EXAMINE、FETCH、STORE、COPY/MOVE、EXPUNGE、POST/PATCH/DELETE
- **Then**: IMAP 响应中不出现 HIGHESTMODSEQ、MODSEQ、VANISHED，其余与现状一致；JSON:API 请求字段、响应字段与状态码与现状一致。
- **Pass Condition**: 协议抓包逐行比较无新增扩展响应；JSON:API 响应结构无变化。
- **Evidence**: server 包协议级 E2E（legacy 连接）+ resource 测试。

### AC-14: 并发与集群下 MODSEQ 正确性
- **Type**: `rubric`
- **Dimension**: 多连接/多事务并发分配与使用 MODSEQ 的正确性
- **Scale**: 1-5
- **Anchors**: 1 = 出现重复/回退值或冲突覆盖；3 = 单连接严格正确、并发下偶有等待重试但无错误结果；5 = 高并发（多 goroutine 事务）下零重复、零回退、严格全序，且无长时间锁竞争
- **Pass Threshold**: >= 4
- **Evidence**: resource 并发测试（SQLite 多事务；可加 MySQL/PG E2E，按现有 real-e2e 模式可选）。

### AC-15: 架构合规与改动最小性
- **Type**: `rubric`
- **Dimension**: 方案对 Daptin 既有权威（资源路径、身份、权限、SQL 持久权威）的复用程度
- **Scale**: 1-5
- **Anchors**: 1 = 出现并行写入路径/第二套邮箱状态模型/绕过资源生命周期；3 = 路径收敛但有少量重复逻辑；5 = 所有邮件入口收敛于同一核心路径，无重复生产逻辑，新增代码集中、受影响文件最小
- **Pass Threshold**: >= 4
- **Evidence**: 代码结构、`git diff --stat`、架构守护测试（如新增 resource 层入口收敛检查）。

## Open Questions
- 无（实现中如发现现有 JSON:API PATCH 不支持 mail_box_id 变更等约束，按 Assumptions 处理并在评审中说明）。
