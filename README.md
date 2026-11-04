# idp-cmi

IDP TELOS 的 Context, Memory & Knowledge Infrastructure（CMI）：可信的企业上下文——谁在什么时候从哪里得到了什么，它还有效吗，谁可以看。设计见 `docs/adr/ADR-0001-cmi-design.md`。

状态：**M1 完成并实测**：来源登记（信任级别）、记忆记录（作用域、来历、有效期、追加与撤销）、知识文档（版本取代、chunk 全文索引含 CJK bigram）、上下文包（检索 → 过滤 → 信任 / 新鲜度 / 相关度排序 → 预算装包 → 密封、内容寻址、可读回并报告失效项）、撤销传播与到期扫描、租户级敏感上限策略、RLS、审计、指标、追踪、TLS；契约 `idp.telos.cmi` 1.0.0 已发布。

| 接口 | 角色 | 内容 |
|---|---|---|
| `/api/v1/sources`、`/api/v1/knowledge/documents`、`/api/v1/policy`、`/api/v1/audit` | `cmi-admin` | 来源、知识文档（索引、版本、撤回）、租户策略、审计 |
| `/v1/memory/records`、`/v1/memory/revoke` | `cmi-writer` | 追加记忆（必须带来历与已登记的来源；敏感级别受写者上限约束）、撤销 |
| `/v1/context/resolve`、`/v1/context/packages/{id}` | `cmi-reader` | 解析上下文包、读回（含 `stale` 与失效条目） |

```sh
make test               # gofmt、vet、单元与契约测试（-race）
make test-integration   # 真实 PostgreSQL 上的集成测试（Docker 起一个临时实例）
bash deploy/build.sh    # 离线构建 scratch 镜像
```

运行：`idp-cmi migrate`（owner 连接）→ `idp-cmi ensure-login idp_cmi_svc` → `idp-cmi seed-tenant acme` → `idp-cmi serve`（环境变量见 `cmd/idp-cmi/main.go` 的注释）。开发模式下 `idp-cmi dev-token TENANT SUBJECT ROLE...` 签发 HS256 令牌。

与 HOST 的联调（HOST 作为 `cmi-reader` 解析上下文并记录 `contextPackageId`）在 HOST 侧实现，见 HOST ADR-0001 的后续章节。

## 许可证

本仓库采用 [PolyForm Noncommercial License 1.0.0](LICENSE)：允许非商业用途，禁止任何商业使用。
