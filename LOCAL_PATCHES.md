# 本 fork 相对上游（electrikmilk/cherri）的改动

本仓库是 [cherri](https://github.com/electrikmilk/cherri) 的 fork，为 **camera-watermark** 项目加了以下补丁。
每个补丁都写了「为什么需要」和「将来 rebase/merge 上游时冲突怎么解」。

## 1. 生成期 Nothing 剪枝（commit `c559f67`）

- 文件：`nothing.go`（新增，`pruneInertNothings()`）、`shortcutgen.go`（在 `generateActions()` 之后、
  `generateImportQuestions()` 之前调用；剪枝会移动动作下标，故同步重映射 import question 的 `actionIndex`）
- 为什么：cherri 的 `parser.go` 在**每个控制流分组收尾**都会插入 `is.workflow.actions.nothing`
  （用于切断 Shortcuts 的隐式输入链）。其中绝大多数是**可证明无副作用的**（下一个动作已显式指定输入，
  或本身不消费输入）⇒ 白占动作数（我们的产物里 31 个 = 284→253）。
- 判据：与 `camera-watermark` 的 `tools/strip_inert_nothings.py` 逐字一致，**fail-closed**；
  遇到「Nothing 后紧跟**没有** WFInput 的 Set Variable」时**必须保留**（Apple 官方 golden 快捷指令
  `WordleBot@46` 证明那种 Nothing 是必需的）。
- ⚠️ 隐患：`WFControlFlowMode` 在 cherri 里是 `uint64`，取出来时若只处理 int/int64/float64 会读成 0，
  把"结束/else 标记"误判成"控制流开始" ⇒ 只剪一半。改类型时务必覆盖 `uint64`/`uint`。
- 冲突指引：上游若改 `parser.go` 的 addNothing 调用点，本补丁不受影响；若改 `shortcutgen.go` 的
  `generateActions` 返回值形态，需同步 `nothing.go` 里的动作图数据结构。

## 2. 文本字面量内联（P2，随 #1 一起纳入版本控制）

- 文件：`shortcutgen.go`（`attachmentValues()` 判断"不含插值"后直接内联进 Set Variable，不再多生成一个 Text 动作）
- 为什么：`@x = "纯字面量"` 本不需要一个独立的 Text 动作。
- 冲突指引：与上游对 Set Variable 生成逻辑的改动**最容易冲突**，解冲突时以"字面量内联仍然生效"为准。

## 3. richtext 强制转换支持（本提交）

- 文件：`actions/{basic,crypto,images,text}.cherri`、`shortcut.go`、`variables.go`、`cherri_test.go`、`tests/*`
- 为什么：Shortcuts 的 "Get Images"（`detect.images`）只有在输入被**显式 coercion 为富文本**
  （`WFRichTextContentItem`）时才把输入当网页渲染。没有这个 coercion，"渲染页面 → 截图"这条链路根本不生效
  （表现为：页面 HTML 被当成纯文本，取不到任何图像）。
- 冲突指引：上游若扩充类型表，保留 `richtext` → `WFRichTextContentItem` 这一项即可。

## 同步上游的方式

```bash
git fetch origin                       # origin = 上游
git log --oneline HEAD..origin/main    # 看上游新增
git merge origin/main                  # 冲突按上面「冲突指引」解
# 或 git rebase origin/main（本 fork 的提交都是独立的，rebase 也安全）
```
