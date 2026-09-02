# 发布说明

## 自动发布

发布流程由 `.github/workflows/release.yml` 管理。推送 `v*` 标签后，GoReleaser 会：

1. 为 Linux、macOS、Windows 和 FreeBSD 构建 amd64/arm64 归档；
2. 生成校验和并创建 GitHub Release；
3. 更新 `techblack/homebrew-tap` 中的 `Formula/fake-vibecoding.rb`。

Release workflow 已使用仓库内置的 `GITHUB_TOKEN`。Homebrew tap 是独立仓库，因此还
需要在本仓库配置 `HOMEBREW_TAP_GITHUB_TOKEN` secret，并授予它对
`techblack/homebrew-tap` 的写权限。没有该 secret 时二进制 Release 仍可构建，但公式
无法推送到 tap。

## 发布 `v0.0.1`

```bash
git checkout -b release/v0.0.1
git tag -a v0.0.1 -m 'release: v0.0.1'
git push origin release/v0.0.1
git push origin v0.0.1
```

发布后安装：

```bash
brew tap techblack/tap
brew install fake-vibecoding
```

如果使用本地 GoReleaser 预览：

```bash
goreleaser release --snapshot --clean
```
