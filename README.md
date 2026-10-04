# incbackup — 可验证的本地增量备份服务（仅 API）

“备份任务显示成功，恢复时却缺了一段文件”——本服务用一个硬规则拦住它：

> **快照在提交之前，必须逐个核对清单引用的每个内容块在内容仓中真实存在且长度正确；
> 恢复时再对流过的每个块和每个整文件做 SHA-256 与长度核对。**

任何一块对不上，快照就是 `failed`（或崩溃留下的 `pending`，重启后自动复验），
永远不会出现“成功”的快照恢复出残缺目录。

研发目录里总有可再生成的临时文件（`*.tmp`、`cache/`……），排除它们是合理需求；
但“排除后显示成功”不能变成无法证明范围的静默漏备。所以扫描策略是**版本化**的：
每条被排除的路径和命中它的规则都会持久化，日后能逐条说明“这个文件为什么不在清单里”。

## 技术栈

| 组件 | 选择 | 用途 |
|---|---|---|
| 分块 | `github.com/restic/chunker`（Rabin 指纹内容定义分块） | 小改动只产生 1 个新块，其余块哈希相同直接复用 |
| 清单 | SQLite（`modernc.org/sqlite`，纯 Go，无 CGO） | 快照、条目、块索引、错误记录 |
| 内容仓 | 独立目录 `<repo>/chunks/ab/cdef…` | SHA-256 内容寻址、去重、只读不可变 blob |
| 接口 | 本地 HTTP API（默认 `127.0.0.1:8090`） | 无 UI、无鉴权，设计为只监听本地 |

分块多项式持久化在 `meta` 表中，跨快照/跨重启保持一致——否则边界漂移会让增量失效。

## 目录结构

```
cmd/backupd/main.go          HTTP 服务（启动时自动复验 pending 快照）
cmd/demo/main.go             端到端演示（走真实 HTTP API，含 59 项断言）
internal/repo/
  contentstore.go            内容寻址块仓（原子写、读时校验摘要、分片目录）
  manifest.go                SQLite schema 与快照状态机（pending/committed/failed）
  manifest_write.go          条目/块写入、缺块诊断查询
  policy.go                  策略/修订/规则存储、发布状态机、冻结规则与选择证据
  meta.go                    分块多项式持久化
internal/backup/
  scan.go                    不跟随链接的目录扫描、分块、整文件摘要、写入中重读
  policy.go                  词法规则校验与匹配（include/exclude/exception，有序求值）
  engine.go                  快照编排、提交前逐块验证、恢复与全部安全约束
  util_linux.go              O_EXCL|O_NOFOLLOW 建文件（阻止沿预置符号链接写出）
internal/api/
  server.go                  快照/恢复路由
  policy.go                  策略发布、预览、选择证据路由
```

## 快速开始

```bash
go run ./cmd/demo            # 端到端演示（临时目录，自动清理）
go test ./...                # 单元测试
go run ./cmd/backupd --repo ./backup-repo --addr 127.0.0.1:8090
```

## HTTP API

| 方法与路径 | 说明 |
|---|---|
| `POST /v1/snapshots` | 扫描 `root` → 落块 → **逐块验证** → 提交。`finish:false`、`lose_chunks:N` 为故障演练参数 |
| `GET  /v1/snapshots` | 列出全部快照（含 failed，失败记录不删除） |
| `GET  /v1/snapshots/{id}` | 单个快照状态 |
| `GET  /v1/snapshots/{id}/missing` | **维护入口**：列出每个缺块的文件路径、SHA-256、期望磁盘路径与原因 |
| `GET  /v1/snapshots/{id}/errors` | 扫描/验证阶段的逐条错误（stage、rel_path、chunk_digest） |
| `POST /v1/snapshots/{id}/verify` | 对 pending 快照重新执行逐块验证并提交/判失败 |
| `POST /v1/snapshots/{id}/restore` | 恢复到**全新**目录，返回逐文件长度+摘要+块数报告 |
| `POST /v1/recover` | 复验所有 pending 快照（服务启动时也会自动执行） |
| `POST /v1/policies` | 创建策略（草稿修订 1），`publish:true` 可立即校验并发布 |
| `GET  /v1/policies` / `GET /v1/policies/{id}` | 策略与其全部修订、有序规则 |
| `POST /v1/policies/{id}/revisions` | 从已发布头复制新草稿（`from_revision` 可指定来源） |
| `PUT  /v1/policies/{id}/revisions/{rev}/rules` | 替换**草稿**的规则集（已发布修订 409 不可变） |
| `POST /v1/policies/{id}/revisions/{rev}/publish` | 校验并原子发布；基线已移动则 `409 publish_conflict` |
| `POST /v1/policies/{id}/revisions/{rev}/disable` | 停用修订（历史保留，新快照不可再引用） |
| `POST /v1/policies/preview` | 干跑预览：按 `rules` 或 `policy_id` 列出会包含/排除什么，不落盘 |
| `GET  /v1/snapshots/{id}/selection` | **选择证据**：冻结的策略修订+规则顺序，每条被排除路径及命中的规则、每条例外命中 |

### 典型请求

```bash
curl -s -XPOST localhost:8090/v1/snapshots \
  -d '{"root":"/srv/data","message":"nightly"}'
# 201 {"snapshot_id":7,"status":"committed","chunks_new":1,"chunks_referenced":5}

curl -s localhost:8090/v1/snapshots/7/missing
# {"snapshot_id":7,"status":"failed","missing":[
#   {"rel_path":"app.log",
#    "chunk_digest":"d2a8d66b…",
#    "expected_blob_path":"/…/chunks/d2/a8d66b…",
#    "reason":"chunk blob missing or length mismatch in content store"}]}

curl -s -XPOST localhost:8090/v1/snapshots/7/restore \
  -d '{"target":"/restore/2026-09-29"}'
```

### 扫描策略请求

```bash
# 排除 *.tmp，但例外保留 keep.tmp；创建即发布
curl -s -XPOST localhost:8090/v1/policies -d '{
  "name": "dev-tmp", "publish": true,
  "rules": [{"action":"exclude","pattern":"*.tmp"},
            {"action":"exception","pattern":"keep.tmp"}]}'

# 预览：这个策略会对 /srv/data 选进什么、排除什么（不写任何东西）
curl -s -XPOST localhost:8090/v1/policies/preview \
  -d '{"root":"/srv/data","policy_id":1}'

# 按已发布修订建快照（缺省取最新已发布修订）
curl -s -XPOST localhost:8090/v1/snapshots \
  -d '{"root":"/srv/data","policy_id":1,"message":"nightly"}'
# 201 {"snapshot_id":9,"status":"committed","policy_id":1,"policy_revision":1,...}

# 事后追问：tmp/debug.tmp 为什么不在清单里？
curl -s localhost:8090/v1/snapshots/9/selection
# {"policy":{"policy_id":1,"revision":1,"rules":[
#    {"seq":1,"action":"exclude","pattern":"*.tmp"}, ...]},
#  "selection":[
#    {"rel_path":"tmp/debug.tmp","decision":"excluded","rule_seq":1,"rule_pattern":"*.tmp"},
#    {"rel_path":"tmp/keep.tmp","decision":"exception","rule_seq":2,"rule_pattern":"keep.tmp"}]}
```

## 扫描策略规则

- **三种动作**：`include`（纳入）、`exclude`（排除）、`exception`（例外——把前面被
  exclude 命中的路径重新纳入）。规则**按顺序求值，最后命中者生效**；没有 include
  规则时默认全量包含，一旦出现 include 规则，未命中的路径默认排除。
- **纯词法匹配**：模式只对“相对快照根的 slash 路径”做 glob 匹配，不解析 `..`、
  不接受绝对路径、不解析符号链接；扫描本身也从不跟随链接。含 `/` 的模式锚定于根
  （`docs/*.tmp`），不含 `/` 的模式匹配任意层级的 basename（`*.tmp`）；`**` 跨目录
  层级，`结尾/` 只匹配目录。排除目录即剪枝整个子树。
- **非法规则在发布时被拒**：`..`、绝对路径、空段、`.` 段、反斜杠、 malformed glob，
  以及任何会命中根目录本身的规则（`*`、`**`……）都返回 `422 invalid_rules`；
  扫描开始时还会对冻结规则再校验一次，非法规则无论如何都产生不了 committed 快照。
- **生命周期**：草稿 → 发布 → 停用。快照只能引用**已发布**修订；已发布修订不可变，
  修改必须先复制出新草稿再发布。发布是原子条件更新：两个窗口基于同一已发布头并发
  发布时只有一个成功，另一个得到 `409 publish_conflict`，历史保持线性。
- **扫描开始即冻结**：快照行在扫描开始前就持久化所引用的修订号与完整有序规则副本，
  并发的发布/停用不会改变进行中的扫描范围；即使扫描中断（pending/failed），冻结的
  策略与失败原因也一并保留。
- **选择证据**：每条被排除的路径连同命中的规则（序号、动作、模式）写入
  `snapshot_selection`；被例外重新纳入的路径也记录为例外命中。被剪枝目录记一条目录
  证据（子树不再逐条展开）。无策略的快照没有任何证据行——行为与旧版完全一致。

## 关键正确性保证

1. **完成前验证所有内容块**：`snapshots.status` 只有 pending→committed/failed。
   提交前 `FindMissingChunks` 同时检查（a）清单里是否有块行、（b）blob 是否存在且长度一致；
   每块在恢复读取时再做流式 SHA-256 校验，每个文件组装后比对整文件摘要与总长度。
2. **扫描中正在写入的文件**：读取前后比对 size+mtime，并增加读后置静窗口
   （防止小文件恰好在两次 append 之间被整文件读完）。检测到变化→整块重读（最多 3 次）；
   仍在变→快照 `failed`，错误明确点名文件，绝不猜测版本。
3. **权限与符号链接本身保留**：保存并恢复目录/文件权限位、属主（root 时）、mtime；
   符号链接存的是链接本身与目标字符串，扫描与恢复均不跟随。
4. **不越界**：恢复前校验清单路径无绝对路径/`..`；符号链接目标按词法解析，解析后必须仍在恢复根内；
   任何现存祖先目录是符号链接一律拒绝；Linux 下用 `O_EXCL|O_NOFOLLOW` 建文件。
5. **不覆盖**：恢复目标已存在（任何类型）直接 `409`；恢复中途失败自动删除半成品目录。
6. **空文件**：长度 0、整文件摘要 `e3b0c442…`、0 个内容块，正常备份与恢复。
7. **失败可定位**：failed/pending 快照永久保留，`/missing` 直接给出“哪个文件的哪个块该在哪个路径”，
   而不是只看到队列空了。
8. **排除可证明**：带策略的快照持久化冻结的规则副本与每条排除/例外证据
   （`/v1/snapshots/{id}/selection`），“排除后显示成功”永远能事后核对范围；
   不传策略的请求不做任何过滤，与旧版行为逐字节一致。

## 演示会依次证明

1. 基线快照 → 恢复到新目录，逐文件核对摘要与长度（含空文件、0750 脚本、符号链接）；
2. 在 160KB 文件中部改 8 字节：**新块=1，复用旧块=4**，恢复结果与源一致；
3. 恢复到已存在目录 → `409 target_exists`；
4. 写入在重读窗口内停止 → 重读后成功；持续写入 → 3 次重读后拒绝并点名；
5. `lose_chunks:1` 模拟提交中断 → `failed` + `/missing` 给出精确缺块，旧快照仍可恢复；
6. 指向根目录外的符号链接 → 恢复 `422`，半成品目录回滚，外部文件不被触及；
7. `finish:false` 制造 pending → 重启服务后自动复验为 committed；
8. 策略排除 `*.tmp` + 例外保留 `keep.tmp`：预览与快照证据同时列出排除/例外两类命中，
   恢复树符合规则；
9. `..`、绝对路径、`*`/`**` 等与根冲突的规则 → `422 invalid_rules`，未发布修订 → `409`，
   均不产生 committed 快照；
10. 策略复制修改后旧快照保留旧证据；两个草稿并发发布只接受一个；扫描中断的 pending
    快照仍保留冻结策略并在恢复后 committed。
