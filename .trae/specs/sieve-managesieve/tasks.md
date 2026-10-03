# Sieve 规则与 ManageSieve 服务 - 实施计划

## Task 1: 本地 fork go-sieve 并增加 vacation 扩展
- **Status**: `pending`
- **Priority**: high
- **Depends On**: None
- **Description**:
  - 将 `github.com/foxcpp/go-sieve@v0.0.0-20260523221512-9ae51b269e52` 的 lexer/parser/interp 非测试源码及 LICENSE、go.mod 复制到 `third_party/go-sieve/`（不含 cmd/、tests/、*_test.go）。
  - 在 interp 包新增 vacation（RFC 5230 + RFC 8580 :handle）：`CmdVacation`、`ActionVacation`（不取消隐式 keep）、`loadVacation`，参数含 reason（位置参数）、:days、:subject、:from、:addresses、:mime、:handle；注册到 commands 与 supportedRequires。
  - 主 go.mod 增加 `require` 与 `replace github.com/foxcpp/go-sieve => ./third_party/go-sieve`，`go mod tidy`，确保 `go build ./...` 通过。
- **Acceptance Criteria Addressed**: AC-5, AC-16
- **Test Requirements**:
  - `rule` TR-1.1: `require ["vacation"]; vacation "away";` 经 `sieve.Load` 成功；未 require 时调用 vacation 被拒绝；证据：`go test ./third_party/...`（仅新增的冒烟测试，最小）与构建结果。
  - `rule` TR-1.2: fork 中既有 interp 包可编译且其导出 API（Load/NewRuntimeData/MessageStatic/EnvelopeStatic）可用；证据：`go build ./...`。
- **Notes**: fork 代码不属于 Daptin 生产逻辑；delta 仅 vacation 相关文件。

## Task 2: 新增 sieve_script 与 vacation_track 资源 schema
- **Status**: `pending`
- **Priority**: high
- **Depends On**: None
- **Description**:
  - 在 `server/resource/columns.go` 的 `StandardTables` 增加两表：
    - `sieve_script`：name varchar(255)（label）、script text（content）、is_active bool（truefalse，默认 false）；DefaultGroups adminsGroup；归属 mail_account。
    - `vacation_track`：track_key varchar(160)（label，IsUnique，承载账户#发件人#handle 唯一键）、sender varchar(255)、handle varchar(255)、last_sent_at timestamp；mail_account 关系列；DefaultGroups adminsGroup。
  - 在 `StandardRelations` 增加：`sieve_script belongs_to mail_account`、`vacation_track belongs_to mail_account`。
- **Acceptance Criteria Addressed**: AC-1, AC-2, AC-11
- **Test Requirements**:
  - `rule` TR-2.1: 经 schema 同步（sqlite 测试库）后两表与关系列（mail_account_id）被创建；证据：`server/resource` 下新 schema 冒烟测试（可并入 Task 4 测试）。
  - `rule` TR-2.2: vacation_track.track_key 唯一约束存在，重复插入报错；证据：测试中插入重复键返回错误。

## Task 3: Sieve 校验权威与能力清单（单一来源）
- **Status**: `pending`
- **Priority**: high
- **Depends On**: Task 1
- **Description**:
  - 新增 `server/resource/sieve_validate.go`：
    - `SupportedSieveCapabilities() []string`：返回受支持扩展清单（fileinto, envelope, reject, ereject, imap4flags, variables, relational, copy, subaddress, environment, body, vacation），作为 ManageSieve SIEVE 能力公告与校验的唯一来源。
    - `ValidateSieveScript(content string) error`：以 `sieve.Load` 完整解析，返回含行列位置的错误；并断言所 require 扩展均在能力清单内。
- **Acceptance Criteria Addressed**: AC-5, AC-16
- **Test Requirements**:
  - `rule` TR-3.1: 合法脚本通过；语法错误脚本错误含行号；require 未知扩展被拒；证据：`sieve_validate_test.go`。
  - `rule` TR-3.2: 能力清单与库实际接受集合一致——清单中每项 `require "x"` 可被 Load 接受，清单外同名探针被拒；证据：`sieve_validate_test.go`（防漂移）。

## Task 4: Sieve 脚本存储 CRUD 权威
- **Status**: `pending`
- **Priority**: high
- **Depends On**: Task 2, Task 3
- **Description**:
  - 新增 `server/resource/sieve_store.go`，全部经由既有 CRUD 授权路径（参照 `CreateMailAccountBox`）：
    - `ListSieveScripts(mailAccountId, tx)`、`GetSieveScriptByName(mailAccountId, name, tx)`；
    - `CreateSieveScript(mailAccountRef, sessionUser, name, content, tx)`（重名返回错误）；
    - `DeleteSieveScript(mailAccountId, name, sessionUser, tx)`；
    - `RenameSieveScript(mailAccountId, old, new, sessionUser, tx)`（目标已存在则错）；
    - `SetActiveSieveScript(mailAccountId, name, sessionUser, tx)`：单事务先全部置 false 再按 name 置 true（name 为空仅清除）；切换前对目标内容 `ValidateSieveScript`；
    - `GetActiveSieveScript(mailAccountId, tx) (name, content, ok, err)`。
- **Acceptance Criteria Addressed**: AC-1, AC-2, AC-4, AC-5
- **Test Requirements**:
  - `rule` TR-4.1: 创建/重名/删除/重命名/获取 的成功与失败行为正确；证据：`sieve_store_test.go`。
  - `rule` TR-4.2: 多次 SetActive 后活动脚本恰为一个；SetActive 空名为 0；重开数据库连接后脚本、内容与活动状态保持；非法内容不可被激活；证据：`sieve_store_test.go`。

## Task 5: 抽取现有本地投递路径（行为保持重构）
- **Status**: `pending`
- **Priority**: high
- **Depends On**: None
- **Description**:
  - 在 `server/resource` 新增 `sieve_deliver.go`，将 `mail_adapter.go` 中本地收件人投递内联逻辑（垃圾分数→目标箱 INBOX/Spam、取/建邮箱、分配 UID、组装 mail 属性、`CreateInboundMailWithTransaction`）抽取为可复用函数：
    - `ResolveOrCreateMailBox(mailAccountId, boxName, sessionUser, tx) (box map, err)`（含账户锁与建箱）；
    - `buildInboundMailAttributes(...)` 与 `appendInboundMail(box, sessionUser, mailBytes, metadata, spamScore, flags, tx)`；
    - `DeliverInboundDefault(mailAccount, sessionUser, mailBytes, meta, spamScore, tx)`：保持现有 INBOX/Spam 分流行为。
  - `mail_adapter.go` 改为调用这些函数；外发中继路径不动。
- **Acceptance Criteria Addressed**: AC-13, AC-14
- **Test Requirements**:
  - `rule` TR-5.1: 重构后既有 `mail_functions_test.go`、`outbox_mail_server_test.go`、`mail_search_test.go`、`mail_uid_test.go` 全绿；证据：测试输出。
  - `rule` TR-5.2: 默认投递在普通邮件（INBOX）、spamScore>299（Spam 箱）、>50（Spam 标记）下结果与原先一致；证据：`sieve_deliver_test.go` 中默认路径用例。

## Task 6: Sieve 执行引擎与动作应用（含 redirect 策略）
- **Status**: `pending`
- **Priority**: high
- **Depends On**: Task 4, Task 5
- **Description**:
  - 在 `sieve_deliver.go` 实现 `DeliverInboundWithSieve(mailAccount, sessionUser, mailBytes, envFrom, spamScore, tx)`：
    - 读取活动脚本；无活动脚本 → `DeliverInboundDefault`；
    - 用 `interp.EnvelopeStatic`/`interp.MessageStatic`（textproto 头 + 原始字节，支持 body）适配；
    - 实现 redirect Policy：地址合法性、同投递去重、禁止回到 envelope from 与本人地址、跳数（Received）上限；
    - `script.Execute` 后遍历 `AppliedActions`，在同一 tx 内：
      - keep（显式/隐式）→ appendInboundMail 到 INBOX；
      - fileinto → ResolveOrCreateMailBox + appendInboundMail（同一邮件多目标各一行）；
      - redirect → 新增 `enqueueOutboxMail(from, to, content, tx)` 走现有 outbox 创建路径（内部管理员上下文，参照中继路径）；
      - discard → 不写；vacation → 见 Task 7。
  - mail_adapter 本地分支改为调用 `DeliverInboundWithSieve`。
- **Acceptance Criteria Addressed**: AC-6, AC-7, AC-8, AC-9, AC-13, AC-16
- **Test Requirements**:
  - `rule` TR-6.1: header/address/size/body 条件命中/不命中时邮箱与 outbox 结果正确；隐式 keep 入 INBOX；证据：`sieve_deliver_test.go`。
  - `rule` TR-6.2: fileinto 自动建箱；discard 无 mail 行；stop 后无后续动作；重复 redirect 只一条；回原 sender/本人地址不排队；超跳数不转发；证据：`sieve_deliver_test.go`。
  - `rule` TR-6.3: 注入失败（非法邮箱名）时整事务回滚，mail/mail_box/outbox 计数不变、UID 无推进；证据：`sieve_deliver_test.go`。

## Task 7: vacation 安全判定、持久去重与回复排队
- **Status**: `pending`
- **Priority**: high
- **Depends On**: Task 6
- **Description**:
  - 实现 vacation 执行：
    - 安全检查：列表邮件（List-Id/List-Help/List-Unsubscribe 等或 Precedence 为 bulk/list/junk）、自动邮件（Auto-Submitted 非 no、X-Autoreply/X-Autorespond）、空 envelope from → 不回复；
    - handle：显式 :handle 优先，否则由 vacation 参数（days/subject/from/addresses/mime/reason）派生稳定 hash；
    - 以 track_key = 账户#规范化发件人#handle 在 tx 内做"存在即锁定（FOR UPDATE）/不存在插入"的事务性预留：周期内不回复，周期后更新 last_sent_at；依赖 track_key 唯一约束保证并发至多一条；
    - 构造回复：MAIL FROM 空、Auto-Submitted: auto-replied、In-Reply-To/References、主题（:subject 或 Re: 原主题）、正文（:mime 处理）；`enqueueOutboxMail` 入 outbox；
    - vacation 不取消隐式 keep（原邮件仍按脚本/隐式保留）。
- **Acceptance Criteria Addressed**: AC-10, AC-11, AC-12, AC-9
- **Test Requirements**:
  - `rule` TR-7.1: 列表/自动/空地址来信零回复，普通邮件一条回复且回复字段满足格式要求；证据：`sieve_deliver_test.go`。
  - `rule` TR-7.2: 同键周期内仅一次、周期后可再回复、不同发件人/handle 分别计数；重开连接后去重仍生效；证据：`sieve_deliver_test.go`。
  - `rule` TR-7.3: 并发两个事务竞争同一 track_key，至多插入一条 track 与一条回复，落败方事务回滚且不出现部分投递；证据：`sieve_deliver_test.go`（多连接 sqlite/串行化下验证唯一键与事务结果）。

## Task 8: ManageSieve 协议服务与运行时接线
- **Status**: `pending`
- **Priority**: high
- **Depends On**: Task 4
- **Description**:
  - 新增 `server/managesieve_server.go`：
    - 自实现 RFC 5804 帧：行读取 + `{N+}` 字面量接收；能力公告（IMPLEMENTATION、SIEVE 来自 `SupportedSieveCapabilities()`、STARTTLS、AUTH/SASL PLAIN（+LOGIN）、VERSION 1.0）；TLS 前不公告 AUTH；
    - STARTTLS（CertificateManager 证书）；支持隐式 TLS（端口以约定后缀时）与否按 IMAP 模式；
    - PLAIN/LOGIN 认证：复用 `GetUserMailAccountRowByEmail` + bcrypt（与 IMAP 同样的 Olric 失败计数）；
    - 命令处理：CAPABILITY/STARTTLS/AUTHENTICATE/LOGOUT/NOOP/PUTSCRIPT（Validate 后 Create）/CHECKSCRIPT/LISTSCRIPTS（ACTIVE 标记）/GETSCRIPT（字面量返回）/SETACTIVE/DELETESCRIPT/RENAMESCRIPT；各命令在事务内完成；
    - 配置：`managesieve.enabled`（默认 true）、`managesieve.listen_interface`（默认 :4190）；`DAPTIN_DISABLE_MANAGESIEVE=true` 关闭；
    - 返回带 Close 的服务句柄，按 IMAP 模式经 goroutine + runtimeErrors 启动。
  - 在 `server/server.go` IMAP 块之后接线。
- **Acceptance Criteria Addressed**: AC-3, AC-4, AC-5, AC-15, AC-16
- **Test Requirements**:
  - `rule` TR-8.1: 黑盒 TCP 测试：CAPABILITY 内容、TLS 前 AUTH 不可用、错误凭证拒绝、正确登录；证据：`managesieve_server_test.go`。
  - `rule` TR-8.2: 全部管理命令端到端成功/失败响应正确，PUTSCRIPT 非法脚本被拒、SETACTIVE 后 LISTSCRIPTS 显示 ACTIVE，且状态持久（重开连接仍在）；证据：`managesieve_server_test.go`。
  - `rule` TR-8.3: 服务可 Close 停止接受连接；证据：`managesieve_server_test.go`。

## Task 9: 全量验证与回归
- **Status**: `pending`
- **Priority**: high
- **Depends On**: Task 7, Task 8
- **Description**:
  - 运行 `go vet ./server/... ./third_party/...`、`go test ./server/resource/ -count=1`、`go test ./server/ -count=1`、`go test ./...`（允许既有需外部环境的 E2E 按既有约定跳过）、`git diff --check`；
  - 审查 `git diff --stat`：确认每个生产改动文件均为必要，无附带重构。
- **Acceptance Criteria Addressed**: AC-13, AC-14, AC-16, AC-17
- **Test Requirements**:
  - `rule` TR-9.1: 上述命令全部通过（或跳过项有既有约定依据）；证据：命令输出记录于 Completion Evidence。
  - `rubric` TR-9.2: 架构契合度；scale 1-5；anchors 见 AC-16；threshold >= 4；证据：diff 审查结论。
  - `rubric` TR-9.3: 测试覆盖质量；scale 1-5；anchors 见 AC-17；threshold >= 4；证据：测试清单与运行结果。
