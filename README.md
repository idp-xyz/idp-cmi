# idp-cmi

IDP TELOS 的 Context, Memory & Knowledge Infrastructure（CMI）：可信的企业上下文——谁在什么时候从哪里得到了什么，它还有效吗，谁可以看。设计见 `docs/adr/ADR-0001-cmi-design.md`。

状态：**M1 完成并实测**：来源登记（信任级别）、记忆记录（作用域、来历、有效期、追加与撤销）、知识文档（版本取代、chunk 全文索引含 CJK bigram）、上下文包（检索 → 过滤 → 信任 / 新鲜度 / 相关度排序 → 预算装包 → 密封、内容寻址、可读回并报告失效项）、撤销传播与到期扫描、租户级敏感上限策略、RLS、审计、指标、追踪、TLS。**M2 完成**（ADR 第 5–6 节）：混合检索（OpenAI 兼容嵌入端点 + PostgreSQL 数组余弦，无扩展依赖，D11）、写入时注入扫描与隔离（`flags`、`includeFlagged` 策略，D12）、显式实体引用与实体视图（D13）、Kubernetes 清单与 compose e2e（`HOST=1` 时与真实 idp-host 联调，D14）；契约 `idp.telos.cmi` 1.1.0 已发布。**M3 完成**（ADR 第 7 节）：记忆整合 Worker（episodic → semantic，产出 informational、带 derivedFrom 与有效期、先过注入扫描，D15）、连接器与处理 Worker（http-manifest / filesystem，租约、重试、内容摘要跳过，D16）、评估套件（污染、来源优先级、撤销传播、时效、敏感与 ACL，证据入库，D17）；本机 Qwen 的 live 门禁通过。**M5 完成**（ADR 第 8–9 节，白皮书评审后的治理补全）：写入幂等键与预期版本（D19）、受控擦除与各域净化（D20）、候选 / 存疑状态与转换（D21）、组织 / 团队 / 项目作用域与显式共享（D22）、外部 PDP（D23）、消费时复验（D24，HOST 与 EER 侧已接）、双时态 asOf（D25）、outbox 向量投影（D26）、标注评估集与基线对照（D27，`make test-eval`，结果在 `docs/eval/`）；契约 1.3.0。**M6 完成**（ADR 第 10–11 节，文档对照后的补齐）：包报告未放入的条目与原因（D28，`omitted`）、按敏感级别限定可送外部模型的内容（D29，`externalModelCeiling`）、审批引用经审批系统核验（D30，`authorization.approvals`、`requireApproval`）、评估报告来源可追溯率（D31）；契约 1.4.0。

| 接口 | 角色 | 内容 |
|---|---|---|
| `/api/v1/sources`、`/api/v1/knowledge/documents`、`/api/v1/policy`、`/api/v1/audit` | `cmi-admin` | 来源、知识文档（索引、版本、撤回）、租户策略、审计 |
| `/api/v1/memory/consolidate`、`/api/v1/memory/consolidations` | `cmi-admin` | 立即整合（可限 scope）与整合记录（D15） |
| `/api/v1/connectors`、`…/{ref}/sync`、`…/{ref}/jobs` | `cmi-admin` | 连接器登记、立即同步、抓取任务（D16） |
| `/api/v1/evaluations` | `cmi-admin` | 跑治理套件并读回证据（D17） |
| `/v1/memory/records`、`/v1/memory/revoke` | `cmi-writer` | 追加记忆（必须带来历与已登记的来源；敏感级别受写者上限约束；可带 `entities`；写入时扫描注入模式并打 `flags`）、撤销；`?flagged=true`（管理员）审阅隔离区 |
| `/v1/context/resolve`、`/v1/context/packages/{id}` | `cmi-reader` | 解析上下文包（全文 + 向量混合检索，`entities` 过滤）、读回（含 `stale` 与失效条目） |
| `/v1/entities/{type}/{id}` | `cmi-reader` | 引用该业务对象的、调用者可见的记忆与文档 |
| `/v1/context/packages/{id}/verify`、`/v1/units`、`/v1/memory/records/{id}/share`、`…/{id}/transitions` | `cmi-reader` / `cmi-writer` / `cmi-admin` | 消费时复验（D24）、我的单元、显式共享（D22）、状态转换（D21） |
| `/api/v1/units`、`/api/v1/erasures`、`/api/v1/projections` | `cmi-admin` | 单元目录（D22）、受控擦除（D20）、投影积压（D26） |

```sh
make test               # gofmt、vet、单元、契约与部署清单一致性测试（-race）
make test-integration   # 真实 PostgreSQL 上的集成测试（Docker 起一个临时实例）
bash deploy/build.sh    # 离线构建 scratch 镜像
bash deploy/e2e.sh      # compose 拓扑端到端（CMI 镜像 + PostgreSQL）；HOST=1 再起 idp-host 镜像验证 D10 / HOST D32
```

CI 与 `deploy/e2e.sh` 里的第三方镜像（PostgreSQL、pgvector、trivy）先由 `deploy/pull-image.sh` 显式拉取并重试（最多 5 次，间隔 15、30、60、120 秒；本机已有则跳过），因为公共 registry 对匿名拉取按来源地址限流，而 CI 的 runner 与许多项目共用地址。

可选配置文件 `IDP_CMI_CONFIG`（见 `internal/config`）：

```yaml
embedding:   # ADR-0001 D11: OpenAI 兼容 /v1/embeddings；不配则只有全文检索
  baseUrl: http://127.0.0.1:8080/v1
  model: text-embedding-3-small
  apiKeyFile: /run/secrets/embedding
  query:         # 可选，ADR-0001 D59：查询自己的端点（同一个模型）；文档（投影、回填、写入）仍用上面的端点
    baseUrl: http://embedding-query:8080/v1
    apiKeyFile: /run/secrets/embedding-query   # 自己的密钥，不继承；不写就不带密钥
    timeout: 10s                                # 缺省 10s；超时的查询只答词面检索
    # 不能写 model、dimensions 与主端点不同的值（启动即拒绝）；前缀、CA 与主端点共用
consolidation:   # ADR-0001 D15: OpenAI 兼容 /v1/chat/completions；不配则没有整合
  baseUrl: http://127.0.0.1:8080/v1
  model: qwen3.8-flash-next-iq3_xxs
  source: distiller      # 本租户里登记的 agent 来源，产出归于它
  interval: 1h           # 0 = 只在管理员触发时整合
```

`embedding.query`（D59）：摄入占着主端点时，查询会排在整个摄入请求后面（4 CPU 实测：每次 64 条输入等 247 秒，8 条时 p95 31 秒），所以查询可以有自己的实例。启动后和之后每 5 分钟（未通过时每 30 秒）CMI 把一条固定文字发给查询端点，与主端点的向量比较，要求维度相等、余弦至少 0.999（参照是主端点对同一条文字的向量，第一次拿到就存进库，之后探测只问查询端点；所以摄入期间重启不会让查询一直只答词面；迁移 0017）；**通过之前或不通过时查询只答词面检索**（不改用主端点），日志记一条（不匹配是 error），指标：`idp_cmi_query_endpoint_state{state}`、`idp_cmi_query_endpoint_probes_total{result}`、`idp_cmi_embedding_requests_total{role,outcome}`、`idp_cmi_embed_failures_total{kind,role}`（新增 `role` 标签；`kind="unavailable"`：整个请求没有得到可用答案，连不上、超时、5xx，每个失败的请求一次，不消耗任何块的重试次数）、`idp_cmi_embedding_last_success_timestamp_seconds{role}`（该角色最近一次成功嵌入的 Unix 时间，0 = 启动后还没有；告警可写 `time() - ... > 600`）。查询端点失败不会让解析失败，也不会把任何块记成永久失败。

`authorization.pdp`（可选，D23）指向 OPA 风格的决策端点；`authorization.approvals`（可选，D30，`url`、`caFile`、`timeout`）指向核验审批引用的审批系统。租户策略（`PUT /api/v1/policy`）中 `externalModelCeiling`（D29，缺省 internal）限定送往嵌入与整合模型的敏感级别，`requireApproval`（D30）要求接纳候选时带审批引用。`IDP_CMI_INGEST`（缺省 10s）是连接器、擦除与投影 Worker 的轮询间隔；`make test-live`（`IDP_LIVE_OPENAI_URL`、`IDP_LIVE_MODEL`）对真实模型跑整合门禁。

Kubernetes 清单在 `deploy/kubernetes`（未在集群上应用；`tests/deploy` 保证其内部一致）。

运行：`idp-cmi migrate`（owner 连接）→ `idp-cmi ensure-login idp_cmi_svc` → `idp-cmi seed-tenant acme` → `idp-cmi serve`（环境变量见 `cmd/idp-cmi/main.go` 的注释）。开发模式下 `idp-cmi dev-token TENANT SUBJECT ROLE...` 签发 HS256 令牌。

与 HOST 的联调（HOST 作为 `cmi-reader` 解析上下文并记录 `contextPackageId`）在 HOST 侧实现，见 HOST ADR-0001 的后续章节。

## 升级到有知识库的版本（ADR-0001 第 24 节）

- **迁移 0013–0016 要当停机处理**：0013 与 0015 取 ACCESS EXCLUSIVE 锁并改写每个 chunk 行，停机随语料增长（本机 30 万 chunk 约 7 秒，估算与测量办法见 ADR D47 之后的"迁移"一段）；事后对 `knowledge_chunks` 做 VACUUM。
- **被取代的旧版本要重新解读**：旧发布把被取代的文档版本存成 `superseded`；升级后"现行"按 D52 推导，撤回更新的一版会让上一版重新现行（之前什么都不返回）。一个版本到期不会让更早的版本复活。
- 对象存储（可选）在后台连：CMI 不等它，原件在它可达之前答 503；`IDP_CMI_ORIGINALS_CA_FILE`、`_TIMEOUT` 见 `cmd/idp-cmi/main.go`。配置文件新增 `knowledge` 一节（`indexContext`、`personalQuotaDocuments`、`personalQuotaBytes`、`maxInFlightUploadBytes`、`maxConcurrentDownloads`、`maxDownloadsPerCaller`、`transferTimeout`）。**原件下载需要一个可写的临时目录**（`IDP_CMI_TEMP_DIR`；根文件系统只读时用 emptyDir / tmpfs，`deploy/kubernetes/cmi.yaml` 已带，compose 要加 tmpfs，见 `deploy/compose.yaml` 头部）。

## 原件转成文本（KB3，ADR-0001 第 26 节）

PDF、Word、PowerPoint、Excel 的原件存进对象存储之后，由一个**专做摄入的 idp-sandbox 实例**提取文本（办公档位，无网络），CMI 的后台线程经**系统网关**调它；字节只在沙箱与 CMI 的内部文件端点之间走。开关与设置都在配置文件的 `knowledge.conversion`（`enabled: false` 或没有这一节：原件只存不转，和以前一样）。

- **需要**：对象存储（`IDP_CMI_ORIGINALS_*`）、内部文件端点的监听（`IDP_CMI_INGEST_LISTEN`，**只给内网，不能是 API 的地址**）、票据的密钥文件（`IDP_CMI_INGEST_TICKET_KEY_FILE`，权限 0600 或 0400、至少 32 字节且至少 16 种不同的字节；可选 `IDP_CMI_INGEST_TLS_CERT_FILE`、`IDP_CMI_INGEST_TLS_KEY_FILE`），以及配置文件里的网关、令牌端点、客户端、工作负载身份。缺哪一项，启动即拒绝。
- **配置项**（`knowledge.conversion`）：`gatewayUrl`（系统网关的 MCP 地址）、`toolPrefix`（缺省 `ingest__`）、`caFile`、`tokenUrl`、`clientId`、`clientSecretFile`、`scope`、`tokenCaFile`（OAuth `client_credentials`，Keycloak 只是现在回答它的那一个）、`workloadTenant`与`workloadSubject`（网关对沙箱断言的调用者；票据里的 `tenant_id` 与 `sub`）、`endpointUrl`（沙箱的 `IDP_SANDBOX_FILES_URL`；票据的受众）、`ticketTtl`（缺省 5m）、`concurrency`（缺省 4）与`sandboxConcurrency`（摄入实例的 `IDP_SANDBOX_MAX_CONCURRENT`，缺省 8；前者不得超过后者）、`maxAttempts`（缺省 6）、`runSeconds`（缺省 300，不得超过摄入实例的 `IDP_SANDBOX_TIMEOUT_S`）、`callTimeout`（缺省 `runSeconds`+90 秒，必须大于系统网关的 `IDP_CALL_TIMEOUT`）、`jobTimeout`（15m）、`lease`（20m，大于 `jobTimeout`）、`interval`（5s）、`maxResultBytes`（6 MiB）、`maxConcurrentReads`（8）。
- **一个版本的文本**：这一版（调用者先写一段占位文本，再存原件）在第一次转换成功之前是 `pending`——不检索、不取代它要取代的那一版；成功时它的文本在一个事务里被换成提取出来的，定位信息（页、幻灯片、标题路径、工作表与单元格范围）随块出来，向量由现有的投影接着做。失败时它仍是 `pending`、前一版照常可取；`GET /v1/knowledge/documents/{id}` 的 `ingest` 给出状态、失败原因码和给上传者的说明，`POST /v1/knowledge/documents/{id}/ingest` 重新排队。
- **契约 `idp.telos.cmi` 1.6.0**（只做新增）：`KnowledgeDocument.ingest`、`status` 增 `pending`、`ContextItem.locator` 增 `slide`、`sheet`、`range`。
- **升级**：迁移 0018 新建两张表、给 `knowledge_documents` 与 `knowledge_chunks` 各加列（不改写已有行）。

## 许可证

本仓库采用 [PolyForm Noncommercial License 1.0.0](LICENSE)：允许非商业用途，禁止任何商业使用。
