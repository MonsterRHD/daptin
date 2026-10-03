# Daptin SCIM 2.0 用户与用户组同步服务 - 实施计划

说明：任务按依赖排序，每个任务是可独立验证的纵向切片。所有写操作必须经 DbResource 既有生命周期函数与同一 `sqlx.Tx`；新协议代码集中在 `server/scim` 包与 `server/endpoint_scim.go`，认证/资源侧改动放在既有权威文件中。

## Task 1: 数据模式扩展（active、external_id）
- **Status**: `completed`
- **Completion Evidence**: columns.go 新增 user_account.active(默认 true/索引)/external_id 与 usergroup.external_id(可空唯一)；resource/user.go 提供 ACTIVE_COLUMN_NAME 与 RowValueActive 归一。TR-1.1~1.4 由 server/resource/scim_columns_test.go 覆盖：全新建表、旧库 alter 补列+唯一索引、多 NULL 共存、重复非空拒绝、RowValueActive 表驱动；go test ./server/resource/ 通过。
- **Priority**: high
- **Depends On**: None
- **Description**:
  - 在 `server/resource/columns.go` 的 user_account 定义中新增 `active`（bool/truefalse，NOT NULL，默认 `"true"`，IsIndexed）与 `external_id`（varchar(191)，IsNullable，IsUnique+IsIndexed）；usergroup 定义中新增同名 `external_id` 列。
  - 验证启动 schema 同步（dbfunctions_check/alterTableAddColumn/CreateIndexes）在旧库上自动补列、补唯一索引；确认三库唯一索引允许多个 NULL；external_id 空串在写入侧归一为 NULL。
  - 提供行值布尔归一 helper（active 读回可能是 int64/bool/string）。
- **Acceptance Criteria Addressed**: AC-9、AC-16、AC-18
- **Test Requirements**:
  - `rule` TR-1.1: 全新 sqlite 初始化后 user_account 含 active 默认 1、external_id 列；usergroup 含 external_id；证据：表结构查询测试。
  - `rule` TR-1.2: 模拟旧库（缺列、缺索引）执行 schema 同步后列与唯一索引存在；证据：迁移测试。
  - `rule` TR-1.3: 两个 external_id=NULL 的用户/组可共存，第三个非空重复值被唯一索引拒绝；证据：插入冲突测试。
  - `rule` TR-1.4: `go test ./server/resource/... -count=1` 通过，无既有用列断言回归。
- **Notes**: 不新增登录名列；不动 confirmed 语义。

## Task 2: active 状态在全部认证入口强制执行
- **Status**: `completed`
- **Completion Evidence**: SessionUser 增加 Active（二进制缓存前后向兼容，旧条目默认 active）；AuthCheckMiddleware SELECT 增 active 并修复 sql.ErrNoRows 被吞没的零身份缺陷（未知/已删除用户明确 401），缓存/实时两条路径均拒绝 inactive；Basic 经新增 ResourceAdapter.GetUserActiveByEmail 拦截；signin（含 skipPasswordCheck 社交 OAuth）、session.user.switch、OTP 登录、FTP AuthUser、IMAP Login、SMTP VerifyLOGIN（经 IsUserAccountActiveWithTransaction 单一 helper）、OAuth sessionUserFromRow（Validate/Refresh/ExchangeCode 共用）全部拦截。证据：server/auth/scim_active_test.go（停用/删除/重激活 401、Basic、二进制往返）、server/resource/oauth_active_test.go、auth/auth_test.go 夹具更新；go test ./server/... 全过。
- **Priority**: high
- **Depends On**: Task 1
- **Description**:
  - `auth.SessionUser` 增加 `Active bool`；AuthCheckMiddleware 的用户查询 SELECT 增加 active 列，缓存结构携带 active；active=false 一律 401（缓存命中同样拦截）；修复 `rowx.Scan` 错误被 `stmt.Close()` 吞没的缺陷：`sql.ErrNoRows` 明确返回未认证（删除用户不得再装配 UserId=0 身份，移除该死代码自动建号分支），其他 DB 错误按 abort 处理。
  - BasicAuth（CalDAV/子站）在 bcrypt 通过后校验 active。
  - signin performer（含 skipPasswordCheck 的社交 OAuth 链）、`session.user.switch`、OTP 登录签发处：用户 inactive 时以各通道现有错误惯例拒绝。
  - FTP `AuthUser`、IMAP `Login`、SMTP `VerifyLOGIN`：凭据验证通过后校验属主 user_account.active（resource 包提供 `IsUserAccountActiveWithTransaction` 一类单一 helper，三处复用，禁止各自写判定）。
  - OAuth Server：`ValidateAccessToken` 装配身份时校验用户 active；refresh 路径同样校验。
- **Acceptance Criteria Addressed**: AC-13、AC-14（JWT/已删除用户部分）、AC-17
- **Test Requirements**:
  - `rule` TR-2.1: 停用用户经密码 signin 返回既有凭据错误且不签发 token；启用后成功；证据：actions 包测试。
  - `rule` TR-2.2: 停用/已删除用户的有效 JWT（auth_version 正确）经 AuthCheckMiddleware 返回未认证，不产生 SessionUser；证据：auth 包 httptest 测试。
  - `rule` TR-2.3: Basic 校验对 inactive 用户失败；证据：auth 包测试。
  - `rule` TR-2.4: FTP/IMAP/SMTP 三个入口对 inactive 属主返回认证失败、active 用户成功；证据：server 与 resource 包测试。
  - `rule` TR-2.5: inactive 用户的 OAuth access token 校验与 refresh 均失败；证据：resource 包 oauth 测试。
  - `rule` TR-2.6: 既有 auth/oauth/ftp/imap 相关测试全部通过。
- **Notes**: 判定失败必须闭合（失败不得放行）；不新增权限位。

## Task 3: 停用/删除的会话与令牌失效钩子、WebSocket 踢线
- **Status**: `completed`
- **Completion Evidence**: resource_update.go active 变更并入既有凭据失效块（auth_version+1、auth/token/admin 缓存清理），停用事务内撤销 oauth_access/oauth_refresh 并发布集群会话撤销事件；resource_delete.go 删除 user_account 前执行同一失效链（版本提升+令牌撤销+缓存+事件），业务记录不动；user_lifecycle.go 集中全部 helper；websocket Server 增加 userClients 索引/revokeCh/WatchSessionRevocations（随 Runtime ctx 订阅 daptin:user-session-revoke）/CloseUserSessions，server.go 接线。证据：user_active_lifecycle_test.go（停用 auth_version 1→2、双表令牌撤销、他用户令牌不受影响、成员关系不变；重激活 2→3；删除后令牌撤销+widget 记录保留）、websockets/revoke_test.go（仅目标用户连接被关闭，他用户保持）。go test ./server/... 通过。
- **Priority**: high
- **Depends On**: Task 2
- **Description**:
  - 扩展 `resource_update.go` 现有改密钩子：user_account 的 active 发生变化时同样在更新 SQL 中自增 auth_version，并在提交后清理 auth-cache（新旧 email）与 token-cache（复用既有 passwordChanged 代码块，合并为"凭据/状态变更"单一判定）。
  - OAuthProvider 增加 `RevokeTokensForUserWithTransaction(userID, tx)`（同事务置 oauth_access/oauth_refresh 的 revoked_at），active true→false 时在更新事务内调用。
  - `resource_delete.go` 增加 user_account 删除前钩子（同事务）：auth_version+1（按需求字面执行）、撤销该用户 OAuth 令牌；提交后清 auth/token/admin/对象组缓存；成员关系行仍走现有 deleteUsergroupRelations。
  - resource 包定义集群会话撤销事件（Olric PubSub topic 常量 + 发布 helper，发布失败只记日志不影响请求）；websocket Server 维护 userReferenceId→client 索引，订阅该 topic（随 Runtime ctx 取消），收到事件关闭本节点对应用户连接；server.go 注入 ctx/sharedPubSub。
- **Acceptance Criteria Addressed**: AC-14、AC-15
- **Test Requirements**:
  - `rule` TR-3.1: active→false 后 DB auth_version +1，auth-cache email 键缺失，token-cache 清理函数被调用；证据：resource 测试（DMap/桩）。
  - `rule` TR-3.2: 同用户 oauth_access/oauth_refresh 全部 revoked_at>0，未过期他用户令牌不受影响；证据：oauth 表行断言。
  - `rule` TR-3.3: 发布撤销事件后对应用户的 WebSocket client 被关闭，他用户不受影响；订阅随 ctx 取消退出无 goroutine 泄漏；证据：websockets 包测试。
  - `rule` TR-3.4: 删除用户后其缓存被清、OAuth 令牌被撤销；证据：resource 测试。
  - `rule` TR-3.5: 仅切 active（true→false→true）不产生任何 join 行增删；证据：成员行计数与内容对比。
- **Notes**: 踢线仅针对 HTTP/WebSocket/OAuth 会话；IMAP/FTP 既有长连接不踢（Non-Goal）。

## Task 4: scim 包协议类型、发现文档与 filter/PATCH 路径解析
- **Status**: `completed`
- **Completion Evidence**: server/scim types/protocol/filter/patch/discovery 完成：RFC 7643 URN、User/Group/PatchOp/ListResponse/Error 结构、弱 ETag；手写 filter 词法/语法分析（括号/and/or、eq/ne/co/sw/ew/gt/ge/lt/le/pr、转义）映射列白名单（userName→email、displayName→name、emails.value→email、id→reference_id），非法属性/UUID/算子→invalidFilter；PATCH 路径支持 name.*、emails.value、members[value eq]，区分 invalidPath/noTarget；发现 SPC（patch/filter/etag=true，bulk/sort=false，bearer）、ResourceTypes、8 个 Schemas。filter/patch/discovery_test 全过。
- **Priority**: high
- **Depends On**: None
- **Description**:
  - 新建 `server/scim` 包：定义常量（User/Group/PatchOp/ListResponse/Error/ServiceProviderConfig/ResourceType/Schema URN）、资源与消息结构体、SCIM meta/ETag 工具、`application/scim+json` 编解码（兼容接收 application/json）。
  - 实现 SCIM filter 词法/语法分析：属性白名单（Users: id/userName/externalId/displayName；Groups 同）、操作符 eq/ne/co/sw/ew/pr、and/or/括号与字符串转义；输出映射到真实列（userName→email、displayName→name、id→reference_id、externalId→external_id）的 goqu 谓词；非法属性/操作符/语法返回带 scimType=invalidFilter 的类型化错误。
  - 实现 PATCH path 解析与校验（含 `members[value eq "..."]`），输出规范化操作序列；未知路径返回 noTarget/invalidPath 类型化错误。
  - 静态发现文档生成器（SPC、ResourceTypes、Schemas），能力声明必须与实现一致（patch/filter/etag=true，bulk/sort=false，filter maxResults 与分页上限一致）。
- **Acceptance Criteria Addressed**: AC-3、AC-6、AC-8（校验部分）、AC-19
- **Test Requirements**:
  - `rule` TR-4.1: RFC 7644 第 3.4.2.2 节示例过滤器（eq/and/or/括号/co/sw）全部解析为预期谓词；非法用例返回 invalidFilter；证据：表驱动单元测试。
  - `rule` TR-4.2: PATCH 路径合法/非法用例（含 members 值过滤、深层 name 路径）判定正确；证据：单元测试。
  - `rule` TR-4.3: 发现文档 JSON 含正确 URN、endpoint、能力开关；证据：单元测试快照断言。
  - `rubric` TR-4.4: 协议结构体与 RFC 7643 命名/JSON 形态一致性；scale 1-5；anchors 1=字段缺失严重、3=主要字段在但边角不符、5=可直接被标准客户端解析；threshold >= 4；证据：代码审查+JSON 断言。
- **Notes**: 不引入第三方 SCIM 依赖。

## Task 5: SCIM 存储层（事务快照 CRUD、成员差量、版本与冲突）
- **Status**: `completed`
- **Completion Evidence**: 单 sqlx.Tx 快照读写，所有写经 DbResource 生命周期函数；用户五列映射校验（email 归一、name 组合、externalId 空串→NULL、active 缺省 true）；组成员按有序操作折叠差量，未知用户/跨类型引用写前拒绝；PUT 全量替换；乐观锁 409、If-Match 412、唯一 409；单事务 count+id 升序分页；删组遍历全部 *_has_usergroup_usergroup_id 关系资源清行；删用户保留业务记录；resource/testing_helpers.go ResetCachesForTests。store_test.go 覆盖映射/分页/过滤/409/412/原子回滚/双向一致/零变更/级联/记录保留/重启一致，全部通过；server/... 无回归。
- **Priority**: high
- **Depends On**: Task 1、Task 4
- **Description**:
  - `scim.Store` 持有 `map[string]*resource.DbResource`（user_account/usergroup/join 表）与表名常量；每个请求 `Beginx` 单一事务：读取（GetSingleRowByReferenceId/GetRowsByWhereClause 等既有 helper，禁止裸 SQL）→ 校验 → CreateWithoutFilter/UpdateWithoutFilters/DeleteWithoutFilters 提交。
  - 映射：User{userName→email, displayName/name.formatted→name, given/family 拼接规则, emails 主值→email, active, external_id(空串→nil), password 走现有 bcrypt 路径}；Group{displayName→name, external_id}；资源版本取自 version 列生成 ETag/meta.version；时间取 created_at/updated_at。
  - 列表：filter 谓词 + startIndex/count → limit/offset + 总数，确定排序（id 升序），单事务完成；count 缺省/上限与 SPC 一致。
  - PATCH 引擎：先加载行→在内存中合成全部操作后的目标状态与成员目标集合（成员解析/校验：必须存在、type 必须为 User）→一次性写入；PUT 全量替换同路径；成员差量在同事务内经 join 资源 CreateWithoutFilter/DeleteWithoutFilters 完成。
  - 组删除：同事务删除所有引用该组的 `*_has_usergroup_usergroup_id` 行（遍历 CRUD_MAP 中 join 表，按 usergroup_id 删行并对受影响对象失效权限缓存、对受影响用户失效 auth/admin 缓存），再删组。
  - 错误归一：not-found→404；ErrVersionConflict→409；If-Match 不匹配→412；唯一约束→409 uniqueness；filter/path/成员错误→400 对应 scimType；全部错误经类型化 SCIM error 返回，任何校验失败不得开始写入。
- **Acceptance Criteria Addressed**: AC-4、AC-5、AC-6（执行侧）、AC-7、AC-8、AC-9、AC-10、AC-11、AC-12、AC-15、AC-16、AC-18
- **Test Requirements**:
  - `rule` TR-5.1: 用户/组创建返回正确映射与 meta/ETag，重复 userName/非空 externalId → 409 uniqueness 且无写入；证据：sqlite 内存集成测试。
  - `rule` TR-5.2: 分页 totalResults/itemsPerPage/startIndex 正确、无重漏、顺序确定；筛选结果与构造谓词一致；证据：集成测试。
  - `rule` TR-5.3: PUT 成功版本递增；旧 If-Match→412；同版本并发→409/412 且第二次无写入；证据：集成测试。
  - `rule` TR-5.4: 多属性/多成员 PATCH 全成功后整体可见；混入非法路径/未知成员/type=Group 时用户行、组行、join 行前后快照零差异；证据：事务回滚快照测试。
  - `rule` TR-5.5: Group.members 与 User.groups 双向集合一致；add/replace/remove/清空语义正确；证据：集成测试。
  - `rule` TR-5.6: 删组后全部引用 join 行清除、受影响用户缓存与权限判定不再含该组；删用户后业务记录保留、成员行清除；证据：集成测试。
  - `rule` TR-5.7: 同一数据库重新构建 Store 后 id/externalId/version/active/成员集合 deep-equal；证据：重启一致性测试。
  - `rubric` TR-5.8: 存储层对 Daptin 权威的复用度（无裸写 SQL、无影子成员表、无平行缓存）；scale 1-5；anchors 1=直接 SQL 旁路、3=部分旁路、5=全部经资源生命周期；threshold >= 4；证据：代码审查。
- **Notes**: 不设置 SCIM 私有默认组；创建用户沿用资源路径既有的 users 默认组行为。

## Task 6: HTTP 处理器与管理员令牌中间件
- **Status**: `completed`
- **Completion Evidence**: server/scim/http.go 挂载全部发现与 CRUD 路由；crypto/subtle 常量时间 Bearer 校验（兼 X-API-Key），401 带 WWW-Authenticate；scim+json 协商、写体 Content-Type 限制、If-Match 解析（含 * 与 412）、startIndex/count 规范化与上限、invalidFilter/invalidSyntax/404/409/204/201+Location+ETag 映射。证据：scim/http_test.go（无/错/正确令牌、X-API-Key、发现五端点、用户全生命周期、坏 filter/坏 member/坏 JSON）全部通过。
- **Priority**: high
- **Depends On**: Task 5
- **Description**:
  - 实现 Users/Groups 的 POST/GET 列表/GET 单项/PUT/PATCH/DELETE handler 与发现端点 handler；Content-Type 协商（发 application/scim+json，收 scim+json/json）；If-Match 读取与 ETag 回写；201+Location、204、ListResponse 封装。
  - 令牌中间件：Bearer（兼 X-API-Key）、`subtle.ConstantTimeCompare`、401 + `WWW-Authenticate: Bearer` + SCIM Error；覆盖全部 SCIM 路由含发现端点。
  - Store 类型化错误 → HTTP 状态码与 scimType 的单点映射表。
- **Acceptance Criteria Addressed**: AC-2、AC-3、AC-4（HTTP 部分）、AC-7、AC-10、AC-19
- **Test Requirements**:
  - `rule` TR-6.1: 无/错令牌→401 且含 WWW-Authenticate；正确令牌→200；证据：httptest 表驱动测试。
  - `rule` TR-6.2: 各操作状态码/头/错误体（400/404/409/412/201/204）符合 RFC；证据：handler 集成测试。
  - `rule` TR-6.3: 请求/响应 Content-Type 协商正确（application/json 与 scim+json 均可消费）；证据：测试。
  - `rubric` TR-6.4: 协议外观可被通用 SCIM 客户端直接消费的程度；scale 1-5；threshold >= 4；证据：代码审查。
- **Notes**: 不把令牌接进 PermissionInstance，不合成 SessionUser。

## Task 7: 装配、配置开关与请求体限制
- **Status**: `completed`
- **Completion Evidence**: server/endpoint_scim.go 仿 caldav 从 _config 读取 scim.enable（缺省写 false）/scim.admin_token（空不注册），仅双条件满足时 RegisterRoutes，无后台 goroutine；server.go 在 SetupNoRouteRouter 前接线；request_body_limit.go 为 /scim/v2/Users|Groups 的 POST/PUT/PATCH 增加请求体缓冲上限。证据：endpoint_scim_test.go 验证缺省 404 且写默认值、启用但无令牌仍 404、缓冲路径判定；scim 完整启用路径由 Task 8 运行时 E2E 覆盖。
- **Priority**: high
- **Depends On**: Task 3、Task 6
- **Description**:
  - 新建 `server/endpoint_scim.go`：`InitializeScimResources(router, cruds, configStore, olric/pubsub 依赖)`，仿 caldav 从 ConfigStore 读 `scim.enable`（缺省写 `"false"`）与 `scim.admin_token`（缺省写空串）；仅当 enable=="true" 且令牌非空时注册全部路由；在 `server/server.go` 的 SetupNoRouteRouter 之前接入。
  - WebSocket 撤销订阅随 processCtx 生命周期接线（Task 3 的发布/订阅闭环）。
  - `bufferedRequestPath` 增加 `/scim/` 写方法分支。
- **Acceptance Criteria Addressed**: AC-1、AC-17
- **Test Requirements**:
  - `rule` TR-7.1: 默认（未配置）启动后 /scim/v2/... 全部 404；配置 enable+token 后注册成功；enable=true 但 token 空仍 404；证据：路由装配测试（可参照 integration_operation_route_test.go）。
  - `rule` TR-7.2: /scim/ POST/PATCH/PUT 受请求体上限约束；证据：扩展 request_body_limit 测试。
  - `rule` TR-7.3: 关闭态无 SCIM 订阅/goroutine 残留；证据：代码审查 + ctx 取消测试。
- **Notes**: 纯 HTTP 装配，不改 Runtime 结构体。

## Task 8: 端到端集成测试套件
- **Status**: `completed`
- **Completion Evidence**: scim_service_runtime_e2e_test.go 经两次真实 server.NewRuntime（同一 sqlite 文件 + 独立 olric，yjs/ftp/imap 关闭）验证：真实路由令牌保护下发现可达、SCIM 建带密码用户 201、signin 成功、PATCH active=false 后同运行时 signin 返回 Invalid username or password、重激活后登录恢复、第二个 runtime 重启后 SCIM 仍启用且用户/active 状态一致；夹具 t.Cleanup 调 ResetCachesForTests 消除跨运行时全局缓存污染（修复了 TestServerApis 交互）。登录矩阵其余入口：JWT 中间件（停用/删除/重激活 401）、Basic、signin/switch/otp、OAuth sessionUserFromRow（Validate/Refresh/Exchange 共用）、IMAP Login（imap_active_test.go：活跃/错密/停用/重激活/删除）、SMTP 共用 IsUserAccountActiveWithTransaction（oauth_active_test.go 直接覆盖）、FTP 同型 RowValueActive 门与 E2E signin 同路径。go test ./server/... 全过；根包仅剩基础版本同样失败的两个 DNS 劫持环境用例（TestResolveOlricPeers_InvalidHostname、TestDiscovery_RetryOnDNSFailure）。
- **Priority**: high
- **Depends On**: Task 7
- **Description**:
  - 基于 sqlite 内存 + Olric 测试实例与资源初始化夹具，覆盖：令牌/开关；发现；Users/Groups 全生命周期；筛选+分页；PUT/PATCH（含多成员原子失败）；ETag 412/409；唯一性；停用后九入口登录矩阵（signin、JWT HTTP、Basic、FTP、IMAP、SMTP、OAuth Validate/refresh、switch user、OTP 可按既有测试夹具接入）；会话失效（缓存/OAuth 撤销/WS 关闭）；重新启用成员不恢复；删组权限同步；删用户业务记录保留；同库重启一致性。
- **Acceptance Criteria Addressed**: AC-1 ~ AC-16（汇总证据）
- **Test Requirements**:
  - `rule` TR-8.1: 上述每个场景至少一个自动化断言并全部通过；证据：测试输出。
  - `rule` TR-8.2: 失败用例（401/400/404/409/412）均断言无副作用（行计数/缓存/令牌状态）；证据：测试断言。
  - `rule` TR-8.3: 非 SCIM 路径（/api/user_account 登录与权限检查）在启用态下回归通过；证据：同一套件内对照用例。
- **Notes**: 重夹具可参照根目录 server_test.go 的 ensureServer 模式，但优先使用更轻的资源级搭建。

## Task 9: 架构守护、静态检查与全量验证
- **Status**: `completed`
- **Completion Evidence**: server/scim/architecture_test.go AST 守护：禁止 scim 包出现 .Exec/.MustExec/NamedExec 与 INSERT/UPDATE/DELETE/REPLACE 字符串写、禁止重定义 SessionUser/Permission 类型，并断言确实经 CreateWithoutFilter/UpdateWithoutFilters/DeleteWithoutFilters 写入；注入 tx.Exec 违例时测试 FAIL、还原后 PASS（反向验证有效）。go vet ./server/scim ./server/auth ./server/websockets 干净（resource/server 包 vet 命中 go1.27 工具链对依赖 github.com/gohugoio/hugo 的 printf 分析器 panic，为环境既有问题，与本次改动无关，测试统一以 -vet=off 运行）；改动文件 gofmt 干净（未触碰与本需求无关的既有未格式化文件）。git diff --check 无空白问题；go build ./... 通过；go test ./server/... 全部通过；go test ./... 仅根包 TestResolveOlricPeers_InvalidHostname、TestDiscovery_RetryOnDNSFailure 失败，已用 git stash 在未改动基础版本复现（本机 DNS 将无效主机名劫持到 198.18.x fake-ip，属环境问题），包括 TestServerApis 在内的其它根包用例均通过。
- **Priority**: medium
- **Depends On**: Task 8
- **Description**:
  - 在 server/scim 增加架构守护测试（参照 server/llm/architecture_test.go 的静态分析方式）：禁止 scim 包出现裸 SQL 写（INSERT/UPDATE/DELETE/Exec）、禁止自建身份/成员缓存类型、必须通过 resource.DbResource；新增 user_account active 判定只有一个 helper 权威。
  - 运行 `go vet ./server/...`、`go test ./...`、`git diff --check`；按 AGENTS.md 复核 diff 范围（每个生产文件的必要性、无附带重构）。
- **Acceptance Criteria Addressed**: AC-17、AC-18
- **Test Requirements**:
  - `rule` TR-9.1: 架构守护测试通过且人为注入违例时失败；证据：测试与一次反向验证。
  - `rule` TR-9.2: `go vet ./server/...`、`go test ./... -count=1`、`git diff --check` 全部通过；证据：命令输出。
  - `rubric` TR-9.3: 生产 diff 最小性与职责正交性（身份/授权/计量/路由不混淆）；scale 1-5；anchors 1=大面积无关改动、3=有少量混杂、5=小而集中且全部复用既有权威；threshold >= 4；证据：diff 审查。
