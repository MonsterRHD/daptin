# IMAP CONDSTORE / QRESYNC 增量同步 - 实现计划

## Task 1: 数据模型 —— 邮箱/邮件 MODSEQ 与删除历史资源
- **Status**: `completed`
- **Completion Evidence**:
  - TR-1.1 pass：`go test ./server/resource -run TestSchemaAddsModSeqColumnsAndMailDeletion -count=1` → ok；PRAGMA 断言 mail_box.highestmodseq/deletion_history_modseq、mail.modseq、mail_deletion(mail_box_id,uid,modseq) 均存在；`go build ./server/resource` 通过。
- **Priority**: high
- **Depends On**: None
- **Description**:
  - 在 `server/resource/columns.go` 的 `mail_box` 表新增：
    - `highestmodseq`：bigint（DataType `bigint`），ColumnType `value`，默认 `0`，有索引；
    - `deletion_history_modseq`：bigint，ColumnType `value`，IsNullable（NULL=未初始化），默认空。
  - 在 `mail` 表新增 `modseq`：bigint，ColumnType `value`，默认 `0`，IsIndexed。
  - 新增系统资源 `mail_deletion`（TableInfo：name 可隐藏，DefaultGroups adminsGroup）：
    - 列 `uid`（int，value）、`modseq`（bigint，value，有索引）；CompositeIndex `{mail_box_id, modseq}`；
  - 新增关系声明：`mail_deletion` belongs_to `mail_box`。
  - 不写迁移脚本：既有库由 schema 同步自动 ADD COLUMN，新资源自动 CREATE TABLE。
- **Acceptance Criteria Addressed**: AC-1, AC-2, AC-3, AC-4, AC-15
- **Test Requirements**:
  - `rule` TR-1.1: 对含 mail_box/mail 的 TableInfo 执行同步后，表中存在新列；mail_deletion 表可创建且含 (mail_box_id, uid, modseq)；证据：resource 包测试（SQLite，调用 CreateTable/同步检查）。
- **Notes**: 列命名必须与后续任务的存储键完全一致：`highestmodseq`、`deletion_history_modseq`、`modseq`；资源表名 `mail_deletion`。

## Task 2: MODSEQ 分配器与水位读取
- **Status**: `completed`
- **Completion Evidence**:
  - TR-2.1 pass：连续分配 1,2,3 且读回一致；
  - TR-2.2 pass（AC-14 rubric 自评 5/5）：8 goroutine × 独立事务并发分配 80 次，无重复、无回退、稠密覆盖 1..80，无长期锁等待；
  - TR-2.3 pass：文件 SQLite 关闭重开后分配值严格大于重启前；
  - TR-2.4 pass：水位初始未初始化，init 后=当前 highestmodseq（边界 3），重复 init 幂等；
  - 命令：`go test ./server/resource -run 'ModSeq|Watermark' -count=1` → ok。
- **Priority**: high
- **Depends On**: Task 1
- **Description**:
  - 在 mail_box DbResource 增加（新文件 `server/resource/mail_modseq.go`）：
    - `AllocateMailBoxModSeq(mailBoxId int64, tx *sqlx.Tx) (int64, error)`：事务内锁定 mailbox 行（复用/抽出 `lockMailboxForUidAllocation` 的同一行锁）→ 读 highestmodseq → highestmodseq+1 写回 → 返回新值；tx 为 nil 报错；
    - `GetMailBoxHighestModSeq(mailBoxId int64, tx *sqlx.Tx) (int64, error)`：只读，tx 可空；
    - `GetDeletionHistoryWatermark(mailBoxId int64, tx) (int64, bool, error)`：返回 (值, 已初始化, err)；未初始化时调用方可按 +inf 处理；
    - `initDeletionHistoryWatermark(mailBoxId int64, tx)`：若行为空，则将水位设为当前 highestmodseq（分配前状态）并持久化。
  - modseq 不使用时钟；严格以数据库行为权威。
- **Acceptance Criteria Addressed**: AC-1, AC-14
- **Test Requirements**:
  - `rule` TR-2.1: 连续分配得到 1,2,3…，持久化后读回一致；证据：单元测试。
  - `rule` TR-2.2: 多 goroutine 各开事务并发分配 N 次，结果无重复、无回退、覆盖 1..N（或严格全序）；证据：并发单元测试。
  - `rule` TR-2.3: 提交后关闭 sqlx DB 再重开同一文件库，分配值大于重启前最大值；证据：单元测试（文件 SQLite）。
  - `rule` TR-2.4: 水位初始未初始化；init 后值 = init 时 highestmodseq，重复 init 不改变；证据：单元测试。

## Task 3: 资源创建路径接入 MODSEQ
- **Status**: `completed`
- **Completion Evidence**:
  - TR-3.1 pass：经 createWithoutFilter（afterAuthorization 路径）创建 mail，首封 modseq=1 = mail_box.highestmodseq，第二封=2；
  - TR-3.2 pass：两次创建严格递增；客户端提供 modseq=999 被服务器覆盖为 3；
  - resource 包全量测试 `go test ./server/resource -count=1` → ok，无回归；
  - 命令：`go test ./server/resource -run CreateWithoutFilter -count=1` → ok。
- **Priority**: high
- **Depends On**: Task 2
- **Description**:
  - 在 `server/resource/resource_create.go` 的 `createWithoutFilter` 中，于列循环之后、INSERT 组装前增加 mail 专属最小逻辑：
    - 当表为 `mail` 且 dataToInsert 中 mail_box_id 解析为有效内部 id：调用 `AllocateMailBoxModSeq`，将结果写入/覆盖 dataToInsert、colsList、valsList 的 `modseq`（忽略客户端任何 modseq 输入）；
    - mail_box_id 为空/无效：跳过（不属任何邮箱的退化行，不参与 IMAP 同步）。
  - 不改动其它表、不改变 uid 分配行为。
- **Acceptance Criteria Addressed**: AC-2, AC-15
- **Test Requirements**:
  - `rule` TR-3.1: 经 createWithoutFilter（afterAuthorization 路径）创建 mail 后，mail.modseq 为正且 = mail_box.highestmodseq；证据：单元测试。
  - `rule` TR-3.2: 两次创建 modseq 严格递增；客户端在 attrs 中提供 modseq=999 被忽略；证据：单元测试。

## Task 4: flags 更新路径（UpdateMailFlags 与通用 PATCH）接入 MODSEQ
- **Status**: `completed`
- **Completion Evidence**:
  - TR-4.1 pass：UpdateMailFlags 实际加 \Answered 后 modseq 前进、flags 正确；
  - TR-4.2 pass：JSON:API PATCH flags 变化 → modseq 前进；flags 集合相等（同值不同序）→ modseq 不变；UpdateMessagesFlags 未变化邮件在调用侧跳过（FlagsEqual）；
  - TR-4.3 pass：API 改 mail_box_id：源箱 mail_deletion 含 uid 42、目标箱 mail.modseq=目标 highestmodseq、UID 保持 42；
  - resource 全包 `go test ./server/resource -count=1` → ok。
- **Priority**: high
- **Depends On**: Task 2
- **Description**:
  - `dbresource.go` 的 `UpdateMailFlags`：构建 patch model 前调用 `AllocateMailBoxModSeq`，将 `modseq` 加入属性；仅当 newFlags 与当前 flags 规范化后不同时才分配/更新（调用方 UpdateMessagesFlags 已有新旧 flags；在 `UpdateMessagesFlags` 内先比较，跳过无变化邮件）。
  - `resource_update.go` 的 `updateWithoutFilters`：mail 表且 allChanges 含 `flags` 时，比较规范化旧/新 flags：
    - 不同 → 分配 modseq 并加入 SET；
    - 相同 → 不加入 modseq（flags 同值重写不推进）。
  - mail 表且 allChanges 含 `mail_box_id`（API 移动）：
    - 源邮箱：确保水位初始化 → 分配 modseq → 写 mail_deletion（取有效 UID：COALESCE(NULLIF(uid,0),id 映射的对外值））；
    - 目标邮箱：分配 modseq 并将 mail.modseq 置为该值；UID 不变。
- **Acceptance Criteria Addressed**: AC-3, AC-15
- **Test Requirements**:
  - `rule` TR-4.1: flags 实际变化后 modseq/highestmodseq 前进且值正确；证据：单元测试（UpdateMailFlags 与 UpdateMessagesFlags）。
  - `rule` TR-4.2: 设置已存在 flag / 无变化 PATCH 前后 modseq 相等；证据：单元测试。
  - `rule` TR-4.3: API 改 mail_box_id 后：源箱出现删除历史、目标箱 mail.modseq 为新分配值、UID 不变；证据：单元测试。

## Task 5: 资源删除路径写入删除历史
- **Status**: `completed`
- **Completion Evidence**:
  - TR-5.1 pass：两邮件删除后 mail_deletion 行一一对应，删除 modseq 为 3,4；
  - TR-5.2 pass：deletionUIDsSince 旧 since 返回全部有效 UID，高 since 返回空；DeletionUIDsInSet 交集正确；
  - TR-5.3 pass：遗留箱（NULL 水位）首次删除水位=删除前 highestmodseq=1，删除事件 modseq=2 且 since=水位时可查到；新箱经创建路径水位=0；
  - resource 全包 `go test ./server/resource -count=1` → ok。
- **Priority**: high
- **Depends On**: Task 2, Task 1
- **Description**:
  - 在 `server/resource/resource_delete.go` 的 `deleteWithoutFiltersWithData` 开头（审计逻辑之前）增加 mail 专属逻辑：
    - 表为 mail 且 data 含 mail_box_id：
      1. `initDeletionHistoryWatermark`（NULL 时先落水位 = 当前 highestmodseq）；
      2. `modseq := AllocateMailBoxModSeq(...)`；
      3. 经 mail_deletion 资源 `createAfterAuthorizationWithTransaction` 写入 `{mail_box_id(引用), uid(有效 UID), modseq}`，请求上下文复用入参 req（owner 由核心路径按活动用户落库）。
  - 提供内部 helper：`recordMailDeletion(mailBoxId int64, uid uint32, modseq int64, req api2go.Request, tx *sqlx.Tx) error` 与 `deletionUIDsSince(mailBoxId int64, since int64, tx) (*imap.SeqSet, error)`（聚合查询 mail_deletion，按 uid 排序生成区间）。
- **Acceptance Criteria Addressed**: AC-4, AC-10
- **Test Requirements**:
  - `rule` TR-5.1: 经 deleteAfterAuthorization 删除后，mail_deletion 新增行 (box, uid, modseq) 与被删邮件一一对应，modseq 为删除事件序号；证据：单元测试。
  - `rule` TR-5.2: `deletionUIDsSince`：旧 since 返回全部新 UID（区间聚合正确），高 since 返回空集；证据：单元测试。
  - `rule` TR-5.3: 升级前遗留邮箱首次删除时水位 = 删除前 highestmodseq，历史行 modseq > 水位；新邮箱（水位 0）首次删除不抬水位；证据：单元测试。

## Task 6: 邮箱增量查询能力与 STATUS HIGHESTMODSEQ
- **Status**: `completed`
- **Completion Evidence**:
  - TR-6.1 pass：FetchChanged(since=1) 恰好返回 modseq 4,3 的两封，r1(modseq1) 排除，每条 Items 含正确 MODSEQ；显式 (MODSEQ) 请求也返回；
  - TR-6.2 pass：VanishedSince 在 since>=水位 时 sufficient=true 且 UID 集正确；since<水位 sufficient=false；knownUids 交集正确（无相关 UID 时为空）；
  - TR-6.3 pass：ConditionalStoreFlags 冲突邮件（modseq>value）不改动并按序号/UID 进入 modified；未冲突邮件更新；无实际 flags 变化时不推进；重复选中不重复判定；
  - TR-6.4 pass：Status(HIGHESTMODSEQ) Items 返回正确值；
  - resource 全包 `go test ./server/resource -count=1` → ok。
- **Priority**: high
- **Depends On**: Task 3, Task 4, Task 5
- **Description**:
  - 在 `DaptinImapMailBox` 增加导出方法（`server/resource/imap_mailbox_modseq.go`）：
    - `HighestModSequence() (int64, error)`；
    - `FetchChanged(uid bool, seqSet *imap.SeqSet, items []imap.FetchItem, since int64, ch chan<- *imap.Message) error`：在候选解析基础上以 modseq > since 过滤；构造消息时按 items 填充并在 Items 注入 MODSEQ（`msg.Items["MODSEQ"] = []interface{}{modseq}`）；
    - `VanishedSince(since int64, knownUids *imap.SeqSet) (*imap.SeqSet, bool, error)`：返回 (UID 集, 历史是否充分)；充分条件 = 有效水位 <= since；knownUids 非空时交集；
    - `ConditionalStoreFlags(uid bool, seqSet, op, flags, unchangedSince) (modified *imap.SeqSet, err error)`：逐邮件比较 modseq：冲突加入 modified 且不动；未冲突按新 flags 更新（走 Task 4 路径）。
  - `Status()`：支持 `HIGHESTMODSEQ` 状态项；`ListMessages`：items 含 MODSEQ 时注入对应值。
- **Acceptance Criteria Addressed**: AC-5, AC-6, AC-7, AC-8, AC-10
- **Test Requirements**:
  - `rule` TR-6.1: FetchChanged 返回邮件集合 = 候选中 modseq > since 者，且每条 Items 含正确 MODSEQ；证据：单元测试。
  - `rule` TR-6.2: VanishedSince 在水位 <= since 时充分标志为 true 且 UID 集与历史查询一致；knownUids 交集正确；不充分时 false；证据：单元测试。
  - `rule` TR-6.3: ConditionalStoreFlags：冲突邮件 flags/modseq 不变并入 modified，其余更新；证据：单元测试。
  - `rule` TR-6.4: Status(items=[HIGHESTMODSEQ]) 返回正确值；证据：单元测试。

## Task 7: IMAP MOVE 后端实现
- **Status**: `completed`
- **Completion Evidence**:
  - TR-7.1 pass：MOVE 后目标箱 1 封：新 UID=1、正 modseq、flags=Recent+保留 Seen；源箱 0 封且源箱 mail_deletion 含原有效 UID；
  - TR-7.2 pass：dest 不存在 → 返回错误，源箱邮件不变；
  - resource 全包 `go test ./server/resource -count=1` → ok。
- **Priority**: medium
- **Depends On**: Task 6
- **Description**:
  - `DaptinImapMailBox` 实现 `MoveMessages(uid bool, seqSet *imap.SeqSet, dest string) error`：
    - 同一事务：解析目标 box（不存在直接报错，不创建）→ 对每封源邮件按 COPY 路径在目标箱创建（新 UID 来自 AllocateMailBoxUid、Recent、保留 flags/internal date，modseq 由创建路径分配）→ 源邮件经删除核心路径删除（删除历史随之产生）；
    - 任一步失败回滚，无部分写入。
- **Acceptance Criteria Addressed**: AC-12, AC-2
- **Test Requirements**:
  - `rule` TR-7.1: MOVE 后目标箱邮件具新 UID/正 modseq/Recent，源箱减少且源箱 mail_deletion 有对应历史；证据：单元测试。
  - `rule` TR-7.2: dest 不存在时返回错误且源箱、目标状态不变；证据：单元测试。

## Task 8: IMAP CONDSTORE/QRESYNC 扩展骨架（能力/ENABLE/连接状态）
- **Status**: `pending`
- **Priority**: high
- **Depends On**: Task 6
- **Description**:
  - 新文件 `server/imap_condstore_extension.go`（package server）：
    - `condStoreExtension` 实现 `server.Extension` + `server.ConnExtension`：Capabilities 固定返回 `CONDSTORE`、`QRESYNC`；
    - `NewConn(c)` 返回 `condStoreConn{Conn: c}`（嵌入接口，同包提升私有方法），仅覆盖 `Close()`：删除该连接状态后委托原 Close；
    - 包内连接状态表：`sync.Map`（key=server.Conn，值=`*imapConnState{enabled map[string]bool, condstoreAware bool}`）；提供 get/mark 辅助；
    - ENABLE 命令处理器：Parse 复用 `commands.Enable`；Handle：仅在认证后、非 selected 状态接受（RFC 5161）；只启用服务器支持的扩展（CONDSTORE/QRESYNC），返回 untagged `ENABLED (<caps>)` 并记录状态；不支持的参数忽略并在 ENABLED 中不列出；
    - Command 工厂按 SELECT/EXAMINE/ENABLE/FETCH/STORE/EXPUNGE 返回处理器（处理器在 Task 9 实现）。
- **Acceptance Criteria Addressed**: AC-5, AC-13, AC-15
- **Test Requirements**:
  - `rule` TR-8.1: CAPABILITY 输出含 CONDSTORE、QRESYNC；ENABLE CONDSTORE QRESYNC 返回 `ENABLED (CONDSTORE QRESYNC)` 且连接状态被标记；证据：协议 E2E（Task 10）或本任务最小服务测试。
  - `rule` TR-8.2: 连接关闭后连接状态被清除（状态表中无该 key）；证据：单元/协议测试。
  - `rule` TR-8.3: ENABLE 含未知扩展时仅回显已知项；未认证时 ENABLE 被拒；证据：协议测试。

## Task 9: SELECT/EXAMINE、FETCH、STORE、EXPUNGE 协议处理器
- **Status**: `pending`
- **Priority**: high
- **Depends On**: Task 8
- **Description**:
  - 新文件 `server/imap_condstore_commands.go`（package server），实现：
    - 响应写类型：`highestModSeqResp`（`* OK [HIGHESTMODSEQ n]`）、`vanishedResp`（`* VANISHED [(EARLIER)] <set>`）；
    - SELECT/EXAMINE 处理器：在标准选择流程基础上解析可选 `(CONDSTORE)` 与 `(QRESYNC (uidvalidity modseq [uidset]))` 参数：
      - 未 ENABLE QRESYNC 却带 QRESYNC 参数 → tagged BAD；
      - UIDVALIDITY 不匹配 → 普通选择（无 VANISHED/增量 FETCH）；
      - 历史不充分（VanishedSince 充分性 false，即 since <= 水位）→ tagged NO，邮箱未被增量选择；
      - 正常：写常规 Select + HIGHESTMODSEQ；先写 VANISHED (EARLIER)（knownUids 交集），再写变化邮件 FETCH（UID/FLAGS/MODSEQ）；
      - CONDSTORE 参数或任何 enabling 命令标记连接 condstoreAware；HIGHESTMODSEQ 仅在该状态下发送；
    - FETCH 处理器：解析 seqSet/items/宏 + 可选 CHANGEDSINCE 与 VANISHED 标记：
      - VANISHED 仅 UID、须与 CHANGEDSINCE 共存、须 ENABLE QRESYNC，否则 BAD；
      - CHANGEDSINCE：先走 VANISHED (EARLIER)（若要求），再 FetchChanged；MODSEQ 自动携带；
      - items 含 MODSEQ：ListMessages 注入 MODSEQ；
    - STORE 处理器：解析 item（含 .SILENT）、值、可选 `(UNCHANGEDSINCE n)`（位置容错）；走 ConditionalStoreFlags；modified 非空时 tagged `OK [MODIFIED <set>]`（UID STORE 为 UID 集）；成功邮件回送含 MODSEQ（QRESYNC 状态含 UID）的 FETCH——即使 SILENT；
    - EXPUNGE 处理器：QRESYNC 启用连接 → ExpungeMailBox 后按本次删除 UID 写无 EARLIER VANISHED + tagged OK 带 HIGHESTMODSEQ；否则保持标准 EXPUNGE 逐序号流程。
  - 在 `server/endpoint_imap.go` 的 InitializeImapResources 中 `imapServer.Enable(&condStoreExtension{})`。
- **Acceptance Criteria Addressed**: AC-5, AC-6, AC-7, AC-8, AC-9, AC-10, AC-11, AC-13
- **Test Requirements**:
  - `rule` TR-9.1: 各命令协议形态符合 RFC 7162（响应码/修饰符/顺序：VANISHED EARLIER 先于 FETCH）；证据：Task 10 协议 E2E。
  - `rule` TR-9.2: 未协商连接下所有命令不出现扩展响应且原行为保持；证据：Task 10 legacy 用例。

## Task 10: 协议级端到端测试
- **Status**: `pending`
- **Priority**: high
- **Depends On**: Task 9, Task 7
- **Description**:
  - 新文件 `server/imap_condstore_e2e_test.go`（package server）：
    - 以 SQLite 内存库按系统 TableInfo 引导 mail_account/mail_box/mail/mail_deletion 等表与 CRUD 图（复用 resource 包内测试构造模式或最小引导函数），构建 go-imap `server.New` + 本扩展；
    - 使用裸 net.Conn + 文本 IMAP 会话断言，或 go-imap client；
    - 场景：
      1. CAPABILITY 含 CONDSTORE/QRESYNC；
      2. ENABLE CONDSTORE QRESYNC → ENABLED；
      3. APPEND 后 SELECT 返回 HIGHESTMODSEQ；未协商 SELECT 无该码；
      4. UID FETCH (MODSEQ) / CHANGEDSINCE 精确返回；
      5. 双会话：另一会话改 flags 后，UID STORE UNCHANGEDSINCE → MODIFIED，冲突邮件不变；
      6. QRESYNC SELECT：变化 FETCH + VANISHED EARLIER 集合精确且顺序正确；
      7. UIDVALIDITY 不匹配 → 普通选择无增量；
      8. 历史水位场景（手工置水位）→ tagged NO；
      9. QRESYNC 连接 EXPUNGE → VANISHED + HIGHESTMODSEQ；legacy 连接 → EXPUNGE；
      10. UID MOVE：源/目标 UID/MODSEQ/历史；
      11. STATUS (HIGHESTMODSEQ)；
      12. UID FETCH (CHANGEDSINCE x VANISHED)；
      13. JSON:API POST/PATCH 邮件后，IMAP 侧可见 modseq/变化；
      14. legacy 全程无扩展响应。
- **Acceptance Criteria Addressed**: AC-2, AC-5, AC-6, AC-7, AC-8, AC-9, AC-10, AC-11, AC-12, AC-13
- **Test Requirements**:
  - `rule` TR-10.1: 上述 14 个场景全部断言通过；证据：`go test ./server -run CondStoreQResync -count=1` 输出。
  - `rubric` TR-10.2: 测试覆盖质量；scale 1-5；anchors 1=仅覆盖快乐路径、3=主要扩展命令有正反用例、5=每个协议特性含协商/未协商、成功/冲突、完整/不完整对偶；threshold >= 4；证据：用例清单与断言。

## 依赖顺序总览
Task 1 → Task 2 → Task 3/4/5（可并行于不同文件，但 4/5 共用 Task 2）→ Task 6 → Task 7 与 Task 8 → Task 9 → Task 10。

## 验收标准覆盖矩阵
| AC | 覆盖任务 |
|---|---|
| AC-1 | Task 2 (+Task 1) |
| AC-2 | Task 3, Task 7, Task 10 |
| AC-3 | Task 4, Task 10 |
| AC-4 | Task 5, Task 10 |
| AC-5 | Task 6, Task 9, Task 10 |
| AC-6 | Task 6, Task 9, Task 10 |
| AC-7 | Task 6, Task 9, Task 10 |
| AC-8 | Task 6, Task 9, Task 10 |
| AC-9 | Task 9, Task 10 |
| AC-10 | Task 5, Task 6, Task 9, Task 10 |
| AC-11 | Task 9, Task 10 |
| AC-12 | Task 7, Task 10 |
| AC-13 | Task 8, Task 9, Task 10 |
| AC-14 | Task 2 |
| AC-15 | Task 1, Task 3, Task 4, Task 8（全程） |
