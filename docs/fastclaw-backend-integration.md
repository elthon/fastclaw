# FastClaw 后端服务接入指南

本文档用于指导业务系统把 FastClaw 作为内部 Agent 后端服务来接入。目标读者是业务系统的后端开发人员。

适用场景：

- 业务系统自己负责租户、用户、权限、页面、计费和审计。
- FastClaw 不直接暴露给业务租户使用。
- FastClaw 只作为后端 Agent 运行时和配置管理服务。
- 业务系统后端通过 FastClaw API 完成租户初始化、Agent 创建、API key 创建和 Agent 调用。

## 1. 核心资源映射

建议按下面方式映射业务系统和 FastClaw 的资源：

| 业务系统 | FastClaw | 说明 |
|---|---|---|
| 租户 | user/account | 一个租户对应一个 FastClaw 用户账号 |
| 租户默认智能体 | agent | 每个租户一份独立 Agent |
| 租户下的业务用户 | app_user | 用于隔离会话、记忆、文件等用户状态 |
| 业务后端调用凭证 | API key | 由业务后端保存和使用，不暴露给前端 |
| 业务会话、案件、任务 | sessionId | 由业务系统生成，作为稳定会话 ID |
| Agent 模板库 | 专用 FastClaw 模板账号 | 建议用一个专门账号集中维护所有模板 Agent |

推荐模型：

```text
一个业务租户 = 一个 FastClaw user/account
一个业务租户 = 至少一个租户专属 Agent
一个业务用户 = 一个 FastClaw app_user
```

不要让所有租户共用同一个 Agent。更推荐创建一个“模板 Agent”，然后每个租户开通时从模板 Agent 克隆一份。这样能力一致，但租户之间配置、会话、记忆、API key 和用量都能独立。

模板 Agent 建议集中放在一个专门的 FastClaw 用户账号下面，例如 `template_admin`。业务系统创建租户 Agent 时，只从这个模板账号名下选择 Agent 作为 `forkFrom` 来源，不从全平台所有 Agent 中随意选择。

## 2. FastClaw 初始准备

FastClaw 部署完成后，需要先准备好以下内容：

1. 完成 FastClaw onboarding，创建 `super_admin` 账号。
2. 在 FastClaw 中配置模型 provider，例如 OpenAI、OpenRouter、本地模型等。
3. 创建一个专门用于维护模板的 FastClaw 用户账号，例如：

```text
template_admin
```

4. 在 `template_admin` 账号下面创建模板 Agent，例如：

```text
公安舆情分析模板
网信舆情分析模板
应急事件分析模板
```

5. 配置模板 Agent：

- `SOUL.md`
- `IDENTITY.md`
- `AGENTS.md`
- `TOOLS.md`
- skills
- skill env
- 模型默认值
- prompt mode
- max tool iterations

6. 为业务系统后端准备一个 FastClaw admin API key。

admin API key 只能保存在业务系统后端的密钥管理系统里，不能下发给浏览器、App、租户或第三方。

### 2.1 模板账号和模板目录

建议业务系统维护自己的模板目录表，而不是直接把 FastClaw 的所有 Agent 暴露给业务人员选择。

示例：

```text
agent_templates
  id
  name                  公安舆情分析Agent
  category              公安
  fastclaw_template_user_id
  fastclaw_template_agent_id
  version               v1
  enabled               true
  sort_order
```

模板目录的初始化可以通过 FastClaw 查询接口完成：

```http
GET /api/users/{template_user_id}/agents
Authorization: Bearer <fastclaw-admin-key>
```

该接口返回指定 FastClaw 用户名下的 Agent。因为模板 Agent 都放在 `template_admin` 下面，所以业务系统可以只展示这些模板：

```text
公安舆情分析Agent
网信舆情分析Agent
应急事件分析Agent
市场监管分析Agent
```

也可以使用跨租户列表接口：

```http
GET /api/agents?all=true
Authorization: Bearer <fastclaw-admin-key>
```

但这个接口会返回全平台所有 Agent，包括租户 Agent。生产接入中更推荐使用 `GET /api/users/{template_user_id}/agents`，或者先同步到业务系统自己的 `agent_templates` 表后再展示。

## 3. 租户开通流程

当业务系统创建一个租户时，需要同步在 FastClaw 中创建以下资源：

1. FastClaw user/account
2. 租户默认 Agent
3. 该 Agent 的 agent-scoped API key

### 3.1 创建 FastClaw 租户账号

接口：

```http
POST /api/users
Authorization: Bearer <fastclaw-admin-key>
Content-Type: application/json
```

请求示例：

```json
{
  "username": "tenant_acme",
  "email": "tenant_acme@internal.local",
  "password": "generate-a-long-random-password",
  "displayName": "某某公安局",
  "role": "user",
  "agentQuota": 0,
  "externalId": "your-tenant-id"
}
```

字段说明：

| 字段 | 说明 |
|---|---|
| `username` | FastClaw 内部账号名，建议用租户 ID 生成 |
| `email` | FastClaw 当前要求用户有 email，可使用内部占位邮箱 |
| `password` | 当前 API 要求传入密码，生成随机强密码即可，不暴露给租户 |
| `displayName` | 租户展示名 |
| `role` | 固定使用 `user` |
| `agentQuota` | 建议设置为 `0`，避免租户账号自助创建 Agent |
| `externalId` | 业务系统租户 ID，用于追踪和幂等语义 |

响应示例：

```json
{
  "user": {
    "id": "u_xxx",
    "username": "tenant_acme",
    "role": "user"
  }
}
```

业务系统需要保存：

```text
fastclaw_user_id = user.id
```

### 3.2 从模板 Agent 创建租户 Agent

业务系统应该先从自己的 `agent_templates` 表中取得模板 Agent ID，例如：

```text
fastclaw_template_agent_id = agt_template_police_xxx
```

这个 ID 来自 `template_admin` 账号下面维护的模板 Agent。

接口：

```http
POST /api/users/{fastclaw_user_id}/agents
Authorization: Bearer <fastclaw-admin-key>
Content-Type: application/json
```

请求示例：

```json
{
  "name": "公安舆情分析Agent",
  "description": "租户默认舆情分析 Agent",
  "forkFrom": "agt_template_police_xxx"
}
```

`forkFrom` 是模板 Agent 的 ID。FastClaw 会在创建时从该模板 Agent 复制一份内容到租户自己的 Agent。这个复制是一次性的，后续模板 Agent 修改不会自动同步到已创建的租户 Agent。

会复制的内容：

- `SOUL.md`
- `IDENTITY.md`
- `AGENTS.md`
- `BOOTSTRAP.md`
- `TOOLS.md`
- `HEARTBEAT.md`
- `agent.json`
- agent-scope 的 `agents.defaults`
- agent-scope 的 `skills.entries`

不会复制的内容：

- 会话记录
- 租户用户记忆
- `USER.md`
- `MEMORY.md`
- cron jobs
- channel bindings
- 运行时状态

响应示例：

```json
{
  "agent": {
    "id": "agt_tenant_xxx",
    "userId": "u_xxx",
    "name": "公安舆情分析Agent"
  }
}
```

业务系统需要保存：

```text
fastclaw_agent_id = agent.id
```

### 3.3 创建租户 Agent 专用 API key

接口：

```http
POST /api/users/{fastclaw_user_id}/apikeys
Authorization: Bearer <fastclaw-admin-key>
Content-Type: application/json
```

请求示例：

```json
{
  "name": "tenant_acme_backend",
  "type": "agent",
  "agentIds": ["agt_tenant_xxx"]
}
```

字段说明：

| 字段 | 说明 |
|---|---|
| `name` | API key 名称，建议包含租户标识 |
| `type` | 固定使用 `agent` |
| `agentIds` | 该 key 允许访问的 Agent ID 列表 |

响应示例：

```json
{
  "apikey": {
    "id": "k_xxx",
    "type": "agent",
    "userId": "u_xxx"
  },
  "token": "fc_..."
}
```

注意：`token` 只会返回一次。业务系统必须立即加密保存。

后续该租户调用 Agent 时，使用这个 `token`，不要使用平台 admin key。

## 4. 业务系统建议保存的数据

业务系统的租户表或租户扩展表中建议保存：

| 字段 | 说明 |
|---|---|
| `tenant_id` | 业务系统租户 ID |
| `fastclaw_user_id` | FastClaw 租户账号 ID |
| `fastclaw_agent_id` | 租户默认 Agent ID |
| `fastclaw_agent_api_key_id` | FastClaw API key ID |
| `fastclaw_agent_api_token` | 加密后的 FastClaw API token |
| `template_user_id` | FastClaw 模板账号 ID，例如 `template_admin` 对应的用户 ID |
| `template_agent_id` | 创建时使用的模板 Agent ID |
| `template_version` | 创建时使用的模板版本 |
| `provision_status` | 开通状态 |
| `created_at` / `updated_at` | 审计字段 |

`fastclaw_agent_api_token` 必须加密保存，不允许返回给前端。

## 5. 调用 Agent

推荐优先使用 FastClaw 原生接口：

```http
POST /api/chat/stream
```

这个接口适合业务系统后端集成，支持流式输出，也支持通过 `X-Fastclaw-End-User` 做业务用户隔离。

### 5.1 流式调用

接口：

```http
POST /api/chat/stream
Authorization: Bearer <tenant-agent-token>
X-Fastclaw-End-User: <your-product-user-id>
Content-Type: application/json
```

请求示例：

```json
{
  "agentId": "agt_tenant_xxx",
  "sessionId": "case-20260606-001",
  "message": "请分析这批舆情数据，并输出风险等级、主要观点和处置建议。",
  "params": {
    "tenantId": "tenant_acme",
    "caseId": "case-20260606-001",
    "source": "public-opinion-platform"
  }
}
```

字段说明：

| 字段 | 说明 |
|---|---|
| `Authorization` | 使用租户自己的 agent API token |
| `X-Fastclaw-End-User` | 业务系统用户 ID，用于隔离会话和记忆 |
| `agentId` | 租户 Agent ID |
| `sessionId` | 业务系统生成的会话 ID、案件 ID 或任务 ID |
| `message` | 用户消息或业务系统生成的分析指令 |
| `params` | 额外结构化参数，会传给 Agent 使用 |

响应是 Server-Sent Events。业务后端可以：

- 直接转发流给前端；
- 或在后端消费流，聚合最终答案后再返回；
- 或把输出写入业务系统的任务结果表。

### 5.2 非流式调用

接口：

```http
POST /api/chat
Authorization: Bearer <tenant-agent-token>
X-Fastclaw-End-User: <your-product-user-id>
Content-Type: application/json
```

请求示例：

```json
{
  "agentId": "agt_tenant_xxx",
  "sessionId": "case-20260606-001",
  "message": "请总结这个案件的舆情趋势。"
}
```

非流式接口适合后台任务或不需要逐字输出的场景。

### 5.3 OpenAI-compatible 接口

FastClaw 也提供：

```http
POST /v1/chat/completions
```

请求示例：

```json
{
  "agent_id": "agt_tenant_xxx",
  "user": "your-product-user-id",
  "stream": true,
  "messages": [
    {
      "role": "user",
      "content": "请分析这批舆情事件。"
    }
  ]
}
```

如果你的系统必须复用 OpenAI SDK，可以考虑使用这个接口。

但在当前接入方案中，建议优先使用 `/api/chat/stream`。原因是 `/api/chat/stream` 是 FastClaw Web Chat 同源路径，对 app_user 隔离、agent lazy attach、事件流等支持更完整。若后续强依赖 `/v1/chat/completions`，建议先补充并验证 app_user + agent-scoped key 的自动 attach 逻辑。

## 6. 用户隔离机制

业务系统每次调用 Agent 时，都应该传入业务用户 ID。

原生 Chat API：

```http
X-Fastclaw-End-User: <your-product-user-id>
```

OpenAI-compatible API：

```json
{
  "user": "<your-product-user-id>"
}
```

FastClaw 会基于下面的组合创建或复用 `app_user`：

```text
(api_key_id, external_user_id)
```

这意味着：

- 同一个租户 API key 下，同一个业务用户 ID 会映射到同一个 FastClaw app_user。
- 不同租户 API key 下，即使业务用户 ID 相同，也会映射到不同 FastClaw app_user。
- 租户之间的会话、记忆和文件不会混在一起。

隔离维度：

| 数据 | 隔离方式 |
|---|---|
| Agent 配置 | 每租户独立 Agent |
| 会话历史 | Agent x app_user x sessionId |
| 用户记忆 | Agent x app_user |
| 附件和工作区文件 | Agent x app_user x session/project |
| API 权限 | API key x allowed agent IDs |

## 7. API key 类型说明

FastClaw 支持三种 API key：

| 类型 | 权限 | 推荐用途 |
|---|---|---|
| `admin` | 平台级权限 | 只给业务系统 provisioning 服务使用 |
| `user` | 当前 FastClaw user 拥有的所有 Agents | 内部工具或租户级管理服务 |
| `agent` | 只能访问指定 Agent IDs | 正常租户运行时调用 |

生产环境中，租户运行时调用必须使用 `type=agent`。

FastClaw Dashboard 的 `/apikeys` 页面只能管理“当前登录 FastClaw 用户”的 API keys。它可以创建 `agent` 类型 key，但只能绑定当前用户自己的 Agents。

如果业务系统要给某个租户账号创建 key，应该由业务后端调用：

```http
POST /api/users/{fastclaw_user_id}/apikeys
```

而不是让租户登录 FastClaw UI 操作。

## 8. 建议封装的业务后端模块

建议在业务系统后端封装一个 FastClaw 客户端模块，不要让业务代码到处直接拼 FastClaw HTTP 请求。

建议方法：

```text
FastClawClient
  createTenantAccount(tenant)
  createTenantAgent(fastclawUserId, templateAgentId)
  createTenantAgentKey(fastclawUserId, fastclawAgentId)
  provisionTenant(tenant)
  streamChat(tenantId, userId, sessionId, message, params)
  sendChat(tenantId, userId, sessionId, message, params)
  rotateTenantKey(tenantId)
  disableTenant(tenantId)
```

业务系统其他模块只调用自己的领域服务，例如：

```text
PublicOpinionAgentService.analyzeCase(...)
```

不要直接依赖 FastClaw 的 API 细节。

## 9. 开通流程的幂等和失败恢复

租户开通过程包含多个远程调用，必须考虑中途失败。

建议在业务库中维护 provisioning 状态：

```text
pending
account_created
agent_created
key_created
ready
failed
```

建议流程：

1. 创建租户记录，状态为 `pending`。
2. 调用 `POST /api/users`，成功后保存 `fastclaw_user_id`，状态改为 `account_created`。
3. 调用 `POST /api/users/{id}/agents`，成功后保存 `fastclaw_agent_id`，状态改为 `agent_created`。
4. 调用 `POST /api/users/{id}/apikeys`，成功后加密保存 token，状态改为 `ready`。
5. 如果任一步失败，从最后成功的状态继续重试。

注意：API key 的 `token` 只返回一次。如果创建 key 成功但业务系统保存 token 失败，建议删除该 key 后重新创建，或者人工轮换 key。

## 10. 安全检查清单

- FastClaw admin key 只保存在业务后端。
- 租户 agent token 加密保存。
- 不把任何 FastClaw token 返回给前端。
- 正常租户调用只使用 `type=agent` key。
- 业务后端必须校验 `tenantId -> fastclaw_agent_id` 映射。
- `sessionId` 应由后端生成，或严格校验属于当前租户。
- 生产环境使用 HTTPS 或内网私有通信。
- 租户删除或停用时，需要同步停用或删除对应 FastClaw 资源。
- 发现租户 token 泄露时，立即 rotate 或重新创建 key。

## 11. 模板 Agent 版本管理

建议不要只有一个永远变化的模板 Agent。可以考虑版本化：

```text
public-opinion-agent-v1
public-opinion-agent-v2
public-opinion-agent-v3
```

新租户使用最新模板，老租户是否升级由业务系统控制。

模板升级策略可选：

- 只影响新租户；
- 对老租户批量 patch 配置；
- 让租户在业务系统中选择升级；
- 保留老版本用于稳定性要求高的租户。

## 12. 最小端到端伪代码

```ts
async function provisionTenant(tenant) {
  const userResp = await fastclaw.post(
    "/api/users",
    {
      username: `tenant_${tenant.id}`,
      email: `tenant_${tenant.id}@internal.local`,
      password: randomSecret(),
      displayName: tenant.name,
      role: "user",
      agentQuota: 0,
      externalId: tenant.id,
    },
    {
      authorization: `Bearer ${FASTCLAW_ADMIN_KEY}`,
    },
  );

  const fastclawUserId = userResp.user.id;

  const agentResp = await fastclaw.post(
    `/api/users/${fastclawUserId}/agents`,
    {
      name: "公安舆情分析Agent",
      description: "租户默认舆情分析 Agent",
      forkFrom: FASTCLAW_TEMPLATE_AGENT_ID,
    },
    {
      authorization: `Bearer ${FASTCLAW_ADMIN_KEY}`,
    },
  );

  const fastclawAgentId = agentResp.agent.id;

  const keyResp = await fastclaw.post(
    `/api/users/${fastclawUserId}/apikeys`,
    {
      name: `${tenant.id}_backend`,
      type: "agent",
      agentIds: [fastclawAgentId],
    },
    {
      authorization: `Bearer ${FASTCLAW_ADMIN_KEY}`,
    },
  );

  await db.tenants.update(tenant.id, {
    fastclawUserId,
    fastclawAgentId,
    fastclawApiKeyId: keyResp.apikey.id,
    fastclawApiTokenEncrypted: encrypt(keyResp.token),
    provisionStatus: "ready",
  });
}

async function streamPublicOpinionAnalysis({
  tenantId,
  userId,
  sessionId,
  message,
}) {
  const tenant = await db.tenants.get(tenantId);

  return fastclaw.stream(
    "/api/chat/stream",
    {
      agentId: tenant.fastclawAgentId,
      sessionId,
      message,
      params: {
        tenantId,
        userId,
      },
    },
    {
      authorization: `Bearer ${decrypt(tenant.fastclawApiTokenEncrypted)}`,
      headers: {
        "X-Fastclaw-End-User": userId,
      },
    },
  );
}
```

## 13. 接入结论

推荐最终架构：

```text
业务系统前端
    -> 业务系统后端
        -> FastClaw /api/users               创建租户账号
        -> FastClaw /api/users/{id}/agents   创建租户 Agent
        -> FastClaw /api/users/{id}/apikeys  创建租户 Agent key
        -> FastClaw /api/chat/stream         调用 Agent
```

租户不需要知道 FastClaw 的存在，也不需要登录 FastClaw。FastClaw Dashboard 只作为平台管理员和开发人员的调试、配置、观察工具。
