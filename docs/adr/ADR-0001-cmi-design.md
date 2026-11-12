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

局限与待办：两套评测数据都不是真实语料。自建集 88 条，既用来选又用来评；留出集由聊天模型模拟，它根据文档写出的问题措辞往往接近原文，所以对全文基线偏友好，嵌入的优势在真实问题上可能更大，也可能不同。两套数据给出的最佳下限不同（0.40 到 0.45）而召回在 0.45 以上陡降，说明阈值对语料敏感：**上线前必须在真实文档和真实问题上重测**，并按需给下限留余量。嵌入模型测了 Qwen3-Embedding-0.6B、granite-97m、multilingual-e5-large-instruct（R@5 0.827，余弦分布很高，下限 0.85 才能让无答案返回空，但召回掉到 0.744；该组数字未入库）；bge-m3（仓库只有 onnx）、EmbeddingGemma-2、jina v5、Qwen3-Embedding-4B/8B 未测；重排只测了 bge-reranker-v2-m3，生产要放 GPU 或接托管服务，GPU 路径尚未试；标识符类查询在语义主导模式下有一例（`HS 9503.00`）掉到下限以下，实体（`entities`）仍按精确匹配；多模态未做；pgvector 对每种维度需单独建索引，换模型要重建。Kubernetes 与 compose 里的 PostgreSQL 镜像已换成带 pgvector 的（kind 验收与备份演练在其上通过）。

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
| 10 条（评测一直用的） | 10 / 2751 | 0.964 | 0.857 | 0.906 | 0.904 | 0.785 | 0.835 | 1.000 |
| 默认 2000 token | 8.4 / 1975 | 0.940 | 0.772 | 0.853 | 0.851 | 0.620 | 0.772 | 0.972 |

（"任一 / 全部证据""答案轮"在 10 条的包里就是 @10，在默认包里是整个包。）全文检索同样下降（答案轮 0.811 → 0.772，全部证据 0.726 → 0.677）。

结论：默认预算比评测量的包少约 1.6 条，含答案那一轮的召回少 5 个点，"全部证据"少 8.5 个点，跨会话题最多（全部证据 −16.5 个点）。摘录截断几乎不丢答案（答案可见只比答案轮低 0.002，470 题里 1 题；这是上限：量的是含答案那句话的开头在不在摘录里，答案本身可能还在更后面）。要不要提高默认预算（比如 3000 token，约等于评测量的 10 条）是消费方与成本的取舍，本节只记录代价，未改默认值；HOST 的发布件可以自己设 `context.maxTokens`。

未做、可以试的：量 3000 / 4000 token 的包，看 10 条以后还涨不涨；命中一轮时带上同一会话的相邻轮次（用 token 换召回，和预算一起看）。
