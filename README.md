# incbackup — 可验证的本地增量备份服务（仅 API）

“备份任务显示成功，恢复时却缺了一段文件”——本服务用一个硬规则拦住它：

> **快照在提交之前，必须逐个核对清单引用的每个内容块在内容仓中真实存在且长度正确；
> 恢复时再对流过的每个块和每个整文件做 SHA-256 与长度核对。**

任何一块对不上，快照就是 `failed`（或崩溃留下的 `pending`，重启后自动复验），
永远不会出现“成功”的快照恢复出残缺目录。

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
cmd/demo/main.go             端到端演示（走真实 HTTP API，含 23 项断言）
internal/repo/
  contentstore.go            内容寻址块仓（原子写、读时校验摘要、分片目录）
  manifest.go                SQLite schema 与快照状态机（pending/committed/failed）
  manifest_write.go          条目/块写入、缺块诊断查询
  meta.go                    分块多项式持久化
  policy.go                  策略/修订的草稿-发布-停用流转（条件发布、复制版本）
  freeze.go                  扫描开始时的修订冻结与逐条选择证据
internal/policy/
  policy.go                  纯词法规则校验与编译（拒 ..、绝对路径、根冲突、畸形 glob）
  walker.go                  目录排除继承的有序求值（最后命中者决定）
internal/backup/
  scan.go                    不跟随链接的目录扫描、分块、整文件摘要、写入中重读、按策略选择
  engine.go                  快照编排、提交前逐块验证、恢复与全部安全约束
  policy.go                  策略管理、复制版本、不落盘预览
  util_linux.go              O_EXCL|O_NOFOLLOW 建文件（阻止沿预置符号链接写出）
internal/api/server.go       快照 HTTP 路由
internal/api/policy.go       策略/预览/选择证据 HTTP 路由
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
| `POST /v1/snapshots` | 扫描 `root` → 落块 → **逐块验证** → 提交。带 `policy_revision_id` 时按已发布修订的冻结规则选择；不传则保持全量扫描。`finish:false`、`lose_chunks:N` 为故障演练参数 |
| `GET  /v1/snapshots` | 列出全部快照（含 failed，失败记录不删除） |
| `GET  /v1/snapshots/{id}` | 单个快照状态（含 `policy_revision_id`） |
| `GET  /v1/snapshots/{id}/missing` | **维护入口**：列出每个缺块的文件路径、SHA-256、期望磁盘路径与原因 |
| `GET  /v1/snapshots/{id}/errors` | 扫描/验证阶段的逐条错误（stage、rel_path、chunk_digest） |
| `GET  /v1/snapshots/{id}/selection` | **选择证据**：冻结的修订与规则文档，每条被排除路径命中的全部规则及决定性规则 |
| `POST /v1/snapshots/{id}/verify` | 对 pending 快照重新执行逐块验证并提交/判失败 |
| `POST /v1/snapshots/{id}/restore` | 恢复到**全新**目录，返回逐文件长度+摘要+块数报告 |
| `POST /v1/recover` | 复验所有 pending 快照（服务启动时也会自动执行） |

### 版本化扫描策略

规则只按 slash 相对路径做词法匹配（glob：`*` 不跨 `/`，`**` 匹配零或多个完整段，
结尾 `/` 表示仅目录）。禁止 `..`、绝对路径、`.`、空段与畸形 glob——规则无法借词法越出根目录；
扫描仍不跟随符号链接。规则按顺序求值，**最后命中的规则决定**（例外放在要覆盖的排除之后）；
一旦某目录被排除，其子树全部继承排除（include/exception 不能穿过已关闭的目录复活文件）；
存在 include 规则时，未命中任何规则的路径默认排除。

| 方法与路径 | 说明 |
|---|---|
| `POST /v1/policies` | 创建策略（lineage）与首个**草稿**修订；非法规则一次性全部返回 |
| `GET  /v1/policies`、`GET /v1/policies/{id}` | 列出策略 / 查看策略及其全部修订 |
| `POST /v1/policies/preview` | 只按给定规则对 `root` 词法预览：每路径判定、命中规则与计数，不落盘 |
| `POST /v1/policies/{id}/revisions` | 复制某修订（`source_revision_id`）或给定规则，新建下一版草稿 |
| `PUT  /v1/revisions/{rid}` | 修改**草稿**；已发布/停用修订不可编辑，只能复制 |
| `POST /v1/revisions/{rid}/publish` | 草稿→发布；条件 UPDATE 保证并发发布同一草稿只有一个成功 |
| `POST /v1/policies/{id}/retire` | 停用 lineage；旧快照保留各自冻结副本，仍可恢复 |

策略语义保证：**快照只能引用已发布修订**；快照行与修订冻结副本（修订号、规则全文与顺序）
在扫描开始的同一个事务写入，因此并发发布/停用不会改变进行中的扫描范围；修订一旦被快照使用即
不可编辑，改动必须复制为新版本；扫描即使失败也保留冻结副本、逐条选择证据和可定位的错误。

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

版本化策略的典型流程（草稿 → 预览 → 发布 → 按修订快照 → 复制新版本 → 停用）：

```bash
# 1. 建策略 + 草稿：排除可再生临时文件，但保留指定的一个
curl -s -XPOST localhost:8090/v1/policies -d '{
  "name":"no-tmp",
  "rules":[
    {"action":"exclude",  "pattern":"*.tmp"},
    {"action":"exception","pattern":"keep.tmp"}
  ]}'
# {"policy_id":1,"revision":{"id":1,"number":1,"status":"draft", ...}}

# 2. 发布前预览（不落盘）
curl -s -XPOST localhost:8090/v1/policies/preview -d '{"root":"/srv/data","rules":[...]}'

# 3. 发布；然后按修订建快照
curl -s -XPOST localhost:8090/v1/revisions/1/publish
curl -s -XPOST localhost:8090/v1/snapshots \
  -d '{"root":"/srv/data","policy_revision_id":1}'

# 4. 日后回答“这个文件为什么没进清单”
curl -s localhost:8090/v1/snapshots/8/selection
# {"frozen_revision":{"revision_id":1,"revision_number":1,"rules":[...]},
#  "selection":[{"rel_path":"scratch.tmp","included":false,
#    "hits":[{"order":0,"action":"exclude","pattern":"*.tmp","decisive":true}]},
#   {"rel_path":"keep.tmp","included":true,
#    "hits":[{"order":0,"action":"exclude","pattern":"*.tmp"},
#            {"order":1,"action":"exception","pattern":"keep.tmp","decisive":true}]}]}

# 5. 已发布修订不可改：复制成新草稿再改、再发布；旧快照继续指向修订 1
curl -s -XPOST localhost:8090/v1/policies/1/revisions -d '{"source_revision_id":1}'
curl -s -XPUT  localhost:8090/v1/revisions/2 -d '{"rules":[...]}'
curl -s -XPOST localhost:8090/v1/revisions/2/publish
```

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
8. **排除可证明**：按策略的快照在扫描开始时冻结已发布修订（修订号、规则全文与顺序），并逐条持久化
   被排除路径命中的规则（含决定性规则与祖先目录继承来源）。无策略请求仍为完整全量扫描，不产生排除。
9. **规则不越界且版本不可变**：规则只做词法匹配，拒绝 `..`、绝对路径、根冲突 `.` 与畸形 glob，
   扫描不跟随链接；修订被快照引用后不可编辑（改动只能复制新版本），草稿/停用修订不能产生 committed 快照。

## 演示会依次证明

1. 基线快照 → 恢复到新目录，逐文件核对摘要与长度（含空文件、0750 脚本、符号链接）；
2. 在 160KB 文件中部改 8 字节：**新块=1，复用旧块=4**，恢复结果与源一致；
3. 恢复到已存在目录 → `409 target_exists`；
4. 写入在重读窗口内停止 → 重读后成功；持续写入 → 3 次重读后拒绝并点名；
5. `lose_chunks:1` 模拟提交中断 → `failed` + `/missing` 给出精确缺块，旧快照仍可恢复；
6. 指向根目录外的符号链接 → 恢复 `422`，半成品目录回滚，外部文件不被触及；
7. `finish:false` 制造 pending → 重启服务后自动复验为 committed；
8. 版本化策略：非法越界规则拒绝；草稿不能建快照；`*.tmp` 排除 + `keep.tmp` 例外的两类证据；
   恢复树符合规则；复制修改后旧快照保留旧证据；两个窗口并发发布只接受一个；停用后旧快照仍可恢复。
