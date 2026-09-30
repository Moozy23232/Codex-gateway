# Codex Gateway

给 Codex CLI 使用的本地多供应商网关。把模型别名映射到不同供应商的 Responses API，在同一个 `/model` 菜单里选择模型；供应商地址、密钥引用、代理和模型目录由独立配置管理。

运行时仅依赖 Python 标准库，需要 **Python 3.11+** 和已安装的 Codex CLI。进程管理使用 `fcntl`，面向 Linux/macOS；不支持原生 Windows。项目基于 Codex CLI **0.159.2** 的配置和协议开发，后续版本的接口变化可能需要适配。

网关仅监听 `127.0.0.1`。它转发 Responses 请求，不做 Anthropic Messages 或 Chat Completions 协议转换。供应商必须支持 Codex 实际使用的 Responses 请求、工具调用和流式事件。

API key 供应商使用 Bearer 认证，只接收白名单内的协议请求头；客户端 Cookie、账号标识和任意自定义认证头不会原样转发。Responses URL 的查询参数也不受支持，包括在 base URL 中附加参数。依赖专用请求头或 `?api-version=...` 等查询参数的入口，需要先提供兼容的前置适配服务。

## 安装与源码运行

在仓库目录中安装到当前 Python 环境：

```bash
python3 -m pip install .
codex-gateway --help
```

如果已安装 pipx，也可以用 `pipx install .` 创建隔离环境。项目尚未承诺 PyPI 发布，上面的 `.` 表示安装当前检出目录。

直接从源码运行无需安装：

```bash
bash scripts/run.sh --help
bash scripts/run.sh --version
```

`scripts/run.sh` 把仓库的绝对 `src/` 路径加入 `PYTHONPATH`，转发全部参数；完成下方配置后，无参数执行 `bash scripts/run.sh` 即可启动。以下命令中的 `codex-gateway` 均可替换为 `bash scripts/run.sh`。

## 快速开始：仅使用 API 供应商

这套流程使用 `token` 模式，无需 ChatGPT 登录。先准备供应商 API key，并通过当前终端的环境变量 `EXAMPLE_API_KEY` 提供；网关配置只保存变量名。还需要一份与你的 Codex 版本匹配的模型目录，作为客户端元数据模板。

```bash
codex-gateway init --auth-mode token
codex-gateway catalog list
```

`init` 默认从 Codex home 的 `models_cache.json` 读取模板；没有缓存时，尝试读取其 `config.toml` 中 `model_catalog_json` 指向的文件。Codex home 按 `--codex-home`、`CODEX_HOME`、`~/.codex` 的顺序选择。

如果 `catalog list` 输出为空，先导入已有的 Codex 模型目录，再继续：

```bash
codex-gateway catalog import /absolute/path/to/models_cache.json
codex-gateway catalog list
```

也可以在初始化时用 `init --auth-mode token --catalog /absolute/path/to/models_cache.json` 指定目录。仓库不打包个人模型缓存；目录必须包含 `models` 数组，每个模型有 `slug`，以及当前 Codex 所需的能力字段。

把下列占位值换成供应商的真实 API 地址、模型 ID，以及 `catalog list` 中能力相符的 `slug`：

```bash
codex-gateway provider add example \
  --base-url https://api.example.com/v1 \
  --api-key-env EXAMPLE_API_KEY

codex-gateway model add example/coding \
  --provider example \
  --upstream-model replace-with-provider-model-id \
  --template replace-with-catalog-slug \
  --display-name 'Example Coding' \
  --default

codex-gateway validate --credentials
codex-gateway run
```

`https://api.example.com/v1` 是示例域名，不是可用的供应商服务。网关会向该 base URL 下的 `/responses` 转发请求。

模型别名 `example/coding` 是 Codex 菜单和对话中使用的名字；`--upstream-model` 是发送给供应商的真实模型 ID。`--template` 复制已有模型的客户端元数据，包括上下文窗口、推理档位和工具能力；它**不会扩大上游模型的实际上下文或能力**。选错模板可能导致上游拒绝请求，必须按供应商真实能力选择。

添加第二个供应商时重复 `provider add` 和 `model add`，使用不同供应商名及模型别名即可。多个别名也可以映射到同一个供应商。

## 使用 Codex 原生账号与官方入口

`init` 不带 `--auth-mode` 时默认使用 `codex` 模式，保留 Codex 原生登录和 OAuth 生命周期。它使用内置 `openai` provider 的历史归属，让官方入口和第三方别名可以出现在同一套 Codex 历史中。网关不会自行刷新或复制账号凭据。

`token` 模式使用独立的 `codex-gateway` provider，由 launcher 把本地 `client-token` 传给 Codex，不需要 ChatGPT 账号。它与 `openai` provider 的历史归属不同；需要延续原有官方对话时，应使用 `codex` 模式。切换供应商是否能继续处理已有会话，仍取决于各上游对历史内容的兼容性。

下面用独立配置目录建立原生账号模式，避免覆盖前面的 API-only 配置。先确保所选 Codex home 已完成原生登录：

```bash
codex-gateway --home ~/.config/codex-gateway-native init --port 33990
codex-gateway --home ~/.config/codex-gateway-native provider add official \
  --base-url https://chatgpt.com/backend-api/codex \
  --auth codex
codex-gateway --home ~/.config/codex-gateway-native catalog list
codex-gateway --home ~/.config/codex-gateway-native model add official/coding \
  --provider official \
  --upstream-model replace-with-official-model-id \
  --template replace-with-catalog-slug \
  --default
codex-gateway --home ~/.config/codex-gateway-native run
```

这个目录也能通过 `provider add ... --api-key-env ...` 添加第三方供应商。`--auth codex` 的供应商只能用于 `codex` 模式。

如果官方账号初始化需要代理，可在 `init` 时提供 `--bootstrap-proxy http://127.0.0.1:7890`，或初始化后设置：

```bash
codex-gateway --home ~/.config/codex-gateway-native config set \
  bootstrap_proxy http://127.0.0.1:7890
```

`bootstrap_proxy` 为 Codex 子进程补齐缺失的代理环境变量，不覆盖已设置的代理。它与供应商代理分别配置：官方模型请求也需要代理时，给该供应商添加 `--proxy http://127.0.0.1:7890`；第三方供应商仍按自己的代理设置连接。

## 配置与密钥

配置目录按以下优先级选择，`--home` 放在子命令前：

1. `codex-gateway --home /absolute/path/to/gateway ...`
2. 环境变量 `CODEX_GATEWAY_HOME`
3. `$XDG_CONFIG_HOME/codex-gateway`，未设置时为 `~/.config/codex-gateway`

`init` 拒绝覆盖非空目录，初始 provider 和 model 列表均为空。它创建网关私有配置，不修改 Codex 本体或全局 `config.toml`。每个配置目录应使用不同端口；例如初始化第二个同时运行的网关时加 `--port 33990`。

| 文件 | 用途 |
| --- | --- |
| `config.json` | 供应商、模型路由及进程配置，密钥使用引用 |
| `templates.json` | 导入的 Codex 模型元数据模板 |
| `models.json` | 按别名生成、传给 Codex 的模型目录 |
| `client-token` | token 模式下 Codex 到本地网关的认证 |
| `admin-token` | 本地网关状态查询和停止操作的认证 |
| `runtime/` | 运行状态、生命周期锁和 `server.log` |

实际供应商配置应放在仓库外的私人目录；不要把真实供应商地址、模型映射、令牌或密钥加入 Git。[examples/config.json](examples/config.json) 只展示配置结构；其中路径、模型 ID 和模板名均是占位值，不应直接覆盖已经初始化的配置。

API key 支持环境变量或私有文件，二者选择其一：

```bash
codex-gateway provider add example-file \
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

`start` 启动或复用这个配置目录对应的后台网关；`run` 先确保网关运行，再调用 `PATH` 中的 `codex`。已有的 Codex launcher 和备份入口仍通过这条调用链运行，也可以用 `run --codex-bin /absolute/path/to/codex` 明确选择可执行文件。Codex 参数放在 `run --` 后，例如 `run -- resume <session-id>`。

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

## 开发检查

代码和文档开发在独立 Git worktree 中进行。仓库内的本地检查通过一个脚本启动：

```bash
bash scripts/check.sh
```

脚本执行 `python3 -m unittest discover -s tests`，每次新建 `runs/_tests/check_<UTC时间>_<进程号>/`，保存 `test.log` 和 `exit-code.txt`。它在终端显示结果摘要，失败时输出完整测试日志；退出码保留测试结果。测试目录中的本地模拟服务用于协议和生命周期检查，不能替代真实 Codex 与供应商的端到端验证。

2026-09-30 在 Linux、Python 3.12、Codex 0.159.2 下完成：53 项自动化测试、wheel 构建与隔离安装、原生 CLI 参数合并对照、交互式双模型菜单，以及无 ChatGPT 登录模式下两条 Responses 入口的固定回答检查。真实供应商配置与实测记录保存在仓库外，不随代码分发；这些检查不代表所有模型或上游均兼容。

### 可选真实检查

配置好供应商、模型和密钥后，可以通过真实 Codex CLI 检查调用链路。**这会向所选上游发送真实请求，消耗上游额度。** 每个模型别名启动一次小任务，要求返回固定字符串；客户端或网关的重试可能增加实际请求次数。

```bash
python3 scripts/smoke_codex.py \
  --gateway-home ~/.config/codex-gateway \
  --model example/coding
```

把 `--gateway-home` 换成你的网关配置目录，`--model` 换成已配置的别名；可以重复提供 `--model` 检查多个模型。脚本默认在 `PATH` 中寻找 `codex-gateway` 和 `codex`，也可通过 `--gateway-bin`、`--codex-bin` 指定可执行文件。`--effort` 可指定这些模型均支持的推理档位，`--timeout` 设置每个模型任务的等待秒数，默认 180 秒。

检查使用指定网关配置中的 Codex home。需要隔离日常会话环境时，先用单独的 `--home` 目录执行 `init --codex-home /absolute/path/to/test-codex-home`，建立独立测试配置，并按上述流程配置认证模式、模板、供应商和模型。

每次运行默认在私人网关配置目录下新建 `runs/_tests/live_codex_<UTC时间>_<唯一后缀>/`，保存各模型的命令快照、JSON 事件、错误日志、回答、退出码，以及汇总 `report.json`。可用 `--output-root` 更改归档根目录，实际供应商的实测资料应继续保存在源码仓库外。通过只表示对应模型完成了这次固定回答检查，不代表跨供应商续聊、所有工具调用或模型质量已经验证。
