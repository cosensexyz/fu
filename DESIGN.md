# fu v1 系统设计

- 状态：已评审（2026-08-01 讨论定稿）
- 配套：`SPEC.md` 回答 Why 与 What；本文档回答 How
- 技术选型：Go（单二进制）· go-git（内嵌 git）· cobra（CLI 框架）· golang.org/x/text（Unicode 规范化与折叠基础）

## 0. 本轮交付范围

本文件描述 fu v1 的完整设计。**plan 1 交付六条命令**：`init`、`new`、`list`、`show`、`enable`、`disable`；
**plan 2 追加四条**：`add`（git URL 与本地目录）、`adopt`（逐项与整目录两种形态）、`rm`、`gc`（仅裁剪已完成事务 journal）；
**本轮追加两条**：`status`（只读一致性检查，见 §2）、`restore`（链接层：按期望重建缺失、清理断链，并报告——不触碰——store 工作区的未提交改动；工作区复位到最近一次提交的另一层由 `--hard` 交付，见 §6「崩溃残留」条目）。
**其后追加一条**：`revert`（快照式回退最近 n 次操作，经标准写流水线调用，见 §4 revert n 行与 §6「崩溃残留」条目）。
`update`、`outdated`、`log`、`commit`、`pull`、`push`、`clone`、`remote`、`agent` 均未交付。
（以上九条均为 SPEC §5.1 列出、但尚未注册进 `internal/cli/root.go` 命令树的命令。）

因此文中凡描述未交付部分的段落一律标注 **（设计，未实现）**，读者不必从"已知缺口"的缺省推断某处是否已经存在。
标注出现在：§2 的 `status` 远端核对（ls-remote）、§3 的 `update` 来源消费、§4 的 pull / push 行与 git 来源取得段落中的 `update` / `outdated`、§6 的 `fu commit` 条目、§8 测试表中依赖未交付命令的行。
"已知缺口"只记录**已交付部分**中已知但未关闭的问题，不重复记录尚未开始的工作。

## 1. 模块划分

```
fu/
├── cmd/fu/main.go        — 入口，装配子命令
├── internal/cli/         — 各子命令实现：参数解析、调用下层、呈现输出
├── internal/store/       — store 布局、fu.yaml 读写、git 操作封装（go-git）
├── internal/skill/       — SKILL.md 解析与校验、目录扫描（多 skill 仓库）
├── internal/source/      — 来源抽象：git 来源、本地目录来源；解析与浅克隆、锁定信息的取得（plan 2 已实现）
├── internal/agent/       — 适配器：检测、skills 目录；claude、codex 两实现
└── internal/engine/      — 对账引擎与写操作流水线（业务编排唯一所在）
```

依赖图保持无环。本轮实际的非测试依赖为
`cli → engine`、`engine → {store, source, agent, skill}`、`source → store`、`store → skill`。
`engine.Application` 是完整产品边界：负责 FU_HOME/store 的初始化与打开、agent 检测、读模型构造、来源准备、写操作编排和持久阶段结果；
CLI 只解析参数、完成交互选择，并格式化 Application 返回的结构化结果。架构测试以 AST 遍历**整个 module** 的非测试 Go 文件（`WalkDir`，按目录豁免 engine 及其依赖的 `store`/`agent`/`skill`/`source` 五个核心包，规则约束的是这条边界**之上**的代码），禁止豁免范围之外的任何包引入对 `store`、`agent`、`skill` 或 `source` 的直接依赖，并以「检查文件数为零即失败」防止它静默地什么都没看。早先只解析 `internal/cli` 一个目录，`cmd/fu` 与将来平级于 cli 的 `fu web` 都不在其中。

- engine 是唯一的业务编排层；cli 不含任何决策逻辑。
- 将来 `fu web` 加入时作为与 cli 平级的包，调用同一 engine——SPEC §5.2 "为 GUI 预留同一调用面"在此兑现。

### 磁盘布局

```
$FU_HOME/                 (默认 ~/.fu)
├── store/                (git repo，多机同步范围)
│   ├── skills/<name>/    (skill 实体)
│   └── fu.yaml           (开关状态、来源锁定、内容摘要)
├── staging/              (写操作准备区；与 store 同文件系统，保证 rename 原子且不跨设备)
├── recovery/             (WAL revision/终态/裁剪记录、事务载荷归档、adopt-link-<digest>.json、配置交换记录与归档；其中 rm 载荷与配置交换的记录/终态/归档在事务完成后回收，adopt 归档、new / add 回滚载荷及其归档与 adopt-link 记录仍保留；store 之外，不入版本与同步)
└── fu.lock               (文件锁；本机自用，不入版本)
```

锁文件置于 store 之外，符合 SPEC "store 之外为本机自用" 的划分。

## 2. 对账引擎

整个 fu 归结为一个纯函数加一个执行器：

```go
// desired：由 fu.yaml + 开关规则计算（agent 覆盖优先，否则跟随全局）
// actual： 扫描各 agent skills 目录所得
// ActionType：CreateLink | RemoveLink | ReportConflict | ReportForeign |
//             ReportDisabledForeign | ReportReserved | ReportInvalid |
//             ReportMissing | ReportSkipped | ReportFailed
func Diff(desired Desired, actual Actual) []Action
```

- **desired 计算**：`effective(skill, agent) = overrides[agent] ?? enabled`；
- **所有权判定**：条目须为 symlink，取其 readlink 原始值（不解析）；经**路径规范化**后与 `$FU_HOME/store/skills/<条目自身的名字>` **精确相等**（无论目标是否存在）= fu 所有。判据是恒等而非包含：fu 写下的每条链接都是 `Store.SkillsDir()` 拼上 skill 名、且条目就叫这个名字，故上式对 fu 自己的链接恒成立，对别的一概不成立。其余一切条目（真实目录、文件、指向别处的链接，无论是否断链）= 未纳管，绝不触碰。不设本机 link manifest——它是会漂移的第三份状态；
- **恒等判据取代包含判据的由来**：前五轮问的是"目标是否落在 `store/skills` 之内"，反复收窄的只是**目标允许如何解析**，判据本身的形状始终未动。第六轮指出了这个形状的代价：用户自建的 `ln -s ~/.fu/store/skills/alpha ~/.claude/skills/notes`——无别名、无转接——本身就满足包含，于是被下一次写操作删除。要求**条目名与目标叶子分量一致**以不变量结束此类讨论：fu 没有任何操作会创建"名为甲、指向 skill 乙"的链接，故二者不一致即证明该链接非 fu 所创；要求父目录**恰为** `store/skills`（而非其任意祖先）则同理处理指向 skill 目录**内部**的目标。两处早先各需专门分支的形态由此自然消解、不再需要各自的代码：指向 skills 根本身的整集别名（`ln -s ~/.fu/store/skills ~/.codex/skills`，SPEC §2.3 所引社区惯例）与 `…/skills-foreign` 前缀陷阱，都不可能等于 `store/skills/<条目名>`；
- **仍不可区分、且明确接受的残留**：用户以 fu 会用的**同一个名字**、指向 fu 会指的位置所建的链接（`ln -s ~/.fu/store/skills/alpha ~/.claude/skills/alpha`，含经 `$FU_HOME` 别名到达的同名写法）与 fu 自己写下的逐字节相同。区分二者只能依靠一份"fu 创建了什么"的记录，即上文否定的 link manifest。`TestKnownResidualSameNameLinkIsTreatedAsFuOwned` 钉住此行为；
- **路径规范化的确切含义**：目标值为相对路径时原样使用（仅 `filepath.Clean`），不做任何文件系统解析——解析相对路径等于相对 fu 进程自身的当前工作目录求值，与"是否属于 store"无关。目标值为绝对路径时，其**目录部分**须依次通过两道检查才被解析，两道缺一不可：其一，未解析的原始文本以 store 的 skills 目录自身的路径尾部两级分量（今天是 `store`、`skills`，读自 `Store.SkillsDir()` 的实际布局，不硬编码字面值）结尾；其二，剥去这两级分量后剩余的原始前缀，其解析结果与 `$FU_HOME` 的解析结果为同一目录。两项都满足才取目录部分存在的最长前缀经 `filepath.EvalSymlinks` 解析，再拼回未解析的剩余分量（含原始的叶子分量）；任一不满足则整个路径原样使用，不作任何解析。叶子分量本身永不解析，即便它自身也是一个 symlink，也不论目录部分是否通过上述检查。比较的另一侧（`$FU_HOME/store/skills/`）则完整解析、叶子也不例外，因为它来自 `Store.Home()` 而非用户可控输入。
- **这条规则是四次独立 Critical 缺陷依次收窄出来的结果**，不是一步到位。两侧都不解析目标祖先目录的最初版本下，`$FU_HOME` 自身的祖先一旦变为 symlink（如 dotfiles 工具把 `~/.fu` 挪走后原地放一个链接指回去），fu 早先写下的链接会因文字拼写不再匹配而被误判为未纳管——为堵住这一缺口，目录部分改为解析。此后若目标一侧连叶子也整体解析，用户自己搭的转接链（如 `~/mylink → store/skills/alpha`，再 `~/.claude/skills/notes → ~/mylink`，fu.yaml 里根本没有 `notes`）会在跟随到最后一跳后落入 store 内部，被误判为 fu 所有并遭下一次 reconcile 删除——为堵住这一缺口，叶子改为永不解析。但目录部分此时仍是无条件解析，转接的 symlink 只要落在目录部分而非叶子（如 `~/hopdir → store/skills`，再 `~/.claude/skills/notes → ~/hopdir/alpha`），也会被这一步跟进 store 内部——为堵住它，目录部分的解析加上了上述第一道尾部分量检查。该检查本身仍不成立为判据：它只约束末两级分量，对其前的文本一无所求，而那段文本完全由用户书写。凡别名落在 `store/skills` **之上**者，其原始文本原样保留这两级分量（如 `mkdir ~/backup && ln -s "$FU_HOME/store" ~/backup/`，再以 `~/backup/store/skills/alpha` 为目标——`ln` 接目录时保留 basename，这正是最自然的写法），照样通过；同一个物理别名仅因自身名字的大小写不同就得到相反归属，说明该检查过滤的是用户自选的拼写，而非判定所有权。第二道前缀检查才是真正的不变量：fu 写下的每条链接，其目标都是 `Store.SkillsDir()` 拼上 skill 名，而 `Store.SkillsDir()` 恒为 `$FU_HOME` 拼上这两级分量，故其前缀必为 `$FU_HOME` 的某种拼写；两侧均解析后比较，无论写入时如何拼写、其后祖先又变成多少层 symlink 都成立。
- **明确接受的残留**：`$FU_HOME` **自身**的别名（`ln -s "$FU_HOME" ~/myfu`，再以 `~/myfu/store/skills/alpha` 为目标）通过前缀检查，被判为 fu 所有。这不是前缀检查可以在履行其职责的同时关闭的缺口：该目标文本与 `$FU_HOME` 拼作 `~/myfu` 时 fu 自己会写下的文本逐字节相同，而前缀之所以要解析，正是为了让 fu 在此类重新拼写之后仍认得自己的链接。路径文字无从区分两者，能区分的只有一份"fu 创建了什么"的记录，而本节开头已否定该方案（会漂移的第三份状态）。第六轮把判据收紧为恒等之后，此残留只剩**同名**一种：经该别名到达、且条目名与目标 skill 名一致者仍判为 fu 所有，异名者已全部关闭（见上文"仍不可区分、且明确接受的残留"）。`TestKnownResidualSameNameLinkIsTreatedAsFuOwned` 钉住此行为，使日后若要关闭它是一次自觉的决定。
- 判定整体仍是路径字符串层面的规范化**恒等**比较，不是文件系统身份（inode / 设备号）判定，覆盖的是上述已发生的场景，不是全部可能的路径别名——例如大小写不敏感文件系统上同一物理路径的两种大小写写法，规范化后仍可能比较不相等；
- **动作类型**：`Diff` 与 `Desired` 共同产生七种 `Action`——两种改变链接的动作 `CreateLink`、`RemoveLink`；五种只报告、绝不触碰磁盘的动作 `ReportConflict`（期望有链接的路径被未纳管条目占据）、`ReportDisabledForeign`（期望关闭的 skill 的路径被未纳管条目占据——用户自己的 disable 是否真正生效，故报告而非静默）、`ReportForeign`（fu.yaml 完全没有意见的名字，磁盘上是未纳管条目，纯信息性，`fu status` 呈现为"unmanaged"、写命令不打印）、`ReportReserved`（skill 名与适配器声明的保留条目同名）、`ReportInvalid`（skill 名未通过 `skill.ValidateName` 校验）；
- **状态矩阵**（保留名冲突与未通过校验的名字在进入下表前已被排除、分别产生 `ReportReserved` / `ReportInvalid`，不再参与期望-现实比较；适配器声明的保留条目本就不出现在"现实"里，见 SPEC 规则 11；agent skills 目录本身为 symlink 时该 agent 整体前置拒绝，见 SPEC 规则 10）：

| 期望 | 现实 | 动作 |
|------|------|------|
| 应有链接 | 无条目 | CreateLink |
| 应有链接 | fu 链接，目标正确且未断链 | 无 |
| 应有链接 | fu 链接，指向过期**拼写**或断链 | RemoveLink + CreateLink（重建） |
| 应有链接 | 未纳管条目占位 | ReportConflict（只报告，绝不覆盖） |
| 不应有链接（fu.yaml 记为关闭） | fu 链接（含断链） | RemoveLink |
| 不应有链接（fu.yaml 记为关闭） | 未纳管条目占位 | ReportDisabledForeign |
| 不应有链接（fu.yaml 未提及该名字） | fu 链接（含断链） | RemoveLink（回收不再被提及的残留链接） |
| 不应有链接（fu.yaml 未提及该名字） | 未纳管条目 | ReportForeign |

"指向过期拼写"专指目标的**叶子分量仍是该 skill 名、目录部分是 `$FU_HOME` 的旧拼写**（如 dotfiles 工具挪动 `~/.fu` 后原地放链接指回）——这类链接仍为 fu 所有，故重建。名字与叶子分量不一致的条目（如名为 `alpha`、指向 `store/skills/other`）不是"目标过期的 fu 链接"，而是未纳管条目：fu 没有产生它的操作，故按 ReportConflict 报告、绝不覆盖。

执行阶段还会产生几类 `Diff` 本身算不出的结果——`Diff` 是纯函数，不碰文件系统：`CreateLink` 执行时通过写会话固定的 skills 根以 no-follow 语义复核 store 侧条目，不存在或不是真实目录（普通文件、symlink、FIFO 等）均跳过创建、计入 `Missing`；`CreateLink` 撞见 `EEXIST`（如大小写不敏感文件系统上，一个大小写不同的未纳管条目已占住目标路径，`Diff` 按名查找区分大小写、认为该位置为空）或 `RemoveLink` 复核发现条目已被替换，计入 `Conflicts`；某 agent 扫描本身出错（如 store 内自引用链接触发的 ELOOP）则整个 agent 计入 `Failed`。

执行器在应用 `RemoveLink` 时不再采用“复核可变名字后 unlink”的两步操作。扫描与执行**锚定同一个目录身份**（见 `AgentState.OpenCheckedDir`），最终分量以 `O_DIRECTORY|O_NOFOLLOW` 打开并比对描述符身份；随后把获准的链接以 descriptor-relative no-replace rename 原子退休到不可预测的 sibling 名，在退休名上复核 inode 与原始 readlink target，只删除这一个已移动且仍匹配的链接。若名字在批准后被替换，移动后的对象验证失败并尽力排他恢复；无法恢复时保留退休对象并计入 `Conflicts`，从不按原可变名字删除。POSIX 没有可移植的按 inode 条件 unlink，故“退休名复核 → 按退休名 unlink”仍有一个同 UID 竞态者先观察不可预测名字、再替换它的残余窗口；随机名与极短窗口降低可利用性但不构成原子证明。进程在 rename 后、unlink 前崩溃则会留下无 WAL 的 `.fu-retired-*` 残链；它不阻塞 reconcile，`fu status` 将其作为所在 agent 下的一条未纳管条目报告（与其他 `ReportForeign` 条目同一渠道，无专门分类），本版本不自动认领或清理。

`Result` 共九个字段：`Conflicts`、`Foreign`、`DisabledForeign`、`Reserved`、`Invalid` 直接对应同名的 Report 动作（`Conflicts` 还收纳上文两类执行阶段发现）；`Missing` 复用 `CreateLink` 动作本身，执行时发现 store 侧内容不存在或不是真实目录时计入这里、未真正创建；`Skipped` 是 agent 名列表，agent 的 skills 目录本身是 symlink 时在 `Diff` 运行前即整体前置拒绝（SPEC 规则 10）；`Warnings` 承载恢复过程中已安全隔离、但需要用户知道原因与出路的持久通知；`Failed` 收纳条目级或 agent 级的意外错误。九个字段中只有 `Failed` 非空会使 `reconcileChecked`（`Reconcile` 与写流水线共用的内部入口）返回 `ErrOperationFailed`、令进程退出码为 1（判定在 `reconcileChecked` 尾部 `len(res.Failed) > 0`，经写命令的调用链传回 `internal/cli/exitcode.go` 的 `execute()`，见 §7）；其余八个字段都是 fu 主动、正确拒绝自行处理的可执行状态，命令仍以退出码 0 收尾，诊断打印在 stderr。
- **命令映射**：
  - `status` = 经 `ScanAgent` 读取各 agent 磁盘实况、与 `Desired` 比对计算 Diff 后只读呈现，附 store 工作区状态（`ChangedPathsIncludingIgnored`——即 `worktree.Status()` 再加一次遍历以捕获 ignored 内容，故其结果含 untracked 与 ignored 路径，而 `--hard` 的复位路径集是 `union(index, HEAD)`，两者并不相等）、未完成事务与 `recovery/`、`staging/` 留存盘点。**盘点的 `Collectable` 一桶以 `fu gc` 自身的判据为准，不重述其规则**：已了结（有终态标记，或已带 `.pruned` 记录的中断裁剪）的事务家族，其 journal 文件本身计入该桶——这是 `recovery/` 下最常见的积累，一次 `fu new` 即留下九个文件，此前两边都不报，`fu status` 于是在九个文件之上打印"nothing to report"。未完成家族的 journal 不入桶，因为未完成事务一节已按 op 与 skill 点名了它，同一事实不以互相矛盾的两种说法各报一次；带 `txn-` 前缀却解析不出任何家族的名字（手工遗留）计入 `Uncollectable`，不再无声消失。**该桶为此要解码记录字节，量恰为每个已完成 rm 家族一条 revision**（第二十五轮补记）：孤立载荷名只写在家族自己的 revision 里，故 `collectableRecoveryNames` 取最新一条解码（`newestTxnRevision`），据以派生载荷名与 `RecoveryPayloadSettled` 要核对的退休名；不做 `validateTxnChain` 的整链重读重哈希，也从不打开载荷本身。这是"`status` 不替别的命令解释恢复权威"这条纪律下自觉的最小放宽——不读这一条，`Collectable` 就无法与 `fu gc` 的判据一致，而不一致正是分桶要避免的事。远端状态核对（ls-remote 查询，不落盘）仍**（设计，未实现）**；
  - `restore` = 应用 Diff 全部 Action，按期望重建缺失链接、清理断链；发现 store 工作区有未提交改动，默认仅报告、不触碰，`--hard` 时改为调用 `ResetWorktreeToHead` 把已跟踪路径复位到 HEAD——untracked 与 ignored 内容不动，也不归档，详见 §6「崩溃残留」条目。**复位若真的动了路径，随即在同一把锁内重新加载 `fu.yaml` 并再对账一次**：第一遍跑在工作区定稿之前，其依据的配置可能正是这次复位丢弃掉的那份，SPEC §3 场景 5／规则 6／§10 要求的「一次运行即完全修复」由第二遍兑现；第二遍的链接层结果**取代**第一遍的（第一遍的发现描述的是已被这次复位改变的状态），只有来自 `RecoverPendingReporting` 的 `Warnings` 跨遍保留。此外单个 agent 的扫描失败（`ErrOperationFailed`）不再取消工作区层，错误留到最后返回；
  - `new`、`rm` 与 toggle 等单项写操作收尾执行一次 reconcile（算 Diff、应用链接层 Action）；`add` / `adopt` 因先发现候选，先执行一次恢复/对账 prologue，再由每个独立 skill 事务各自收尾 reconcile，故一次命令是 1 + N 次。"新 agent 首次投放由下一次写操作或 restore 完成"（SPEC 规则 4）由此自动成立，无需专门逻辑。

Diff 是纯函数，表驱动测试穷举状态组合；文件系统副作用全部收敛在执行器。reconcile 幂等。

**事务记录与统一恢复入口**：`new` 与 `rm` 每条 CLI 命令各使用一条事务链；批量 `add` 与 `adopt` 则按 skill 使用彼此独立的事务链。两种批处理都保留已完成的前序项。`add` 在某一候选进入自身前置检查后发生的普通失败会回滚并报告该候选，再继续独立候选；store / 写会话级错误（无论出现在前置检查之前还是之后）、事务/并发冲突、候选退出时仍有未完成 WAL，或候选已提交但无法证明 canonical path，则立即中止批次，把中止候选列入失败、后续候选列为未尝试。`adopt` 按 agent 隔离可安全放弃的目标变化；无法证明 fu 自建载荷身份的安全冲突仍中止。每一项先完成统一恢复、配置检查、sweep 与自身只读前置检查，然后在**该项自身的任何 store 或 agent 变更之前**，在 recovery 中原子且排他地追加首条记录——随机事务 ID、单调序号、操作类型、起始 HEAD、预期新状态、涉及目标与当前阶段；阶段推进追加新的不可变 revision，完成时追加排他的终态标记，既不替换旧 revision，也不按固定路径删除。每个 revision 文件名提交其精确字节的 SHA-256，内容携带前一 revision 摘要。

**读取端对未完成与已完成两类事务的验证强度不同**（第十八轮 I5 起）：未完成事务从序号 1 起读取并验证**完整连续链**——文件名与字节的对应、`PreviousDigest` 的逐环链接——任何处理器接手之前都要全量通过。已完成事务（带终态标记者）在普通写命令的恢复扫描中不再读取 revision 字节：只按文件名验证序号从 1 起连续，并核对标记指向的序号与摘要恰为最高序号 revision 的文件名摘要。绑定关系本身由写入端的 `ClearTxn` 建立，它在写标记之前从磁盘完整读取并重新哈希整条链。这样收窄是有意的：早先一条完成于很久以前的事务里坏掉一个字节，会让其后每条写命令永久失败，而用户对那条记录已无任何可执行的动作；`store/config_exchange.go` 的配置交换 journal 出于同样理由做了同样的取舍。代价是普通恢复扫描允许篡改一条不会再被处理器读取的已完成记录；追加 revision 会破坏标记的序号绑定，删除 revision 会破坏连续性，两者仍被发现。revision 与标记的写入端和读取端共用 16 MiB 上限，超限在排他创建前拒绝。

`fu gc` 裁剪已完成事务的 revision 与终态标记，并在裁剪前重新读取、哈希和验证整条链。它先排他写入内容寻址的 `.pruned` 记录，绑定终态标记内容与有序 revision 文件名，再开始删除；恢复扫描只接受该记录明确覆盖的删除前缀，因此进程在任一次删除后中断都可由下一次 `fu gc` 继续，完成后连同 `.pruned` 记录一起移除。损坏家族的诊断始终把 revision、`.done` 与 `.pruned` 视为一个整体，人工放弃时必须把整个家族一起移出 recovery，绝不能只移走报错中最先出现的单个文件。除 journal 外，该命令还回收两类已完成事务留下的残留。其一是已完成 rm 家族在 recovery 下遗留的孤立载荷——`reclaimCommittedRemovePayload` 丢弃错误、崩溃也会留下同样的孤立载荷，故 gc 必须能独立收拾。**回收排在裁剪该家族 journal 之前，这一次序是硬性要求**：证明载荷所需的清单就写在待删的 revision 里，先裁再收就再也无从验证；回收失败则整个家族跳过裁剪，留待下一次 `fu gc` 在清单仍在时重试。**判断载荷是否已了结必须跨越处置协议的两个名字**（第二十二轮修复）：处置不是一次 syscall——`RemoveOwnedTreeAt` 先清空树、再把根退休到按清单派生的确定性兄弟名、最后才 unlink，故后两步之间的崩溃会腾出载荷名，而把已清空的根留在 `.fu-retired-dir-<token>` 上。只 stat 载荷名即答"已了结"，裁剪就会据此删掉承载该清单的 revision，而清单正是 `RemoveOwnedTreeAt` 从那个兄弟名续跑的唯一凭据；此后该退休根再无任何东西收得走，`fu status` 却仍按前缀把它计为可回收，恰好是分桶本要避免的"让用户运行一条命令、看着计数不动"。该判断因此交给 store 的 `RecoveryPayloadSettled` 同时核对两个名字——退休名的派生留在 store 内部，日后协议再添中间名也不会把调用方甩下。只有未完成事务集读取失败那条分支会直接询问载荷，正常路径本就经 `ReclaimRecoveryPayloadOwned` 自行续跑，不受影响。**install 家族没有对应的孤儿窗口，故 gc 不为它们设回收分支**（第二十二轮记录，防再查）：`new` / `add` / `adopt` 的归档一律排在 `ClearTxn` 之前且失败即返回（`rollBackUncommittedInstall` 与 `committedInstallRecovery.finish`），无载荷那条分支更把"载荷不存在"直接写成清 WAL 的前置条件，因此一个已完成的 install 家族永远不会遗留 `rollback-*`：它要么已经在 `.fu-archive-*` 上，要么其事务根本尚未完成、名字仍被未完成记录所主张。这与 `rm` 的"回收排在 `ClearTxn` 之后"（§6 rm 流程第 4 条）恰好相反，而两者都是有意的——`rm` 的载荷在 WAL 仍开时正是回滚要还原的那份内容，不能先收；install 的载荷则是回滚已经产出的副本，不先归档就不能宣告事务结束。据此，未被认领的 `rollback-*` 只可能来自人工按 `addRecoveryConflictRemedy` 的放弃建议移走整个家族，清单已失，确实无人可收。其二是 `completeConfigExchange` 未能就地收掉的配置交换记录、终态与归档（见 §6"fu.yaml 写入"一条），它们不属于任何事务 journal，故每次运行整体回收一次，其失败只记入诊断、不阻断 journal 裁剪。这一遍回收按名字前缀进行，只碰三类名字：已带终态标记的记录、记录已消失的标记、以及名字自述的 identity 仍能对上的归档；并且只要还有任一未完成的交换记录在，归档一律不动——那份归档可能正是恢复即将收敛过去的唯一副本。**载荷名加上一份对得上的清单并不构成所有权**：`removed-<name>-<StartHead>` 在 rm 家族之间并不唯一，而从 skills 根到 recovery 的每一跳都是 rename，设备号、inode 与内容因此一路不变；于是一个已完成家族的清单可能与另一个仍未完成家族的现存载荷逐项相等，照此匹配删除会毁掉那条未完成事务的载荷，并让其后每条写命令都卡在恢复入口。故 gc 先一次性收集全部未完成事务所主张的载荷名（`pendingRecoveryPayloadClaims`）并整体排除；读取这些记录不等于恢复它们，该命令仍然刻意不调用 `RecoverPending`。未完成事务、adopt 归档载荷（`adopt-archive-*`）、`new` / `add` 回滚出的隔离载荷及其归档（`rollback-*` 与 `.fu-archive-*`）、`adopt-link-*.json` 与 adopt 中断留下的孤立载荷仍不在删除范围内。link archive 在退休 symlink 前成为恢复 authority；崩溃若发生在记录落盘与 journal append 之间会留下内容寻址的孤立记录。当前无法在并发写入下证明这类记录已无恢复用途，故有意保留而不自动收集。`.old`、sibling 备份等现场残留只是事务载荷，仅在与记录匹配时才被处理；无记录匹配的 `.old` 按普通内容对待，不会被当作残留清理。除只处理已完成事务的 journal 与残留、刻意不调用 `RecoverPending` 的 `fu gc` 外，所有写命令取得锁后的第一步是按记录恢复未完成事务，终态为三者之一：**完成、回滚、安全冲突**（现场在崩溃后被外部改动时如实报告，不强行收敛）；恢复完成前不执行普通 reconcile，sweep 不把与记录匹配的事务残留当外部修改。`status` 只读报告未完成事务，呈现为 `PendingOperation`（操作与对象名）。由此，任何中断后的下一次普通写命令或 `fu restore` 都到达定义的终态，用户无需知道崩溃发生在哪条命令。

## 3. fu.yaml schema

`source` 字段已实现（plan 2）：`add` 写入 git（type/url/ref/ref_kind/commit/subdir）与 local（type/path[/subdir]）两种记录，
`show` 显示来源与锁定 commit 短哈希；默认分支固定与 tag / branch 锁定均已实现，只有 `update` 对记录的消费仍属未交付命令**（设计，未实现）**。
`internal/source/` 包存在：`ParseArg` 将参数整体作为 URL 或本地路径，不再从 `@` 猜测 ref；`ParseArgWithRef` 接受 CLI 的显式
`--ref <ref>`，支持含 `/` 的 branch / tag 名，并明确拒绝 40-hex commit ref。来源分类先接受真实存在的本地目录，再用 go-git endpoint parser 识别 URL，因而支持任意用户名与无用户名的 SCP 形态（如 `alice@example.com:team/repo.git`、`example.com:team/repo.git`），同时不会把同形的现存本地路径误判为远端。`Prepare` 负责 git 浅克隆至身份固定的 staging scratch 或固定 local 根，
并在整个候选展示与安装期间持有 `os.Root`；扫描、摘要、投影和复制都通过该描述符读取，绝不重新打开可变路径。正常关闭时 scratch 的每个子对象与根都先原子退休并复核 identity，路径被替换则保留新对象并报冲突。

**与 plan Task 1 接口规格的偏差（第十五轮记录，均为有意为之）**：`LockInfo` 用离散字段 `Ref/RefKind/Commit` 而非规格中的 `Fields map[string]string`（枚举字段让 `EncodeFields` 的 git/local 分支直接可读，无需把 map 语义再解释一遍）；`EncodeFields` 因此多收一个 `lock` 参数；local 实现并入 `source.go`，git 准备逻辑位于 `git.go`（local 与 git 的差异只有 Prepare 的两三行，独立 `local.go` 是过度的边界）；local 路径在 `ParseArg` 时经 symlink 解析为规范绝对路径而非仅绝对化（记录值必须可复制，未解析的链接目标在另一台机器上不可复现）。

~~**URL 路径中的 `@` 会被误解析成 ref（第十六轮记录）**~~ **（已关闭）**：CLI 改为 `fu add [--ref <ref>] <git-url>`；
位置参数中的 `@` 始终属于 URL，ref 只来自独立选项，因此路径含 `@` 与 ref 含 `/` 两种形态可同时无歧义支持。

```yaml
version: 1
skills:
  pdf-tools:
    source:
      type: git              # git | local
      url: https://github.com/x/skills
      ref: refs/heads/main   # 安装时解析并持久化完整形式；缺省时解析为当时的默认分支并固定
      ref_kind: branch       # branch | tag | commit，SPEC 规则 9 行为分派的依据；当前 add 只产生 branch/tag，commit 预留给未来 update/clone 的锁定记录
      commit: a1b2c3d…       # 锁定（完整 hash）
      subdir: pdf-tools      # 多 skill 仓库内的相对路径，仓库根即 skill 时省略
    digest: sha256:…         # 安装/更新时的规范化内容摘要（本地修改判定基线）
    enabled: true            # 全局开关
    overrides:               # 仅记录与全局不同的 agent（同值归一，见 SPEC §4.1）
      codex: false
  my-skill:
    source:
      type: local
      path: /Users/x/dev/my-skill
    digest: sha256:…
    enabled: true
```

- `version` 起步即有；读—改—写往返**保留未知字段**（以 `yaml.Node` 层级操作，不经 struct 全量重排），为将来字段留路；`version` 高于支持范围时：只读命令尽力而为并警告，写命令拒绝执行；
- **skill 名校验**：`skills:` 下每个键即 skill 名，会被当作路径分量拼进 store 与各 agent skills 目录（`fu show` 的 SKILL.md 读取、engine 的链接物化均如此）；`LoadConfig` 载入时对每个键施加与 `fu new` 相同的命名规则（Agent Skills 规范：小写字母数字与连字符、不以连字符首尾、无连续连字符、长度 1–64）。**处置为逐条目隔离，不是整体拒绝该 fu.yaml**：不合规的条目被排除出该 config 的 skill 集合（`SkillNames`、`HasSkill`，以及一切以此二者为门的访问器），因而不合规名字不作为路径分量参与任何计算；其余条目照常可读可写。底层文档不动——隔离发生在访问器边界而非从 `c.doc` 删除——故 `Save` 原样往返该条目，不会因一次无关写入把它从 fu.yaml 抹掉；`fu rm <不合规名字>` 会说明该名字为何被隔离并指向 `fu.yaml`，但不会把未经校验的名字当路径处理，移除条目仍需手工编辑该文件。被隔离的名字记入 `Config.InvalidNames()`：写命令经 engine 的 `configInvalidNames` 整趟折叠一次报告为 `ReportInvalid`（与 agent 无关，零 agent 时同样报告；但若该名字同时是某 agent 的保留条目，则由 `Desired` 逐 agent 判为 `ReportReserved`，保留名的诊断更具体，优先），只读命令经 `printInvalidNames` 报告。早先"任一不合规即整体拒绝"的做法代价过大：一个坏条目会让 `fu list`、`fu show <任意名>` 等只读命令全部失败，且使"回收记在不合规名字下的残留 fu 链接"这一修复在生产中不可达——没有命令能拿到一个 `LoadConfig` 已拒绝构造的 `Config`；
- engine 的 `Desired` / `Diff` 各自保留一份相同校验作纵深防御。经 `LoadConfig` 得到的 `Config` 不会让这两处观察到不合规名字（该名字根本不进 `SkillNames()`），故这两份副本对生产路径不可达；它们防的是将来某个调用方不经 `skill.ValidateName` 直接改动 `Config`（`AddSkill` 本身刻意不校验）。**注意 `Desired` 把 `cfg.InvalidNames()` 折进报告这件事本身是生产路径**，与上述副本不是一回事；
- 同值归一只在 agent 级开关写入时执行：`overrides[agent] == enabled` 的条目删除；全局开关写入不触发（SPEC §4.1）；
- **摘要算法（规范化投影）**：skill 目录内的**文件与 symlink**（**统一排除 `.git`**；普通 `.git` 目录或文件不投影，`.git` symlink 则因会改变内核解析语义而直接拒绝）逐项纳入路径、文件内容 hash、可执行位、symlink 目标，编码为带类型前缀的记录后按（类型, 路径）排序，整体取 sha256。**目录本身不参与**：git 不独立保存目录，空目录根本无法表示，若为每个目录记一条，一个含空目录的 skill 在 store 工作区与其新克隆中就会永久算出不同摘要；代价是增删空目录对 fu 不可见，这一点与 git 自身一致。该投影唯一：`add` 的本地与 git 来源、adopt、update 的复制与 store / source 双侧摘要一律使用它——复制与摘要永远看到同一集合。安装与更新时计算写入 `digest`；
- **`source` 可省略**：`fu new` 与真实目录收编的 skill 无上游，update / outdated 对其跳过并提示；symlink 收编的条目在原目标路径唯一时记录其为 local 来源，多处路径不一致则省略并警告（见 AdoptPlan）；
- local 来源的 `path` 为绝对路径，随 store 同步到其他机器后仅作提示信息——该来源只在路径存在的机器上可更新（SPEC 规则 9）。

## 4. git 封装（go-git 边界决定）

| 操作 | 实现 | 说明 |
|------|------|------|
| commit | 私有 stage + 冻结候选 + CAS 发布 | `PrepareCommit` 先在短暂持有 `.git/index.lock` 时只读捕获公共 index 基线，随后在共享真实对象库、但 `Index` / `SetIndex` 完全落于内存副本的私有 repository 中只 stage 一次，冻结由路径、Git mode 与 blob hash 组成的完整树；直接 Git 在验证、WAL 追加与发布期间始终看不到 fu 的临时候选。commit 直接从冻结条目递归构造 tree 与 commit 对象，分支只以捕获的旧引用做 CAS 更新（§6"命令提交候选冻结"）。发布成功后仅当公共 index 的完整结构仍精确等于捕获基线、且该基线在准备时与 HEAD 同树，才在重新取得 `.git/index.lock` 后把它同步到已提交候选；期间到达的直接 Git index 内容原样保留。候选被放弃不需要还原公共 index，因为准备从未写入它。他人已持有锁时 fu 与 git 一样停下并报出锁路径。提交信息格式 `<操作>: <对象>`，如 `new: pdf-tools`、`disable: writing --agent codex`、`external: manual modifications`；`fu log` 的展示直接以此为素材 |
| pull | fetch + 仅快进 | 检测到分支分歧即停，报错附 store 路径与建议命令，用户以系统 git 处理（store 是标准 repo，SPEC §5.1 已同步此语义） （设计，未实现）|
| revert n | 快照前滚，工作区先行、`Commit` 发布 | 由 `resolveOperationsBack(n)` 走 first-parent 历史、**按操作而非按裸提交计数**解析出目标提交（判据是 SPEC §5.3 的操作清单白名单 `isOperationMessage`；sweep 的 `external: manual modifications`、恢复补偿的 `recover: roll back interrupted …` 及其被补偿的那一笔、以及 `init: store` 均不计），读取其 tree，经 `targetTreePaths` 展平为路径到目标条目的映射，交给与 `restore --hard` 共用的路径受限更新器 `applyTreeToWorktree`（见 §6「崩溃残留」）把工作区与 index 收敛到该目标，然后才调用 `Commit`：由其冻结候选树、以捕获的旧分支引用做 CAS，发布为新 commit（tree = 目标树，parent = 当前 HEAD 且为唯一父）。不经 checkout（避免 detached HEAD）。**这一顺序取代了旧算法**——旧版先构造 commit 对象、CAS 更新分支引用，再调用 `Worktree.Reset(HardReset)` 刷新工作区，而后者无条件重写分支引用，等于把刚做的 CAS 又写了一遍：期间落地的直接 Git 写入会被无声吞掉。新顺序下发布者唯一，只有 `Commit` 的 CAS 写一次引用；`TestRevertWritesTheBranchReferenceExactlyOnce` 以计数钉住新旧两版的差异——旧实现写两次分支引用，新实现只写一次。`Store.Revert` 本身仍是不取 `fu.lock`、不 reconcile 的裸操作（index.lock 由 §6「崩溃残留」所述的 `rebuildIndexFromTarget` 内部持有，与此处的 `fu.lock` 是两回事）；`fu revert` 现由 `RevertOperations`（`internal/engine/restore.go`）经标准写流水线调用它：取锁、恢复未完成事务、先 `Sweep` 把待处理手工编辑记入其自身的 `external: manual modifications` commit，再以原封不动的 `n` 调用 `Revert`——sweep 自己新增的提交不必再由调用方折算，因为计数已改为按操作进行，该 sweep 与其他任何 sweep 一样不是操作（此前的 `n+skip` 补偿只覆盖本次调用自己的 sweep，历史中既有的 sweep 仍会吃掉一次操作，故整段删除），随后 reconcile 链接层。测试须覆盖连续回退：A-B-C → revert 1 → D(parent=C, tree=B) → revert 1 → E(parent=D, tree=C)。**按操作计数的一个直接后果值得写明**：不计数不等于不回退。回退目标是某次操作提交的 tree，因此夹在目标与 HEAD 之间的非操作提交（sweep 的 `external: manual modifications`、直接 git 提交）其内容同样被收敛掉——`fu revert 1` 会撤销最近一次操作**并且**丢掉其后手工提交的笔记。内容不丢，仍可从 git 历史取回，且这些路径会出现在 revert 打印的 changed 清单里；但相对旧的黑名单算法（把直接 git 提交计为一次操作）这是行为变化 |
| push / fetch | go-git transport | SSH（ssh-agent）与公开 HTTPS（认证范围见下）；奇特配置以"直接用 git 操作 store"兜底 （设计，未实现）|
| 外部修改 sweep | HEAD→公共 index 与 index→worktree 分层入库 | 写操作与 push / pull 执行前统一检查（SPEC §5.3）；先精确提交用户已 staged 的公共 index 快照，再以私有 index 提交其后的 worktree 快照，二者不同时产生两笔同为 `external: manual modifications` 的有序历史 |

git 来源的取得（add）：浅克隆（depth 1）指定 ref 至 `$FU_HOME/staging/`，整个取得过程受 2 分钟 context 截止时间与 512 MiB 聚合写入预算共同约束。远端 transport 的预算由 `.git` 与工作区共享；`file://` 为避免 go-git upload-pack 在预算错误后阻塞，直接从既有本地 object store 只物化所选 commit 的工作区，因此预算覆盖实际新写入的普通文件、扩容与 symlink target 字节，而不重复计算源仓库中已存在的 object。任一受预算写入超过上限即终止并清理准备区。两条路径都只接受 `SingleBranch` + `Depth: 1` 形态，并把 branch / tag（含 annotated tag）剥离到 commit 后记录完整 commit hash。解析后**从工作区经 §3 的规范化投影复制**——不是从 git tree 导出。`.git` 由投影按名排除（任意深度），与 local 来源走的是同一条路径、同一份投影：两种来源的复制与摘要因此永远看到同一集合，这正是 §3「该投影唯一」所要求的。若改为从 git tree 导出，git 与 local 两侧就会各有一份集合定义，`digest` 基线（SPEC 规则 3、9）随之失去意义。`update` 的取得与 `outdated` 的 ls-remote 式比对（比对 ref 头与锁定 commit，不产生本地写入）**（设计，未实现）**。

**完整入库投影**：`Worktree.Status` 可见的修改加上 store 中所有「磁盘上存在但 index 中不存在」的非目录条目，任意层级的 `.git` 除外；后一部分使未跟踪且被 `.gitignore` 隐藏的内容也不会漏掉。脏状态查询严格无副作用，并分别保留 HEAD→公共 index 与 index→worktree 两层：例如 HEAD/工作区为 A、公共 index 为 staged-only 的 B 时，sweep 先提交 B，再提交 A，既不抹掉 B，也不把两个时序状态压成一笔。普通 `Commit` 在私有 index 中应用同一完整投影；`Sweep` 与事务恢复共用同一遍历规则，恢复在补偿 commit 前拒绝任何事务范围外的变更，不先改动公共 index。

**基线三态判定**：`本地修改 = digest(store) ≠ 记录基线`（sweep 使 worktree 常态干净，`worktree.Status()` 不能承担此判定）；`outdated = digest(source) ≠ 记录基线`（local 来源；git 来源按 `ref_kind` 分派，仅 branch 以 ref 头 ≠ 锁定 commit 判定，tag 与 commit-pinned lock 不参与 outdated）。两侧同时偏离基线走冲突分支（update 拒绝，`--force` 覆盖）；两侧内容彼此相同而基线过期时，update 仅刷新基线、不改内容。

**认证范围（v1）**：SSH 经 ssh-agent（go-git 默认支持）；HTTPS 仅公开仓库，私有源指引改用 SSH。此范围同样适用于 add / update 的来源仓库——store 远端的兜底（"直接用 git 操作"）救不了来源侧，故来源侧范围必须明确。

## 5. 适配器

```go
type Agent interface {
    Name() string          // "claude" / "codex"
    Detect() bool          // 特征路径存在：~/.claude/、~/.codex/
    SkillsDir() string     // ~/.claude/skills、~/.codex/skills
    Reserved() []string    // 保留条目，永不纳管/收编（codex: [".system"]）
}
```

- 注册表静态声明全部已知适配器；`Detect()` 为真者即纳管（SPEC 规则 4）；
- 新增 agent = 一个新实现加注册表一行；
- 专有元数据透明传递（SPEC 规则 5）无需接口支持：链接指向整个 skill 目录，内容原样可见。

## 6. 写操作流水线与原子性

统一流水线：

```
取锁 → 恢复未完成事务 → 加载配置、检查可写性 → sweep 外部修改 → 命令只读前置检查 → [多阶段命令：追加事务 revision] → 准备（staging 下载与校验）→ 落盘 store → commit → post-commit → [追加事务终态标记] → canonical-path check → reconcile 链接 → 释放锁
```

上图适用于已进入 `run` 的单项写操作。`add` 的来源准备是例外：先在一次短锁内执行恢复、可写性检查、sweep 与 reconcile 的 prologue，随后**释放锁**完成 clone/local root 固定、扫描与交互选择；每个选中 skill 再分别进入一次上图所示的持锁事务。`adopt` 同样先跑 prologue，再按 skill 分别进入持锁事务。

- **可写性检查先于 sweep，这一次序是硬性要求**：加载 `fu.yaml` 后立即检查 `version` 是否在本 fu 支持范围内（§3），检查置于 sweep 之前而非之后。sweep 本身是一次 commit——若排在可写性检查之前，一个因版本过高而被拒绝的写命令仍会先把 sweep 扫到的外部修改提交入库（且提交信息与真正的拒绝原因无关），版本护栏形同虚设；提前到 sweep 之前，被拒绝的命令不产生任何提交，store 保持用户离开时的原样；
- **命令前置检查先于 WAL**：会对当前状态作普通拒绝的只读条件必须在事务记录前完成；例如 `new` 先检查配置重名和固定 skills 根下的既有目标，避免在尚未产生任何命令内容时崩溃，却留下无法自动收敛的空摘要 WAL。进入 mutation 前再检一次相同条件，抵御非 fu 写入者在前置检查后竞态放入目标；
- **准备区**：`$FU_HOME/staging/`——与 store 同文件系统，落盘用平台原子且排他的 rename 完成（Linux `renameat2(RENAME_NOREPLACE)`，Darwin `renameatx_np(RENAME_EXCL)`），目标已存在时绝不替换，其他平台安全报不支持；staging 根先以随机私有名排他创建并取得完整 manifest，reservation 写入 WAL 后才排他发布为事务逻辑名，发布后的 identity 再写回 WAL。恢复只处理这条 reservation/manifest 链能证明的对象；空目录、匹配 `.fu-new-*` 的名字或无 manifest 的最终名都不构成所有权，无匹配条目一律保留为安全冲突；内容在此完成下载与规范校验（SPEC 规则 7）后才移入 store；
- **结构化持久结果**：每条 Application 写操作返回单调阶段 `Committed`、`PostCommitComplete`、`WALComplete`、`CanonicalChecked`、`ReconcileComplete`，另以状态信号 `RecoveryPending` 表示当前是否仍需恢复（它可在 WAL 完成时由 true 变 false），并携带 reconcile 结果。Git commit 已发布后即使验证、post-commit、WAL 完成或 canonical check 返回错误，`Committed` 仍为真；CLI 保持非零退出，但先如实呈现已发生的 add/adopt/rm/new/toggle 结果及待恢复阶段。adopt 另区分切换完成的 `Adopted` 与 store 已提交但切换未完成的 `Pending`，不会把半完成状态称作完成；
- **fu.yaml 写入**：临时文件 + fsync + rename，永不原地写；不对父目录 fsync，故只保证进程崩溃安全、不保证掉电安全（见附录"崩溃恢复"）。写入是条件安装：先在 `staging/` 下以随机 `.fu-config-candidate-*` 名和 `O_EXCL|O_NOFOLLOW` 排他创建候选，写入并 fsync 后取得其 identity；再把候选 identity、交换前 `fu.yaml` identity、预期旧字节摘要与候选字节摘要作为不可变 `.fu-config-exchange-*.json` 排他写入 `recovery/`，记录成功后才把候选排他 rename 成固定活动名 `.fu-config-swap`。因此记录落盘前的崩溃至多留下永不自动认领的随机候选，不会占住活动名；活动名已有外部条目时也绝不打开或复用（即使它为空——其 inode 仍可能通过硬链接属于外部路径）。随后活动名与 `store/fu.yaml` 做一次原子交换（Linux `renameat2(RENAME_EXCHANGE)`，Darwin `renameatx_np(RENAME_SWAP)`）。交换后被换出的旧对象即"文件在交换那一刻确实持有预期字节"的凭据：该名字在交换后只解析一次得到描述符，此后 identity 与内容都在该描述符上验证。凭据成立即完成安装，把活动名以排他 rename 移入 `recovery/` 下由设备号与 inode 派生的唯一 `.fu-config-archive-*` 终态名，并在归档名上复核 identity；旧 inode 的内容始终不修改，因为 store 外的硬链接或仍打开的描述符可能共享它。凭据不成立且 `fu.yaml` 仍是 fu 刚装入的对象时，把被换出的权威对象换回 `fu.yaml`，再以同一协议归档 fu 自己撤回的 inode；若此时 `fu.yaml` 已被第三方再次改变，则第三方版本原样保留、被换出的对象继续停在活动名，均不删除。交换而非"先移开再安装"，是因为后者存在 `fu.yaml` 不存在的瞬间，此刻崩溃会让 store 无法打开；交换在任何时刻（含崩溃）都留下两个完整版本之一。**残留与恢复**：统一写恢复入口先扫描未带匹配 `.done` 终态的 exchange 记录，并同时核对候选、活动名、`fu.yaml` 与两个 identity 派生归档位置；只有 identity 与摘要完整对应"尚未交换 / 已交换 / 已撤回 / 已归档"之一时才确定性归档或复原并写入绑定记录摘要的终态，故进程在 exchange syscall 后退出也无需人工清理。任一未知 identity、字节或位置组合均保留全部版本并安全冲突；没有匹配记录的活动名仍视为外部占用而拒绝接管。记录、终态与归档不再永久保留：终态标记一旦落地，`completeConfigExchange` 就地回收这三者，进程在回收中途退出留下的残留由 `fu gc` 按名字前缀整体回收（§2 `fu gc` 一条）。**但这句只覆盖记录、终态与归档这三个名字本身，不覆盖已退休到一半的名字**：本路径退休到的是随机名（下文 `retireOwnedLeafAt`），磁盘上没有任何东西能把它归回 fu，`ReclaimCompletedConfigExchanges` 因此明确不扫描退休名。于是退休 rename 与紧随其后的 unlink 之间的崩溃，以及复核不符而 `RestoreRetiredAt` 也失败（该错误与其他回收错误一并丢弃，因为交换本身已经完成）这两种情形，都会把对象永久停在 `.fu-retired-entry-<随机>` 上。量级上：撞上这个窗口的崩溃每次留下一个小文件，而窗口本身只有退休 rename 与 unlink 之间的一两次 syscall；作为对照，本分支之前是每条写命令无条件留下三个文件。POSIX 确实没有可移植的按 inode 条件 unlink，但安全性并不需要该原语，两条删除路径各以自己的方式做到。`retireOwnedLeafAt` 与 §2 链接退休协议同源：先把名字原子退休到外部无法预测的兄弟名（`RetireNameAt` 的随机名取自 `crypto/rand`），在退休后的对象上复核 identity 与类型，确认无误才 unlink，不符则原样放回。`store.RemoveOwnedTreeAt` 退休到的兄弟名则是确定性的——由前缀、逻辑路径与设备号/inode 取 SHA-256（`ownedCleanupRetiredName`），且这份确定性是有意的：它正是 `compareOwnedTreeCleanupState` 能认出中断处、让删了一半的删除续跑的前提。它的安全性因此不来自名字不可猜，而来自全量清单的前置校验、无替换的退休 rename 与移动后按清单的再校验。两条路径都只删除那一个已移动、且仍与记录逐项相符的对象，因此最终名字上的竞态替换只会被保留，不会被删除，早先据此得出的"只能永久保留"并不成立。**回收顺序是不变式：记录必须排在它自己的终态标记之前**——先删标记会留下一条 `readPendingConfigExchangeRecords` 仍当作待办读入的记录，把恢复再送回一次已经结束的交换。所有这些条目均在版本控制之外，不会被 sweep 计入历史，也不会与 skill 内容混淆；
- **命令提交候选冻结**：初始 sweep 之后的命令只允许声明过的 Git 路径进入自身提交（`new` 为 `fu.yaml` 与 `skills/<name>/...`，开关命令仅为 `fu.yaml`）。`revert` 不走 `run`，故它以更强的等价条件满足本规则：冻结候选的**整树指纹**必须逐字等于目标提交的树指纹（`commitTreeFingerprint`），不等即拒绝并撤回候选索引——这比逐条声明路径更强，因为它同时排除了任何未声明的多余内容（`stageAll` 会强制加入 untracked 文件），而这正是该原语的定义性不变式：结果树**就是**目标树。落盘与 publish 完成后在私有内存 index 中只 stage 一次，冻结由路径、Git mode 与 blob hash 组成的完整树；冻结候选中的配置字节、事务所有权载荷与允许路径必须精确匹配，且 worktree 不得再有未进入候选的 tracked、ignored 或删除变化。随后 commit 直接从这组不可变条目递归构造 tree 与 commit 对象，最终校验后不再把公共 index 当作候选输入或扫描文件系统；当前分支只以捕获的旧引用做 CAS 更新。直接 Git 写入公共 index 的内容既不会进入 fu 的候选，也不会被候选放弃或发布后的条件同步覆盖；事务在 operation/compensation commit 前分别持久化候选树指纹，恢复识别提交时除 parent 与 message 外还必须匹配完整树。因此初始 sweep 后到达的外部修改要么使命令回滚并保留为下一次独立 external commit，要么在候选冻结后保持未提交，绝不会被归因到当前命令；
- **update 目录交换**：旧目录先改名保留（`<name>.old`），新内容就位并 commit 成功后清理；`.old` 是由命令级事务记录背书的载荷——恢复入口仅处理与记录匹配的 `.old`（新目录已 commit 则清理，否则改名还原），无匹配的 `.old` 按普通内容对待；
- **崩溃残留**：落盘后、commit 前崩溃会留下 untracked 内容。go-git 的 hard reset 与系统 `git reset --hard` 行为相反——**会**删除 untracked 内容，包括被 `.gitignore` 忽略的部分（系统 git 两者都不删；已用当前依赖 go-git v5.19.2 的 `TestGoGitV519HardResetDeletesUntrackedAndIgnoredFiles` 直接验证）。`restore` 与 `revert` 的工作区复位落地前，这一 hard reset 逻辑所在的函数原有五项前置条件未关闭（第十三轮补记）。**本轮（Task 1–8）五项全部关闭；第 1、2 条不是逐条修补关闭的，而是各自的前提或来源本身消失，第 3、4、5 条才是逐条修补关闭的**：（1）先归档 untracked 再 hard reset——**前提消失**：该要求成立的唯一原因是 go-git 的 `Worktree.Reset` 会删除 untracked 与 ignored 文件；新的路径受限更新器 `applyTreeToWorktree`（`internal/store/worktree_apply.go`）不再调用它，其路径集合恒为 `union(index, target tree)`——两者都是内存中已解析的已知集合，工作区本身不参与该集合的计算，因此其中不可能出现 untracked 或 ignored 的**文件**名字。工作区确实被枚举过一次——前置条件 `checkNoAbsoluteSymlinks` 的只读遍历——但那一遍读取的名字不流向任何写入或 unlink。**但路径集论据只覆盖文件，不覆盖目录，因此它不是完整的安全论据**（第二十四轮更正）：`applyTreeToWorktree` 会调 `pruneEmptiedParents`，后者对祖先**目录**名发出 `Remove`，而 git 不索引目录，故这些名字既不在 index 也不在目标树中——更新器确实会命名并 unlink `union(index, target)` 之外的路径。真正兜住 untracked 内容的是另一条不变式：裁剪不先列目录再判断，而是**直接尝试 rmdir 并读结果**，`ENOTEMPTY` 即停止条件；「空」因此是在系统调用处原子证明的，目录中任何 untracked 内容都会挡住删除，无从竞态。两条论据必须并列写出——只写路径集那条，一次把裁剪改成递归的改动会满足每一条落在纸面上的规则（路径集仍是 `union(index, target)`），同时销毁 untracked 内容；没有可能被误删的内容，就没有需要归档的对象，**不做任何归档，也不需要归档**；（2）`Worktree.Reset` 无条件改写分支引用，直接抵消刚做的 CAS——**来源消失**：`Store.Revert` 现在先用 `applyTreeToWorktree` 把工作区与 index 收敛到目标 tree，再调用普通的 `Commit` 发布，`Commit` 以捕获的旧分支引用做一次 CAS、是唯一的写入者；旧算法「构造 commit 对象 → CAS 更新分支引用 → 调用 `Worktree.Reset(HardReset)` 刷新」整段删除，第二次无条件写引用的代码路径不复存在——「已知缺口」一节"`Commit` 只能察觉、不能阻止并发的分支改写"一条标为已关闭时留的这条尾巴，至此一并剪掉；`TestRevertWritesTheBranchReferenceExactlyOnce` 以计数钉住新旧差异：旧实现写两次分支引用，新实现只写一次；（3）不取 `.git/index.lock`——**已关闭**：index 重建（`rebuildIndexFromTarget`）跑在既有的 `withIndexLock` 之内，`TestApplyTreeToWorktreeRefusesWhenTheGitIndexIsLocked` 验证持锁时的拒绝；（4）跑在 `PlainOpen` 的工作区上，而非写会话固定的那组描述符——**已关闭**：`applyTreeToWorktree` 在没有固定 `worktreeFS` 时一律拒绝（`errUnpinnedWorktree`），只写写会话固定的那组描述符，`TestApplyTreeToWorktreeRefusesOutsideAWriteSession` 验证未持会话时的拒绝；（5）不调用 `checkNoAbsoluteSymlinks`——**已关闭**：在写入任何路径之前调用，拒绝是前置条件而非收尾检查，`TestApplyTreeToWorktreeRefusesAnAbsoluteSymlinkBeforeWriting` 断言拒绝发生后，此前已被编辑的文件仍保持编辑后的内容，证明拒绝确实先于任何写入。**该前置只覆盖工作区侧，目标树侧另有一道**（第二十四轮补记）：`checkTargetNoAbsoluteSymlinks` 在同一位置扫描目标树中的 symlink 条目并拒绝绝对目标——缺了它，revert 进一个含绝对 symlink 的提交会由 fu 自己把该链接写入工作区，随后被自己的 `stageAll` 拒绝，此后每条写命令都失败而只有手工 `rm` 能解开；`writeWorktreeEntry` 另有一行同义的纵深防御。**另需记入本条的是索引重建的原子性**：`rebuildIndexFromTarget` 必须经 `writePublicIndexAtomically`（rename 安装）而非 `Storer.SetIndex`（就地 `O_TRUNC`）——后者会让并发的 `git status` 读到被截断的索引，崩溃更会永久留下截断索引加陈旧锁，而本更新器刻意无 WAL、全部恢复叙事都押在「重跑即收敛」上，撕裂写入恰好摧毁该前提。`ResetWorktreeToHead`（`restore --hard` 的入口）与 `Store.Revert`（`fu revert` 的入口）是 `applyTreeToWorktree` 的两个调用方，以上五项关闭对两条命令同时生效：`restore` 不带 `--hard` 时仍止步于链接层，只报告、不触碰（见 §2「命令映射」）；带 `--hard` 时调用 `ResetWorktreeToHead`，把已跟踪路径复位到 HEAD，untracked 与 ignored 内容原样保留；`fu revert` 现由 `RevertOperations` 提供 CLI 入口，见 §4 revert n 行。**遍历次序也是安全论据的一部分，须与上述两条并列**（第二十五轮修复）：更新器改为「先删后写、删的一遍按名字倒序」两遍走完路径集，而非按序单遍交错。原先的单遍交错无法收敛已跟踪路径的**目录↔文件类型翻转**——`skills/alpha` 排在自己的子项 `skills/alpha/SKILL.md` 之前，于是写入在使该目录非空的已跟踪子项尚未删除时就被尝试，`removeWorktreeEntry` 以 `ENOTEMPTY` 拒绝，`restore --hard` 与 `revert` 因此修不好一个系统 `git reset --hard` 修得好、且经本文档承认的直接 Git 路径即可造出的状态，报错文案（"an unexpected directory"）描述的还是 untracked 那种情形。倒序是另一半：go-git 的 index 确实可以同时持有 `skills/dir` 与 `skills/dir/f.txt`（系统 git 拒绝这一对，go-git 不拒），正序会在子项仍在其中时走到目录自身的名字。两遍都不放宽可删范围——名字仍恒出自 `union(index, target)`，裁剪仍在 rmdir 处原子证明「空」，故目录中任何 untracked 内容照旧挡住删除，untracked 挡路那一例仍然拒绝而非收敛。同轮另修 `ENOTDIR` 的读法：普通文件占住某级父目录时该名字**不可能存在**，`worktreeMatchesTarget` 与 `removeWorktreeEntry` 的 `Lstat` 均按「不存在」处理（`os.IsNotExist` 不覆盖 `ENOTDIR`，须另行判断），前者据此走到 `writeWorktreeEntry` 由 `MkdirAll` 给出点名拒绝，后者据此如实回答「本无可删」而收敛；两处都不删除任何东西，所有权不变式不受影响。**类型翻转的补集须一并处理，否则上述对等性主张就强于代码**（第二十六轮修复）：index 可能在某名字上持**陈旧的文件 blob**（该路径被 staged 时确是文件，其后被目录取代，go-git 不清理这对条目），而目标只把该名字作为**目录**提及（其键为 `a/b.txt` 一类）。照 index 字面处理，删除遍会去 unlink 一个目标自己想要的目录，并因其中的 untracked 内容以 `ENOTEMPTY` 拒绝——而系统 `git reset --hard` 在此收敛：丢弃陈旧条目、把目标文件写入既有目录、保留 untracked 内容，仅打印 `unable to unlink` 警告（已实测）。故删除遍先查 `targetDirectories`（由目标键自身派生的祖先目录集合，git 不存独立目录条目，这是唯一可问之处）：名字落在该集合内且磁盘上确为目录时**跳过删除**，陈旧条目由随后的 `rebuildIndexFromTarget` 整体重写索引时自然丢弃。类型判断不可省——同名处若是普通文件则必须删除，否则写入遍无从在其下建目录；用 `Lstat` 而非 `Stat`，symlink 不是目标要的目录，删掉它才好让写入遍放一个真目录进去。此改动只会**少删**，从不多删；目标要的是**文件**而目录挡路的那一形态，其拒绝原样保留。裁剪的停止条件同轮由「止于 store 根的子级」改为「止于 pinned logical root」（`rootFilesystem.isLogicalRoot`）：原判据保护了挂载的 `skills` 根，却连带豁免了每一个顶级目录，于是经直接 Git 路径产生的用户跟踪目录（如 `misc/notes.txt`）在其最后一个已跟踪文件被删后留下空 `misc/`，正是裁剪本要防止的无声残留、只浅一层；`staging/` 与 `recovery/` 是仓库目录的兄弟、根本不在工作区内（`StagingDir`、`RecoveryDir`），不受此影响。
  - **与 git 的分歧**：真实 `git revert` 在脏工作区上拒绝执行（"Your local changes to the following files would be overwritten by merge ... Aborting"），因为对回退内容的文本合并可能与无关的未提交编辑冲突。`fu revert` 不是合并——`store.Store.Revert` 把工作区收敛到目标 tree 快照后重新发布，不存在补丁应用步骤，也就不存在可能的冲突。SPEC §5.3 已要求写命令在做自己的工作之前先把待处理的手工编辑折叠进历史；`fu revert` 遇到脏工作区时执行的正是这条规则——先 `Sweep`，把手工编辑记入其自身的 `external: manual modifications` commit，再继续 revert，而不是像 git 一样拒绝；
  - **已知残留**：`fu revert` 若在"工作区已更新、commit 未落地"之间崩溃，工作区已经落在目标内容上但 HEAD 未移动。**残留也可能是部分应用的树**（第二十四轮更正）：`applyTreeToWorktree` 在第一个路径错误上就返回，故真实状态既非旧态也非完整目标态，而是介于两者之间——这是要紧的那种，因为下一次 sweep 会把它整体当作手工编辑记录下来；下一次写命令的 sweep 会把它重新记录为一笔 `external: manual modifications` commit——revert 的效果保住了，它自己那条提交信息没有保住。本轮不为此新造 WAL。这一顺序（工作区先、commit 后）因此不可颠倒：反过来（commit 先、工作区后）会让同一次 sweep 在崩溃后把回退前的内容重新提交回去，静默撤销刚做的 revert；
- **事务所有权与补偿恢复**：`new` 在排他创建 staging 根后、写入首个文件前立即持久化只含根的所有权清单；此后每个条目都以描述符相对、`O_EXCL|O_NOFOLLOW` 的方式创建，立即核对实际写入的 identity、模式与内容摘要，再逐项扩展权威清单。基线摘要只从这份权威清单推导，不从稍后枚举到的整棵活目录反向认领；每次推进与 publish 前后都要求现场全量扫描与预期集合精确相等，未知或被替换的后代一律保留 WAL 并安全冲突。记录包含设备/inode、类型、模式以及文件摘要或链接目标。事务路径存在而 WAL 无清单，或任一现场对象不再全量匹配，都必须安全冲突；运行中可见的回滚与崩溃后恢复共用这一条路径，不再按名盲目递归删除。若命令 commit 已写但 WAL 尚未清除，先全量重验已发布目录，再以排他 rename 移入 recovery，并对移动后的同一对象再验；只有所有权成立后才还原配置并写补偿 commit。隔离载荷完成后不再执行“校验路径再 unlink”：它被排他 rename 到由原名与根 identity 派生的终态归档名，并在归档名上再次全量校验后保留。**此处说的是 `new` / `add` 回滚出的隔离载荷**；`rm` 已提交后的载荷改为回收，配置交换的记录、终态与归档亦然（分别见下方 rm 流程第 4 条与上文“fu.yaml 写入”一条）。**这里的“保留”是范围决定，不是原语所限**：`RemoveOwnedTreeAt` 以“退休后按清单复核再 unlink”同样做到了安全删除（见上文“fu.yaml 写入”一条），故 POSIX 缺少按 inode 条件删除推不出“只能保留”；这两条路径继续保留，只是本轮未改动它们。归档校验对现场与清单执行双向精确相等：清单记录的条目必须全部存在并逐项匹配，出现未知条目同样拒绝；缺失一项与被替换一项等价看待，都要保留 WAL 并安全冲突。清理本身只有一次 rename，不存在“清理到一半”的中间形态，因此不为旧格式保留宽松识别：`.fu-cleanup-*` 条目/根保留名从未被任何版本写出，本分支之前也没有发布过 WAL 格式，原先的兼容承诺随之撤销，旧工作区无需迁移。若日后确需兼容，应先为持久记录加上明确的 cleanup 版本，再按版本放宽，而不是让当前版本继续接受不完整载荷。原名与终态归档名两个根位置都不存在时，已失去“载荷仍被保留”的证据，必须保留 WAL 并安全冲突。终态归档也可在 WAL 清除前再次崩溃后幂等识别。任何竞态替换都被排他恢复或保留在归档名下，fu 不会自动删除它；
- **store 内不可读的文件会挡住每一条写命令**：入库必须读取 store 内的每一个文件，因此一个 `chmod 000` 的文件会让所有写命令在 sweep 阶段失败。这与系统 git 一致——`git status` 容忍不可读的未跟踪文件，`git add -A` 则以 "unable to index file" 失败——所以行为本身不可约减；已做的是把裸 errno 换成点名完整路径并给出处置办法的消息（`explainStagingFailure`）。判定"是否有变更"一侧不需要读权限，已修好（`statEntryNoFollow` 从分类用的 `fstatat` 直接构造 `FileInfo`）；
- **`store.Config.Save` 是无调用方的导出写入**（第十四轮记录，本轮更正）：写流水线走 `SaveConfigExpecting`（条件安装，见上文 fu.yaml 写入一条），`store.Init` 的引导写入走 `WriteFileAtomicNoReplace`（该分支只在目标不存在时运行，故用无替换写入端，并在发布后校验已装入对象）——`Save` 因此在生产代码里**一个调用方都没有**，第十四轮「只有 `Init` 这一个调用方」的记录不成立。连带结论（第十九轮）：`WriteFileAtomic` 这个**替换式**写入端至此同样没有任何生产调用方，其文档注释里「for the one case that needs it」已不再成立；两者一并记录，以免下一个作者照它选型。它仍是导出的、以替换式 rename 结尾的写入，摆在与 `SaveConfigExpecting` 同一个类型上，下一个写命令的作者会先看到它。同轮并列记录的 `skill.DigestFS` 与 `skill.DigestManifest` 一致性问题已关闭：`internal/skill/project_test.go` 在含 symlink、非默认模式与 `.git` 条目的混合树上断言二者相等；
- **锁**：`$FU_HOME/fu.lock` 文件锁（flock），写命令互斥；只读命令不取锁，接受瞬时竞态；
- **逻辑根与对账固定**：`Store.Open` 先以 `O_DIRECTORY|O_NOFOLLOW` 打开 `FU_HOME`，再相对已固定父根打开 `store/`、`store/.git/` 与 `store/skills/`（必须已存在，否则报错——`list` / `show` 这类只读命令也走 `Open`，不创建或修改任何 store 内容），并打开或按需创建机器本地、store 外的 `staging/` 与 `recovery/` 容器目录。只读命令创建缺失的这两个空容器符合 SPEC §9 的边界：版本库内容与 agent 目录均不变；代价是完全只读的 `$FU_HOME` 文件系统会拒绝 `Store.Open`。布局、HEAD 和配置验证以及身份捕获均经同一组描述符完成，返回前再核对逻辑名仍指向这些身份。写会话以 `openat(O_DIRECTORY|O_NOFOLLOW)` 重新打开并核对所有身份，整个命令复用这些描述符；配置与 worktree 走固定的 store 根、对象与引用走固定的 Git 根、WAL/准备区/技能内容各走自己的根，跨根迁移使用两个固定目录间的原子排他 rename。公开 `Reconcile` 自行打开检查会话、取得同一 `fu.lock`、完成未决恢复并从固定 store 根重新加载配置，写流水线则复用已持锁会话的内部对账入口；所有 store 目标都通过固定 skills 根以 no-follow 语义确认为真实目录后才可建链；
- **特殊文件遍历拒绝与稳定读取**：提供给 go-git 的固定根目录适配器不以阻塞式只读 open 探测未知条目。每个目录项先经相对目录描述符的 `fstatat(AT_SYMLINK_NOFOLLOW)` 分类；FIFO、socket 与设备立即返回带路径的“不支持类型”错误。普通文件与目录随后也只用 `O_NONBLOCK|O_NOFOLLOW|O_CLOEXEC` 打开并以 `fstat` 重验类型和 identity，因此分类后发生的 FIFO 替换同样不能让持锁写命令挂起；go-git 未经枚举而直接打开的 index、引用与对象等控制文件也在只读 open 时强制 `O_NONBLOCK|O_NOFOLLOW`，并在每次读取后复核同一描述符的 identity、类型、大小、mtime 与 ctime。同一套 descriptor-relative 规则用于读取 `fu.yaml`、WAL revision / 终态标记和 OwnedTree 文件哈希；配置读取与序列化共用 8 MiB 上限，WAL 写入端与读取端共用单文件 16 MiB 上限，并在实际读取时再次限制长度以覆盖打开后增长。普通文件读取完成后再次对同一描述符 `fstat`，设备、inode、原始类型/模式、大小、实际字节数、mtime 与 ctime 必须与读取前一致，否则按对象已变更处理；错误返回后锁按统一 defer 路径释放；
- **`fu commit`**：同一流水线的特化——无准备阶段，即"定向 sweep"（可限定单 skill 路径、可带 `-m`）**（设计，未实现）**；
- 链接操作不可事务化，以 reconcile 幂等性弥补。

### adopt 流程（AdoptPlan）

**已实现（plan 2）**：逐项与整目录两种形态均交付，流程细节见下；恢复由 `recoverAdoptSkill` 处理器承接——已提交态继续完成未竟切换（含整目录交换状态机），未提交态与 new/add 同路回滚。

整个流程按 skill 由一组不可变事务 revision 护航（见 §2）：该 skill 的首条 revision 在其任何 store / agent 变更之前排他落盘，阶段推进时追加，完成后追加终态标记；同一 `fu adopt` 中已完成的前序 skill 保持已提交，当前 skill 可留一条待恢复 WAL，后序 skill 尚未尝试。

**阶段一 · 入库（所有形态共同）**

1. **扫描与分类**：逐 agent 扫描 skills 目录，条目分五类——真实目录；指向外部的 symlink（只读其目标内容）；fu 链接（跳过）；适配器保留条目（排除，如 codex `.system`）；agent 目录本身为 symlink（只读扫描其目标，投放走整目录切换）。目标目录与外部内容在本阶段一概不被修改。

   **两种形态都在此处施加规则 7 的过滤，并区分两种"不是候选"**：缺 `SKILL.md` 意为「这不是一个 skill」，静默跳过——整目录形态下它会成为透传链接，逐项形态下它本就不归 fu 管；而 `skill.ParseMeta` / `skill.Validate` 失败意为「这是一个 skill，但违反规则 7」，须**带原因**报告（SPEC 规则 7 的「不合规拒绝并说明原因」），CLI 呈现为 `invalid:`。同名条目常同时存在于多个 agent，故同一名字的原因只保留一条，以免同一处修复被重复淹没；若另一 agent 提供了该名字的合规副本，则该名字已在被收编，此时再报 invalid 会自相矛盾，故不报；
2. **去重、冲突与来源**：多 agent 出现同名条目，规范化投影摘要相同则合并为一，不同则报冲突、该项整体跳过；symlink 条目的原目标路径唯一时记为 local 来源，不一致则省略 `source` 并警告，不静默取其一；
3. **写入 store**：候选内容按规范化投影复制至 staging 并通过校验 → 移入 store、写 fu.yaml → commit。开关编码：`enabled=true`（全局开），对已检测但收编前未拥有该 skill 的 agent 写显式 false 覆盖（现状矩阵不变，未来新增 agent 默认获得）；`--agent` 限定收编来源时，仍只读盘点其他已检测 agent 写入覆盖，避免收尾 reconcile 意外投放；

**阶段二 · 投放切换（按 agent 形态）**

4. **逐项切换**（skills 目录为真实目录的 agent）：扫描时把父目录、条目、来源的 identity、类型、原始 link target 与内容摘要写入事务。真实目录经固定父目录描述符逐项复核 → 把原条目排他退休到事务记录名并在移动后复核 → 将**限定精确的原树**复制到 recovery 并双向验证 → 仅清理已退休且仍匹配的树 → 建 store 链接 → 记录推进。归档保留树形、条目类型、普通权限位、文件字节、任意深度 `.git` 与原始 symlink target；不保留 setuid/setgid/sticky、owner、mtime 或 hard-link identity（同一 inode 的多个名字恢复为独立文件）。单个普通文件最多 64 MiB，超限时在退休前隔离该 agent，并把具体路径、上限和 `fu rm <name>` 后重试 `fu adopt` 的出路报告给用户。原条目为 symlink 时不复制其外部目标：状态机先排他写入并逐字节复核内容寻址的 `adopt-link-<digest>.json`（identity、mode、原始 target 与原路径），再退休并删除匹配链接。该记录已包含未来还原所需 authority，但当前没有读取它执行还原的 `fu restore` 命令，恢复仍需人工处理。安装到 store 的规范化投影仍排除 `.git`，破坏性恢复归档与安装投影是两个独立契约；任一同路径替换、归档篡改或来源变化都保留现场与 WAL 并报安全冲突；
5. **整目录切换**（skills 目录本身为 symlink 的 agent）：事务记录原始 link target、目标目录 identity 及其完整直属条目 manifest；在固定父目录旁构造并记录 identity 的隐藏 sibling 替代目录——收编条目为 store 链接，未收编、冲突与非 skill 条目为指向原目标的透传链接（agent 视野不变，性质为未纳管）。归档原链接和落地替代目录之前，通过固定描述符重验用户目标的 manifest 与被收编 skill 摘要，并重验 sibling 与备份 identity；替代目录已经落地后不再读取用户目标，只验证并清理 fu 自建的 backup。交换使用 descriptor-relative no-replace rename；child、sibling 根与 backup 清理均先原子退休再复核；

   **两侧比较的强度不同，依据是"该对象是否为 fu 所建"**（第十九轮收窄）：sibling 与 backup 是 fu 自己创建的，比较包含 inode identity（`sameDirSwitchEntries`）；用户的目标目录不是，只比较**名字集合与条目类型**（`sameDirSwitchTargetEntries`），既不比子条目 identity，也不比其 link target。名字集合必须成立，因为 sibling 的透传链接逐一镜像它；而透传链接的值是 `目标目录/条目名`，经该名字当时的内容解析，故子条目**被替换**并不使它失效。落地前另行验证被收编 skill 的摘要；一旦替代目录已落地，agent 已从 store 读取该 skill，目标副本不再被使用，故恢复只验证替代目录与 backup，不再验证用户目标。

   此处曾两次以过强的判据自伤：先是落地后仍重算摘要，用户编辑自己的文件即令其后每条写命令冲突（第十八轮 I6）；改后残留的 identity 比较更糟——`git pull`、编辑器原子保存、rsync 替换任一直属子条目都会换 inode，而字节可以还原、inode 不能，冲突因此**永久**。所有验证用户所拥有对象的函数现在统一把失败标为 target conflict；只要当前事务阶段能够证明、撤销或清理 fu 自己的 sibling / backup / retired 对象，该 agent 就进入 abandon 并到达终态，包括尚未移动用户对象的 `building`。用户目标、skills 父目录或原条目改变因此隔离当前 agent，不再把开放 WAL 留给之后每条写命令；fu 自建对象的 identity、内容或位置无法证明仍是硬冲突，保留现场与 WAL。分类边界按函数统一包装，而非在各调用点枚举个别比较；

   **未记录 identity 的对象有三处显式例外**。第一处是 sibling 根：`startDirSwitch` 先持久化 sibling 名、再 `Mkdir`、然后才持久化其 identity，这一个 syscall 的窗口里崩溃会留下 fu 拥有却无法证明的目录。此时接受三项合取为所有权证据：**目录为空**、**名字不可预测**（`.fu-skills-` 加随机后缀且受 `validDirSwitchPath` 约束）、**`rmdir` 原子地提供空性证明**；resume 与 abandon 两条路径共用该回收规则。第二处是已持久化 sibling 根内、上一批 revision 后新建但尚未记录 identity 的 child symlink：根 identity、持久化 manifest 中的名字与逐字节 raw target、不可预测的 cleanup namespace，以及退休后取得的 inode snapshot 共同约束它；任何不匹配都保留现场。inode 无法在对象存在前写入 WAL，这两处收窄避免把 fu 自己的崩溃残留变成永久冲突，同时不把一般的"空目录"或同名对象当作所有权证据。第三处是只读判定 `wholeDirAgentAlreadySwitched`：它不取得对象所有权、不删除或修改任何条目，只以持久化名字集合、条目类型与逐字节 raw link target 识别 agent 是否已经处于替代目录形态；任何不匹配只返回“尚未切换”，后续真正修改仍须经过各自的 identity / manifest 校验；

**阶段三 · 恢复与清理**

6. 全部中断状态由统一恢复入口按事务记录收尾或回滚，收尾前以清单校验现场（**完成判定基于经所有权清单验证的 store 内容与链接归属——`ValidateSkillOwned` + `ownsLink`，强于早期设计文本的"现场条目与 store 同名且投影摘要相等即完成切换"；任何未知 identity、字节或位置组合均保留全部版本并安全冲突**），终态为完成 / 回滚 / 安全冲突三态之一；事务 journal 是 recovery 下保留的本机操作证据，不是内容所有权清单；
7. **隔离**：任一 skill 失败不影响其他项，逐项报告结果；整目录形态下失败项以透传链接保持可见。

### rm 流程

**已实现（plan 2）**。`add` 的事务形状与 `new` 相同、并共用 `recoverInstallSkill` 处理器，故不另记；`rm` 有自己的两阶段状态机与独立处理器（`recoverRemoveSkill`），记于此处：

1. **前置检查**（`checkRemoveAvailable` + `checkRemoveStoreEntry`）：先以 `cfg.HasSkill` 为门；若请求的名字是 `LoadConfig` 已隔离的不合规条目，则给出完整校验原因与 `fu.yaml` 路径、要求手工编辑，而不是误报 `unknown skill`；再确认 store 条目仍是真实目录，普通文件、symlink 或其他类型在 WAL 创建前拒绝并给出移开后重试的出路；
2. **`snapshotted`**：在任何删除之前，把 store 中该 skill 的完整所有权清单写入事务记录，同时记下配置的原状，供回滚还原；
3. **`quarantined`**：内容以排他 rename 移出 skills 根、进入 recovery 载荷位置，并在移动后的对象上再次全量校验；此后写 fu.yaml、commit；
4. **恢复终态**：operation commit 已存在且校验通过 → 清 WAL，随后回收隔离载荷（**完成**）；否则内容按清单还原回 skills 根、配置复原、清 WAL（**回滚**）；现场与清单不再全量相等则**安全冲突**。**回收严格排在 `ClearTxn` 之后，这一次序是硬性要求**：WAL 仍打开时回收，会毁掉回滚分支要还原的那份内容；排在终态标记之后，它就永远不构成任何恢复的前置条件——此处崩溃或回收失败都只留下无人等待的孤立载荷，交由 `fu gc` 独立收拾（§2）。被删内容本身仍可从 git 历史取回，载荷只是第二份副本，故丢弃它无需自带防篡改检查，失败也绝不把一条已经持久成功的 rm 报成失败。**该处理器并不在未崩溃的命令路径上**：`pipeline.go` 的 `run()` 在 commit 落地后自行清除 WAL，`finishCommittedRemove` 因此只在崩溃恢复时被调用。为使普通 `fu rm` 走完全相同的次序，`Op` 增设默认为 nil 的 `afterTxnCleared` 回调，由 `run()` 在自身临界区内、`ClearTxn` 成功后立即触发；它仍持有本命令已取得的 `fu.lock`，无需为回收另开一次持锁会话（对一条已经成功的 rm 而言，那在用户看来就是卡死）。该回调没有错误返回：触发时操作已持久成功，其结果不容再被改变；目前只有 `rm` 设置它。**回滚一侧没有补偿 commit，也不需要**：该分支只在 `currentHead == startHash` 时进入，即命令的 commit 尚未发布，没有任何已入库状态需要抵消（`finishCommittedRemove` 的注释即为此说明）。这与 `new` / `add` 的安装流程不同——那里的 `rollBackCommittedInstall` 确有 `compensation-ready` / `compensation-committed` 两阶段——不要按那份记录为 `rm` 补上它并不需要的机制。`rm` 已不再调用归档原语，"载荷已归档、WAL 未清"这一第八轮曾令恢复永久卡死的窗口随之消失：载荷一直停在隔离名上，直到 WAL 清除后才被回收，而回收原语对已经不在的载荷不报错，故恢复照旧不另做前置检查（第八轮的教训见下方"两条恢复/报告经验"；归档原语对活动名与归档名的双位置幂等仍是 `new` / `add` 侧的现行约束）。

### 已实现的恢复/报告约束

- **两条恢复/报告经验（第十六轮记录，防再犯）**：其一，恢复函数必须对**自身产生的终态**幂等——第八轮发现 rm 恢复在"载荷已归档、WAL 未清"窗口后再次运行会因前置检查而永久卡死（已修复：归档原语自身对活动名/归档名双位置幂等，恢复不再另做前置检查）；今后任何恢复路径的新增前置检查都必须核对"该状态是否可能由恢复自身在上一轮制造"。其二，adopt 的 `Skipped` 必须可行动——第八轮发现"已纳管但某 agent 仍持有未纳管副本"的条目永远无法再收编，用户无从得知出路（已补诊断 warning）；今后任何"跳过"类报告若用户无命令可解除其后果，须在报告中给出出路；

### 已知缺口（本轮未覆盖）

以下缺口本轮已确认存在、判断为可接受，留给后续计划处理，记录于此以免被误当作新发现：

- ~~**adopt 会让 symlink 目标根部的 skill 静默消失（第十七轮记录）**~~ **（已关闭）**：扫描在创建事务前显式检测 `target/SKILL.md`；由于整目录交换无法在保持目标不变的同时安全表达该根级 skill，命令以可操作诊断拒绝整个 agent，保持原链接与目标不变，并提示把 skill 移到子目录或改用 `fu add`；
- **中断产生的孤立 payload 尚无状态报告与 GC（第十七轮记录；rm 一半已关闭，adopt 与 source scratch 仍开放）**：**rm 一半已关闭**——已提交 rm 的隔离载荷在 `ClearTxn` 之后就地回收，此处崩溃或回收失败留下的孤立载荷由 `fu gc` 在裁剪该家族 journal 之前、凭同一份清单收走，并整体排除任何未完成事务所主张的载荷名（§2）。**仍开放的两半**：adopt 归档复制在进程退出窗口可能留下尚未进入完整事务所有权状态的 payload；git source scratch 的正常关闭与路径替换已按 identity 安全处理，但进程在 `Close` 前退出仍可能留下 owned 或 quarantine `.fu-src-*`。这两类仍不自动猜测归属或删除。**报告的一半已关闭（第二十二轮）**：`fu status` 现按名字盘点 `staging/`，分三桶——待某次恢复收尾的（未完成事务的已发布 staged 根与其私有 reservation，未完成配置交换的候选与活动交换名）；今天无人回收的（`.fu-src-*` 三种形态、`.fu-retired-staging-*`、无记录背书的 `.fu-new-*` 与候选）；以及 fu 没有任何待处理记录可与之对应的条目（`Unmatched`，即前两类都不匹配的剩余名字）。第三桶单列而不并入第二桶，是因为它确有补救、只是不由 fu 执行：`fu new` 与 `fu add` 会拒绝复用这些名字并点名该路径，而刚被拒绝的用户跑 `fu status` 本应看得见挡路的是什么。staging 没有「可回收」与「按设计保留」两桶而 recovery 有，是因为 `fu gc` 从不查看 staging，且此处没有 SPEC §9 承诺保留的 authority。**GC 的一半仍开放，且开放得有道理**：staging 里每一条清理路径都是进程内的 defer（`source/scratch.go` 的构造失败回滚与 `Close`、`ownedtree.go` 的 reservation 清理），进程退出正因此留下残留，而这些名字的 identity 从未进入任何持久记录，故没有任何证据可据以证明归属——DESIGN 对 GC 的条件"在具备可验证所有权证据时"至今不成立。要让它成立须先为这些名字加持久 reservation，那是独立的一件事；
- ~~**SPEC 规则 7 的路径安全检查（symlink 逃逸等越界引用）未实现**~~ **（已关闭，plan 2；第二十一轮校正描述）**：`skill.ValidateLinks` 对投影条目做纯函数解析——绝对目标拒绝；相对目标按分量逐项解析（内核语义：`..` 作用于**已解析**位置、在根处再弹出即拒绝，分量落于树内 symlink 时把该链接自身目标压入 frame 栈、优先解析后再继续外层剩余分量；拼入的绝对目标亦就地拒绝）后须留在根内。判环只靠链长上限；另设单次解析的分量总数上限兜底。树内链接按路径分量 trie 索引，push 只查单个分量、pop 同步恢复父节点，成本不再随已解析深度增长；目标拼接不再反复复制剩余分量。对 macOS 默认文件系统的歧义检测使用 `x/text` 规范化/折叠后再取 `unicode.SimpleFold` 轨道的稳定代表，并以最终 NFD 保证幂等；每次校验缓存分量折叠键，每个 exact miss 都进入折叠查找，缓存最多保留 4096 个不同键以限制内存。检查在两处执行：`ScanSource` 扫描时（避免把逃逸候选当作可安装项展示给用户、再在批量安装中途失败，第十八轮 I7），以及 `add` / `adopt` 复制路径发布前的 staged 拷贝上；整目录盘点亦复用；
- ~~**WAL revision 与终态标记无保留上限、无 GC（第十九轮补记）**~~ **（已关闭）**：`fu gc` 以内容寻址的 `.pruned` 记录先绑定已完成事务的完整 revision 家族与终态标记，再删除该家族；任何删除前缀均可安全续跑。同一命令现已一并回收已完成 rm 家族的孤立载荷与配置交换残留；仍刻意不处理的是 adopt 归档载荷、`adopt-link-*.json` 与 source scratch，它们仍是独立缺口；
- **已完成事务的三重哈希校验已刻意放弃（第十九轮补记）**：见 §2"读取端对未完成与已完成两类事务的验证强度不同"。已完成事务不再重读 revision 字节，故可以篡改一条不会再被读取的记录；这是为换取"陈年坏字节不再永久阻塞每条写命令"而自觉付出的代价，记录于此以免被误当作疏漏；
- **`fu revert` 未对并发的直接 git 写入做基线比对（第二十三轮记录）**：写流水线的 `run`（`pipeline.go`）在 sweep 之后逐字节比对基线，发现期间落地的直接 git 写入即以 `ErrConcurrentStoreChange` 中止；`revert` 没有等价物。`applyTreeToWorktree` 会先把工作区与目标树收敛，之后才取树指纹，因此指纹能发现目标未命名的内容，却发现不了目标已命名路径上的**不同**内容——更新器已经把它覆盖掉了。窗口很窄：`fu.lock` 排除其他 fu 进程，须是一次直接 git 写入恰好落在这段持锁区间内。选择记录而非关闭，是因为关闭意味着把 `run` 整套基线比对搬给 revert，超出本批范围；其代价是本文档「sweep 使 fu 这边一个字节都不丢」的说法就 revert 而言强于代码所能支撑的程度。
- **`recovery/` 下有一批对象今天无人回收，`fu status` 已如实报告（第二十三轮补记）**：本轮交付的 `fu status` 会为它们打印"N that no command collects yet"，故按本文档 §0 的规矩（"已知缺口"只记录已交付部分中已知但未关闭的问题），这笔欠账应在此有一条。清单：无人认领的 `rollback-*`（其家族 journal 已被人工按 `addRecoveryConflictRemedy` 的放弃建议移走，清单已失）、`.fu-archive-*`（`ArchiveRecoveryPayloadOwned` 有意永不 unlink）、`reclaimConfigExchangeFile` 在 `recovery/` 顶层留下的随机名 `.fu-retired-entry-*`（随机名不可归属，与树内那种由清单派生的确定性同名前缀不同）、无家族可依的 `removed-*` 与 `.fu-retired-dir-*`（清单已失，`fu gc` 再不会产出这两个名字），满足宽松待处理文法却不满足严格收集文法的 `.fu-config-exchange-*`（编辑器留下的 `.json.bak`、非 hex 后缀——待处理扫描不认领它，`CollectableConfigExchangeNames` 也不收它），没有任何已完成交换所描述的 `.fu-config-archive-*`（名字畸形、或格式正确但其自述的 inode 已漂移——`reclaimConfigExchangeStatedArchive` 每次都保留它，`CollectableConfigArchiveNames` 因此把它计入 `Uncollectable`），以及原子写入协议的两个前缀 `.tmp-*` 与 `.tmp-retired-*`——`writeFileAtomicNoReplaceRoot`（`internal/store/fsutil.go`）以 `O_CREATE|O_EXCL` 建立 `.tmp-<16 hex>` 后经 `fillRegularFile` 写入并 fsync，唯一的清理是 defer，故此窗口内被杀即永久留下该名字；其失败路径的 `retireOwnedLeafAt(..., ".tmp-retired-", ...)` 在退休 rename 与 unlink 之间被杀则留下 `.tmp-retired-<32 hex>`。两个前缀既不被 `ReclaimCompletedConfigExchanges` 扫（它只匹配三条交换文法）也不被裁剪循环扫，故仍无回收者；但盘点已由兜底的 `default:` 分支把它们计入 `Uncollectable`，`fu status` 会为其打印"N that no command collects yet"，不再落不进任何一档；增长以崩溃次数而非命令次数为界，无数据丢失或安全后果。注意 `.tmp-` 是 `.tmp-retired-` 的前缀，将来若写单前缀清扫器须知道这一点，且 `.tmp-retired-` 的名字随机、与 `.fu-retired-entry-` 一样不可归属。写"每个前缀都登记了回收者"的枚举用例时，生产方清单应从对字面前缀的 grep 派生而非手工列举——这两个前缀正是手工列举漏掉的；
- **对账的隔离粒度止于 agent**：`ScanAgent` 遇到单个条目出错（如 store 内自引用链接触发的 ELOOP）会放弃该 agent 剩余的全部条目，但不影响其他 agent；条目级隔离尚未实现；
- ~~**只读命令报告期望而非现实**~~ **（已关闭）**：`engine.Application` 的 `ListSkills` / `ShowSkill` 读模型仍只来自 `store.Config`，不比对磁盘上的实际链接状态；但缺口本身由新增的 `engine.Status` 关闭——它经 `ScanAgent` 读取各 agent 的磁盘实况，与 `Desired` 比对计算 `Diff`；SPEC 规则 4 要求的"只读命令仅提示待投放"由 `AgentStatus.DirMissing` 兑现；
- ~~**cli 层绕过 engine 自行决策**~~ **（已关闭）**：`engine.Application` 现统一承担初始化/打开、读模型、来源准备、agent 检测、开关生效判定、reconcile 报告可见性及所有写编排；生产 CLI 非测试代码只依赖 engine，架构测试防止反向漂移。CLI 保留的逻辑仅为参数/flag 解析、add 的交互选择和结构化结果的文本呈现；
- ~~**`Commit` 只能察觉、不能阻止并发的分支改写**~~ **（已关闭）**：`Store.Commit` 已不再调用会重读可变 index、无条件写引用的 `Worktree.Commit`；它从冻结条目自行构造 tree / commit 对象，并以捕获的旧分支引用做 CAS。CAS 前落地但未发布的对象只是不可达对象，不会移动分支；CAS 后仍复核最终引用，以报告随后落地的直接 Git 写入；
- ~~**agent 目录与活动叶子替换竞态未关闭**~~ **（已关闭，退休名残余见 §2）**：父目录由 `ScanAgent` 记录 identity，执行时以 `O_DIRECTORY|O_NOFOLLOW` 打开并比对；其后的单分量操作全部相对该描述符。删除方向进一步使用 §2 的“原子退休 → 移动后复核 → 仅清理退休对象”协议，关闭原活动名上复核与删除之间的可预测叶子替换窗口；退休名复核后的条件 unlink 在 POSIX 上不可表达，保留 §2 已记录的同 UID 残余；
- **store 内绝对目标的 symlink 只能拒绝、不能如实记录**：go-git 的 worktree 跑在 go-billy 的 chroot 文件系统上，其 `Readlink` 把绝对目标改写为 `"/" + filepath.Rel(base, target)`。两个方向都坏——入库侧把改写后的文本当作 blob 记入历史（`git cat-file` 与磁盘 `readlink` 不符，`clone` 据此重建出错误链接，破坏 SPEC §9 的兜底与核心场景 4）；状态侧以同样方式读取，故 worktree 与自己刚提交的 blob 永远比较不等，`IsDirty` 恒真、`Sweep` 永不收敛，证伪 §4"sweep 使 worktree 常态干净"的前提。已确认元凶是 go-git 核心而非本项目的 `SkipStatus` 强制入库路径：裸 `AddWithOptions{All:true}` 产出同样的错误 blob，且即便用系统 `git add -A` 正确提交，go-git 的 `IsDirty` 仍返回 true。**本轮的处理是检测到即明确报错**（`store.ErrAbsoluteSymlink`，由 `Commit` / `IsDirty` / `Sweep` 共用的 `checkNoAbsoluteSymlinks` 在入库前拒绝，错误消息点名该条目），不是修复：如实记录需绕开 go-git 自行写 blob 与索引项**并**自行计算状态，超出本轮范围。相对目标的 symlink 不受影响（chroot 原样透传），照常入库且收敛。**第十三轮更正**：以上两句此前只在 `os.DirFS` 视图下验证过，而写命令实际走的是会话内的 `rootStandardFS`。该适配器当时只实现了 `ReadLink` 而无 `Lstat`，不满足 `fs.ReadLinkFS`，于是 `fs.ReadLink` 对 store 内**任何** symlink 都返回 `ErrInvalid`：绝对目标的具名拒绝根本无法触发，相对目标反而成为主要受害者——一条手工链接即让每条写命令在 sweep 阶段以 `invalid argument` 失败，而读命令照常，store 看起来是健康的。同时 `symlinkFileInfo.Size()` 恒为 0，与 go-git worktree noder 按 `NewHasher(BlobObject, size)` + 目标字节求哈希的方式矛盾，即使补上 `Lstat`，链接也会被 `Status` 永远判为已修改，`IsDirty` 恒真而 `Sweep` 不收敛。两处均已修复（`rootStandardFS.Lstat` 委托，`symlinkFileInfo` 取 `fstatat` 已得到的 `st_size` 与 mtime），并以会话内的提交与收敛用例固定。fu 自己从不在 store 内创建 symlink（链接是从 agent 目录指**向** store），故该形态只可能来自手工编辑（场景 7）或将来的 adopt。
- ~~**`store.Home()` 不做绝对化**~~ **（已关闭，第七轮）**：`Home()` 现对 `FU_HOME` 与 `HOME` 回退两条路径都要求绝对路径，非绝对即报错——归一（`filepath.Abs`）会让 store 的**身份**取决于 `fu init` 首次运行的目录，比明确报错更隐蔽。适配器一侧的同类缺陷已于第六轮由 `agent.homeDir()` 关闭。此条保留为记录，以免被误当作新发现。
## 7. CLI 与错误处理

- **框架**：cobra——子命令、`--help`、shell 补全免费；
- **Go 测试 flag 兼容**：pflag 会静默跳过单短横线的 `-test.*` token；`execute()` 仅在该 token 会作为选项解析时把它改写成普通未知长 flag，以进入统一的 usage-error 路径。已知 flag 的独立值、`--` 后的位置参数及 `__complete` / `__completeNoDesc` 的补全输入保持原字节，不参与改写；命令函数测试与真实二进制 smoke test 同时覆盖该边界；
- **输出**：人读为主，`fu list` 对齐表格呈现开关矩阵；`--json` 不入 v1；
- **错误**：sentinel error（如 `ErrStoreNotFound`、`ErrTxnConflict`、`ErrConcurrentStoreChange`、`ErrAbsoluteSymlink`）+ `%w` 包装。**"用户可读消息统一在 cli 层拼装"是设计意图，当前两个方向都未达成**（第十三轮）：核心包直接拼装面向 CLI 的文案（`internal/store/store.go` 的 "run `fu init` first"、`internal/store/config.go` 的 `git -C %s checkout -- fu.yaml`、`internal/engine/ops.go` 的 `fu new %s`），而 cli 层不做任何组装，`internal/cli/exitcode.go` 直接打印整条 `%w` 链，内部包装层因此原样泄漏给用户（第十四轮举证的 `check operation preconditions` 与 `execute mutation` 两个步骤名本身已被移除，但机制未变，例如恢复路径的 `recover pending transactions: recover transaction ...` 前缀）。底层的 sentinel 与 `%w` 包装本身是对的，缺的只是 cli 侧的翻译；
- **退出码**：0 成功、1 操作失败、2 用法错误——用法错误包含四类：参数数量不对、未知 flag、**格式不合法的 flag 取值**（`--agent ""`、`--agent <无此适配器>`、显式空 `--ref`、不合法或不适用于来源类型的 `--ref`），以及**格式不合法的位置参数**（`fu revert abc` / `fu revert 0`）。前两类由 `UsageError` 在 cobra 生成对应错误的原处构造（`SetFlagErrorFunc`、`Args` 校验器）；第四类同样在 `RunE` 内构造 `UsageError`，见 `cli/revert.go`；第三类在 `RunE` 内由 engine sentinel 判定后包装（`engine.ErrEmptyAgentScope`、`engine.ErrUnknownAgent`、`engine.ErrEmptyAddRef`、`engine.ErrInvalidAddRef`），因为「这个名字是否对应一个适配器」与来源/ref 组合是否合法只有 engine 知道。对 `adopt`，已知但本机未检测到的 agent 是环境事实而非书写错误，计为操作失败、退出码 1；`enable` / `disable --agent` 则允许为这类 agent 预先持久化 override 并成功，待 agent 可用时生效。凡退出码 2 的路径都同时打印 usage（含未知子命令一路）；未知子命令的判定必须在 `root.Execute()` 返回之后，用 cobra 自身的只读 `Find` 复核——`help` / `completion` / `__complete` 等内建子命令要到 `Execute()` 内部才注册，提前判断会把它们也误判为未知命令；其余错误（含 `Reconcile` 因 `Result.Failed` 非空而返回的 `ErrOperationFailed`）一律计为操作失败。判定逻辑集中在 `internal/cli/exitcode.go` 的 `execute()`；
- store 未初始化时，除 `init` / `clone` 外的命令统一前置报错并提示。

## 8. 测试策略

| 层 | 手段 | 覆盖 |
|----|------|------|
| 单元 | engine.Diff 表驱动 | 断链 × 未纳管 × 新 agent 的状态组合穷举。全局 × 覆盖两维不在此层：`Diff` 收到的已是解析后的布尔值，该维度由 `Config.Effective` 的用例覆盖 |
| 单元 | fu.yaml 读写往返 | 含未知字段保留、同值归一 |
| 集成 | `t.TempDir()` 构造 FU_HOME 与假 agent 目录（覆盖 `HOME`） | 已交付十三条命令的读写路径（含 git 来源端到端、adopt 双形态、gc 裁剪边界及 status/restore/revert 的场景走查）。restore 收敛性覆盖链接层与 `--hard` 的工作区复位半层；revert 覆盖连续回退与脏工作区先 sweep 再回退两类场景 |
| 集成 | 本地 bare repo 充当远端 | `file://` 覆盖无 subprocess 的本地物化路径；localhost `git daemon` 的 `git://` fixture 覆盖真实 `git.CloneContext` transport。两者共同覆盖 branch 与 annotated tag 的 commit 锁定；store 的 `Pull` / `Push` / `Fetch` / `Clone` / `Remote` 仍**（设计，未实现）** |
| 端到端 | 命令函数级联走查 | `scenario_test.go` 走查场景 7、场景 2 与场景 5 的 store 侧，调用 engine API 而非 CLI。场景 1（安装）与场景 6（收编既有环境）自 plan 2 起已不依赖未交付命令，功能覆盖在 `add_test.go`、`adopt_test.go` 与 `adopt_whole_test.go`，但尚未整理进这份场景走查——这是组织缺口而非测试缺口（SPEC §8 的验收标准是场景形态的，故记在此处）；其余核心场景仍依赖未交付命令 |
| 端到端 | 编译后二进制冒烟（go build + exec） | 1–2 条主路径，防"函数级可用、装配层坏"的盲区 |
| 故障注入 | 关键中断点模拟崩溃（`os.Args[0]` 派生子进程 + `os.Exit`，不跑 defer/回滚/Close） | 已交付：`new` 的两个所有权窗口、事务记录各阶段切换点、恢复内部六个边界、配置交换四个崩溃点；plan 2 追加：`add` 复制各窗口、`rm` 隔离各窗口、`adopt` 入库/切换/整目录交换各窗口。update 的 `.old` 残留随 update 一并延后。**工作区更新器（`applyTreeToWorktree`）刻意不用这套派生子进程注入，改以进程内中断覆盖**（第二十五轮记录）：`TestApplyTreeToWorktreeConvergesAfterAnInterruptedEntryLoop` 在条目循环中途停下，`TestResetWorktreeToHeadConvergesAfterAnInterruptedRun` 借持有 `.git/index.lock` 停在工作区已改写、index 未重建的那一步。这是形态之别而非覆盖之缺：派生子进程注入的价值在于跳过 defer/回滚/Close 以检验**崩溃点恢复**，而本更新器刻意无 WAL、也没有单独不安全的中间态，需要钉住的是"重跑即收敛"这条唯一的恢复叙事；唯一的原子性要求（index 经 `writePublicIndexAtomically` 以 rename 安装）另由 `TestRebuildIndexFromTargetInstallsTheIndexByRename` 直接钉住。记录于此，以免日后被读作漏做 |

平台：CI 以 macOS 与 Linux 为必需（SPEC 声明双平台支持，symlink 行为需双侧验证）。

## 9. GUI 预留

v1 不写任何 web 代码。预留体现为两条纪律：

1. 业务逻辑不出 engine；
2. cli 层不含决策逻辑，只做参数解析与呈现。

将来 `fu web` = 新增与 cli 平级的包，`embed` 前端资源，调用同一 engine。

## 附录：技术决策记录

| 决策点 | 选择 | 理由 |
|--------|------|------|
| 实现语言 | Go | 单二进制、`embed`、标准库 HTTP 契合工具形态；编译快、心智简单；旧原型的 Rust 经验以模式而非代码形式迁移 |
| git 集成 | go-git 内嵌 | 二进制零外部依赖；代价为 pull 仅快进、revert 快照前滚、认证覆盖标准场景，均有"用户直接以 git 操作 store"兜底 |
| CLI 框架 | cobra | 子命令、帮助、补全免费，Go CLI 事实标准 |
| Unicode 路径等价 | `golang.org/x/text` 的 normalization / full fold + `unicode.SimpleFold` 稳定代表 | 识别默认 APFS 上大小写与规范化等价但字节不同的名字；代表函数必须幂等，避免 Cherokee 等多成员 fold 轨道在重复应用时翻转 |
| 核心抽象 | 对账引擎（desired × actual → actions） | 与 Terraform plan/apply、Kubernetes reconcile 同构；status 即 plan，restore 即 apply；Diff 纯函数化支撑穷举测试 |
| 锁 | FU_HOME 下 flock，写互斥、读不锁 | 自用场景瞬时竞态代价为零，换取实现最简 |
| 前向兼容 | fu.yaml 带 version、未知字段保留；高版本拒绝写入 | 为 roadmap 功能（更多 agent、web GUI 等）留路 |
| 内容基线 | fu.yaml 记录安装摘要（digest） | sweep 使 worktree 常态干净，git status 无法承担"本地修改"判定 |
| 链接所有权 | readlink 规范化后与 `store/skills/<条目自身的名字>` 精确相等即 fu 所有；不设本机 manifest | manifest 是会漂移的第三份状态；判据是恒等而非包含——包含会把用户自建的、指向 store 内某 skill 的链接也算作 fu 所有（第六轮 Critical）；恒等比较仍在路径字符串层面，不是文件系统身份判定，覆盖已发生过的场景（$FU_HOME 祖先变为 symlink、用户自建的转接链、跨名与深层目标），不是全部可能的路径别名 |
| adopt | 三阶段 AdoptPlan：入库（全形态共同）→ 投放切换（逐项 / 整目录）→ 恢复清理 | store 事务与投放事务分离，整目录形态不再依赖未执行的入库步骤；拒绝的是**内容永久所有权清单**，保留的 append-only 事务 journal 只证明一次本机操作的阶段与终态 |
| 崩溃恢复 | `new`/`rm` 按命令、`add`/`adopt` 按 skill 的不可变事务 revision 先于该项任何变更排他落盘（WAL 式），文件名提交本 revision 摘要且内容链接前一摘要，阶段只追加，完成标记绑定最新摘要 | 任意下一次写命令先验证完整连续链再到达定义终态；任何既有路径都不会被 WAL 更新覆盖或在完成时删除，`.old` 等残留仅在匹配记录时处理；保证覆盖进程崩溃，不覆盖掉电——原子写未对父目录 fsync，rename 能否在掉电后存活不受保证 |
| 认证范围 | SSH（ssh-agent）+ 公开 HTTPS | 私有 HTTPS 指引走 SSH；若认证或合并痛点超预期，切换系统 git 的影响面限于 internal/store 与 internal/source 两个封装点（退路） |
