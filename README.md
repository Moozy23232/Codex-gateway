# Codex Gateway

为原生 Codex CLI 添加**多供应商模型路由**和**跨供应商历史列表**，日常仍使用 `codex`、`codex resume` 和 `codex exec`。

- **统一模型菜单**：在原生 `/model` 中切换已注册的官方与第三方 GPT 模型，网关按模型分流。
- **原生历史界面**：无需配置网关供应商或模型，也能查看当前 Codex home 中跨供应商的历史；默认当前目录，保留 Cwd / All 切换。
- **按需转发**：仅官方配置保持原生直连；使用网关路由时自动启动本地转发服务。

支持 Linux / macOS 的 amd64、arm64，已提供[预编译 Release](https://github.com/Moozy23232/Codex-gateway/releases/latest)，使用者无需 Go 或 Python。仍需安装[原生 Codex CLI](https://github.com/openai/codex)，npm、官方安装脚本或独立二进制均可；当前验证版本为 **Codex 0.160.1**。

## 安装与升级

下面的命令默认安装 **Latest 版本**，重复执行即可升级，不需要指定版本号：

```bash
curl -fsSL https://github.com/Moozy23232/Codex-gateway/releases/latest/download/install.sh | sh
```

默认安装到 `~/.local/bin`，提供 `codex-gateway` 管理命令和 `codex` 包装入口。安装器校验 SHA-256，不使用 sudo，不自动修改 shell 配置；下载、校验或安装失败时保留原有安装。

让包装入口优先于其他 Codex 安装目录：

```bash
export PATH="$HOME/.local/bin:$PATH"
command -v codex
```

输出应为 `$HOME/.local/bin/codex` 对应的绝对路径。将 `export` 行放到 `~/.zshrc`（Bash 为 `~/.bashrc`）的 **nvm 等环境初始化之后**，再重新打开终端。自定义安装目录时替换上述路径。

安装器保留并调用原生入口。若目标目录已经存在 `codex`，会备份该入口再安装包装器，不丢失原程序；原生登录、帮助和版本查询仍交给原客户端。尚未安装原生 Codex 时也能安装网关，但运行前需先安装原生客户端。

可选安装参数：将命令末尾的 `sh` 换为 `sh -s -- <参数>`。

| 参数 | 用途 |
| --- | --- |
| `--install-dir "$HOME/apps/codex/bin"` | 自定义安装目录，也支持 `CODEX_GATEWAY_INSTALL_DIR` |
| `--version v0.3.0` | 锁定或回退到指定版本，也支持 `CODEX_GATEWAY_VERSION`；日常安装无需设置 |

## 开始使用

下面三种方式按需选择，官方订阅与第三方 API 也可以混合使用。

### 只查看已有历史

```bash
codex resume
```

**不需要先执行 `add-provider` 或 `add-model`**，也不需要为了浏览列表登录或填写旧供应商的 key。历史范围和继续对话的要求见[恢复会话](#恢复会话)。

### 官方订阅

```bash
codex login     # 已登录可跳过
codex
```

沿用原生 ChatGPT 登录和账号可用的官方模型，**无需逐个执行 `add-model`**。只有官方配置时直接使用原生 Codex，不启动转发服务。

也可执行 `codex-gateway add-provider --official`，复用或完成原生登录并同步官方模型。已有官方登录、随后添加第三方时，包装入口会自动识别登录并尝试将官方模型加入菜单。

### 第三方 GPT API

```bash
codex-gateway add-provider example
codex-gateway add-model
codex
```

- `example` 是你给供应商起的名称。`add-provider` 会询问 **Responses API base URL** 和 API key，首次使用自动初始化。地址按供应商文档填写，例如 `https://api.example.com/v1`，不要误填返回网页的官网首页。
- 交互输入的 key 不回显，保存到私有文件；也支持 `--api-key-env` 或 `--api-key-file`，见[配置与密钥](docs/advanced.md#配置与密钥)。
- `add-model` 自动选择唯一的第三方供应商，或让你选择供应商和模型；尚无默认模型时，首次添加会设为默认。使用 `--default` 可指定新的默认模型。
- 推理档位、上下文等能力按 GPT 型号匹配，无官方登录时使用附带的公开模型能力后备。自定义模型名可用 `--gpt-model` 指定对应 GPT 型号；未知型号不会被静默套用固定的 32K 上下文。

进入 Codex 后用 `/model` 切换已配置模型，部分版本需展开 **All models**。混合配置下，已注册的官方和第三方模型统一经过网关，无需退出重开来切换供应商；官方权限和额度仍以账号为准。

## 选择模型与设置默认值

配置过网关后，可以查看模型、临时选择模型，或修改后续新会话的默认值：

```bash
codex-gateway model list                     # 查看已注册的模型别名
codex-gateway config show                    # 查看配置中的 default_model
codex -m example/coding                      # 仅本次启动指定模型
codex-gateway config set default_model example/coding
```

`example/coding` 只是示例，请替换为 `model list` 输出中的模型别名（JSON 的键），不是供应商名称或未注册的上游模型 ID。

- **当前会话**：用 `/model` 切换；修改网关默认值不会切换已经打开的会话。
- **后续新会话**：`config set default_model` 设置的值在下次启动时生效；显式 `-m` 优先。网关通过启动参数覆盖原生默认，不改写 `~/.codex/config.toml` 中的 `model`；恢复旧会话时可能沿用历史模型。
- **初始默认值**：只安装、未配置网关模型时沿用原生配置。尚无网关默认值时，首次 `add-model` 会把添加的模型设为默认；先执行 `add-provider --official` 并同步成功，则采用官方模型列表标记的默认项，不保证等于原生配置中的选择。已有默认值时，后续添加模型会保留它，除非指定 `--default`；原默认模型被移除时可能重新选择。

## 恢复会话

```bash
codex resume          # 原生选择器，默认当前目录（Cwd）
codex resume --all    # 默认所有目录（All）
codex fork           # 原生选择器，选择历史并创建分支会话
codex resume --last   # 原生最近会话路径，不经过增强选择器
```

- 列表跨供应商显示历史，可用左右键切换 **Cwd / All**；搜索、分页、预览和归档筛选保留原生行为，`resume` 也包含 `exec` 创建的记录。
- 只读取当前 Codex home，通常为 `~/.codex`；无网关配置时遵循 `CODEX_HOME`，已有配置时使用其中的 `codex_home`。不会自动合并其他 home、迁移或批量改写历史。
- 浏览列表仅启动临时本地历史读取进程，不启动转发服务、不请求模型。选中后才进入正常会话和路由流程，保留目录确认、profile、沙箱与审批参数。
- **看得见历史不等于旧接口和凭据仍然可用。** 继续对话需要有效的原生配置或网关路由及凭据；可用 `-m` / `-c model=...` 指定模型，或进入后用 `/model` 切换。
- 显式会话 ID / 名称和 `--last` 保留原生路径；`--last` 仍有原生 provider 筛选。跨供应商的加密推理历史不保证兼容，详见[跨供应商会话兼容](docs/advanced.md#跨供应商会话兼容)。

## 日常使用与服务管理

```bash
codex
codex exec "检查当前项目"
codex-gateway provider list
codex-gateway model list
codex-gateway status
codex-gateway stop
```

- `add-provider` / `add-model` **只保存配置，不启动服务**。运行 `codex` 并匹配网关路由时，才自动启动或复用后台转发服务；也可手动执行 `codex-gateway start`。
- 退出 Codex 后，已启动的网关继续运行。`stop` 停止空闲网关，不会强行中断活跃请求；本项目不安装开机服务，机器重启后再次运行 `codex` 会按需启动。
- 新增模型或修改配置后，重新打开 Codex。网关在无活跃请求时重启应用配置；已有请求执行中则等待结束后重试。官方模型自动同步并保留缓存，短暂离线时保留已有模型。

## 兼容性与配置边界

- 第三方必须支持 Codex 使用的 **Responses API、工具调用和流式事件**；不转换 Chat Completions 或 Anthropic Messages。第三方能力限制以其实际服务为准。
- 网关只监听 `127.0.0.1`。配置默认在 `~/.config/codex-gateway`，密钥保存在私有目录或通过引用读取，不应提交到 Git；第三方请求不携带原生账号凭据或任意客户端认证头。目录覆盖与密钥权限见[高级配置](docs/advanced.md#配置与密钥)。
- 每个供应商默认直连，**不继承终端 HTTP 代理**；需要代理时在 `add-provider` 中显式传入 `--proxy URL`。
- 混合使用官方订阅时，认证刷新由原生 Codex 管理，桥接需要文件凭据。仅保存在 keyring 时，可通过 `add-provider --official` 完成原生文件模式登录；需要地区专属后端的工作空间请使用原生入口。
- 历史增强依赖原生 app-server 协议，已验证 Codex 0.160.1；升级原生客户端或供应商模型后可能需要兼容性适配。其他入口、自定义模型能力与排错见[高级配置与兼容命令](docs/advanced.md)。

## 开发与验证

```bash
mise trust
mise install
mise exec -- bash scripts/build.sh
mise exec -- bash scripts/check.sh
```

Go 版本由 `go.mod` 和 `mise.toml` 固定；已有对应版本 Go 时可直接执行脚本。构建使用 `CGO_ENABLED=0`，默认输出到 `dist/<系统>_<架构>/codex-gateway`；源码中用 `bash scripts/run.sh --help` 查看管理命令，不自动安装到 PATH。

检查记录写入 `runs/_tests/`，覆盖模块校验、静态检查、race 测试、安装回滚、模拟上游与后台生命周期。设置 `CODEX_GATEWAY_TEST_CODEX_BIN=/absolute/path/to/native/codex` 可启用原生客户端的隔离集成检查，测试不使用个人账号或真实供应商。

[打包与 Release](docs/advanced.md#打包与-release) 说明多平台构建和发布流程。公开模型能力后备的来源与许可见 `internal/gateway/native_models_source.txt` 及相邻 LICENSE / NOTICE，也可运行 `codex-gateway --licenses` 查看。
