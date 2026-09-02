fake-vibecoding

一个golang程序，支持常见的主流操作系统

1. 支持模拟codex/claude/opencode 模拟在vibe coding
2. 完全模拟其最新版的命令行样式，并无限随机在执行“任务”，“工具”，支持指定工作目录（但不会对工作目录有任何修改），只读取代码片段用以模拟
3. 不需要原有的codex/claude/opencode程序，独立可模拟，可运行
4. 可适当模拟一些模型超时等错误重试
