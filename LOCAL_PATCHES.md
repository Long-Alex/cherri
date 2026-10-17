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

- 文件：`shortcutgen.go`（`makeVariableInput` 里"不含插值"的分支直接内联进 Set Variable，不再多生成一个 Text 动作）
- 为什么：`@x = "纯字面量"` 本不需要一个独立的 Text 动作。
- **后续修正（P2 续）**：内联的值写成显式 `WFTextTokenString{string: …}`，而不是裸 plist 字符串。
  Shortcuts 里文本值一律序列化为 `WFTextTokenString`，参考插件校验器也会把裸字符串报成
  "missing WFTextTokenString"。注意用 `map[string]any` 而不是 `WFTextTokenStringValue` 结构 ——
  后者的 `String` 字段带 `omitempty`，空串会被整个丢掉、产出 `{"Value":{}}`（更不合规）。
  同时把原来的插值探测从 `val.(map[string]any)` 改成具体类型断言：`attachmentValues()` 对插值串
  返回的是 **`WFTextTokenString` 结构体**（不是 map），所以旧探测永远不成立。
- 冲突指引：与上游对 Set Variable 生成逻辑的改动**最容易冲突**，解冲突时以"字面量内联仍然生效、
  且值是显式 `WFTextTokenString`"为准。

## 3. 空触发器键不写（P3）

- 文件：`shortcut.go`（`Shortcut.WFWorkflowTriggers` 加 `plist:",omitempty"`）
- 为什么：没有自动化触发器时，这个空数组键是纯样板；Shortcuts 视"缺键"与"空数组"等价。
  省掉后产物少一个键，参考校验器也少一条空参数告警。
- 冲突指引：上游若给 `Shortcut` 结构体加字段，保留这个 `omitempty` 标签即可。

## 4. richtext 强制转换支持

- 文件：`actions/{basic,crypto,images,text}.cherri`、`shortcut.go`、`variables.go`、`cherri_test.go`、`tests/*`
- 为什么：Shortcuts 的 "Get Images"（`detect.images`）只有在输入被**显式 coercion 为富文本**
  （`WFRichTextContentItem`）时才把输入当网页渲染。没有这个 coercion，"渲染页面 → 截图"这条链路根本不生效
  （表现为：页面 HTML 被当成纯文本，取不到任何图像）。
- 冲突指引：上游若扩充类型表，保留 `richtext` → `WFRichTextContentItem` 这一项即可。

## 5. 生成期去掉"纯转手中转"动作（P4 / P5）

- 文件：`prune_intermediates.go`（新增）、`shortcutgen.go`（`generateActions()` 之后调用
  `pruneIntermediates()`）、`nothing.go`（把"删动作 + 重映射 import question 下标"抽成共用的
  `removeActions()`）
- 为什么：cherri 为每个可变赋值生成一个 Set Variable、为每个 `getText()` 生成一个
  `detect.text`。其中相当一部分是**纯转手**——语言里没有"就在这里用这个引用"的表达式，只能先
  存进变量/转成文本再用：
  - **P4 `pruneAliasSetVariables`**：Set Variable 的唯一非空赋值就是一个引用（变量/魔法变量/
    动作输出），且所有读取都在它之后 ⇒ 读取直接指向该引用，删掉 Set Variable 与它的空初始化。
  - **P5 `pruneTextConversions`**：`detect.text` 的输出**只被消费一次**，且消费方本来就会把输入
    转成文本（Count 按 Characters 计、词典键、文件路径、或纯转手的 Set Variable）⇒ 删掉转换，
    消费方直接拿原引用。
- 安全性（都 fail-closed，无法证明就不动）：
  - 动作输出做来源时要求别名写入与**所有**读取在**同一控制流作用域**内（否则生产者可能没执行过，
    读到的是空值而不是旧值）；
  - 魔法变量（Repeat Item / Shortcut Input …）恒在作用域内，无需检查；
  - 一般变量做来源时要求它是"快照"：对该变量的每次写入要么是空初始化，要么与别名写入在
    **同一控制流块**内且在它之前（对应 `@latitudeText = getText(@latitude)` 紧跟
    `@latitude = …` 的写法）；
  - 删除前再确认"没有任何动作还在读这个变量"、"每个读取点都真的被改写"。
- 收益：`camera-watermark` 的主快捷指令 253 → 240 个动作；消费方的
  `simplify_intermediate_steps.py`（493 行、9 条按变量名硬编码的规则）已经没有活可干，已退役删除。
- 冲突指引：这两个 pass 只读 `shortcut.WFWorkflowActions` 的**结构**（ident / 参数树 / 控制流
  模式），与具体动作定义无关；上游若改 `ShortcutAction`/`Value` 的字段形态，只需要调整
  `prune_intermediates.go` 里的反射遍历（`walkValues` / `mapValues`）与 `singleRef`。
  另注意：`nothing.go` 与它共用 `removeActions()`，删动作时 import question 下标必须同步平移。

## 同步上游的方式

```bash
git fetch origin                       # origin = 上游
git log --oneline HEAD..origin/main    # 看上游新增
git merge origin/main                  # 冲突按上面「冲突指引」解
# 或 git rebase origin/main（本 fork 的提交都是独立的，rebase 也安全）
```
