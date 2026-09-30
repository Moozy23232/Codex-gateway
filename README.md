# Codex Gateway

给 Codex CLI 使用的本地多供应商网关。把模型别名映射到不同供应商的 Responses API，在同一个 `/model` 菜单里选择模型；供应商地址、密钥引用、代理和模型目录由独立配置管理。

本分支使用 **Go** 实现。构建得到一个 `codex-gateway` 可执行文件，使用者不需要安装 Python 或 Go；仍需已安装的 Codex CLI。支持 Linux/macOS，暂不支持原生 Windows。项目沿用 Codex CLI **0.159.2** 的配置与协议，后续客户端版本可能需要适配。

网关仅监听 `127.0.0.1`。它转发 Responses 请求，不做 Anthropic Messages 或 Chat Completions 协议转换。供应商必须支持 Codex 实际使用的 Responses 请求、工具调用和流式事件。

API key 供应商使用 Bearer 认证，只接收白名单内的协议请求头；客户端 Cookie、账号标识和任意自定义认证头不会原样转发。Responses URL 的查询参数也不受支持，包括在 base URL 中附加参数。依赖专用请求头或 `?api-version=...` 等查询参数的入口，需要先提供兼容的前置适配服务。

## 安装

当前还没有发布 Release，下面的远端安装入口将在首次正式发布后可用。安装脚本自动识别 Linux/macOS 和 amd64/arm64，下载对应二进制并校验 SHA256；使用者不需要 Go、Python 或 mise。

```bash
curl -fsSL https://github.com/Moozy23232/Codex-gateway/releases/latest/download/install.sh | sh
```

默认安装为 `~/.local/bin/codex-gateway`。安装目录可以自定义，命令行参数优先于环境变量；包含空格的路径需要加引号：

```bash
curl -fsSL https://github.com/Moozy23232/Codex-gateway/releases/latest/download/install.sh | \
  sh -s -- --install-dir "$HOME/apps/codex/bin"

curl -fsSL https://github.com/Moozy23232/Codex-gateway/releases/latest/download/install.sh | \
  CODEX_GATEWAY_INSTALL_DIR="$HOME/apps/codex/bin" sh
```

也可以用 `--version 0.2.0` 或 `CODEX_GATEWAY_VERSION=0.2.0` 选择版本（此处版本仅作示例）。默认最新版会先解析为一个固定 tag，二进制和校验文件都从该 tag 下载。指定的版本必须已有完整 Release。

安装脚本只替换所选目录下的 `codex-gateway`，按当前用户的权限安装，不自动使用 `sudo`，也不修改 shell 配置或供应商配置。请选择自己有写权限的目录。下载或校验失败时保留已有程序。如果安装目录尚未在 `PATH` 中，脚本会给出提示；也可以用该程序的绝对路径运行。重新执行安装命令即可升级。

源码中的安装脚本可以直接查看选项，此命令不发网络请求：

```bash
sh scripts/install.sh --help
```

## 快速开始

安装后，第三方供应商只需要三步：

```bash
codex-gateway add-provider example
codex-gateway add-model
codex-gateway
```

`add-provider` 会询问 Responses API 的 base URL 和 API key；终端中的 key 输入不回显，保存为配置目录下权限 `0600` 的私有文件。省略供应商名时也会询问。首次添加自动初始化，不必先执行 `init`。

`add-model` 在只有一个供应商时直接使用它，否则让你选择。随后尝试列出模型，也可以直接输入模型 ID；供应商没有模型列表接口时仍可手工添加。模型目录自动生成，第一个模型自动设为默认。最后直接运行 `codex-gateway`，即可启动网关并进入 Codex，在 `/model` 中选择已添加的模型。

已知模型 ID 时可以省略列表请求，指定别名也可选：

```bash
codex-gateway add-model replace-with-provider-model-id --provider example
codex-gateway add-model replace-with-provider-model-id \
  --provider example --alias example/coding --default
```

需要脚本化配置时，通过环境变量或现有私有文件提供 key 的引用：

```bash
# EXAMPLE_API_KEY 应事先从你的私有环境加载。
codex-gateway add-provider example \
  --base-url https://api.example.com/v1 \
  --api-key-env EXAMPLE_API_KEY
codex-gateway add-model replace-with-provider-model-id --provider example
```

`https://api.example.com/v1` 和模型 ID 都是占位值；网关向 base URL 下的 `/responses` 转发请求。重复添加其他供应商和模型即可。同名条目需要明确使用 `--replace`，并提供完整的新参数。

### 官方订阅

添加官方入口时使用 `--official`，无需填写 URL 或 API key：

```bash
codex-gateway add-provider --official
codex-gateway add-model --provider official
codex-gateway
```

默认供应商名为 `official`，也可在命令中指定其他名字。网关复用所选 Codex home 中的 ChatGPT 文件登录；没有时调用原生 `codex login`，完成订阅账号登录。Codex home 使用 `CODEX_HOME`，未设置时为 `~/.codex`；提前使用 `init --codex-home ...` 可以选择其他目录。

新配置使用本地 `token` 认证，官方与第三方模型可以混合添加。只有请求官方模型时才读取 ChatGPT 凭据并让原生 Codex 处理刷新，第三方模型无需 ChatGPT 登录。网关不自行实现 OAuth 刷新，也不把 ChatGPT token 写进网关配置。官方 token 仅发往固定官方端点。

此桥接需要原生 `auth.json` 文件凭据。如果原来只存于系统 keyring，`--official` 会调用原生登录，以仅影响该次命令的文件存储选项重新登录；不会改写全局 `config.toml`。要求地区专属后端的工作空间暂不支持此桥接，需要使用原生 Codex。开发测试不使用个人账号登录或真实模型请求。

原有 `init` / `provider add` / `model add` / `catalog` 命令仍可使用。高级 `init` 的默认值继续是 `--auth-mode codex`，已有配置不会自动迁移。`codex` 模式沿用内置 `openai` provider 的历史归属；新 `token` 模式使用 `codex-gateway` provider，历史归属不同。若需要延续原有官方对话，可继续使用已有的 `codex` 模式配置。

### 模型能力与代理

自动目录优先复用模型 ID 完全匹配的已有元数据；没有匹配时，生成通用文本配置，不提供可选推理档位或图片输入。Codex 0.159.2 在此配置下仍会发送 `reasoning.effort=none`，上游需要接受该值。通用配置使用 32,000 token 的客户端预算，并在 28,000 token 左右触发自动压缩；这只是保守的默认预算，不代表供应商实际支持的上下文长度。应按模型真实能力覆盖：

```bash
codex-gateway add-model replace-with-provider-model-id --provider example \
  --context-window 64000 --reasoning-effort medium
```

也可以使用 `--template <slug>` 明确选择兼容模板，或通过 `catalog import <file>` 导入元数据。能力参数只影响客户端行为，不会增加上游能力；模型仍须支持 Codex 的 Responses、工具调用和流式协议。改变已添加模型的设置时加 `--replace`。

原生登录与账号刷新需要代理时，可使用终端代理环境变量；也可先 `init --auth-mode token --bootstrap-proxy http://127.0.0.1:7890`，或对现有配置执行 `config set bootstrap_proxy http://127.0.0.1:7890`。此设置补齐原生子进程缺失的代理环境变量，不覆盖已设置的值。模型请求使用供应商自己的 `--proxy`，例如 `add-provider --official --proxy http://127.0.0.1:7890`；第三方默认直连。

## 配置与密钥

配置目录按以下优先级选择，`--home` 放在子命令前：

1. `codex-gateway --home /absolute/path/to/gateway ...`
2. 环境变量 `CODEX_GATEWAY_HOME`
3. `$XDG_CONFIG_HOME/codex-gateway`，未设置时为 `~/.config/codex-gateway`

首次 `add-provider` 自动初始化空目录；显式 `init` 拒绝覆盖非空目录，初始 provider 和 model 列表均为空。配置只保存在网关私有目录，不修改 Codex 本体或全局 `config.toml`。每个配置目录应使用不同端口；例如初始化第二个同时运行的网关时先执行 `init --auth-mode token --port 33990`。

| 文件 | 用途 |
| --- | --- |
| `config.json` | 供应商、模型路由及进程配置，密钥使用引用 |
| `templates.json` | 自动生成或导入的 Codex 模型元数据模板 |
| `models.json` | 按别名生成、传给 Codex 的模型目录 |
| `keys/` | 交互输入的供应商 API key，文件权限 `0600` |
| `client-token` | token 模式下 Codex 到本地网关的认证 |
| `admin-token` | 本地网关状态查询和停止操作的认证 |
| `runtime/` | 运行状态、生命周期锁和 `server.log` |

实际供应商配置应放在仓库外的私人目录；不要把真实供应商地址、模型映射、令牌或密钥加入 Git。[examples/config.json](examples/config.json) 只展示配置结构；其中路径、模型 ID 和模板名均是占位值，不应直接覆盖已经初始化的配置。

除了交互输入，API key 也支持环境变量或现有私有文件，二者选择其一：

```bash
codex-gateway add-provider example-file \
  --base-url https://api.example.com/v1 \
  --api-key-file /absolute/path/to/private-api-key
```

密钥文件只包含 key，建议权限为 `0600`。不要把 key 写进 base URL 或命令参数。`config show` 和 `provider list` 输出引用，不输出密钥值。本地 `client-token` 和 `admin-token` 不会转发给上游；API key 由网关按供应商引用读取。

每个供应商默认直连，不继承终端的 HTTP 代理；需要代理时显式添加 `--proxy`。显式供应商代理优先于终端的 `NO_PROXY` / `no_proxy`，不会被它们绕过。远程 HTTP 地址默认拒绝，只有明确用于可信网络时才添加 `--allow-insecure-http`；HTTPS 和 loopback HTTP 不需要该参数。

更新已有供应商或模型使用 `--replace`，并重新提供该条目的完整参数。删除供应商前必须先移除引用它的模型别名。

```bash
codex-gateway provider list
codex-gateway model list
codex-gateway config show
codex-gateway config set default_model example/coding
codex-gateway catalog build
codex-gateway validate --credentials
```

`catalog import` 按 `slug` 合并模板，同名模板由导入内容替换；`catalog build` 根据当前路由重新生成模型目录。CLI 修改配置时也会重新生成目录。

## 启动、恢复与停止

```bash
codex-gateway start
codex-gateway status
codex-gateway run
codex-gateway run -- resume
codex-gateway stop
```

无参数的 `codex-gateway` 等同于 `codex-gateway run`。`start` 启动或复用这个配置目录对应的后台网关；`run` 先确保网关运行，再调用 `PATH` 中的 `codex`。已有的 Codex launcher 和备份入口仍通过这条调用链运行，也可以用 `run --codex-bin /absolute/path/to/codex` 选择本次客户端的可执行文件。Codex 参数放在 `run --` 后，例如 `run -- resume <session-id>`。

首次调用原生客户端时，如果所选 Codex home 尚不存在，会创建该目录；已有目录的权限、配置和登录文件保持原样。官方登录或凭据刷新产生的认证更新由原生 Codex 负责。

如果需要同时为客户端、官方登录、账号刷新和模型发现选择原生可执行文件，启动网关前设置 `CODEX_GATEWAY_CODEX_BIN=/absolute/path/to/codex`。该环境变量优先于 `PATH`，单次 `run --codex-bin` 则优先于它。已运行的网关不会自动继承新环境变量，需要先停止空闲网关再启动。

`run` 会把用户的 `-c` / `--config` 参数与网关配置放到同一层，用户参数优先；字面 `--` 后的提示内容保持原样。这兼容 Codex 0.159.2 中子命令后置 `-c` 替换前置配置列表的行为，因此 `run -- exec -c model_reasoning_effort=low ...` 也能保留网关设置。

修改网关配置后，下一次 `start` 或 `run` 会在无活跃请求时重启网关。有正在处理的请求时会报错，等待其结束后重试；`stop` 同样拒绝中断活跃请求。已经打开的 Codex 窗口不会自动重载启动时传入的模型目录和默认模型，修改后重新运行 Codex。

更新 API key 环境变量后，执行 `stop`，再从带有新变量的终端执行 `start` 或 `run`，让新网关进程继承变量。仅重复 `start` 不会把当前终端环境注入已有进程。

需要在前台排查网关时，先停掉同一端口的后台实例，再运行：

```bash
codex-gateway serve
```

后台日志在配置目录的 `runtime/server.log`。`validate` 检查本地配置和目录；`validate --credentials` 还检查密钥引用能否读取，**不验证上游可达性、额度或模型质量**。

## Codex 的 embedded mode 提示

`run` 用本次进程的 `-c` 参数设置模型目录、provider 和本地地址。因此，支持 shared background server 的 Codex 版本可能显示：

```text
Running without the shared background server: command-line configuration overrides (...) requires embedded mode.
```

这是运行模式提示。正常开着 Codex 使用时可以聊天、调用工具和切换模型；前台 Codex 进程退出后，当前 agent 任务不会由共享后台继续执行。网关 daemon 只是请求转发服务，不能代替 Codex agent 后台。重新打开后可用 `run -- resume` 恢复已保存的会话。

本项目不安装系统开机服务。机器重启后执行 `run` 会重新启动网关；原有 Codex 自动备份由原启动入口负责，网关不接管备份。

## 跨供应商会话兼容

不同供应商可能无法读取彼此生成的加密 reasoning。默认保留原始请求并返回上游错误，不猜测未知失败原因。

对于上游明确返回的 `invalid_encrypted_content`，提供默认关闭的兼容开关：

```bash
codex-gateway config set retry_invalid_encrypted_reasoning true
```

开启后仅在 HTTP 400 错误明确点名某个加密 reasoning 项时，移除该项并做有限次数的重试；不会修改已保存的原会话，也不会删除 compaction 项。它不保证所有上游之间的历史兼容，更不修复任意认证、网络或模型错误。无法兼容时，应为对应供应商开启新会话。

## 构建与使用

开发者可以先在本机构建，或把对应系统和架构的二进制分发给使用者。

开发环境使用 `mise` 管理 Go 版本，项目 `mise.toml` 固定 Go 1.27.1；依赖由 Go Modules 管理，版本和校验值保存在 `go.mod` / `go.sum`。安装 mise 后，在仓库目录运行：

```bash
mise trust
mise install
mise exec -- bash scripts/build.sh
```

如果 Go 已在当前终端的 PATH 中，直接运行 `bash scripts/build.sh` 即可。默认输出 `dist/<系统>_<架构>/codex-gateway`，例如 Linux x86_64 为 `dist/linux_amd64/codex-gateway`。TOML 解析和终端输入依赖都编译进程序；构建使用 `CGO_ENABLED=0`。

把二进制放入 PATH 中的目录后：

```bash
codex-gateway --version
codex-gateway --help
```

源码开发可以一条命令构建并运行：

```bash
bash scripts/run.sh --help
bash scripts/run.sh --home /absolute/path/to/private-gateway run
```

`run.sh` 会先构建持久二进制，再运行它。无参数执行时默认启动 `run`；以下命令中的 `codex-gateway` 均可替换为 `bash scripts/run.sh`。

交叉构建使用同一入口：

```bash
GOOS=linux GOARCH=arm64 bash scripts/build.sh
GOOS=darwin GOARCH=arm64 bash scripts/build.sh
```

## 打包与 Release

仅打包到本地，不创建 tag、不上传文件：

```bash
VERSION=0.2.0 bash scripts/package.sh
```

打包入口面向 Linux，需要 GNU tar 和 GNU coreutils；macOS 用户可以使用源码构建入口或预编译程序。默认生成 `dist/releases/v0.2.0/`，包括四个平台的 `codex-gateway_<系统>_<架构>.tar.gz`、`install.sh` 和 `SHA256SUMS`。压缩包只包含根目录的可执行文件，二进制 `--version` 使用传入的发布版本。输出目录已存在时会拒绝覆盖；`OUTPUT_DIR` 可指定新目录，`TARGETS` 可选平台子集：

```bash
VERSION=0.2.0 TARGETS='linux/amd64' OUTPUT_DIR=/absolute/path/to/new-package \
  bash scripts/package.sh
```

`.github/workflows/release.yml` 提供两种入口：

- **手动运行 workflow**：输入版本号，只测试、构建并保存 Actions artifact，供检查，不发布 Release。
- **推送 `v*` tag**：检查 tag 对应提交已在 `main` 上，测试通过后构建四个平台，上传 Release 草稿的附件，再公开该 Release。预发布版本会标记为 prerelease。

工作流会再次核对安装包校验值，并使用仓库提供的 `GITHUB_TOKEN` 发布，不需要供应商 key 或个人开发机配置。已有同名 Release 不会被覆盖。

准备正式发布时，在代码合并、确认版本之后才执行下面的示例；这会触发公开发布：

```bash
git tag -a v0.2.0 -m "Release v0.2.0"
git push origin v0.2.0
```

## 开发检查

代码和文档开发在独立 Git worktree 中进行。统一验证入口：

```bash
mise exec -- bash scripts/check.sh
```

脚本运行 `go mod verify`、`go vet ./...`、`go test -race ./...`，然后构建并运行真实二进制。每次建立唯一的 `runs/_tests/check_<UTC时间>_<随机后缀>/`，保存工具链版本、依赖快照、测试日志、构建产物和退出码。

测试使用本地模拟上游、假的 Codex 启动器与独立配置目录，检查路由、流式转发、凭据隔离、后台生命周期以及参数合并。交互配置测试覆盖自动初始化、取消不写入、密钥权限、模型选择、默认模型和终端输入恢复；官方认证使用模拟账号测试刷新及重试，不调用个人账号。

可通过 `CODEX_GATEWAY_TEST_CODEX_BIN=/absolute/path/to/native/codex` 额外启用原生客户端检查。请指向原生可执行文件；测试从不存在的 Codex home 开始，自动添加本地模拟供应商及模型，验证原生客户端收到流式回复，并检查模型路由、推理字段和文本能力。这些测试不读取个人登录。Go 分支不沿用此前 Python 实现的真实供应商验证结论；真实上游的可用性、跨供应商会话兼容和模型质量需要另行验证。

安装与打包测试还覆盖自定义目录、参数优先级、指定版本、下载或校验失败时保留旧程序、重复打包的字节一致性，以及真实二进制经本地 Release 服务安装的完整链路。网关、安装器和打包模块的全量 `-race` 检查已通过；Release 工作流通过 actionlint 静态检查和版本条件检查，尚未在 GitHub 执行发布。

2026-09-30 使用 Go 1.27.1 在 Linux amd64 完成依赖校验、`go vet`、全量 `-race` 测试和独立二进制检查。测试子进程的 `PATH` 不含 Go/Python，仍能启动网关、转发模拟 SSE 请求并保留假 Codex 的退出码。另用原生 Codex 0.159.2 从不存在的配置目录开始，验证自动配置后的首次启动及本地模拟 SSE 回复；通用模型实际发送 `reasoning.effort=none`。Linux amd64/arm64、macOS amd64/arm64 四个目标均编译成功；其他架构产物只做了交叉编译，实际运行验证限于 Linux amd64。

供应商地址、模型映射、API key 和真实请求日志都属于私人配置，应保存在仓库外。仓库只包含通用示例和本地模拟测试。
