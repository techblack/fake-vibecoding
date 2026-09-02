package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	"golang.org/x/term"
)

const version = "0.0.1"
const codexVersion = "0.152.1"
const claudeVersion = "2.1.198"
const openCodeVersion = "1.15.12"

// Agent identifies the command-line presentation to simulate.
type Agent string

const (
	AgentCodex    Agent = "codex"
	AgentClaude   Agent = "claude"
	AgentOpenCode Agent = "opencode"
)

func (a Agent) valid() bool {
	return a == AgentCodex || a == AgentClaude || a == AgentOpenCode
}

// Config contains simulation settings. Iterations == 0 means run until Ctrl-C.
type Config struct {
	Agent        Agent
	Workdir      string
	Prompt       string
	Iterations   int
	Seed         int64
	Interval     time.Duration
	ErrorRate    float64
	Retries      int
	Timeout      time.Duration
	SnippetBytes int
	MaxFiles     int
	JSON         bool
	NoColor      bool
	Model        string
	Once         bool
	FullAuto     bool
	Interactive  bool
	NoAltScreen  bool
	ShowHelp     bool
	ShowVersion  bool
}

// Snippet is a bounded, text-only preview of a source file.
type Snippet struct {
	Path string `json:"path"`
	Text string `json:"text"`
}

// Event is the stable machine-readable representation of one simulation step.
type Event struct {
	At        time.Time    `json:"at"`
	Iteration int          `json:"iteration"`
	Type      string       `json:"type"`
	Name      string       `json:"name,omitempty"`
	Input     string       `json:"input,omitempty"`
	Detail    string       `json:"detail,omitempty"`
	Attempt   int          `json:"attempt,omitempty"`
	Snippet   *Snippet     `json:"snippet,omitempty"`
	Diff      *VirtualDiff `json:"diff,omitempty"`
}

// VirtualDiff describes a proposed change only; it is deliberately never applied.
type VirtualDiff struct {
	Path    string `json:"path"`
	Hunk    string `json:"hunk"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
}

var toolCatalog = []string{
	"read_file", "list_directory", "search", "inspect_symbols", "run_tests",
	"apply_patch", "git_diff", "git_status", "view_file", "web_search", "run_command",
}

var tasks = []string{
	"梳理一个小功能的实现路径",
	"检查最近改动并提出改进建议",
	"为现有代码补充一个单元测试",
	"寻找潜在的边界条件",
	"总结模块之间的调用关系",
}

var sourceExtensions = map[string]bool{
	".c": true, ".cc": true, ".cpp": true, ".cs": true, ".go": true,
	".h": true, ".hpp": true, ".java": true, ".js": true, ".jsx": true,
	".json": true, ".kt": true, ".md": true, ".php": true, ".py": true,
	".rb": true, ".rs": true, ".sh": true, ".sql": true, ".swift": true,
	".toml": true, ".ts": true, ".tsx": true, ".txt": true, ".xml": true,
	".yaml": true, ".yml": true, ".css": true, ".scss": true, ".html": true,
	".vue": true, ".svelte": true, ".dart": true, ".ex": true, ".exs": true,
	".lua": true, ".proto": true, ".zig": true,
}

var sourceNames = map[string]bool{
	"Dockerfile": true, "Makefile": true, "Justfile": true, "Gemfile": true,
	"go.mod": true, "go.sum": true,
}

// ParseConfig parses both a leading style name and familiar CLI flags.
func ParseConfig(args []string) (Config, error) {
	cfg := Config{
		Agent:        AgentCodex,
		Workdir:      ".",
		Iterations:   0,
		Seed:         time.Now().UnixNano(),
		Interval:     700 * time.Millisecond,
		ErrorRate:    0.12,
		Retries:      2,
		Timeout:      8 * time.Second,
		SnippetBytes: 1200,
		MaxFiles:     8,
		Model:        "gpt-5.6-sol xhigh",
	}
	styleSet := false
	styleAgent := cfg.Agent
	if len(args) > 0 {
		if a := Agent(strings.ToLower(args[0])); a.valid() {
			cfg.Agent, args = a, args[1:]
			styleAgent = a
			styleSet = true
		}
	}
	// Codex and OpenCode expose an explicit run/exec command; accepting it keeps
	// their usual invocation shape usable even though this simulator has one mode.
	if len(args) > 0 && (args[0] == "exec" || args[0] == "run") {
		args = args[1:]
	}
	fs := flag.NewFlagSet("fake-vibecoding", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var agent string
	var interval string
	fs.StringVar(&agent, "agent", string(cfg.Agent), "模拟风格: codex、claude 或 opencode")
	fs.StringVar(&cfg.Workdir, "workdir", cfg.Workdir, "只读工作目录")
	fs.StringVar(&cfg.Workdir, "work-dir", cfg.Workdir, "--workdir 的别名")
	fs.StringVar(&cfg.Workdir, "cd", cfg.Workdir, "兼容 Codex 的工作目录选项")
	fs.StringVar(&cfg.Workdir, "C", cfg.Workdir, "只读工作目录（--workdir 的简写）")
	fs.IntVar(&cfg.Iterations, "iterations", cfg.Iterations, "任务轮数，0 表示持续运行")
	fs.IntVar(&cfg.Iterations, "steps", cfg.Iterations, "--iterations 的别名")
	fs.Int64Var(&cfg.Seed, "seed", cfg.Seed, "随机种子")
	fs.StringVar(&interval, "interval", cfg.Interval.String(), "轮次间隔，例如 200ms")
	fs.Float64Var(&cfg.ErrorRate, "error-rate", cfg.ErrorRate, "模拟模型超时的概率（0 到 1）")
	fs.IntVar(&cfg.Retries, "retries", cfg.Retries, "超时后的重试次数")
	fs.IntVar(&cfg.Retries, "max-retries", cfg.Retries, "--retries 的别名")
	fs.DurationVar(&cfg.Timeout, "timeout", cfg.Timeout, "模拟的模型超时时间")
	fs.IntVar(&cfg.SnippetBytes, "snippet-bytes", cfg.SnippetBytes, "每个代码片段最多读取的字节数")
	fs.IntVar(&cfg.MaxFiles, "files", cfg.MaxFiles, "最多读取的代码文件数")
	fs.BoolVar(&cfg.JSON, "json", false, "每行输出一个 JSON 事件")
	fs.BoolVar(&cfg.NoColor, "no-color", false, "关闭终端颜色（当前为兼容选项）")
	fs.BoolVar(&cfg.NoAltScreen, "no-alt-screen", false, "在当前终端滚动区显示 TUI")
	fs.BoolVar(&cfg.Once, "once", false, "只运行一轮")
	var promptFlag string
	fs.StringVar(&promptFlag, "prompt", "", "任务描述（也可直接作为位置参数传入）")
	fs.StringVar(&cfg.Model, "model", cfg.Model, "显示用模型名称")
	fs.StringVar(&cfg.Model, "m", cfg.Model, "--model 的简写")
	fs.BoolVar(&cfg.FullAuto, "full-auto", false, "兼容 Codex 的自动模式选项")
	fs.BoolVar(&cfg.FullAuto, "dangerously-skip-permissions", false, "兼容 Claude Code 的选项")
	fs.BoolVar(&cfg.FullAuto, "yolo", false, "兼容 OpenCode 的自动模式选项")
	fs.BoolVar(&cfg.FullAuto, "dangerously-bypass-approvals-and-sandbox", false, "兼容 Codex 的自动模式选项")
	var printMode, continueMode, ossMode, searchMode bool
	var approvalMode, sandboxMode, profile string
	fs.StringVar(&approvalMode, "ask-for-approval", "", "兼容 Codex 的审批策略")
	fs.StringVar(&approvalMode, "a", "", "--ask-for-approval 的简写")
	fs.StringVar(&sandboxMode, "sandbox", "", "兼容 Codex 的沙箱策略")
	fs.StringVar(&sandboxMode, "s", "", "--sandbox 的简写")
	fs.BoolVar(&printMode, "print", false, "兼容 Claude Code 的非交互模式选项")
	fs.StringVar(&profile, "profile", "", "兼容 Codex 的配置 profile")
	fs.StringVar(&profile, "p", "", "--profile 的简写")
	fs.BoolVar(&continueMode, "continue", false, "兼容继续会话选项")
	fs.BoolVar(&ossMode, "oss", false, "兼容 Codex 的本地 provider 选项")
	fs.BoolVar(&searchMode, "search", false, "兼容 Codex 的联网搜索选项")
	var configOverride, enableFeature, disableFeature, remote, addDir string
	fs.StringVar(&configOverride, "config", "", "兼容 Codex 的配置覆盖")
	fs.StringVar(&enableFeature, "enable", "", "兼容 Codex 的 feature 选项")
	fs.StringVar(&disableFeature, "disable", "", "兼容 Codex 的 feature 选项")
	fs.StringVar(&remote, "remote", "", "兼容 Codex 的远程 app-server 选项")
	fs.StringVar(&addDir, "add-dir", "", "兼容 Codex 的附加目录选项")
	fs.BoolVar(&cfg.ShowVersion, "version", false, "显示版本")
	fs.BoolVar(&cfg.ShowVersion, "V", false, "显示版本")
	fs.BoolVar(&cfg.ShowHelp, "help", false, "显示帮助")
	fs.BoolVar(&cfg.ShowHelp, "h", false, "显示帮助")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if cfg.ShowVersion || cfg.ShowHelp {
		return cfg, flag.ErrHelp
	}
	cfg.Agent = Agent(strings.ToLower(agent))
	if !cfg.Agent.valid() {
		if styleSet {
			// Claude's --agent selects a named sub-agent; it does not change the
			// outer Claude presentation.
			cfg.Agent = styleAgent
		} else {
			cfg.Agent = AgentCodex
		}
	}
	if cfg.Iterations < 0 {
		return cfg, errors.New("iterations 不能小于 0")
	}
	if cfg.Retries < 0 {
		return cfg, errors.New("retries 不能小于 0")
	}
	if cfg.ErrorRate < 0 || cfg.ErrorRate > 1 {
		return cfg, errors.New("error-rate 必须在 0 到 1 之间")
	}
	if cfg.SnippetBytes < 1 || cfg.MaxFiles < 0 {
		return cfg, errors.New("snippet-bytes 必须大于 0，files 不能小于 0")
	}
	var err error
	cfg.Interval, err = time.ParseDuration(interval)
	if err != nil || cfg.Interval < 0 {
		return cfg, fmt.Errorf("无效 interval %q", interval)
	}
	if cfg.Timeout < 0 {
		return cfg, errors.New("timeout 不能小于 0")
	}
	if cfg.Once {
		cfg.Iterations = 1
	}
	iterationsFlagSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "iterations" || f.Name == "steps" {
			iterationsFlagSet = true
		}
	})
	cfg.Workdir, err = filepath.Abs(cfg.Workdir)
	if err != nil {
		return cfg, fmt.Errorf("解析 workdir: %w", err)
	}
	info, err := os.Stat(cfg.Workdir)
	if err != nil {
		return cfg, fmt.Errorf("访问 workdir %s: %w", cfg.Workdir, err)
	}
	if !info.IsDir() {
		return cfg, fmt.Errorf("workdir 不是目录: %s", cfg.Workdir)
	}
	positionalPrompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if promptFlag != "" && positionalPrompt != "" {
		cfg.Prompt = strings.TrimSpace(promptFlag + " " + positionalPrompt)
	} else if promptFlag != "" {
		cfg.Prompt = strings.TrimSpace(promptFlag)
	} else if positionalPrompt != "" {
		cfg.Prompt = positionalPrompt
	}
	if cfg.Prompt != "" && cfg.Iterations == 0 && !iterationsFlagSet {
		cfg.Iterations = 1
	}
	// With no task or machine-readable mode, behave like the interactive CLI.
	cfg.Interactive = cfg.Prompt == "" && !cfg.JSON && cfg.Iterations == 0
	return cfg, nil
}

func printUsage() {
	fmt.Printf(`fake-vibecoding %s

用法:
  fake-vibecoding [codex|claude|opencode] [选项] [任务描述]

选项:
  --workdir, -C DIR       只读工作目录（默认当前目录）
  --cd DIR                --workdir 的 Codex 兼容别名
  --iterations, --steps N 任务轮数，0 表示持续运行
  --interval DURATION      事件间隔（默认 700ms）
  --seed N                 随机种子
  --error-rate P           模型超时概率，范围 0 到 1
  --retries N              超时重试次数
  --timeout DURATION       模拟超时时间
  --json                   每行输出一个 JSON 事件
  --snippet-bytes N        单个代码片段上限
  --files N                最多读取的代码文件数
  --once                   只运行一轮
  --model NAME             显示用模型名称
  --no-alt-screen          保留终端滚动历史
  --version                显示版本
  --help, -h               显示帮助
`, version)
}

func executableAgent(name string) Agent {
	name = strings.ToLower(filepath.Base(name))
	name = strings.TrimSuffix(name, ".exe")
	for _, a := range []Agent{AgentCodex, AgentClaude, AgentOpenCode} {
		if name == string(a) {
			return a
		}
	}
	return ""
}

// LoadSnippets recursively reads a bounded set of source previews. It never writes.
func LoadSnippets(root string, maxFiles, maxBytes int) ([]Snippet, error) {
	if maxFiles < 0 {
		return nil, errors.New("files 不能小于 0")
	}
	if maxBytes < 1 {
		return nil, errors.New("snippet-bytes 必须大于 0")
	}
	if maxFiles == 0 {
		return nil, nil
	}
	var out []Snippet
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if len(out) >= maxFiles {
			return filepath.SkipAll
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", ".hg", ".svn", "node_modules", "vendor", "dist", "build", "target":
				if path != root {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !entry.Type().IsRegular() || (!sourceExtensions[strings.ToLower(filepath.Ext(entry.Name()))] && !sourceNames[entry.Name()]) {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil // an unreadable file should not stop a simulation
		}
		data, readErr := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
		_ = f.Close()
		if readErr != nil || len(data) == 0 {
			return nil
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		if len(data) > maxBytes {
			data = data[:maxBytes]
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		out = append(out, Snippet{Path: filepath.ToSlash(rel), Text: text})
		return nil
	})
	return out, err
}

// Simulator emits random task/tool activity until the configured number of rounds.
type Simulator struct {
	cfg      Config
	rng      *rand.Rand
	snippets []Snippet
	now      func() time.Time
}

func NewSimulator(cfg Config, snippets []Snippet) *Simulator {
	return &Simulator{cfg: cfg, rng: rand.New(rand.NewSource(cfg.Seed)), snippets: snippets, now: time.Now}
}

func (s *Simulator) event(iteration int, typ, name, input, detail string, attempt int, snippet *Snippet) Event {
	return Event{At: s.now().UTC(), Iteration: iteration, Type: typ, Name: name, Input: input, Detail: detail, Attempt: attempt, Snippet: snippet}
}

func (s *Simulator) emit(ctx context.Context, emit func(Event) error, event Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := emit(event); err != nil {
		return err
	}
	if s.cfg.Interval == 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(s.cfg.Interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Simulator) Run(ctx context.Context, emit func(Event) error) error {
	for iteration := 1; s.cfg.Iterations == 0 || iteration <= s.cfg.Iterations; iteration++ {
		task := s.cfg.Prompt
		if task == "" {
			task = tasks[s.rng.Intn(len(tasks))]
		}
		if iteration == 1 || s.cfg.Prompt == "" {
			if err := s.emit(ctx, emit, s.event(iteration, "task", "", "", task, 0, nil)); err != nil {
				return err
			}
		}
		sequence := s.toolSequence(task)
		for _, name := range sequence {
			input := "."
			if name == "run_command" && strings.HasPrefix(strings.TrimSpace(task), "!") {
				input = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(task), "!"))
			}
			var snippet *Snippet
			if len(s.snippets) > 0 && (name == "read_file" || name == "inspect_symbols" || name == "apply_patch") {
				snippet = s.chooseSnippet(task)
				input = snippet.Path
			}
			if err := s.emit(ctx, emit, s.event(iteration, "tool", name, input, "调用工具", 0, snippet)); err != nil {
				return err
			}
			if s.rng.Float64() < s.cfg.ErrorRate {
				resolved := false
				for attempt := 1; attempt <= s.cfg.Retries+1; attempt++ {
					detail := fmt.Sprintf("模型超时（%s）", s.cfg.Timeout)
					if err := s.emit(ctx, emit, s.event(iteration, "error", "model_timeout", "", detail, attempt, nil)); err != nil {
						return err
					}
					if attempt <= s.cfg.Retries {
						if err := s.emit(ctx, emit, s.event(iteration, "retry", "model", "", "重试请求", attempt, nil)); err != nil {
							return err
						}
						if s.rng.Float64() >= s.cfg.ErrorRate {
							resolved = true
							break
						}
					}
				}
				if !resolved {
					continue
				}
			}
			if name == "apply_patch" {
				diff := s.fakeDiff(snippet)
				event := s.event(iteration, "diff", name, input, "虚拟 diff（未应用）", 0, snippet)
				event.Diff = &diff
				if err := s.emit(ctx, emit, event); err != nil {
					return err
				}
			}
			if err := s.emit(ctx, emit, s.event(iteration, "output", name, "", "工具返回模拟结果", 0, snippet)); err != nil {
				return err
			}
		}
		if s.cfg.Iterations != 0 || s.cfg.Prompt == "" {
			if err := s.emit(ctx, emit, s.event(iteration, "done", "", "", task, 0, nil)); err != nil {
				return err
			}
		}
	}
	return nil
}

func isChangeRequest(prompt string) bool {
	lower := strings.ToLower(prompt)
	for _, phrase := range []string{"不要修改", "不修改", "无需修改", "只读", "do not modify", "don't modify", "read-only"} {
		if strings.Contains(lower, phrase) {
			return false
		}
	}
	for _, word := range []string{"实现", "修改", "修复", "更新", "新增", "添加", "删除", "完成", "补充", "implement", "fix", "change", "update", "add", "remove"} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return false
}

func (s *Simulator) chooseSnippet(prompt string) *Snippet {
	lower := strings.ToLower(prompt)
	for i := range s.snippets {
		path := strings.ToLower(s.snippets[i].Path)
		if strings.Contains(lower, path) || strings.Contains(lower, strings.ToLower(filepath.Base(path))) {
			return &s.snippets[i]
		}
	}
	return &s.snippets[s.rng.Intn(len(s.snippets))]
}

func (s *Simulator) toolSequence(prompt string) []string {
	if strings.HasPrefix(strings.TrimSpace(prompt), "!") {
		return []string{"run_command"}
	}
	if isChangeRequest(prompt) {
		pool := readOnlyTools()
		count := 3 + s.rng.Intn(4)
		sequence := s.randomTools(pool, count)
		sequence = append(sequence, "apply_patch", "run_tests")
		return sequence
	}
	lower := strings.ToLower(prompt)
	for _, snippet := range s.snippets {
		if strings.Contains(lower, strings.ToLower(snippet.Path)) || strings.Contains(lower, strings.ToLower(filepath.Base(snippet.Path))) {
			return []string{"read_file"}
		}
	}
	pool := readOnlyTools()
	return s.randomTools(pool, 2+s.rng.Intn(4))
}

func readOnlyTools() []string {
	tools := make([]string, 0, len(toolCatalog)-1)
	for _, tool := range toolCatalog {
		if tool != "apply_patch" {
			tools = append(tools, tool)
		}
	}
	return tools
}

func (s *Simulator) randomTools(pool []string, count int) []string {
	if count >= len(pool) {
		return append([]string(nil), pool...)
	}
	perm := s.rng.Perm(len(pool))
	sequence := make([]string, 0, count)
	for _, index := range perm[:count] {
		sequence = append(sequence, pool[index])
	}
	return sequence
}

func (s *Simulator) fakeDiff(snippet *Snippet) VirtualDiff {
	path := "src/example.go"
	before := "return value"
	if snippet != nil {
		path = snippet.Path
		if line := firstLine(snippet.Text); line != "" {
			before = line
		}
	}
	after := before + " // simulated"
	return VirtualDiff{
		Path:    path,
		Hunk:    fmt.Sprintf("@@ -1 +1 @@\n-%s\n+%s", before, after),
		Added:   1,
		Removed: 1,
	}
}

type renderer struct {
	agent Agent
	json  bool
	out   io.Writer
}

func (r renderer) interactiveHeader(cfg Config) error {
	if r.agent == AgentCodex {
		for _, line := range codexCard(cfg.Model, displayDirectory(cfg.Workdir), 80) {
			if _, err := fmt.Fprintln(r.out, line); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintf(r.out, "  Tip: New Build faster with Codex.\n\n› Ask Codex to do anything\n\n  ? for shortcuts\n  %s · %s\n", cfg.Model, displayDirectory(cfg.Workdir))
		return err
	}
	if r.agent == AgentClaude || r.agent == AgentOpenCode {
		lines, _ := otherBase(r.agent, cfg.Model, displayDirectory(cfg.Workdir), 80)
		for _, line := range lines {
			if _, err := fmt.Fprintln(r.out, line); err != nil {
				return err
			}
		}
		return nil
	}
	label := map[Agent]string{AgentCodex: "Codex", AgentClaude: "Claude Code", AgentOpenCode: "OpenCode"}[r.agent]
	_, err := fmt.Fprintf(r.out, "╭────────────────────────────────────────────────────────────╮\n│ fake-vibecoding · %-40s │\n│ 工作目录: %-47s │\n│ 这是只读模拟环境，不会修改文件。                         │\n╰────────────────────────────────────────────────────────────╯\n\n", label, shortenPath(cfg.Workdir, 47))
	return err
}

func shortenPath(path string, limit int) string {
	runes := []rune(path)
	if len(runes) <= limit {
		return path
	}
	if limit < 4 {
		return string(runes[:limit])
	}
	return "..." + string(runes[len(runes)-(limit-3):])
}

func runInteractive(ctx context.Context, cfg Config, snippets []Snippet, r renderer) error {
	if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
		return runCodexTUI(ctx, cfg, snippets, r.out)
	}
	return runInteractiveLines(ctx, cfg, snippets, r)
}

func runInteractiveLines(ctx context.Context, cfg Config, snippets []Snippet, r renderer) error {
	if err := r.interactiveHeader(cfg); err != nil {
		return err
	}
	if r.agent != AgentCodex {
		fmt.Fprintln(r.out, "输入任务开始模拟，输入 /help 查看命令，输入 /exit 退出。")
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	sim := NewSimulator(cfg, snippets)
	for {
		if ctx.Err() != nil {
			return nil
		}
		if r.agent == AgentCodex {
			fmt.Fprint(r.out, "\n› Ask Codex to do anything\n")
		} else {
			fmt.Fprint(r.out, "\n› ")
		}
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return err
			}
			fmt.Fprintln(r.out)
			return nil
		}
		fmt.Fprintln(r.out)
		input := strings.TrimSpace(scanner.Text())
		switch strings.ToLower(input) {
		case "":
			continue
		case "/exit", "/quit", "exit", "quit":
			fmt.Fprintln(r.out, "\n已退出 fake-vibecoding。")
			return nil
		case "/help", "help":
			fmt.Fprintln(r.out, "\n/help 查看帮助，/clear 清屏，/exit 退出。输入普通文字即可开始模拟。")
			continue
		case "/clear":
			if err := r.interactiveHeader(cfg); err != nil {
				return err
			}
			continue
		}
		sim.cfg.Prompt = input
		sim.cfg.Iterations = 1
		if err := sim.Run(ctx, r.event); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
	}
}

type codexTUI struct {
	cfg       Config
	snippets  []Snippet
	out       io.Writer
	history   []string
	input     []rune
	commands  []string
	commandAt int
	cursor    int
	status    string
	spinner   int
	startedAt time.Time
	activeAt  int
	active    string
	activeArg string
	width     int
	height    int
	escape    byte
}

func runCodexTUI(ctx context.Context, cfg Config, snippets []Snippet, out io.Writer) error {
	tui := &codexTUI{cfg: cfg, snippets: snippets, out: out, width: 80, height: 24, activeAt: -1}
	if width, height, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		tui.width, tui.height = width, height
	}
	if !cfg.NoAltScreen {
		fmt.Fprint(out, "\033[?1049h\033[H\033[2J")
		defer fmt.Fprint(out, "\033[?1049l")
	}
	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(os.Stdin.Fd()), state)

	// Codex first paints a loading card, then replaces it with model and directory.
	if cfg.Agent == AgentCodex {
		if err := tui.render(true); err != nil {
			return err
		}
		time.Sleep(80 * time.Millisecond)
	}
	if err := tui.render(false); err != nil {
		return err
	}
	return tui.inputLoop(ctx)
}

func (t *codexTUI) render(loading bool) error {
	if width, height, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		t.width, t.height = width, height
	}
	if t.cfg.Agent != "" && t.cfg.Agent != AgentCodex {
		return t.renderOther(loading)
	}
	if t.width < 8 {
		t.width = 8
	}
	if t.height < 5 {
		t.height = 5
	}
	var model, directory string
	if loading {
		model, directory = "loading", "loading"
	} else {
		model, directory = t.cfg.Model, displayDirectory(t.cfg.Workdir)
	}
	cardLines := codexCard(model, directory, t.width)
	cardLen := len(cardLines)
	lines := append([]string(nil), cardLines...)
	if !loading && len(t.history) == 0 {
		lines = append(lines, "  Tip: New Build faster with Codex.", "")
	}
	for _, item := range t.history {
		lines = append(lines, wrapTranscriptLine(item, t.width)...)
	}
	if t.status != "" {
		lines = append(lines, shortenDisplay(t.statusLine(), t.width))
	}
	placeholder := string(t.input)
	inputEmpty := placeholder == ""
	if placeholder == "" {
		placeholder = "Ask Codex to do anything"
	}
	lines = append(lines, "")
	promptStart := len(lines)
	promptLines := wrapWithPrefixes("› ", placeholder, "  ", t.width)
	lines = append(lines, promptLines...)
	lines = append(lines, "")
	if loading {
		lines = append(lines, "  ? for shortcuts")
	} else {
		lines = append(lines, shortenDisplay("  "+t.cfg.Model+" · "+displayDirectory(t.cfg.Workdir), t.width))
	}
	maxLines := t.height
	dropped := 0
	if len(lines) > maxLines {
		dropped = len(lines) - maxLines
		lines = lines[dropped:]
	}
	var frame strings.Builder
	frame.WriteString("\033[?2026h\033[H\033[2J")
	for index, line := range lines {
		line = t.styleLine(line, index+dropped, cardLen)
		frame.WriteString(line)
		// term.MakeRaw disables OPOST, so LF alone does not return to column 1.
		frame.WriteString("\r\n")
	}
	// Keep the cursor in the composer instead of at the end of the footer.
	cursorRow, column := composerCursor(string(t.input[:t.cursor]), t.width)
	promptIndex := promptStart + cursorRow - dropped
	if promptIndex < 0 {
		promptIndex = 0
	}
	if inputEmpty {
		column = 3
	}
	fmt.Fprintf(&frame, "\033[%d;%dH\033[?2026l", promptIndex+1, column)
	_, err := io.WriteString(t.out, frame.String())
	return err
}

func wrapTranscriptLine(line string, width int) []string {
	if line == "" || displayWidth(line) <= width {
		return []string{line}
	}
	for _, format := range []struct{ prefix, continuation string }{
		{"  └ ", "    "},
		{"  - ", "    "},
		{"› ", "  "},
		{"• ", "  "},
		{"◦ ", "  "},
		{"■ ", "  "},
	} {
		if strings.HasPrefix(line, format.prefix) {
			return wrapWithPrefixes(format.prefix, line[len(format.prefix):], format.continuation, width)
		}
	}
	return wrapWithPrefixes("", line, "", width)
}

func wrapWithPrefixes(prefix, text, continuation string, width int) []string {
	runes := []rune(text)
	currentPrefix := prefix
	var lines []string
	for len(runes) > 0 {
		limit := width - displayWidth(currentPrefix)
		if limit < 1 {
			limit = 1
		}
		count, used := 0, 0
		for count < len(runes) {
			runeWidth := displayWidth(string(runes[count]))
			if count > 0 && used+runeWidth > limit {
				break
			}
			used += runeWidth
			count++
			if used >= limit {
				break
			}
		}
		lines = append(lines, currentPrefix+string(runes[:count]))
		runes = runes[count:]
		currentPrefix = continuation
	}
	if len(lines) == 0 {
		return []string{prefix}
	}
	return lines
}

func composerCursor(text string, width int) (row, column int) {
	lines := wrapWithPrefixes("› ", text, "  ", width)
	row = len(lines) - 1
	column = displayWidth(lines[row]) + 1
	if column > width {
		row++
		column = 3
	}
	return row, column
}

func (t *codexTUI) statusLine() string {
	marker := "•"
	if t.spinner%2 == 1 {
		marker = "◦"
	}
	elapsed := 0
	if !t.startedAt.IsZero() {
		elapsed = int(time.Since(t.startedAt).Seconds())
	}
	return fmt.Sprintf("%s %s (%ds • esc to interrupt)", marker, t.status, elapsed)
}

func (t *codexTUI) renderOther(loading bool) error {
	if t.width < 8 {
		t.width = 8
	}
	if t.height < 5 {
		t.height = 5
	}
	if loading {
		loading = false
	}
	base, historyAt := otherBase(t.cfg.Agent, t.cfg.Model, displayDirectory(t.cfg.Workdir), t.width)
	cardLen := historyAt
	lines := append([]string(nil), base[:historyAt]...)
	for _, item := range t.history {
		lines = append(lines, wrapTranscriptLine(item, t.width)...)
	}
	if t.status != "" {
		lines = append(lines, shortenDisplay(t.statusLine(), t.width))
	}
	lines = append(lines, base[historyAt:]...)
	placeholder := string(t.input)
	inputEmpty := placeholder == ""
	if inputEmpty {
		if t.cfg.Agent != AgentClaude {
			placeholder = "Ask anything... \"What is the tech stack of this project?\""
		}
	}
	promptPrefix := "› "
	if t.cfg.Agent == AgentClaude {
		promptPrefix = "❯ "
	} else {
		promptPrefix = "   ┃  "
	}
	for index, line := range lines {
		switch {
		case t.cfg.Agent == AgentClaude && strings.HasPrefix(line, "❯ "):
			lines[index] = promptPrefix + placeholder
		case t.cfg.Agent == AgentOpenCode && strings.HasPrefix(line, "   ┃  Ask anything"):
			lines[index] = promptPrefix + placeholder
		}
	}
	promptStart := len(lines) - 4
	for index, line := range lines {
		if strings.HasPrefix(line, promptPrefix) {
			promptStart = index
			break
		}
	}
	dropped := 0
	if len(lines) > t.height {
		dropped = len(lines) - t.height
		lines = lines[dropped:]
	}
	var frame strings.Builder
	frame.WriteString("\033[?2026h\033[H\033[2J")
	for index, line := range lines {
		line = t.styleOtherLine(line, index+dropped, cardLen)
		frame.WriteString(line)
		frame.WriteString("\r\n")
	}
	promptRow, column := composerCursorAt(promptStart, promptPrefix, string(t.input), t.width)
	promptRow -= dropped
	if promptRow < 0 {
		promptRow = 0
	}
	if inputEmpty {
		column = displayWidth(promptPrefix) + 1
	}
	fmt.Fprintf(&frame, "\033[%d;%dH\033[?2026l", promptRow+1, column)
	_, err := io.WriteString(t.out, frame.String())
	return err
}

func otherBase(agent Agent, model, directory string, width int) ([]string, int) {
	if agent == AgentClaude {
		model = claudeModel(model)
		card := claudeCard(model, directory, width)
		tail := []string{
			"",
			strings.Repeat("─", maxInt(width, 1)),
			"❯ ",
			strings.Repeat("─", maxInt(width, 1)),
			"  ⏵⏵  don't ask on (shift+tab to cycle) · ← for agents",
			"  ◉ xhigh · /effort",
		}
		return append(card, tail...), len(card)
	}
	logo := []string{
		"", "", "", "", "",
		centerLine("⠀                                ▄", width),
		centerLine("█▀▀█ █▀▀█ █▀▀█ █▀▀▄ █▀▀▀ █▀▀█ █▀▀█ █▀▀█", width),
		centerLine("█  █ █  █ █▀▀▀ █  █ █    █  █ █  █ █▀▀▀", width),
		centerLine("▀▀▀▀ █▀▀▀ ▀▀▀▀ ▀  ▀ ▀▀▀▀ ▀▀▀▀ ▀▀▀▀ ▀▀▀▀", width),
		"", "",
	}
	shortcut := " tab agents     ctrl+p commands"
	horizontal := maxInt(width-4, 1)
	tail := []string{
		"   ┃",
		"   ┃  Ask anything... \"What is the tech stack of this project?\"",
		"   ┃",
		"   ┃  Build · GPT OpenAI",
		"   ╹" + strings.Repeat("━", horizontal),
		strings.Repeat(" ", maxInt(width-displayWidth(shortcut), 0)) + shortcut,
		"", "", "", "",
		openCodeStatusLine(directory, width),
		"  main",
	}
	return append(logo, tail...), len(logo)
}

func openCodeStatusLine(directory string, width int) string {
	right := "  " + openCodeVersion
	left := shortenDisplay("  "+directory+":main    ⊙ 0 MCP    /status", maxInt(width-displayWidth(right), 1))
	return left + strings.Repeat(" ", maxInt(width-displayWidth(left)-displayWidth(right), 0)) + right
}

func claudeModel(model string) string {
	if strings.HasPrefix(strings.ToLower(model), "gpt-") || model == "" {
		return "opus"
	}
	return model
}

func claudeCard(model, directory string, width int) []string {
	inner := maxInt(width, 40)
	if inner > 80 {
		inner = 80
	}
	leftWidth, rightWidth := 51, inner-54
	if rightWidth < 12 {
		rightWidth = 12
		leftWidth = inner - rightWidth - 4
	}
	row := func(left, right string) string {
		left = shortenDisplay(left, leftWidth)
		right = shortenDisplay(right, rightWidth)
		return "│" + left + strings.Repeat(" ", leftWidth-displayWidth(left)) + "│" + right + strings.Repeat(" ", rightWidth-displayWidth(right)) + "│"
	}
	top := "╭─── Claude Code v" + claudeVersion + " "
	top = shortenDisplay(top, inner-2)
	top = "╭" + strings.TrimPrefix(top, "╭") + strings.Repeat("─", inner-displayWidth(top)-1) + "╮"
	return []string{
		top,
		row("", "Tips for getting started"),
		row("                   Welcome back!", "Run /init to create a CLAUDE.md file"),
		row("", "────────────────────────"),
		row("                       ▐▛███▜▌", "What's new"),
		row("                      ▝▜█████▛▘", "Check the Claude Code changelog"),
		row("                         ▘▘  ▝▝", ""),
		row("", ""),
		row("    "+model+" with xhigh effort · API Usage Billing", ""),
		row("   "+directory, ""),
		"╰" + strings.Repeat("─", inner-2) + "╯",
	}
}

func centerLine(line string, width int) string {
	line = shortenDisplay(line, width)
	left := (width - displayWidth(line)) / 2
	if left < 0 {
		left = 0
	}
	return strings.Repeat(" ", left) + line
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func composerCursorAt(promptStart int, prefix, text string, width int) (int, int) {
	prompt := wrapWithPrefixes(prefix, text, "  ", width)
	lastLine := prompt[len(prompt)-1]
	return promptStart + len(prompt) - 1, displayWidth(lastLine) + 1
}

func otherCard(agent Agent, model, directory string, terminalWidth int) []string {
	var title, second, third string
	switch agent {
	case AgentClaude:
		title = "Claude Code v" + claudeVersion
		second = "Welcome back!"
		third = "Tips for getting started · /init"
	default:
		title = "OpenCode v" + openCodeVersion
		second = "Build with your favorite models."
		third = "Type /help for commands · @ to reference files"
	}
	contentWidth := maxDisplayWidth(title, second, third, directory) + 2
	if contentWidth < 37 {
		contentWidth = 37
	}
	if terminalWidth > 8 && contentWidth+4 > terminalWidth {
		contentWidth = terminalWidth - 4
	}
	fit := func(value string) string {
		value = shortenDisplay(value, contentWidth)
		return "│ " + value + strings.Repeat(" ", contentWidth-displayWidth(value)) + " │"
	}
	border := "╭" + strings.Repeat("─", contentWidth+2) + "╮"
	return []string{border, fit(title), fit(""), fit(second), fit(third), fit("directory: " + directory), "╰" + strings.Repeat("─", contentWidth+2) + "╯", ""}
}

func (t *codexTUI) styleOtherLine(line string, index, cardLen int) string {
	if t.cfg.NoColor {
		return line
	}
	const dim, reset, bold = "\033[2m", "\033[0m", "\033[1m"
	if index < cardLen {
		return dim + line + reset
	}
	if strings.HasPrefix(line, "❯ ") {
		return bold + "❯" + reset + line[len("❯"):]
	}
	if strings.HasPrefix(line, "› ") {
		return bold + "›" + reset + line[len("›"):]
	}
	if strings.HasPrefix(line, "  ?") || strings.Contains(line, "ctrl+p") || strings.Contains(line, " · ") {
		return dim + line + reset
	}
	return line
}

func (t *codexTUI) styleLine(line string, index, cardLen int) string {
	if t.cfg.NoColor {
		return line
	}
	const (
		dim    = "\033[2m"
		reset  = "\033[0m"
		bold   = "\033[1m"
		norm   = "\033[22m"
		cyan   = "\033[36m"
		red    = "\033[31m"
		italic = "\033[3m"
		roman  = "\033[23m"
	)
	if index < cardLen {
		if title := strings.Index(line, "OpenAI Codex"); title >= 0 {
			end := title + len("OpenAI Codex")
			return dim + line[:title] + bold + line[title:end] + norm + dim + line[end:] + reset
		}
		if model := strings.Index(line, "/model"); model >= 0 {
			end := model + len("/model")
			valueAt := strings.Index(line, "     ") + 5
			return dim + line[:valueAt] + norm + line[valueAt:model] + cyan + line[model:end] + dim + line[end:] + reset
		}
		if directory := strings.Index(line, "directory: "); directory >= 0 {
			valueAt := directory + len("directory: ")
			borderAt := strings.LastIndex(line, " │")
			if borderAt < valueAt {
				return dim + line + reset
			}
			return dim + line[:valueAt] + norm + line[valueAt:borderAt] + dim + line[borderAt:] + reset
		}
		return dim + line + reset
	}
	if strings.HasPrefix(line, "› ") {
		if strings.HasPrefix(line, "› Ask Codex") {
			return bold + "›" + norm + " " + dim + line[len("› "):] + reset
		}
		return bold + dim + "› " + norm + line[len("› "):] + reset
	}
	if strings.Contains(line, "esc to interrupt") {
		return dim + line + reset
	}
	if strings.HasPrefix(line, "• ") {
		text := line[len("• "):]
		if strings.HasPrefix(text, "Explor") || strings.HasPrefix(text, "Runn") || strings.HasPrefix(text, "Ran ") || strings.HasPrefix(text, "Edit") {
			return dim + "• " + reset + bold + text + reset
		}
		return dim + "• " + reset + text
	}
	if strings.HasPrefix(line, "◦ ") {
		return dim + line + reset
	}
	if strings.HasPrefix(line, "  └ ") {
		return dim + "  └ " + reset + line[len("  └ "):]
	}
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "+") {
		return "\033[32m" + line + reset
	}
	if strings.HasPrefix(trimmed, "-") {
		return red + line + reset
	}
	if strings.HasPrefix(trimmed, "@@") {
		return cyan + line + reset
	}
	if strings.HasPrefix(line, "■ ") {
		return red + line + reset
	}
	if strings.HasPrefix(line, "  Tip: New ") {
		return "  " + bold + "Tip:" + norm + " " + italic + "New" + roman + strings.TrimPrefix(line, "  Tip: New") + reset
	}
	if separator := strings.Index(line, " · "); separator >= 0 {
		return line[:separator] + dim + " · " + reset + line[separator+len(" · "):]
	}
	if strings.HasPrefix(strings.TrimSpace(line), "?") {
		return dim + line + reset
	}
	return line
}

func codexCard(model, directory string, terminalWidth int) []string {
	title := ">_ OpenAI Codex (v" + codexVersion + ")"
	modelLine := "model:     " + model + "   /model to change"
	directoryLine := "directory: " + directory
	contentWidth := maxDisplayWidth(title, modelLine, directoryLine)
	if contentWidth < 37 {
		contentWidth = 37
	}
	if terminalWidth > 8 && contentWidth+4 > terminalWidth {
		contentWidth = terminalWidth - 4
	}
	fit := func(line string) string {
		line = shortenDisplay(line, contentWidth)
		return "│ " + line + strings.Repeat(" ", contentWidth-displayWidth(line)) + " │"
	}
	border := "╭" + strings.Repeat("─", contentWidth+2) + "╮"
	return []string{border, fit(title), fit(""), fit(modelLine), fit(directoryLine), "╰" + strings.Repeat("─", contentWidth+2) + "╯", ""}
}

func maxDisplayWidth(lines ...string) int {
	max := 0
	for _, line := range lines {
		if width := displayWidth(line); width > max {
			max = width
		}
	}
	return max
}

func displayWidth(value string) int {
	width := 0
	for _, r := range value {
		switch {
		case r == '\t':
			width += 4
		case isWideRune(r):
			width += 2
		default:
			width++
		}
	}
	return width
}

func shortenDisplay(value string, limit int) string {
	if displayWidth(value) <= limit {
		return value
	}
	if limit <= 1 {
		return "…"
	}
	used := 0
	var out []rune
	for _, r := range value {
		w := displayWidth(string(r))
		if used+w > limit-1 {
			break
		}
		out = append(out, r)
		used += w
	}
	return string(out) + "…"
}

func isWideRune(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hangul, r) ||
		(r >= 0x1100 && r <= 0x11ff) || (r >= 0x2e80 && r <= 0xa4cf) ||
		(r >= 0xac00 && r <= 0xd7ff) || (r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe10 && r <= 0xfe6f) || (r >= 0xff01 && r <= 0xff60) ||
		(r >= 0x1f300 && r <= 0x1faff)
}

func displayDirectory(directory string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(directory, home+string(os.PathSeparator)) {
		return "~" + strings.TrimPrefix(directory, home)
	}
	return directory
}

func (t *codexTUI) inputLoop(ctx context.Context) error {
	keys := make(chan rune)
	keyErrors := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(os.Stdin)
		for {
			key, _, err := reader.ReadRune()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					keyErrors <- err
				}
				close(keys)
				return
			}
			keys <- key
		}
	}()
	var running *taskRun
	refresh := time.NewTicker(120 * time.Millisecond)
	defer refresh.Stop()
	for {
		select {
		case <-ctx.Done():
			if running != nil {
				running.cancel()
			}
			return nil
		case err := <-keyErrors:
			return err
		case <-refresh.C:
			if running != nil && t.status != "" {
				t.spinner++
				if err := t.render(false); err != nil {
					return err
				}
			}
		case err := <-func() <-chan error {
			if running == nil {
				return nil
			}
			return running.done
		}():
			if running != nil {
				running.cancel()
				running = nil
				t.status = ""
				t.startedAt = time.Time{}
				if errors.Is(err, context.Canceled) && ctx.Err() == nil {
					t.finalizeActiveTool()
					t.history = append(t.history, "■ Conversation interrupted - tell the model what to do differently.", "")
				} else if err != nil {
					return err
				}
				if err := t.render(false); err != nil {
					return err
				}
			}
		case event := <-func() <-chan Event {
			if running == nil {
				return nil
			}
			return running.events
		}():
			if err := t.event(event); err != nil {
				return err
			}
		case key, ok := <-keys:
			if !ok {
				if running != nil {
					running.cancel()
				}
				return nil
			}
			if running != nil {
				if key == 3 || key == 27 {
					running.cancel()
					t.status = "Cancelling"
					if err := t.render(false); err != nil {
						return err
					}
				}
				continue
			}
			start, err := t.handleKey(ctx, key)
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			if start != nil {
				running = start
			}
		}
	}
}

type taskRun struct {
	events <-chan Event
	done   <-chan error
	cancel context.CancelFunc
}

func (t *codexTUI) handleKey(ctx context.Context, key rune) (*taskRun, error) {
	if t.escape > 0 {
		if t.escape == 1 {
			if key == '[' {
				t.escape = 2
				return nil, nil
			}
			t.escape = 0
			return nil, nil
		}
		if t.escape == 3 { // Delete: ESC [ 3 ~
			t.escape = 0
			if key == '~' && t.cursor < len(t.input) {
				t.input = append(t.input[:t.cursor], t.input[t.cursor+1:]...)
				return nil, t.render(false)
			}
			return nil, nil
		}
		t.escape = 0
		switch key {
		case 'A':
			if t.commandAt > 0 {
				t.commandAt--
				t.input = []rune(t.commands[t.commandAt])
				t.cursor = len(t.input)
			}
		case 'B':
			if t.commandAt < len(t.commands)-1 {
				t.commandAt++
				t.input = []rune(t.commands[t.commandAt])
				t.cursor = len(t.input)
			} else {
				t.commandAt = len(t.commands)
				t.input, t.cursor = nil, 0
			}
		case 'C':
			if t.cursor < len(t.input) {
				t.cursor++
			}
		case 'D':
			if t.cursor > 0 {
				t.cursor--
			}
		case 'H':
			t.cursor = 0
		case 'F':
			t.cursor = len(t.input)
		case '3':
			t.escape = 3
			return nil, nil
		default:
			return nil, nil
		}
		return nil, t.render(false)
	}
	if key == 27 {
		t.escape = 1
		return nil, nil
	}
	switch key {
	case 3, 4: // Ctrl-C / Ctrl-D
		return nil, io.EOF
	case '\r', '\n':
		value := strings.TrimSpace(string(t.input))
		t.input, t.cursor = nil, 0
		if value == "" {
			return nil, t.render(false)
		}
		switch strings.ToLower(value) {
		case "/exit", "/quit", "exit", "quit":
			fmt.Fprint(t.out, "\r\n已退出 fake-vibecoding。\r\n")
			return nil, io.EOF
		case "/help", "help", "?":
			t.history = append(t.history, "? /help 查看帮助，/clear 清屏，/exit 退出")
			return nil, t.render(false)
		case "/model":
			t.history = append(t.history, "Model: "+t.cfg.Model+" (simulated)")
			return nil, t.render(false)
		case "/clear":
			t.history = nil
			return nil, t.render(false)
		}
		t.commands = append(t.commands, value)
		t.commandAt = len(t.commands)
		t.status = "Working"
		t.startedAt = time.Now()
		t.spinner = 0
		if err := t.render(false); err != nil {
			return nil, err
		}
		runCtx, cancel := context.WithCancel(ctx)
		events := make(chan Event)
		done := make(chan error, 1)
		t.cfg.Prompt, t.cfg.Iterations = value, 0
		go func() {
			err := NewSimulator(t.cfg, t.snippets).Run(runCtx, func(event Event) error {
				select {
				case events <- event:
					return nil
				case <-runCtx.Done():
					return runCtx.Err()
				}
			})
			done <- err
		}()
		return &taskRun{events: events, done: done, cancel: cancel}, nil
	case 8, 127:
		if t.cursor > 0 {
			t.input = append(t.input[:t.cursor-1], t.input[t.cursor:]...)
			t.cursor--
			return nil, t.render(false)
		}
	case 12: // Ctrl-L
		t.history = nil
		return nil, t.render(false)
	default:
		if key >= 32 {
			t.input = append(t.input, 0)
			copy(t.input[t.cursor+1:], t.input[t.cursor:])
			t.input[t.cursor] = key
			t.cursor++
			return nil, t.render(false)
		}
	}
	return nil, nil
}

func (t *codexTUI) event(e Event) error {
	if t.cfg.Agent != "" && t.cfg.Agent != AgentCodex {
		return t.otherEvent(e)
	}
	switch e.Type {
	case "task":
		t.history = append(t.history, "› "+e.Detail, "")
		t.status = "Working"
	case "tool":
		t.finalizeActiveTool()
		t.activeAt = len(t.history)
		t.active, t.activeArg = e.Name, e.Input
		switch e.Name {
		case "run_tests":
			t.activeArg = testCommand(t.cfg.Workdir)
			t.history = append(t.history, "• Running "+t.activeArg)
		case "apply_patch":
			t.history = append(t.history, "• Editing "+e.Input)
		default:
			t.history = append(t.history, "• "+toolVerb(e.Name), "  └ "+t.toolDetail(e))
		}
		t.status = "Working"
	case "diff":
		if e.Diff != nil {
			t.history = append(t.history, fmt.Sprintf("  └ %s (+%d -%d) [virtual, not applied]", e.Diff.Path, e.Diff.Added, e.Diff.Removed))
			for _, line := range strings.Split(e.Diff.Hunk, "\n") {
				if line != "" {
					t.history = append(t.history, "    "+line)
				}
			}
		}
	case "output":
		if t.activeAt >= 0 && t.activeAt < len(t.history) {
			switch t.active {
			case "run_tests":
				t.history[t.activeAt] = "• Ran " + t.activeArg
				t.history = append(t.history, "  └ "+testOutput(t.cfg.Workdir, t.snippets))
			case "apply_patch":
				t.history[t.activeAt] = "• Edited " + t.activeArg + " (+3 -1)"
			default:
				t.history[t.activeAt] = "• Explored"
			}
		}
		t.history = append(t.history, "")
		t.activeAt = -1
		t.active, t.activeArg = "", ""
	case "error":
		t.finalizeActiveTool()
		t.history = append(t.history, "■ stream disconnected before completion: request timed out")
		t.status = "Working"
	case "retry":
		t.history = append(t.history, fmt.Sprintf("  └ Retrying %d/%d…", e.Attempt, t.cfg.Retries))
	case "done":
		t.finalizeActiveTool()
		t.status = ""
		t.history = append(t.history, codexFinalResponse(e.Detail, t.snippets)...)
	}
	t.trimHistory()
	return t.render(false)
}

func (t *codexTUI) otherEvent(e Event) error {
	if t.cfg.Agent == AgentClaude {
		switch e.Type {
		case "task":
			t.history = append(t.history, "> "+e.Detail, "")
		case "tool":
			t.history = append(t.history, "⏺ "+claudeToolName(e), "")
		case "output":
			t.history = append(t.history, "⎿  "+otherToolResult(e), "")
		case "diff":
			if e.Diff != nil {
				t.history = append(t.history, "⎿  Proposed changes (not applied)")
				for _, line := range strings.Split(e.Diff.Hunk, "\n") {
					if line != "" {
						t.history = append(t.history, "     "+line)
					}
				}
			}
		case "error":
			t.history = append(t.history, "⎿  ⚠ "+e.Detail)
		case "retry":
			t.history = append(t.history, fmt.Sprintf("↻ Retrying request #%d", e.Attempt))
		case "done":
			t.history = append(t.history, "✓ Done", "")
		}
	} else {
		switch e.Type {
		case "task":
			t.history = append(t.history, "› "+e.Detail, "")
		case "tool":
			t.history = append(t.history, "• "+toolVerb(e.Name), "  └ "+t.toolDetail(e))
		case "output":
			t.history = append(t.history, "  └ "+otherToolResult(e), "")
		case "diff":
			if e.Diff != nil {
				t.history = append(t.history, "  └ "+e.Diff.Path+" (+"+fmt.Sprint(e.Diff.Added)+" -"+fmt.Sprint(e.Diff.Removed)+")")
				for _, line := range strings.Split(e.Diff.Hunk, "\n") {
					if line != "" {
						t.history = append(t.history, "     "+line)
					}
				}
			}
		case "error":
			t.history = append(t.history, "! "+e.Detail)
		case "retry":
			t.history = append(t.history, fmt.Sprintf("↻ retry %d", e.Attempt))
		case "done":
			t.history = append(t.history, "✓ done", "")
		}
	}
	if e.Type == "tool" || e.Type == "error" || e.Type == "retry" {
		t.status = "Working"
	}
	if e.Type == "done" {
		t.status = ""
	}
	t.trimHistory()
	return t.render(false)
}

func claudeToolName(e Event) string {
	name := map[string]string{
		"read_file": "Read", "list_directory": "List", "search": "Search",
		"run_tests": "Bash", "inspect_symbols": "Read", "apply_patch": "Edit",
		"git_diff": "Bash", "git_status": "Bash", "view_file": "Read", "web_search": "WebSearch",
	}[e.Name]
	if name == "" {
		name = e.Name
	}
	if e.Input == "" {
		return name
	}
	return name + "(" + e.Input + ")"
}

func otherToolResult(e Event) string {
	if e.Snippet != nil {
		return "Read " + e.Snippet.Path
	}
	if e.Name == "run_tests" {
		return "Tests passed"
	}
	return "Done"
}

func (t *codexTUI) trimHistory() {
	const maxHistoryLines = 2000
	if len(t.history) <= maxHistoryLines {
		return
	}
	drop := len(t.history) - maxHistoryLines
	t.history = append([]string(nil), t.history[drop:]...)
	if t.activeAt >= 0 {
		t.activeAt -= drop
		if t.activeAt < 0 {
			t.activeAt = -1
		}
	}
}

func (t *codexTUI) finalizeActiveTool() {
	if t.activeAt < 0 || t.activeAt >= len(t.history) {
		return
	}
	switch t.active {
	case "run_tests":
		t.history[t.activeAt] = "• Ran " + t.activeArg
	case "apply_patch":
		t.history[t.activeAt] = "• Edited " + t.activeArg + " (+3 -1)"
	default:
		t.history[t.activeAt] = "• Explored"
	}
	t.history = append(t.history, "")
	t.activeAt = -1
	t.active, t.activeArg = "", ""
}

func (t *codexTUI) toolDetail(e Event) string {
	switch e.Name {
	case "list_directory":
		return "List ."
	case "search":
		return "Search " + searchTerm(t.cfg.Prompt)
	case "read_file", "inspect_symbols":
		if e.Snippet != nil {
			return "Read " + e.Snippet.Path
		}
		return "Read files"
	case "git_status":
		return "git status --short"
	case "git_diff":
		return "git diff --stat"
	case "view_file":
		return "Open " + e.Input
	case "web_search":
		return "Search web for " + searchTerm(t.cfg.Prompt)
	case "run_command":
		if e.Input != "" && e.Input != "." {
			return e.Input
		}
		return "echo simulated"
	default:
		return e.Name
	}
}

func toolVerb(name string) string {
	switch name {
	case "run_tests":
		return "Running tests"
	case "git_status":
		return "Checked git status"
	case "git_diff":
		return "Reviewed diff"
	case "view_file":
		return "Opened file"
	case "web_search":
		return "Searched the web"
	case "run_command":
		return "Ran command"
	default:
		return "Exploring"
	}
}

func searchTerm(prompt string) string {
	fields := strings.Fields(prompt)
	if len(fields) == 0 {
		return "relevant symbols"
	}
	term := strings.Join(fields, " ")
	return shortenDisplay(term, 36)
}

func testCommand(workdir string) string {
	for _, candidate := range []struct{ file, command string }{
		{"go.mod", "go test ./..."},
		{"Cargo.toml", "cargo test"},
		{"package.json", "npm test"},
		{"pyproject.toml", "pytest"},
		{"Makefile", "make test"},
	} {
		if _, err := os.Stat(filepath.Join(workdir, candidate.file)); err == nil {
			return candidate.command
		}
	}
	return "git diff --check"
}

func testOutput(workdir string, snippets []Snippet) string {
	if testCommand(workdir) == "go test ./..." {
		module := "./..."
		for _, snippet := range snippets {
			if snippet.Path == "go.mod" {
				for _, line := range strings.Split(snippet.Text, "\n") {
					if value := strings.TrimSpace(strings.TrimPrefix(line, "module ")); strings.HasPrefix(line, "module ") && value != "" {
						module = value
						break
					}
				}
			}
		}
		return "ok  \t" + module + "\t0.003s"
	}
	return "(no output)"
}

func codexFinalResponse(prompt string, snippets []Snippet) []string {
	lower := strings.ToLower(prompt)
	if strings.Contains(lower, "第一行") || strings.Contains(lower, "first line") {
		for _, snippet := range snippets {
			if strings.Contains(lower, strings.ToLower(snippet.Path)) || strings.Contains(lower, strings.ToLower(filepath.Base(snippet.Path))) {
				return []string{"• " + snippet.Path + " 第一行内容是：" + firstLine(snippet.Text) + "。"}
			}
		}
	}
	if isChangeRequest(prompt) {
		return []string{
			"• 已完成“" + shortenDisplay(strings.TrimSpace(prompt), 48) + "”。",
			"",
			"  - 已更新相关实现",
			"  - 已运行针对性测试，结果通过",
		}
	}
	return []string{"• 已完成检查，相关代码与当前任务要求一致。"}
}

func (r renderer) header(cfg Config) error {
	if r.json {
		return nil
	}
	label := map[Agent]string{AgentCodex: "OpenAI Codex", AgentClaude: "Claude Code", AgentOpenCode: "OpenCode"}[r.agent]
	_, err := fmt.Fprintf(r.out, "%s · fake-vibecoding %s\n工作目录: %s\n模型: %s（模拟）\n按 Ctrl-C 停止\n\n", label, version, cfg.Workdir, cfg.Model)
	return err
}

func (r renderer) event(e Event) error {
	if r.json {
		return json.NewEncoder(r.out).Encode(e)
	}
	if e.Type == "diff" && e.Diff != nil {
		if _, err := fmt.Fprintf(r.out, "  └ %s (+%d -%d) [virtual, not applied]\n%s\n", e.Diff.Path, e.Diff.Added, e.Diff.Removed, indentDiff(e.Diff.Hunk)); err != nil {
			return err
		}
		return nil
	}
	switch r.agent {
	case AgentClaude:
		switch e.Type {
		case "task":
			_, _ = fmt.Fprintf(r.out, "\n> %s\n", e.Detail)
		case "tool":
			_, _ = fmt.Fprintf(r.out, "  ⏺ %s(%s)\n", e.Name, e.Input)
		case "output":
			_, _ = fmt.Fprintf(r.out, "  ⎿  %s\n", e.Detail)
		case "error":
			_, _ = fmt.Fprintf(r.out, "  ⎿  ⚠ %s\n", e.Detail)
		case "retry":
			_, _ = fmt.Fprintf(r.out, "  ↻ %s #%d\n", e.Detail, e.Attempt)
		case "done":
			_, _ = fmt.Fprintln(r.out, "  ✓ 本轮结束")
		}
	case AgentOpenCode:
		switch e.Type {
		case "task":
			_, _ = fmt.Fprintf(r.out, "\n› %s\n", e.Detail)
		case "tool":
			_, _ = fmt.Fprintf(r.out, "  %s  %s\n", e.Name, e.Input)
		case "output":
			_, _ = fmt.Fprintf(r.out, "  └ %s\n", e.Detail)
		case "error":
			_, _ = fmt.Fprintf(r.out, "  ! %s\n", e.Detail)
		case "retry":
			_, _ = fmt.Fprintf(r.out, "  ↻ retry %d: %s\n", e.Attempt, e.Detail)
		case "done":
			_, _ = fmt.Fprintln(r.out, "  ✓ done")
		}
	default:
		switch e.Type {
		case "task":
			_, _ = fmt.Fprintf(r.out, "• %s\n", e.Detail)
		case "tool":
			_, _ = fmt.Fprintf(r.out, "  ↳ %s(%s)\n", e.Name, e.Input)
		case "output":
			_, _ = fmt.Fprintf(r.out, "  %s\n", e.Detail)
		case "error":
			_, _ = fmt.Fprintf(r.out, "  ⚠ %s\n", e.Detail)
		case "retry":
			_, _ = fmt.Fprintf(r.out, "  ↻ %s #%d\n", e.Detail, e.Attempt)
		case "done":
			_, _ = fmt.Fprintln(r.out, "  ✓ 本轮任务完成")
		}
	}
	if e.Snippet != nil && e.Type == "output" {
		_, _ = fmt.Fprintf(r.out, "    %s: %s\n", e.Snippet.Path, firstLine(e.Snippet.Text))
	}
	return nil
}

func indentDiff(hunk string) string {
	lines := strings.Split(hunk, "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = "    " + line
		}
	}
	return strings.Join(lines, "\n")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func main() {
	args := os.Args[1:]
	if a := executableAgent(os.Args[0]); a.valid() && (len(args) == 0 || Agent(args[0]).valid() == false) {
		args = append([]string{string(a)}, args...)
	}
	cfg, err := ParseConfig(args)
	if errors.Is(err, flag.ErrHelp) {
		if cfg.ShowVersion {
			fmt.Println("fake-vibecoding " + version)
		} else {
			printUsage()
		}
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake-vibecoding:", err)
		fmt.Fprintln(os.Stderr, "用法: fake-vibecoding [codex|claude|opencode] [选项] [任务描述]")
		os.Exit(2)
	}
	snippets, err := LoadSnippets(cfg.Workdir, cfg.MaxFiles, cfg.SnippetBytes)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读取代码片段:", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := renderer{agent: cfg.Agent, json: cfg.JSON, out: os.Stdout}
	if cfg.Interactive {
		if err := runInteractive(ctx, cfg, snippets, r); err != nil {
			fmt.Fprintln(os.Stderr, "交互模式停止:", err)
			os.Exit(1)
		}
		return
	}
	if err := r.header(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	sim := NewSimulator(cfg, snippets)
	if err := sim.Run(ctx, r.event); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "模拟停止:", err)
		os.Exit(1)
	}
}
