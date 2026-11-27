# trae-signin — TRAE 纯签到工具（豆包账号版）

TRAE（CN）的轻量签到工具。只做三件事：**登录 → 签到 → 查积分**，没有任何 OpenAI API 反代或对话功能。

基于 [traework2api](https://github.com/Sliverkiss/traework2api) 精简而来。

已适配 **2026-11-18 起 TRAE 账号并入豆包飞书账号**的新登录/刷新体系（豆包 SSO + PKCE + v3 ExchangeToken 设备签名）。

纯 Go 标准库，零第三方依赖。

## 账号体系融合升级（2026-11-18）

TRAE 账号已经升级为豆包飞书账号，旧的登录/刷新方式失效：

| 项目 | 旧 | 新 |
|---|---|---|
| 登录 | 回调里直接带 `refreshToken` | 豆包 SSO 回调 `authCodeInfo` + PKCE 换 token |
| 刷新 token | `api.trae.com.cn/cloudide/.../ExchangeToken` | `api.trae.cn/trae/api/v3/oauth/ExchangeToken`，需设备密钥签名 |
| 账号信息 | `GetUserInfo` | `GetUserInfo` + `GetUIDFusionInfo: true` |
| 签到/积分 | `api.trae.cn/trae/api/v2/ug/...` | **未变** |

因此升级后需要重新登录一次，生成带设备密钥对的新凭证；旧凭证仍会尝试走 legacy 接口兜底，但随时可能彻底失效。

## 功能

| 功能 | 说明 |
|------|------|
| **登录** | `login.sh` 走豆包 SSO：生成登录链接 → 浏览器登录 → 粘贴回调 → 自动换 token 落盘（含设备密钥对） |
| **签到** | 批量遍历 `auths/` 下所有账号，自动刷新过期 token，逐个签到 |
| **到账校验** | 签到后回查确认，接口返回成功但积分未到账会标记为 `UNCONFIRMED`（不谎报成功） |
| **积分拆分** | 分别显示「月赠额度」与「签到奖励额度」，一眼看出签到有没有加分 |
| **通知** | 签到完成后按配置推送到 Server酱 / Bark，可只配一个或同时启用 |
| **定时签到** | GitHub Actions 每天 08:10、16:10 自动执行 |

## 快速开始

```bash
# 1. 安装依赖（需要 Go 1.21+；通知使用 curl）
# macOS: brew install go curl
# Ubuntu: apt install golang curl

# 2. 克隆仓库
git clone https://github.com/<your-username>/trae-signin.git
cd trae-signin
chmod +x login.sh signin.sh

# 3. 登录账号（设备号默认自动从 TRAE 客户端读取）
./login.sh
# → 浏览器打开链接 → 手机号/验证码登录 → 粘贴回调链接

# 4. 签到
./signin.sh
# → 自动刷新 token → 签到 → 显示积分 → Server酱 / Bark 通知
```

### Windows 用户

登录必须在**装了 TRAE 客户端的那台机器**上做（要读 `storage.json` 里的真实设备号），但签到可以放到 Linux / NAS / CI 上跑。

```powershell
# 在 Windows 上登录（PowerShell，不需要 Git Bash / WSL）
cd D:\path\to\trae-signin
.\login.ps1

# 也可以直接跑 Go 命令
go run ./cmd/login
```

`login.sh` 是 bash 脚本，Windows 上可用 Git Bash 运行；PowerShell 用 `login.ps1`。

登录完成后，把生成的 `auths/trae-*.json` 复制到 Linux 服务器即可——**不需要复制 storage.json**，设备号和设备私钥都已经在凭证 JSON 里。

### 在 Linux 上签到

两种方式都可以：

```bash
# 方式 A：复制源码到 Linux，直接编译运行（零第三方依赖）
git clone <repo> && cd trae-signin
go build -o signin_bin ./cmd/signin
./signin_bin auths

# 方式 B：Windows 交叉编译后只传二进制
# PowerShell:
#   $env:GOOS="linux"; $env:GOARCH="amd64"
#   go build -o signin_bin ./cmd/signin
# 然后 scp signin_bin 和 auths/ 到 Linux 执行
```

## Server酱 / Bark 通知配置

签到完成后会按配置推送：配置 `SERVERCHAN_SENDKEY` 启用 Server酱，配置 `BARK_URL` 启用 Bark。两个都配置就两个都发，只配置一个就只发一个（**不要把真实 Key 写进仓库**）。

```bash
# Server酱
export SERVERCHAN_SENDKEY="SCT..."

# Server酱³ 需要自定义端点
export SERVERCHAN_API="https://<uid>.push.ft07.com/send"

# Bark（可选）
export BARK_URL="https://api.day.app/你的Key"

./signin.sh
```

推送内容示例：

> **✅ TRAE 签到成功**
> 总计2 | 成功1 | 已签1 | 未确认0 | 失败0
> 弎水 ALREADY 总600(签到+100)
> 用户0670362 ✅ OK 总600(签到+100)

> **⚠️ TRAE 签到未确认**
> 总计2 | 成功0 | 已签1 | 未确认1 | 失败0
> 弎水 ALREADY 总600(签到+100)
> 用户0670362 UNCONFIRMED 总500(签到0)

## ⚠️ 关键：必须使用真实设备号

签到接口会校验 `x-device-id`，**必须是 TRAE 客户端真实注册过的设备号**，否则一律返回：

```json
{"code": 9074, "message": "当前参与用户太多，请稍后再试"}
```

> **这句话是骗人的。** 跟"人多不多"没关系，真实原因是设备校验不通过。

### 为什么随机设备号不行

- 自己生成的 32 位 hex / UUID → 服务端直接忽略 → `9074`
- 真号只能从**装过 TRAE 客户端并登录过的机器**上取

### 怎么取真实设备号

`login.sh` 会自动在下列位置查找，无需手动输入：

```text
Windows: %APPDATA%\Trae CN\User\globalStorage\storage.json
         %APPDATA%\TRAE SOLO CN\User\globalStorage\storage.json
         %APPDATA%\Trae\User\globalStorage\storage.json
macOS:   ~/Library/Application Support/Trae CN/User/globalStorage/storage.json
Linux:   ~/.config/Trae CN/User/globalStorage/storage.json
```

文件里搜索 `iCubeAuthInfo://icube-dc`：

```json
"iCubeAuthInfo://icube-dc:1234567890123": "dGMFEAAA..."
                        ^^^^^^^^^^^^^^^
                        这串数字就是设备号（8~24 位十进制，示例中为占位值）
```

**注意**：不要用 `telemetry.devDeviceId`（UUID 格式，会被拒绝）。
设备号是**纯数字**。

### 一台设备一天只能签一个账号

```
9095 = 当前设备今日已经签到
```

签发额度是按**设备**算的，不是按账号。所以：

| 场景 | 需要几台设备 |
|---|---|
| 签 1 个账号 | 1 台 |
| 签 N 个账号 | **N 台**（每台都要装过 TRAE 客户端并登录） |

同一设备号填给多个账号，第二个起必然 `9095`。

### 用法

```bash
# 默认自动检测设备号
./login.sh

# 手动指定设备号
DEVICE_ID=1234567890123 ./login.sh

# 旧 SOLO 账号如果换 token 失败，可指定旧 client_id
./login.sh -client-id en1oxy7wnw8j9n

# 只打印登录链接，不等待回调（调试用）
./login.sh -print-url
```

登录成功后凭证里会多出 `clientId` / `devicePrivateKey` / `devicePublicKey` 三个字段，
**不要手动删除**——刷新 token 时要用私钥做设备签名。

已有旧凭证也可以直接补 `deviceId` 字段，但仍然建议重新登录一次。

## 签到状态说明

| 状态 | 含义 |
|---|---|
| `✅ OK` | 签到成功，且已回查确认积分到账 |
| `ALREADY` | 今日已签到，无需重复 |
| `BAD_DEVICE` | 设备号未被服务端认可（`code=9074`），需按上文更换真实设备号 |
| `UNCONFIRMED` | 签到接口返回成功，但回查发现积分未到账，**不计为失败但会告警** |
| `DISABLED` | 账号被服务端禁用签到，不计为失败 |
| `FAIL` | 网络错误、HTTP 4xx/5xx 或 token 刷新失败 |

> **关于「假成功」**：TRAE 的 `claim` 接口在业务失败时**仍返回 HTTP 200**，真正的结果在响应体的 `code` 字段里（`0` 为成功，`9074` 为设备校验失败）。本工具会解析 `code`，并按 3s → 12s 退避重试；重试后仍失败会**回查积分确认到账**，只有确实进账才判为成功。

## 积分构成说明

TRAE 的积分由**多个独立到期的包**组成，`总计` 是它们的合计：

| 包 | 来源 | 有效期 |
|---|---|---|
| 每月登录赠送 | 每月自动发放（通常 500） | 按自然月 |
| 签到奖励 | 每日签到发放（100/天） | 自签到日起 31 天 |

所以「签到成功但总积分没涨」的常见原因是：**签到包是独立的一份，且会单独过期**。本工具已把两项拆开显示在 `月赠` / `签到` 两列。

> 账号与豆包合并后，`ide_user_ent_usage` 返回的是 Trae + 豆包的合并余额
> （`balance_summary` 里 `source=1` 为 Trae、`source=2` 为豆包）。本工具只显示
> **Trae 来源**的余额，不会把豆包积分算进 `总计`。

## 定时签到

### 方式一：GitHub Actions（推荐，仍然支持）

最稳定的方案，不依赖本地环境，GitHub 服务器每天自动触发。**Actions 只负责签到，登录仍需在本地做一次。**

**配置步骤：**

1. Fork 或推送本仓库到你的 GitHub

2. 在仓库 Settings → Secrets and variables → Actions 中添加以下 Secrets：

   | Secret 名称 | 值 | 说明 |
   |---|---|---|
   | `TRAE_AUTH_1` | 账号1 的完整凭证 JSON | **新版** `auths/trae-*.json` 的文件内容 |
   | `TRAE_AUTH_2` | 账号2 的完整凭证 JSON | 同上，多个账号依次添加 |
   | `SERVERCHAN_SENDKEY` | 可选，Server酱 SendKey | 配置后启用 Server酱 通知 |
   | `SERVERCHAN_API` | 可选，自定义端点 | Server酱³ 使用，默认 `https://sctapi.ftqq.com` |
   | `BARK_URL` | 可选，`https://api.day.app/你的Key` | 配置后启用 Bark 通知 |
   | `GH_PAT` | 可选，Personal Access Token | 用于把刷新后的凭证自动写回 Secrets |

   > 账号融合后请用新版 `login.ps1` / `login.sh` 重新登录，把**新的**凭证 JSON 覆盖到 Secrets。
   > 旧凭证缺少设备私钥，只能走 legacy 兜底，大概率已经失效。

3. 每天北京时间 **08:10** 自动执行，**16:10** 再执行一次兜底；也可在 Actions 页面手动触发

   > GitHub 的 cron 使用 **UTC**，且整点附近容易排队延迟，所以这里错开到 10 分。
   > 定时触发后还会随机等待 1~15 分钟再签到，避免每天时间完全一致；手动触发不等待。

4. 可选：配置 `GH_PAT` 自动回写凭证

   Actions 的 `GITHUB_TOKEN` **不能**修改 Secrets，且运行器里的 `auths/` 会在结束时删除。
   由于 refreshToken 会随刷新轮换，建议创建一个 PAT 并保存为 `GH_PAT`：

   - fine-grained PAT：仓库权限勾选 **Secrets: Read and write**
   - 或 classic PAT：勾选 `repo`

   配置后，**只有 token 实际刷新时**才会把新凭证写回 `TRAE_AUTH_1/2`；未刷新则跳过，避免无意义的 Secret 更新。
   不配置也能跑，但 refreshToken 轮换后 Secret 会失效，需要重新登录并手动覆盖。

5. 仓库保活（已内置）

   GitHub 会在仓库连续 60 天无提交时停用定时任务，因此仓库自带
   [keepalive.yml](.github/workflows/keepalive.yml)，每月 1 日、15 日自动提交一次时间戳。
   它需要 `contents: write` 权限，请确认：

   **Settings → Actions → General → Workflow permissions → Read and write permissions**

   `signin.yml` 本身只需要读取权限，不受此项影响。

> 凭证 JSON 格式（**用新版 `./login.sh` 重新登录后**从 `auths/trae-*.json` 复制）：
> ```json
> {"account":{"uid":"...","nickname":"..."},"auth":{"accessToken":"...","refreshToken":"...","expiresAt":1786858238,
>  "clientId":"ono9krqynydwx5","deviceId":"...","machineId":"...","devicePrivateKey":"-----BEGIN PRIVATE KEY-----...",
>  "devicePublicKey":"-----BEGIN PUBLIC KEY-----..."}}
> ```
>
> 账号融合后，`devicePrivateKey` 是刷新 token 的必需品；缺失时只能走旧接口兜底。

### 方式二：Crontab

```bash
# 每天 08:00 签到
crontab -e
# 添加：
0 8 * * * cd /path/to/trae-signin && bash signin.sh >> signin.log 2>&1
```

## 目录结构

```
cmd/
├── login/main.go       登录入口（豆包 SSO + PKCE + v3 换 token）
└── signin/main.go      签到入口

internal/
├── auth/auth.go        凭证文件解析与原子写回（含设备密钥对）
└── upstream/upstream.go 上游 API 客户端（签到/积分/刷新 token）

login.sh                登录脚本（macOS/Linux/Git Bash，包装 cmd/login）
login.ps1               登录脚本（Windows PowerShell，包装 cmd/login）
signin.sh               签到脚本（含 Server酱 / Bark 通知）
crontab.txt             Crontab 配置参考
auths/                  （gitignored）凭证文件
```

## API 端点

本工具调用 TRAE 官方 API：

| 端点 | 用途 |
|------|------|
| `api.trae.cn/trae/api/v3/oauth/ExchangeToken` | AuthCode / refreshToken 换 accessToken（v3，需设备签名） |
| `api.trae.cn/cloudide/api/v3/trae/ExchangeToken` | 旧版换 token（仅兜底） |
| `api.trae.cn/cloudide/api/v3/trae/GetUserInfo` | 查询账号信息（`GetUIDFusionInfo`） |
| `api.trae.cn/trae/api/v2/ug/checkin_credits/status` | 查询签到状态 |
| `api.trae.cn/trae/api/v2/ug/checkin_credits/claim` | 执行签到 |
| `api.trae.cn/trae/api/v2/pay/ide_user_ent_usage` | 查询积分余额 |

## 安全说明

- 所有 token 和**设备私钥**仅保存在 `auths/` 目录（已 gitignored），不会上传到 Git
- 终端输出只显示 UID/昵称/积分，不打印 token
- 回调链接里只有一次性 AuthCode，不包含长期凭证

## 致谢

- [traework2api](https://github.com/Sliverkiss/traework2api) — 原项目，本工具的精简来源
- [Server酱](https://sct.ftqq.com/) — 签到结果推送
- [Bark](https://github.com/Finb/Bark) — iOS 通知推送工具

## License

MIT
