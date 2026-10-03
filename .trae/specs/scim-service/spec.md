# Daptin SCIM 2.0 用户与用户组同步服务 - 产品需求文档

## Overview

- **Summary**: 在 Daptin 中新增一个可开关、受管理员令牌保护的 SCIM 2.0（RFC 7643/7644）服务，将 SCIM Users/Groups 映射到现有 `user_account`、`usergroup` 及其既有的成员关系资源，支持发现、创建、读取、筛选列表、替换（PUT）、补丁（PATCH）和删除，并以统一的快照/版本语义支撑分页、筛选与 If-Match 条件修改。
- **Purpose**: 企业身份提供商（IdP）管理员通过标准协议持续完成入职、调组、离职，不再为每次变更编写私有数据接口脚本；停用/离职立即在 Daptin 全部登录通道生效，同时保留用户拥有的业务记录。
- **Target Users**: 配置企业 IdP（如 Entra ID、Okta 等 SCIM 客户端）的 Daptin 管理员；被同步的终端登录用户。

## Goals

- SCIM Users/Groups 完整 CRUD + 协议发现，全部数据落在现有 `user_account`、`usergroup`、`user_account_user_account_id_has_usergroup_usergroup_id` 关系资源上，不建影子表、不建第二身份模型。
- 稳定的 `id`（reference_id）、`externalId`、资源版本（ETag/meta.version）与更新时间；筛选、分页、If-Match 共享同一事务快照语义。
- userName（映射 email）与非空 externalId 在资源类型内唯一；冲突、陈旧版本、非法路径、未知成员、跨资源类型引用一律拒绝且无部分更新；多操作 PATCH 全有或全无。
- 用户 `active` 状态统一控制密码、OAuth（社交登录与 OAuth Server 令牌）、SMTP、IMAP、FTP、API JWT/Basic 登录；停用或删除立即提升 auth_version、清理认证/权限缓存、撤销 OAuth 令牌并关闭 WebSocket 会话；业务记录保留；重启后状态一致。
- SCIM 未启用时不暴露任何端点；手工用户管理、用户组权限检查、非 SCIM 账户行为保持不变。

## Non-Goals

- 不实现 SCIM Bulk（/Bulk）、排序（sorting）、`attributes`/`excludedAttributes` 裁剪、跨资源类型组成员（Group 嵌套）、SCIM 自定义 schema 与企业扩展 schema 的写回。
- 不做 SCIM 客户端（主动向外推送）、不做密码哈希同步（入站密码按现有 bcrypt 处理）。
- 不改变现有 JSON:API `/api` 的鉴权模型、计量模型与权限位语义；不重构 mail_account 与 user_account 的凭据关系。
- 不强制踢出停用前已经建立的 IMAP/FTP 长连接（新登录一律拒绝）；HTTP/WebSocket/OAuth 会话立即失效。
- 不提供 SCIM 之外的用户导入/导出 UI。

## Background & Context

- 现有权威（调研结论，均有源码行号证据）：
  - `user_account` 业务列：`name`（必填、**非唯一**）、`email`（唯一索引、登录标识）、`password`（可空、bcrypt）、`confirmed`（bool，**全链路无人读取**）、`auth_version`（int，ExcludeFromApi，默认 1，仅改密时自增）；标准列 `id/version/created_at/updated_at/reference_id/permission`。
  - `usergroup`：`name` 唯一；系统种子组 guests/users/administrators。
  - 成员关系即访问组 join 表 `user_account_user_account_id_has_usergroup_usergroup_id`（列 `user_account_id/usergroup_id/reference_id/permission/version/...`，两外键复合唯一），是普通资源；其 Create/Update/Delete 已自动失效 auth/admin/对象组权限缓存。
  - 资源生命周期：`CreateWithoutFilter/UpdateWithoutFilters/DeleteWithoutFilters` 及 `*WithTransaction`，更新 SQL 自带 `WHERE version=?` 乐观锁，0 行返回 `resource.ErrVersionConflict`；唯一约束冲突经 `normalizeDatabaseConstraintError` 归一为 409。
  - 装配：`server/server.go` NewRuntime 中各协议按 `_config` 开关注册（caldav/imap/ftp/yjs/graphql 先例）；ConfigStore 键如 `caldav.enable` 默认 `"false"`；路由须在 `SetupNoRouteRouter` 之前注册。
  - 认证通道现状：JWT（含 auth_version 比对）、Basic（CalDAV/子站，无版本校验）、社交 OAuth（`jwt.token` with skipPasswordCheck）、OAuth Server 不透明令牌（`ValidateAccessToken` 不查用户状态、无按用户撤销）、SMTP/IMAP（mail_account 独立密码，经 `user_account_id` 属主列关联）、FTP（user_account 密码）；所有通道均**不检查任何用户启停状态**。
  - 已发现缺陷：`AuthCheckMiddlewareWithHttp` 中 `rowx.Scan` 的错误被下一行 `stmt.Close()` 覆盖，已删除用户持有旧 JWT 会被装配成 UserId=0 的身份并通过校验——本需求必须修复。
  - 缓存失效可复用件：`auth.InvalidateAuthCacheForEmail`、`InvalidateAdminCacheForUser`、`InvalidateObjectPermissionCache/InvalidateRowPermissionCache/InvalidateObjectGroupsCache/InvalidateObjectUsergroupRelationPermissionCaches`、`jwtmiddleware.InvalidateTokenCache`。
  - 列变更由启动 schema 同步自动 `alter table add column` 并补建唯一索引；SQLite/MySQL/Postgres 唯一索引均允许多个 NULL。
- 已确认的设计决策（用户批准）：
  1. 开关与令牌使用数据库 `_config`：`scim.enable`（默认 `"false"`）与 `scim.admin_token`（默认空；空则即使 enable 也不注册端点），可经现有管理员 `/_config` 接口运行时设置。
  2. SCIM `userName` 映射到 `user_account.email`；`displayName` 映射到 `user_account.name`；Groups 的 `displayName` 映射到 `usergroup.name`。

## Functional Requirements

- **FR-1 开关与端点暴露**：仅当 `scim.enable=="true"` 且 `scim.admin_token` 非空时注册 `/scim/v2/...` 路由；未启用时这些路径返回 404（落入现有 no-route 处理），且不产生任何 SCIM 相关 goroutine/订阅。
- **FR-2 管理员令牌认证**：所有 SCIM 端点（含发现端点）要求 `Authorization: Bearer <token>`（亦可接受 `X-API-Key`），使用 `crypto/subtle.ConstantTimeCompare` 校验；失败返回 401 与 `WWW-Authenticate: Bearer` 头及 SCIM Error JSON。令牌是受信后端配置，不进入 `PermissionInstance`，不合成 Daptin 用户身份。
- **FR-3 协议发现**：提供 `GET /scim/v2/ServiceProviderConfig`、`/ResourceTypes`、`/ResourceTypes/User`、`/ResourceTypes/Group`、`/Schemas`、`/Schemas/{schemaId}`；能力声明（patch/filter/etag=true，bulk/sort=false）必须与实现一致；响应用 `application/scim+json`（同时接受请求侧 `application/json`）。
- **FR-4 标识与元数据**：SCIM `id` = 现有 reference_id UUID 字符串，跨更新/重启稳定；`externalId` 持久化到新列 `external_id`（user_account、usergroup 各一列，可空、非空唯一）；`meta.version` 与响应头 `ETag` 均为 `W/"<version>"`（version 为现有行版本列）；`meta.lastModified` 取 updated_at（为空回退 created_at），`meta.created` 取 created_at；`meta.location` 为规范 SCIM URL；创建返回 201 + Location。
- **FR-5 用户创建**：POST 接受 userName(必填)、externalId、displayName、name.formatted/givenName/familyName、emails、active（缺省 true）、password（可空，走现有 bcrypt）；经现有资源创建路径落库（含默认 users 组、自引用 owner 等既有行为）；重复 userName/email 或重复非空 externalId 返回 409 `uniqueness`。
- **FR-6 读取与列表**：GET `/{id}` 支持 200/404；GET 集合支持 `startIndex`(1 基)/`count`/`filter`，返回 `totalResults/itemsPerPage/startIndex/Resources`，顺序确定（按内部 id 升序），count 缺省/上限遵循 ServiceProviderConfig 声明。
- **FR-7 筛选**：支持在 `id`、`userName`、`externalId`、`displayName` 上的 `eq/ne/co/sw/ew/pr`，支持 `and/or` 与括号组合，标准 SCIM filter 转义；User 的 userName→email、displayName→name，Group 的 displayName→name，id→reference_id；未知属性/不支持的操作符/畸形表达式返回 400 `invalidFilter`。
- **FR-8 替换 PUT**：全量替换语义（未出现的可写属性重置为缺省，成员以提交为准）；每次成功更新 version+1、updated_at 刷新、ETag 变化。
- **FR-9 补丁 PATCH**：支持 add/replace/remove；用户可改 userName/externalId/displayName/active/password/name/emails；组可改 displayName/externalId 及 members（单值 `value` 或 `members` 列表；remove 支持 `members[value eq "..."]` 与移除全部）。所有操作先在内存中校验并合成目标状态，再在单一事务内提交：非法路径 400（`noTarget`/`invalidPath`）、未知成员 400（`invalidValue`）、成员 `type` 非 `User` 的跨类型引用 400、陈旧 If-Match 412、乐观锁冲突 409；任一失败全部回滚，成员关系也不得部分变更。
- **FR-10 条件修改**：GET 返回 ETag；PUT/PATCH/DELETE 携带 `If-Match` 时必须与当前版本匹配，否则 412；未携带则按乐观锁正常提交（version WHERE 兜底并发）。
- **FR-11 组成员一致性**：成员只接受现存 user_account；User 资源的 `groups` 与 Group 资源的 `members` 从同一 join 关系派生，双方读取一致；成员增删经现有 join 关系资源的生命周期函数执行，自动同步权限关系与缓存。
- **FR-12 组替换/删除的权限同步**：替换/PATCH 组成员在同一事务内对 join 行做差量增删；删除组前在同一事务内清理所有引用该组的 `*_has_usergroup_usergroup_id` 关系行（用户成员关系与各资源授权行），并失效相关对象权限缓存与受影响用户的 auth/admin 缓存；删除返回 204。
- **FR-13 active 列与全通道强制**：user_account 新增 `active` 布尔列（NOT NULL DEFAULT TRUE，启动同步补列，存量用户为 true）。下列入口在凭据验证通过后、签发/装载身份前必须拒绝 active=false 的用户：密码 signin（含 skipPasswordCheck 的社交 OAuth 链）、session.user.switch、OTP 登录签发、JWT 全局中间件（API + WebSocket 新建请求）、HTTP Basic（CalDAV/子站）、FTP、IMAP、SMTP、OAuth Server 的 `/oauth/token` 刷新与 `ValidateAccessToken`（userinfo/introspect/资源访问）。拒绝使用各通道现有错误惯例；不得因用户不存在/已删除而放行（修复 Scan 错误吞没）。
- **FR-14 停用/删除失效链**：active 由 true 变为 false（任何写入面）以及删除 user_account 时，在同一事务内：auth_version+1、撤销该用户全部未过期 oauth_access/oauth_refresh（置 revoked_at）；提交后：清理 auth-cache（按 email）、token-cache、admin/对象组权限缓存，并发布集群级会话撤销事件使各节点立即关闭该用户的 WebSocket 连接。删除走现有硬删除路径，清理其成员关系行，不删除/不转移该用户拥有的业务记录。
- **FR-15 重新启用**：active 由 false 变回 true 后可重新登录；active 切换不修改任何成员关系；成员关系只在 SCIM（或既有手工面）提交时变化，停用前已移除的成员关系不因重新启用而恢复。
- **FR-16 持久化一致性**：externalId、active、version、auth_version 与成员关系全部存储在受管 SQL 数据库；服务重启后读取结果与停机前一致，SCIM 层不持有任何仅内存的权威状态。
- **FR-17 请求体限制**：`/scim/` 写方法纳入现有 `bufferedRequestBodyMiddleware` 路径限制。
- **FR-18 共存**：SCIM 关闭或启用均不改变 `/api/user_account`、`/api/usergroup` 的现有行为、权限检查、默认组与审计；非 SCIM 创建账户默认 active=true 可正常使用全部既有功能。

## Non-Functional Requirements

- **NFR-1 原子性**：每个 SCIM 写请求对应一个 `sqlx.Tx`：读取快照→If-Match 校验→合成目标→全部写入→提交；列表请求在单一只读事务内完成筛选与分页，保证页内快照一致。
- **NFR-2 数据库兼容**：仅用 goqu/既有资源 helper 与标准 DDL，保持 SQLite、MySQL、Postgres 行为一致；external_id 唯一索引依赖多 NULL 允许语义（三库均满足）。
- **NFR-3 安全**：令牌比较常量时间；不记录令牌；SCIM 处理器不信任调用方传入的身份/权限；所有副作用（缓存失效、令牌撤销、会话关闭）在授权与校验通过后发生；拒绝请求不产生任何副作用。
- **NFR-4 可运维**：开关/令牌可经 `_config` 运行时配置；协议错误使用 SCIM Error schema（`schemas/status/scimType/detail`）与正确状态码；组件随 Runtime ctx 取消订阅，不泄漏 goroutine。
- **NFR-5 测试**：新增单元（filter/path 解析、序列化、版本语义）+ sqlite 内存集成（CRUD、原子 PATCH、冲突、成员一致性、登录拦截、失效链、重启一致性）+ 开关/鉴权 handler 测试；`go test ./...`、`go vet ./...`、`git diff --check` 通过。

## Constraints

- **Technical**：Go、gin、api2go v2、sqlx/goqu、Olric；新代码以 `server/scim` 包承载协议与存储逻辑，`server/endpoint_scim.go` 承载路由装配（仿 caldav）；不引入新依赖（SCIM 结构体自行定义）。
- **Business/架构（AGENTS.md）**：一个关注点一个权威——身份仍归 `auth.SessionUser`/`user_account`，成员归持久化关系，权限归 `PermissionInstance`，缓存归 Olric 既有键；附加服务适配资源而非重建；不加兼容分支/第二路径；最小生产 diff，测试不受此限。
- **Dependencies**：现有 schema 同步、DbResource 生命周期函数、ConfigStore、Olric PubSub/DMap、OAuthProvider。

## Assumptions

- SCIM `userName` 必须是合法 email 形态（映射到 email 列的格式约束）；非法形态按 400 `invalidValue` 拒绝。
- `name.givenName/familyName` 无独立存储列：缺 formatted 时以二者拼接写入 `user_account.name`；读回仅返回 formatted/displayName，不声明持久化 given/family。
- 删除用户时在已建立外键约束的 MySQL/Postgres 上，若业务记录仍引用该用户，数据库可能拒绝删除——这是现有手工删除的同一行为；SCIM 不新增级联删除或归属转移，业务记录保留策略不变。
- 发现端点同样要求管理员令牌（主流 IdP 的 SCIM 探测请求均携带配置的 Bearer 令牌）。
- 种子组（guests/users/administrators）对 SCIM 可见且可被令牌持有者管理，不设特殊豁免分支；其写操作后果由管理员策略负责。

## Acceptance Criteria

### AC-1: SCIM 开关控制端点暴露
- **Type**: `rule`
- **Given**: 服务启动且未设置 `scim.enable=true`+非空 `scim.admin_token`
- **When**: 客户端访问任意 `/scim/v2/...`（含发现端点）
- **Then**: 返回 404，路由未注册；置为启用并配置令牌后（新运行时/重启）端点可达
- **Pass Condition**: 关闭态全部 SCIM 路径 404；启用态 ServiceProviderConfig 携带令牌可 200
- **Evidence**: handler 测试 + server.go 注册条件代码审查

### AC-2: 管理员令牌认证
- **Type**: `rule`
- **Given**: SCIM 已启用且令牌为 T
- **When**: 无令牌、错误令牌、错误 scheme 访问，以及使用正确 `Bearer T`/`X-API-Key: T` 访问
- **Then**: 前三者 401 且含 `WWW-Authenticate: Bearer` 与 SCIM Error JSON；正确令牌 200；比较使用常量时间实现
- **Pass Condition**: 401/200 结果与头均符合；代码使用 `subtle.ConstantTimeCompare`
- **Evidence**: 中间件表驱动测试；代码审查

### AC-3: 协议发现资源合规
- **Type**: `rule`
- **Given**: SCIM 已启用
- **When**: GET ServiceProviderConfig / ResourceTypes(/User,/Group) / Schemas(/User,/Group)
- **Then**: 文档 id 与 `schemas` 声明符合 RFC 7643/7644；ResourceType 的 endpoint 为 Users/Groups；SPC 声明 patch/filter/etag supported=true、bulk=false；Content-Type 为 application/scim+json
- **Pass Condition**: 上述字段逐项断言通过
- **Evidence**: 发现端点测试 JSON 断言

### AC-4: 用户创建与稳定标识
- **Type**: `rule`
- **Given**: 管理员令牌
- **When**: POST /scim/v2/Users 携带 userName/externalId/displayName/active/name/emails
- **Then**: 201 + Location 与 ETag 头；响应含稳定 UUID id、externalId、meta(resourceType=User, created, lastModified, version=`W/"n"`, location)；user_account 落库 email=userName、active 默认 true、password 为 bcrypt 形态；重复 GET 同 id 返回相同 id 与（未变更时）相同 version
- **Pass Condition**: 字段映射与头断言；DB 行断言；重复读取一致
- **Evidence**: 集成测试

### AC-5: 读取不存在资源与列表分页
- **Type**: `rule`
- **Given**: 数据集含已知数量用户
- **When**: GET 不存在的 id；GET /Users?startIndex=1&count=2 逐页取完
- **Then**: 不存在返回 404 SCIM Error；列表响应 totalResults 等于总数、itemsPerPage/startIndex 正确、页间无重无漏、顺序确定（id 升序）
- **Pass Condition**: 计数、页内容、顺序断言通过
- **Evidence**: 分页集成测试

### AC-6: 筛选表达式
- **Type**: `rule`
- **Given**: 含不同 userName/externalId/displayName 的数据集
- **When**: 分别按 id/userName/externalId/displayName 的 eq/co/sw/ew/ne/pr 与 and/or/括号组合查询；再发未知属性与畸形 filter
- **Then**: 合法表达式仅返回匹配资源（Users 与 Groups 各自的属性映射正确）；非法请求 400 scimType=invalidFilter
- **Pass Condition**: 结果集与非法状态码断言通过
- **Evidence**: filter 解析器单元测试 + 集成测试

### AC-7: PUT 替换与版本推进
- **Type**: `rule`
- **Given**: 已存在用户/组及其当前 ETag
- **When**: 携带匹配 If-Match PUT 全量替换；再用旧 ETag PUT；并发用相同版本 PUT 两次
- **Then**: 匹配时 200 且 version 递增、lastModified 变化、ETag 更新、未提供的可写字段回到缺省；旧 ETag 412；第二次并发提交 409（或 412，二者之一且无写入）
- **Pass Condition**: 版本链、412、冲突无写入断言通过
- **Evidence**: 集成测试

### AC-8: PATCH 原子性与错误拒绝
- **Type**: `rule`
- **Given**: 已存在用户与组
- **When**: 一个 PATCH 中同时包含多个合法属性/成员变更；以及混入未知路径、未知成员 id、`type:"Group"` 成员、陈旧 If-Match 的 PATCH
- **Then**: 合法 PATCH 整体提交且 version+1；非法 PATCH 返回对应 400（invalidPath/noTarget/invalidValue）或 412，且用户、组、join 表均无任何变化（多成员场景同样零变更）
- **Pass Condition**: 变更前后全量快照对比，失败用例零差异
- **Evidence**: 原子性集成测试（含成员表计数与行内容断言）

### AC-9: 唯一性约束
- **Type**: `rule`
- **Given**: 已存在 userName=u1、externalId=e1 的用户与同名组
- **When**: 重复创建/更新使 userName 或非空 externalId 重复；externalId 传空字符串
- **Then**: 冲突返回 409 scimType=uniqueness 且无部分写入；空字符串按无 externalId 处理（可多个）；重启后约束仍生效
- **Pass Condition**: 409 与空值语义、重启后复测通过
- **Evidence**: 集成测试（含重启一次后再冲突）

### AC-10: 用户删除保留业务记录
- **Type**: `rule`
- **Given**: 用户拥有业务记录（其创建的其它资源行）
- **When**: DELETE /scim/v2/Users/{id}
- **Then**: 204；再 GET 404；其成员关系行被清理；其拥有的业务记录仍存在且 owner 列不被改写
- **Pass Condition**: 用户/成员行删除、业务记录保留断言通过
- **Evidence**: 集成测试（sqlite）

### AC-11: 组成员双向一致与成员校验
- **Type**: `rule`
- **Given**: 若干现存用户
- **When**: PUT/PATCH 组成员（含全部替换、增量 add、按 value remove、清空）；提交不存在的用户 id 与 type=Group 的成员；随后读 Group 与各 User
- **Then**: 合法提交后 Group.members 与每个 User.groups 从同一 join 表派生且完全一致；非法成员 400 invalidValue/跨类型 400 且无变更
- **Pass Condition**: 双向集合断言 + 失败零变更断言
- **Evidence**: 集成测试

### AC-12: 组删除同步权限关系与缓存
- **Type**: `rule`
- **Given**: 组内有成员且该组被授予某资源访问权（join 授权行存在）
- **When**: DELETE /Groups/{id}
- **Then**: 204；同事务内所有引用该组的 join 关系行删除；受影响用户后续权限判定不再含该组（缓存不返回陈旧结果）；GET 组 404
- **Pass Condition**: join 行计数、缓存失效后权限结果断言通过
- **Evidence**: 集成测试（权限经既有 PermissionInstance 路径复测）

### AC-13: active 停用阻断全部登录通道
- **Type**: `rule`
- **Given**: 一个具备密码、社交 OAuth 可登录、OAuth Server 令牌、mail_account(SMTP/IMAP)、FTP 凭据的活跃用户
- **When**: 将 active 置 false 后，逐一尝试密码 signin、skipPasswordCheck 签 token、session.user.switch、OTP 签发、API JWT 请求、HTTP Basic、FTP AuthUser、IMAP Login、SMTP VerifyLOGIN、OAuth ValidateAccessToken/刷新令牌；再置 true 重试
- **Then**: 停用后全部拒绝（各通道现有错误惯例），启用后恢复成功
- **Pass Condition**: 9 个入口停用拒绝/启用恢复断言全部通过
- **Evidence**: 各入口单元/集成测试（auth/actions/resource/server 包）

### AC-14: 停用/删除立即失效会话
- **Type**: `rule`
- **Given**: 用户持有有效 JWT、OAuth 令牌与打开的 WebSocket 连接
- **When**: active 置 false；另对一用户执行 DELETE
- **Then**: 两用户旧 JWT 的下一次 HTTP 请求均 401（含已删除用户，不得装配 UserId=0 身份）；oauth_access/oauth_refresh 全部 revoked_at 非空；auth-cache 中 email 键被清除；两节点（模拟 Olric 发布）上该用户的 WebSocket 被关闭
- **Pass Condition**: 401、撤销行、缓存缺失、WS 关闭断言通过
- **Evidence**: 失效链集成测试 + auth 中间件回归测试

### AC-15: 重新启用不恢复成员关系
- **Type**: `rule`
- **Given**: 用户在组 G 中；停用后经 SCIM 将其移出 G
- **When**: 重新启用并登录、读取成员关系
- **Then**: 可登录；仍不在 G 中；active 切换前后成员关系零变化（另设对照：仅切 active 不动成员）
- **Pass Condition**: 登录成功 + 成员集合断言通过
- **Evidence**: 集成测试

### AC-16: 重启后状态一致
- **Type**: `rule`
- **Given**: 经 SCIM 创建/修改/调组/停用的一批用户与组（含 externalId、版本、成员）
- **When**: 用同一数据库重新构建运行时（不重新生成数据）后经 SCIM 全量读取
- **Then**: id、externalId、version、lastModified、active、成员集合与重启前逐字段一致
- **Pass Condition**: 重启前后快照 deep-equal
- **Evidence**: 两阶段集成测试（重建 runtime/store）

### AC-17: 与现有管理面共存
- **Type**: `rule`
- **Given**: SCIM 启用态
- **When**: 走既有 `/api/user_account`、`/api/usergroup` 手工创建/更新/删除与权限检查；运行仓库既有测试套件
- **Then**: 行为与当前主线一致；非 SCIM 账户默认 active=true 且全部既有功能正常
- **Pass Condition**: `go test ./...` 全部通过；关键既有鉴权/权限测试无回归
- **Evidence**: 全量测试输出

### AC-18: 架构一致性质量
- **Type**: `rubric`
- **Dimension**: 实现是否复用 Daptin 既有权威（资源生命周期、关系资源、权限/缓存/会话机制），避免影子表、私有 SQL 写路径、第二身份模型与平行实现
- **Scale**: 1-5
- **Anchors**: 1 = 新建私有用户/成员表或绕过资源路径直接写 SQL；3 = 主要复用但有个别平行逻辑/缓存；5 = 所有写操作经 DbResource 既有生命周期与同一事务，缓存失效/权限/身份全部复用现有权威，新增生产面最小
- **Pass Threshold**: >= 4
- **Evidence**: 代码审查 + 架构守护测试（约束 scim 包不出现裸 INSERT/UPDATE/DELETE、不新建身份类型）

### AC-19: SCIM 协议规范符合度
- **Type**: `rubric`
- **Dimension**: 状态码、SCIM Error（schema/scimType/status/detail）、Content-Type、ETag/meta、ListResponse 字段、PATCH Op 语义与 RFC 7643/7644 的符合程度
- **Scale**: 1-5
- **Anchors**: 1 = 仅 JSON 形似；3 = CRUD 可用但错误码/条件请求/分页语义多处偏差；5 = 发现资源、错误类型、条件请求、分页、PATCH 语义均可被标准 SCIM 客户端直接消费
- **Pass Threshold**: >= 4
- **Evidence**: 协议断言测试 + 代码审查

## Open Questions

- 无（配置位置与 userName 映射已经用户确认；其余按 Assumptions 执行）。
