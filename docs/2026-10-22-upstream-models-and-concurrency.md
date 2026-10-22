# 2026-10-22 上游模型轮换与并发调度修复说明

本文记录 2026-10-22 的一次完整排查与修复：现象是「账号登录正常、接口也能通，但常用模型调用失败」
以及「多并发请求大面积超时」。两部分问题分别来自上游模型授权变更和网关侧的账号调度缺陷。

## 1. 现象

- `/v1/models` 里列出的 `deepseek-v4-flash-0731`、`deepseek-v4-pro-0813`、`glm-5.3-flash`
  调用一律 404：`InferHub.4004.200 benefit not found`。
- 新出现在发现结果里的 `DeepSeek-V4.1-Flash`、`GLM-5.3`、`Kimi-K3` 调用 404：
  `InferHub.002002009.404 The model is not registered`。
- 20 并发压测（`glm-5.2`，max_tokens=8）：9/20 成功、11 个 180s 超时，最高 137s；
  日志显示 20 个请求全部落在**同一个账号**上，并持续触发
  `upstream queue limit retry=N/30`。

## 2. 上游侧实测结论（不是本地代码问题）

### 2.1 福利网关已关闭

```text
GET  {snap}/v1/benefit-gateway-config
  → 200 {"enabled":false,"is_examination_domain":false,"examination_model_enable":false}

GET  {opengw}/api/v1/gateway/config        → 200 {"error_code":"4000","error_msg":"invalid channel, group not found"}
POST {opengw}/api/v1/benefit/claim {}      → 200 {"error_code":"4000", ...同上...}
GET  {opengw}/api/v1/user/tokens/balance   → 200 {"error_code":"4000", ...同上...}
```

因此旧的三个福利模型已停发，领取（claim）也必然失败——`benefit_auto_claim` 开着只会每轮刷日志。

### 2.2 新【限时体验】模型走福利路由但未授予

`DeepSeek-V4.1-Flash`、`GLM-5.3` 在 `GET /v1/model/builtin` 里出现（描述带【限时体验】）：

| 请求头 | 上游返回 |
|---|---|
| 不带 `maas_type` | `InferHub.002002009.404 The model is not registered` |
| 带 `maas_type: benefit` | `InferHub.4004.200 benefit not found` |

即它们确实是福利路由模型，但当前账号未获授权；福利网关恢复前无法使用，本地代码无法绕过。

### 2.3 逐模型实测（真实账号，2026-10-22）

| 模型 | 结果 |
|---|---|
| `GLM-5.2` / `glm-5.2` / `snap-chat`（旧别名） | ✅ 200 |
| `glm-5.2-sft-harmony` | ✅ 200 |
| `Qwen3-VL-235B` / `qwen3-vl-235b` | ✅ 200 |
| `kimi-k2.6-vl` | ✅ 200 |
| `GLM-5.2-ArkTS-SPARK`、`OpenPangu-2.0-Flash/Pro`、`Kimi-K3` | ❌ `002002009.404` 未注册 |
| `DeepSeek-V4.1-Flash`、`GLM-5.3` | ❌ `4004.200` 未授予 |
| `deepseek-v4-flash-0731`、`deepseek-v4-pro-0813`、`glm-5.3-flash` | ❌ `4004.200` 已停发 |

OpenAI 兼容层本身正常：流式（含 `stream_options.include_usage` 的 usage chunk 与 `[DONE]`）、
工具调用（`tools` + `tool_choice: auto/required/指定函数`）、小写别名归一均实测通过。

## 3. 代码改动

### 3.1 模型目录（`internal/server/handler.go`）

`staticModels` 是 `/v1/models` 在动态发现失败时的兜底表，必须只放**实测确认可用**的模型：

- 删除已停发的 `deepseek-v4-flash-0731`、`deepseek-v4-pro-0813`、`glm-5.3-flash`
  （它们会从静态表补位，导致客户端选中就 404）。
- 新增实测可用的 `kimi-k2.6-vl`。
- 福利模型不再写进静态表：福利网关恢复后由模型发现自动带回（发现结果优先于静态表）。

### 3.2 重新登录后自动解禁（`internal/pool/pool.go`）

账号因 `refresh_token expired; re-login required` 被禁用后，状态会持久化到 `data/state.json`。
重新登录换了凭证，但禁用标记不会自己消失，表现为「登录了还是不可用」。

- `SyncToDir`（`POST /admin/api/reload` 走这里）：凭证 `updated_at` 比内存里的新时自动解除禁用。
- `loadState`（进程启动时）：凭证文件比 `state.json` 新时，忽略当时的「终态失效」禁用结论。
- 手动禁用（凭证未变）不受影响。

### 3.3 并发调度（`internal/server/handler.go` + `internal/pool/pool.go`）

修复前：所有并发请求都走

```go
acct = h.cfg.Pool.PickExcluding(tried)                  // 永远返回第一个健康账号
h.cfg.Pool.AcquireLockWait(acct.Name, 180*time.Second)  // 然后在这一个账号上死等
```

于是 20 个并发全部堆在同一个账号上，撞上游 `TM.00001041`（单账号 3 个并发会话且释放慢）。

修复后：

- `acquireSlot()`：按顺序挑「能服务该模型 + 还有空闲并发槽位」的健康账号并加锁，
  并发自然分散到整个账号池；一个账号满员就换下一个。
- `AcquireAnyLockWait()`：全池满载时等待「任意账号」释放槽位（轮询 50ms），
  而不是钉在某个账号上反复超时、把换号预算耗光后返回 503。
- 上游排队（`TM.00001041`）仍以「原地等上游释放」为主：实测换号无用——别的账号本地槽位
  可能空着，但上游会话同样没释放；原地等通常十几秒即可恢复。
- 用 `holding` 标志管理槽位，修掉排队重试路径上重复 `ReleaseLock` 的隐患。
- `AcquireLock` 失败不再打印日志（`acquireSlot` 会逐个试满号账号，否则刷屏）。

### 3.4 构建（国内网络）

- `Dockerfile`：`ARG GO_IMAGE` / `ARG RUNTIME_IMAGE` / `ARG APK_MIRROR`，
  基础镜像和 apk 源可按需换镜像站（默认值不变）。
- `docker-compose.yml`：`build.args` 读取 `CA2A_GO_IMAGE`、`CA2A_RUNTIME_IMAGE`、`CA2A_APK_MIRROR`。
- `.env.example`、`README.md`：补充用法与 `daemon.json` 镜像加速说明。

### 3.5 默认值

- `config.example.json`：`max_concurrent` 由 5 改为 **2**（上游单账号 3 个并发会话且释放慢）。
- `README.md`：环境变量表修正 `CA2A_MAX_CONCURRENT`（原写默认 5，代码实际默认 1）。

## 4. 压测对比（20 并发，`glm-5.2`，max_tokens=8，10 个账号）

| 场景 | 结果 |
|---|---|
| 修复前 | 9/20 成功、11 个 180s 超时，max 137s，全部压在 1 个账号，数十次 queue limit |
| 修复后（上游空闲） | **20/20 成功，p50 8.6s，max 11.7s，上游 queue limit 0 次** |
| 5 并发（常态） | 5/5 成功，p50 8.9s，max 9.1s |
| 连续两波 20 并发 | 第二波仍 20/20，但 p50≈57s（上游会话容量上限，网关排队不报错） |
| `max_concurrent=1` 对照 | 更差：一个请求排队会阻塞整个账号（head-of-line blocking） |

## 5. 运维配置建议

| 配置 | 值 | 理由 |
|---|---|---|
| `max_concurrent` | `2`（建议 2~3） | 上游单账号 3 个并发会话且释放慢；设 1 会阻塞整号，>3 必撞 `TM.00001041` |
| `benefit_auto_claim` | `false` | 福利网关 `enabled:false`，claim 必然返回 4000；恢复后改回 `true` |
| `queue_retry_seconds` / `queue_max_attempts` | 默认 10s / 30 | 上游排队时原地等待的节奏 |

## 6. 验证方法

```bash
# 单元测试 / 静态检查
go test ./... -count=1
go vet ./...

# 账号状态与运行时配置
curl -s http://127.0.0.1:7866/admin/api/overview -H "Authorization: Bearer $CA2A_API_KEY"

# 账号当前真实可用模型（-claim=false 只读）
go run ./cmd/models -claim=false

# 单条对话
curl -s http://127.0.0.1:7866/v1/chat/completions \
  -H "Authorization: Bearer $CA2A_API_KEY" -H "Content-Type: application/json" \
  -d '{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"max_tokens":8}'

# 重新登录后热重载（凭证更新会自动解禁）
curl -s -X POST http://127.0.0.1:7866/admin/api/reload -H "Authorization: Bearer $CA2A_API_KEY"
```

## 7. 已知限制

- `DeepSeek-V4.1-Flash`、`GLM-5.3`、`Kimi-K3` 等需上游恢复福利/体验授权，本地无法绕过。
- 长期并发超过「账号数 × 2」时延迟会明显上升（上游会话容量限制），但不会报错；
  网关会排队并在上游释放后完成。
- 后台模型可用性探测会占用上游会话，高峰时可能加剧排队（代码已做避让，已发出的探测无法中断）。
