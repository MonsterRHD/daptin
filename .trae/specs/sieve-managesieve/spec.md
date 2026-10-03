# Sieve 规则与 ManageSieve 服务 - 产品需求文档

## Overview
- **Summary**: 为 Daptin 内置邮件系统的每个邮件账户增加持久化的 Sieve（RFC 5228）规则存储、ManageSieve（RFC 5804）管理服务，以及 SMTP 本地收信时在写入邮箱前执行活动脚本的投递能力（含 vacation 自动回复）。
- **Purpose**: 让用户在邮件到达服务器时即按自己的规则归档/处理邮件，并能从标准邮件客户端（任何 ManageSieve 客户端）远程管理这些规则；规则、活动状态与 vacation 去重状态在服务重启后保持不变。
- **Target Users**: 使用 Daptin 内置邮箱（SMTP 收信 + IMAP 访问）的终端用户及其邮件客户端。

## Goals
- 每个邮件账户拥有多个持久化命名 Sieve 脚本，且同一账户至多一个活动脚本；活动脚本的切换是原子的。
- 提供 ManageSieve 服务：认证用户可上传（PUTSCRIPT）、检查（CHECKSCRIPT）、列出（LISTSCRIPTS）、获取（GETSCRIPT）、重命名（RENAMESCRIPT）、删除（DELETESCRIPT）、原子切换唯一活动脚本（SETACTIVE）。
- 语法错误或使用服务器不支持的扩展/能力的脚本，必须在激活之前被拒绝（上传时即校验，激活时再次防御性校验）。
- SMTP 接收本地邮件时，在写入邮箱前执行收件人的活动脚本：
  - 条件：基于邮件头（header）、地址（address）、大小（size）、正文（body）；
  - 动作：keep、fileinto、redirect、discard、stop、vacation；
  - 没有命中终止投递的动作时，仍隐式保留到 INBOX；
  - fileinto 使用该账户自己的邮箱并在需要时安全创建；
  - redirect 进入现有外发队列（outbox），不得形成重复转发或回送循环。
- 脚本执行、目标邮箱写入、原邮件保留/丢弃、待发送转发形成一致结果：处理失败时不留下部分投递。
- vacation 自动回复避开列表邮件、自动回复邮件和空退信地址；按发件人和规则（vacation handle）在设定周期内持久去重；并发收信或服务重启均不重复回复。

## Non-Goals
- 不替换、不改动现有 IMAP 邮箱操作、普通 JSON:API 邮件读写、外发邮件发送链路与垃圾邮件打分实现。
- 不实现 Sieve 的全部扩展（如 notify、include、vacation-seconds 的秒级周期等）；仅支持所选用 Sieve 库内置扩展及新增的 vacation。
- 不提供用户自定义配额（ManageSieve quota 响应不设置上限）。
- 不改变认证身份模型：ManageSieve 与 SMTP/IMAP 使用同一 mail_account 密码权威，不引入第二套用户模型。

## Background & Context
- SMTP 接收入口：`server/smtp_server.go` 启动 go-guerrilla，处理器 `server/mail_adapter.go` 逐收件人处理：
  - 本地账户：DKIM 打分为垃圾邮件（>299 投递 Spam 箱，>50 加 Spam 标记），取/建目标邮箱、分配 UID、在单个 sqlx 事务内创建 `mail` 行后提交；
  - 非本地地址：要求已认证登录，DKIM 签名后把邮件写入 `outbox`（现有外发队列），并在 Sent 留存。
- 持久化权威：`mail_account`、`mail_box`、`mail`、`outbox` 均为 `StandardTables` 中的 Daptin 资源，关系定义在 `StandardRelations`（`server/resource/columns.go`）。
- IMAP 服务：`server/endpoint_imap.go` + `server/resource/imap_*.go`，经 mail_account + bcrypt 登录；ManageSieve 必须复用同一登录权威。
- Go 生态中可用的 Sieve 解释器为 `github.com/foxcpp/go-sieve`（MIT，含 lexer/parser/interp，支持 fileinto、envelope、body、reject、variables 等），但其不支持 vacation，也不含 ManageSieve 服务端。方案：将该库以本地模块方式置于 `third_party/go-sieve`（经 `replace` 引用），在其中以最小增量加入 vacation 命令/动作；ManageSieve 协议在本仓库内实现。库自带 `interp.MessageStatic`（含 body 支持）与 `interp.EnvelopeStatic` 可直接复用。

## Functional Requirements
- **FR-1（脚本存储）**：每个邮件账户可创建/持有多个命名 Sieve 脚本；脚本内容为文本；脚本归属邮件账户。重新创建连接（模拟重启）后脚本与内容仍在。
- **FR-2（唯一活动脚本）**：每个邮件账户至多一个活动脚本；SETACTIVE 以单事务原子切换；SETACTIVE 空名取消活动；重启后活动状态保持不变。
- **FR-3（ManageSieve 认证）**：服务以邮件账户名 + 同一 bcrypt 密码认证；未认证连接只能执行 CAPABILITY/STARTTLS/AUTHENTICATE/LOGOUT；认证失败不授权任何管理操作。
- **FR-4（ManageSieve 管理命令）**：支持 CAPABILITY、STARTTLS、AUTHENTICATE、LOGOUT、PUTSCRIPT、CHECKSCRIPT、LISTSCRIPTS、GETSCRIPT、SETACTIVE、DELETESCRIPT、RENAMESCRIPT、NOOP；响应码与行为遵循 RFC 5804（如重名 PUTSCRIPT 返回 SCRIPTEXISTS，GETSCRIPT 不存在返回 NO）。
- **FR-5（上传/激活校验）**：PUTSCRIPT 与 CHECKSCRIPT 对脚本做完整语法与能力校验；语法错误（含行/列位置信息）或 require 了不支持的扩展一律拒绝；SETACTIVE 在切换前再次校验脚本内容；非法脚本永远不会成为活动脚本。
- **FR-6（收信执行）**：SMTP 收到本地收件人邮件时，在同一投递事务内、写入任何邮箱之前执行其活动脚本；header/address/size/body 条件按 RFC 5228/5173 语义判定（含 :is/:contains/:matches、:all/:any、allof/anyof/not、地址 localpart/domain 部分、comparator）。
- **FR-7（动作语义）**：
  - keep：保留到 INBOX；fileinto：保留到该账户的指定邮箱，邮箱不存在时安全创建；
  - discard：不保留；stop：终止脚本后续执行；
  - redirect：以外发队列条目（outbox）排队；同一投递中重复目的地址只排队一次；
  - 脚本未取消隐式保留时，隐式 keep 到 INBOX。
- **FR-8（循环防护）**：redirect 不得回到原 envelope 发件人、不得指向收件人本人账户地址；单脚本 redirect 数量受上限约束（默认 5）；邮件跳数（Received）超限时拒绝转发。
- **FR-9（投递原子性）**：脚本执行、所有目标邮箱 mail 行、fileinto 所需邮箱创建、redirect/vacation 的 outbox 行、vacation 去重记录全部在同一数据库事务内；任一步失败即回滚，SMTP 返回临时错误（4xx），不得出现部分投递。
- **FR-10（vacation 安全条件）**：vacation 对列表邮件（List-Id/List-Help 等或 Precedence: bulk/list/junk）、自动/批量产生邮件（Auto-Submitted 非 no、X-Autoreply/X-Autorespond）以及空 envelope sender 不发送自动回复。
- **FR-11（vacation 去重）**：以（收件账户、规范化发件人、vacation handle）为键持久记录上次回复时间；:days 周期内不重复回复，周期过后可再次回复；无显式 :handle（RFC 8580）时由 vacation 参数派生稳定 handle；并发投递在事务+唯一键约束下最多产生一条回复；重启不丢失去重记录。
- **FR-12（vacation 回复内容）**：回复 MAIL FROM 为空（<>），含 Auto-Submitted: auto-replied、In-Reply-To/References、原邮件主题（或 :subject）、原因正文（:mime 时按 MIME 处理）；回复经 outbox 发送。
- **FR-13（回归保持）**：未配置活动脚本的账户收信路径与现状完全一致；IMAP 邮箱操作、JSON:API mail/outbox 读写、垃圾邮件分流（Spam 箱与 Spam 标记）维持现有行为。

## Non-Functional Requirements
- **NFR-1（一致性/持久性）**：脚本、活动状态、vacation 去重记录均以配置的 SQL 数据库为唯一持久权威（兼容 SQLite/MySQL/PostgreSQL），不使用内存映射作为持久状态。
- **NFR-2（安全）**：认证先于一切管理/投递决策；拒绝发生在外部副作用之前；用户输入不能选择内部身份或权限模式；vacation/redirect 不产生回环放大。
- **NFR-3（可组合性）**：所有投递/排队均经由既有 DbResource 生命周期与 outbox 路径，不新增影子映射表、第二套身份或并行投递实现。
- **NFR-4（可运维性）**：ManageSieve 服务按现有运行时生命周期启动、上报错误并可关闭；默认端口 4190（STARTTLS），可用环境变量/配置关闭。

## Constraints
- **Technical**：Go；复用 `github.com/foxcpp/go-sieve`（本地 fork 于 `third_party/go-sieve`，新增 vacation）；新表经 `StandardTables/StandardRelations` 走既有 schema 同步；ManageSieve 认证复用 mail_account bcrypt。
- **Business**：不破坏任何现有邮件相关行为与公开 API。
- **Dependencies**：go-sieve fork；go-message（已在依赖）；证书由现有 CertificateManager 提供。

## Assumptions
- ManageSieve 默认启用（配置键 `managesieve.enabled`，默认 true），监听 `:4190`，提供 STARTTLS，认证仅在 TLS 后可用；可用 `DAPTIN_DISABLE_MANAGESIEVE=true` 关闭。
- 当账户存在活动脚本时，邮箱目标由脚本动作决定；垃圾邮件分数与 Spam 标记等元数据仍记录在存储的邮件上，但隐式/显式 keep 的目标为 INBOX（RFC 5228）。无活动脚本账户的 Spam 箱分流保持不变。
- 活动脚本在每次投递时从数据库读取（解析）；解析失败按投递失败处理（临时错误），不退回旧路径静默投 INBOX。

## Acceptance Criteria

### AC-1: 脚本持久化且重启后保持
- **Type**: `rule`
- **Given**: 某邮件账户已通过存储接口创建命名脚本
- **When**: 关闭并重新打开数据库连接（模拟服务重启）后读取该账户脚本
- **Then**: 脚本仍存在且名称、内容完全一致
- **Pass Condition**: 重开连接后脚本列表与内容与提交时一致
- **Evidence**: `server/resource/sieve_store_test.go` 中持久化测试

### AC-2: 唯一活动脚本原子切换并持久
- **Type**: `rule`
- **Given**: 某账户存在多个脚本
- **When**: 以 SETACTIVE/存储接口依次设置活动脚本、设置空名取消活动
- **Then**: 任意时点至多一个活动脚本，切换在单事务内生效，重启后活动状态（或无活动状态）保持
- **Pass Condition**: 切换后活动脚本计数恒为 1（或取消后为 0）；重开连接后状态一致
- **Evidence**: `sieve_store_test.go`

### AC-3: ManageSieve 认证权威与拒绝
- **Type**: `rule`
- **Given**: ManageSieve 服务运行中
- **When**: 使用错误密码连接并尝试管理命令；使用正确的 mail_account 密码认证
- **Then**: 错误认证被 NO 拒绝且无法执行任何管理命令；正确认证后可管理脚本
- **Pass Condition**: 未授权命令全部失败；正确凭证 LISTSCRIPTS 成功
- **Evidence**: `server/managesieve_server_test.go`

### AC-4: ManageSieve 脚本管理命令符合 RFC 5804
- **Type**: `rule`
- **Given**: 已认证连接
- **When**: 执行 LISTSCRIPTS、GETSCRIPT、PUTSCRIPT、RENAMESCRIPT、DELETESCRIPT（含重复上传、获取/重命名不存在目标等边界）
- **Then**: 各命令返回与状态符合 RFC 5804（重复名 SCRIPTEXISTS；GET 不存在 NO；删除/重命名后列表反映变化；GETSCRIPT 返回内容逐字节一致）
- **Pass Condition**: 全部命令的成功/失败响应与最终存储状态符合预期
- **Evidence**: `managesieve_server_test.go`

### AC-5: 语法与不支持能力在激活前被拒绝
- **Type**: `rule`
- **Given**: 含语法错误的脚本、require 未支持扩展的脚本、合法脚本
- **When**: 分别对其 PUTSCRIPT、CHECKSCRIPT、SETACTIVE
- **Then**: 非法脚本在三种操作下均被 NO 拒绝且错误包含位置信息，且不会被存储为活动脚本；合法脚本通过
- **Pass Condition**: 非法脚本 0 次被接受；活动脚本始终为合法脚本
- **Evidence**: `sieve_validate_test.go`、`managesieve_server_test.go`

### AC-6: 收信执行活动脚本（条件覆盖）
- **Type**: `rule`
- **Given**: 账户配置了基于 header/address/size/body 条件与不同动作的活动脚本
- **When**: SMTP 投递匹配/不匹配条件的本地邮件
- **Then**: 命中分支按脚本动作投递（fileinto 目标箱、redirect 排队），未取消保留时隐式入 INBOX
- **Pass Condition**: 各邮件最终 mail 行所在邮箱与 outbox 条目符合脚本分支预期
- **Evidence**: `server/resource/sieve_deliver_test.go`

### AC-7: 动作语义（keep/fileinto/discard/stop）
- **Type**: `rule`
- **Given**: 含 keep、fileinto、discard、stop 的活动脚本
- **When**: 投递触发相应动作
- **Then**: keep→INBOX；fileinto→账户指定邮箱，邮箱不存在时被安全创建；discard→无 mail 行；stop 后同脚本后续命令不执行
- **Pass Condition**: mail 行、邮箱行与命令执行痕迹符合上述语义
- **Evidence**: `sieve_deliver_test.go`

### AC-8: redirect 防重复与防循环
- **Type**: `rule`
- **Given**: 活动脚本含多个相同 redirect、redirect 到原发件人、redirect 到本人地址、超过数量上限的 redirect
- **When**: 投递触发这些动作
- **Then**: 相同目的仅产生一条 outbox；回到原发件人/本人地址不产生 outbox；超上限脚本不被接受或执行中止
- **Pass Condition**: outbox 行的目的地址集合与计数符合约束
- **Evidence**: `sieve_deliver_test.go`、`sieve_validate_test.go`

### AC-9: 投递原子性、失败无部分投递
- **Type**: `rule`
- **Given**: 活动脚本执行链路中被注入一个失败（如非法目标邮箱名/不可用重定向目的）
- **When**: 执行本地投递
- **Then**: 事务回滚，mail/mail_box/outbox/vacation_track 无任何新增行或 UID 推进，SMTP 得到临时错误
- **Pass Condition**: 失败后相关表计数与投递前完全一致，无 UID/nextuid 残留推进
- **Evidence**: `sieve_deliver_test.go`

### AC-10: vacation 安全条件
- **Type**: `rule`
- **Given**: vacation 活动脚本；来信分别为列表邮件、带 Auto-Submitted 的自动邮件、空 envelope sender、普通个人邮件
- **When**: 投递这些邮件
- **Then**: 前三类不产生任何回复 outbox；普通个人邮件产生回复
- **Pass Condition**: 仅普通邮件产生 1 条回复 outbox
- **Evidence**: `sieve_deliver_test.go`

### AC-11: vacation 持久周期去重（含并发/重启）
- **Type**: `rule`
- **Given**: vacation 脚本（:days 与 :handle），同一发件人在周期内/周期后来信，不同发件人、不同 handle 并发来信
- **When**: 投递并在并发事务下竞争去重键，且重开连接后再次投递
- **Then**: 同（账户、发件人、handle）在周期内仅回复一次；周期后可再回复；不同键分别计数；并发下至多一条回复；重启后去重仍生效
- **Pass Condition**: vacation_track 行与回复 outbox 数量符合上述全部情形
- **Evidence**: `sieve_deliver_test.go`

### AC-12: vacation 回复格式
- **Type**: `rule`
- **Given**: vacation 触发一次回复
- **When**: 检查排队的回复邮件
- **Then**: MAIL FROM 为空，含 Auto-Submitted: auto-replied、In-Reply-To/References，主题与正文符合参数
- **Pass Condition**: 解析 outbox 中回复邮件，上述字段全部满足
- **Evidence**: `sieve_deliver_test.go`

### AC-13: 无活动脚本账户行为不变
- **Type**: `rule`
- **Given**: 未配置任何活动脚本的账户
- **When**: SMTP 投递普通邮件与垃圾邮件
- **Then**: 目标邮箱（INBOX/Spam）、flags、Spam 标记、metadata 与改动前完全一致
- **Pass Condition**: 新旧路径产出行逐字段一致
- **Evidence**: `sieve_deliver_test.go` 与既有 `mail_functions_test.go`/`outbox_mail_server_test.go` 全绿

### AC-14: IMAP 与 JSON:API 邮件操作回归
- **Type**: `rule`
- **Given**: 现有 IMAP/JSON:API 测试
- **When**: 运行相关测试套件
- **Then**: 全部通过，行为无变化
- **Pass Condition**: `go test ./server/...`（含 endpoint_imap、mail search、outbox 等）通过
- **Evidence**: 测试命令输出

### AC-15: ManageSieve TLS 与运行时生命周期
- **Type**: `rule`
- **Given**: ManageSieve 服务启动
- **When**: 在 STARTTLS 前请求认证、完成 STARTTLS 后认证；关闭服务
- **Then**: TLS 前 AUTH 能力不公告/不可用，TLS 后可认证；服务可干净关闭，错误经 runtimeErrors 上报
- **Pass Condition**: 握手与关闭行为符合预期
- **Evidence**: `managesieve_server_test.go`

### AC-16: 架构契合度
- **Type**: `rubric`
- **Dimension**: 实现复用 Daptin 既有权威（资源 CRUD、关系、outbox、mail_account 身份）的程度；是否存在影子存储/第二路径
- **Scale**: 1-5
- **Anchors**: 1 = 新增并行存储/身份/投递实现；3 = 大体复用但存在少量绕过；5 = 全部经由既有 DbResource 与 outbox 路径，无影子表/兼容分支
- **Pass Threshold**: >= 4
- **Evidence**: 代码审查与 git diff

### AC-17: 测试覆盖质量
- **Type**: `rubric`
- **Dimension**: 测试对边界（拒绝、回滚、去重、并发、循环）与两个入口（SMTP 直投、ManageSieve）的证明力
- **Scale**: 1-5
- **Anchors**: 1 = 仅 happy path；3 = 主要路径覆盖；5 = 每条 AC 均有自动化证据且含失败/并发边界
- **Pass Threshold**: >= 4
- **Evidence**: 测试文件与运行结果

## Open Questions
- 无（需求边界已明确；如审查中发现 vacation :from/:addresses 的账户归属校验策略需要收紧，将在实现中按"语法合法即可、发送仍以 outbox 与空 MAIL FROM 约束"处理）。
