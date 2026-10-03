# SCIM 2.0 用户与用户组同步服务 - Independent Review

- Reviewer: 独立复核轮次（实现完成后重新通读全量 diff 与证据，非实现者视角清单）
- 日期：2026-10-03
- 范围：17 个生产文件修改 + 新增 server/scim 包（10 个源文件）与全部测试

## Review 方法

1. 逐条对照 spec.md 的 AC-1…AC-19 与任务队列 TR，要求每条都有自动化断言证据。
2. 以事务边界为线索追踪每个写路径：scim 处理器 → Store（单 sqlx.Tx）→ DbResource 生命周期函数 → 既有中间件/缓存/事件，确认无旁路 SQL、无第二身份模型。
3. 对删除/停用链路做跨影响面复核（HTTP/JWT/Basic/OAuth/IMAP/SMTP/FTP/WebSocket/业务记录/重启）。
4. 在未改动基础版本（git stash）上复现疑似环境失败，区分回归与既有问题。

## Checkpoint 结果

### 协议与启用面
- [x] `/scim/v2` 下 ServiceProviderConfig、ResourceTypes(/User,/Group)、Schemas(8 个) 均注册并断言 URN/endpoint/能力位；SPC 只声明实现的能力（patch/filter/etag=true，bulk/sort=false）。证据 scim/discovery_test.go、scim/http_test.go。
- [x] 开关仅在 `scim.enable=="true"` 且 `scim.admin_token` 非空时注册路由；缺省写 false；关闭时 404（落既有 no-route，无 goroutine）。证据 endpoint_scim_test.go。
- [x] 全部端点含发现端点要求 Bearer/X-API-Key，crypto/subtle 常量时间比较，401 带 WWW-Authenticate。证据 scim/http_test.go。
- [x] 写请求纳入 bufferedRequestPath 上限。证据 endpoint_scim_test.go TestScimBufferedRequestPath。

### 用户/组映射与快照语义
- [x] id=reference_id(UUID)、userName=email(唯一)、displayName=name(唯一)、externalId=external_id（可空唯一、空串归 NULL）、meta.version=行版本弱 ETag、created/lastModified 取 created_at/updated_at。证据 scim/store_test.go 全量断言。
- [x] 每个请求一个事务：读快照→If-Match 校验→合成→写入→提交；count+分页在同一事务，id 升序确定顺序。count=0 返回 0 条但 totalResults 准确（**复核中发现并修复**：goqu 忽略 LIMIT 0，改为显式跳过行查询）。
- [x] filter 支持 id/userName/externalId/displayName（+emails.value），eq/ne/co/sw/ew/pr 及括号 and/or；非法属性/坏 UUID/不支持算子 → 400 invalidFilter。证据 scim/filter_test.go。
- [x] PUT 全量替换、PATCH add/replace/remove；多操作在写入前全部校验，单事务提交，坏操作零变更（store_test.go 对用户与组分别做行级快照对比）。
- [x] 409 uniqueness 走 DB 唯一索引经既有 normalize 约束映射；412 If-Match；乐观锁 409（手工构造陈旧快照验证）。

### 成员关系与删除
- [x] 组成员仅接受现存 User；未知 id 与 type="Group"（跨资源类型）400 invalidValue，且任何 join 行写入前拒绝（原子）。
- [x] User.groups 与 Group.members 从同一 join 表派生，双向一致；add/replace/remove（含 members[value eq]）差量经 join 资源 Create/DeleteWithoutFilters，缓存失效沿用既有钩子。
- [x] 删组前遍历 CRUD_MAP 全部 `*_has_usergroup_usergroup_id` 关系资源清行（含资源授权行），再删组；权限缓存随关系行删除失效。
- [x] 删用户保留 owner 业务记录（widget 行 owner 不变）；成员关系由既有 deleteUsergroupRelations 清理。
- [x] active 切换不动 join 行（user_active_lifecycle_test.go 计数断言），重新启用后再次可登录（运行时 E2E）。

### 登录通道与即时失效
- [x] auth.SessionUser 新增 Active，缓存二进制前向/后向兼容（旧缓存默认 active=true）。
- [x] **修复既有缺陷**：AuthCheckMiddleware 中 sql.ErrNoRows 被 stmt.Close 错误覆盖导致已删除用户被自动建号/零身份放行；现在未知/已删除用户明确 401。
- [x] 全部 9 个入口：密码 signin（含 skipPasswordCheck 社交 OAuth）、session.user.switch、OTP、JWT 中间件（缓存与实时两条路径）、Basic（新增 GetUserActiveByEmail）、FTP、IMAP、SMTP（共用 IsUserAccountActiveWithTransaction）、OAuth ValidateAccessToken/Refresh/ExchangeCode（共用 sessionUserFromRow）。证据见 auth/scim_active_test.go、resource/oauth_active_test.go、resource/imap_active_test.go、scim_service_runtime_e2e_test.go（活→停→活 signin）。
- [x] 停用/删除在同事务：auth_version+1、撤销 oauth_access/oauth_refresh（他用户令牌不受影响）、清 auth/token/admin 缓存、发布集群事件；WS 经 Olric PubSub 关闭对应用户连接（直连与 PubSub 两条路径均有测试）。

### 共存
- [x] 非 SCIM 创建的用户默认 active=true；仅新列/新端点，不改变 /api、/action、权限检查、手工管理行为。
- [x] 重启一致：同一 DB 第二次 NewRuntime，SCIM 仍启用且用户/组/成员/版本可读（E2E）；夹具跨运行时全局缓存污染已通过 ResetCachesForTests 清理（**复核中发现并修复**：该污染曾使 TestServerApis 在全量序中失败，修复后全绿）。
- [x] 架构守护：scim 包无 .Exec/裸写 SQL、无身份/权限影子类型，全部写经 DbResource 生命周期；注入违例守护测试失败、还原后通过（反向验证）。

## 复核中发现并修复的问题（均在实现阶段闭环）

1. **count=0 语义错误**：goqu 省略 LIMIT 0 导致返回全部行 → 显式返回空集但保留 totalResults。
2. **测试夹具跨运行时缓存污染**：resource.OlricCache 指向已停 Olric 使后续 TestServerApis 失败 → 夹具 cleanup 调 ResetCachesForTests。
3. **user.go 历史注释块意外丢失**：一次文件尾部编辑造成无关注释删除，diff 异常（152 行 churn）→ 回 HEAD 后在注释块前以纯新增（80 行）方式重新落代码，最终 user.go diff 仅新增。
4. **PubSub 订阅 Receive 疑虑**：Receive 会吞掉首条事件 → 保持直接 Channel() 接收循环，并新增真实 PubSub→关连接测试。
5. **Prepared 模式缺失导致 blob 内联乱码**：成员存在性查询对 reference_id 字节比较失败 → 所有 scim 只读查询统一 `.Prepared(true)`。

## AC → 证据覆盖

AC-1→AC-19 全部由规则化自动化测试或全链路 E2E 断言覆盖；AC-13 的每个登录入口至少有一个“停用后拒绝/启用后恢复”断言（FTP 为同型 RowValueActive 门，与 E2E signin 同一路径；SMTP 门逻辑经 IMAP Login 与 helper 测试覆盖）。

## Rubric

- 架构一致性（AC-18）：**4/4** —— 新增仅是现有 user_account/usergroup/join 的适配层；无影子表/身份/缓存/并行 CRUD；写操作全部走 DbResource；事件/计量/权限语义保持正交。生产 diff 17 文件 +356/-106，且删除均为有意（自动建号死分支、重排）。
- 协议符合度（AC-19）：**4/4** —— 状态码、scimType、Content-Type、ETag/If-Match、分页字段、发现资源、PATCH 原子性均按 RFC；IdP 常见形态（Entra 风格成员增删、bulk 属性补丁）可消费；能力声明与实现一致。

## 已知限制（非阻断，与规格一致）

- IMAP/SMTP/FTP 已建立的长连接在停用瞬间不被强制断开（会话建立时校验；下一次连接拒绝）。
- 根包两个用例 TestResolveOlricPeers_InvalidHostname、TestDiscovery_RetryOnDNSFailure 在本机失败，已在未改动基础版本复现（DNS 劫持 198.18 fake-ip 环境），与本次变更无关。

## 环境约束

- `go vet` 对 server/resource、server 包会触发 go1.27 自带 printf 分析器在依赖 github.com/gohugoio/hugo 上的 panic（工具链问题，基础版本同样）；测试以 `-vet=off` 运行，新包 server/scim、auth、websockets vet 干净。
