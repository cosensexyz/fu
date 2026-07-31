# fu（符）v1 产品规格说明

- 状态：草案，待评审
- 日期：2026-07-31
- 读者：产品与研发
- 范围：本文档回答 Why 与 What；How 仅记录必要约束，实现方案由后续实现规划文档承担

## 1. 一句话定位

fu（符）是运行在本机的 agent skill 管理器：以一个 git 化的中央 store 收纳全部 skills，通过 symlink 向各 AI coding agent 投放。一处安装，处处生效；一键开关，随时回退。

## 2. Why：背景与问题

### 2.1 背景

SKILL.md 已成为跨 agent 的通用技能格式：Claude Code、Codex（2025 年 12 月起）等主流 agent 均原生支持，同一份 skill 目录无需修改即可被多个 agent 使用。但"格式通用"并未带来"管理统一"：每个 agent 仍各自维护自己的 skills 目录（`~/.claude/skills/`、`~/.codex/skills/`），skills 的获取、启停、更新、迁移完全依赖手工文件操作。

### 2.2 问题

按重要性排序：

1. **多 agent 重复管理**。同一 skill 需在每个 agent 目录各放一份，增删改要多处执行，久之内容不一致。
2. **开关不便**。skill 放入目录即常驻加载，占用上下文；想临时禁用只能移走文件，恢复靠记忆。
3. **来源与更新脱节**。社区 skill 靠手动 clone、复制安装，装完与上游断联；是否有新版本、如何升级全凭手工。
4. **迁移与回退困难**。换机器需逐目录重建；误删改无历史可回退。

### 2.3 为什么是现在

- 格式已收敛：SKILL.md 成为事实标准，管理工具无需做格式转换，实现成本大幅下降；
- 数量在增长：个人常用 skills 已达到需要工具管理的规模；
- 社区的临时做法（如 `ln -s ~/.claude/skills ~/.codex/skills` 整目录共享）验证了需求真实存在，但整目录共享无法提供按 agent 开关、版本锁定与历史回退。

### 2.4 目标用户

自用优先：使用多个 AI coding agent 的个人开发者，以作者本人的工作流为设计基准。不为团队协作与社区分发做设计妥协。

### 2.5 价值主张

| 痛点 | fu 的回答 |
|------|-----------|
| 多 agent 重复管理 | 一处安装，处处生效：store 保存唯一实体，symlink 投放到各 agent |
| 开关不便 | 全局与 agent 两级开关，一条命令 |
| 来源与更新脱节 | 记录来源与 commit 锁定，`update` / `outdated` 有据可依 |
| 迁移与回退困难 | store 即 git repo：操作即提交，`clone` 即恢复 |

## 3. 核心场景

v1 以下列七个场景全程可走通为完成标志：

1. **安装**：`fu add <git-url>` → 扫描出仓库内全部 skills → 交互选择 → 所有已纳管 agent 下次会话即可用。
2. **临时禁用**：某 skill 干扰当前任务 → `fu disable <name> --agent claude` → 新会话生效；用毕 `enable` 恢复。
3. **更新**：`fu outdated` 列出可更新项 → `fu update <name>` 升级并记录新 commit。
4. **换机**：新机器上 `fu clone <远端地址>` → 全部 skills 与开关状态按记录重建。
5. **误操作恢复**：未提交的手工损坏由 `fu status` 发现、`fu restore` 复原；已提交的误操作由 `fu log` 查看、`fu revert` 回退。
6. **存量迁入**：已有散装 skills 的老环境（真实目录或 symlink 结构均可）→ `fu adopt` 收编入 store，原地留下链接，开关保持收编前现状，agent 侧体验不变；既有 symlink 只复制目标内容，原目标仓库不受影响。
7. **自写 skill**：`fu new <name>` 在 store 中创建骨架，直接编辑，默认对所有 agent 生效；编辑完成后 `fu commit <name>` 主动记录，或留待自动入账。

## 4. 概念模型

| 概念 | 定义 |
|------|------|
| skill | 含 SKILL.md 的目录，跨 agent 通用标准格式，fu 不做格式转换 |
| store | 位于 `$FU_HOME/store/` 的 git repo（`FU_HOME` 默认 `~/.fu`）；skill 实体统一置于其 `skills/` 子目录，开关状态与来源锁定记录于 `fu.yaml`；`FU_HOME` 下 store 之外的内容为本机自用 |
| agent | 受支持的投放目标，v1 为 Claude Code 与 Codex；每个 agent 对应一个适配器，封装 skills 目录位置等差异 |
| 来源 | skill 的出处：git 仓库（记录 URL + ref + commit）或本地目录；是 `update` 的依据 |
| 开关 | 两级：全局开关为默认值，agent 级开关为覆盖；**agent 有显式设置时依其值，否则跟随全局** |

### 4.1 开关语义

全局开关是默认值，agent 级开关是对它的覆盖：

- **生效判定**：某 agent 存在显式覆盖时依其值，否则跟随全局；
- 安装后默认：全局开，无任何 agent 级覆盖（各 agent 均跟随全局）；
- 新 agent 被检测纳管时：无覆盖即跟随全局；
- 经 `adopt` 收编的 skill 例外：初始开关保持收编前现状（原本在哪些 agent 生效即保持哪些）；对未来新增的 agent 与 add 一致，默认获得；
- **覆盖的存在条件（同值归一）**：这是一条**关于写入时刻的规则**，不是对文件的恒定断言——agent 级开关写操作在写入时，若新值与当时的全局值相同，就不记覆盖（等价于回到跟随全局）。**归一只发生在 agent 级开关写操作时**，因此文件中可以长期存在与全局同值的覆盖，见下一条；
- 全局切换只影响跟随全局的 agent，不清除任何覆盖——即使切换后某条覆盖的值恰好与新全局值相同，它仍原样保留在 `fu.yaml` 中，直到下一次针对该 agent 的开关写操作才会被归一掉。因此 `fu.yaml` 可能记录一条与全局同值的冗余覆盖，`fu list` 的覆盖标记也可能因此出现在一个实际上正跟随全局值的 agent 上；这只是展示层面的冗余，生效矩阵本身的可判定性不受影响。

### 4.2 状态模型：期望与现实

开关状态分两层：

- **期望**：`fu.yaml` 记录每个 skill 的全局开关、已设置的 agent 级覆盖与来源锁定。期望由全局值与覆盖按 4.1 规则完全决定，完整存于 store，随版本历史与远端同步。
- **现实**：各 agent skills 目录下的 symlink——某 skill 对某 agent 实际生效，等价于指向 store 的链接存在。

`status` 报告期望与现实的偏差及未纳管内容；`restore` 按期望重建现实。`clone` 与 `revert` 能够恢复开关状态，正因期望完整存于 store 之内。远端配置不入 `fu.yaml`，直接使用 git 自身机制（`clone` 自动建立，`fu remote` 查看与设定）。

```
~/.fu/                            (FU_HOME，默认 ~/.fu)
├── store/                        (git repo，唯一实体，多机同步的范围)
│   ├── skills/
│   │   ├── pdf-tools/SKILL.md ...
│   │   └── code-review/SKILL.md ...
│   └── fu.yaml                   (开关状态、来源锁定)
└── ...                           (store 之外为本机自用文件，不参与版本控制与同步)

~/.claude/skills/pdf-tools  → symlink → ~/.fu/store/skills/pdf-tools
~/.codex/skills/pdf-tools   → symlink → ~/.fu/store/skills/pdf-tools
```

需要跨机器保持一致的信息一律入 store；只与本机相关的内容（缓存、本机配置等，具体由实现决定）留在 `FU_HOME` 下 store 之外，版本控制与 push / pull / clone 均不涉及。

## 5. 功能规范

### 5.1 CLI 命令面

命令动词分两个域：**store 层**（对象是整个仓库）严格沿用 git 动词；**skill 层**（对象是单个 skill）采用 git 与新一代包管理器（yarn、pnpm、cargo、uv）的共同惯例。

**安装与移除**

| 命令 | 说明 |
|------|------|
| `fu add <git-url>[@ref]` | 从 git 仓库安装；扫描仓库内全部含 SKILL.md 的目录，交互选择或 `--all` 全装；重名项拒绝（见规则 1） |
| `fu add <本地目录>` | 复制进 store 后纳管，来源记录为本地路径；扫描规则与 git 形态相同 |
| `fu adopt [--agent <a>]` | 接管既有环境：真实目录移入 store；symlink 条目（含 skills 目录本身为 symlink 的整目录形态）只读复制目标内容入 store，不移动、不修改目标；完成后原位换为指向 store 的链接。开关初始状态保持收编前现状 |
| `fu new <name>` | 在 store 中脚手架一个新 skill 并默认启用，面向自写 skill 场景 |
| `fu rm <name>` | 删除各 agent 目录的链接与 store 实体；git 历史可找回 |

**更新**

| 命令 | 说明 |
|------|------|
| `fu update [name]` | 按来源记录拉取新版本，省略 name 时更新全部可更新项；本地修改过的 skill 默认拒绝覆盖并提示差异，`--force` 强制 |
| `fu outdated` | 列出上游有新版本的 skills |

**开关**

| 命令 | 说明 |
|------|------|
| `fu enable <name>` / `fu disable <name>` | 全局级开关（默认值） |
| `fu enable <name> --agent claude\|codex` | 设置该 agent 的开关（disable 同理）；与全局不同时作为覆盖记录并优先于全局，相同则回到跟随全局 |

**查看与校验**

| 命令 | 说明 |
|------|------|
| `fu list` | 全部 skills × agent 的生效状态矩阵 |
| `fu show <name>` | 详情：来源、锁定版本、描述、各级开关状态 |
| `fu status` | 只读一致性检查：期望与现实的偏差、断链与未纳管条目、store 工作区状态、远端同步状态、未完成事务（如中断的 adopt） |
| `fu restore` | 双层修复：store 工作区复位到最近一次提交（撤销未提交的手工删改）；链接层按期望重建缺失、清理断链 |

**提交与历史**

| 命令 | 说明 |
|------|------|
| `fu commit [name] [-m <说明>]` | 主动提交工作区的手工修改；带 name 仅提交该 skill，省略则提交全部；`-m` 省略时自动生成描述 |
| `fu log` | 操作历史的友好视图 |
| `fu revert [n]` | 回退最近 n 次操作（默认 1），链接随之重建；本身也是一次可回退的操作 |

**store 与同步**

| 命令 | 说明 |
|------|------|
| `fu init` | 初始化空 store |
| `fu clone <url>` | 新机恢复：克隆远端 store 并按记录状态重建全部链接 |
| `fu remote [url]` | 无参数查看、带参数设定远端（单远端简化） |
| `fu push` / `fu pull` | 与远端同步；pull 仅做获取与快进合并，分支分歧时不做包装，提示用户以 git 自行处理（store 是标准 git repo） |

**agent**

| 命令 | 说明 |
|------|------|
| `fu agent` | 列出检测到的 agent（检测到即纳管）及各自的投放概况 |

### 5.2 GUI

v1 不实现 GUI，仅交付 CLI。本地 web GUI（`fu web`）列入 roadmap；架构须为其预留：业务逻辑全部位于核心库，任何 UI 层（含未来的 web）只做调用与呈现。

### 5.3 自动提交

每次改变 store 状态的操作（add、rm、adopt、new、update、enable、disable、revert）自动生成一次 commit，提交信息为可读的操作描述。上述操作以及 `push`、`pull` 执行前，若工作区存在未经 fu 的手工改动（如直接编辑 skill 内容），先将其单独提交为一笔"外部修改"，再执行本次操作——任何内容变化都进入历史，`push` 与换机恢复因此完整。主动、带语义的记录用 `fu commit`；自动入账是它的兜底。`restore` 使现实回归期望、不产生新历史；`pull` 的合并提交由 git 自身完成。`fu log` 与 `fu revert` 建立在此之上。

## 6. 行为规则

1. **重名冲突**：skill 名是唯一标识；按 Agent Skills 规范，SKILL.md frontmatter 的 name 必须与目录名一致，fu 以此名为准。`fu add` 遇同名即拒绝安装，提示先 `fu rm` 旧项；批量安装时重名项跳过并提示。fu 不提供改名能力——改名即修改 skill 内容，违反规则 5。
2. **非纳管条目**：agent 目录中不是 fu 创建的内容，fu 绝不触碰；`fu status` 将其列为"未纳管"供参考，`fu adopt` 是唯一收编途径。
3. **本地修改与更新**：`fu update` 检测该 skill 自安装以来是否被本地修改；有则拒绝覆盖并提示差异，`--force` 强制，被覆盖内容留存于 git 历史。
4. **agent 检测**：按特征路径探测（`~/.claude/`、`~/.codex/`），检测到即纳管，未检测到的 agent 不投放、不报错。新 agent 的首次投放由下一次任意写操作或 `restore` 完成，只读命令仅提示待投放。
5. **专有元数据透明传递**：如 Codex 的 `openai.yaml`，随 skill 目录整体投放，fu 不解析、不修改。
6. **断链与漂移**：链接指向缺失目标（如 store 实体被手工删除）、期望与现实不符等偏差，由 `fu status` 发现；`fu restore` 将 store 工作区复位到最近一次提交，并按期望重建、清理链接。
7. **安装校验**：add 与 adopt 时按 Agent Skills 规范校验：SKILL.md 存在；name 与 description 非空且长度合规（≤64 / ≤1024 字符）；name 仅含小写字母数字与连字符、不以连字符首尾、无连续连字符，且与目录名一致；skill 内无越界引用（symlink 逃逸等路径安全检查）。不合规拒绝并说明原因。
8. **生效时机**：各 agent 在会话启动时加载 skills，开关变更于下次新会话生效；fu 不干预运行中的进程，仅在 CLI 输出与 GUI 中如实提示。
9. **更新基准**：git 来源沿其跟踪 ref 判定与获取新版本，ref 缺省时在安装当时解析为默认分支并固定记录，不动态跟随远端变更；锁定到具体 tag 或 commit 的来源视为固定，不参与 `outdated`。本地目录来源以源路径内容相对**安装基线**的差异判定 `outdated`——store 侧相对基线的差异属"本地修改"（规则 3），二者不混同；local 来源仅在其路径存在的机器上可更新与判定，其他机器上 `status` 如实提示。
10. **agent 目录前置检查**：某 agent 的 skills 目录本身是 symlink 时，日常投放（reconcile）拒绝执行并提示，绝不写穿链接改动其目标；`fu adopt` 是唯一例外——它以只读方式扫描链接目标完成收编，随后归档链接条目本身（记录其指向，可完全还原）、原位创建真实目录并投放，目标目录自始至终不被修改。
11. **保留条目**：各适配器可声明保留条目（如 Codex 的 `.system`），fu 永不纳管、永不收编、不在未纳管清单中提示。

## 7. Non-goals

明确不做，并记录理由：

| 不做 | 理由 |
|------|------|
| project 级 skills | 项目内 skills 应随项目仓库做版本管理、与项目共享，fu 聚焦个人全局层 |
| marketplace 类发现与搜索 | v1 面向自用，来源由用户自行给出；列入 roadmap |
| 团队共享与权限 | 自用优先，不为协作做设计妥协 |
| skill 格式转换 | SKILL.md 已是跨 agent 通用标准，转换无必要 |
| skill 间依赖关系 | 现实需求未出现，避免引入包管理器最复杂的部分 |
| Claude Code plugins 体系 | plugin 自带的 skills 由 plugin 生态自治，fu 不介入 |
| Windows | v1 机制依赖 symlink，仅支持 macOS 与 Linux；copy 回退机制列入 roadmap |

## 8. Roadmap（非承诺）

- 本地 web GUI（`fu web`：按需本地服务、仅监听 `127.0.0.1`、前端内嵌二进制、与 CLI 功能对等）；
- 更多 agent 适配（Gemini CLI、OpenClaw 等）；
- 菜单栏常驻入口（轻量托盘：一步开关与唤起 web GUI）；
- copy 投放机制作为适配器级回退，随之支持 Windows；
- 源发现与 marketplace；
- 团队共享。

## 9. 实现约束

How 层面仅约定以下边界，具体方案由实现规划文档承担：

- 业务逻辑全部位于核心库，CLI 只做参数解析与呈现（为将来的 GUI 预留同一调用面）；
- store 目录布局与 `fu.yaml` 格式保持前向兼容；
- 只读命令（list、show、status、log、outdated、agent、无参数的 remote）不修改 store 内容与 agent 目录；status 的远端核对仅作网络查询，不落盘；
- 所有破坏性操作均可恢复：store 内容以 git 历史兜底，adopt / restore 触及的本机原有条目以 recovery 归档兜底；
- v1 平台：macOS 与 Linux。

## 10. 验收标准

- 第 3 节七个核心场景全程可走通（CLI）；
- 链接被手工破坏后，`fu status` 能准确报告，`fu restore` 能完全修复；
- 任何操作不改变非纳管内容；
- 空环境与存量环境（散装真实目录或 symlink 结构）均可顺利迁入。

## 附录 A：关键决策记录

| 决策点 | 选择 | 理由 |
|--------|------|------|
| 与旧原型的关系 | 全新开始 | 旧原型（Rust workspace + profiles 机制）仅作经验参考，不构成约束 |
| 生效机制 | symlink 投放 | 单一事实源、无内容漂移、git 回滚即时生效；copy 作为将来的适配器级回退 |
| git 能力边界 | 含远端同步 | 对应"迁移与回退"痛点；冲突处理直接交给 git，不做包装 |
| GUI | 移出 v1，列入 roadmap | 先交付 CLI 核心价值；此前定为与 CLI 对等、本地 web（`fu web`）承载，该形态作为 roadmap 方向保留 |
| 两级开关语义 | 全局为默认值，agent 级为覆盖 | 支持"全局关但个别 agent 开"的精细控制；覆盖按同值归一，无需清除类命令；取代旧原型的 profiles 机制 |
| skill 层动词 | add / rm | 与 git（`submodule add`、`git rm`）及新一代包管理器（yarn、pnpm、cargo、uv）一致 |
| store 层动词 | 严格 git 化 | init、clone、status、restore、commit、log、revert、push、pull、remote 与 git 动词语义对应，直觉可整体迁移（`fu revert` 以操作次数为参数，是对 git revert 的简化） |
| 重名与改名 | 重名即拒，不提供改名 | 规范要求 frontmatter name 与目录名一致，改名即改内容，违反"fu 不修改内容"原则 |
| adopt 范围 | 完整收编（含 symlink 环境） | 真实环境即 symlink 管理（整目录链接与逐项链接并存），adopt 必须安全处理：symlink 只复制目标、不移动；整目录 symlink 由 adopt 自动迁移（归档链接条目、原位建真实目录），日常 reconcile 仍拒绝写穿 |
