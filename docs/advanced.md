# 高级配置与兼容命令

日常使用请从 [README](../README.md) 的安装、官方登录和第三方配置流程开始。本页保留旧命令与排错细节。

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
| `official-models-cache.json` | 与原生账号对应的官方模型能力缓存 |
| `keys/` | 交互输入的供应商 API key，文件权限 `0600` |
| `client-token` | token 模式下 Codex 到本地网关的认证 |
| `admin-token` | 本地网关状态查询和停止操作的认证 |
| `runtime/` | 运行状态、生命周期锁和 `server.log` |

实际供应商配置应放在仓库外的私人目录；不要把真实供应商地址、模型映射、令牌或密钥加入 Git。[examples/config.json](../examples/config.json) 只展示配置结构；其中路径、模型 ID 和模板名均是占位值，不应直接覆盖已经初始化的配置。

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

旧入口中，无参数的 `codex-gateway` 等同于 `codex-gateway run`；安装后的日常入口是 `codex`。`start` 启动或复用这个配置目录对应的后台网关；`run` 需要已有网关配置，在真正启动会话时确保网关运行，再调用安装时保留的原启动器，未记录原启动器时从 `PATH` 查找 Codex。已有的 Codex launcher 和备份入口仍通过这条调用链运行，也可以用 `run --codex-bin /absolute/path/to/codex` 选择本次客户端的可执行文件。Codex 参数放在 `run --` 后，例如 `run -- resume <session-id>`。

首次调用原生客户端时，如果所选 Codex home 尚不存在，会创建该目录；已有目录的权限、配置和登录文件保持原样。官方登录或凭据刷新产生的认证更新由原生 Codex 负责。

`CODEX_GATEWAY_CODEX_BIN=/absolute/path/to/codex` 可以选择原客户端入口，优先于安装记录和 `PATH`；单次 `run --codex-bin` 则优先于该变量。账号刷新、模型发现和历史查询会单独查找原生二进制，支持原生安装及官方 npm 安装布局；需要指定时使用 `CODEX_GATEWAY_NATIVE_CODEX_BIN=/absolute/path/to/native/codex`。内部调用没有单独指定时也会接受显式设置的 `CODEX_GATEWAY_CODEX_BIN`，因此该值应指向适合这些调用的可信入口。已运行的网关不会自动继承新环境变量，需要先停止空闲网关再启动。

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

混合配置下，包装入口把已注册的官方与第三方模型放在同一个 `model_provider` 和模型目录中，由网关按模型别名选择上游。`/model` 只切换模型，不切换客户端连接；这也适用于通过 `-m` 选择官方模型和恢复已注册官方模型的会话。仅官方配置继续原生直连；未知模型保留原生处理，不会被强制发送给第三方。

每个供应商独立配置代理。如果第三方可用而官方模型未出现在菜单，检查官方登录和官方供应商的网络设置；例如需要代理时执行 `codex-gateway add-provider official --official --proxy http://127.0.0.1:7890 --replace`（替换为自己的供应商名称及代理地址），再重开 Codex。命令复用原生登录并尝试同步模型，不要仅为了显示菜单手工声明账号不可用的官方模型。

### 原生历史列表

`codex resume` / `codex fork` 的列表增强独立于模型路由配置：未配置网关、仅官方配置或显式选择未知模型时也生效。只读取当前 `CODEX_HOME`（已有网关配置时使用其 `codex_home`），不会自动合并其他目录。通过私有 Unix socket 使用原生选择器，只把 `thread/list` 的 `modelProviders` 改为空数组；保留原生 Cwd / All 切换，默认 Cwd，传入 `--all` 才默认 All。`resume` 额外包含非交互会话；搜索、分页、预览和归档筛选仍由原生界面处理。

列表后端使用临时、无需上游认证的本地历史读取配置，不同步模型、不连接供应商、不启动网关转发服务，也不写入用户 provider 配置。选中后关闭临时连接，再把会话 ID 和原始参数交回正常启动路径，此时才解析模型和准备所需路由。真正的会话不走 remote 模式，避免改变目录确认和权限参数。退出或取消时回收临时进程与 socket，不迁移历史、不重写 provider 元数据。显式会话 ID/名称和 `--last` 不经过选择器。此路径已在 Codex 0.160.1 验证，依赖其 app-server 协议，原生版本升级时需要回归检查。

可见性不依赖旧 provider 是否已注册，但继续对话仍需原生配置或网关路由及有效凭据。`codex-gateway run -- resume` 是显式网关模式，仍需要网关配置；只想浏览已有历史时直接使用包装入口 `codex resume`。

不同供应商可能无法读取彼此生成的加密 reasoning。默认保留原始请求并返回上游错误，不猜测未知失败原因。

对于上游明确返回的 `invalid_encrypted_content`，提供默认关闭的兼容开关：

```bash
codex-gateway config set retry_invalid_encrypted_reasoning true
```

开启后仅在 HTTP 400 错误明确点名某个加密 reasoning 项时，移除该项并做有限次数的重试；不会修改已保存的原会话，也不会删除 compaction 项。它不保证所有上游之间的历史兼容，更不修复任意认证、网络或模型错误。无法兼容时，应为对应供应商开启新会话。


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


## 手动模型能力覆盖

普通 `add-model` 按 GPT 型号匹配能力，无需配置本节参数。第三方入口明确采用更小的上下文或限制推理档位时，才使用 `--context-window`、`--reasoning-effort` 等覆盖。

旧 `--template`、`model add ... --template ...`、`catalog import/list/build` 仍可用于迁移已有配置。它们是兼容入口，不是首次使用步骤。覆盖值不会增加供应商的实际能力；未识别的 GPT 型号不会静默降成固定的 32K 配置。

`codex-gateway --help-advanced` 列出兼容命令。显式 `init` 的默认值继续是 `--auth-mode codex`，新交互配置默认使用本地 token；两种模式的本地认证和原生会话归属不同。安装后的恢复入口会兼顾原生与网关会话，不迁移会话文件。
