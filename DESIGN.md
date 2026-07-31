# fu v1 系统设计

- 状态：已评审（2026-08-01 讨论定稿）
- 配套：`SPEC.md` 回答 Why 与 What；本文档回答 How
- 技术选型：Go（单二进制）· go-git（内嵌 git）· cobra（CLI 框架）

## 0. 本轮交付范围

本文件描述 fu v1 的完整设计。**plan 1 只交付其中六条命令**：`init`、`new`、`list`、`show`、`enable`、`disable`。
`add`、`adopt`、`update`、`outdated`、`rm`、`log`、`revert`、`restore`、`commit`、`pull`、`push` 均未交付。

因此文中凡描述未交付部分的段落一律标注 **（设计，未实现）**，读者不必从"已知缺口"的缺省推断某处是否已经存在。
已标注的有：§3 的 `source` 模式与锁定信息、§4 的 pull / push / revert 行、§6 的 AdoptPlan 流程、§8 测试表中依赖未交付命令的行。
"已知缺口"只记录**已交付部分**中已知但未关闭的问题，不重复记录尚未开始的工作。

## 1. 模块划分

```
fu/
├── cmd/fu/main.go        — 入口，装配子命令
├── internal/cli/         — 各子命令实现：参数解析、调用下层、呈现输出
├── internal/store/       — store 布局、fu.yaml 读写、git 操作封装（go-git）
├── internal/skill/       — SKILL.md 解析与校验、目录扫描（多 skill 仓库）
├── internal/source/      — 来源抽象：git 来源、本地目录来源；锁定信息的取得（设计，未实现：本轮无此包）
├── internal/agent/       — 适配器：检测、skills 目录；claude、codex 两实现
└── internal/engine/      — 对账引擎与写操作流水线（业务编排唯一所在）
```

依赖方向单向：`cli → engine → {store, source, agent, skill}`。本轮实际的非测试依赖为
`cli → {engine, store, agent, skill}`、`engine → {store, agent, skill}`、`store → skill`
（`skill.ValidateName` 与 `skill.DigestManifest`）——图仍是无环的，其中 `cli → store` 一条即上文"cli 层绕过 engine"的缺口，
而 `store → skill` 这条 §1 画成平级的边此前未记录。

- engine 是唯一的业务编排层；cli 不含任何决策逻辑。
- 将来 `fu web` 加入时作为与 cli 平级的包，调用同一 engine——SPEC §5.2 "为 GUI 预留同一调用面"在此兑现。

### 磁盘布局

```
$FU_HOME/                 (默认 ~/.fu)
├── store/                (git repo，多机同步范围)
│   ├── skills/<name>/    (skill 实体)
│   └── fu.yaml           (开关状态、来源锁定、内容摘要)
├── staging/              (写操作准备区；与 store 同文件系统，保证 rename 原子且不跨设备)
├── recovery/             (WAL revision 与终态标记、事务载荷归档、配置交换记录与归档；store 之外，不入版本与同步)
└── fu.lock               (文件锁；本机自用，不入版本)
```

锁文件置于 store 之外，符合 SPEC "store 之外为本机自用" 的划分。

## 2. 对账引擎

整个 fu 归结为一个纯函数加一个执行器：

```go
// desired：由 fu.yaml + 开关规则计算（agent 覆盖优先，否则跟随全局）
// actual： 扫描各 agent skills 目录所得
// Action： CreateLink | RemoveLink | ReportConflict | ReportForeign | ReportDisabledForeign | ReportReserved | ReportInvalid
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
- **动作类型**：`Diff` 与 `Desired` 共同产生七种 `Action`——两种改变链接的动作 `CreateLink`、`RemoveLink`；五种只报告、绝不触碰磁盘的动作 `ReportConflict`（期望有链接的路径被未纳管条目占据）、`ReportDisabledForeign`（期望关闭的 skill 的路径被未纳管条目占据——用户自己的 disable 是否真正生效，故报告而非静默）、`ReportForeign`（fu.yaml 完全没有意见的名字，磁盘上是未纳管条目，纯信息性、为将来的 `fu status` 保留，写命令不打印）、`ReportReserved`（skill 名与适配器声明的保留条目同名）、`ReportInvalid`（skill 名未通过 `skill.ValidateName` 校验）；
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

执行阶段还会产生两类 `Diff` 本身算不出的结果——`Diff` 是纯函数，不碰文件系统：`CreateLink` 执行时通过写会话固定的 skills 根以 no-follow 语义复核 store 侧条目，不存在或不是真实目录（普通文件、symlink、FIFO 等）均跳过创建、计入 `Missing`；`CreateLink` 撞见 `EEXIST`（如大小写不敏感文件系统上，一个大小写不同的未纳管条目已占住目标路径，`Diff` 按名查找区分大小写、认为该位置为空）或 `RemoveLink` 复核发现条目已被替换，计入 `Conflicts`；某 agent 扫描本身出错（如 store 内自引用链接触发的 ELOOP）则整个 agent 计入 `Failed`。

执行器在应用 `RemoveLink` 前重新 lstat / readlink 核对该条目仍为 fu 所有，已被替换则跳过并计入 `Conflicts`。扫描与执行**锚定同一个目录身份**（见 `AgentState.OpenCheckedDir`）：扫描本身按路径名读取（`os.Lstat` / `os.ReadDir` / `os.Readlink`）并记下该目录的身份，只有执行阶段持有描述符；执行前以 `O_DIRECTORY|O_NOFOLLOW` 打开最终分量并比对描述符身份，因此最终分量即使是指回原 inode 的 symlink 也会被拒绝；此后的建链、复核与删除分别通过 `symlinkat` / `fstatat` / `readlinkat` / `unlinkat` 相对该描述符进行，且只接受单路径分量。故扫描之后把路径换成别的目录，既不能把新建的链接引到别处，也不能让删除落到 fu 从未分类过的条目上。仍不能彻底消除的只剩最后一档：同一描述符上的复核（lstat + readlink）与随后的 `unlinkat` 之间那段极窄的间隙。

`Result` 共八个字段：`Conflicts`、`Foreign`、`DisabledForeign`、`Reserved`、`Invalid` 直接对应同名的 Report 动作（`Conflicts` 还收纳上文两类执行阶段发现）；`Missing` 复用 `CreateLink` 动作本身，执行时发现 store 侧内容不存在或不是真实目录时计入这里、未真正创建；`Skipped` 是 agent 名列表，agent 的 skills 目录本身是 symlink 时在 `Diff` 运行前即整体前置拒绝（SPEC 规则 10）；`Failed` 收纳条目级或 agent 级的意外错误。八个字段中只有 `Failed` 非空会使 `reconcileChecked`（`Reconcile` 与写流水线共用的内部入口）返回 `ErrOperationFailed`、令进程退出码为 1（判定在 `reconcileChecked` 尾部 `len(res.Failed) > 0`，经写命令的调用链传回 `internal/cli/exitcode.go` 的 `execute()`，见 §7）；其余七个字段都是 fu 主动、正确拒绝自行处理的可执行状态，命令仍以退出码 0 收尾，诊断打印在 stderr。
- **命令映射**：
  - `status` = 计算 Diff 后只读呈现，附 store 工作区状态（`worktree.Status()`）与远端状态（ls-remote 查询，不落盘）；
  - `restore` = store 工作区复位到 HEAD（撤销未提交手工删改）→ 应用 Diff 全部 Action；
  - 所有写命令收尾统一执行一次 reconcile（算 Diff、应用链接层 Action）。"新 agent 首次投放由下一次写操作或 restore 完成"（SPEC 规则 4）由此自动成立，无需专门逻辑。

Diff 是纯函数，表驱动测试穷举状态组合；文件系统副作用全部收敛在执行器。reconcile 幂等。

**命令级事务记录与统一恢复入口**：多阶段写命令（new、adopt、update）先完成统一恢复、配置检查、sweep 与命令自身的只读前置检查，然后在**该命令自身的任何 store 或 agent 变更之前**，在 recovery 中原子且排他地追加一条命令级事务记录——随机事务 ID、单调序号、操作类型、起始 HEAD、预期新状态、涉及目标与当前阶段；阶段推进追加新的不可变 revision，完成时追加排他的终态标记，既不替换旧 revision，也不按固定路径删除。每个 revision 文件名提交其精确字节的 SHA-256，内容携带前一 revision 摘要；恢复从序号 1 起读取并验证完整连续链，而不是只信最高序号。终态标记只记录事务身份、最新序号与该 revision 的精确摘要，只有完整链与标记同时匹配才视为已完成。revision 与标记的写入端和读取端共用 16 MiB 上限，超限在排他创建前拒绝。旧 revision 与终态标记作为本机所有权证据保留；`.old`、sibling 备份等现场残留只是事务载荷，仅在与记录匹配时才被处理；无记录匹配的 `.old` 按普通内容对待，不会被当作残留清理。所有写命令取得锁后的第一步是按记录恢复未完成事务，终态为三者之一：**完成、回滚、安全冲突**（现场在崩溃后被外部改动时如实报告，不强行收敛）；恢复完成前不执行普通 reconcile，sweep 不把与记录匹配的事务残留当外部修改。`status` 只读报告未完成事务。由此，任何中断后的下一次写命令或 `fu restore` 都到达定义的终态，用户无需知道崩溃发生在哪条命令。

## 3. fu.yaml schema

`source` 及其全部字段为**（设计，未实现）**：本轮没有任何非测试代码读写 `source` / `url` / `ref` / `ref_kind` / `commit` / `subdir` / `path`，
`internal/source/` 包也不存在。直接的后果是 `fu show` 目前不显示 SPEC §5.1 的"来源"与"锁定版本"两项——不是遗漏，是这部分尚未开始。

```yaml
version: 1
skills:
  pdf-tools:
    source:                    # （设计，未实现）
      type: git              # git | local
      url: https://github.com/x/skills
      ref: refs/heads/main   # 安装时解析并持久化完整形式；缺省时解析为当时的默认分支并固定
      ref_kind: branch       # branch | tag | commit，SPEC 规则 9 行为分派的依据
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
- **skill 名校验**：`skills:` 下每个键即 skill 名，会被当作路径分量拼进 store 与各 agent skills 目录（`fu show` 的 SKILL.md 读取、engine 的链接物化均如此）；`LoadConfig` 载入时对每个键施加与 `fu new` 相同的命名规则（Agent Skills 规范：小写字母数字与连字符、不以连字符首尾、无连续连字符、长度 1–64）。**处置为逐条目隔离，不是整体拒绝该 fu.yaml**：不合规的条目被排除出该 config 的 skill 集合（`SkillNames`、`HasSkill`，以及一切以此二者为门的访问器），因而不合规名字不作为路径分量参与任何计算；其余条目照常可读可写。底层文档不动——隔离发生在访问器边界而非从 `c.doc` 删除——故 `Save` 原样往返该条目，不会因一次无关写入把它从 fu.yaml 抹掉（本计划无 `fu rm`，抹掉即不可恢复）。被隔离的名字记入 `Config.InvalidNames()`：写命令经 engine 的 `configInvalidNames` 整趟折叠一次报告为 `ReportInvalid`（与 agent 无关，零 agent 时同样报告；但若该名字同时是某 agent 的保留条目，则由 `Desired` 逐 agent 判为 `ReportReserved`，保留名的诊断更具体，优先），只读命令经 `printInvalidNames` 报告。早先"任一不合规即整体拒绝"的做法代价过大：一个坏条目会让 `fu list`、`fu show <任意名>` 等只读命令全部失败，且使"回收记在不合规名字下的残留 fu 链接"这一修复在生产中不可达——没有命令能拿到一个 `LoadConfig` 已拒绝构造的 `Config`；
- engine 的 `Desired` / `Diff` 各自保留一份相同校验作纵深防御。经 `LoadConfig` 得到的 `Config` 不会让这两处观察到不合规名字（该名字根本不进 `SkillNames()`），故这两份副本对生产路径不可达；它们防的是将来某个调用方不经 `skill.ValidateName` 直接改动 `Config`（`AddSkill` 本身刻意不校验）。**注意 `Desired` 把 `cfg.InvalidNames()` 折进报告这件事本身是生产路径**，与上述副本不是一回事；
- 同值归一只在 agent 级开关写入时执行：`overrides[agent] == enabled` 的条目删除；全局开关写入不触发（SPEC §4.1）；
- **摘要算法（规范化投影）**：skill 目录内的**文件与 symlink**（**统一排除 `.git`**，按名排除，任何深度、任何条目类型）逐项纳入路径、文件内容 hash、可执行位、symlink 目标，编码为带类型前缀的记录后按（类型, 路径）排序，整体取 sha256。**目录本身不参与**：git 不独立保存目录，空目录根本无法表示，若为每个目录记一条，一个含空目录的 skill 在 store 工作区与其新克隆中就会永久算出不同摘要；代价是增删空目录对 fu 不可见，这一点与 git 自身一致。该投影唯一：local add、adopt、update 的复制与 store / source 双侧摘要一律使用它——复制与摘要永远看到同一集合。安装与更新时计算写入 `digest`；
- **`source` 可省略**：`fu new` 与真实目录收编的 skill 无上游，update / outdated 对其跳过并提示；symlink 收编的条目在原目标路径唯一时记录其为 local 来源，多处路径不一致则省略并警告（见 AdoptPlan）；
- local 来源的 `path` 为绝对路径，随 store 同步到其他机器后仅作提示信息——该来源只在路径存在的机器上可更新（SPEC 规则 9）。

## 4. git 封装（go-git 边界决定）

| 操作 | 实现 | 说明 |
|------|------|------|
| commit | 私有 stage + 冻结候选 + CAS 发布 | `PrepareCommit` 先在短暂持有 `.git/index.lock` 时只读捕获公共 index 基线，随后在共享真实对象库、但 `Index` / `SetIndex` 完全落于内存副本的私有 repository 中只 stage 一次，冻结由路径、Git mode 与 blob hash 组成的完整树；直接 Git 在验证、WAL 追加与发布期间始终看不到 fu 的临时候选。commit 直接从冻结条目递归构造 tree 与 commit 对象，分支只以捕获的旧引用做 CAS 更新（§6"命令提交候选冻结"）。发布成功后仅当公共 index 的完整结构仍精确等于捕获基线、且该基线在准备时与 HEAD 同树，才在重新取得 `.git/index.lock` 后把它同步到已提交候选；期间到达的直接 Git index 内容原样保留。候选被放弃不需要还原公共 index，因为准备从未写入它。他人已持有锁时 fu 与 git 一样停下并报出锁路径。提交信息格式 `<操作>: <对象>`，如 `new: pdf-tools`、`disable: writing --agent codex`、`external: manual modifications`；`fu log` 的展示直接以此为素材 |
| pull | fetch + 仅快进 | 检测到分支分歧即停，报错附 store 路径与建议命令，用户以系统 git 处理（store 是标准 repo，SPEC §5.1 已同步此语义） （设计，未实现）|
| revert n | 快照前滚 | 读取 `HEAD~n` 的 tree，构造新 commit 对象（tree = 目标树，parent = 当前 HEAD 且为唯一父），CAS 更新当前分支引用，随后以 `Worktree.Reset(HardReset)` 刷新 index 与工作区。不经 checkout（避免 detached HEAD）。**不做 reconcile，也不取 `fu.lock`**——`Revert` 目前无 CLI 入口，只被测试与内部场景调用，见 §6 已知缺口。测试须覆盖连续回退：A-B-C → revert 1 → D(parent=C, tree=B) → revert 1 → E(parent=D, tree=C) |
| push / fetch | go-git transport | SSH（ssh-agent）与公开 HTTPS（认证范围见下）；奇特配置以"直接用 git 操作 store"兜底 （设计，未实现）|
| 外部修改 sweep | HEAD→公共 index 与 index→worktree 分层入库 | 写操作与 push / pull 执行前统一检查（SPEC §5.3）；先精确提交用户已 staged 的公共 index 快照，再以私有 index 提交其后的 worktree 快照，二者不同时产生两笔同为 `external: manual modifications` 的有序历史 |

git 来源的取得（add / update）：浅克隆（depth 1）指定 ref 至 `$FU_HOME/staging/`，解析出完整 commit hash 后从 git tree 导出 `subdir` 内容（不含 `.git`，仓库根即 skill 时同理）；`outdated` 用 ls-remote 式查询比对 ref 头与锁定 commit，不产生本地写入。

**完整入库投影**：`Worktree.Status` 可见的修改加上 store 中所有「磁盘上存在但 index 中不存在」的非目录条目，任意层级的 `.git` 除外；后一部分使未跟踪且被 `.gitignore` 隐藏的内容也不会漏掉。脏状态查询严格无副作用，并分别保留 HEAD→公共 index 与 index→worktree 两层：例如 HEAD/工作区为 A、公共 index 为 staged-only 的 B 时，sweep 先提交 B，再提交 A，既不抹掉 B，也不把两个时序状态压成一笔。普通 `Commit` 在私有 index 中应用同一完整投影；`Sweep` 与事务恢复共用同一遍历规则，恢复在补偿 commit 前拒绝任何事务范围外的变更，不先改动公共 index。

**基线三态判定**：`本地修改 = digest(store) ≠ 记录基线`（sweep 使 worktree 常态干净，`worktree.Status()` 不能承担此判定）；`outdated = digest(source) ≠ 记录基线`（local 来源；git 来源以 ref 头 ≠ 锁定 commit 判定）。两侧同时偏离基线走冲突分支（update 拒绝，`--force` 覆盖）；两侧内容彼此相同而基线过期时，update 仅刷新基线、不改内容。

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
取锁 → 恢复未完成事务 → 加载配置、检查可写性 → sweep 外部修改 → 命令只读前置检查 → [多阶段命令：追加事务 revision] → 准备（staging 下载与校验）→ 落盘 store → commit → [追加事务终态标记] → reconcile 链接 → 释放锁
```

- **可写性检查先于 sweep，这一次序是硬性要求**：加载 `fu.yaml` 后立即检查 `version` 是否在本 fu 支持范围内（§3），检查置于 sweep 之前而非之后。sweep 本身是一次 commit——若排在可写性检查之前，一个因版本过高而被拒绝的写命令仍会先把 sweep 扫到的外部修改提交入库（且提交信息与真正的拒绝原因无关），版本护栏形同虚设；提前到 sweep 之前，被拒绝的命令不产生任何提交，store 保持用户离开时的原样；
- **命令前置检查先于 WAL**：会对当前状态作普通拒绝的只读条件必须在事务记录前完成；例如 `new` 先检查配置重名和固定 skills 根下的既有目标，避免在尚未产生任何命令内容时崩溃，却留下无法自动收敛的空摘要 WAL。进入 mutation 前再检一次相同条件，抵御非 fu 写入者在前置检查后竞态放入目标；
- **准备区**：`$FU_HOME/staging/`——与 store 同文件系统，落盘用平台原子且排他的 rename 完成（Linux `renameat2(RENAME_NOREPLACE)`，Darwin `renameatx_np(RENAME_EXCL)`），目标已存在时绝不替换，其他平台安全报不支持；同名准备条目只有在 WAL 与所有权清单同时匹配时才可由恢复处理，无匹配条目是安全冲突，`new` 既不递归清理也不覆盖；内容在此完成下载与规范校验（SPEC 规则 7）后才移入 store；
- **fu.yaml 写入**：临时文件 + fsync + rename，永不原地写；不对父目录 fsync，故只保证进程崩溃安全、不保证掉电安全（见附录"崩溃恢复"）。写入是条件安装：先在 `staging/` 下以随机 `.fu-config-candidate-*` 名和 `O_EXCL|O_NOFOLLOW` 排他创建候选，写入并 fsync 后取得其 identity；再把候选 identity、交换前 `fu.yaml` identity、预期旧字节摘要与候选字节摘要作为不可变 `.fu-config-exchange-*.json` 排他写入 `recovery/`，记录成功后才把候选排他 rename 成固定活动名 `.fu-config-swap`。因此记录落盘前的崩溃至多留下永不自动认领的随机候选，不会占住活动名；活动名已有外部条目时也绝不打开或复用（即使它为空——其 inode 仍可能通过硬链接属于外部路径）。随后活动名与 `store/fu.yaml` 做一次原子交换（Linux `renameat2(RENAME_EXCHANGE)`，Darwin `renameatx_np(RENAME_SWAP)`）。交换后被换出的旧对象即"文件在交换那一刻确实持有预期字节"的凭据：该名字在交换后只解析一次得到描述符，此后 identity 与内容都在该描述符上验证。凭据成立即完成安装，把活动名以排他 rename 移入 `recovery/` 下由设备号与 inode 派生的唯一 `.fu-config-archive-*` 终态名，并在归档名上复核 identity；旧 inode 的内容始终不修改，因为 store 外的硬链接或仍打开的描述符可能共享它。凭据不成立且 `fu.yaml` 仍是 fu 刚装入的对象时，把被换出的权威对象换回 `fu.yaml`，再以同一协议归档 fu 自己撤回的 inode；若此时 `fu.yaml` 已被第三方再次改变，则第三方版本原样保留、被换出的对象继续停在活动名，均不删除。交换而非"先移开再安装"，是因为后者存在 `fu.yaml` 不存在的瞬间，此刻崩溃会让 store 无法打开；交换在任何时刻（含崩溃）都留下两个完整版本之一。**残留与恢复**：统一写恢复入口先扫描未带匹配 `.done` 终态的 exchange 记录，并同时核对候选、活动名、`fu.yaml` 与两个 identity 派生归档位置；只有 identity 与摘要完整对应"尚未交换 / 已交换 / 已撤回 / 已归档"之一时才确定性归档或复原并写入绑定记录摘要的终态，故进程在 exchange syscall 后退出也无需人工清理。任一未知 identity、字节或位置组合均保留全部版本并安全冲突；没有匹配记录的活动名仍视为外部占用而拒绝接管。记录、终态与归档永久保留：POSIX 没有可移植的按 inode 条件 unlink，这是确保最终名字竞态不会删除外部内容的有意空间代价。所有这些条目均在版本控制之外，不会被 sweep 计入历史，也不会与 skill 内容混淆；
- **命令提交候选冻结**：初始 sweep 之后的命令只允许声明过的 Git 路径进入自身提交（`new` 为 `fu.yaml` 与 `skills/<name>/...`，开关命令仅为 `fu.yaml`）。落盘与 publish 完成后在私有内存 index 中只 stage 一次，冻结由路径、Git mode 与 blob hash 组成的完整树；冻结候选中的配置字节、事务所有权载荷与允许路径必须精确匹配，且 worktree 不得再有未进入候选的 tracked、ignored 或删除变化。随后 commit 直接从这组不可变条目递归构造 tree 与 commit 对象，最终校验后不再把公共 index 当作候选输入或扫描文件系统；当前分支只以捕获的旧引用做 CAS 更新。直接 Git 写入公共 index 的内容既不会进入 fu 的候选，也不会被候选放弃或发布后的条件同步覆盖；事务在 operation/compensation commit 前分别持久化候选树指纹，恢复识别提交时除 parent 与 message 外还必须匹配完整树。因此初始 sweep 后到达的外部修改要么使命令回滚并保留为下一次独立 external commit，要么在候选冻结后保持未提交，绝不会被归因到当前命令；
- **update 目录交换**：旧目录先改名保留（`<name>.old`），新内容就位并 commit 成功后清理；`.old` 是由命令级事务记录背书的载荷——恢复入口仅处理与记录匹配的 `.old`（新目录已 commit 则清理，否则改名还原），无匹配的 `.old` 按普通内容对待；
- **崩溃残留**：落盘后、commit 前崩溃会留下 untracked 内容。go-git 的 hard reset 与系统 `git reset --hard` 行为相反——**会**删除 untracked 内容，包括被 `.gitignore` 忽略的部分（系统 git 两者都不删；已用 go-git v5.16.0 直接验证）。`restore` 的设计建立在这条事实之上，顺序是硬性要求，不可颠倒：**必须先把 untracked 条目归档到 `$FU_HOME/recovery/<时间戳>/`，确认归档无误后才能执行 hard reset**——先 reset 会让待归档的内容在归档发生前就被删除。`Store.Revert`（`revert n` 的实现，§4）目前直接 hard reset、不做归档，是已知缺口而非已解决问题。**同一函数还有四处一并未关闭**（第十三轮补记，均为 `fu revert` / `restore` 落地前的前置条件）：（1）`Worktree.Reset` 无条件改写分支引用，直接抵消上一行刚做的 CAS——期间落地的直接 Git 写入会被无声吞掉，因此 §6 中"`Commit` 只能察觉、不能阻止并发的分支改写"一条标为已关闭时所依据的"已无无条件写引用的路径"并不完全成立，`Revert` 就是剩下的那一条；（2）不取 `.git/index.lock`，故其 index 刷新对受支持的直接 Git 写入者不是原子的；（3）跑在 `PlainOpen` 的工作区上，而不是写会话固定的那组描述符；（4）不调用 `checkNoAbsoluteSymlinks`。本计划未交付 `restore` 与 `fu revert` 的 CLI，`Revert` 目前只被测试与内部场景调用，风险处于潜伏状态，但以上五点必须在其落地前一并补上；
- **事务所有权与补偿恢复**：`new` 在排他创建 staging 根后、写入首个文件前立即持久化只含根的所有权清单；此后每个条目都以描述符相对、`O_EXCL|O_NOFOLLOW` 的方式创建，立即核对实际写入的 identity、模式与内容摘要，再逐项扩展权威清单。基线摘要只从这份权威清单推导，不从稍后枚举到的整棵活目录反向认领；每次推进与 publish 前后都要求现场全量扫描与预期集合精确相等，未知或被替换的后代一律保留 WAL 并安全冲突。记录包含设备/inode、类型、模式以及文件摘要或链接目标。事务路径存在而 WAL 无清单，或任一现场对象不再全量匹配，都必须安全冲突；运行中可见的回滚与崩溃后恢复共用这一条路径，不再按名盲目递归删除。若命令 commit 已写但 WAL 尚未清除，先全量重验已发布目录，再以排他 rename 移入 recovery，并对移动后的同一对象再验；只有所有权成立后才还原配置并写补偿 commit。POSIX 没有可移植的按 inode 条件删除，因此隔离载荷完成后不再执行“校验路径再 unlink”：它被排他 rename 到由原名与根 identity 派生的终态归档名，并在归档名上再次全量校验后保留。归档校验对现场与清单执行双向精确相等：清单记录的条目必须全部存在并逐项匹配，出现未知条目同样拒绝；缺失一项与被替换一项等价看待，都要保留 WAL 并安全冲突。清理本身只有一次 rename，不存在“清理到一半”的中间形态，因此不为旧格式保留宽松识别：`.fu-cleanup-*` 条目/根保留名从未被任何版本写出，本分支之前也没有发布过 WAL 格式，原先的兼容承诺随之撤销，旧工作区无需迁移。若日后确需兼容，应先为持久记录加上明确的 cleanup 版本，再按版本放宽，而不是让当前版本继续接受不完整载荷。原名与终态归档名两个根位置都不存在时，已失去“载荷仍被保留”的证据，必须保留 WAL 并安全冲突。终态归档也可在 WAL 清除前再次崩溃后幂等识别。任何竞态替换都被排他恢复或保留在归档名下，fu 不会自动删除它；
- **store 内不可读的文件会挡住每一条写命令**：入库必须读取 store 内的每一个文件，因此一个 `chmod 000` 的文件会让所有写命令在 sweep 阶段失败。这与系统 git 一致——`git status` 容忍不可读的未跟踪文件，`git add -A` 则以 "unable to index file" 失败——所以行为本身不可约减；已做的是把裸 errno 换成点名完整路径并给出处置办法的消息（`explainStagingFailure`）。判定"是否有变更"一侧不需要读权限，已修好（`statEntryNoFollow` 从分类用的 `fstatat` 直接构造 `FileInfo`）；
- **两处随 `add` / `adopt` 一并处理的前置项**（第十四轮记录，今日均无危害，但都会在 `DigestFS` 拿到第一个生产调用方那一轮开始有代价）：（1）`store.Config.Save` 仍是导出的、以替换式 rename 结尾的写入，没有条件安装，也没有说明何时不该用它——写流水线走的是 `SaveConfigExpecting`（条件安装，见上文 fu.yaml 写入一条），`Save` 目前只有 `Init` 的引导写入这一个调用方，但它就摆在同一个类型上，下一个写命令的作者会先看到它；（2）`skill.DigestFS`（走文件系统）与 `skill.DigestManifest`（走已授权清单）是同一份规范化投影的两个独立实现，没有任何测试断言二者一致——今日已核对在混合树上结果相同，但 `DigestFS` 本轮没有生产调用方，一旦 `add` / `adopt` 用它计算基线摘要而 `update` 用另一个复核，二者漂移就会表现为无法解释的"本地已修改"；
- **锁**：`$FU_HOME/fu.lock` 文件锁（flock），写命令互斥；只读命令不取锁，接受瞬时竞态；
- **逻辑根与对账固定**：`Store.Open` 先以 `O_DIRECTORY|O_NOFOLLOW` 打开 `FU_HOME`，再相对已固定父根打开 `store/`、`store/.git/` 与 `store/skills/`（必须已存在，否则报错——`list` / `show` 这类只读命令也走 `Open`，不应创建任何 store 内容），并打开或按需创建机器本地的 `staging/` 与 `recovery/`；布局、HEAD 和配置验证以及身份捕获均经同一组描述符完成，返回前再核对逻辑名仍指向这些身份。写会话以 `openat(O_DIRECTORY|O_NOFOLLOW)` 重新打开并核对所有身份，整个命令复用这些描述符；配置与 worktree 走固定的 store 根、对象与引用走固定的 Git 根、WAL/准备区/技能内容各走自己的根，跨根迁移使用两个固定目录间的原子排他 rename。公开 `Reconcile` 自行打开检查会话、取得同一 `fu.lock`、完成未决恢复并从固定 store 根重新加载配置，写流水线则复用已持锁会话的内部对账入口；所有 store 目标都通过固定 skills 根以 no-follow 语义确认为真实目录后才可建链；
- **特殊文件遍历拒绝与稳定读取**：提供给 go-git 的固定根目录适配器不以阻塞式只读 open 探测未知条目。每个目录项先经相对目录描述符的 `fstatat(AT_SYMLINK_NOFOLLOW)` 分类；FIFO、socket 与设备立即返回带路径的“不支持类型”错误。普通文件与目录随后也只用 `O_NONBLOCK|O_NOFOLLOW|O_CLOEXEC` 打开并以 `fstat` 重验类型和 identity，因此分类后发生的 FIFO 替换同样不能让持锁写命令挂起；go-git 未经枚举而直接打开的 index、引用与对象等控制文件也在只读 open 时强制 `O_NONBLOCK|O_NOFOLLOW`，并在每次读取后复核同一描述符的 identity、类型、大小、mtime 与 ctime。同一套 descriptor-relative 规则用于读取 `fu.yaml`、WAL revision / 终态标记和 OwnedTree 文件哈希；配置读取与序列化共用 8 MiB 上限，WAL 写入端与读取端共用单文件 16 MiB 上限，并在实际读取时再次限制长度以覆盖打开后增长。普通文件读取完成后再次对同一描述符 `fstat`，设备、inode、原始类型/模式、大小、实际字节数、mtime 与 ctime 必须与读取前一致，否则按对象已变更处理；错误返回后锁按统一 defer 路径释放；
- **`fu commit`**：同一流水线的特化——无准备阶段，即"定向 sweep"（可限定单 skill 路径、可带 `-m`）；
- 链接操作不可事务化，以 reconcile 幂等性弥补。

### adopt 流程（AdoptPlan）

**（设计，未实现）**：本轮不交付 `adopt`。

整个流程由一组命令级不可变事务 revision 护航（见 §2）：首条 revision 在任何 store / agent 变更之前排他落盘，阶段推进时追加，完成后追加终态标记。

**阶段一 · 入库（所有形态共同）**

1. **扫描与分类**：逐 agent 扫描 skills 目录，条目分五类——真实目录；指向外部的 symlink（只读其目标内容）；fu 链接（跳过）；适配器保留条目（排除，如 codex `.system`）；agent 目录本身为 symlink（只读扫描其目标，投放走整目录切换）。目标目录与外部内容在本阶段一概不被修改；
2. **去重、冲突与来源**：多 agent 出现同名条目，规范化投影摘要相同则合并为一，不同则报冲突、该项整体跳过；symlink 条目的原目标路径唯一时记为 local 来源，不一致则省略 `source` 并警告，不静默取其一；
3. **写入 store**：候选内容按规范化投影复制至 staging 并通过校验 → 移入 store、写 fu.yaml → commit。开关编码：`enabled=true`（全局开），对已检测但收编前未拥有该 skill 的 agent 写显式 false 覆盖（现状矩阵不变，未来新增 agent 默认获得）；`--agent` 限定收编来源时，仍只读盘点其他已检测 agent 写入覆盖，避免收尾 reconcile 意外投放；

**阶段二 · 投放切换（按 agent 形态）**

4. **逐项切换**（skills 目录为真实目录的 agent）：事务记录推进至切换阶段 → 原条目归档（复制 + fsync + 摘要校验通过后删除；recovery 与 agent 目录未必同盘，不可依赖 rename）→ 建 store 链接 → 记录推进；
5. **整目录切换**（skills 目录本身为 symlink 的 agent）：在 agent 目标旁构造隐藏 sibling 替代目录（同盘，rename 必原子；staging 与 agent 目录未必同盘，故不从 staging 直接 rename）——收编条目为 store 链接，未收编、冲突与非 skill 条目为指向原目标的透传链接（agent 视野不变，性质为未纳管）；旧父链接先 rename 为同目录 sibling 备份（同盘），recovery 只持久化其 readlink 值与事务元数据 → rename 替代目录就位 → 清理备份与标记；

**阶段三 · 恢复与清理**

6. 全部中断状态由统一恢复入口按事务记录收尾或回滚，收尾前以摘要等价核对现场（现场条目与 store 同名且投影摘要相等即完成切换，不等则安全冲突），终态为完成 / 回滚 / 安全冲突三态之一；事务 journal 是 recovery 下保留的本机操作证据，不是内容所有权清单；
7. **隔离**：任一 skill 失败不影响其他项，逐项报告结果；整目录形态下失败项以透传链接保持可见。

### 已知缺口（本轮未覆盖）

以下缺口本轮已确认存在、判断为可接受，留给后续计划处理，记录于此以免被误当作新发现：

- **SPEC 规则 7 的路径安全检查（symlink 逃逸等越界引用）未实现**。当前校验（`internal/skill/meta.go` 的 `Validate`）只覆盖规则 7 中 name / description 的合规性与目录名匹配，SKILL.md 存在性由 `ParseMeta` 另行把关；本轮没有 `add` / `adopt`，没有导入外部内容的入口，因而没有需要防范的场景，暂缓是对的——但实现 `add` / `adopt` 的计划必须补上这项检查，不能默认它已被覆盖；
- **对账的隔离粒度止于 agent**：`ScanAgent` 遇到单个条目出错（如 store 内自引用链接触发的 ELOOP）会放弃该 agent 剩余的全部条目，但不影响其他 agent；条目级隔离尚未实现；
- **只读命令报告期望而非现实**：`list` / `show` 直接读 `store.Config`，不比对磁盘上的实际链接状态；SPEC 规则 4 要求的"只读命令仅提示待投放"未实现；
- **cli 层绕过 engine 自行决策，范围不止读命令一侧**：`list` / `show` 绕过 engine、直接依赖 `store` / `agent` / `skill`，§1 "cli 不含决策逻辑"的纪律在读命令这一侧尚未落实，这只是已查明范围的一部分，不是全部——`init` 本身是写命令，却直接调用 `store.Home` / `store.Init`，既不经取锁，也不经 §6 的写操作流水线；`list` 与 `show` 还各自直接调用 `agent.Detected()` 自行决定纳管 agent 集合，而非经某个读侧概念取得；`openStore` / `openStoreAndConfig` 作为几乎每条命令共用的打开步骤，本身就直接调用 store 包完成打开与加载；`toggle` 命令中决定是否软化确认措辞的 `skillBlocked` 函数（连同其文档注释约七十行）依据 `engine.Result` 的具体字段作判断，是不折不扣的、§1 定义下禁止出现在 cli 层的决策逻辑。engine 尚无读侧 API 只是根因之一；规划补齐这一层时须按上述实际范围规划，而非只补一个读侧 API；还有三处此前未列入：**写命令同样在 cli 层选定纳管 agent 集合**（`internal/cli/new.go`、`internal/cli/toggle.go` 都调用 `agent.Detected()`），而 engine 的签名 `Run(st, agents, op)` 本身就要求调用方作此决定，因此这一条是结构性的，补一个只读 API 并不能关闭；**两处 cli 站点自行判断 `engine.ErrOperationFailed` 对持久化意味着什么**（`new.go`、`toggle.go` 各自据此决定仍然打印确认信息），这是把 engine 语义复制到了 cli；**`printResult` 决定 `Result.Foreign` 一律不呈现**（`internal/cli/root.go`），§2 记录了该行为，但策略本身住在 cli。另一个方向同样未达成，见 §7 的错误一条：核心包直接拼装面向 CLI 的文案，而 cli 不做组装。下一轮据此条估算工作量时，两个方向都要算上；
- ~~**`Commit` 只能察觉、不能阻止并发的分支改写**~~ **（已关闭）**：`Store.Commit` 已不再调用会重读可变 index、无条件写引用的 `Worktree.Commit`；它从冻结条目自行构造 tree / commit 对象，并以捕获的旧分支引用做 CAS。CAS 前落地但未发布的对象只是不可达对象，不会移动分支；CAS 后仍复核最终引用，以报告随后落地的直接 Git 写入；
- ~~**agent 目录的父路径替换竞态未关闭**~~ **（已关闭）**：原缺陷为 `ScanAgent` 以 `Lstat` 判定 skills 目录不是 symlink，而其后的建链、复核、删除仍按路径名操作，两者之间存在被替换的窗口。现在 `ScanAgent` 记下所检查目录的身份；执行前以 `O_DIRECTORY|O_NOFOLLOW` 打开最终分量并以 `os.SameFile` 比对描述符身份，所以不仅不同目录，连指回原 inode 的最终 symlink 也不可被采纳。其后的单分量操作全部使用 `*at` syscall 相对该描述符进行。残留仅剩同一描述符上复核与 `unlinkat` 之间的极窄间隙（§2）；
- **store 内绝对目标的 symlink 只能拒绝、不能如实记录**：go-git 的 worktree 跑在 go-billy 的 chroot 文件系统上，其 `Readlink` 把绝对目标改写为 `"/" + filepath.Rel(base, target)`。两个方向都坏——入库侧把改写后的文本当作 blob 记入历史（`git cat-file` 与磁盘 `readlink` 不符，`clone` 据此重建出错误链接，破坏 SPEC §9 的兜底与核心场景 4）；状态侧以同样方式读取，故 worktree 与自己刚提交的 blob 永远比较不等，`IsDirty` 恒真、`Sweep` 永不收敛，证伪 §4"sweep 使 worktree 常态干净"的前提。已确认元凶是 go-git 核心而非本项目的 `SkipStatus` 强制入库路径：裸 `AddWithOptions{All:true}` 产出同样的错误 blob，且即便用系统 `git add -A` 正确提交，go-git 的 `IsDirty` 仍返回 true。**本轮的处理是检测到即明确报错**（`store.ErrAbsoluteSymlink`，由 `Commit` / `IsDirty` / `Sweep` 共用的 `checkNoAbsoluteSymlinks` 在入库前拒绝，错误消息点名该条目），不是修复：如实记录需绕开 go-git 自行写 blob 与索引项**并**自行计算状态，超出本轮范围。相对目标的 symlink 不受影响（chroot 原样透传），照常入库且收敛。**第十三轮更正**：以上两句此前只在 `os.DirFS` 视图下验证过，而写命令实际走的是会话内的 `rootStandardFS`。该适配器当时只实现了 `ReadLink` 而无 `Lstat`，不满足 `fs.ReadLinkFS`，于是 `fs.ReadLink` 对 store 内**任何** symlink 都返回 `ErrInvalid`：绝对目标的具名拒绝根本无法触发，相对目标反而成为主要受害者——一条手工链接即让每条写命令在 sweep 阶段以 `invalid argument` 失败，而读命令照常，store 看起来是健康的。同时 `symlinkFileInfo.Size()` 恒为 0，与 go-git worktree noder 按 `NewHasher(BlobObject, size)` + 目标字节求哈希的方式矛盾，即使补上 `Lstat`，链接也会被 `Status` 永远判为已修改，`IsDirty` 恒真而 `Sweep` 不收敛。两处均已修复（`rootStandardFS.Lstat` 委托，`symlinkFileInfo` 取 `fstatat` 已得到的 `st_size` 与 mtime），并以会话内的提交与收敛用例固定。fu 自己从不在 store 内创建 symlink（链接是从 agent 目录指**向** store），故该形态只可能来自手工编辑（场景 7）或将来的 adopt。
- ~~**`store.Home()` 不做绝对化**~~ **（已关闭，第七轮）**：`Home()` 现对 `FU_HOME` 与 `HOME` 回退两条路径都要求绝对路径，非绝对即报错——归一（`filepath.Abs`）会让 store 的**身份**取决于 `fu init` 首次运行的目录，比明确报错更隐蔽。适配器一侧的同类缺陷已于第六轮由 `agent.homeDir()` 关闭。此条保留为记录，以免被误当作新发现。
- **框架**：cobra——子命令、`--help`、shell 补全免费；
- **输出**：人读为主，`fu list` 对齐表格呈现开关矩阵；`--json` 不入 v1；
- **错误**：sentinel error（如 `ErrStoreNotFound`、`ErrTxnConflict`、`ErrConcurrentStoreChange`、`ErrAbsoluteSymlink`）+ `%w` 包装。**"用户可读消息统一在 cli 层拼装"是设计意图，当前两个方向都未达成**（第十三轮）：核心包直接拼装面向 CLI 的文案（`internal/store/store.go` 的 "run `fu init` first"、`internal/store/config.go` 的 `git -C %s checkout -- fu.yaml`、`internal/engine/ops.go` 的 `fu new %s`），而 cli 层不做任何组装，`internal/cli/exitcode.go` 直接打印整条 `%w` 链，把 "check operation preconditions"、"execute mutation" 这类内部步骤名泄漏给用户。底层的 sentinel 与 `%w` 包装本身是对的，缺的只是 cli 侧的翻译；见 §6 已知缺口；
- **退出码**：0 成功、1 操作失败、2 用法错误——用法错误（参数数量不对、未知 flag）由 `UsageError` 类型标记，在 cobra 生成对应错误的原处构造（`SetFlagErrorFunc`、`Args` 校验器）；未知子命令的判定必须在 `root.Execute()` 返回之后，用 cobra 自身的只读 `Find` 复核——`help` / `completion` / `__complete` 等内建子命令要到 `Execute()` 内部才注册，提前判断会把它们也误判为未知命令；其余错误（含 `Reconcile` 因 `Result.Failed` 非空而返回的 `ErrOperationFailed`）一律计为操作失败。判定逻辑集中在 `internal/cli/exitcode.go` 的 `execute()`；
- store 未初始化时，除 `init` / `clone` 外的命令统一前置报错并提示。

## 8. 测试策略

| 层 | 手段 | 覆盖 |
|----|------|------|
| 单元 | engine.Diff 表驱动 | 断链 × 未纳管 × 新 agent 的状态组合穷举。全局 × 覆盖两维不在此层：`Diff` 收到的已是解析后的布尔值，该维度由 `Config.Effective` 的用例覆盖 |
| 单元 | fu.yaml 读写往返 | 含未知字段保留、同值归一 |
| 集成 | `t.TempDir()` 构造 FU_HOME 与假 agent 目录（覆盖 `HOME`） | 本轮六条命令的写操作流水线。"restore 收敛性"为**（设计，未实现）**：`restore` 未交付 |
| 集成 | 本地 bare repo 充当远端 | **（设计，未实现）**：本轮无 `Pull` / `Push` / `Fetch` / `Clone` / `Remote`，此行暂无覆盖 |
| 端到端 | 命令函数级联走查 | 目前覆盖场景 7、场景 2 与场景 5 的 store 侧，调用 engine API 而非 CLI；其余核心场景依赖未交付命令 |
| 端到端 | 编译后二进制冒烟（go build + exec） | 1–2 条主路径，防"函数级可用、装配层坏"的盲区 |
| 故障注入 | 关键中断点模拟崩溃（`os.Args[0]` 派生子进程 + `os.Exit`，不跑 defer/回滚/Close） | 已交付：`new` 的两个所有权窗口、事务记录各阶段切换点、恢复内部六个边界、配置交换四个崩溃点。**（设计，未实现）**：整目录切换与 update 的 `.old` 残留随对应命令一并延后 |

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
| 核心抽象 | 对账引擎（desired × actual → actions） | 与 Terraform plan/apply、Kubernetes reconcile 同构；status 即 plan，restore 即 apply；Diff 纯函数化支撑穷举测试 |
| 锁 | FU_HOME 下 flock，写互斥、读不锁 | 自用场景瞬时竞态代价为零，换取实现最简 |
| 前向兼容 | fu.yaml 带 version、未知字段保留；高版本拒绝写入 | 为 roadmap 功能（更多 agent、web GUI 等）留路 |
| 内容基线 | fu.yaml 记录安装摘要（digest） | sweep 使 worktree 常态干净，git status 无法承担"本地修改"判定 |
| 链接所有权 | readlink 规范化后与 `store/skills/<条目自身的名字>` 精确相等即 fu 所有；不设本机 manifest | manifest 是会漂移的第三份状态；判据是恒等而非包含——包含会把用户自建的、指向 store 内某 skill 的链接也算作 fu 所有（第六轮 Critical）；恒等比较仍在路径字符串层面，不是文件系统身份判定，覆盖已发生过的场景（$FU_HOME 祖先变为 symlink、用户自建的转接链、跨名与深层目标），不是全部可能的路径别名 |
| adopt | 三阶段 AdoptPlan：入库（全形态共同）→ 投放切换（逐项 / 整目录）→ 恢复清理 | store 事务与投放事务分离，整目录形态不再依赖未执行的入库步骤；拒绝的是**内容永久所有权清单**，保留的 append-only 事务 journal 只证明一次本机操作的阶段与终态 |
| 崩溃恢复 | 命令级不可变事务 revision 先于任何变更排他落盘（WAL 式），文件名提交本 revision 摘要且内容链接前一摘要，阶段只追加，完成标记绑定最新摘要 | 任意下一次写命令先验证完整连续链再到达定义终态；任何既有路径都不会被 WAL 更新覆盖或在完成时删除，`.old` 等残留仅在匹配记录时处理；保证覆盖进程崩溃，不覆盖掉电——原子写未对父目录 fsync，rename 能否在掉电后存活不受保证 |
| 认证范围 | SSH（ssh-agent）+ 公开 HTTPS | 私有 HTTPS 指引走 SSH；若认证或合并痛点超预期，切换系统 git 的影响面限于 internal/store 与 internal/source 两个封装点（退路） |
