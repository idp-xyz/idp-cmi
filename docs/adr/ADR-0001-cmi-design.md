# ADR-0001　IDP CMI 的设计（Context, Memory & Knowledge Infrastructure）

| 项 | 内容 |
|---|---|
| 状态 | 已接受（2026-11-04，决策人选择按建议顺序开始缺失的子系统，CMI 先于 Workspace；同日决策人要求先把 CMI 的后续阶段做完，见第 5 节） |
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

## 5. M2–M4：后续阶段（2026-11-04 决策）

决策人 2026-11-04 选择在 Workspace 之前把第 3 节列为"后续阶段"的条目做完，分三个里程碑：M2（检索与治理补全、交付件）、M3（记忆生命周期：整合、连接器与处理 Worker、评估）、M4（与 EER 的联调）。约束不变：不引入向量库产品，不让记忆取代事实，所有来自 agent 的内容仍是 informational 且按不可信输入处理（《Repository & Engineering Architecture》08：对 Memory 按不可信输入处理，避免记忆污染）。

| # | 决策 | 理由 |
|---|---|---|
| D11 | **嵌入检索作为第二个检索器（混合检索）**（M2）。配置文件 `embedding`（OpenAI 兼容 `POST /v1/embeddings` 的 baseUrl、model、维度 ≤ 4096、可选 apiKey）；配置了嵌入时，记忆与 chunk 在写入时同步取嵌入存入 `embedding real[]`（取不到不阻塞写入，行留空，由回填 Worker 补），查询时同样取嵌入；相似度在 SQL 里以 `real[]` 的余弦函数计算（租户 + 可见性谓词之内的暴力比较；ANN 索引如 pgvector 留作部署选项，本仓库不依赖扩展）。候选 = 全文命中 ∪ 向量 top-N；文本相关度 = max(归一化 ts_rank, 余弦)；排序仍是信任 > 新鲜度加权相关度；条目增 `match`（lexical / semantic / both）。未配置嵌入时行为与 M1 相同。测试用确定性的假嵌入器（词袋哈希向量）；真实嵌入端点的门禁留给有端点的环境 | 全文检索对同义改写召回差；但向量只是"另一条召回路"，来源、时效、权限、撤销的边界不变。不依赖扩展是因为本仓库必须在任何 PostgreSQL 上可测可运行 |
| D12 | **记忆污染防线在写入时**（M2）。写入记忆与索引文档时用确定性注入规则（与 HOST D27 同一组模式，代码复制）扫描内容；命中的记录带 `flags`（如 `injection:instruction_override`），文档按 chunk 标记、文档汇总 `flags`。被标记的记忆与 chunk **默认不进入上下文包**（租户策略 `includeFlagged`，缺省 false；打开时进入但条目带 flags，HOST 侧仍有 D27 护栏）；管理员以 `GET /v1/memory/records?flagged=true` 审阅。整合（D15）产出的记忆同样过扫描，命中则不落库并记审计 | 审计 P0"污染测试"：agent 写入的记忆是最可能被投毒的入口，拦在写入时比在每次组装时便宜且可审计 |
| D13 | **实体索引是显式声明的引用**（M2）。记忆与文档可带 `entities: [{type, id}]`（type 为名字，id 为引用，≤ 32 个），存入 `entity_refs`；`resolve` 增 `entities` 过滤（任一匹配即取），条目带 `entities`；`GET /v1/entities/{type}/{id}` 列出调用者可见的、引用该实体的记忆与文档。不做自动实体抽取 | 自动抽取不确定且难测；显式引用让 HOST / EER 以订单号、客户号把上下文钉在业务对象上，正是"事实的引用而不是事实本身"（D1） |
| D14 | **交付件**（M2）：Kubernetes 清单（与 HOST 同样的加固、`tests/deploy` 一致性测试）、compose 拓扑与 `deploy/e2e.sh`（PostgreSQL + 迁移 + CMI 容器 + `tests/e2e`；带 `HOST=1` 时再起 idp-host 镜像与假模型，HOST 发布件 `context.cmi = true`，证明 D10 / HOST D32 在真实进程之间成立） | HOST ADR 第 14 节的局限之一就是"与真实 idp-cmi 进程的联调未做" |
| D15 | **记忆整合**（M3）：Worker（间隔 `IDP_CMI_CONSOLIDATE`，0 关闭）或管理员 `POST /api/v1/memory/consolidate` 把同一（scope、sensitivity、ACL）组里、上次整合之后新增且 ≥ 策略 `consolidateMinRecords`（缺省 5）条的 active episodic 记忆交给配置文件 `consolidation` 一节的 OpenAI 兼容聊天模型，要求只输出 JSON 陈述列表；每条陈述成为一条 semantic 记忆：来源 = 配置里登记的 agent 来源（必然 informational）、`provenance.ref = consolidation:<id>`、`structured.derivedFrom` = 输入记录 id、sensitivity = 输入的最高级、ACL = 该组的、有效期 = 策略 `consolidatedTtlDays`（缺省 90）。输入记忆不删除不改。整合记录表 `consolidations` 存输入、输出与模型；输出先过 D12 扫描。测试用假模型；本机 Qwen 的真实门禁为 `make test-live` | 架构图："CMI 提炼经验"。整合是最像"把历史当事实"的地方，所以产出永远 informational、带可追溯的 derivedFrom、有有效期 |
| D16 | **连接器与处理 Worker**（M3）：`connectors`（属于一个 document-store 来源；kind：`http-manifest` = 一个列出文档 {uri, url, title, sensitivity, acl, entities} 的 JSON 清单 URL；`filesystem` = CMI 主机上的目录，开发用）；`POST /api/v1/connectors`、`POST /api/v1/connectors/{id}/sync`（管理员）生成 `ingest_jobs`，Worker 以租约领取（与 HOST Worker 同款）：抓取 → 限制（≤ 4 MiB；text/plain、text/markdown、text/html 去标签、application/json）→ 内容摘要未变则跳过 → `CreateDocument`（走 D12 扫描与 D11 嵌入）；失败重试 ≤ 3；`GET /api/v1/connectors/{id}/jobs`。抓取只允许 url 以连接器登记的 baseUrl 为前缀 | 知识不该靠人手 POST 文本；Worker 的租约与重试和 HOST 一样可观测 |
| D17 | **评估**（M3）：`POST /api/v1/evaluations`（管理员）对本租户跑固定套件并保存证据：污染（带注入文本的探针记忆必须被标记且不进包）、来源优先级（同一查询下 authoritative 探针先于 informational 探针）、撤销传播（解析 → 撤销 → 读回 `stale` 且新解析不再含它）、时效（已过期探针不进包）、敏感与 ACL（restricted 探针对 reader 不可见）。探针是真实记录（tag `cmi-eval`、唯一随机词、短 TTL），套件结束时撤销；结果 PASSED / FAILED 与每项证据存 `evaluations`，`GET /api/v1/evaluations/{id}` 读回 | 审计 P0 要求的证据正是"污染测试、撤销传播、时效评估"；跑在真实数据路径上而不是单元测试里，才算证据 |
| D18 | **与 EER 的联调**（M4，EER 侧）：EER Intelligence worker 以工作负载身份（cmi-reader + cmi-writer）在决策前 `resolve`（purpose = decision，scope = thread:<executionId>，实体 = 执行引用的业务对象），把包条目作为决策输入并在证据里记 `contextPackageId`；决策后追加一条 episodic 记忆（来源 `eer`，agent 种类）。Checkpoint 与执行状态不写入 CMI（《Repository & Engineering Architecture》：不可把执行 Checkpoint 写入 CMI 记忆）。**已做（2026-11-04）**：idp-eer ADR-037；`DecisionProposal.context_package_id`（eer/v1 可选新增）；CI `cmi.yml` 对真实 idp-cmi 跑通 | 第 3 节留下的空位；写什么、不写什么由仓库文档已定 |

**契约**：D11–D13 进入 `idp.telos.cmi` 1.1.0（MemoryRecord 增 `entities`、`flags`；KnowledgeDocument 增 `entities`、`flags`；ContextItem 增 `entities`、`match`；新定义 Entity），只做新增。D15–D17 的对象（Connector、IngestJob、Consolidation、Evaluation）是 CMI 自己的管理对象，由 `api/openapi.yaml` 描述，不进契约。

## 6. M2 实施结果（2026-11-04，WSL 实测）

- 混合检索（D11）：配置文件 `IDP_CMI_CONFIG` 的 `embedding` 一节（OpenAI 兼容 `/v1/embeddings`）；记忆与 chunk 写入时同步取向量存 `embedding real[]`（取不到留空），回填 Worker 每 `backfill` 间隔补齐缺向量或换了模型的行；SQL 函数 `cosine_similarity(real[], real[])` 在可见性谓词之内暴力比较；候选 = 全文 ∪ 向量，相关度 = max(归一化 ts_rank, 余弦)，条目带 `match`。未配置时与 M1 相同。
- 污染防线（D12）：`internal/guard`（与 HOST D27 同一组规则加不可见字符）在写入时扫描；记录 `flags`，文档按 chunk 标记并汇总；解析默认排除被标记的记忆与 chunk（其余 chunk 照常）；`GET /v1/memory/records?flagged=true`（管理员）审阅；策略 `includeFlagged` 打开时条目带 flags。
- 实体索引（D13）：`entities` 写入 `entity_refs`；`resolve` 的 `entities` 过滤；`GET /v1/entities/{type}/{id}` 列出可见引用；条目带 `entities`。
- 交付件（D14）：`deploy/kubernetes`（与 HOST 同样加固，`tests/deploy` 一致性测试）、`deploy/compose.yaml` + `deploy/e2e.sh` + `tests/e2e`；`HOST=1` 时起 idp-host 镜像与其假模型：HOST 发布件 `context.cmi = true` 的运行记录了 `contextPackageId`，事件 `idp.host/context.resolved` 的包 id 与摘要与 CMI 中保存的包一致，包的 `requestedBy` 是 HOST 的身份、scope 是运行的线程。
- 契约 1.1.0 发布（Entity；MemoryRecord / KnowledgeDocument 的 entities、flags；ContextItem 的 entities、match、flags），只做新增。

**验证**：单元（guard、embedding 假与 OpenAI 线格式、config）、契约（1.1.0：示例含实体、标记记录与三种 match，5 种新拒绝用例）、集成（新增 hybrid-flags-entities：同义改写只被向量命中、实体过滤与实体视图、跨租户为空、注入记忆被标记并隔离、策略放行后条目带 flags、部分被标记的文档只有干净 chunk 可解析、回填后原本无向量的记录被语义命中）、部署清单一致性、compose e2e（单 CMI；HOST=1 与真实 idp-host 进程联调）全部通过。

**局限（不标 PASS）**
- 真实嵌入端点未验证（本机模型服务没有 `/v1/embeddings`）；向量比较是暴力的，无 ANN 索引。
- 注入检测是确定性模式，不是分类器；它拦的是已知句式。
- HOST 联调用 static 凭据（HOST 以自己的 cmi-reader 身份解析），委派模式对真实 Keycloak 未验证。

## 7. M3 实施结果（2026-11-04，WSL 实测）

- 记忆整合（D15）：`internal/consolidate`（OpenAI 兼容聊天模型，只要 JSON；`ParseStatements` 容忍代码围栏与前言，≤ 5 条、每条 ≤ 300 字符）；分组 =（scope、sensitivity、ACL）里 active、未整合、未被标记的 episodic 记忆，≥ 策略 `consolidateMinRecords`（缺省 5），每组至多 50 条；产出以配置的 agent 来源写为 semantic 记忆（`provenance.ref = consolidation:<id>`、`structured.derivedFrom`、tag `consolidated`、有效期 `consolidatedTtlDays` 缺省 90 天、sensitivity 与 ACL 继承该组、实体为输入的并集），与"标记输入已消费 + 记 `consolidations`"在同一事务；产出先过 D12 扫描，命中的不落库并记入 detail。管理员 `POST /api/v1/memory/consolidate`（可限 scope）与 `GET /api/v1/memory/consolidations`；配置 `consolidation.interval` 时后台按租户轮询。
- 连接器与处理 Worker（D16）：`connectors`（`http-manifest`：清单 URL 与 `baseUrl`，只抓取其下的 URL；`filesystem`：CMI 主机上的目录，开发用）；`POST /api/v1/connectors/{ref}/sync` 枚举并按 uri 排队 `ingest_jobs`（已排队的不重复）；Worker 以租约领取（`FOR UPDATE SKIP LOCKED`）、抓取（≤ 4 MiB；text/plain、markdown、HTML 去标签与脚本、JSON）、内容摘要未变则 `skipped`，否则经 `CreateDocument`（D12 扫描、D11 嵌入、版本取代）；失败重试至 3 次后 `failed`；`scheduleSeconds` 到期自动 sync。
- 评估（D17）：`POST /api/v1/evaluations` 在本租户真实数据路径上写探针（唯一 nonce、tag `cmi-eval`、10 分钟有效期，结束后全部撤销）并证明五项：注入探针被标记且不进包；同一查询下 authoritative 先于 informational；撤销后旧包 `stale` 且新包不含；已过期探针不进包；restricted 探针只有管理员可见、角色绑定的探针只有持该角色者可见（管理员无该角色也看不到）。结果与证据存 `evaluations`。
- `tests/live`：本机 Qwen（OpenAI 兼容）对 5 条 episodic 便签产出 4 条事实陈述；加入一条注入便签后产出的 5 条陈述均无注入模式（写入时扫描仍会兜底）。

**验证**：单元（consolidate 解析、ingest 媒体类型与 HTML 转文本）、集成（新增 consolidation：来源缺失 404、非管理员 403、5 条便签 + 1 条被标记便签 → 1 组 1 条产出、被注入的产出被扫描丢弃并记录、产出可解析且 informational、输入被消费后再跑无组；connectors：baseUrl 之外的条目被丢弃、HTML 去标签入库并带实体、角色绑定文档只有持角色者可见、404 重试三次后 failed、内容未变的重同步 skipped、filesystem 连接器只收文本类文件、跨租户为空；evaluation：PASSED 且 5 项证据、探针不残留、跨租户为空）、部署清单一致性、compose e2e（迁移 0003）、live 门禁全部通过。

**局限（不标 PASS）**
- 整合与抓取的 Worker 在每个副本都跑；整合按租户串行，抓取按租约并行，但没有跨副本的整合互斥（同一组被两个副本同时整合时，后者在 `consolidation_id IS NULL` 的更新上写不到行，仍可能产出重复陈述）。
- 连接器只支持两种种类；没有增量清单（每次 sync 列全量，靠内容摘要跳过）。
- 评估套件覆盖审计 P0 列出的五类属性；它证明的是"此刻此租户的策略与数据路径"，不是对所有输入的穷尽。

## 8. M5：白皮书评审后的治理补全（2026-11-04 决策）

《IDP CMI 企业上下文与记忆基础设施白皮书》V1.0（2026-11-04）依据 10 月中的 Python 原型资料写成，与本实现有差异（V1.1 已对齐，见 `docs/IDP_CMI_Enterprise_Context_Memory_Whitepaper_V1.1.html`）；但它指出的下列缺口是实现确实没有的。决策人 2026-11-04 选择把它们作为 M5 范围。顺序按风险：先是让写入与撤销"可重放、可擦除"的 D19–D21，再是授权与作用域的 D22–D24，最后是检索一致性与评估的 D25–D27。

| # | 决策 | 理由 |
|---|---|---|
| D19 | **写入幂等与预期版本**：`POST /v1/memory/records` 与 `POST /api/v1/knowledge/documents` 接受 `Idempotency-Key` 头（每租户、每写者唯一）：同键同请求重放返回首次结果（200 而非 201，带 `Idempotent-Replayed: true`），同键不同请求 409 `IDEMPOTENCY_MISMATCH`；键保留 24 小时。撤销与状态转换接受 `If-Match: <recordVersion>`，记录增 `version`（每次状态变化 +1），版本不符 409 `VERSION_CONFLICT`。契约 1.2.0：MemoryRecord / KnowledgeDocument 增可选 `version` | 白皮书 10.2.1、15.1"已知缺口"：Worker 重试会产生重复记忆；状态转换没有并发保护 |
| D20 | **擦除是独立的受控过程**：`POST /api/v1/erasures`（管理员）按记录 id、来源引用、主体或实体发起擦除；同一事务内先对规范读路径设屏障（`erasure_id` 置于记录，所有读路径排除，包读回标 stale 并列出），再由 Worker 异步净化：明文内容、structured、向量、全文索引、实体引用、审计明文被替换为最小化墓碑（保留 id、来源名、时间、擦除原因摘要）；每个域分别记录 pending / done / failed；`GET /api/v1/erasures/{id}` 报告各域状态，只有全部域 done 才标 `verified`。备份不在本仓库承诺范围内，状态里明示 | 白皮书 7.5、9.3：失效、归档、擦除不能混为一谈；"接受删除请求"不等于"所有副本已删" |
| D21 | **候选与存疑状态**：MemoryRecord 的 `status` 增 `candidate` 与 `disputed`；写者可以 `status: candidate` 提交（整合与连接器的产出按租户策略 `consolidatedAsCandidate` 决定是否先为候选）；候选不进包；管理员或策略许可的审批者以 `POST /v1/memory/records/{id}/transitions`（带 `If-Match`、`reason`、可选 `approvalRef`）转为 active 或 revoked；active 记录与同 scope 内权威来源的新记录冲突时（同 `entities` 且 structured 中声明 `contradicts`）标 `disputed`，不进包直到解决。转换记审计 | 白皮书 7.1、7.4.1：未经验证的聊天与模型输出只能是候选；冲突不能静默覆盖 |
| D22 | **作用域扩展与多范围检索**：Scope 的 `kind` 增 `organization`、`team`、`project`；租户策略登记组织单元目录（`units: [{kind, id, parent?, members: [subjects], roles: [..]}]`，由管理员维护或由 IdP 声明映射），记录对某单元可见当且仅当调用者是其成员（或持其角色）；`resolve` 的 `scope` 变为可多值（`scopes: [...]`），缺省 = tenant + subject + 调用者所属全部单元 + 请求给出的 thread/agent；条目带 `scope`。个人记忆（subject 范围）发布到单元需 `POST /v1/memory/records/{id}/share`（显式、记来源、可撤回） | 白皮书 6.1–6.4：多范围、组织层级、个人到组织的显式共享 |
| D23 | **外部策略决策点（PDP）**：配置 `authorization.pdp`（HTTP，OPA / 自建均可）：每次读取与写入前以 {tenant, principal, roles, actor, action, resource{kind, scope, sensitivity, source, entities}, purpose} 询问；允许则继续并在 SQL 可见性之上再加 PDP 返回的 `obligations`（如 `maxSensitivity`、`excludeSources`）；拒绝则 403 并记审计；PDP 不可用时按策略 `pdpOnError`（`deny` 缺省）处理，绝不以缓存结论放行。委派（RFC 8693 `act`）作为 `actor` 传给 PDP | 白皮书 6.3、9.1：资源级、用途级授权由策略服务决定；CMI 的角色 + ACL 是纵深防御的内层 |
| D24 | **消费时复验**：`POST /v1/context/packages/{id}/verify` 返回 `{stale, staleItems, revisions}` 并可按 `since` 只报变化；HOST（D32）在运行的每一步开始前调用一次，发现 stale 时按发布件 `context.onStale`（`continue` 缺省 / `refresh` 重新 resolve / `fail`）处理；EER 在决策交付前调用。包读回与 verify 都不延长包的有效期 | 白皮书 8.2 回放边界、9.3.1：检索后、模型调用前的撤权窗口要有约定的交付边界 |
| D25 | **双时态查询**：记录已有 `validity`（业务有效期）与 `createdAt`（登记时间）；`resolve` 与 `GET /v1/memory/records` 增 `asOf: {valid?: time, recorded?: time}`：只取在 `valid` 时点有效、且在 `recorded` 时点已登记（且当时未撤销）的记录；撤销与过期保留时间戳，使"当时已知什么"可答。不承诺跨事务严格全序 | 白皮书 5.3：区分业务时间与登记时间；历史案例要标明当时适用的规则 |
| D26 | **投影一致性**：嵌入与全文索引改为 outbox 投影：写入事务只写规范行与 `projection_events`（记录 id、版本、代际）；投影 Worker 幂等消费（旧事件不覆盖新版本，撤销/擦除事件删除向量与索引）；`GET /api/v1/projections` 报告积压与代际；`resolve` 在积压超过策略阈值时在包里标 `projectionLag`。回填 Worker 并入投影 Worker | 白皮书 10.2：不要双写后假定一致；旧事件不能复活已撤销内容 |
| D27 | **评估集与基线对照**：`tests/eval` 增带标注的中文 / 英文评估集（脱敏、仓库内），度量 Recall@k、精确引用命中率、错误合并率、过期引用次数，并对照"无 CMI""只全文""全文 + 向量"三条基线；结果写入 `docs/eval/`，每次发布更新；D17 的套件继续作为租户级运行时证据 | 白皮书 14：公开基准不能代替企业数据上的对照；效果须可复验 |

**不做的**：MCP 适配器（HOST / EER 通过契约 API 调用即可，第三方消费者再议）；外部记忆引擎（Mem0 / Hindsight / Graphiti）接入（白皮书 10.4 的约束已满足"不依赖"，接入在有真实需求后评估）；候选自动提取（agent 写什么由 HOST 的发布件决定）。

**契约**：D19、D21、D22、D24、D25 进入 `idp.telos.cmi` 1.2.0（MemoryRecord 增 `version`、status 增 `candidate` / `disputed`、Scope kind 增三值、ContextItem 增 `scope`、ContextPackage 增 `projectionLag`、新定义 Verification），只做新增。

## 9. M5 实施结果（2026-11-04，WSL 实测）

- D19：`Idempotency-Key`（每租户、每写者；同键同请求 200 重放并带 `Idempotent-Replayed: true`，同键不同请求 409；键 24 小时后由扫描器清理）；记忆 `version`、文档 `revision` 随每次状态变化加一，撤销与转换接受 `If-Match`（不符 409）。
- D20：`POST /api/v1/erasures` 在同一事务内设读屏障（目标置 `erasure_id` 并撤销；所有读路径以 SQL 把正文、structured、来源引用、标题替换为 `[erased]`），Worker 按域净化（content、vectors、index、entities、packages、audit），包文档改写为墓碑并标 `redacted`（原摘要保留为"当时密封了什么"的记录），每域记录 done / failed，全部 done 才 `verified`；集成测试最后以服务角色扫全部表确认明文为零。
- D21：`candidate` / `disputed` 状态，`POST /v1/memory/records/{id}/transitions`（管理员、reason 必填、可带 approvalRef、If-Match），`structured.contradicts` 只允许权威来源的记录指认并把被指认的 active 记录标 disputed；策略 `consolidatedAsCandidate` 让整合产出先为候选；管理员以 `?status=candidate|disputed` 审阅。
- D22：`units` 目录（organization / team / project，成员与角色，可带 parent），单元范围的读写都要求成员资格（管理员例外）；解析缺省加入调用者所属全部单元，请求 `scopes` 命名非成员单元时 403；`POST /v1/memory/records/{id}/share` 由记录的 subject 或管理员把记录复制进单元（`sharedFrom`、tag `shared`，原记录不变）；删除单元后其记忆不可达。白皮书 14.3.2 的 Alice / Bob 场景以集成测试落实。
- D23：配置 `authorization.pdp`（OPA 数据 API 线格式）：解析、记忆读写撤销转换共享、知识写撤、擦除请求前询问；拒绝 403 `PDP_DENIED` 并记审计；义务 `maxSensitivity` / `excludeSources` 只收窄可见性（含来源排除进入两条检索 SQL）；PDP 不可达按 `onError`（deny 缺省 → 503 `PDP_UNAVAILABLE`；skip → 以 CMI 自身授权继续并告警）。
- D24：`POST /v1/context/packages/{id}/verify` 返回每一引用项此刻的状态、版本与可用性（契约 Verification / ItemStatus），不延长包的有效期。HOST 侧（idp-host 契约 1.7.0 `context.onStale`）在第二步起每步复验：continue 记 `idp.host/context.stale`、refresh 以静态凭据重新解析并替换上下文、fail 以 `CONTEXT_STALE` 结束；EER 侧（idp-eer ADR-037 §6）在交付前复验并把 `contextStale` 记进留下的记忆。
- D25：`asOf: {valid, recorded}` 进入 `resolve` 与 `GET /v1/memory/records`（`validAt` / `recordedAt`）：记忆按 `valid_from ≤ valid < valid_until`、`created_at ≤ recorded`、`revoked_at > recorded` 判定，文档另排除在 `recorded` 之前已有更新版本者；缺失的 recorded 轴取数据库的 now（登记时钟是数据库的）；包记录 `asOf`。
- D26：写入事务同时写 `projection_events`（kind、object、version、action）；投影 Worker 幂等消费（`FOR UPDATE SKIP LOCKED`，只在行仍为事件版本且 active 时写向量，旧事件与已撤销行不写），嵌入端点失败时事件留队并计 attempts；`GET /api/v1/projections` 报告积压与最旧事件年龄；包带 `projectionLag`。全文索引仍在写入事务内同步建立（它不是投影）。
- D27：`tests/eval/dataset.json`（20 条中英文查询，人工标注相关项与禁止项：撤销、过期、被标记）；`make test-eval` 对比只全文与全文 + 向量（确定性假嵌入器），度量 Recall@5、Precision@5、nDCG@5、禁止项与重复项，结果写入 `docs/eval/`，CI 校验结果未变。评审中发现向量路径的噪声近邻会在固定预算下挤掉高相关的 informational 条目（hybrid 一度低于 lexical），据此加入 `SemanticFloor = 0.35`（仅向量命中的候选低于此不予考虑）；当前 lexical Recall@5 0.8889、hybrid 0.9444，两者禁止项均为 0。
- 契约 1.2.0（D19、D21、D22、D24）与 1.3.0（D25、D26）均只做新增；1.3.0 待登记。

**验证**：单元、契约（1.3.0）、集成（新增 idempotency-and-versions、erasure、candidates-and-disputes、units-and-sharing、policy-decision-point、verify-at-delivery、as-of、projections 八个场景）、评估集、部署清单、compose e2e；idp-host 集成（cmi-verify-at-steps：fail / continue / refresh）；idp-eer 单元与契约（PR #22，CI 含真实 idp-cmi）全部通过。

**局限（不标 PASS）**
- 擦除不覆盖数据库备份与已交付给调用方的明文（状态里明示）；审计净化只处理 memory.create / knowledge.create 的 detail。
- 整合与投影 Worker 在每个副本都跑；投影靠 `SKIP LOCKED` 并行安全，整合仍无跨副本互斥。
- PDP 只验证了假 PDP；对真实 OPA 的策略包未写。
- 评估集是合成的、小规模的；真实嵌入模型的数字需在有端点的环境重跑。

## 10. M6：文档对照后的补齐（2026-11-04 决策）

白皮书 V1.1 与统一入口逐条对照实现后，决策人选择补齐其中四项属于 CMI 本体的缺口；其余（委派在 CMI 内判定、精确标识独立路由、包内 tokenizer 配置、成本度量）不做，理由记在对照记录（统一入口第 15.4 节）。密钥轮换、运行手册、备份内擦除、真实 OPA 策略包属于部署与运营，不在本仓库范围。

| # | 决策 | 理由 |
|---|---|---|
| D28 | **包报告它没放进去的东西**：ContextPackage 增 `omitted: {budget, flagged, candidate, disputed, forbidden, lowSimilarity}`——解析时分别统计因预算（token 或条目数）、写入时标记、候选、存疑、PDP 义务排除来源、低于相似度下限而未进入包的候选数（去重后的候选为单位，不含 ACL / 敏感上限 / 状态过滤——那些在 SQL 内发生且对调用者本就不可见，不报告以免泄露存在性）。契约 1.4.0 只加字段 | 白皮书 8.2：必要信息放不下时应明确报告预算不足，而非静默删除 |
| D29 | **按敏感级别决定什么可以送外部模型**：租户策略增 `externalModelCeiling`（缺省 `internal`）。敏感级别高于它的记忆与 chunk 不取向量（写入时不嵌入、投影事件直接标 processed、回填跳过），不进入整合分组；它们仍可被全文检索到。策略下调时已存向量由 Worker 清除；策略上调不回填（由回填 Worker 按 `embedding IS NULL` 自然补齐） | 白皮书 9.2、统一入口 7.2：敏感数据能否发给外部模型须先满足企业政策 |
| D30 | **审批引用可以被核验**：配置 `authorization.approvals`（HTTP）：带 `approvalRef` 的转换先向审批系统 `POST {approvalRef, tenant, recordId, to, principal}`，期望 `{valid: true, approvedBy, expiresAt}`；无效、过期、对象不符或服务不可达均 409 `APPROVAL_INVALID` / 503 `APPROVAL_UNAVAILABLE`（不以缓存放行）；通过的审批人与时效写入审计。未配置时保持只记审计，审计里标 `approvalVerified: false`。对候选的 `active` 转换可由策略 `requireApproval` 强制要求审批引用 | 统一入口 10.4：审批应由可信审批系统返回可校验决定并绑定对象、范围、时效 |
| D31 | **来源可追溯率进入评估**：D27 的评估对每个返回条目反查其来源（记忆 → `GET /v1/memory/records/{id}` 的 provenance 与来源名；知识 → 文档 URI 与 chunk 序号可定位），报告可追溯比例；目标 1.0，低于 1.0 失败 | 统一入口 15.2：返回记忆中具有可访问来源和正确版本的比例 |

**契约**：D28 进入 `idp.telos.cmi` 1.4.0（ContextPackage 的 `omitted`，新定义 Omitted），只做新增。


## 11. M6 实施结果（2026-11-04，WSL 实测）

- D28：解析对每条去重后的候选记录未进包的原因：`budget`（token 或条目数）、`flagged`（`includeFlagged` 关闭时）、`candidate`、`disputed`、`forbidden`（PDP 义务排除来源）、`lowSimilarity`（低于 `SemanticFloor` 的仅向量命中）；全为 0 时不出现 `omitted`。候选、存疑、标记与排除来源以与检索同一查询条件的全文计数得到，ACL、敏感上限与租户隔离过滤掉的不计。
- D29：策略 `externalModelCeiling`（缺省 internal，迁移 0009）。高于它的记忆与文档在写入时不嵌入；投影 Worker 对其事件直接完成而不调用端点；回填跳过；整合分组不收它们。全文索引照常建立。下调策略在同一事务内清除新上限以上的已存向量；上调后由回填 Worker 补齐。
- D30：配置 `authorization.approvals`（`url`、`caFile`、`timeout`）。带 `approvalRef` 的转换先请求审批系统，回答须 `valid`、有审批人、`expiresAt` 在未来，且回显的 `recordId` / `to`（若有）与请求一致；否则 409 `APPROVAL_INVALID`，不可达或非 200 为 503 `APPROVAL_UNAVAILABLE`，记录状态不变。审计记 `approvalVerified`、`approvedBy`、`approvalExpiresAt`；未配置审批系统时引用照记，`approvalVerified: false`。策略 `requireApproval`（迁移 0010）使"候选 → active"必须带引用，否则 409 `APPROVAL_REQUIRED`；拒绝（→ revoked）不需要。
- D31：评估对每个返回条目以同一读者身份反查：记忆回到记录（来源与引用一致、仍 active、来源已登记），知识回到该 URI 的现行文档与存在的 chunk 序号。`docs/eval/` 报告 `traceabilityRate`；lexical 与 hybrid 均为 1.0，低于 1.0 测试失败。
- 契约 `idp.telos.cmi` 1.4.0（D28 的 `omitted` 与 Omitted 定义）只做新增，已登记到 idp-contracts。D29、D30 只改管理接口与配置，不进契约。

**验证**：单元（含 `internal/approvals` 的有效、拒绝、过期、无时效、对象不符、转换不符、不可达）、契约（1.4.0）、集成（新增 omitted-report、approval-verification、external-model-ceiling 三个场景）、评估集（含可追溯率）全部通过。

**局限（不标 PASS）**
- 审批只验证了假审批系统；与真实工单或审批平台的对接需按其接口写适配。
- `externalModelCeiling` 只管 CMI 自己调用的嵌入与整合模型；包交付给 HOST 或 EER 之后送往哪个模型，由它们的策略决定。
- `omitted` 的计数是全文路径上的近似：仅向量命中的候选、存疑或被标记条目不计入。

## 12. 补充：整合的跨副本互斥（2026-11-04）

§9 的局限"整合仍无跨副本互斥"已解决：`Consolidator.RunTenant` 先取该租户的 PostgreSQL 会话级咨询锁（`Store.TryWorkLock`，键为 `idp-cmi:consolidate:<tenant>` 的哈希，持锁连接专用、崩溃即释放）。取不到时本副本不做任何事：定时任务跳过该租户，管理员手动触发得到 409 `CONSOLIDATION_RUNNING`。集成测试 consolidation 场景：另一会话持锁时调用返回 409 且模型未被调用；同一锁不能取两次；其他租户不受影响；释放后正常整合。

## 15. 成员擦除自己的记忆（2026-11-06）

起因：idp-host 开始把对话写成 subject 作用域的情景记忆（HOST ADR D33），成员应当能看到并删除"关于自己的记忆"，而他们不是写入者（`recordedBy` 是 idp-host），也没有 cmi-writer 角色。

| # | 决策 | 理由 |
|---|---|---|
| D26 | **`POST /v1/memory/revoke` 允许 cmi-reader 撤销自己 subject 作用域里的记录**：只能按 `recordId`（按 `ref` 撤销仍要 cmi-writer，否则 403），只限 `scope = {subject, 调用者}`，不论谁写的；别人的记录或不存在的 id 一律 404（不泄露存在性）；不带 If-Match；审计动作 `memory.revoke_own`。存储层是单独的 `RevokeOwnMemory`，不改原有 `RevokeMemory` 的规则（写入者撤销自己写的、管理员撤销任何） | 一个人有权让系统忘记关于他的事；范围限定到他自己的 subject 作用域，不给读者任何别的撤销能力。测试：`tests/integration` 的 `memory` 子测试（按引用 403、他人记录 404、自己的 200、重复 404、列表里消失、他人的记录没动） |

契约 `idp.telos.cmi` 不变（请求体不是契约文档）。

## 16. 生产就绪：限额、运行指标、成员导出（2026-11-09）

| # | 决策 | 理由 |
|---|---|---|
| D34 | **写入限额与速率限制**：配置 `limits {maxRecordsPerScope, maxRecordsPerTenant, writesPerMinute}`（0 = 不限）。超限返回 429：`QUOTA_EXCEEDED`（作用域或租户已满，撤销或过期后腾出空间）、`RATE_LIMITED`（带 `Retry-After`）。条数在插入前检查，不在同一语句内，并发突发可小幅超出；速率是每主体、每副本一分钟固定窗口，内存实现 | 失控的 Agent 或写错配置的写入者不能把库写满；限额按作用域计（HOST 用同一个服务身份写所有成员的记忆，按写入者计无意义） |
| D35 | **运行指标与告警**：`idp_cmi_memory_writes_total{outcome}`、`idp_cmi_projection_pending`、`idp_cmi_projection_oldest_age_seconds`、`idp_cmi_db_up`；`deploy/observability/alerts.yaml`（SLO 记录规则与 8 条告警，promtool 单测）；SLO：可用性 99.5%、resolve p95 1 s、投影滞后 10 分钟 | 没有告警就是靠用户发现故障；指标名由测试保证存在，改名不会让告警静默失效 |
| D36 | **成员导出自己的记忆**：`GET /v1/memory/export`（cmi-reader 或 cmi-writer）返回调用者 subject 作用域里 CMI 当前视为有效的记录，信封 `idp-cmi.memory-export.v1`，`Content-Disposition: attachment`，超过 500 条时 `truncated: true`；永不包含别人的记录 | 数据主体访问权；与 D26（成员删除）配套 |

契约 `idp.telos.cmi` 不变（导出信封与 429 不是契约文档）。局限：限额计数不是原子的；速率限制按副本；导出只含有效记录，不含已过期、已撤销或待审的记录。

## 17. 检索质量：评测、真实嵌入模型、pgvector（2026-11-09）

起因：生产就绪盘点里"检索质量"一项。先建评测，再按数据决定，而不是凭感觉。

**评测集**（`tests/eval/dataset_large.json`，`docs/eval/LARGE.md`）：虚构的包裹承运商手册（中英文）、客户与订单记忆，88 条查询分 10 类（同义改写、关键词、跨语言、标识符、多答案、近似干扰、个人、线程、租户、无答案）。指标：Recall@1/3/5/10、MRR、nDCG@10、"无答案时返回空"的比例、禁止项（撤销、过期、被标记、他人）。`make test-eval` 校验全文基线逐字节不变；真实嵌入端点用 `IDP_EVAL_EMBEDDING_*` 环境变量接入（TEI 在本机 CPU 上跑通）。

两套数据，同一组冻结之前选定的配置（自建集 88 条查询，用来选；留出集 97 份文档 / 321 条查询，模型模拟，从不用来选阈值）：

| 配置 | 自建集 R@5 / MRR / 无答案返回空 | 留出集 R@5 / MRR / 无答案返回空 |
|---|---|---|
| 只全文 | 0.635 / 0.624 / 4 of 10 | 0.536 / 0.448 / 6 of 30 |
| Qwen3-Embedding-0.6B，原融合（取较大者，下限 0.35） | 0.891 / 0.760 / 3 of 10 | 0.577 / 0.473 / 5 of 30 |
| 校准，下限 0.45，混合 0.05 | 0.936 / 0.849 / 8 of 10 | 0.852 / 0.805 / 23 of 30 |
| **校准，下限 0.40，混合 0.05（推荐）** | **0.962 / 0.864 / 7 of 10** | **0.921 / 0.847 / 20 of 30** |
| 下限 0.50 | — | 0.708 / 0.696 / 23 of 30 |
| 校准 0.45 + 重排（10 个候选，下限 0.05） | 0.942 / 0.917 / 10 of 10 | 0.790 / 0.766 / 30 of 30 |
| 校准 0.40 + 重排（下限 0.01） | — | 0.887 / 0.832 / 29 of 30 |
| granite-embedding-97m-multilingual-r2（下限 0.75） | 0.891 / 0.808 / 1 of 10 | — |

| # | 决策 | 理由 |
|---|---|---|
| D37 | **嵌入要按模型校准**：配置 `embedding.queryPrefix` / `documentPrefix`（非对称模型的指令前缀；文档前缀的哈希进入 `embedding_model`，改了就重算，查询前缀不影响存量向量）、`floor`（余弦下限）、`lexicalBlend`（> 0 时相关度改为"余弦 + blend × 归一化全文分"，有向量的候选低于下限一律丢弃，没有向量的候选按全文半权保留）。默认值不变（0.35、旧融合），所以旧部署的行为不变 | 原融合里最佳全文命中归一化后恒为 1，垃圾查询也会返回东西（无答案返回空 3/10）；不同模型余弦分布差很多（Qwen3 在 0.45 附近分界，granite 要 0.75 以上），常数 0.35 对谁都不对。查询指令前缀对 Qwen3 有小幅收益（R@5 0.878 → 0.891）。**推荐 `floor` 0.40、`lexicalBlend` 0.05（Qwen3-Embedding-0.6B）**：我最初在自建集上选了 0.45，留出集显示 0.45 以上召回断崖式下降（0.50 时 R@5 掉到 0.708），而多拦住的无答案查询只多 3 条；0.40 在两套数据上召回都最高。这是只在一套数据上调参会得出错的结论的实例。**记忆另有下限 `embedding.memoryFloor`（0 = 同 `floor`；Qwen3 推荐 0.20）**：`floor` 是在知识分块上校准的，一条记忆是一整轮对话，答案常是其中顺带的一句，和问题的余弦更低。LongMemEval（`docs/eval/LONGMEMEVAL.md`，470 题）上含答案的轮次有 40% 低于 0.40，被整条丢掉，混合检索反而不如纯全文（any@5 0.851 对 0.875）；记忆下限 0.20 时为 0.930，全部证据进前 10 从 0.672 升到 0.857。代价：自建记忆集里问没说过的事时“什么都不返回”从 0.34 降到 0（`docs/eval/MEMORY.md`），拒答交给阅读模型。知识不能跟着降：留出集上 0.20 时无答案返回空只剩 2/30（0.40 时 20/30） |
| D38 | **可选 pgvector**：`Migrate` 在扩展可用时（幂等，晚装也行）加 `embedding_v vector` 列，用触发器与 `real[]` 保持同步（擦除、撤销清空数组时副本同步清空，写入路径一行不改）；语义检索改用原生 `<=>`；`idp-cmi vector-index DIMS` 为每种向量维度建部分表达式 HNSW 索引（`CONCURRENTLY`，`maintenance_work_mem` 可设，默认 1GB），查询自动识别并设置 `hnsw.ef_search ≥ LIMIT` 与迭代扫描。没有扩展时仍用 SQL 函数，行为不变；两条路径在评测集上给出同样的指标 | 实测（本机，1024 维）：SQL 函数逐行扫描 5 000 行 0.8 s（十万行按线性外推约 16 s，未实测）；原生精确扫描 50 000 行 22 ms；HNSW 在 5% 选择度过滤下 10–22 ms。HNSW 构建 50 000 × 1024 在默认内存下 233 s，所以构建要给内存。`LIMIT 200` 而 `ef_search` 默认 40 会静默只返回 40 行，查询侧必须设置 |
| D39 | **可选交叉编码器重排，默认关闭**：配置 `rerank {url, style tei\|cohere, model, top, floor}`。检索与打分之后，对最好的 `top` 个候选（默认 30，建议 10）用重排模型重新打分，相关度换成重排分，低于 `floor` 的丢弃，其余按它排序；候选的 `Score` 仍乘新鲜度。重排失败（超时、响应不完整）时保持之前的顺序并记警告，检索不受影响 | 交叉编码器把查询和文本一起读，它的分数对每个查询含义一致，所以一个阈值就能判断"这里没有答案"（嵌入余弦做不到）。**但它是取舍，不是白赚**：自建集上它显得很好（MRR 0.849 → 0.917），留出集上只排序不过滤时 MRR 仅 +0.012（0.805 → 0.817），加下限后无答案能全拦住（20 of 30 → 29 of 30），代价是召回少 3 到 7 个点（下限 0.40 加重排 0.01：R@5 0.921 → 0.887；0.45 加 0.05：0.852 → 0.790）。CPU 上约 0.85 s 每查询（10 个候选，p95 约 1.7 s）。所以：默认关；只在"宁可不答也不要答错"的场景下开，下限从 0.01 起、在自己的数据上测 |

局限与待办：两套评测数据都不是真实语料。自建集 88 条，既用来选又用来评；留出集由聊天模型模拟，它根据文档写出的问题措辞往往接近原文，所以对全文基线偏友好，嵌入的优势在真实问题上可能更大，也可能不同。两套数据给出的最佳下限不同（0.40 到 0.45）而召回在 0.45 以上陡降，说明阈值对语料敏感：**上线前必须在真实文档和真实问题上重测**，并按需给下限留余量。嵌入模型测了 Qwen3-Embedding-0.6B、granite-97m、multilingual-e5-large-instruct（R@5 0.827，余弦分布很高，下限 0.85 才能让无答案返回空，但召回掉到 0.744；该组数字未入库）；bge-m3（仓库只有 onnx）、jina v5、Qwen3-Embedding-4B/8B 未测（EmbeddingGemma 2 见本节末）；重排只测了 bge-reranker-v2-m3，生产要放 GPU 或接托管服务，GPU 路径尚未试；标识符类查询在语义主导模式下有一例（`HS 9503.00`）掉到下限以下，实体（`entities`）仍按精确匹配；多模态未做；pgvector 对每种维度需单独建索引，换模型要重建。Kubernetes 与 compose 里的 PostgreSQL 镜像已换成带 pgvector 的（kind 验收与备份演练在其上通过）。

**补测 EmbeddingGemma 2（2026-11-12）。** `google/embeddinggemma-2`（提交 f9972ac，Apache 2.0，只加载文本部分 270M，BF16，768 维）。text-embeddings-inference 1.9.4 还不认识它的架构（`embedding_gemma2`），所以用 sentence-transformers 6.3 临时起了一个 OpenAI 兼容端点（脚本未入库）；查询前缀 `task: search result | query: `，文档前缀 `title: none | text: `。它的余弦分布整体偏高（相关约 0.8、不相关 0.6～0.7），下限要按它重新校准：自建集上 0.68 最好，留出集上 0.66 最好，与 Qwen3 的 0.40 不能通用。

| 配置 | 自建集 R@5 / MRR / 无答案返回空 | 留出集 R@5 / MRR / 无答案返回空 | 留出集跨语言 R@5 / MRR |
|---|---|---|---|
| Qwen3-Embedding-0.6B，下限 0.40（现行） | 0.962 / 0.864 / 7 of 10 | 0.921 / 0.847 / 20 of 30 | 0.93 / 0.81 |
| EmbeddingGemma 2，下限 0.68 | 0.936 / 0.831 / 6 of 10 | 0.914 / 0.777 / 27 of 30 | 0.82 / 0.55 |
| EmbeddingGemma 2，下限 0.66 | 0.923 / 0.817 / 5 of 10 | 0.938 / 0.779 / 24 of 30 | 0.85 / 0.54 |

同语言的改写与关键词它略好（留出集改写 R@5 / MRR 0.98 / 0.87，Qwen3 0.91 / 0.83），**中英跨语言明显差**（MRR 0.54 对 0.81），而中文问题查英文资料（或反过来）正是 CMI 语料的常态。结论：不换，Qwen3-Embedding-0.6B 仍是推荐模型。它的长处是多模态，而 CMI 现在只收文本（记忆、知识文档、连接器只取文本类内容），用上多模态先要有接入、存储和"命中一张图时交给阅读模型什么"的设计，那是产品范围的事，不是换模型。LongMemEval 没有跑完（这个端点比 TEI 慢约三倍，全量约 85 分钟）：它只能回答英文对话记忆，结果怎样都不改变上面的结论。该组数字未入库。

## 18. 记忆评测与事实抽取（2026-11-09）

起因：对照 Mem0、Graphiti、Hindsight 等记忆系统的想法时，先问"我们现在的记忆到底差在哪里"。idp-host 现在每轮对话写一条"用户说了…助手答了…"的情景记忆（HOST D33）。

**评测**（`tests/eval/dataset_memory.json`、`memory_test.go`、`gen_memory.py`，结果 `docs/eval/MEMORY.md`）：25 个人物、1750 轮对话、6 个个人属性（四成被更新一到两次，夹杂闲聊和"同事的属性"干扰），350 条提问。同一份对话存两种：**原样存回合**（HOST 现在）和**抽取成事实**（每个事实一条，同属性的旧事实关闭有效期；这里抽取是精确的，所以是上限）。

| 存法（Qwen3 混合，下限 0.40），被更新过的属性 170 条 | 当前值第一 | 当前值在前 5 | 前 5 含过期值或同事的值 |
|---|---|---|---|
| 原样存回合（HOST D33） | 0.45 | 0.92 | 0.92 |
| 抽取成事实，精确（上限） | 0.96 | 0.99 | 0.00 |
| **抽取成事实，idp-host 的 Extractor + 本机 Qwen3.8-flash，第一版提示词** | 0.71 | 0.80 | 0.20 |
| **同上，改进后的提示词（HOST D34）** | **0.79** | **0.89** | **0.06** |

全文检索基线：当前值在前 5 仅 0.21（原样）/ 0.54（事实）。语义检索解决了召回；**原样存回合的问题是过期值**：被更新过的属性有 93% 的提问会同时取回旧值，模型要自己判断哪个是新的。事实抽取并关闭旧值的有效期后，旧值不再出现。问从未说过的事，原样存只有 4% 返回空，事实 34%（同一个人的其他属性也与问题相近），真实抽取同为 34%。

| # | 决策 | 理由 |
|---|---|---|
| D40 | **做事实抽取，分两步**：(1) CMI 提供原子的事实写入 `POST /v1/memory/facts`（主体作用域、属性键、取值、内容、出处）：同一事务里关闭同一主体同键的现行事实的有效期并写入新事实，同值重复写入只刷新；(2) idp-host 在每轮之后用模型**只从用户的话里**抽取事实再调用它，不再把助手的回答写进记忆 | 评测显示召回已不是瓶颈，过期值才是；"关闭有效期"用的是已有的双时间模型（D25），不需要图或新存储。只存用户的话还去掉了"助手答错被当成事实"的污染 |
| D41 | **事实写入的语义**：`POST /v1/memory/facts {key, value, scope, content, provenance, sensitivity, ...}`（cmi-writer，检查、配额、幂等键与 `POST /v1/memory/records` 相同）。事实是 `structured = {"fact": {"key", "value"}}` 的语义记忆，同一（租户、作用域、键）同一时刻至多一条现行值。在一个事务里、持（租户|作用域|键）的咨询锁：找出现行值 → 取值相同（忽略大小写和空白）则 `unchanged`（200，不写）；现行值比这条陈述更新则 409；否则把现行值的 `valid_until` 关到新值的 `valid_from`（状态仍是 active，版本加一，审计 `memory.fact.close`）并写入新记录（走 `insertMemory`：写入时注入扫描、嵌入、实体、投影都照常），返回 `created` / `updated` 与被关闭的记录 id。不接受 `validUntil`（下一个值就是它的终点）。迁移 0011 加部分表达式索引 | 关有效期而不是撤销或标"被取代"：D25 的双时间查询仍能回答"某天它是什么"，而可用谓词（`valid_until` 在未来）只看现行值，不需要改任何读路径；咨询锁让并发更新留下恰好一个现行值（测试：8 个并发写者）；评测里事实经此接口写入，全文基线的结果与手工关闭有效期的逐位一致 |

局限：精确事实是上限；真实抽取已经接进同一评测（`idp-host` 的 `TestExtractOnTheMemoryDataset` 产出 `tests/eval/extracted_*.json`，`IDP_EVAL_EXTRACTED` 读入）：1750 条用户消息，1.3% 的调用失败，268 条真值事实抽到 252 条（94%；第一版提示词 89%），键名更接近一个属性一个键（158 个，理想 150；第一版 168）；用的是一个模型（本机 Qwen3.8-flash），换模型要重测，且提示词是在这批数据上改的，换成真实对话要再看；属性是我设计的 20 个，真实对话的属性分布更杂；闲聊、同事的属性是模型写的，比真实的更干净；助手回答污染这一项只是间接涉及。

## 19. 安全评审后的加固（2026-11-10）

评审记录在 `docs/security/REVIEW-2026-11-10.md`。最重要的一条：**别人的个人记忆原来对任何同租户的 `cmi-reader` 可读**（只要显式指名那个作用域），之前的"记忆是私有的"验收没有覆盖到这一点。

| # | 决策 | 理由 |
|---|---|---|
| D42 | **(1) 个人记忆只有本人、写入它的服务（`cmi-writer`）和管理员能指名**：列表的 `scopeKind=subject&scopeId=`、resolve 的 `scope`/`scopes`、按 id 读取（404）都检查；对别人记忆的读取写审计 `memory.read_other`。**(2) 连接器的出站请求**：`cmi.URLWithin` 按解析后的协议、主机、端口与路径段边界判断"在 baseUrl 之下"（不再是字符串前缀）；连接器客户端不走环境代理、连接时拒绝非公网地址（`IDP_CMI_INGEST_ALLOW_PRIVATE=true` 放开）、重定向必须仍在 baseUrl 之下。**(3) 文件系统连接器默认禁用**，`IDP_CMI_FILESYSTEM_ROOT` 指定唯一根目录，解析符号链接后必须在其下，遍历不跟随符号链接。**(4) JWKS 必须是 https**（`IDP_CMI_ALLOW_INSECURE_JWKS=true` 显式放行开发网络），密钥在颁发方不可达时最多再用 24 小时。**(5) 开发模式在非回环监听上拒绝启动**，除非 `IDP_DEV_ALLOW_REMOTE=true`。**(6) `go.mod` 固定 `toolchain go1.26.10`**（本机 1.26.8 的二进制带 10 个标准库漏洞），CI 增加 `govulncheck` | 这些都是"默认就不安全"的行为；修复把安全作为默认，把不安全变成需要显式写出的运营选择，并在平台的开发配置里显式写明 |

未修复：线程作用域没有所有者（O1）、连接器请求头仍可带类凭据头（O3）、PDP `onError: skip` 的 fail-open（O5，需显式配置）。

## 20. 公开基准 LongMemEval（2026-11-11）

自建的记忆评测集是我们自己编的，分数没法和别人比。加了公开基准 [LongMemEval](https://github.com/xiaowu0162/LongMemEval)（500 题，每题约 50 次会话的历史），按 idp-host 的写法入库、按论文的会话级指标量检索，结果在 `docs/eval/LONGMEMEVAL.md`。它带出了两个决定：记忆单独的相似度下限（写在 D37 里：`embedding.memoryFloor`），和下面这条。

| # | 决策 | 理由 |
|---|---|---|
| D43 | **包内条目按相关度排，不按事件时间调整；消费方必须把每条的 `observedAt` 交给阅读模型**。考虑过并放弃的两种做法：分数相近的按时间排（新的在前）、包内说同一件事的条目之间新的在前。评测里也不再量“最新的证据是不是排最前”，改量“最新的证据在不在前 k 条里” | 起因：问一件后来改过的事，旧的那次有 29% 排在最新那次前面，担心模型照旧值回答。实验（`tests/eval/longmemeval_reader.py`，72 道知识更新题，同样的 10 条，只换顺序和带不带日期，本机 qwen3.8-flash 答题并裁判）：**带日期时三种顺序都是 63 / 72**，顺序不起作用；**不带日期**时相关度顺序 50、新的在前 46、旧的在前 63——模型没有日期就按“越靠后越新”猜，所以“新的在前”恰恰最差。两种重排的代价也量了：按分数带宽 10% 重排，全部证据进前 5 从 0.719 掉到 0.698；按主题分组重排不改包的内容，但前 5 条被打乱。结论是日期起作用，顺序不起作用。平台里的两个消费方都已带日期：idp-host 的每条上下文前有 `observed <时间>`（`cmiclient.ContextOf`），EER Intelligence 把 `observedAt` 原样交给 Decision Provider。新的消费方照此办理；实在不能给日期的，自己把条目按 `observedAt` 从旧到新排（63 / 72，和带日期一样） |

局限：答题实验只有一个阅读模型、一类题、72 题，裁判是同一个模型（同一句答案偶尔被判成两样，量级 1–2 题）；相关度顺序下带不带日期差 13 题，远大于这个噪声；带日期时三种顺序之间的 0 题差异则只能说“看不出差别”。LongMemEval 的会话是模型生成的英文闲聊，不是我们的真实语料。

## 21. 线程记忆的归属与连接器请求头（2026-11-12）

起因：安全评审（`docs/security/REVIEW-2026-11-10.md`）留下的 O1、O3。决策人要求先修这两项，再做检索上的实验。

O1 的实际暴露面：现在写线程作用域的只有 EER（每个决策一条 `thread:<executionId>` 的情景记忆，`acl: {}`），同租户任何 reader 只要知道执行 id 就能读；执行 id 和线程 id 会出现在运行记录、上下文包和日志里。HOST 只写个人作用域、只在解析时带线程作用域，Workspace 用默认作用域，平台的端到端路径不读线程记忆。

| # | 决策 | 理由 |
|---|---|---|
| D44 | **线程记忆默认归写入者**：写入线程作用域且没给 ACL（`subjects`、`roles` 都空）的记录，CMI 把 ACL 填成 `{subjects: [写入者], roles: [cmi-admin]}`，写在存储层唯一的写入路径 `insertMemory` 里（记录、事实、整合、共享都经过它），返回的记录带着这个 ACL。写入者要让别人读，就显式给 ACL（比如 HOST 以自己的身份替用户写，就写 `subjects: [用户]`）。读路径不改：ACL 本来就在每条读 SQL 里执行（列表、按 id、解析、实体视图、双时态），所以不是本人的 reader 指名线程时什么都拿不到，按 id 读得到 404，不会泄露"有没有"。迁移 `0012_thread_acl.sql` 给之前写的、没有 ACL 的线程记录补上同样的 ACL：该线程的写入者（不含整合与评估这两个内部写入者）加 `cmi-admin`；逐租户绑定执行（行级安全对表所有者也强制），有改动的租户写一条审计 `migration.0012_thread_acl`。租户、Agent、组织单元作用域不变：它们本来就是共享的，要保密的内容自己带 ACL | 评审建议的方向是"把线程所有者写进 ACL，用现有机制执行"。放在 CMI 这一侧兜底，而不是让每个写入方记得设 ACL：EER 不用改（它读写用同一个工作负载身份，默认 ACL 正好是它自己），以后的写入方忘了 ACL 也不会漏。没有选"线程所有者表"：谁拥有线程只有 HOST / EER 知道，CMI 只看得到写入者；也没有选"线程写入必须带 ACL（否则 422）"：那会让 EER 现有的写入失败 |
| D45 | **连接器请求头不能带凭据**：除了 `Authorization`，名字里有 auth、token、secret、key、cookie、session、pass、credential、signature、csrf、xsrf、jwt、bearer 的头，以及值以 Bearer / Basic / Digest / Token / Negotiate 开头的头，登记时一律 422（`cmi.CredentialHeader`）；之前存进库的这类头由迁移 0012 把值改成 `<redacted>`（保留名字，管理员看得到该删什么），抓取时遇到这类头直接拒绝（同步返回 502，任务记错误），不发出去 | 请求头明文存库、并原样返回给管理员；配置说明里"不要放凭据"没有强制。按名字的规则会误伤个别普通头（比如名字里带 key 的），对抓文档的 GET 请求影响很小，凭据本来就该放在文档库前面的反向代理里 |

测试：`tests/integration/thread_test.go`（不是本人的 reader 用 `scope`、`scopes`、列表、按 id 都读不到；写入者和管理员能读；显式 ACL 只共享给被指名的人；租户作用域不变；迁移在旧行上的效果、重跑不再改动、审计、被改写的连接器不再同步；各类凭据头被拒、普通头可用）、`internal/cmi/credentials_test.go`。原有的两处集成测试由 bob（写入服务）替 carol 写线程记忆，按 D44 改为显式 `acl.subjects: [carol]`。

局限：默认 ACL 用的是写入时令牌的 subject；同一个线程由多个身份写入时，各自的记录各归各的，互相读要显式 ACL。迁移把一个线程的所有写入者都放进该线程每条旧记录的 ACL，比按条归属宽一些，但不会比修之前更宽。

## 22. 多证据召回：按会话限额（2026-11-12）

起因：LongMemEval 上"全部证据都在前 k 条"偏低（跨会话 all@10 0.785、时间推理 0.740，而"至少一条"在 0.95 左右），猜测是同一会话的几轮占满了前 10 条，把别的证据会话挤了出去。实验（`IDP_EVAL_SESSION_CAPS`，不改产品代码）：同一个解析器取一个更大的候选池（100 条，受 32000 token 预算），每个会话最多留 c 条，取剩下的前 10 条。全部 470 题，混合检索（Qwen3-Embedding-0.6B，memoryFloor 0.20，blend 0.05）：

| 每会话上限 | any@5 | all@5 | all@10 | 答案轮@10 | 跨会话 all@10 | 时间推理 all@10 | 时间推理 答案轮@10 | 知识更新 答案轮@10 |
|---|---|---|---|---|---|---|---|---|
| 不限（现状） | 0.930 | 0.719 | 0.857 | 0.906 | 0.785 | 0.740 | 0.835 | 1.000 |
| 1 | 0.953 | 0.838 | 0.908 | 0.734 | 0.835 | 0.858 | 0.638 | 0.750 |
| 2 | 0.936 | 0.781 | 0.883 | 0.849 | 0.818 | 0.803 | 0.772 | 0.944 |
| 3 | 0.932 | 0.760 | 0.870 | 0.891 | 0.810 | 0.764 | 0.803 | 1.000 |

结论：猜测成立，同一会话挤占位置是真的（上限 1 时 all@5 +0.12、all@10 +0.05）；但一个会话里得分最高的那一轮常常不是含答案的那一轮，限额把答案轮挤掉了（上限 1 时答案轮@10 −0.17，时间推理 −0.20）。阅读模型读的是轮次不是会话，答案轮在不在包里才决定能不能答对，所以上限 1、2 得不偿失，上限 3 基本持平（all@10 +0.013，答案轮@10 −0.015）。**不做按会话限额。**全文检索上结论相同（上限 1：all@10 0.726 → 0.785，答案轮@10 0.811 → 0.702）。

### 默认预算下（同日）

上面的 LongMemEval 数字量的都是 10 条的包（预算 32000 token，实际约 2750 token：记忆条目的摘录截到 1200 字节，一条最多约 300 token）。CMI 的默认预算是 2000 token / 20 条（D5），HOST 的发布件不设 `context.maxTokens` 时用的就是它（本节初稿说默认包"只装得下三到六轮"，是忘了摘录截断，错了：实测平均 8.4 条）。`IDP_EVAL_BUDGETS=20:2000` 在同一份存储上再按默认预算解析一次；同时新量"答案可见"：含答案的那一轮在包里，且含答案的那句话的开头在摘录里（阅读模型看到的是摘录）。全部 470 题，混合检索：

| 预算 | 平均条数 / token | 任一证据 | 全部证据 | 答案轮在包里 | 答案可见 | 跨会话 全部证据 | 时间推理 答案轮 | 知识更新 最新证据 |
|---|---|---|---|---|---|---|---|---|
| 默认 2000 token | 8.4 / 1975 | 0.940 | 0.772 | 0.853 | 0.851 | 0.620 | 0.772 | 0.972 |
| 10 条（评测一直用的） | 10 / 2751 | 0.964 | 0.857 | 0.906 | 0.904 | 0.785 | 0.835 | 1.000 |
| 3000 token | 11.9 / 2970 | 0.964 | 0.860 | 0.906 | 0.904 | 0.777 | 0.835 | 1.000 |
| 4000 token | 15.7 / 3967 | 0.975 | 0.889 | 0.928 | 0.925 | 0.826 | 0.874 | 1.000 |

（证据与答案轮量的是整个包：10 条的包是前 10 条，其余是包里的全部条目。）全文检索同样随预算上升（答案轮 2000 / 3000 / 4000 token：0.772 / 0.808 / 0.834，10 条 0.811）。

结论：默认预算比评测量的包少约 1.6 条，含答案那一轮的召回少 5 个点，"全部证据"少 8.5 个点，跨会话题最多（全部证据 −16.5 个点）。3000 token 和 10 条基本相同（多出的约 2 条几乎没用）；4000 token 再涨一截（答案轮 +2.2 个点、全部证据 +2.9 个点，跨会话全部证据 +4.9 个点），回报在递减：2000 → 3000 每千 token 换答案轮 5.3 个点，3000 → 4000 换 2.2 个点。摘录截断几乎不丢答案（答案可见只比答案轮低 0.002～0.003；这是上限：量的是含答案那句话的开头在不在摘录里，答案本身可能还在更后面）。| # | 决策 | 理由 |
|---|---|---|
| D46 | **默认预算改为 3000 token**（条数上限仍是 20）：请求不带 `budget` 时用它（`context.DefaultMaxTokens`）。HOST 的发布件不设 `context.maxTokens` 时得到的就是它；EER 自己带预算（`EER_CMI_MAX_TOKENS`，缺省 2000），不受影响。需要更高召回的消费方自己设更大的预算（4000 token：答案轮 0.928） | 2000 token 的默认包比所有 LongMemEval 结论（含 D43 的阅读实验）所依据的 10 条包少约 1.6 条，含答案那一轮的召回低 5.3 个点；3000 token 恢复到那个水平，每个包多约 1000 token。没有直接选 4000：检索更好，但阅读模型面对约 16 条时答得是否更好没有量过 |

未做、可以试的：命中一轮时带上同一会话的相邻轮次（用 token 换召回，和预算一起看）。

## 23. 真实语料评测（2026-11-13）

起因：第 17 节写明"上线前必须在真实文档和真实问题上重测"。

- **语料：** 一份真实的内部设计文档，以中文为主，夹大量英文术语与代码标识符。文档按章节切分，同一文档的旧版本不收（同一 URI 下旧版本本来就被取代，D4）。
- **问题：** 文档所有者写的 40 个中文问题，其中 37 个文档里有答案、3 个没有；另把这 40 个问题译成英文，单独统计。
- **标注：** 答案章节经核对确定，平均每题约 3 个章节。
- **不进仓库：** 语料、问题和标注都不进仓库，测试 `TestRetrievalOnAnExternalSet` 通过 `IDP_EVAL_DATASET` 读取仓库外的数据文件。
- **指标：** 前 k 条里出现任一标注章节就算命中（hit@k）。评测自带的 R@k 是"标注章节被找回的比例"，每题有多个标注章节时偏严。

| 配置 | 中文 hit@1 / @5 / @10 | 英文 hit@1 / @5 / @10 | 无答案返回空 |
|---|---|---|---|
| 全文 | 0.46 / 0.81 / 0.81 | 0.32 / 0.62 / 0.73 | 0 / 6 |
| Qwen3 混合，下限 0.35～0.50（结果完全相同） | 0.51 / 0.78 / 0.92 | 0.35 / 0.59 / 0.78 | 0 / 6 |
| 下限 0.60 | 0.46 / 0.76 / 0.92 | 0.30 / 0.54 / 0.73 | 0 / 6 |
| 下限 0.70 | 0.38 / 0.49 / 0.54 | 0.27 / 0.41 / 0.46 | 0 / 6 |
| 下限 0.40 + 重排（bge-reranker-v2-m3，前 10 个候选，重排下限 0.01） | 0.51 / 0.78 / 0.89 | 0.41 / 0.65 / 0.76 | 2 / 6 |

结论：

1. **全文检索在前 5 条上与混合检索持平（0.81 对 0.78）。** 真实问题里带着大量文档原词和代码标识符，全文就能命中。向量检索的价值在前 10 条（0.92 对 0.81）。默认预算 3000 token（D46）大约装 8 条知识摘录，正好落在 5 和 10 之间。
2. **相似度下限在这份语料上几乎不起作用。** 现行的知识下限 0.40 什么也筛不掉；提到 0.70 时召回已经崩了，无答案的问题仍然不返回空。无答案的问题问的都是同一个系统的同主题内容，只是文档里恰好没有答案，余弦相似度分不出"相关但没答案"。
   - 下限保持现值：它挡的是离题问题，这在模拟集上有效。
   - "这里没有答案"交给读包的模型来判断：HOST 交给模型的条目都标为 informational，并带来源和观察时间。
3. **重排（下限 0.01）没有改变 D39 的结论，仍是以召回换拒答、默认关闭。**
   - 英文问法 hit@1 和 hit@5 各提高约 6 个点；中文前 10 条从 0.92 降到 0.89。
   - 6 个无答案问法里挡住了 2 个，都是语料完全没涉及其主题的那一题。
   - 另把 1 个有答案的中文问题判成了空。
   - 重排下限 0.05 的那一组没有跑完：CPU 上的重排太慢，测试在 40 分钟时超时，不计入。

局限：

- 只有一份语料、一位所有者、40 道题。
- 无答案问题只有 3 个，"返回空"的数字只能说明方向。
- 标注以每题 3 个左右章节为准，语料里可能还有别的章节也能回答，所以命中率会被低估。
- 英文问法是中文问题的翻译，不是独立写的；而且有不少标注章节本身就是英文，所以英文问法并不全是跨语言检索。

## 24. 知识库（2026-11-24；2026-11-25 评审后按决策人的裁定修订）

企业知识库计划在平台仓库 `docs/adr/knowledge-base-plan.md`。这一刀做库本身、三级可见、成员写入、生效窗口、结构分块和原件，契约 `idp.telos.cmi` 1.5.0 只做新增（新定义 KnowledgeBase；KnowledgeDocument 增 `knowledgeBase`、`validFrom`、`docType`、`owner`、`reviewDue`、`original`；ContextItem 增 `locator`）。D47–D50 是这一刀的基本决定，D51–D58 是评审后补写的语义规则：每一条都有对应的回归测试（见本节末的删除表）。

| # | 决策 | 理由 |
|---|---|---|
| D47 | **知识库是新对象**：`knowledge_bases`，作用域为企业、单元或个人。已有文档在迁移时归入以其来源为名的企业库，可见性不变（迁移的做法见下）。管理员不指明库的文档归入来源同名的企业库，库在第一次用到时建立。个人库必须用 `user` 来源；**一个成员只有一个个人库，按所有者（作用域）查找而不按名字**（`(租户, 作用域 id)` 在个人库上唯一）；`personal-` 前缀留给服务为成员建的库，管理员登记不得用。别人读不到的个人库，读取返回 404。归档后不再检索、不再接收文档，归档库里的文档在包的复验里是陈旧的；管理员可以重开任何库，成员只能重开自己的个人库（`POST /api/v1/knowledge/bases/{name}/unarchive`）。审计一律用库的 id 作资源 id（创建、归档、重开、读取），名字放在 detail 里 | 权限按库生效，而不是逐份文档配 ACL。按名字找个人库会让管理员建一个同名的库把成员锁在外面 |
| D48 | **成员写入**：`POST /v1/knowledge/documents` 允许读者与写入者往自己能写的库里写文档：个人库只有所有者能写（管理员也不能往成员的个人库里写，能撤回里面的文档）；企业库和单元库要求是该库的管理者（主体或角色）——是单元成员却不是管理者得 403，读不到的库是 404。不指明库时，第一次上传建立本人的个人库。文档的敏感级别不得高于写入者的上限和库的上限；没写敏感级别时用库的 `defaultSensitivity`。**每条成员路由都像管理员路由一样先问 PDP（D23）**：`knowledge.read`（列表、读取、下载）、`knowledge.write`（上传、存原件）、`knowledge.revoke`；PDP 的 `maxSensitivity` 与 `excludeSources` 对成员的列表、读取、撤回和原件下载同样生效。成员的列表与撤回用和读取相同的可见性（ACL、敏感级别、擦除、范围）：管理者撤不了自己读不到的文档；`If-Match` 不符在两条撤回路径上都是 409 | 成员不是管理员。写权限不等于读权限，两个都要过 |
| D49 | **知识文档按结构分块**：标题另起一块；表格放得下 1500 字节（`cmi.KnowledgeChunkBytes`）就整块，放不下就按行分组，每组重复表头和分隔行且不超过 1500 字节，单行超过 1500 字节的像文本一样切；围栏代码块（\`\`\` 与 ~~~）对结构解析不透明（里面的 `#`、`\|`、页标记都不算），整块一个 chunk，超过 1500 字节才按行切、每块重复围栏。没有标题、表格、围栏和页码标记的文本与原来的 `Chunk` 相同。每块带标题路径、页码、表名，经 `ContextItem.locator` 给出；**标题路径与表名永远在索引和向量的文本里**（标题不在正文里，章节要能按它的标题词找到——HEAD 能，第一版草稿不能），摘录是正文，前面加一行标题路径（`A › B`），模型看得见它在哪一节；**写入时的注入扫描（D12）扫的是模型会看到、索引会匹配的全部：标题路径、表名和正文**，标题里的"忽略之前的指令"和正文里的一样被打标、不进包；没有标题的块与原来逐字相同。建索引和取向量的文本最多 8192 字节（`MaxIndexTextBytes`，切的是上下文不是正文），交给嵌入服务时 64 条一批；一块嵌入服务不收（单独重试时仍失败）就记下原因跳过（`knowledge_chunks.embed_error`，D58）。记忆不走这条路径 | 一条条款要能按它所在的章节被找到，一行表格要能被找到并且摘录里就是这一行 |
| D50 | **原件单独存放**：文档版本可以另存一份原件，库里只留对象键、摘要、大小和媒体类型，键不出契约；读出时按摘要核对，不符是 500 并写日志。擦除域 `originals` 删掉对象之后，这次擦除才标 verified。没配对象存储时文本入库照旧，存原件得到 503；对象存储在后台连，CMI 不等它（D54）。键、竞态、下载头、配额见 D54、D55 | 原件不是检索用的正文 |
| D51 | **版本的身份是（租户、知识库、uri）**。版本号、取代、"现行"判断、as-of 都只在一个库之内比较；`(knowledge_base_id, uri, version)` 唯一。成员的上传和管理员指名了库的上传，uri 必须是 `kb://<库名>/<路径>`，否则 422，信息里说明应有的写法（拒绝，不改写）；uri 指着另一个库的 `kb://` 也是 422。成员没指名库时（本人个人库的名字带哈希，上传前无法知道）可以写 `kb://me/<路径>`，存入的是规范的 `kb://<个人库名>/<路径>`，返回的文档里就是规范写法；`me` 因此是保留名，不能登记同名的库或来源。管理员不指名库的上传（旧路径）与连接器一样保留各自的 uri，落在来源同名的企业库里：不同来源的库里相同的 uri 是两份文档，谁也取代不了谁。连接器的未变化检查（`ActiveDocumentDigest`）也按来源的库 | 审查里：成员往个人库传 `handbook://travel-policy`，企业库的 v1 被取代、全员读不到。三个方向（个人→企业、企业→个人、单元→企业）都有测试 |
| D52 | **"现行"只有一个定义**，检索、实体列表、omitted 统计、配额、管理员列表和 `VerifyPackage` 用同一条：一个文档是它的（库、uri）里最高的、没有被撤回、没有过期、没有被擦除、有效窗口含"现在"的那一版，并且所在的库是活动的。`validFrom` 缺省（NULL）表示"从一开始就有效"，不是 created_at；as-of 对没有 `validFrom` 的文档与这次改动之前完全一样（有效时间不受它的登记时间限制；已知时间不得早于它的登记时间）。**一个版本开始之后，它的结束不会让更早的版本复活**：在没有被撤回、没有被擦除、已经开始（`validFrom` ≤ 现在）的版本里取最高的；它的窗口含现在才是现行，不含就没有现行版本，不回退到更早的。检索、as-of、配额、列表、包复验同一条规则。理由：一个没有人重新确认过的旧版本，不该在更新的一版到期时悄悄回来（2025 年的费率表不能在 2027-01-01 重新生效）；到期是一个事实，撤回才是"这一版作废、回到上一版"的明确动作。**取代是推导的，不再写库**：新版本一开始，旧版本就不是现行，管理员列表里显示 superseded，旧行的存储状态不变（旧发布留下的 `superseded` 行按同一规则解读）。后果：撤回最新一版（回滚），上一版重新成为现行，检索、"现在"的 as-of 视图、包的复验同时给出同一个答案（之前 as-of 返回旧版而检索什么都不返回）；定为未来生效的版本不取代现行版本；一个有 `validUntil` 的新版本到期后，上一版**不**回来。**升级说明**：旧发布把被取代的版本存成 `superseded`，升级后这些行按这一条重新解读——被取代的版本在更新的一版被撤回之后会重新现行，这是和升级之前不同的地方（之前什么都不返回）；要让某份文档整个下线，撤回它的所有版本。包里引用的版本不再现行（有更新的版本已生效、撤回、过期、库归档、被擦除）就在复验里是陈旧的 | 审查 S6、S7：取代曾经是写入时的一次 UPDATE，与生效窗口、撤回不一致 |
| D53 | **个人库的范围与审计**：解析的缺省范围对每个人（管理员也一样）是：企业库、调用者所属单元的库、调用者自己的个人库。管理员读别人的个人库只能指名：解析时在 `scope`/`scopes` 里指名该成员的主体作用域（与个人记忆同一规则 D42：写入服务和管理员可以指名，读者不行），或按文档 id 读、按 `knowledgeBase` 列出、按名字读库。每次这样的读取记审计 `knowledge.read_other`（资源 id 是库 id；detail 有 via＝resolve／document／original／list、subject、client、actor；resolve 里按有 chunk 返回的库各记一条），读库的元数据记 `knowledge_base.read`；管理员的库列表也不含别人的个人库。别人读不到的文档是 404 | 计划选择点 5 (a)：管理员能读，每次读审计 |
| D54 | **原件的键、竞态与交付**：每次上传有自己的对象键 `kb/<租户>/<数据库里的文档 id>/<版本>/<上传 id>`（不用调用者写的 id 拼写）。流程：PDP → 权限（读得到且能写该库，**在读请求体之前**）→ 请求体边读边算 SHA-256、按媒体类型限长（25 MiB，pdf／pptx／图片 50 MiB，不另存一份拷贝）→ 写对象 → 在事务里锁库行、再锁文档行，**条件更新**（行仍指着本次上传开始时的对象、文档没有被擦除，否则 409／404 并删掉新对象）→ 提交之后再删旧对象（删失败只记日志）。写对象失败时库里不变，上一份原件仍在。擦除的屏障（`erasure_id`）在事务里重查，所以擦除开始之后不会再记入新键。上传不增加文档的 `revision`（它管撤回的 If-Match 与投影事件）。每个调用者同时最多 2 个上传（429）。下载一律是附件：`Content-Disposition: attachment; filename="original-<id 前八位>.<扩展名>"`、`X-Content-Type-Options: nosniff`、`Content-Security-Policy: sandbox`、`Cache-Control: private, no-store`；存的媒体类型每次下载前再按白名单核对，不在白名单里的按 `application/octet-stream` 给。对象存储配置 `IDP_CMI_ORIGINALS_CA_FILE`、`_TIMEOUT`（默认 2 分钟），桶检查每次 20 秒（`blob.CheckTimeout`），CMI 启动不等它：后台每 30 秒重试，之前原件一律 503；检查得到 403 不再当成桶不存在去建桶，只有 404 才建。**擦除按前缀清扫**：`originals` 域列出并删除 `kb/<租户>/<文档 id>/` 下的全部对象（`ListKeys`），不只删行里指着的那一个，删完再列一遍，还有剩的就让该域失败；所以替换时没删掉的旧对象、写了对象却没来得及提交（崩溃）留下的对象，都随文档的擦除一起消失。（崩溃留下的对象在它所属的文档被擦除时才被找到；文档一直没有被擦除时它只是占着存储，没有后台清扫，这是成本问题。）**对象存储不可达不算一次失败的尝试**：关着的门（`ErrUnavailable`）、5xx、429、超时、网络错误都归还这次尝试，擦除保持 pending，存储回来后完成，等多久都行；存储拒绝（403 等）才算失败。**记录时在事务里再核对**：锁了库行和文档行之后，库是否归档、调用者是否仍是管理者再判一次（归档了是 409，不再是管理者是 404，两种情况都删掉刚写的对象）。**对象键用数据库里的租户 id**（令牌里写成大写也一样），擦除的前缀因此总能找到它们。**全局的上传字节上限**：所有调用者同时在途的上传预留 `min(Content-Length, 类型上限)`（没有长度的按类型上限，长度超过类型上限在读之前就 422），合计不超过 `knowledge.maxInFlightUploadBytes`（缺省 256 MiB，至少要有最大的原件 50 MiB，否则启动失败），超出 429。**下载经临时文件**：边读边算摘要写进 `IDP_CMI_TEMP_DIR`（缺省系统临时目录，配了原件存储就在启动时检查它可写）里的文件，创建后立刻 unlink（保留文件描述符，崩溃不留东西），核对通过才发出，不整份读进内存；同时的下载不超过 `knowledge.maxConcurrentDownloads`（缺省 8），其中一个调用者至多 `knowledge.maxDownloadsPerCaller`（缺省 2），超出 429。上传和下载这两条路由有自己的截止时间 `knowledge.transferTimeout`（缺省 10 分钟），不受服务器 60 秒读写超时的限制（否则 50 MiB 的传输被卡在约 7 Mbit/s）。发布物的根文件系统是只读的，所以 `deploy/kubernetes/cmi.yaml` 给了一个 `emptyDir`（`sizeLimit: 512Mi` = 8 × 50 MiB 加余量）挂在 `/var/cache/idp-cmi`，`IDP_CMI_TEMP_DIR` 指向它；compose 需要给 cmi 服务一个同样位置的 tmpfs（`tmpfs: ["/var/cache/idp-cmi:size=512m"]`，见 `deploy/compose.yaml` 头部的说明）。**监控**：等对象存储的擦除有指标 `idp_cmi_erasures_waiting_for_store` 与 `idp_cmi_erasures_waiting_oldest_seconds`，并且每分钟至多一条警告日志写明等了多久 | 审查 S5、S11、S13 |
| D55 | **配额**：文档数是有现行版本的文档个数（一份文档不论几个版本只算一次），字节是索引文本加原件，只计没有被更新版本取代、没有撤回／过期／擦除的版本；定为未来生效的版本另计（防止用未来日期绕过）。新版本立即生效时扣掉它取代的那一版的字节；替换原件时旧原件只算一次；检查和写入在同一个事务里，锁库行（`FOR UPDATE`），并发突发不会超出。个人库自己的配额为 0 时用成员缺省，来自配置 `knowledge.personalQuotaDocuments`／`personalQuotaBytes`（缺省 **500 份文档、200 MiB**；写 0 表示不限）；企业库和单元库为 0 仍表示不限。超出 429 `QUOTA_EXCEEDED`。**已知的代价**：配额检查在持有库行锁时扫描该库里现行的文档，开销与库的大小成正比，同一个库的上传因此串行；现在不优化，库大到上传变慢时改成计数器 | 审查 S8：满库拒绝已有文档的新版本、个人库无限、原件不计、先查后写 |
| D56 | **文档标题进索引文本是一个设置，缺省关**（`knowledge.indexContext`；标题路径不在设置里，见 D49，永远在）。关着时没有标题的块的索引文本就是正文，评测语料没有 Markdown 标题，所以 `make test-eval` 不受 H1 的影响。**量过，没采纳——在这个知识库自己的标注题上重新决定（计划 2.6 节）**：在评测集上打开标题前缀（语料没有标题，所以量到的就是文档标题的作用）：大集词面 R@1 0.5641→0.5513（评审那次的草稿 0.5385），MRR 0.6239→0.6175（草稿 0.6122），R@5 0.6346→0.6346（草稿 0.6282），R@10 0.6603→0.6667，nDCG@10 0.6226→0.6205；留出集词面 R@1 0.3814→0.3918，R@5 0.5361→0.5326，MRR 0.4484→0.4552，nDCG@10 0.4797→0.4849；标注集混合 R@5 0.9444→1.0（旧并列顺序下；换成显式顺序之后这一项没有变化）。有升有降，差别在几道题之内，样本不够下结论。**并列分数的顺序**：词面检索里并列很多（中文按 bigram）。之前的顺序是查询计划碰巧留下的（HEAD 按 uri 索引扫描），换一个计划结果就变；现在写明：`ORDER BY r DESC, d.uri, d.version, c.seq`（外层，不再有内层排序，内层排序曾使大库上的词面解析慢一倍）。这把 `docs/eval` 重新定了一次基线（大集词面 R@1 0.5641→0.5513，MRR 0.6239→0.6147；留出集 R@1 0.3814→0.378，MRR 0.4484→0.4481），**并列的顺序本身就让 R@1 与 MRR 移动约一个百分点，上面打开上下文的差别（新基线上：大集 R@1 0.5513→0.5385，MRR 0.6147→0.6083；留出集 R@1 0.378→0.3883，MRR 0.4481→0.4547）都在这个噪声之内**。`IDP_EVAL_INDEX_CONTEXT=1 make test-eval UPDATE=1` 重复这次测量（会改 docs/eval，别提交） | 审查 B2：门禁不得因为一个没量过的默认值失败 |
| D57 | **擦除主体也擦掉他的个人库**：`subject` 选择器除了记忆，还选中他个人库里的所有文档，其文本块、原件（`originals` 域）、实体、包里的摘录按 D20 净化；其他文档里的 `owner` 若是该主体则清空。个人库里的文档不再留着成员自己起的路径：uri 改成 `kb://<库名>/erased-<文档 id>`、`created_by` 改成 `[erased]`，已存的包里引用这些文档的 `ref` 同步改写；主体被擦除的个人库改名为 `personal-erased-<库 id 前 12 位>`、`scope_id` 改成 `erased-<库 id>`、管理者清空并归档（成员若还在，下一次上传会建一个新库）。**主体擦除**（设计决定，评审时定的，不是别的擦除的做法）还处理该成员自己请求的包：`query` 与 `purpose` 换成墓碑，`requestedBy` 与指着他的 `scope` 换成一个只由这次擦除派生的稳定标记（`erased-<擦除 id 前 8 位>`，不是主体的哈希，猜名字也还原不了），列 `requested_by` 同样，包标 `redacted`；他撤回的文档的 `revoked_by` 也清掉。别的擦除（按记录、文档、引用、实体）不动提问人和提问原文：D20 擦除的是被引用的内容，不是谁问过。栅栏设在请求的事务里；请求之后才传进个人库的文档在 `content` 域开始时一并归入同一次擦除。`content` 域结束前核对：个人库里没有不在擦除之下的文档、没有还留着文本的块，否则该域失败、擦除不标 verified | 审查 S3：擦除标了 verified，个人库的文档、原件还在 |
| D58 | **一个无法索引的块不拖住回填，但只有输入错误才是永久的**：嵌入服务整批失败时逐条重试。只有嵌入服务拒绝文本本身（400、413"太长"、422）才算永久；401、403、404、405 这类是凭据或地址不对，不是文本的问题，按暂时的处理，并且记一条错误日志、计入 `idp_cmi_embed_failures_total{kind="refused"}`（`permanent`、`transient` 也在这个指标里）：记 `embed_error` 与当时的模型（原因最多 300 字符，另写一条警告日志），在换了嵌入模型之前跳过，管理员也可以清除（`POST /api/v1/knowledge/embed-errors/clear`）；它仍在词面索引里。无应答、5xx、429、超时、格式不对都是暂时的：计一次尝试（`embed_attempts`），下一次尝试的时间按 10 秒翻倍、至多一小时（`embed_retry_at`），成功后清零，等待期间不挡别的块。一个块都没成功而有暂时性错误，是服务端的问题，整批留给下一轮、什么也不记；每一条都是永久错误则都记下。投影事件同样处理，事件照常标为已处理（暂时失败的块由回填接着做） | 一块永远失败的内容不能让后面的内容永远嵌入不了 |
| D59 | **查询可以有自己的嵌入端点（`embedding.query`，可选）**：配了之后，一切查询侧的嵌入（解析与搜索、记忆召回，凡是嵌入提问者文字的地方，实际只有解析器一处）走它；一切文档侧的嵌入（投影 D26、回填、记忆与知识的写入）仍走主端点。模型、维度、查询前缀与文档前缀（D37）、CA 共用，因为同一个模型必须服务两边、向量才可比较：`embedding.query.model` 或 `dimensions` 若写了且与主端点不同，**启动即拒绝**（不写即可）；密钥不继承（`apiKey`／`apiKeyFile` 自己的，主端点的密钥不发往另一个地址）；`timeout` 缺省 10 秒（一次查询是有人在等的）。不配则与以前完全一样。**护栏**：启动时在后台（不挡启动），之后每 5 分钟（未通过时每 30 秒）把一条固定探测文字（不加前缀）发给查询端点，与**主端点对同一条文字的向量**比较，要求维度相等且余弦相似度至少 0.999。这个容差放过同一权重在 CPU 与 GPU（或 float32 与 bfloat16）上的数值差（量级约 0.9999），挡住量化过的副本（量级约 0.99）和别的模型（远低于此）；它是按这个量级定的，没有在 TEI 的两种硬件上实测，换了模型或硬件要重新核对（常量 `embedding.MinProbeCosine`）。**参照向量落库**：主端点正是被批量摄入占住的那个（一个请求可能排几分钟到几小时），所以探测不能每次都问它。主端点第一次回答探测文字时，它的向量存进 `embedding_probe_references`（迁移 0017：键为已存向量带的模型标识 `embedding_model`——含文档前缀的哈希，D37——加探测文字的 SHA-256；不属于任何租户，只有 `app.role = 'embedder'` 能读写，行级安全强制）；之后的探测只问查询端点，与存下的参照比较，**只有没有参照时才问主端点**（并存下它的回答）。换了模型或文档前缀就是新的标识，没有参照，不借用别的标识的。存下的参照与查询端点不符时不直接下结论（可能是过期：比如配置里改了维度），先问主端点一次、用新的回答比较并替换参照；主端点没有回答时，按存下的参照判不匹配。所以摄入期间重启：参照在库里，查询端点立即可用；没有参照而主端点忙：状态保持 pending（不是永远 unreachable），30 秒后重试。已知的限度：主端点背后换了权重而模型名不变，参照不会察觉（和 D11 里"模型名即身份"同一前提）。**通过之前、或探测失败之后，查询不用查询端点，也不改用主端点：只答词面检索**（和单端点失败时今天的行为一样）。选词面而不是主端点，是因为主端点正是被摄入占住的那个，查询排在后面等几十秒到几分钟，比一个词面答案更糟。探测没有结论（查询端点没答）时：已经是 ok 的保持 ok，其余保持原状（从未判过的是 unreachable，不匹配的仍是不匹配）。**失败**：查询端点的任何失败（连不上、超时、4xx、5xx，包括 413）都不让解析失败，只记一条警告、该次答词面检索，也不记任何"永久"：D58 讲的是文档。**整个请求失败要看得见**：嵌入端点连不上、超时、5xx 时（没有逐块结果），不消耗任何块的 `embed_attempts`/`embed_retry_at`（这是端点的问题，不是块的问题，D58 不变），但每个失败的请求计一次 `idp_cmi_embed_failures_total{kind="unavailable",role}`（该指标新增 `role` 标签：已有的 permanent、transient、refused 是 `role="documents"`；查询角色另计 `unavailable` 与 `refused`；400/413/422 是对某段文字的拒绝，不算端点故障），并有 `idp_cmi_embedding_last_success_timestamp_seconds{role}`（该角色最近一次成功的嵌入请求的 Unix 时间，启动后尚无成功为 0），告警可以写"N 分钟没有成功的嵌入"。批量回填在端点挂掉时会先试整批、再逐条试，所以一次回填可能计多次 `unavailable`（每个失败的 HTTP 请求一次）。其余指标：`idp_cmi_embedding_requests_total{role="query"\|"documents",outcome="ok"\|"error"\|"unavailable"}`（unavailable：一次查询因探测未通过而没有用查询端点；未配 `embedding.query` 时同样按角色计数）、`idp_cmi_query_endpoint_state{state="pending"\|"ok"\|"mismatch"\|"unreachable"}`（当前状态为 1）、`idp_cmi_query_endpoint_probes_total{result="ok"\|"mismatch"\|"error"}`；状态变为 ok 记一条 info，不匹配每次探测记一条 error（含原因：维度或余弦）。接线（谁用哪个端点）在 `internal/embedwire`，`main.go` 与集成测试用的是同一段代码。OpenAPI 与契约不变（没有外部可见的变化）。 | 在 4 个 CPU 上实测：摄入约 0.25 块/秒；单条查询嵌入本身 p95 约 220 毫秒；同一实例上做批量摄入时，查询排在整个摄入请求后面：每次 64 条输入时等 247 秒，每次 8 条输入时 p95 31 秒；另给查询一个 2 CPU 的实例，查询 p95 约 400 毫秒，摄入吞吐不变；批量摄入占着主端点时它的一个请求可以排到几分钟以上，所以探测只在没有参照时才问它。所以查询需要自己的端点；而两个端点配得不一样（别的模型、别的维度、量化过的副本）会悄悄把查询向量放进不属于它的空间，所以必须有探测，探测通过之前宁可不用 |

**迁移**（0013–0016）：迁移由模式所有者运行；所有者不是超级用户时 FORCE 行级安全对它同样生效，回填读不到任何行。所以每个读写了现有行的迁移在**同一事务里对相关表 `NO FORCE ROW LEVEL SECURITY`，回填，再 `FORCE`**（`sources`、`knowledge_documents`、`knowledge_chunks`；新表 `knowledge_bases` 在回填之后才启用 RLS）。没有选择"逐租户设置租户再分批"，因为回填要跨所有租户而且是一条语句。`valid_from` 对存量文档保持 NULL（D52）；旧的 `(tenant_id, uri, version)` 唯一约束换成 `(knowledge_base_id, uri, version)`。**升级的代价（0013、0015）**：0013 对 `sources`、`knowledge_documents` 取 ACCESS EXCLUSIVE 锁（改约束、加列），0015 对 `knowledge_chunks` 取同样的锁，并且 `UPDATE ... SET index_text = content` **改写每一个 chunk 行**（一个事务；表和 GIN 索引暂时涨一倍，事后要 VACUUM）。停机时间随语料大小增长：在本机（SSD，PostgreSQL 17）上 30 万个 chunk、620 MB（表加索引）约 7 秒，即每个 chunk 约 25 微秒、每 100 MB 约 1 秒；估算办法是 `SELECT count(*), pg_size_pretty(pg_total_relation_size('knowledge_chunks')) FROM knowledge_chunks`，再用上面的比例，并且在生产库的恢复副本上量一次再定窗口。升级期间服务读写同一批表会等锁，所以按停机处理。测试 `TestMigrationOnAPopulatedDatabase` 在模式 12 的库里放进数据再升级，分别用超级用户和非超级用户的所有者各跑一遍。

**配置**：`knowledge.indexContext`、`knowledge.personalQuotaDocuments`、`knowledge.personalQuotaBytes`、`knowledge.maxInFlightUploadBytes`、`knowledge.maxConcurrentDownloads`、`knowledge.maxDownloadsPerCaller`、`knowledge.transferTimeout`（配置文件）；`IDP_CMI_ORIGINALS_CA_FILE`、`IDP_CMI_ORIGINALS_TIMEOUT`、`IDP_CMI_TEMP_DIR`（环境变量）；`embedding.query.baseUrl`、`.apiKey`／`.apiKeyFile`、`.timeout`（D59，配置文件）。迁移 0017（`embedding_probe_references`）是新增的表，不改旧表。

**验证**：契约测试；`make test`、`make test-integration`、`make test-integration-pgvector`、`make test-eval`（见下表对应的测试）。

**删除表**：每个控制各删一次，下面的测试必须失败（`tests/integration/` 里的测试用 `make test-integration` 的方式运行，单元测试在各自的包里）。

| # | 删掉的控制（每行只删这一处） | 失败的测试 |
|---|---|---|
| R01 | version identity per base: the replacement/in-force checks compare uri across bases | `TestConnectorURIsNeverCrossBases` |
| R02 | kb://<base>/ uri required for uploads into a base | `TestPersonalBaseNamingAndUnarchive`、`TestVersionIdentityIsPerBase` |
| R03 | a uri naming another base is refused | `TestVersionIdentityIsPerBase` |
| R04 | index context setting off by default | `TestKnowledgeHeadingIndexSetting`、`TestRetrievalOnTheHeldOutSet`、`TestRetrievalOnTheLargeSet` |
| R05 | equal scores are ordered by uri, version and chunk (outer tie-break) | `TestRetrievalOnTheHeldOutSet`、`TestRetrievalOnTheLargeSet` |
| R06 | large tables are cut into row groups | `TestLargeTableIsSplitIntoRowGroupsThatRepeatTheHeader`、`TestHugeTableIsAccepted`、`TestLargeTableRowIsRetrievableAndExcerpted` |
| R07 | a row longer than the cap is cut like text | `TestRowLongerThanTheCapIsCutLikeText` |
| R08 | fenced code is opaque to the structure parser | `TestFencedCodeIsOpaque`、`TestLongFencedCodeIsCutWithTheFenceRepeated` |
| R09 | a long fence is cut with the fence repeated | `TestLongFencedCodeIsCutWithTheFenceRepeated` |
| R10 | the indexed and embedded text is bounded | `TestIndexTextIsBounded` |
| R13 | default scope: administrators see only enterprise, their units and themselves | `TestAdministratorReadsAPersonalBaseOnlyByNamingIt` |
| R14 | a resolve that reads another member's chunks is audited | `TestAdministratorReadsAPersonalBaseOnlyByNamingIt` |
| R15 | a read of another member's document or original by id is audited | `TestAdministratorReadsAPersonalBaseOnlyByNamingIt` |
| R16 | an administrator does not write into a member's personal base | `TestAdministratorReadsAPersonalBaseOnlyByNamingIt` |
| R17 | member routes ask the PDP | `TestMemberKnowledgeRoutesAskThePDP` |
| R18 | the PDP's obligations narrow member reads | `TestMemberKnowledgeRoutesAskThePDP` |
| R19 | excluded sources apply to member reads of documents and originals | `TestMemberKnowledgeRoutesAskThePDP` |
| R20 | erasure of a subject covers their personal base | `TestSubjectErasureCoversThePersonalBase` |
| R21 | documents that reach the base after the request are claimed | `TestSubjectErasureCoversThePersonalBase` |
| R22 | owner fields naming the subject are erased | `TestSubjectErasureCoversThePersonalBase` |
| R23 | first use of a base: INSERT ... ON CONFLICT DO NOTHING then SELECT | `TestConcurrentFirstUseNeverFails` |
| R24 | source base: ON CONFLICT DO NOTHING | `TestConcurrentFirstUseNeverFails` |
| R25 | the base row lock serializes version numbers and quota checks | `TestConcurrentFirstUseNeverFails`、`TestQuotaHoldsUnderConcurrency` |
| R27 | the row is updated only if it still points to what the upload started from | `TestOriginalReplacementRacesAndFailures` |
| R28 | erasure is re-checked inside the transaction that records the key | `TestOriginalAttachedWhileTheDocumentIsErased` |
| R30 | the replaced object is deleted only after the commit | `TestOriginalReplacementRacesAndFailures` |
| R31 | an object whose row was not updated is deleted | `TestOriginalAttachedWhileTheDocumentIsErased`、`TestOriginalReplacementRacesAndFailures` |
| R32 | package verification uses the one definition of current | `TestNewVersionInForceCondition`、`TestPackageStaleness` |
| R33 | a version not yet in force does not hide the one in force | `TestNewVersionInForceCondition` |
| R34 | an archived base makes its documents not current | `TestPackageStaleness` |
| R35 | a document written without validFrom has none (NULL = from the beginning) | `TestAsOfOfADocumentWithoutValidFrom`、`TestNewVersionInForceCondition` |
| R36 | as-of: a revoked newer version no longer hides the older one | `TestRevokedSuccessorLeavesOneAnswer` |
| R37 | a new version of a document the base holds does not count twice | `TestQuotaCounting` |
| R38 | a version in force frees the bytes of the one it replaces | `TestQuotaCounting` |
| R39 | originals count toward the bytes of a base | `TestQuotaCounting` |
| R40 | a personal base has the member default quota | `TestQuotaCounting` |
| R41 | an original on a replaced or revoked version is not counted, a replacement counts once | `TestQuotaCounting` |
| R42 | downloads are attachments | `TestOriginalDownloadHeaders` |
| R43 | downloads are sandboxed | `TestOriginalDownloadHeaders` |
| R44 | downloads are nosniff | `TestOriginalDownloadHeaders` |
| R45 | the stored media type is checked against the allow-list on download | `TestOriginalDownloadHeaders` |
| R46 | uploads of originals are limited to the allow-list of media types (checked in the route, in CheckOriginalWrite and in PutOriginal) | `TestOriginalDownloadHeaders` |
| R47 | the permission is checked before the body is read (in the route and in PutOriginal; removed together) | `TestOriginalPermissionBeforeBodyAndSizeLimit`、`TestOriginalRouteAnswersBeforeTheBodyArrives` |
| R48 | the body is bounded as it is read | `TestOriginalPermissionBeforeBodyAndSizeLimit` |
| R49 | concurrent uploads per caller are bounded | `TestOriginalUploadsPerCallerAreBounded` |
| R50 | the digest of a stored original is checked on download | `TestOriginalDigestMismatchIsAServerError` |
| R51 | list of member documents applies ACL and sensitivity | `TestMemberListAndRevokeFollowReadVisibility` |
| R52 | a manager cannot revoke what they cannot read | `TestMemberListAndRevokeFollowReadVisibility` |
| R53 | a stale If-Match is a conflict on both routes | `TestMemberListAndRevokeFollowReadVisibility` |
| R54 | a missing bucket (404) is created, anything else is an error | `TestEnsureBucketCreatesOnlyAMissingBucket` |
| R55 | originals answer unavailable until the store is reached | `TestGateAnswersUnavailableUntilTheStoreIsReached`、`TestOriginalStoreGate` |
| R56 | the migration sees the rows as a schema owner without superuser (sources) | `TestMigrationOnAPopulatedDatabase` |
| R57 | the migration sees the rows (documents) | `TestMigrationOnAPopulatedDatabase` |
| R58 | the migration sees the rows (chunks) | `TestMigrationOnAPopulatedDatabase` |
| R59 | FORCE row-level security is switched back on after the backfill | `TestMigrationOnAPopulatedDatabase` |
| R60 | the name prefix personal- is reserved | `TestKnowledgeBasesAreScoped` |
| R61 | a member has one personal base (unique by owner) | `TestKnowledgeBasesAreScoped` |
| R62 | only a manager (or the owner) writes a base | `TestKnowledgeBaseManagersAndCeilings` |
| R63 | the base's sensitivity ceiling holds | `TestKnowledgeBaseManagersAndCeilings` |
| R64 | the writer's sensitivity ceiling holds | `TestKnowledgeBaseManagersAndCeilings` |
| R65 | the base's default sensitivity is applied | `TestKnowledgeBaseManagersAndCeilings` |
| R66 | an owner may reopen their archived personal base | `TestPersonalBaseNamingAndUnarchive` |
| R67 | the personal base is found by its owner (the name is derived from a safe encoding) | `TestPersonalBaseNameIsSafeForAnySubject`、`TestPersonalBaseNamingAndUnarchive` |
| R68 | erasure of originals fails when the store refuses (the delete and the check after it) | `TestErasureFailsWhenAnObjectSurvivesItsDelete`、`TestOriginalErasureFailureIsNotVerified` |
| R69 | a chunk's heading, page and table are exposed as the item's locator | `TestKnowledgeHeadingIndexSetting` |
| R70 | the connector digest check is per base | `TestConnectorURIsNeverCrossBases` |
| R71 | naming a personal base in the administrator's list is audited | `TestAdministratorReadsAPersonalBaseOnlyByNamingIt` |
| R72 | the administrator's list leaves other members' personal bases out | `TestAdministratorReadsAPersonalBaseOnlyByNamingIt` |
| R73 | a version replaced stays in the list as superseded (derived status) | `TestMigrationOnAPopulatedDatabase`、`TestNewVersionInForceCondition` |
| R74 | the quota check of a new version | `TestKnowledgeBasesAreScoped`、`TestQuotaCounting`、`TestQuotaHoldsUnderConcurrency` |
| R75 | an expired newest version leaves no current version (current view) | `TestExpiredNewestVersionDoesNotResurrectAnOlderOne` |
| R76 | an expired newest version leaves no current version (as-of) | `TestExpiredNewestVersionDoesNotResurrectAnOlderOne` |
| R77 | a swept (expired) newest version still counts as the newest | `TestExpiredNewestVersionDoesNotResurrectAnOlderOne` |
| R78 | the originals erasure sweeps the document's whole prefix | `TestSubjectErasureLeavesNothingThatNamesTheMember` |
| R79 | the erasure lists the prefix again and fails if an object survived | `TestErasureFailsWhenAnObjectSurvivesItsDelete` |
| R81 | the closed gate is a transient condition | `TestTransientErrors`、`TestErasureWaitsForTheObjectStore` |
| R82 | a 5xx answer of the store is a transient condition | `TestTransientErrors`、`TestErasureWaitsForTheObjectStore` |
| R83 | a personal document keeps no member-chosen path after its erasure | `TestSubjectErasureLeavesNothingThatNamesTheMember` |
| R84 | the erased subject's personal base is renamed and closed | `TestSubjectErasureLeavesNothingThatNamesTheMember` |
| R85 | the stored packages' refs are rewritten | `TestSubjectErasureLeavesNothingThatNamesTheMember` |
| R87 | a transient failure waits out a backoff before the retry | `TestTransientEmbeddingFailureIsRetried` |
| R88 | a permanent embedding error is forgotten when the model changes | `TestPermanentEmbeddingErrorIsClearedByAModelChangeOrAnAdministrator` |
| R89 | an administrator can clear embedding errors | `TestPermanentEmbeddingErrorIsClearedByAModelChangeOrAnAdministrator` |
| R90 | a successful embedding clears the retry bookkeeping | `TestTransientEmbeddingFailureIsRetried` |
| R91 | a transient failure counts an attempt and sets the next time | `TestTransientEmbeddingFailureIsRetried` |
| R92 | base archived / manager removed is checked again when the original is recorded | `TestOriginalRechecksBaseAndManagerWhenRecording` |
| R95 | me is reserved as a source name | `TestReservedNames` |
| R96 | me is reserved as a base name | `TestReservedNames` |
| R97 | a prose line that starts with a pipe is not a table row | `TestChunkerQuirks` |
| R98 | a setext heading is a heading | `TestChunkerQuirks` |
| R99 | a table marker names the next table of its own section only | `TestChunkerQuirks` |
| R100 | the heading path of a chunk is always in the indexed and embedded text | `TestChunkKnowledgeHeadingsTablesAndPages`、`TestKnowledgeHeadingIndexSetting` |
| R101 | the excerpt of a chunk starts with its heading path | `TestKnowledgeHeadingIndexSetting` |
| R102 | a download's temporary file is unlinked at once | `TestDownloadsNeedAWritableTempDirAndLeaveNothingInIt` |
| R103 | downloads use the configured temporary directory | `TestDownloadsNeedAWritableTempDirAndLeaveNothingInIt` |
| R105 | a download gives its place back when it ends | `TestDownloadsAtOnceAreBounded` |
| R106 | a 401/403/404 of the embedder is not an input error | `TestPermanentErrors`、`TestAnExpiredEmbedderCredentialMarksNothingPermanent` |
| R107 | an embedder that refuses the caller is counted and logged | `TestAnExpiredEmbedderCredentialMarksNothingPermanent` |
| R108 | object keys carry the database's tenant id | `TestObjectKeysUseTheCanonicalTenant` |
| R110 | the erased subject's own packages lose their query and their name | `TestSubjectErasureRedactsTheMembersOwnPackagesAndRevocations` |
| R111 | a document the erased subject withdrew no longer names them | `TestSubjectErasureRedactsTheMembersOwnPackagesAndRevocations` |
| R112 | an upload reserves its own length, not the most its type may have | `TestUploadBytesInFlightAreBoundedAndDownloadsAreStreamed` |
| R113 | a length above the limit of its type is refused before the body is read | `TestUploadBytesInFlightAreBoundedAndDownloadsAreStreamed` |
| R114 | maxInFlightUploadBytes must cover the largest original | `TestValidateUploadBudget` |
| R116 | an erasure that finished is no longer counted as waiting | `TestErasureWaitsForTheObjectStore` |
| R11 | an input error of one chunk is recorded and skipped, not retried for ever | `TestBackfillSkipsAChunkThatCannotBeEmbedded`、`TestPermanentEmbeddingErrorIsClearedByAModelChangeOrAnAdministrator` |
| R26 | every upload of an original has its own object key | `TestOriginalReplacementRacesAndFailures` |
| R29 | the object key is built from the database's document id | `TestOriginalKeyUsesTheCanonicalID` |
| R86 | only an input error is a permanent embedding error | `TestPermanentErrors`、`TestTransientEmbeddingFailureIsRetried` |
| R93 | the bytes of uploads in flight are bounded over all callers | `TestUploadBytesInFlightAreBoundedAndDownloadsAreStreamed` |
| R115 | an erasure waiting for the object store is counted | `TestErasureWaitsForTheObjectStore` |
| R12 | an endpoint that is down is not mistaken for a poisoned chunk | `TestBackfillSkipsAChunkThatCannotBeEmbedded` |
| R80 | an unreachable object store does not consume an erasure attempt | `TestErasureWaitsForTheObjectStore` |
| R117 | the injection scan covers the heading path line a model sees, not only the body | `TestInjectionInAHeadingIsFlaggedLikeOneInABody` |
| R118 | one caller's downloads are bounded within the global limit | `TestDownloadsPerCallerAreBounded` |
| R119 | the routes that move originals have their own deadline | `TestTransferRoutesHaveTheirOwnDeadline` |
| R104 | concurrent downloads are bounded | `TestDownloadsAtOnceAreBounded` |
| R120 | queries are kept off the query endpoint until its probe has passed | `TestGuardKeepsQueriesOffAnEndpointThatHasNotPassed`、`TestGuardRefusesADifferentDimensionOrModel`、`TestAMismatchedQueryEndpointIsNotUsed`、`TestAQueryEndpointNotYetCheckedIsNotUsed` |
| R121 | a query is never embedded on the documents endpoint instead (lexical only) | `TestGuardKeepsQueriesOffAnEndpointThatHasNotPassed`、`TestGuardRefusesADifferentDimensionOrModel`、`TestAMismatchedQueryEndpointIsNotUsed`、`TestAQueryEndpointNotYetCheckedIsNotUsed` |
| R122 | the probe compares the dimensions | `TestGuardRefusesADifferentDimensionOrModel` |
| R123 | the probe requires a cosine of 0.999 | `TestGuardToleranceIsACosineOf0999`、`TestGuardRefusesADifferentDimensionOrModel`、`TestAMismatchedQueryEndpointIsNotUsed` |
| R124 | an inconclusive probe leaves a passed (or failed) check as it is | `TestGuardAnInconclusiveProbeChangesNothing` |
| R125 | embedding requests are counted by the role they serve | `TestCountedTellsTheRole` |
| R126 | a different model for queries is refused at start | `TestEmbeddingQueryEndpoint` |
| R127 | different dimensions for queries are refused at start | `TestEmbeddingQueryEndpoint` |
| R128 | the query endpoint does not inherit the main endpoint's key | `TestEmbeddingQueryEndpoint` |
| R129 | the probe uses the kept reference vector (the documents endpoint is not asked) | `TestGuardUsesAKeptReferenceAndNeverAsksABusyDocumentsEndpoint`、`TestGuardKeepsTheFirstAnswerOfTheDocumentsEndpoint`、`TestTheProbeReferenceSurvivesARestart` |
| R130 | the documents endpoint's first answer is kept | `TestGuardKeepsTheFirstAnswerOfTheDocumentsEndpoint`、`TestGuardReplacesAStaleReference`、`TestTheProbeReferenceSurvivesARestart` |
| R131 | a kept reference that does not match is replaced by a fresh answer, not trusted | `TestGuardReplacesAStaleReference` |
| R132 | the reference is keyed by the model identity (another identity borrows none) | `TestTheProbeReferenceSurvivesARestart` |
| R133 | references are read and written under the embedder role (row-level security) | `TestTheProbeReferenceSurvivesARestart` |
| R134 | no reference and a busy documents endpoint leaves the state pending, not unreachable | `TestGuardAnInconclusiveProbeChangesNothing`、`TestGuardKeepsTheFirstAnswerOfTheDocumentsEndpoint`、`TestTheProbeReferenceSurvivesARestart` |
| R135 | documents are embedded on the main endpoint (the wiring main.go uses) | `TestQueriesAndDocumentsUseTheirOwnEndpoints`、`TestWithoutAQueryEndpointQueriesUseTheMainOne`、`TestAnEmbedderOutageIsCountedPerRole` |
| R136 | queries go through the guard, never straight to the query endpoint | `TestAMismatchedQueryEndpointIsNotUsed`、`TestAQueryEndpointNotYetCheckedIsNotUsed` |
| R137 | a whole-request failure is counted as unavailable, per role | `TestFailureKindsAndLastSuccess`、`TestAnEmbedderOutageIsCountedPerRole` |
| R138 | the last-success time moves only on a success | `TestFailureKindsAndLastSuccess` |

R04、R05 在评测测试（`TestRetrievalOnTheLargeSet`、`TestRetrievalOnTheHeldOutSet`，`make test-eval`）里失败（R04 还在 `TestKnowledgeHeadingIndexSetting` 失败），其余在 `make test`（单元）与 `make test-integration`（集成）里失败。没有单独一行的控制：对象键用数据库租户 id 时，擦除前缀里的租户 id 也用数据库的写法（`deleteOriginals` 里的 `current_tenant()` 查询）——擦除的工作线程本来就用数据库的写法调用，只有键本身是令牌写法时才露出来（R108）；擦除里 `verifyPersonalErased`（核对个人库没有残留）——栅栏的认领（R21）已经保证没有残留，核对是第二道防线，单独删掉不会让任何测试失败，与认领一起删时 R21 的测试失败；媒体类型白名单在路由、`CheckOriginalWrite`、`PutOriginal` 三处各查一次，R46 把三处一起删；"先查权限再读请求体"在路由和 `PutOriginal` 各有一道，R47 把两道一起删。

## 25. 组件边界：知识留在 CMI（2026-11-25，决策人裁定）

问题：知识库的路线图（入库管道、连接器、去重与冲突、知识缺口、负责人与复核、外部 MCP 入口）远大于记忆，要不要把知识管理拆成独立的仓库或组件。2026-11-25 实测：CMI 非测试 Go 代码约 1.36 万行（含测试约 2.4 万行），按文件名粗分，知识相关（文档、分块、原件、嵌入）约 3600 行，约四分之一；记忆约 1000 行，擦除约 650 行，上下文包与解析器约 700 行。

| # | 决策 | 理由 |
|---|---|---|
| D60 | **知识管理留在 CMI，不拆仓库**：知识的权威仍是 CMI（平台 `catalog/ownership.yaml`：KnowledgeDocument、KnowledgeBase、知识文档的原件）。**文档转换与 OCR 另立运行单元，不进 CMI 的进程**（平台 `docs/adr/knowledge-base-plan.md` 第 5 节）：第一期（有文字层的 PDF 与 Office，不用视觉模型）用 idp-sandbox 的 office 档位（`pdftext`、`officecli`，在 gVisor 里隔离），按计划选择点 1 的 (a) 另部署一个专做摄入的实例，不新建仓库；计划第 5 节列的三处（文件进出只接 Workspace、沙箱只认网关签名的调用者、容量）在 KB3 开头先实测。选定 OCR / 版面解析器之后，常驻的文档处理服务（选择点 1 的 (d)）放进**独立的新仓库**，不做成 idp-sandbox 里的又一个程序；接口是"文件进，带页码、标题与表格位置的结构化文本出"，它不拥有数据，原件与转换结果仍归 CMI。**先拆进程，再谈拆仓库**：同一仓库出两种运行形态，一种是 API 与 resolve，一种是后台工作（嵌入投影与补算、连接器处理与入库编排、擦除清理、整合、过期清扫），分开部署，资源互不影响。现在 `serve` 把两者放在同一个进程里，`IDP_CMI_INGEST=0` 只关掉连接器处理与擦除清理。 | **三处耦合**：(1) 一次 `resolve` 同时取记忆与知识，合在一起排序（`kinds`），封成一个上下文包；(2) 两者共用同一套可见性（租户 RLS、ACL、组织单元 D22、敏感级别、外部模型上限 D29），权限与检索在同一条 SQL 里（D7、D11）。拆开要么两边各写一套权限（会漂移），要么跨服务合并结果（做不到同一条 SQL）；(3) 包同时引用知识版本与记忆（`VerifyPackage`），一次主体擦除覆盖七个域并只给一个"已核实"（D20、D57），拆开就成了跨服务、要补偿的流程。**另外**：嵌入投影与补算、D58、D59、pgvector、评测工具由记忆与知识共用；改所有者牵动契约 `idp.telos.cmi` 与 HOST、EER、Workspace 三个使用方；每多一个仓库都要 CI、镜像检查、BOM、RUNBOOK、契约、compose 与 kind 接入和验收路径；第一期还没有真实用户，现在拆，大概率要拆第二次。**文档转换要独立的理由硬得多**：解析不可信文件是攻击面（与 idp-browser 渲染不可信网页同类）；依赖与 CMI 完全不同（Python，可能要 GPU，体积大、更新快）；它无状态、不拥有数据，拆出去不牵动权限与擦除。 |

**重新考虑拆仓库的条件**（出现任一条就按这里的理由重评，不凭感觉）：

- 知识的规模超出 PostgreSQL 加 pgvector 能承受的范围，需要另一种存储或检索引擎；
- 记忆与知识的权限模型开始分叉；
- 由独立的团队负责，发版节奏不同；
- 拆进程之后，入库仍拖慢检索。

阅读与维护的负担不靠拆仓库缓解：知识部分的决策可以挪到单独的 ADR 文档；知识相关代码收进自己的包、接口清楚，日后真要拆时成本更低。

## 26. 原件转成文本：专用摄入沙箱、内部文件端点与票据（2026-11-25，KB3）

计划在平台仓库 `docs/adr/knowledge-base-plan.md` 第 2.2、5 节与第 8 节 KB3，量测与设计在 `docs/adr/kb3-step1-measurements.md`，转换器的输出格式是 `docs/adr/kb3-converter-format.md`（idp-sandbox 一侧写，这里读）。这一刀做 CMI 一侧：内部文件端点与票据、最小 MCP 客户端、摄入作业与后台线程、定位信息、擦除、状态与契约 `idp.telos.cmi` 1.6.0（只做新增）。沙箱一侧的转换器、系统网关、Keycloak 客户端、摄入实例与网络策略是平台与别的仓库的事（D70）。**决策人 2026-11-25 已定、这里不再讨论的**：摄入只用一个平台工作负载身份，放在系统租户里，经专用的系统网关到达摄入沙箱；租户隔离留在 CMI（一份文档一张票据、一个新工作区用完即删、审计由 CMI 记）；摄入实例的传输上限 50 MiB；引用的定位 PDF 到页、PPTX 到幻灯片、DOCX 到标题路径、XLSX 到工作表与单元格范围；XLSX 每表至多 1 万行、200 列，超出的入库为"truncated"并告知上传的人；空输出是失败。

| # | 决策 | 理由 |
|---|---|---|
| D61 | **一次转换的路径**：CMI 的后台线程（`internal/convert`）按作业驱动摄入沙箱，经系统网关调它的五个工具：`sandbox_create` → `sandbox_import_file`（读票据，沙箱从 CMI 的文件端点取原件到 `in/original.<扩展名>`）→ `sandbox_run`（办公档位，`language: office`，一段 shell：PDF 是 `pdftext IN --markdown out/extract.md`，Word 与 PowerPoint 是 `ooxml2md IN -o out/extract.md`，Excel 另带 `--max-rows 10000 --max-cols 200`；命令行、退出码与标记按转换器格式文档）→ `sandbox_export_file`（写票据，沙箱把 `extract.md` 发回 CMI 的文件端点）→ `sandbox_delete`。**删除在 defer 里，不论成败**；工作区 id 先记进作业行，worker 崩溃后下一次尝试先删它，摄入实例的闲置回收（建议 5 分钟，计划第 5 节）兜底。一份文档一个新工作区，所以一个工作区至多装过一个租户的一份文档。每次尝试有时限（`jobTimeout`，缺省 15 分钟；租约缺省 20 分钟，必须更长，启动校验）。没有 `knowledge.conversion` 或 `enabled: false` 时，原件只存不转，和 D50 一样 | 摄入沙箱和成员的交互沙箱是两个实例，限额、并发和回收各自的（量测第 4.6 节）；工作区用完即删，是"租户隔离留在 CMI"的另一半 |
| D62 | **内部文件端点与票据**（安全边界）：独立的监听（`IDP_CMI_INGEST_LISTEN`，只给内网，不得与 API 同址，启动校验），两条路由：`GET /content`（读票据：这份文档的原件，头 `Idp-File-Ref`、`Idp-File-Sha256`、`Content-Type`、`Content-Disposition`，先按行里的摘要核对再发）与 `POST /`（写票据：转换结果，正文受 `maxResultBytes` 限制，应答 `{ref, name, size, sha256}`）。**沙箱不验票据的签名**（只读 `tenant_id`、`sub`、`kind` 并与调用者比对，再把整张票原样交给端点），所以验证全在这里。票据是三段式（沙箱的 `Peek` 要这个形状）、HMAC-SHA256、密钥是 CMI 的文件（`IDP_CMI_INGEST_TICKET_KEY_FILE`，至少 32 字节）。声明：`tenant_id`、`sub`＝摄入工作负载（沙箱拿它和网关断言的调用者比），`kind`（`read`／`write`）、`aud`（端点，`endpointUrl`）、`exp`（`ticketTtl`，缺省 5 分钟，每张在用之前才签）、`iat`、`jti`，以及 CMI 自己的：`dt`（文档的真实租户）、`doc`、`ver`、`jid`（作业）、`att`（第几次尝试）、`key` 与 `sha`（原件的对象键与摘要）。**端点逐项验**：头必须正是我们的（算法固定）、签名、签发者、受众、有效期、`kind` 与路由一致；然后**单次使用**——`jti` 写进 `knowledge_ticket_uses`（主键插入、冲突即已用过），所以跨重启和副本成立，记录在票据过期一小时之后清掉；然后**对着数据库**核对票据所指：作业存在、`running`、租约未过、是这一次尝试（`att`）、作业与文档行的原件键和摘要都等于票据所写、版本相同、文档没被擦除也没被撤回；写票据还要这次尝试没有收过结果。任何一项不符都是同一个 403 加同一句话（不告诉调用者哪一项）。**5xx 与 429 不带错误信息**：沙箱的客户端（`files.status`）有信息就打印"the Workspace refused the file: <信息>"，没有才打印"(HTTP n)"，CMI 的 worker 就靠这个区分"端点忙或坏了，再试"（存储不可达 503、内部错误 500、读的并发到上限 429）与"拒绝"（带信息的 4xx，永久）；`convtest` 的替身逐字照 `files.status` 与 `transferFailure` 打印（那个模块的包是 internal，不能直接引用，所以是带出处的拷贝）。**读的并发上限在使用票据之前取**：因为忙而被挡回去的票据还可以再用。写票据收到的正文先暂存为对象 `kb/<租户>/<文档 id>/<版本>/result-<作业>-<尝试>-<jti>`（在文档的前缀下，擦除按前缀清扫，D54），作业行记下键与摘要；**写票据不往文档里写任何东西**，提交在 D64 | 沙箱不验签名，所以端点的实现错误（没查单次使用、没限定到这份文档）就是跨租户的读写；"票据只对这一份文档、这一次尝试、这一个方向、一次"是测试逐条对抗的（下表 R139 起） |
| D63 | **状态、通知与重试**：每份文档版本带最近一次作业的 `ingest`（`state`：queued、running、succeeded、truncated、failed；`attempts`；`error` 原因码；`notice` 给上传者的说明；`updatedAt`、`nextAttemptAt`），成员与管理员的文档读取、列表和存原件的应答都有，契约 1.6.0 与 OpenAPI 同步。**给上传者的通知用审计加文档上的字段**：CMI 没有事件或通知机制，不另造；每次摄入的结果记审计（`knowledge.ingest.queued／succeeded／truncated／failed`，操作者是上传的人，detail 有作业、次数、块数、notice、原因码），上传的人在自己的文档上读到 `ingest.notice`（被截掉的工作表，没有文字层的页），Workspace 的页面（KB5）读这个字段。原因码是固定的几个词，**转换器的原话只进日志与审计的 detail（至多 300 字节），不进接口**。`POST /v1/knowledge/documents/{id}/ingest` 重新排队（失败之后，或再来一次）：权限与存原件相同，已有排队或运行中的是 409，没有可转换的原件是 422，没开转换是 503 | 上传的人要知道他的文件有没有进来、少了什么；失败的文档要有一个办法再试 |
| D64 | **一个版本的文本与原子提交**：调用者先写这一版（带一段占位文本，最简单的是标题），再存 PDF 或 Office 原件；存原件的事务里，如果这一版是 `active` 且文本还是调用者给的（`text_source = supplied`），**它改成 `pending`**（新增的存储状态，契约 1.6.0 的 `status` 增一个值）：它不是现行（`knowledgeCurrentSQL` 只认 active 与 superseded）、**不取代任何版本**（取代的判断只数 active、superseded、expired）、不被检索、不被嵌入。所以**转换期间，它要取代的前一版照常可取**，半份文档永远搜不到。提交（`CommitConversion`）在**一个事务**里：锁库行、文档行、作业行；核对文档没被擦除或撤回、原件还是作业所指的那份、作业仍是这一次尝试（租约丢了的 worker 提交不了）、配额；然后删除这一版的全部块、写入新的块（定位信息、标志、索引文本照 D49，注入扫描照 D12，范围含标题路径、表名、工作表名）、排投影事件（`version` 是新的 `revision`，D26）、`pending` 变 `active`、`text_source = extracted`，`content_sha256`、`byte_size`、`chunk_count` 更新，`revision` 加一，作业结案。**已经是提取出来的文本的版本**（再转一次，或换了原件）状态不变，旧文本照常可取，直到提交把它换掉。失败的转换让这一版留在 `pending`：前一版仍是现行，上传的人或管理者可以撤回它（撤回接受 pending）、重试或换原件。**配额**：`pending` 的版本连同占位文本与原件一起计入用量（否则失败的转换让原件无限堆积）；提交时文本替换占位文本，并释放它要取代的那一版的字节（版本晚于现在才生效的除外，D52）。**一个等着第一份文本的 `pending` 版本不接受不转换的原件**（文本、Markdown、图片等）：存原件得 409，因为没有什么会去换掉那段占位文本，这一版就永远 pending（要换成 PDF 或 Office 文件，或撤回这一版）。提交之后嵌入由现有的投影与回填接着做（D26、D58），这条路径里不调模型。这一版的文本是**原地**换掉的，而不是新开一个版本：原件的对象键在文档 id 之下（D54），擦除按文档的前缀清扫，新开版本就得复制原件，或让两个文档指向同一个对象 | "转换期间旧版本照常可取，半份文档搜不到"（计划第 8 节 KB3 的验收）；`pending` 用一个状态而不是另一个字段，是因为"现行"的定义只有一处（D52），所有只认 active 的查询自动排除它 |
| D65 | **标记与定位**：转换器的输出格式是 `docs/adr/kb3-converter-format.md`（两边都读，要改就在末尾加带日期的注记）。分块器认 `<!-- page: N -->`、`<!-- slide: N -->`、`<!-- sheet: NAME range: A1:F100 -->`、`<!-- table: ... -->`（D49 已有的，加上 slide 与 sheet），`locator` 增 `slide`、`sheet`、`range`（1.6.0）。**XLSX 的单元格范围从行号来**：格式规定每个 Markdown 表的第一列是 Excel 的行号，所以一块的范围是（这一块第一行与最后一行的行号，标记里的列范围），例如 `A12:F40`；表按行分组时每块有自己的范围；从一行中间切下来的块没有行号，取标记的整个范围。工作表名在索引文本、向量文本和注入扫描里（和标题路径、表名一样，D49），名字里的 `-->`（转换器写成 `--&gt;`）读回来。**只描述转换的标记**（`truncated`、`no-text`、`notes`、`skipped`、空表）由 `CleanExtracted` 从文本里取出，不进索引也不给模型看：`truncated` 让作业成为 `truncated` 并成为通知；`no-text: page N` 汇总成"N 页没有文字层"的说明。**没有正文的结果（只有标记与空白）是失败**（`empty_output`），不论转换器的退出码；清理后超过一份文档的文本上限（4 MiB，`MaxDocument`）也是失败（`text_too_large`）。**NUL 与其它 C0 控制字符（除制表符与换行）在清理时去掉**（PostgreSQL 的文本放不下 NUL）；`page` 与 `slide` 的数字不是 1 到 100 万之间的整数就当没有位置（不是错误），否则一个溢出的数字会让整份文档的写入失败。DOCX 的 `page` 只在显式分页符处出现，Word 的引用主要靠标题路径 | 引用要能回到原件的具体位置（计划第 0 节） |
| D66 | **队列、重试与失败的分类**：`knowledge_conversions` 一行一次转换（状态 queued、running、succeeded、truncated、failed；`attempts`、`next_attempt_at`、租约、`error_code`、时间戳），存原件时创建，**该文档上还没结束的旧作业同时作废**（`replaced`）。后台线程按 `SKIP LOCKED` 跨租户取作业（专用角色 `app.role = 'converter'`，和嵌入线程一样；两张新表照常 ENABLE 加 FORCE 行级安全），一次最多取"空位数"个，空位 = `concurrency` − 在跑的；**`concurrency` 不得超过摄入沙箱自己的并发（`sandboxConcurrency`，缺省 8），启动校验**，因为沙箱对第 9 个并发的运行是直接拒绝、不排队。租约过期的 running 作业被重新取走（worker 死了），用完次数就失败。**忙不是失败**：沙箱满了（`sandbox_busy`）和 CMI 自己的读并发上限（429）不花这一次尝试——尝试数退回去，一分钟多一点之后再来——，只有一个作业前后一共等了超过 6 小时（`MaxBusyWait`）才算失败。**并发上限跨副本成立**：取作业时在一把咨询锁下数一下租约未过的 running 作业，取的个数不超过 `sandboxConcurrency` 减去它们（`concurrency` 仍是每个副本自己的）。**关机不花尝试**：被关机打断的尝试立即退回队列（`ReleaseConversion`），尝试数也退回。**暂时的**（退避 30 秒起翻倍、至多 30 分钟，共 `maxAttempts` 次，缺省 6）：沙箱满了或工作区数到上限（`sandbox_busy`）、网关 401／429／5xx、连不上或超时——包括网关对一次调用的 60 秒上限（`gateway_timeout`，结果不明，下一次是新的工作区）——、对象存储不可达、转换器退出码 1（`converter_internal`，只试两次，格式文档说"可重试一次"）、提交时数据库出错（但 SQLSTATE 22 开头的数据错误是永久的，`output_invalid`，重跑也一样）。**永久的**：转换器退出码 3、4、5、6 与其它（`unsupported`、`corrupt`、`limits`、`no_text`、`conversion_failed`）、运行超时（`conversion_timeout`）、内存不够（`conversion_oom`，看沙箱的标志，也看标准错误里的 `OutOfMemoryException`，因为 officecli 内存不够时退出码是 0）、沙箱拒收文件（`import_refused`：类型、宏、解压炸弹、超过传输上限）、结果太大、空输出、配额不够（`quota_exceeded`）、文档在排队期间被撤回、擦除或换了原件（`document_changed`，**在叫沙箱做任何事之前**就结案）。 | 沙箱的限额是整个实例一套，批量入库不能把摄入实例刷成一片错误；"转换错误是永久的"，同一份文件不会无限重试 |
| D67 | **工作负载身份与到网关的客户端**：CMI 用 OAuth 2.0 `client_credentials`（RFC 6749 4.4）以一个客户端的身份取令牌（`internal/workload`：令牌端点、客户端 id、**密钥文件**是设置，每次续期重读，所以轮换不用重启；令牌到期前十分之一寿命（至少 30 秒）续，网关答 401 时丢掉那一张、换一张再试一次；令牌端点不跟重定向、不走环境代理；密钥与令牌不进日志）。Keycloak 只是现在回答这个授权的那一个：CMI 只认"一个 OIDC 客户端的凭据"。MCP 客户端（`internal/mcpclient`，照 idp-host 的）说 2026-07-28：每次调用一个 POST，头 `MCP-Protocol-Version`、`Mcp-Method`、`Mcp-Name`，`_meta` 带协议版本、`clientInfo`、`clientCapabilities`，应答是 JSON 或恰好一条 SSE 消息，没有环境代理、不跟重定向，只做 `tools/call`（目录在网关发布时已经评审钉住，不用发现）。单次调用的等待是 `callTimeout`（缺省 `runSeconds` + 90 秒），**必须大于系统网关的 `IDP_CALL_TIMEOUT`**（量测第 4.4 节：运行时限设到几百秒，网关的默认 60 秒就是 `UNKNOWN`）。工具名是网关给目标名加的前缀（`toolPrefix`，缺省 `ingest__`）。**`hostOnlyTools` 的陷阱**（量测第 3.1 节第 3 点）：`ingest__*` 不能写进网关的 `hostOnlyTools`——写了，CMI（不是 HOST）就调不了摄入工具，摄入悄悄断掉；访问控制靠"只有 CMI 的角色有授权加客户端名单"。这一条在平台的验收里要有正向检查（D70） | 网关一个实例只服务一个租户（idp-mcp D6），系统租户的主体走专用的系统网关；成员与 HOST 的令牌到不了它 |
| D68 | **限额**：摄入实例的传输上限 50 MiB，与 CMI 的原件上限一致（D54：PDF、PPTX 与图片 50 MiB，Word 与 Excel 25 MiB）；Excel 每个工作表 1 万行、200 列（两个常量在 `internal/convert`，作为 `--max-rows`、`--max-cols` 交给转换器；转换器流式读取并写 `truncated` 标记）；暂存的结果至多 `maxResultBytes`（6 MiB），一份文档的文本至多 4 MiB（`MaxDocument`）。配额在提交时检查（D55 的算法：这一版的文本替换原来的；pending 的版本还要带上它的原件，并释放它要取代的那一版的字节）。一次运行的时限 `runSeconds` 不得超过摄入实例的 `IDP_SANDBOX_TIMEOUT_S` | 量测第 4.1 节：Excel 大表 736 MB 内存、默认 20 秒的运行时限 |
| D69 | **擦除**覆盖提取出来的东西：**文本**（块的内容、索引文本、标题路径、表名、**工作表名与单元格范围**在 `content` 域里换成墓碑或清空；向量与词面索引在原来的两个域）；**作业**与**用过的票据**在 `content` 域里删除，域结束前核对一个都不剩（否则该域失败，擦除不标 verified）；**暂存的结果**是文档前缀下的对象，`originals` 域按前缀清扫、删完再列一遍（D54）；作业自己结束时也删它（提交、失败、被新原件作废、取作业时发现次数用完），不等到擦除；**错误信息与审计 detail 里像票据的三段式令牌（`eyJ…`）存入前先换成 `[redacted]`**；**审计**里 `knowledge.ingest.*` 的 detail（可能引用文档的工作表名与转换器的话）和 `knowledge.create` 一样被缩成"已擦除"。栅栏照旧是 `erasure_id`：擦除开始之后，端点拒绝这份文档的票据（核对在读完正文之后再来一遍，暂存的对象在拒绝时自己删掉），worker 的提交拒绝它，沙箱里剩下的工作区由 worker 的 defer 删、摄入实例的闲置回收兜底。**主体擦除**的个人库文档同样（包括栅栏之后才到的）。**局限**（量测第 6.5 节第 9 点）：工作区里短暂放着原件明文，一个崩溃的 worker 最多留到实例的闲置回收（建议 5 分钟）；"擦除之后沙箱里没有明文"不是这里的保证，这一点在这里承认 | 擦除标了 verified，就不能有明文还在 |
| D70 | **平台要接的（CMI 之外，这一刀没有改别的仓库）**：①**系统网关**：一个单独的 `mcp-gateway` 实例，租户是系统租户，Release 里只有 `ingest` 这一个目标，客户端名单只有 CMI，`IDP_CALL_TIMEOUT` 放大（大于 `callTimeout`）；`ingest__*` **不放进** `hostOnlyTools`；验收里要有正向检查（CMI 看得到并能调，HOST 与成员看不到）；②**Keycloak 客户端**：service account，`fullScopeAllowed: false`，受众映射成系统网关，硬编码系统租户的 `tenant_id`，角色 `cmi-ingest`，令牌的 `sub` 等于 `workloadSubject`（网关从 `sub` 取调用者，idp-mcp `adapters/identity/jwt.go`；平台给这个服务账户用户一个固定 id，写进 `workloadSubject`。2026-11-25 平台首次端到端运行时更正：原来写的 `preferred_username` 不对，按它配置时摄入实例拒绝票据，报"文件引用属于别人"）；密钥文件给 CMI；③**摄入实例**：第二个 idp-sandbox，`IDP_SANDBOX_FILES_URL` 指向 CMI 的内部端点（与 `endpointUrl` 完全一致），传输上限 50 MiB、工作区至少 160 MiB、内存按并发留（每路约 110 MiB，Excel 大表另算）、运行时限与 `runSeconds` 一致、`IDP_SANDBOX_MAX_CONCURRENT` 与 `sandboxConcurrency` 一致、`IDP_SANDBOX_MAX_PER_CALLER` 不小于它（所有调用者是同一个身份）、闲置回收 5 分钟；办公镜像里要有 `ooxml2md` 与 `pdftext --markdown`（另一个 agent 在 idp-sandbox 的 `kb3` 分支做）；Kubernetes 上独立的运行命名空间与 ConfigMap 配额 60；④**网络策略**：摄入实例的 Pod → CMI 的内部端点端口（只放它），CMI → 系统网关，CMI → 令牌端点，运行 Pod 无网络，成员侧的 Pod 到不了内部端点；⑤**CMI 的部署**：内部端点的 Service（不暴露到集群外）、`IDP_CMI_INGEST_LISTEN`、票据密钥的 Secret、配置文件的 `knowledge.conversion`。**没有做的**：扫描件与 OCR（KB4；`no_text` 是它的入口）；对象存储里崩溃留下的暂存对象的后台清扫（和 D54 一样，下一次擦除时才会被找到，只是占存储）；票据密钥的无缝轮换（滚动更新时旧副本签的票被新副本拒绝，作业当暂时的错误重试；要无缝就得同时认两把）；计划里的数字与文字层交叉核对（`extraction:low_confidence`）；票据密钥的文件权限必须是 0600 或 0400、至少 32 字节且至少 16 种不同的字节（启动校验），端点、网关或令牌端点用 http 时启动记一条警告；**已知的缺口（这里记下，没有做）：队列没有按租户的公平性**（一个租户的批量上传排在前面会让别人等），**票据密钥的轮换用 `kid` 同时认两把**；内部端点的传输加密缺省关（可选 TLS；内网的明文在网络策略与五分钟、单次、绑定到一份文档的票据之后） | 这一刀的验收路径要第一次真正"CMI 经系统网关调摄入沙箱"（量测第 3.3 节），在平台的 compose 与 kind 上跑 |

### 删除表（续：每一行都在隔离的副本里删掉那一处控制、跑对应的测试、看到它失败；2026-11-25）

| # | 删掉的控制（每行只删这一处） | 失败的测试 |
|---|---|---|
| R139 | the ticket's signature is verified | `TestForgedExpiredWrongKindAudienceAndKeyAreRefused`、`TestForgedExpiredWrongKindAndWrongAudienceTicketsAreRefused` |
| R140 | the ticket's audience is checked | `TestForgedExpiredWrongKindAudienceAndKeyAreRefused`、`TestForgedExpiredWrongKindAndWrongAudienceTicketsAreRefused` |
| R141 | the ticket's expiry is checked | `TestForgedExpiredWrongKindAudienceAndKeyAreRefused`、`TestForgedExpiredWrongKindAndWrongAudienceTicketsAreRefused` |
| R142 | the ticket's kind is checked | `TestForgedExpiredWrongKindAudienceAndKeyAreRefused`、`TestForgedExpiredWrongKindAndWrongAudienceTicketsAreRefused` |
| R143 | a ticket works once (the use is recorded in the database) | `TestAReadTicketServesItsOriginalOnceWithTheHeadersTheSandboxReads`、`TestATicketPresentedToAnotherReplicaIsStillUsed` |
| R144 | the endpoint refuses a ticket the store says was used | `TestAReadTicketServesItsOriginalOnceWithTheHeadersTheSandboxReads`、`TestATicketPresentedToAnotherReplicaIsStillUsed` |
| R145 | a ticket is for a running job | `TestATicketOfAJobThatIsNotRunningOrWhoseOriginalWasReplacedOrErasedIsRefused` |
| R146 | a ticket is for the attempt it was made in | `TestATicketThatDoesNotFitItsJobIsRefused` |
| R147 | a ticket is for the original of its job and its document (each check shadows the other: both go) | `TestATicketThatDoesNotFitItsJobIsRefused` |
| R148 | a ticket of a withdrawn or erased document is refused | `TestATicketOfAJobThatIsNotRunningOrWhoseOriginalWasReplacedOrErasedIsRefused` |
| R149 | one result for an attempt (the check when the ticket is bound and the check when the result is recorded: both go) | `TestAWriteTicketStagesOneResultWithinTheLimit` |
| R150 | a result is bounded as it is read | `TestAWriteTicketStagesOneResultWithinTheLimit` |
| R151 | a staged result is under the document's prefix | `TestErasingAMemberCoversTheirConversionsInFlight`、`TestAWriteTicketStagesOneResultWithinTheLimit` |
| R152 | a version that waits for its text is pending | `TestAPdfOriginalBecomesSearchableTextWithPages` |
| R153 | an empty conversion is a failure | `TestConversionErrorsArePermanentAndLeaveTheOldVersionCurrent` |
| R154 | a text over the limit of a document is a failure | `TestATextOverTheLimitOfADocumentIsAFailure` |
| R155 | an out-of-memory conversion with exit 0 is read from stderr | `TestConversionErrorsArePermanentAndLeaveTheOldVersionCurrent` |
| R156 | a conversion that timed out is a permanent failure | `TestConversionErrorsArePermanentAndLeaveTheOldVersionCurrent` |
| R157 | the sandbox's capacity refusal is transient | `TestBusySandboxAndGatewayTroublesAreRetried`、`TestConcurrencyStaysWithinTheSandboxAndTheNinthRunIsRetried` |
| R158 | a gateway that answers with a failure status is transient | `TestBusySandboxAndGatewayTroublesAreRetried`、`TestTransientFailuresEndInFailureAfterTheAttempts` |
| R159 | a gateway call that timed out is recognized | `TestAGatewayTimeoutIsTransient` |
| R160 | the converter's own error is tried once more, not for ever | `TestATransientConverterErrorIsTriedOnceMore` |
| R161 | the workspace is deleted whatever happened | `TestAPdfOriginalBecomesSearchableTextWithPages`、`TestConversionErrorsArePermanentAndLeaveTheOldVersionCurrent`、`TestAGatewayTimeoutIsTransient` |
| R162 | the worker takes no more jobs than it has places | `TestTheWorkerRunsNoMoreConversionsThanItIsToldTo` |
| R163 | a job whose worker died is taken again | `TestAJobOfADeadWorkerIsTakenAgain` |
| R164 | a worker that lost its lease cannot commit | `TestAWorkerThatLostItsLeaseCannotCommit` |
| R165 | a conversion nobody wants is ended before the sandbox is asked | `TestAPendingVersionCanBeWithdrawnAndItsConversionThenCommitsNothing` |
| R166 | a conversion that does not fit the quota is a failure | `TestAConversionOverTheQuotaFailsWithoutCommitting` |
| R167 | a truncated conversion ends as truncated | `TestOfficeOriginalsAreLocatedBySlideHeadingAndCells` |
| R168 | the uploader's notice is recorded | `TestAPdfOriginalBecomesSearchableTextWithPages`、`TestOfficeOriginalsAreLocatedBySlideHeadingAndCells` |
| R169 | a notice is one bounded line without controls | `TestNoticesAreOneBoundedLineWithoutControls` |
| R170 | the markers that only describe the conversion are taken out of the text | `TestCleanExtractedTakesOutTheDescribingMarkers` |
| R171 | a truncated marker makes the conversion truncated | `TestCleanExtractedTakesOutTheDescribingMarkers`、`TestOfficeOriginalsAreLocatedBySlideHeadingAndCells` |
| R172 | a slide marker locates the chunks after it | `TestChunkKnowledgeLocatesSlidesAndSheets`、`TestOfficeOriginalsAreLocatedBySlideHeadingAndCells` |
| R173 | a worksheet chunk says the cells it covers | `TestChunkKnowledgeLocatesSlidesAndSheets`、`TestALargeWorksheetTableIsCutIntoRowRanges` |
| R174 | the sheet name is in the indexed and embedded text | `TestChunkKnowledgeLocatesSlidesAndSheets` |
| R175 | the sheet name is in the text the injection scan reads | `TestChunkKnowledgeLocatesSlidesAndSheets` |
| R176 | a sheet name written with --&gt; is read back | `TestASheetNameMayContainRangeAndArrows` |
| R177 | erasure deletes the conversion jobs | `TestErasingAConvertedDocumentLeavesNoTextJobsTicketsOrObjects`、`TestErasingAMemberCoversTheirConversionsInFlight`、`TestAnErasureDuringARunCommitsNothingAndLeavesNothing` |
| R178 | erasure deletes the presented tickets | `TestErasingAConvertedDocumentLeavesNoTextJobsTicketsOrObjects` |
| R179 | erasure verifies no conversion job or ticket is left | `TestErasingAConvertedDocumentLeavesNoTextJobsTicketsOrObjects`、`TestErasingAMemberCoversTheirConversionsInFlight`、`TestAnErasureDuringARunCommitsNothingAndLeavesNothing` |
| R180 | erasure clears the sheet and the cell range of a chunk | `TestErasingAConvertedDocumentLeavesNoTextJobsTicketsOrObjects` |
| R181 | erasure reduces the audit detail of a conversion | `TestErasingAConvertedDocumentLeavesNoTextJobsTicketsOrObjects` |
| R182 | row-level security is on for the conversion jobs | `TestConversionTablesAreTenantIsolated` |
| R183 | row-level security is on for the presented tickets | `TestConversionTablesAreTenantIsolated` |
| R184 | the conversions may not exceed the sandbox's own limit | `TestConversionSettingsAreChecked` |
| R185 | the lease is above the time of an attempt | `TestConversionSettingsAreChecked` |
| R186 | the file endpoint is not on the API's address | `TestStartConversionRefusesAnUnsafeSetup` |
| R187 | the ticket key must be long enough | `TestSigningRefusesAWeakKeyAndAnIncompleteTicket` |
| R188 | the gateway client follows no redirect | `TestNoProxyAndNoRedirects` |
| R189 | the gateway client uses no environment proxy | `TestNoProxyAndNoRedirects` |
| R190 | a refused token is renewed once | `TestA401RenewsTheTokenOnceAndRepeatsTheCall` |
| R191 | one message of an event stream, no more | `TestTwoMessagesAreRefused` |
| R192 | the token is renewed before it expires | `TestTheTokenIsKeptAndRenewedBeforeItExpires` |
| R193 | the token endpoint is not followed to another address | `TestTheTokenEndpointIsNotFollowedToAnotherAddress` |
| R194 | a pending version can be withdrawn | `TestAPendingVersionCanBeWithdrawnAndItsConversionThenCommitsNothing` |
| R195 | a conversion is queued one at a time per document | `TestRetryingAFailedConversion` |
| R196 | only an original that is converted can be converted again | `TestRetryingAFailedConversion` |
| R197 | originals are only stored when conversion is off | `TestWithoutConversionOriginalsAreOnlyStored` |
| R198 | only PDF and Office originals are converted | `TestAMarkdownOriginalIsNotConverted`、`TestRetryingAFailedConversion` |
| R199 | a ticket names the ingest workload, not the document's tenant | `TestAPdfOriginalBecomesSearchableTextWithPages` |
| R200 | the sandbox's conversion has the one limit of rows and columns | `TestOfficeOriginalsAreLocatedBySlideHeadingAndCells` |

### 删除表（评审之后补，2026-11-25，每一行都跑过）

| # | 删掉的控制（每行只删这一处） | 失败的测试 |
|---|---|---|
| R201 | versions that wait for their text are counted against the quota | `TestVersionsWaitingForTheirTextCountAgainstTheQuota` |
| R202 | a pending version takes no original that is never converted | `TestAPendingVersionIsNotGivenAnOriginalThatIsNeverConverted` |
| R203 | the staged text of a job that is given up is deleted | `TestStagedTextIsDeletedWhenItsJobEnds` |
| R204 | a failed job's staged text is deleted | `TestStagedTextIsDeletedWhenItsJobEnds` |
| R205 | the staged text of a job with no attempts left is deleted at claim | `TestAJobThatDiedOnItsLastAttemptFailsAtClaimAndLosesItsStagedText` |
| R206 | a busy sandbox does not spend an attempt | `TestABusySandboxDoesNotSpendAttemptsButDoesNotWaitForEver`、`TestBusySandboxAndGatewayTroublesAreRetried` |
| R207 | a job does not wait for a busy sandbox for ever | `TestABusySandboxDoesNotSpendAttemptsButDoesNotWaitForEver` |
| R208 | the sandbox's limit holds over all replicas | `TestSandboxConcurrencyHoldsOverReplicas` |
| R209 | an attempt cut off by a shutdown is given back unspent | `TestAnInterruptedAttemptIsGivenBackUnspent` |
| R210 | a classified error keeps its cause | `TestACancelledContextIsStillOneAfterClassification` |
| R211 | a ticket-shaped text is redacted before it is stored | `TestATicketShapedTextIsNeverStored` |
| R212 | NUL and the other C0 controls are taken out of a converted text | `TestControlCharactersAreTakenOutOfAConvertedText`、`TestControlCharactersAndHugeMarkerNumbersDoNotFailTheCommit` |
| R213 | a page or slide number that does not fit is no location | `TestAPageOrSlideNumberThatIsNoNumberOfTheDatabaseIsNoLocation`、`TestControlCharactersAndHugeMarkerNumbersDoNotFailTheCommit` |
| R214 | a data error of the database is a permanent failure | `TestADataErrorOfTheDatabaseIsPermanent` |
| R215 | the endpoint takes its place before it uses the ticket | `TestTheEndpointAtItsLimitOfReadsTurnsAwayWithoutUsingTheTicket` |
| R216 | a 5xx or 429 of the endpoint carries no message, so the sandbox prints (HTTP n) | `TestTheEndpointAtItsLimitOfReadsTurnsAwayWithoutUsingTheTicket`、`TestWhatTheEndpointAnswersIsTold5xxFromARefusalByTheSandboxsOwnWords` |
| R217 | a ticket key that the group or others can read is refused | `TestStartConversionRefusesAnUnsafeSetup` |
| R218 | a ticket key of few distinct bytes is refused | `TestStartConversionRefusesAnUnsafeSetup` |
| R219 | a ticket presented after the lease lapsed is refused | `TestATicketPresentedAfterItsLeaseLapsedIsRefused` |
| R220 | presented tickets are forgotten after they would have expired | `TestPresentedTicketsAreForgottenAfterTheyWouldHaveExpired` |

没有单独一行的控制：**提交前才跑注入扫描（不在库行锁之下）**，没有测试，是锁持有时间的问题；**票据的头必须正是我们的**（`Verify` 里的算法固定）——签名覆盖头，改了头签名就不对，所以这一道被签名那一道遮住，单独删掉不会让测试失败，它是第二道防线；**作业行与文档行各自核对原件的键和摘要**互相遮住，R147 把两处一起删；**写票据的"这次尝试还没有结果"在核对时与记录结果时各查一次**，R149 把两处一起删；**提交时核对文档的原件仍是作业所指的那份**被作业状态（存原件时旧作业已作废）遮住，同样是第二道防线；**`SKIP LOCKED`**：并发的取作业在 READ COMMITTED 下不会重复，只会少取，删掉它没有测试会失败，但换成别的锁语句会让多副本互相等待，所以保留；文件端点的并发读上限（`maxConcurrentReads`）与缺少票据的 401 有代码，没有单独的测试。
