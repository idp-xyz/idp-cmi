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

可选配置文件 `IDP_CMI_CONFIG`（见 `internal/config`）：

```yaml
embedding:   # ADR-0001 D11: OpenAI 兼容 /v1/embeddings；不配则只有全文检索
  baseUrl: http://127.0.0.1:8080/v1
  model: text-embedding-3-small
  apiKeyFile: /run/secrets/embedding
consolidation:   # ADR-0001 D15: OpenAI 兼容 /v1/chat/completions；不配则没有整合
  baseUrl: http://127.0.0.1:8080/v1
  model: qwen3.8-flash-next-iq3_xxs
  source: distiller      # 本租户里登记的 agent 来源，产出归于它
  interval: 1h           # 0 = 只在管理员触发时整合
```

`authorization.pdp`（可选，D23）指向 OPA 风格的决策端点；`authorization.approvals`（可选，D30，`url`、`caFile`、`timeout`）指向核验审批引用的审批系统。租户策略（`PUT /api/v1/policy`）中 `externalModelCeiling`（D29，缺省 internal）限定送往嵌入与整合模型的敏感级别，`requireApproval`（D30）要求接纳候选时带审批引用。`IDP_CMI_INGEST`（缺省 10s）是连接器、擦除与投影 Worker 的轮询间隔；`make test-live`（`IDP_LIVE_OPENAI_URL`、`IDP_LIVE_MODEL`）对真实模型跑整合门禁。

Kubernetes 清单在 `deploy/kubernetes`（未在集群上应用；`tests/deploy` 保证其内部一致）。

运行：`idp-cmi migrate`（owner 连接）→ `idp-cmi ensure-login idp_cmi_svc` → `idp-cmi seed-tenant acme` → `idp-cmi serve`（环境变量见 `cmd/idp-cmi/main.go` 的注释）。开发模式下 `idp-cmi dev-token TENANT SUBJECT ROLE...` 签发 HS256 令牌。

与 HOST 的联调（HOST 作为 `cmi-reader` 解析上下文并记录 `contextPackageId`）在 HOST 侧实现，见 HOST ADR-0001 的后续章节。

## 许可证

本仓库采用 [PolyForm Noncommercial License 1.0.0](LICENSE)：允许非商业用途，禁止任何商业使用。
