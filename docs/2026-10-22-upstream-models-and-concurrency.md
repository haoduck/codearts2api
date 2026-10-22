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

### 2.3 逐模型实测（真实账号，2026-10-22/23）

修正 ID 大小写后（见 §2.4），当前账号 **9 个模型全部可用**：

| 模型 ID（必须按此大小写） | 说明 | 结果 |
|---|---|---|
| `GLM-5.2` | 旗舰模型（`glm-5.2`、`snap-chat` 是别名） | ✅ 200 |
| `glm-5.2-sft-harmony` | 展示名是 **GLM-5.2-ArkTS-SPARK** | ✅ 200 |
| `Qwen3-VL-235B` | 多模态（`qwen3-vl-235b` 是别名） | ✅ 200 |
| `kimi-k2.6-vl` | 编程 | ✅ 200 |
| `openpangu-2.0-flash` | 小写 ID | ✅ 200 |
| `openpangu-2.0-pro` | 小写 ID | ✅ 200 |
| `glm-5.3` | 【限时体验】，积分计费 | ✅ 200 |
| `deepseek-v4.1-flash` | 【限时体验】，积分计费 | ✅ 200 |
| `kimi-k3` | 【限时体验】，积分计费 | ✅ 200 |
| `deepseek-v4-flash-0731`、`deepseek-v4-pro-0813`、`glm-5.3-flash` | 已停发 | ❌ `4004.200` |

### 2.4 根因：agent-center 把「展示名」当模型 ID（2026-10-23 实测）

上游两个模型来源的字段含义不同：

```text
GET /v1/model/builtin              （权威）
  model_id="openpangu-2.0-flash"   model_name="OpenPangu-2.0-Flash"
  model_id="glm-5.2-sft-harmony"   model_name="GLM-5.2-ArkTS-SPARK"

GET /v1/agent-center/agents/detail
  model_id=""                      model_name="OpenPangu-2.0-Flash"
  model_parameters.model_id="openpangu-2.0-flash"
  model_alias="openpangu-2.0-flash"
```

`fetchAgentModels` 原来用 `firstNonEmpty(model_name, model_id, model_alias, ...)`，于是把
**展示名**当成了模型 ID。上游聊天接口对模型 ID **大小写敏感**：

| 请求 model | 结果 |
|---|---|
| `openpangu-2.0-flash` / `openpangu-2.0-pro` | ✅ 流式正常 |
| `OpenPangu-2.0-Flash` / `OpenPangu-2.0-Pro` | ❌ `InferHub.002002009.404 not registered` |
| `glm-5.3` / `deepseek-v4.1-flash` / `kimi-k3` | ✅ |
| `GLM-5.3` / `DeepSeek-V4.1-Flash` / `Kimi-K3` | ❌ `002002009.404` |
| `GLM-5.2` / `Qwen3-VL-235B`（大写/混合） | ✅（改成全小写反而 ❌） |
| `GLM-5.2-ArkTS-SPARK` | ❌（它只是 `glm-5.2-sft-harmony` 的展示名） |

另外三个【限时体验】模型是**按积分计费**的普通模型，**不能**带 `maas_type: benefit`
（带上会返回 `InferHub.4004.200 benefit not found`，不带则正常流式）。

> 这也解释了此前把它们判成「未注册/未授予」的误判：请求发出去的是展示名。

OpenAI 兼容层本身正常：流式（含 `stream_options.include_usage` 的 usage chunk 与 `[DONE]`）、
工具调用（`tools` + `tool_choice: auto/required/指定函数`）、小写别名归一均实测通过。

## 3. 代码改动

### 3.1 模型目录与 ID 归一（`internal/upstream/models.go` + `internal/server/handler.go`）

- **`fetchAgentModels` 优先取真实 ID**：`model_parameters.model_id` → `model_id` →
  `model_alias` → `model_name`，`Name` 保留展示名。此前优先取 `model_name`，
  导致把展示名当 ID 发给上游。
- **新增 `remapToBuiltinIDs`**：用 builtin 的「显示名 → 真实 `model_id`」映射兜底，
  防止 agent-center 再塞展示名。
- **`setAccountModels` 改为「发现结果权威」**：无条件覆盖 `catalog.known` 里的映射。
  原来只在「映射不存在」时写入，冷启动种子猜错大小写后会被永久固化。
- **`seedKnownModels` 清理**：删掉展示名 `GLM-5.2-ArkTS-SPARK`，`openpangu-2.0-*`
  改小写，补上 `kimi-k3` / `glm-5.3` / `deepseek-v4.1-flash` / `glm-5.2-sft-harmony`。
- **`staticModels` 兜底表**（`internal/server/handler.go`）：只放实测可用的模型，
  删除已停发的 3 个福利模型，补入 `kimi-k2.6-vl`、`openpangu-2.0-flash`、
  `openpangu-2.0-pro`、`glm-5.3`、`deepseek-v4.1-flash`、`kimi-k3`。
  福利模型不写进静态表：福利网关恢复后由模型发现自动带回（发现结果优先于静态表）。

效果：`/v1/models` 现在输出 9 个真实 ID（不再出现 `GLM-5.2-ArkTS-SPARK`、
`OpenPangu-2.0-Flash` 这类展示名），且用展示名大小写发起聊天也会被归一后正常返回。

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
