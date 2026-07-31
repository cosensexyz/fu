# Fu 工程规范

AI 代码生成基线规范。专项规范见 `AGENT/skills/`，按任务类型自动加载。

**代码注释用英文。**

## AI 行为

- 必须采用 Red/Green TDD
- Worktree 必须创建在项目根目录下 `.worktrees/`，创建后复制 `.claude/settings.local.json` 到新 worktree
- AI 生成的临时文档（计划、设计等）不提交到仓库


