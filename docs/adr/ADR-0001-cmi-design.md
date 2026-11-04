# ADR-0001　IDP CMI 的设计（Context, Memory & Knowledge Infrastructure）

| 项 | 内容 |
|---|---|
| 状态 | 已接受（2026-11-04，决策人选择按建议顺序开始缺失的子系统，CMI 先于 Workspace） |
| 范围 | `idp-cmi`：Source（来源）、MemoryRecord（记忆）、KnowledgeDocument 与索引（知识）、ContextPackage（上下文包）、Context API、记忆生命周期（时效、撤销）、ACL 与敏感级别、来源追溯 |
| 依据 | 《IDP TELOS System Architecture v1.0》（CMI：ContextResolver、ContextPackage、记忆生命周期、知识检索、语义索引、企业 ACL、来源追溯与治理；状态与权威：上下文与记忆资产，不替代事实库、执行状态或 Agent Session）；《Final Audit v1.1》§4.1（CMI：ContextPackage、Memory Record、Knowledge Index、来源、版本与过期；不能把历史记忆当作当前业务事实）与 §5 P0（业务来源优先级、记忆失效、证据及敏感信息治理：ContextPackage Schema、污染测试、撤销传播、时效评估）；《Repository & Engineering Architecture v1.0》的 `idp-cmi` 目录树（建议稿）；idp-host 的 ADR-0001（身份、RLS、审计、指标、TLS、契约的做法在这里沿用） |

## 1. 背景与边界

五个子系统里 HOST、EER、MCP 已存在；HOST 的 `context` 只是调用方给的文本，EER 没有记忆。CMI 补的是**可信的企业上下文**：谁在什么时候从哪里得到了什么，它还有效吗，谁可以看。它不持有订单、库存、账本等业务事实（领域系统持有），不持有 AgentRun（HOST）或 Execution / Checkpoint（EER）。

设计前先排除的做法：
- 把向量库当产品：检索只是组装的一步，来源、时效、权限和撤销才是边界；M1 不引入向量库，索引用 PostgreSQL 全文检索，嵌入检索作为 M2 的另一种检索器接入。
- 让 agent 自由写记忆并当作事实回放：agent 写入的记忆永远是 informational，而且每条记忆带来源与有效期；上下文包里每一项都标明种类与新鲜度。
- 返回"一段拼好的文本"：ContextPackage 是结构化、内容寻址、可引用的发布件；HOST / EER 引用 packageId，审计能回答"模型当时看到了什么"。

## 2. 决策

| # | 决策 | 理由 |
|---|---|---|
| D1 | **权威状态只有四种对象**：Source、MemoryRecord、KnowledgeDocument（及其 chunk 索引）、ContextPackage。每一项内容都带 **provenance**（来源、引用、观察时间、记录者）、**validity**（生效 / 失效时间）、**ACL**（主体、角色）与 **sensitivity**（public / internal / confidential / restricted）。CMI 不存业务事实：来自领域系统的内容是对事实的引用与摘录，不是事实本身 | 审计 §4.1：来源、版本与过期；不能把历史记忆当作当前业务事实 |
| D2 | **Source 是登记的来源**：名字、种类（domain-system / document-store / agent / user）、信任级别（authoritative / informational）、可选的基址。每条记忆与文档必须指向一个 Source；agent 与 user 来源永远是 informational | 业务来源优先级是 P0：排序时 authoritative 先于 informational |
| D3 | **MemoryRecord**：作用域（tenant / subject / thread / agent）、种类（episodic：发生过的事；semantic：总结出的偏好与事实性陈述）、文本内容与可选结构化字段、标签、provenance、validity（`validUntil` 或 TTL）、状态（active / expired / revoked）、撤销信息、sensitivity、ACL。写入必须给出 provenance 与来源；只有 `cmi-writer` 角色可写；写入是追加，修改 = 撤销旧的 + 写新的 | 记忆有生命周期；追加与撤销比就地修改可审计 |
| D4 | **KnowledgeDocument**：来源、URI、标题、版本、内容摘要（sha256）、文本按段切成 chunk 建 PostgreSQL 全文索引（`tsvector`，`simple` + 中文按字符 bigram 的辅助列），ACL、sensitivity、validUntil。同一 URI 的新版本取代旧版本（旧版本保留、标 superseded，不再检索） | M1 不引入向量库；全文索引确定、可测；嵌入检索是 M2 的另一个检索器 |
| D5 | **ContextPackage 是组装结果，不可变、内容寻址**：`POST /v1/context/resolve`（query、scope、purpose、budget、filters、sensitivityCeiling）→ 检索（对调用者可见的记忆与知识）→ 过滤（过期、撤销、超出敏感上限、ACL 不可见）→ 排序（信任级别 > 新鲜度 > 文本相关度）→ 装包（按 token 预算，4 字符 ≈ 1 token；按来源引用去重）→ 密封（规范化 JSON + sha256）并保存。每一项带 kind（memory / knowledge）、source、ref、excerpt、observedAt、validUntil、trust、sensitivity、score。`GET /v1/context/packages/{id}` 读回；引用了之后被撤销或过期项的包标 `stale: true` 并列出受影响项 | 让 HOST / EER 能引用"模型当时看到的上下文"；撤销传播（P0）落在读回而不是改历史 |
| D6 | **撤销传播**：`POST /v1/memory/revoke`（按记录 id 或来源引用）把匹配的记忆标 revoked，记审计；文档撤回同理（`DELETE /v1/knowledge/documents/{id}` 是逻辑撤回）。到期由 Worker 定时扫描标 expired。两者都不从索引里物理删除内容，只让它们在检索里消失、在包的读回里显示为 stale | P0：撤销传播、时效评估 |
| D7 | **身份、租户、审计、指标、追踪、TLS 与 HOST 同款**（OAuth bearer JWT、FORCE RLS、只追加审计、Prometheus、OTLP）。角色：`cmi-admin`（来源、文档、策略）、`cmi-writer`（写记忆、撤销自己来源的记忆）、`cmi-reader`（解析上下文）。ACL 判定在 SQL 里：行对调用者可见当且仅当租户相同、且（记录的 subjects 为空或含调用者、或记录的 roles 与调用者角色相交）、且 sensitivity ≤ 调用者的上限（角色映射：reader → internal，writer → confidential，admin → restricted，可在租户策略里下调） | 同一套运维与安全口径；跨租户不可见由数据库保证 |
| D8 | **依赖尽量少、代码复制不共享**：pgx、golang-jwt、OpenTelemetry、yaml.v3；identity、transport、canonical、observability、store 骨架从 idp-host 复制（HOST ADR D13） | 仓库文档禁止跨仓库引用未发布的依赖 |
| D9 | **契约 `idp.telos.cmi` 1.0.0**：ContextPackage、ContextItem、MemoryRecord、Source、KnowledgeDocument（由 `api/contracts` 产出并有实现侧一致性测试）；REST 由 `api/openapi.yaml` 描述并由测试钉住与路由一致 | idp-contracts 的四条规则 |
| D10 | **与 HOST 的联调（CMI 侧）**：HOST 作为 `cmi-reader` 调 `resolve`，把 ContextPackage 的条目作为 developer 消息交给模型，并在 Run 上记录 `contextPackageId`；HOST 侧的改动（发布件的 `context` 配置、契约 1.6.0）在 CMI M1 通过后做——已做：idp-host ADR-0001 第 14 节 D32，`idp.telos.host` 1.6.0（2026-11-04） | HOST ADR 第 3 节留下的"与 CMI 的 Context API 联调"空位 |

## 3. 第一阶段范围（M1）

**包含**：D1–D9 的对象、API 与 Worker（到期扫描）；PostgreSQL 迁移与 RLS；单元测试；真实 PostgreSQL 上的集成测试（ACL 与敏感上限、租户隔离、来源优先级、时效与撤销传播、包的内容寻址与 stale 读回、预算装包）；contract 1.0.0；Kubernetes 清单待 M2。

**不包含（后续阶段）**：嵌入检索与语义索引；记忆整合（consolidation）；连接器（connectors）与处理 Worker；实体索引；评估（evaluation）；与 EER 的联调。

## 4. 工程结构

Go module `idp-cmi`，单个可执行程序 `cmd/idp-cmi`（`serve` = API + 到期扫描 Worker、`migrate`、`ensure-login`、`seed-tenant`、`dev-token`）。目录：`internal/{context,memory,knowledge,api,authorization,observability,transport,canonical}`、`adapters/{identity,store/postgres}`、`api/{openapi.yaml,contracts/}`、`migrations/`、`tests/integration`、`deploy/`。与建议稿的差异：`context/{resolver,assembly,packaging,validation}` 合为 `internal/context`；`memory/{semantic,episodic,lifecycle}` 合为 `internal/memory`；`retrieval` 在 `internal/knowledge`；`provenance`、`permissions` 是对象上的字段与 SQL 策略而不是包；`connectors`、`processing-workers`、`entity-index`、`evaluation` 进入后续阶段。
