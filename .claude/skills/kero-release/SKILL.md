---
name: kero-release
description: Use when 发布 kero 新版本 (release, cut a version, 打版本, ship a build of kero / QianLongGit fork), when building or uploading kero release assets (DMG, linux tar/deb, kero-server, install-server), when signing server archives (.sig/.sha256, MYGO_UPDATER_PRIVATE_KEY), or when a kero release step fails (build-server.sh network error, gh untagged URL, missing assets, remote-install 404). Covers the fork's all-local release runbook: no GitHub Actions, ad-hoc macOS signing, gh -R required.
---

# Kero 发布（QLongGit/kero fork 全本地流程）

Kero 的发布**完全在本地构建**，GitHub Actions 已禁用（tag push 不触发任何 workflow），
绝不等待 workflow。RELEASING.md 的 tag-触发-CI 流程**不适用**。目标 ≈25 个资产。

## 0. 本机环境（每条命令的前提）

```sh
export PATH=/Users/dev/sdk/go1.27.3/bin:$PATH
unset GOROOT GOPATH
```

- go 不在 PATH；环境残留失效的 `GOROOT`/`GOPATH`，不 unset 直接报 `cannot find GOROOT directory`
- 模块下载失败时前缀 `GOPROXY=https://goproxy.cn,direct`（默认 proxy.golang.org 在本机不可达）
- **gh 每条命令必带 `-R QianLongGit/kero`**——仓库有多个 remote（origin/上游 egoist/其他），gh 默认解析到 upstream egoist/kero
- **rtk hook 的 exit code 不可信**（出现过 DATA RACE + FAIL 但 exit 0）：判绿必须看输出里有 `ok` 行、grep 不到 `FAIL`/`DATA RACE`；输出被吞用 `rtk proxy <cmd>` 跑原始命令
- 本机无 Developer ID 证书 → `-sign - -skip-notarize`（ad-hoc），首开需右键打开

## 1. 发布前检查（全绿才继续）

```sh
go vet ./... && rtk proxy go test ./internal/... -count=1   # TestBridgeScript 本机必挂（装了 /Applications/Kero.app 所致），已裁决忽略，CI 绿
rtk proxy go test . -count=1                                # root UI 测试，首跑下载 libghostty-vt
go tool mygo vet .                                          # mygo 存储值/goroutine 检查，无输出即通过
gh repo view QianLongGit/kero --json visibility             # 必须 PUBLIC：远程安装是匿名 curl
```

- 公钥两处镜像必须一致（sign 工具会对照 update.go 校验，不一致直接报错）：
  `mygo.json` 的 `updates.publicKey` ↔ `internal/update/update.go` 的 `const PublicKey`
- `internal/update/update.go` 的 `const Repo` = `QianLongGit/kero`
- `proto.Version` 只在不兼容协议改动时 bump，加可选字段不 bump（改动涉及协议时的检查项）

## 2. 版本与提交（X.Y.Z 全程替换）

1. `mygo.json` 的 `version` 改 X.Y.Z
2. `CHANGELOG.md` 顶部加用户视角小节（一个 bullet 一个功能，格式看上一节）
3. `git commit -am "Release X.Y.Z"` → `git tag vX.Y.Z` → `git push origin main vX.Y.Z`

## 3. 构建

```sh
# app：DMG + linux tar/deb（Windows 安装器是 CI-only，本 fork 不做；server 的 Windows zip 覆盖远程安装）
go tool mygo build -platform "darwin/arm64,linux/amd64,linux/arm64" -sign - -skip-notarize
# → build/darwin-arm64/kero-X.Y.Z-arm64.dmg, build/linux-*/…
ls build/darwin-arm64/*.dmg build/linux-*/   # upload 前核对确切文件名（.deb 是 kero_X.Y.Z_amd64.deb，下划线、无 linux- 前缀）

# server：6 平台归档 + install-server.sh/.ps1
sh scripts/build-server.sh
# → build/server/kero-server-X.Y.Z-{os}-{arch}.tar.gz|.zip
```

- build-server.sh 下载 libghostty-vt 走 GitHub，路由不稳：`curl: (35)`/SSL 错误**直接重跑整个脚本**（幂等）；卡死就停掉重跑
- 脚本会自动盖 install 脚本版本戳，仍要核对：`grep -o 'X\.Y\.Z' build/server/install-server.sh | head -1`；停在旧版就 `sed -i '' 's/OLD/NEW/g'`

## 4. 签名与校验（sidecar）

**sign 工具一条命令同时写 `.sig` 和 `.sha256`**，无需再手动 shasum：

```sh
cd build/server
MYGO_UPDATER_PRIVATE_KEY=$(cat ~/Library/Application\ Support/mygo/update-keys/mygo-update.key) \
  go run ../../internal/update/sign kero-server-X.Y.Z-*.tar.gz kero-server-X.Y.Z-*.zip
# 6 归档 × 3（归档+.sha256+.sig）= 18 个文件
```

- 私钥在 `~/Library/Application Support/mygo/update-keys/mygo-update.key`，永不入仓库
- **重跑过 build-server.sh 重建了归档 → 必须重跑 sign**：旧 .sig 对新归档，远程更新 Verify 必挂

## 5. 发布（draft → upload → 公开）

```sh
gh release create vX.Y.Z -R QianLongGit/kero --draft --title "Kero X.Y.Z" --notes "$(cat <<'EOF'
- <CHANGELOG 小节原文>

Note: this fork's macOS builds are ad-hoc signed; on first launch right-click the app and choose Open.
EOF
)"

gh release upload vX.Y.Z -R QianLongGit/kero \
  build/darwin-arm64/kero-X.Y.Z-arm64.dmg \
  build/linux-amd64/kero-X.Y.Z-linux-amd64.tar.gz build/linux-amd64/kero_X.Y.Z_amd64.deb \
  build/linux-arm64/kero-X.Y.Z-linux-arm64.tar.gz build/linux-arm64/kero_X.Y.Z_arm64.deb \
  build/server/kero-server-X.Y.Z-* \
  build/server/install-server.sh build/server/install-server.ps1

gh release edit vX.Y.Z -R QianLongGit/kero --draft=false
```

## 6. 验证

```sh
gh release view vX.Y.Z -R QianLongGit/kero --json isDraft,assets -q '{draft:.isDraft, assets:(.assets|length)}'
# 期望 {"draft":false,"assets":25}：5 app（1 dmg + 2 tar.gz + 2 deb）+ 18 server（6 归档 ×3）+ 2 install 脚本
# 数目不对时按此分项定位缺的是哪类；重传同名资产加 --clobber
# 匿名可达（远程安装就是匿名下载，缺一个 = 远程 404）：
curl -fsSL -o /dev/null -w '%{http_code}\n' https://github.com/QianLongGit/kero/releases/download/vX.Y.Z/install-server.sh
```

## 坑速查

| 症状 | 原因与处置 |
|---|---|
| `gh release create` 返回 `untagged-xxx` URL | **正常**：tag 已存在时 draft 的临时 slug；`--draft=false` 后变正式 URL。不放心用 `gh release list -R QianLongGit/kero --json tagName` 核实 |
| upload 传了 2.3.0 等旧资产 | `build/server/` 残留旧版本文件，**glob 必须带版本号** `kero-server-X.Y.Z-*` |
| `the key is not the one Kero checks` | 公钥两处镜像不同步：改 `internal/update/update.go` 的 `const PublicKey` 使与 `mygo.json` 一致 |
| 远程主机安装 404 | release 缺 server 资产 / 仓库转私有；对照第 6 步匿名 curl |
| go 命令 `cannot find GOROOT directory` | 见第 0 节：unset GOROOT GOPATH |
| gh 查到/操作了 egoist/kero | 漏了 `-R QianLongGit/kero` |
| 测试 exit 0 但其实挂了 | rtk 假阴性，grep FAIL/DATA RACE |
