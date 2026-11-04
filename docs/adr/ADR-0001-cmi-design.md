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

