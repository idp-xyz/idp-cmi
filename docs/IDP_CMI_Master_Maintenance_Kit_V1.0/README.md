# IDP CMI 统一入口维护包

本包用于维护已交付的统一 HTML 文档。正文唯一源为 `CMI_Master.md`，请不要同时手工编辑生成后的 HTML。`evidence/` 是 2026-10-16 原型的历史机器可读契约快照，不是当前生产接口。

## 重新生成 HTML

需要 Node.js 18 或更高版本。包内已包含 Marked 解析器，因此生成时无需联网安装依赖。

```bash
node build.mjs
```

输出 `IDP_CMI_Consolidated_Master_V1.0.html`。也可以在命令后指定输出路径。HTML 内嵌字体、样式、图示与脚本，浏览正文不需要联网；历史资料和外部参考链接需要联网。

若增加正文或界面中未包含的新字符，为保持嵌入字体完整，请在有 Python 3 和 FontTools 的环境中运行 `python3 update_font.py`，再执行构建。未更新子集时浏览器会尝试使用操作系统本地字体作为回退。

## 变更检查

- 确认各项目方向、目标设计、历史快照、验证记录和当前部署的状态分别清楚；保留证据时间、版本和来源链接。
- 涉及 API、授权和测试时，以对应已批准契约、代码提交和原始证据为准，不能从此 HTML 推断生产上线。
- 检查所有历史资料链接、表格、图示和目录；生成之后进行浏览器和打印效果检查。
- 如涉及重要架构决定，在组织内完成相应审阅、批准与影响范围更新。

## 字体与解析器

`assets/DroidSansFallback.ttf` 为 Droid Sans Fallback Regular，Apache License 2.0，许可证见 `assets/DroidSansFallback-LICENSE.txt`。`vendor/marked/` 含 Marked 17.0.5 的 MIT 许可证和用于离线构建的 ESM 文件。其他原型契约属于原历史项目资料，保留原始用途与权属。
