# Codex Gateway

给原生 Codex CLI 添加多个 GPT 供应商。配置一次，日常继续使用 `codex`、`codex resume` 和 `codex exec`，在 `/model` 中选择模型。

使用 Go 构建为单个可执行文件，使用者不需要安装 Go 或 Python。支持 Linux/macOS 的 amd64、arm64；仍需要原生 Codex CLI。当前对接并验证 Codex 0.160.1。

## 安装

当前尚未发布 Release，以下远端安装命令将在首次发布后可用：

```bash
curl -fsSL https://github.com/Moozy23232/Codex-gateway/releases/latest/download/install.sh | sh
```

默认安装到 `~/.local/bin`，提供 `codex-gateway` 管理命令和 `codex` 包装入口。安装器会保留目标目录内已有的 Codex 启动器，后续客户端调用继续经过它；原生登录、帮助、版本查询等仍交给原程序。更新和失败回滚会保留原入口。

安装目录可以自定义：

```bash
curl -fsSL https://github.com/Moozy23232/Codex-gateway/releases/latest/download/install.sh | \
  sh -s -- --install-dir "$HOME/apps/codex/bin"
```

也支持 `CODEX_GATEWAY_INSTALL_DIR`。所选目录需要在 `PATH` 中排在其他 Codex 安装目录之前，才能直接使用包装后的 `codex`；安装器会给出提示，不自动修改 shell 配置或使用 sudo。尚未安装原生 Codex 时也能安装网关，实际运行前需先安装原生客户端。

如果 `codex` 仍指向 npm 原版，把下面这一行放到 `~/.zshrc`（Bash 则为 `~/.bashrc`）中 **nvm 等环境初始化之后**，然后重新打开终端或执行 `source ~/.zshrc`：

```bash
export PATH="$HOME/.local/bin:$PATH"
```

用 `command -v codex` 确认输出为 `~/.local/bin/codex` 对应的绝对路径。自定义安装目录时替换上面的目录；原生 Codex 不会被覆盖，仍可通过原安装路径直接启动。

安装脚本校验 SHA-256，`--version` 或 `CODEX_GATEWAY_VERSION` 可以选择已发布版本。重复执行安装命令即可升级；下载、校验或安装失败时保留原有安装。

## 官方订阅

直接沿用原生命令：

```bash
codex login
codex
codex resume
```

官方模型跟随账号自动加载，**不需要逐个执行 `add-model`**。只有官方模型时直接运行原生 Codex，不启动网关转发服务。

也可执行 `codex-gateway add-provider --official`：它会调用或复用原生 ChatGPT 登录，自动同步官方模型及默认项。已有官方登录、随后添加第三方供应商时，包装入口会自动识别该登录，并把官方模型加入选择列表。

**混合配置统一入口**：配置了第三方模型后，已注册的官方和第三方模型都通过同一个本地网关。无论启动时选择官方还是第三方，都能在 `/model` 中切换已配置模型（部分版本放在 **All models** 下），无需退出重开来切换供应商。官方模型仍按账号可用列表同步，不会把第三方模型的额度或权限当作官方权限。

混合使用官方与第三方时，官方认证由原生 Codex 管理，网关不自行实现 OAuth 刷新。桥接需要原生文件凭据；只保存在 keyring 时，可通过 `add-provider --official` 完成原生文件模式登录。需要地区专属后端的工作空间请直接使用原生入口。

## 添加第三方 GPT 供应商

```bash
codex-gateway add-provider example
codex-gateway add-model
codex
```

`add-provider` 会询问供应商的 Responses API 地址和 API key，首次使用自动初始化。终端中的 key 不回显，保存到私有文件中。`add-model` 自动选择唯一的第三方供应商，或让你选择供应商和模型；第一个模型自动设为默认。

这两个命令只保存配置，不启动常驻转发服务。之后运行 `codex`，需要已配置的网关路由时才自动启动或复用服务；也可手动执行 `codex-gateway start`。关闭 Codex 不会停止已启动的网关，停止使用 `codex-gateway stop`。

推理档位、上下文窗口等能力按具体 GPT 型号自动匹配。没有官方登录也能使用第三方供应商，程序附带与已验证 Codex 版本对应的公开模型能力信息作为后备。已有较新的官方模型信息时优先使用它。

供应商使用自定义模型名称时，只需要确认它对应哪个 GPT 型号。例如：

```bash
codex-gateway add-model provider-coder \
  --provider example --gpt-model gpt-6.1-sol
```

这里的 `provider-coder` 和 `example` 是占位名称，应替换成实际供应商的模型 ID 和已添加的供应商名称。未知 GPT 型号会要求确认或更新，不会自动套用固定的 32K 上下文。

脚本化配置可以引用已有环境变量或密钥文件：

```bash
codex-gateway add-provider example \
  --base-url https://api.example.com/v1 \
  --api-key-env EXAMPLE_API_KEY
```

`https://api.example.com/v1` 为示例域名。供应商需要支持 Codex 实际使用的 Responses、工具调用和流式事件；不进行 Chat Completions 或 Anthropic Messages 协议转换。第三方对模型能力的额外限制仍以其实际服务为准。

## 日常使用

```bash
codex
codex resume
codex resume --last
codex exec "检查当前项目"
```

配置命令用于管理供应商和模型：

```bash
codex-gateway provider list
codex-gateway model list
codex-gateway status
codex-gateway stop
```

`codex resume` / `codex fork` 使用原生选择界面，跨供应商显示当前 Codex home 中的历史，**无需添加 provider 或模型**。默认只看当前目录（Cwd），可在界面用左右键切换 Cwd / All，或显式使用 `--all`；`resume` 也包含 `exec` 创建的记录，归档记录仍通过原生状态筛选查看。浏览列表只启动临时本地历史读取进程，不启动转发服务；不会扫描其他 Codex home、迁移或批量改写历史。

选中后才按会话 ID 进入正常启动和模型路由流程，保留目录确认、profile、沙箱和审批参数。已注册模型匹配网关路由，未注册的模型交给原生 Codex；能看到历史不代表旧供应商凭据和接口仍然可用。可用 `-m` / `-c model=...` 指定模型，或进入后用 `/model` 切换。显式会话 ID/名称和 `--last` 不经过选择器，`--last` 仍按原生 provider 规则选择。跨供应商的加密推理历史不保证兼容，遇到此类错误请参考[兼容说明](docs/advanced.md#跨供应商会话兼容)或新开会话。

官方列表会自动同步并保留缓存；短暂离线时保留已有模型。新增模型或修改配置后，重新打开 Codex 即可加载更新。网关在空闲时重启应用配置；已有请求正在执行时不会强行中断。原生客户端升级、供应商模型升级或跨供应商恢复仍可能需要兼容性适配。

网关只监听本机 `127.0.0.1`。供应商 key、地址与配置保存在仓库外的私人目录中；第三方请求不携带原生账号凭据或任意客户端认证头。每个供应商默认直连，需要代理时在 `add-provider` 中指定 `--proxy`。

## 开发与验证

```bash
mise trust
mise install
mise exec -- bash scripts/build.sh
mise exec -- bash scripts/check.sh
```

Go 版本由 `mise.toml` 固定，依赖由 Go Modules 管理。已有 Go 时也可直接执行这些脚本；构建使用 `CGO_ENABLED=0`。输出默认位于 `dist/<系统>_<架构>/codex-gateway`。源码中用 `bash scripts/run.sh --help` 查看管理命令，不会自动安装到本机 PATH。

检查记录归档在唯一的 `runs/_tests/` 子目录，包含模块校验、静态检查、race 测试、安装回滚、模拟上游和后台生命周期。设置 `CODEX_GATEWAY_TEST_CODEX_BIN=/absolute/path/to/native/codex` 可启用原生客户端的隔离集成检查；测试不用个人账号或真实供应商。

公开模型能力后备取自 OpenAI Codex 的对应版本源代码，来源版本与上游许可保存在 `internal/gateway/native_models_source.txt`、相邻 LICENSE/NOTICE 文件中，也可运行 `codex-gateway --licenses` 查看。

本地打包四个平台：

```bash
VERSION=0.2.0 bash scripts/package.sh
```

此命令只打包，不发布。手动 Release workflow 仅生成 artifact；发布需要另行推送符合 main 祖先检查的版本 tag。完整说明见 [高级配置与发布](docs/advanced.md)。本项目不安装开机服务，机器重启后再次运行 `codex` 会按需启动网关。
