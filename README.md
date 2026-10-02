# shop-tenant-service

把租户档案、资源配额、计量事件、限流额度与用量报表记录成可查询的服务，支持按租户和账期读取配额余量，并追溯每一期用量汇总与超限判定依据。

## 运行要求

- Go 1.26 或以上
- SQLite（本服务自带存储，不需要外部数据库）

## 构建、测试与启动

```bash
go build ./...
go test ./...
go run .
```

服务默认监听 `127.0.0.1:8080`。可用环境变量覆盖：

| 变量 | 默认值 | 用途 |
|---|---|---|
| `ADDR` | `127.0.0.1:8080` | HTTP 监听地址 |
| `DB_PATH` | `shop-tenant-service.db` | SQLite 数据库文件路径 |

## 租户生命周期

租户状态限定为 `provisioning`、`active`、`suspended`、`archived`，合法迁移：

- `provisioning` → `active` / `suspended` / `archived`
- `active` → `suspended` / `archived`
- `suspended` → `active` / `archived`
- 同一状态允许重复提交；`archived` 为终态

`suspended` 与 `archived` 租户拒绝所有变更类请求（配额、计量、限流），返回 `tenant_suspended`。

## 已公开的入口

### `GET /healthz`

返回服务与存储状态。正常时 HTTP 200：

```json
{"status":"ok","database":"ok"}
```

存储不可用时 HTTP 503：

```json
{"error":{"code":"storage_unavailable","message":"database is not available"}}
```

### `POST /v1/tenants`

创建租户档案。请求体提供 `id`（租户标识）、`name`、`status`（初始状态）、`plan_ref`（套餐引用）。成功返回 HTTP 201 与可读取的档案。

### `GET /v1/tenants/:id`

返回租户档案。租户不存在返回 `tenant_not_found`。

### `PUT /v1/tenants/:id`

可选地更新 `name`、`status`、`plan_ref`；非法状态或非法迁移返回 `invalid_tenant_input`。

### `PUT /v1/tenants/:id/quotas`

按 `period`（账期）、`resource`、`limit` 调整配额。每次变更只追加一条 `quota_changes` 记录，并重算该账期余量。也接受 `quotas` 数组一次调整多个资源。返回上限 `limit`、已计量 `used`、余量 `remaining` 与 `status`。

### `GET /v1/tenants/:id/quotas?period=YYYY-MM`

返回该账期全部资源的上限、已计量、余量与状态。

### `POST /v1/metering/events`

上报计量事件，字段为 `tenant_id`、`event_no`（事件号）、`period`、`resource`、`amount`、`event_time`（RFC3339）。事件只追加；同一 `(tenant_id, event_no)` 重复上报返回 `duplicate_metering_event`。余量不足或超过上限返回 `quota_exceeded`。

### `GET /v1/tenants/:id/usage?period=YYYY-MM`

按账期返回每个资源的总量 `used`、`limit`、`remaining`、`status`，与配额读取使用同一账期口径，重算后结果一致。

### `POST /v1/tenants/:id/throttle/check`

限流判定，字段为 `check_no`、`period`、`resource`、`amount`。返回 `allowed`、`remaining`、`status` 并保存结果。放行时在同一事务内条件扣减配额；相同 `check_no` 重复提交返回首次结果，不重复扣减。并发判定通过单写事务串行化，不会超卖。

## 配额状态

对每个 `(账期, 资源)`：

- `remaining > 0` 且余量高于上限的 20%：`ok`
- `0 <= remaining` 且余量不超过上限的 20%（含恰好为 0）：`warning`
- `remaining < 0`：`exceeded`

## 隔离与错误约定

- 所有带租户标识的读写都按租户隔离；访问不存在租户或跨租户访问统一返回 `tenant_not_found`。
- 配额不存在返回 `quota_not_found`；字段或状态非法返回 `invalid_tenant_input`。
- 错误响应只包含一个顶层 `error` 对象，且只有 `code`、`message` 两个字符串字段；`message` 不包含 SQL、堆栈或文件路径。未知路径保持返回 `route_not_found`。

| code | HTTP | 含义 |
|---|---|---|
| `tenant_not_found` | 404 | 租户不存在或跨租户访问 |
| `quota_not_found` | 404 | 该租户、账期、资源没有配额 |
| `invalid_tenant_input` | 400/409 | 字段缺失、格式非法或状态迁移非法 |
| `tenant_already_exists` | 409 | 租户标识已存在 |
| `quota_exceeded` | 409 | 余量不足或超过上限 |
| `duplicate_metering_event` | 409 | 同一事件号重复上报 |
| `tenant_suspended` | 403 | 暂停或归档租户发起变更 |
| `route_not_found` | 404 | 未知路径 |
| `storage_unavailable` | 503 | 存储健康检查失败 |
