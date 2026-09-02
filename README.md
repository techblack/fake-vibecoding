# fake-vibecoding

一个独立的 Go 命令行程序，用来演示 Codex、Claude Code 和 OpenCode 风格的
vibe-coding 事件流。程序不需要安装这些工具，也不会调用模型或修改工作目录。

当前版本为 `v0.0.1`。Codex 交互界面按本机 `codex-cli 0.152.1` 的终端布局实现，
Claude Code 和 OpenCode 提供各自的启动卡片、输入提示和 transcript 风格。

为保证“绝不修改本地文件、绝不调用模型”，程序不会启动本机的 `codex`、`claude`
或 `opencode` 进程。真实客户端的 TUI 虽然可以通过进程透传复用，但无法同时证明其
插件、权限、网络和本地配置都不会产生副作用；因此 `--native`、`--passthrough` 和
`--real` 参数会直接拒绝启动。当前 TUI 和任务流均为本地模拟，不访问模型服务。

## 使用

```bash
go run . codex
# 然后在出现的 Codex 风格页面中输入任务，输入 /exit 退出

go run . --once --workdir .
go run . claude --iterations 3 "检查登录模块"
go run . opencode --json --seed 42 --error-rate 0.3 --retries 2
```

第一个位置参数可选 `codex`、`claude` 或 `opencode`，默认是 `codex`。

直接运行 `fake-vibecoding codex`（或 `go run . codex`）会打开 Codex 风格的终端页面：
标题卡片、模型/目录信息、`› Ask Codex to do anything` 输入区和 `? for shortcuts` 底栏。
输入一条任务后会持续、无限地模拟探索、读取、编辑、测试以及偶发的模型重试，不会自行
结束。按 `Esc` 或 `Ctrl-C` 中断当前任务后，可以输入下一条；空闲时输入 `/exit` 退出。

`claude` 和 `opencode` 也支持真实终端中的 raw TUI：Claude Code `2.1.198` 使用
欢迎卡片、Tips 分栏、`❯` 输入线和权限底栏；OpenCode `1.15.12` 使用居中 logo、
多行 composer、`tab agents`/`ctrl+p commands` 快捷键栏和状态栏。两者均使用各自的
工具 transcript。三种 agent 都支持 `--workdir` / `-C` 指定只读目录。

Codex TUI 在真实终端中使用 raw 键盘输入和 alternate screen；使用 `--no-alt-screen`
可保留滚动历史。方向键、退格、Ctrl-C/Ctrl-D 和 Enter 均可用。

带有任务参数或 `--prompt` 时使用自动/脚本模式，默认执行一轮；带有 `--json` 或显式
设置 `--iterations 0` 时会持续生成随机任务，按 `Ctrl-C` 停止；`--once` 等价于运行一轮。

每轮会随机生成任务和工具调用（读取文件、搜索、git 状态/diff、网页搜索、测试等），并从 `--workdir` 中
递归读取少量文本代码片段作为上下文。读取有文件数和字节数上限，自动跳过 `.git`、
`node_modules`、`vendor` 等目录，任何情况下都不会在工作目录创建或修改文件。

涉及修改的任务会出现 `apply_patch` 风格的虚拟 diff。diff 只渲染提议的 `+`/`-`
行和统计信息，明确标记为 `virtual, not applied`，不会写回文件；JSON 模式会将完整
的虚拟 diff 放在事件的 `diff` 字段中。

### 常用选项

| 选项 | 说明 |
| --- | --- |
| `--workdir`, `--work-dir`, `-C` | 指定只读工作目录 |
| `--iterations`, `--steps` | 轮数；`0`（默认）表示持续运行 |
| `--interval` | 事件间隔，如 `200ms` |
| `--seed` | 固定随机种子，便于复现 |
| `--error-rate` | 模拟模型超时概率（0 到 1） |
| `--retries`, `--max-retries` | 超时重试次数 |
| `--prompt` | 任务描述（也可以直接写在选项后） |
| `--json` | 每行输出一个 JSON 事件，适合脚本消费 |
| `--no-alt-screen` | 禁用 alternate screen，保留终端滚动历史 |
| `--snippet-bytes`, `--files` | 代码片段大小和文件数量上限 |

### 虚拟 diff

涉及修改的任务会随机调用 `apply_patch`。它只生成显示用的虚拟 diff：

```text
• Editing main.go
  └ main.go (+1 -1) [virtual, not applied]
    @@ -1 +1 @@
    -old simulated line
    +new simulated line
```

diff 不会写回磁盘。JSON 输出中的事件会包含 `diff.path`、`diff.hunk`、`diff.added` 和
`diff.removed` 字段，方便测试或前端渲染。

### CI、Release 和 Homebrew

- `.github/workflows/ci.yml`：格式检查、race test、vet、JSON 烟测和 Linux/macOS/Windows
  交叉编译。
- `.github/workflows/release.yml`：推送 `v*` 标签后使用 GoReleaser 创建多平台归档、校验和、
  GitHub Release，并更新 `techblack/homebrew-tap`。
- 发布前需要在本仓库配置 `HOMEBREW_TAP_GITHUB_TOKEN` secret（对 tap 仓库有写权限）。

安装发布版本：

```bash
brew tap techblack/tap
brew install fake-vibecoding
```

详细发布流程见 [`docs/releasing.md`](docs/releasing.md)。

## 构建与测试

```bash
go build -o fake-vibecoding .
go test ./...
```

程序使用 Go 标准库和跨平台终端库 `golang.org/x/term`，可在常见 Linux、macOS、Windows
和 FreeBSD 系统编译运行。
