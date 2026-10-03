# 独立审查记录 - Sieve 规则与 ManageSieve 服务

审查范围：对照 spec.md 的 AC-1..AC-17，独立检查代码、测试与运行证据。审查者必须独立运行验证命令，不信任实现者自述。

## 审查命令建议（独立执行）
- `go build ./...`
- `cd third_party/go-sieve && go vet ./... && go test ./...`
- `go vet ./server/resource/`
- `go vet -printf=false ./server/`（printf 分析器在 hugo 类型上的崩溃为工具链既有 bug，需判断与本次改动无关）
- `go test ./server/resource/ ./server/ ./server/actions/ -count=1 -vet=off`
- `git diff --check`；`git diff` 逐文件审查

## Checkpoints
- [ ] CP-1 (AC-1)：脚本持久化——重开连接后名称与内容一致。证据：sieve_store_test.go。
- [ ] CP-2 (AC-2)：唯一活动脚本原子切换/清除/重启保持。证据：sieve_store_test.go 中 switch/clear/restart 用例。
- [ ] CP-3 (AC-3)：ManageSieve 认证——错误密码/正确密码；认证权威为 mail_account bcrypt。证据：sieve_manage_server_test.go。
- [ ] CP-4 (AC-4)：LISTSCRIPTS/GETSCRIPT/PUTSCRIPT/RENAMESCRIPT/DELETESCRIPT 符合 RFC 5804（SCRIPTEXISTS、NONEXISTENT、逐字节一致）。
- [ ] CP-5 (AC-5)：语法错误（含位置）与未知扩展在 PUTSCRIPT/CHECKSCRIPT/SETACTIVE 下被拒。
- [ ] CP-6 (AC-6)：header/address/size/body 条件命中与隐式 keep。
- [ ] CP-7 (AC-7)：keep/fileinto（自动建箱）/discard/stop 语义。
- [ ] CP-8 (AC-8)：redirect 入 outbox；重复目的一条；回原 sender/本人地址不排队；数量/跳数限制。
- [ ] CP-9 (AC-9)：失败整体回滚——mail/mail_box/outbox/vacation_track 无残留、无 UID 推进。
- [ ] CP-10 (AC-10)：vacation 对列表/自动/空 sender 不回复。
- [ ] CP-11 (AC-11)：vacation 按 sender/handle 周期去重，重启与并发不重复。
- [ ] CP-12 (AC-12)：vacation 回复格式——空 MAIL FROM、Auto-Submitted、In-Reply-To/References、主题/正文。
- [ ] CP-13 (AC-13)：无活动脚本账户默认路径行为不变（INBOX/Spam 与元数据）。
- [ ] CP-14 (AC-14)：IMAP/JSON:API 回归套件通过。
- [ ] CP-15 (AC-15)：TLS 前无 AUTH、STARTTLS 后可认证；Close 干净。
- [ ] CP-16 (AC-16, rubric)：架构契合度评分 >=4（无影子存储/第二身份/兼容分支；全部经 DbResource 与 outbox）。
- [ ] CP-17 (AC-17, rubric)：测试覆盖质量评分 >=4（每条 AC 有证据，含失败/并发边界）。

## 审查结论
第三轮独立复审结论：**PASS**（2026-10-02）。CP-1..CP-15 全部通过，CP-16/17 评分 5/5；审查提出的 3 个 Minor 加固残留已随后修复：
- readQuoted 增加 1 MiB 字节上限；
- literal 长度数字串循环内 >20 即拒绝；
- 新增 TestManageSieveRejectsOversizedToken；
- 另清理了 autoReplySpec.mime 死字段。
最终验证：go vet 通过（printf 分析器在 hugo 类型上的崩溃为工具链既有 bug，以 -printf=false 验证）；server/resource、server、server/actions 测试全绿；ManageSieve/Vacation/Sieve 子集 `-race` 通过；git diff --check 通过。
