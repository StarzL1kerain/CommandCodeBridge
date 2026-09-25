# CommandCodeBridge

CommandCodeBridge 是 [CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI) 的 Command Code 插件。它把 [Command Code](https://commandcode.ai/) 的 API Key 订阅接入 CPA 的凭据系统，提供模型目录、OpenAI 兼容转发、Claude 模型的 Anthropic 协议自动转换、真实 SSE 流与额度上报。首版面向 CLIProxyAPI v7.3.12，发布 Linux amd64/arm64、macOS amd64/arm64 和 Windows amd64 五个平台的动态库。

## 功能

- **API Key 凭据**：Command Code 只使用长期 API Key（Studio 或 `cmd login` 产生，CLI 与 Provider API 共用同一把 key）。在插件管理页粘贴即可，保存时自动经 `GET /alpha/whoami` 校验有效性并抓取用户名作为默认备注。凭据交给 CPA 的 `auth-dir` 保存；插件状态目录不保存凭据。
- **模型目录一键刷新**：`GET /provider/v1/models` 无需鉴权（已实测），返回 81+ 个模型与各自的 `supported_endpoints`。刷新后全部模型可用，无需逐个添加；也支持手动维护别名映射。
- **Claude 模型自动协议转换**：Claude 系模型（`supported_endpoints` 仅含 `/messages`）自动走 Anthropic Messages 端点，插件完成 OpenAI ⇄ Anthropic 的双向转换（含 system、tools、tool_choice、tool_result、thinking/reasoning_content、stop_reason 与 usage 映射，流式与非流式均覆盖）。其余模型直接走 OpenAI 兼容的 `/chat/completions`，零转换。
- **额度上报**：聚合 `whoami` / `billing/credits` / `billing/subscriptions` / `usage/summary` 四个内部接口，展示剩余积分、额外积分、本周期已花费、周期剩余天数，以及 5 小时与 7 天两个滚动窗口的剩余比例（CLI `/usage` 同款数据源）。也可用 `GET /v0/management/commandcodebridge/quota` 直接查看。
- **流式转发**：处理跨网络分块的事件、用量与终止信号；非流式支持 `native`、`native-fallback`、`stream-aggregate` 三种模式，默认 `stream-aggregate`。
- **管理页**：展示凭据、模型映射、请求状态、耗时、用量与尝试记录，界面跟随同源 CPA 主题。

## 鉴权与登录

Command Code 的鉴权只有一种形态：`Authorization: Bearer <API Key>`，key 长期有效，`auth.refresh` 永远返回静态时间。

官方 CLI 的浏览器登录（`https://commandcode.ai/studio/auth/cli`）依赖在本地起 loopback 回调端口接收 key，插件进程内无法提供，因此本插件不支持浏览器登录：`auth.login.start/poll` 返回引导信息，请在插件管理页粘贴 API Key。Studio 地址：<https://commandcode.ai/studio>（API Keys 页生成）。

## 额度

额度来自四个内部只读接口（CLI 1.65.2 逆向确认，`/usage` 命令同款调用序列），全部以同一枚 API Key 访问：

| 接口 | 用途 |
|---|---|
| `GET /alpha/whoami?limits=1` | 用户与组织（orgId） |
| `GET /alpha/billing/credits?orgId=` | 剩余积分（monthly + purchased + free）与 `windowLimits` |
| `GET /alpha/billing/subscriptions?orgId=` | 套餐（planId）、当前周期 |
| `GET /alpha/usage/summary?orgId=&since=` | 周期内已花费（totalCost） |

注意各接口没有统一信封：credits 包了一层 `credits`、subscriptions 包了一层 `data`，whoami 与 summary 是平铺对象。`windowLimits.fiveHour/weekly` 的 `used`/`cap` 是积分（1 积分 ≈ 1 美元用量），`resetAt` 是 epoch 毫秒（CLI 直接与 `Date.now()` 比较）。

窗口以额度桶（bucket）形式上报，`remainingFraction = 1 - used/cap`，取值 **0~1 的小数**（不是百分数）。**宿主会丢弃没有有效 `remainingFraction` 的桶**，所以这个字段不能省。窗口 token 使用 `five_hour` 与 `seven_day`（宿主面板可识别）。`windowLimits.limited` 为 false（按量付费）时不报桶，只报余额，整体不失败。

`quota.reset` 一律返回失败，因为 Command Code 未提供重置额度的接口。

### 在管理面板里查看（注意面板版本）

插件按 CPA 的额度能力上报（`quota_provider: true`），宿主侧完整支持。但**官方管理面板目前不支持插件额度**——它把额度分派写死在 7 个内置厂商上，没有插件分支（最新版 v1.24.2 实测同样如此）。因此在**未打补丁**的面板上，凭据卡片上不会出现额度区块，也不会报错。

两条路：

1. **不依赖面板**：直接 `GET /v0/management/commandcodebridge/quota`，或打开控制台页面查看。
2. **让面板显示**：给管理面板套用插件额度补丁，卡片上即出现「点击此处刷新额度」。现成产物见 `../panel-plugin-quota/`，
   原理与部署步骤见 [`../docs/04-管理面板-额度显示与部署.md`](../docs/04-管理面板-额度显示与部署.md)。

补丁版面板会把 `window` token 翻译成标签，识别 `five_hour` / `weekly` / `seven_day` / `monthly`。换到 Command Code 无需再改面板。

## 安装

先在 CPA 配置中启用插件并添加本仓库的市场源。需要 CLIProxyAPI v7.3.12 或兼容的插件 ABI。以下是通用配置片段，按现有配置合并：

```yaml
plugins:
  enabled: true
  dir: plugins
  store-sources:
    - https://raw.githubusercontent.com/StarzL1kerain/CommandCodeBridge/main/marketplace/registry.json
```

CPA 的内置官方市场始终保留；`store-sources` 添加一个额外来源。在 CPA 的插件市场找到 **CommandCodeBridge** 并安装。市场读取 `marketplace/registry.json`，再从本仓库 Release 下载与宿主平台匹配的 ZIP 和 `checksums.txt`，核验 ZIP 的 SHA-256。各 ZIP 根目录分别是 `commandcodebridge.so`（Linux）、`commandcodebridge.dylib`（macOS）或 `commandcodebridge.dll`（Windows）；安装后的文件名带版本，但插件 ID 始终是 `commandcodebridge`。

市场安装会写入插件配置。插件加载后打开：

```text
/v0/resource/plugins/commandcodebridge/console
```

页面优先复用同源 CPA 管理中心通过“记住密码”保存的登录信息，并核对其服务器地址。未保存登录信息、存储不可用或管理中心跨域时，才提示输入 **CPA 管理密钥**；手动输入的密钥仅保存在当前页面内存。插件不会额外持久化管理密钥。CPA 的管理 API 必须已启用；接口鉴权仍由 CPA 控制。进入页面后点“添加凭据”，粘贴 Command Code API Key，再刷新或选择模型。

插件配置与请求记录默认写在 `plugins/commandcodebridge-data`；凭据文件写在 CPA 配置的 `auth-dir`。使用容器时，应分别持久化这两个目录及插件目录。备份时也应覆盖这两处数据。

## 模型与路由

客户端调用 CPA 的 OpenAI 兼容接口时使用模型名即可。默认映射 `deepseek-v4-flash` → `deepseek/deepseek-v4-flash`、`deepseek/deepseek-v4-flash` → 同名、`claude-sonnet-5` → 同名；在管理页“模型映射”里可一键刷新目录获取全部模型，或手动添加别名。

```json
{
  "model": "claude-sonnet-5",
  "messages": [{"role": "user", "content": "你好"}],
  "stream": true
}
```

路由规则（依据目录 `supported_endpoints`，未知模型按 `claude` 前缀兜底）：

| 端点 | 模型 | 说明 |
|---|---|---|
| `POST /provider/v1/chat/completions` | DeepSeek、Qwen、Gemini、GPT 等 | OpenAI 兼容，直接转发 |
| `POST /provider/v1/messages` | Claude 系（如 `claude-sonnet-5`） | 插件自动做 OpenAI ⇄ Anthropic 转换 |

Claude 模型不能发给 `/chat/completions`（上游返回 400），这是路由转换存在的原因。thinking 块映射为 `reasoning_content`，工具调用增量在流式下逐块翻译为 OpenAI `tool_calls` 分片，聚合逻辑与其他模型共用。

日志中的实际 provider 来自上游响应显式 `provider` 字段；未回报时显示“未知”，不猜测。

非流式三种模式用于应对偶发空内容，推荐的 `stream-aggregate` 从第一次请求就使用流式上游，客户端仍收到非流式 JSON；它不会消除上游本身的错误。可选的 `native-fallback` 可能产生第二次上游请求。SSE 一旦开始向客户端输出，就不进行透明重试。上游错误、订阅额度与模型可用性仍由 Command Code 决定。

CPA v7.3.12 的 Chat Completions 流式接口由宿主封装 SSE，插件提交原始 JSON 并由宿主发送结束标记。标准 `/v1/messages` 路由的 Claude 转换器要求 SSE 输入，插件依据宿主传入的 `request_path` 适配。

## 从源码构建

项目使用 Go 1.26、标准 C ABI 和 `gopkg.in/yaml.v3`。在仓库根目录执行：

```bash
docker build --platform linux/amd64 -f Dockerfile.build --output type=local,dest=dist .
```

构建阶段使用 `golang:1.26-bookworm`，并以 `CGO_ENABLED=1`、`-buildmode=c-shared` 编译 `./cmd/codecbridge`。输出 `dist/commandcodebridge.so`，目标为 Debian 12 兼容的 Linux amd64 动态库。

[构建工作流](.github/workflows/release.yml) 在普通 push 和 PR 中分别测试并构建五个平台：Linux amd64 使用 Debian 12 Docker 构建，Linux arm64 在 `ubuntu-24.04-arm` 上使用同一 Go 镜像；macOS amd64/arm64 使用原生 `macos-15-intel`/`macos-15`；Windows amd64 使用 `windows-latest` 和 MSYS2 UCRT64 GCC。只有推送与源码版本一致的 `v<version>` 标签且五个作业全部通过时，工作流才汇总发布五个 ZIP 与统一的 `checksums.txt`。资产命名遵循 [CPA 官方插件市场规范](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store#release-requirements)。

市场 registry 使用 CPA v7.3.12 的 `schema_version: 1`、`github-release` 安装类型。它是 CPA 商店入口，不是一个可直接安装的 `.so` URL；若使用自己的市场源，也必须托管符合该 schema 的 JSON registry，并提供对应 GitHub Release 资产。

## 版本与开发记录

本文档描述 **v0.1.0** 的功能集。

Command Code 侧的实证事实（Provider API 形状、`/alpha/*` 用量接口、登录流程、错误信封）由官方文档、`command-code` npm 包 1.65.2 逆向与无鉴权实测交叉确认：

- Provider API：`https://api.commandcode.ai/provider/v1`，OpenAI 兼容，`/models` 无鉴权。
- 错误信封：`{"success":false,"error":{"code","status","message"}}`（401 实测样例）。
- `GET /provider/v1/models` 的 `supported_endpoints` 决定模型端点：Claude 系仅 `/messages`，多数模型为 `/chat/completions,/responses`。

宿主契约与开发方法论整理在 `docs/` 下（与插件仓库同级）：

| 文档 | 内容 |
|---|---|
| [`docs/README.md`](../docs/README.md) | 索引与一页速查（新增供应商四阶段） |
| [`docs/01-宿主契约-CPA插件ABI.md`](../docs/01-宿主契约-CPA插件ABI.md) | 能力键、方法分派、登录/额度/管理 API 的字段级契约 |
| [`docs/02-开发手册-新增供应商.md`](../docs/02-开发手册-新增供应商.md) | 四阶段 checklist、安全基线、验收清单 |

> `docs/` 目前位于插件仓库同级目录。若要随仓库一起发布，把它移入仓库根目录即可，上表中的相对链接无需改动。

## 许可与来源

本项目按 [MIT License](LICENSE) 发布。CommandCodeBridge 为独立实现，从 [ClinePassBridge](https://github.com/StarzL1kerain/ClinePassBridge) 的已验证宿主适配层演进而来；Command Code 侧行为全部自行实证，未复制任何官方代码。Command Code 与 CLIProxyAPI 分别由各自项目维护。
