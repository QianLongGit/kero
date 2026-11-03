# `kero tui` — Interactive Session Picker（设计文档）

- 日期：2026-11-03
- 状态：待批准
- 路线：已选 方案 A（手写 ANSI TUI，零新依赖）

## Intent & Why

手机或任意 SSH shell 上进入指定会话的流程太长：`kero ls` → 肉眼找 ID → 复制 → `kero attach <id>`；遇到分屏还必须复制 pane ID。`kero tui` 一条命令进入全屏可点选的会话列表，选中即 attach。

## Goals

- 全屏 TUI 列出全部运行中会话，多 pane 会话缩进展开 pane 行
- 键盘（↑↓/Enter/数字 1-9）、鼠标点击、手机触摸点按直接 attach 到选中 pane
- 管理动作：`n` 新建、`s` 分屏、`d` 关闭（确认框）、`r` 重命名、`/` 过滤
- 底部固定快捷键帮助栏，随当前状态（列表/过滤/输入/确认）切换提示，简明易读
- 列表实时刷新（daemon 事件流推送，非轮询）
- attach 后 detach（Ctrl+B d）→ 直接落回 shell（一次性选择器，非常驻工作台）

## Non-goals

- restart / resume 等更多动作（按需后加）
- detach 后循环回到 TUI
- 会话内操作（那是 attach 直通的事）

## Anti-goals

- 引入任何终端 UI 依赖（bubbletea/tview/tcell…）——仓库极简依赖是硬约束（当前 go.mod 仅 7 个）
- 动 daemon 协议（proto.Version 不变，纯客户端功能）

## Constraints

- 一 feature 一文件：`internal/cli/tui.go` + `tui_test.go`；`cli.go` 大 switch 加 `case "tui"` + usage 文本
- 复用现成件：`connect(host)`、`c.Call("state")`、`c.Call("watch", nil, nil)` 激活事件订阅（daemon 在锁下立即推首包，范例 `internal/cli/events.go:322`）、`Client.OnEvent` 收 `Message{Event:"state", State:…}` 整包推送、`pickAttach` 的 pane 解析语义、`AttachViaRaw`/`attachLoop`（全参数注入，不持有终端/信号，`attach.go:218-221`）
- 管理动作全部走现有 RPC：`new` / `split` / `close`（`kill` 别名）/ `rename`
- `-H host` 多主机参数惯例自动继承

## Approaches

- **A. 手写 ANSI TUI（已选）** — `x/term` 已在依赖；自研 alt-screen、SGR 鼠标解析、列表渲染、单行输入框（约 500–700 行）。零新依赖，与 attach 的 raw 直通手写风格一致，fork merge 无新增漂移面
- B. bubbletea + bubbles — 组件开箱即用，但带进 charmbracelet 全家桶（十几个传递依赖），违背仓库哲学，放弃
- C. 无 alt-screen 的打印列表+读数字 — 满足不了点按/触摸需求，排除

## Decisions

对话已定：

- 一次性选择器；detach 落 shell
- 七个动作：attach / `n` / `s` / `d` / `r` / `/` / `q`·`Esc`
- 底部帮助栏必须清楚注明按键用法（用户明确要求）
- 方案 A 手写

设计补全（my call，标注理由）：

- **列表行**：状态色点 + 名字 + 目录（超长截断）+ agent/命令 + 最近活跃时间；单 pane 会话不重复列 pane 行，多 pane 缩进展开；状态色用终端默认 16 色（idle/working/waiting/error/exited 各一色），不硬编码 RGB，天然适配明暗主题
- **排序**：按 Activity 降序，最近活跃在最上——与 bare `attach`（pickAttach 选最近活跃）语义一致【my call】
- **attach 目标**：会话行 → FocusPane；pane 行 → 该 pane
- **鼠标**：SGR 1006 编码 + 按钮追踪（模式 1002，点击+滚轮），不用 any-motion 1003（手机上事件流量浪费）【my call】；滚轮上下移动光标
- **数字键 1-9**：移动光标到对应行，再 Enter 确认（比直接进入多一步，防手机误触）【my call】
- **动作语义**：`d` → `close`（y/N 确认框）；`s` → 无参数 `split`（默认向右分屏跑 shell，零输入）；`n` → 单行输入命令，空 = 默认 shell；`r` → 单行输入预填当前名
- **过滤**：`/` 进入过滤态，子串大小写不敏感匹配 name/title/dir，`Esc` 清除退出
- **刷新**：初始 `state` RPC，随后 `watch` RPC 激活订阅，`OnEvent` 收 `Message{Event:"state"}` 整包即重绘；**OnEvent 回调只向 channel 入队并唤醒渲染循环，回调内不做重绘/RPC**（回调在 client 读 goroutine 上同步执行，范例模式 `events.go:296-321`；watcher 落后会被 daemon 直接断连 `daemon.go:630-638`）；断连时静默重订阅一次 `watch`，仍断则退化为静态列表并在状态行提示 live off；进 attach 前退订
- **动作×行类型矩阵**：
  - attach：会话行 → FocusPane；pane 行 → 该 pane
  - `d`：跟随 CLI 先例（`cli.go:781-784`）——会话行 → `close` 整个会话；pane 行 → `pane.close` 只关该 pane。确认框文案区分 "close session …?" / "close pane …?"
  - `s`：作用目标 = 光标所指 pane（pane 行 → 该 pane；会话行 → FocusPane），与 `findOrCurrent` 语义对齐
  - `r`：恒作用于光标行所属**会话**（`rename` 是会话级，`cli.go:743`）
- **`n` 新建闭环**：建完留在列表，等 state 事件刷出新行，光标自动跳到新会话行；不直接 attach（用户可能连建多个，attach 是 Enter 的事）
- **过滤态键位**：过滤态只收编辑键 + `Enter`（确认过滤，回列表态，光标落首个匹配行）+ `Esc`（清除过滤，回列表态）；过滤态下 `n/s/d/r/q` 一律不触发（防误操作）；列表态再按 `/` 可继续编辑当前过滤词
- **输入框编辑键**：字符插入、退格、←→、Ctrl+W（删词）、Ctrl+U（清行）、Enter 确认、Esc 取消【my call】
- **进程流与 termios 时序**：TUI 全程持有 raw 模式（读按键所需）。connect → state + watch → 渲染循环（读 stdin 解析按键/鼠标）→ 选中后：退出 alt-screen、关闭鼠标追踪，**保持 raw** 进入 `attachLoop`（新开 raw attach 连接；raw 直通要求字节不被终端加工）→ detach 后 Restore 恢复 cooked → 进程退出落 shell。范例：`attachCmd` 即 MakeRaw → attachLoop → defer Restore（`attach.go:159-176`）
- **resize**：读输入间隙检测 terminal size 变化即整帧重绘【my call】

## Left to builder

- 渲染与输入解析做成纯函数（`state+filter+size → 行`、`bytes → 输入事件`）便于单测；集成冒烟靠真机（手机 SSH 实测点按）
- usage 文本与 `docs/cli.md` 补 `tui` 条目（英文，跟现有文档惯例）
- 测试遵循 `t.Setenv("KERO_HOME", t.TempDir())` + 临时 `KERO_CONFIG` 隔离惯例
- 状态色与帮助栏文案的具体措辞
