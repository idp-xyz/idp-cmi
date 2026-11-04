# IDP CMI 技术路线研究：PostgreSQL / OpenSearch / Hybrid

**版本**：研究草案 R0.1  
**日期**：2026-10-26  
**对象**：IDP CMI（独立企业上下文与记忆基础设施）  
**研究性质**：基于官方文档的能力和风险评估，**不是实测报告，也不是生产选型锁定**。

## 1. 结论摘要

建议维持 **CMI 对外 API 与记忆语义独立、PostgreSQL 作为权威记忆记录、检索后端可替换** 的架构约束，以 **路线 A 为初始基线、路线 C 为重点性能候选、路线 B 为功能验证候选**。

- **A：PostgreSQL 16 + pgvector + 自研 Memory Engine**：事务、版本和治理边界清楚，部署简单；需要开发抽取、整合和混合检索质量优化。
- **B：OpenSearch Agentic Memory + CMI Governance**：OpenSearch 从 3.3 起提供原生 Agentic Memory API，支持四类记忆、提取和 ADD/UPDATE/DELETE 决策、历史轨迹；3.6 起提供长期记忆混合搜索 API。功能覆盖强，但企业级生命周期、权限、稳定性均需实测，不能把 namespace 当授权模型。
- **C：PostgreSQL + OpenSearch**：可以同时获得权威事务存储和专业混合检索，面向大规模或复杂中文语义检索具潜力；增加同步、删除传播、索引漂移和运维负担。未达到性能/质量门槛前不宜将其设为强制依赖。

**关键风险**：OpenSearch 3.8 的 agentic memory retention 功能仍被官方标注为 *experimental / not recommended for production*；原生记忆容器默认不会自动执行该保留策略。企业级 CMI 不能依赖此能力承担合规删除或保留义务。[OS-RET]

## 2. 研究边界与基线假设

1. IDP CMI 是独立部署的产品，对 HOST、EER、Workspace、MCP Infrastructure 没有强制运行时依赖。它们只是可选消费者或集成系统。
2. 记忆对象包括用户偏好、Agent 工作摘要、项目决议、企业知识片段、来源与证据；**订单状态、库存、财务余额等实时业务事实以源业务系统为准**。
3. `tenant_id`、`project_id`、`principal_id`、权限版本、来源、证据引用、有效时间、状态、版本应属于 CMI 自己定义的逻辑契约。
4. 测试环境尚未提供真实企业语料、流量、SLA 和部署拓扑；所有技术评分和建议为设计判断，**没有任何数据库吞吐、召回或延迟数据被实际测出**。

## 3. 已核实的官方能力

### 3.1 PostgreSQL + pgvector

- PostgreSQL 16 提供全文搜索以及 GIN / GiST 索引、事务、约束和行级安全策略；RLS 未定义适用策略时的默认行为为拒绝访问。[PG-FTS][PG-RLS]
- pgvector 官方 README 提供精确向量检索及近似 HNSW、IVFFlat 索引。0.8.0 起加入 iterative index scans；过滤选择率较低时，近似索引可能在后过滤阶段返回不足的候选，需要通过迭代扫描、分区或局部索引等方案处理。[PGV]
- pgvector 文档明确指出：多个租户共享一个 ANN 近似索引时，会对彼此的召回和性能造成影响；建议按租户分区或分表。注意：**这主要是检索隔离和质量问题，不等同于必然的数据访问泄露**，权限仍需在数据库与服务层单独保证。[PGV]

### 3.2 OpenSearch 检索与 Agentic Memory

- OpenSearch 原生支持 BM25 全文搜索、向量相似度搜索、混合检索及可配置搜索 pipeline。混合搜索可使用分数归一化或 RRF 进行融合，不能未经数据验证就假设一定优于 PostgreSQL 的自实现融合。[OS-HYB]
- OpenSearch Agentic Memory API 于 3.3 引入；可创建容器、添加/获取/更新/删除记忆和会话、执行语义及混合记忆搜索。[OS-API]
- 原生记忆容器有 `working`、`sessions`、`long-term`、`history` 四类记录；配置 LLM、embedding 模型和策略后，才支持长期记忆提取、整合决策与 history。省去这几项配置时，容器主要是原始消息存储。[OS-CONTAINER]
- 官方处理策略包括 `SEMANTIC`、`USER_PREFERENCE`、`SUMMARY`；推理需要注册模型，`infer=false` 时只存原始工作记忆。此处的 ADD/UPDATE/DELETE 决策是 **LLM 辅助语义处理能力**，并不天然满足企业事实权威性或冲突证明要求。[OS-MEM][OS-CONTAINER]
- OpenSearch 3.6 提供长期记忆 BM25+向量的 Hybrid Search Agentic Memory API，允许 namespace、tags、filter 和权重设置。[OS-HYB-MEM]
- OpenSearch 的高效 k-NN 过滤策略与引擎有关；不能将普通 post-filter 等同于检索过程中的高效过滤，尤其当过滤选择率极低时。[OS-FILTER]
- OpenSearch DLS 约束读取但 **不限制写操作**。一旦赋予索引写权限，用户可能修改自己无法通过 DLS 读到的文档。必须通过服务端授权、受限服务身份、角色/索引设计来保护写入路径。[OS-DLS]
- OpenSearch 的搜索是近实时的，写入/更新/删除后的搜索可见性受到 refresh 设置影响；在 CMI 中应当设计版本栅栏、授权重校验与删除传播验证。[OS-NRT]
- OpenSearch 3.8 的 retention 特性当前处于实验阶段，默认关闭；默认长期积累记忆可能带来成本及过期内容检索风险，不应直接承担正式数据保留与删除承诺。[OS-RET]

## 4. 三路线比较

| 评估维度 | A PG16 + pgvector | B OS Agentic Memory | C PG + OS |
|---|---|---|---|
| 记忆真相源及版本事务 | **强**；单一事务域 | 需自行证明/补足业务语义 | **强**；PG 为真相源 |
| 原生自动提取/摘要/整合 | **需开发** | **已有基础 API**；需验证可靠性 | 可选自研或调用 OS |
| 复杂全文与向量融合 | 可实现，需调参 | **成熟的检索工具链** | **强**；以 OS 为候选后端 |
| 企业多租户权限 | PG RLS + 服务鉴权 | DLS/索引权限 + 服务鉴权；写权限注意 | 双边校验与索引隔离，最复杂 |
| 写入到可检索一致性 | 同数据库较易控制 | 受 refresh / 引擎处理影响 | 需要同步、重放、版本栅栏 |
| 删除与合规可证明性 | 比较容易实现审计与状态机 | 必须验证 API、索引及保留策略 | 必须跨库传播、验证残留 |
| 运维与部署 | **最少组件** | 搜索集群及 ML/模型配置 | **最多组件** |
| 后端替换与成本控制 | 简单 | 防止 OpenSearch Memory API 契约锁定 | 可保持适配层抽象 |
| 当前证据完备性 | 官方文档，尚未实测 | 官方文档，尚未实测 | 架构上可行，尚未实测 |

**定性排序（设计判断，非基准测试）**：开发便利性 B ≥ A > C（若仅看原生 Agentic Memory 功能）；治理可控性 A > C > B；全文/混合检索工具完整度 B ≈ C > A；部署简洁性 A > B > C。上述排序会因团队技能、数据分布和运维基础改变。

## 5. 最关键的架构约束

### 5.1 CMI 统一契约不得绑定 OpenSearch

建议 `MemoryService` 提供如下逻辑操作，而不是对外暴露供应商原始 `memory_container_id`：

- `write_memory`：身份、来源、作用域、授权、幂等、事实类型、预期版本。
- `supersede_memory` / `invalidate_memory`：变更因果、有效区间、旧版保留策略。
- `search_memories`：请求主体、任务意图、时间点、预算和检索类型。
- `explain_memory`：原始来源、事实声明者、提取流程、版本及得分原因。
- `delete_memory`：逻辑删除、队列传播、检索隔离、生效证明与审计。

数据库和搜索引擎只承担相应存储、索引和处理职责；CMI 掌握契约、主权、权限和质量策略。

### 5.2 路线 C 的一致性策略

1. PostgreSQL 事务写入记忆版本和 outbox 事件；禁止应用端分别直接双写 PostgreSQL/OpenSearch。
2. 同步消费者读取 outbox/CDC，按 `(memory_id, version)` 幂等 Upsert 或 Tombstone 到搜索索引。
3. 检索时先按 CMI 授权范围做查询约束，返回命中 ID 后对高敏感数据再次验证授权与最新 `status/version`。
4. 删除时 PostgreSQL 立即生效 deny/tombstone；后台删除 OpenSearch 索引并验证；删除尚未传播时查询不能返回明文。
5. 提供全量重建与校验：`source_count`、`index_count`、版本漂移、滞后窗口、失败重试、死信处理。

### 5.3 记忆类型与权威性

正式 CMI 不应把 LLM 摘要视为业务事实：

| 类型 | 例子 | 验证方式 |
|---|---|---|
| 用户声明 | “EER 已开发完成” | 记录 `reported_by_user`；不冒充验收事实 |
| 产品决议 | “MCP 必须独立于 EER” | 与 ADR/决策记录关联；保留有效区间 |
| 系统事实 | “包裹已签收” | 授权访问 TMS 权威记录，带时间戳 |
| 推断性知识 | “该客户偏好周一发货” | 标识为 hypothesis，不覆盖明确声明 |

## 6. 可重复 PoC / Benchmark 设计

### 6.1 试验控制变量

- 在同一宿主硬件配额下隔离部署 A/B/C；使用**同一份权威语料、同一 embedding 模型、同一 query 集、同一任务定义**。
- 自然语言数据采用中文/英文混合；业务 ID、货运单号、订单编号、跨字段过滤与中文分词需专门测试。
- 三档数据量建议起点：10 万、100 万、1000 万条记忆/知识片段；如果机器容量不足，记录上限而非伪造 1000 万结果。
- 三档查询选择率：0.1%、1%、10%，另测高并发多租户分布及冷热缓存。
- 测试原生提取质量时，A 的提取/整合模型应与 B 的 OpenSearch 外接模型保持可比，单独统计 token/成本。

### 6.2 标注集

至少覆盖六种任务（每类初始建议 100–200 个经过人工审定的测试问题）：

1. 精确标识：SKU、渠道、运单号、合同编号；
2. 语义改写：跨说法、同义词、中文业务缩写；
3. 时效冲突：旧状态被新状态覆盖、历史时点查询；
4. 多租户/权限：完全禁止越权返回，即使语义高度相关；
5. 删除与失效：删除后即时阻断、索引同步滞后、重建后不复活；
6. 证据溯源：正确指向来源文档、时间与已认证事实类型。

### 6.3 指标与门槛

| 指标 | 度量方法 | 准入建议 |
|---|---|---|
| Recall@10 / nDCG@10 / MRR | 人工标注相关性，分任务维度 | 比较 A/B/C，不凭单个均值定夺 |
| 精确 ID Hit@1 | 业务主键精确问题 | 单列报告，禁止向量语义吞没精确匹配 |
| 检索 P50/P95/P99 | 并发压测，分冷热缓存/过滤率 | SLA 应按 IDP 具体部署目标设定 |
| 索引可见性延迟 | 从 PG 提交或 Memory API ACK 到可检索 | 报告分布及最坏尾部；路径定义一致 |
| 删除隔离窗口 | 删除确认到所有 API 不再暴露明文 | 对外查询准入必须即时阻断 |
| 跨租户越权 | 构造对抗性主体与过滤条件 | **0 次成功越权（阻断门槛）** |
| 溯源正确率 | 对人工来源标签 | 纳入正式准入指标 |
| 恢复与重放 | OS/PG 故障、重复事件、乱序事件 | 保持幂等且无已删除记忆复活 |
| 资源成本 | CPU、内存、磁盘、索引时间、LLM token | 标准化为每百万记忆与每千次查询 |

**不应简单用 `0 次越权` 就宣称绝对安全。** 它只是有限测试集的准入门槛，仍需威胁建模、代码审查、渗透及生产监测。

### 6.4 实施顺序

1. **P0**：统一数据 schema、权限语义、检索结果格式和标注集；先做 A 的 reference 实现。
2. **P1**：部署 OpenSearch Agentic Memory，针对提取、去重、更新、混合检索和删除逐条核对 API 行为。
3. **P2**：实现 C 的 outbox/CDC 同步和索引重建，对比检索效果与运维代价。
4. **P3**：在相同数据与压力下跑三套基准，附环境清单、运行日志、命令、原始 JSON/CSV 指标。
5. **Decision Gate**：仅当 B/C 显著改善核心指标且满足治理与一致性门槛时，将 OS 设为正式可选后端或默认配置。

## 7. 当前决议状态

| 项 | 状态 |
|---|---|
| CMI 独立于 EER/HOST/MCP | **沿用既有架构基线** |
| PG 作为权威记忆记录 | **建议，待架构确认** |
| pgvector 第一检索后端 | **建议，待运行验收** |
| OpenSearch Search Adapter | **纳入 PoC 候选** |
| OpenSearch Agentic Memory | **纳入功能 PoC；不代替治理层** |
| CMI V1.0 正式生产后端 | **未锁定** |
| 真实性能测试 | **未执行** |

## 8. 第一手参考资料

- [PG-FTS] PostgreSQL 16, Full Text Search / Indexes: https://www.postgresql.org/docs/16/textsearch-indexes.html
- [PG-RLS] PostgreSQL 16, CREATE POLICY: https://www.postgresql.org/docs/16/sql-createpolicy.html
- [PGV] pgvector 官方 README: https://github.com/pgvector/pgvector
- [OS-HYB] OpenSearch, Hybrid Search: https://docs.opensearch.org/latest/vector-search/ai-search/hybrid-search/index/
- [OS-FILTER] OpenSearch, Filtering Vector Search Results: https://docs.opensearch.org/latest/vector-search/filter-search-knn/index/
- [OS-API] OpenSearch, Agentic Memory APIs: https://docs.opensearch.org/latest/ml-commons-plugin/api/agentic-memory-apis/index/
- [OS-CONTAINER] OpenSearch, Create Memory Container API: https://docs.opensearch.org/latest/ml-commons-plugin/api/agentic-memory-apis/create-memory-container/
- [OS-MEM] OpenSearch, Agentic Memory: https://docs.opensearch.org/latest/ml-commons-plugin/agentic-memory/
- [OS-HYB-MEM] OpenSearch, Hybrid Search Agentic Memory API: https://docs.opensearch.org/latest/ml-commons-plugin/api/agentic-memory-apis/hybrid-search-memory/
- [OS-DLS] OpenSearch, Document-Level Security: https://docs.opensearch.org/latest/security/access-control/document-level-security/
- [OS-NRT] OpenSearch, Document APIs (Refresh): https://docs.opensearch.org/latest/api-reference/document-apis/index/
- [OS-RET] OpenSearch, Agentic Memory Retention: https://docs.opensearch.org/latest/ml-commons-plugin/agentic-memory-retention/

> 本报告所有关于性能、实施难易、运维复杂度的比较属于工程判断，只有官方文档能力已得到文档级核实。后续必须将“官方支持”与“IDP 环境完成测试”单独标记。
