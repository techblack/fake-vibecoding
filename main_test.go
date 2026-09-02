package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	d := t.TempDir()
	return Config{Agent: AgentCodex, Workdir: d, Iterations: 1, Seed: 7, Interval: 0, ErrorRate: 0, Retries: 1, Timeout: time.Second, SnippetBytes: 40, MaxFiles: 2}
}

func TestLoadSnippetsBoundedAndSkipsIgnoredDirectories(t *testing.T) {
	cfg := testConfig(t)
	if err := os.Mkdir(filepath.Join(cfg.Workdir, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Workdir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.Workdir, "node_modules", "ignored.js"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSnippets(cfg.Workdir, 2, cfg.SnippetBytes)
	if err != nil || len(got) != 1 || got[0].Path != "main.go" {
		t.Fatalf("snippets = %#v, err = %v", got, err)
	}
	if len(got[0].Text) > cfg.SnippetBytes {
		t.Fatalf("snippet exceeds bound: %d", len(got[0].Text))
	}
}

func TestLoadSnippetsRejectsInvalidBounds(t *testing.T) {
	d := t.TempDir()
	if _, err := LoadSnippets(d, -1, 10); err == nil {
		t.Fatal("expected negative file bound to fail")
	}
	if _, err := LoadSnippets(d, 1, 0); err == nil {
		t.Fatal("expected zero byte bound to fail")
	}
}

func TestSimulatorEmitsRetries(t *testing.T) {
	cfg := testConfig(t)
	cfg.ErrorRate = 1
	cfg.Retries = 2
	sim := NewSimulator(cfg, nil)
	var events []Event
	if err := sim.Run(context.Background(), func(e Event) error { events = append(events, e); return nil }); err != nil {
		t.Fatal(err)
	}
	var errorsSeen, retries int
	for _, e := range events {
		if e.Type == "error" {
			errorsSeen++
		}
		if e.Type == "retry" {
			retries++
		}
	}
	if errorsSeen == 0 || retries == 0 {
		t.Fatalf("expected timeout retries, events = %#v", events)
	}
}

func TestSimulatorUsesPromptAwareToolSequence(t *testing.T) {
	cfg := testConfig(t)
	snippets := []Snippet{{Path: "README.md", Text: "# title"}}
	sim := NewSimulator(cfg, snippets)
	if got := sim.toolSequence("读取 README.md 第一行，不要修改文件"); len(got) != 1 || got[0] != "read_file" {
		t.Fatalf("read-only sequence = %#v", got)
	}
	got := sim.toolSequence("修复登录逻辑")
	if len(got) < 2 || got[len(got)-2] != "apply_patch" || got[len(got)-1] != "run_tests" {
		t.Fatalf("change sequence = %#v", got)
	}
}

func TestFakeDiffIsVirtualAndNeverWrites(t *testing.T) {
	cfg := testConfig(t)
	snippet := Snippet{Path: "main.go", Text: "package main\nfunc main() {}"}
	sim := NewSimulator(cfg, []Snippet{snippet})
	diff := sim.fakeDiff(&snippet)
	if diff.Path != "main.go" || diff.Added != 1 || diff.Removed != 1 || !strings.Contains(diff.Hunk, "-package main") || !strings.Contains(diff.Hunk, "+package main // simulated") {
		t.Fatalf("unexpected virtual diff: %#v", diff)
	}
}

func TestInteractivePromptRunsUntilCancelled(t *testing.T) {
	cfg := testConfig(t)
	cfg.Iterations = 0
	cfg.Prompt = "持续检查项目"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var tasksSeen, toolsSeen, doneSeen int
	err := NewSimulator(cfg, nil).Run(ctx, func(event Event) error {
		switch event.Type {
		case "task":
			tasksSeen++
		case "tool":
			toolsSeen++
		case "done":
			doneSeen++
		}
		if toolsSeen >= 7 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context cancellation", err)
	}
	if tasksSeen != 1 || toolsSeen < 7 || doneSeen != 0 {
		t.Fatalf("events: task=%d tool=%d done=%d", tasksSeen, toolsSeen, doneSeen)
	}
}

func TestParseConfigAgentAndPrompt(t *testing.T) {
	cfg, err := ParseConfig([]string{"claude", "--iterations", "2", "--interval", "0", "fix", "the", "bug"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent != AgentClaude || cfg.Iterations != 2 || cfg.Prompt != "fix the bug" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	claudeSubagent, err := ParseConfig([]string{"claude", "--agent", "reviewer", "--once", "--interval", "0"})
	if err != nil || claudeSubagent.Agent != AgentClaude {
		t.Fatalf("Claude named agent changed presentation: cfg=%+v err=%v", claudeSubagent, err)
	}
}

func TestParseConfigSelectsInteractiveMode(t *testing.T) {
	interactive, err := ParseConfig([]string{"codex", "--interval", "0"})
	if err != nil {
		t.Fatal(err)
	}
	if !interactive.Interactive {
		t.Fatal("codex without a task should enter interactive mode")
	}
	automatic, err := ParseConfig([]string{"codex", "--prompt", "demo", "--interval", "0"})
	if err != nil {
		t.Fatal(err)
	}
	if automatic.Interactive {
		t.Fatal("a supplied prompt should use automatic mode")
	}
	if automatic.Iterations != 1 {
		t.Fatalf("a supplied prompt should run one round by default, got %d", automatic.Iterations)
	}
	continuous, err := ParseConfig([]string{"codex", "--prompt", "demo", "--iterations", "0"})
	if err != nil {
		t.Fatal(err)
	}
	if continuous.Iterations != 0 {
		t.Fatal("explicit --iterations 0 should remain continuous")
	}
}

func TestParseConfigRejectsNativeClientExecution(t *testing.T) {
	for _, args := range [][]string{
		{"codex", "--native"},
		{"claude", "--passthrough"},
		{"opencode", "--real"},
	} {
		if _, err := ParseConfig(args); err == nil || !strings.Contains(err.Error(), "不调用模型") {
			t.Fatalf("ParseConfig(%#v) error = %v, want native execution rejection", args, err)
		}
	}
}

func TestCodexCardContainsRealCLIFields(t *testing.T) {
	card := strings.Join(codexCard("loading", "loading", 80), "\n")
	for _, want := range []string{
		">_ OpenAI Codex (v" + codexVersion + ")",
		"model:     loading   /model to change",
		"directory: loading",
	} {
		if !strings.Contains(card, want) {
			t.Fatalf("card does not contain %q: %s", want, card)
		}
	}
}

func TestOtherAgentLayoutsUseTerminalWidth(t *testing.T) {
	for _, agent := range []Agent{AgentClaude, AgentOpenCode} {
		lines, historyAt := otherBase(agent, "gpt-5.6-sol xhigh", "/tmp/project", 80)
		if historyAt == 0 || len(lines) <= historyAt {
			t.Fatalf("%s base has no transcript split: %d %#v", agent, historyAt, lines)
		}
		for i, line := range lines {
			if displayWidth(line) > 80 {
				t.Fatalf("%s line %d exceeds width: %d %q", agent, i, displayWidth(line), line)
			}
		}
	}
	claude, _ := otherBase(AgentClaude, "gpt-5.6-sol xhigh", "/tmp/project", 80)
	claudeText := strings.Join(claude, "\n")
	for _, want := range []string{"Claude Code v" + claudeVersion, "Welcome back!", "Tips for getting started", "❯ "} {
		if !strings.Contains(claudeText, want) {
			t.Fatalf("Claude TUI missing %q", want)
		}
	}
	opencode, _ := otherBase(AgentOpenCode, "gpt-5.6-sol xhigh", "/tmp/project", 80)
	opencodeText := strings.Join(opencode, "\n")
	for _, want := range []string{"█▀▀█", "Ask anything...", "tab agents", openCodeVersion} {
		if !strings.Contains(opencodeText, want) {
			t.Fatalf("OpenCode TUI missing %q", want)
		}
	}
}

func TestCodexCardKeepsBordersAlignedForChineseDirectory(t *testing.T) {
	card := codexCard("gpt-5.6-sol xhigh", "/tmp/项目/代码", 60)
	if len(card) < 6 {
		t.Fatalf("card too short: %#v", card)
	}
	for _, line := range card[:6] {
		if displayWidth(line) != displayWidth(card[0]) {
			t.Fatalf("misaligned card line %q (width %d, border %d)", line, displayWidth(line), displayWidth(card[0]))
		}
	}
	styled := (&codexTUI{}).styleLine(card[4], 4, len(card))
	if !utf8.ValidString(styled) {
		t.Fatal("styled directory line contains invalid UTF-8")
	}
}

func TestCodexTUIRenderUsesRawTerminalLineEndings(t *testing.T) {
	var out bytes.Buffer
	tui := codexTUI{
		cfg:    Config{Model: "gpt-test", Workdir: "/tmp/项目", NoColor: true},
		out:    &out,
		width:  60,
		height: 24,
	}
	if err := tui.render(false); err != nil {
		t.Fatal(err)
	}
	data := out.Bytes()
	for i, b := range data {
		if b == '\n' && (i == 0 || data[i-1] != '\r') {
			t.Fatalf("raw TUI emitted a bare LF at byte %d", i)
		}
	}
}

func TestCodexTranscriptWrapsWithoutTerminalOverflow(t *testing.T) {
	lines := wrapTranscriptLine("› 这是一个很长的中文任务用于验证自动换行", 20)
	if len(lines) < 2 || !strings.HasPrefix(lines[1], "  ") {
		t.Fatalf("unexpected wrapped lines: %#v", lines)
	}
	for _, line := range lines {
		if displayWidth(line) > 20 {
			t.Fatalf("line exceeds terminal width: %q = %d", line, displayWidth(line))
		}
	}
}

func TestCodexTranscriptMatchesToolAndReplyCells(t *testing.T) {
	var out bytes.Buffer
	snippet := Snippet{Path: "README.md", Text: "# fake-vibecoding\n\nbody"}
	tui := codexTUI{
		cfg:       Config{Model: "gpt-test", Workdir: t.TempDir(), Prompt: "读取 README.md 第一行", Retries: 2, NoColor: true},
		snippets:  []Snippet{snippet},
		out:       &out,
		width:     80,
		height:    40,
		activeAt:  -1,
		startedAt: time.Now(),
	}
	events := []Event{
		{Type: "task", Detail: tui.cfg.Prompt},
		{Type: "tool", Name: "read_file", Input: snippet.Path, Snippet: &snippet},
		{Type: "output", Name: "read_file", Snippet: &snippet},
		{Type: "done", Detail: tui.cfg.Prompt},
	}
	for _, event := range events {
		if err := tui.event(event); err != nil {
			t.Fatal(err)
		}
	}
	transcript := strings.Join(tui.history, "\n")
	for _, want := range []string{
		"› 读取 README.md 第一行",
		"• Explored\n  └ Read README.md",
		"• README.md 第一行内容是：# fake-vibecoding。",
	} {
		if !strings.Contains(transcript, want) {
			t.Fatalf("transcript does not contain %q:\n%s", want, transcript)
		}
	}
	for _, unwanted := range []string{"Simulated tool result", "• Completed"} {
		if strings.Contains(transcript, unwanted) {
			t.Fatalf("transcript contains generic text %q:\n%s", unwanted, transcript)
		}
	}
}

func TestRunDoesNotModifyWorkdir(t *testing.T) {
	cfg := testConfig(t)
	path := filepath.Join(cfg.Workdir, "x.py")
	if err := os.WriteFile(path, []byte("print('x')"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := NewSimulator(cfg, nil).Run(context.Background(), func(Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) || strings.Contains(string(after), "fake-vibecoding") {
		t.Fatal("simulation modified source file")
	}
}
