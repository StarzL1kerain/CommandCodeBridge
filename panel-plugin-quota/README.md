# 管理面板 · 插件额度补丁

> **这一步独立于插件本身**：CPA 宿主完整支持插件额度，但**官方管理面板前端没有插件分支**，
> 所以不打这个补丁，面板上不会出现额度卡片（也不会报错，只是看不到）。
> 不打补丁插件照样能用 —— 额度可以直接 `GET /v0/management/<pluginID>/quota`，
> 或打开插件控制台页面查看。

## 为什么必须打补丁

宿主的额度能力是完整体（`quota_provider` 能力键、`/v0/management/quota/providers|fetch|reset`、
`/v0/management/plugins/:id/quota`），但面板把额度分派**写死在 7 个内置厂商上**：

- `features/quota/providers/index.ts` 的 `QUOTA_ADAPTERS` 是闭合静态表，没有回退
- `QuotaProviderType` 是封闭联合类型，`AuthFileQuotaSection.tsx` 里一串硬编码 `if` + 穷尽断言
- `features/authFiles/logic.ts` 用 `QUOTA_PROVIDER_TYPES` 白名单一票否决

也就是说宿主的 `/quota/fetch` 在面板里**一个消费者都没有**。官方最新版 **v1.24.2（2026-09-22）**
实测同样如此 —— **更新面板解决不了**，只能自己补。

补丁的做法是加**一个通用适配器**去接宿主的通用端点，而不是逐个厂商手写；规模 23 个文件
＋686 / −17 行（其中 5 个新文件）。

## 上游基线（补丁只在这条基线上保证干净套用）

| 项 | 值 |
|---|---|
| 仓库 / 分支 | `router-for-me/Cli-Proxy-API-Management-Center` @ `main` |
| commit | `4530da271ba2e89810d4dccebc57f3091afa590a`（2026-09-21） |
| 对应发布 | v1.24.2（2026-09-22，由该源码构建） |
| `src/` 整树指纹 | sha256 `c18803e2df1344ba6b73ee37b5db7fd03789ab54a93937825c1c6cbc060076d0`（398 个文件）|
| 本目录补丁 | `panel-plugin-quota.patch`，43,830 字节，UTF-8 + LF，路径前缀 `a/src/…` |

## 用法

### 方式一：已经拿到补丁版产物（最快）

```bash
CFG=~/.config/cliproxyapi
cp -a $CFG/static/management.html $CFG/static/management.html.bak-$(date +%Y%m%d-%H%M%S)
# 把补丁版 management.html 覆盖到 $CFG/static/management.html
```

CPA **每次请求都从磁盘读**面板，**不需要重启**。校验：

```bash
curl -s -o /tmp/served.html -w "%{http_code} %{size_download}\n" http://127.0.0.1:1235/management.html
grep -c plugin_quota /tmp/served.html    # 期望 14
```

> 本仓库出于体积与再分发的考虑**只放源码补丁**，不放 2.8 MB 的构建产物；没有现成产物时用方式二。

### 方式二：从补丁重新构建

```bash
# 1) 取与上表一致的基线源码
curl -L -o panel.tar.gz \
  https://codeload.github.com/router-for-me/Cli-Proxy-API-Management-Center/tar.gz/4530da271ba2e89810d4dccebc57f3091afa590a
tar -xzf panel.tar.gz && cd Cli-Proxy-API-Management-Center-4530da271ba2e89810d4dccebc57f3091afa590a

# 2) 套用补丁（在含 src/ 的根目录执行，-p1 剥掉 a/ 或 b/ 前缀）
git apply -p1 < /path/to/panel-plugin-quota.patch
#    没有 git 时等价写法：patch -p1 < panel-plugin-quota.patch

# 3) 构建（产物是单个 HTML）
npm install && npm run build      # = tsc && vite build → dist/index.html（约 2.8 MB）

# 4) 上传为 $CFG/static/management.html，然后强制刷新浏览器
```

## 两个必须注意的坑

1. **面板自动更新会覆盖补丁**：确认 CPA 配置里 `remote-management.disable-auto-update-panel: true`。
2. **浏览器缓存**：CPA 返回面板时只给 `Last-Modified`，**没有 `Cache-Control`、也没有 `ETag`**，
   浏览器会按启发式新鲜度缓存（约"距今时间 × 10%"），于是出现"部署成功了但打开还是旧界面"，
   连普通 F5 都可能无效。
   - 用户侧：`Ctrl+Shift+R`、无痕窗口，或访问 `.../management.html?v=2`
   - 根治：在反代（如 nginx）给面板单独加 `add_header Cache-Control "no-cache" always;`

## 回滚

```bash
mv ~/.config/cliproxyapi/static/management.html.bak-<时间戳> ~/.config/cliproxyapi/static/management.html
```

## 补丁后能看到什么

- 凭据卡片底部出现 **「点击此处刷新额度」**，点一下取数并渲染窗口条
- 配额管理页多出 **「插件」** 分组
- 识别 `five_hour` / `weekly` / `seven_day` / `monthly` 四种 window token，其余原样显示

## 许可

`panel-plugin-quota.patch` 是针对上游面板源码的修改（面板由 `router-for-me` 独立维护）。
本目录只放 diff，不含上游代码；套用前请自行获取上游源码并遵守其许可。
