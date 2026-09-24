# SpecForge v2.0 完整交付包

Agent 原生的 OpenAPI 生成系统 —— 基于设计文档实现的完整工程（P0+P1 范围）。

## 包内容

| 路径 | 说明 |
|---|---|
| `SpecForge-v2.0-设计文档.md` | v2.0 完整设计文档（2.1 万字，20 章 + 3 附录） |
| `specforge/` | 完整 Go 工程（含 git 历史、单元测试、端到端回归守卫） |
| `specforge/testdata/sample-repo/` | 样本仓库：复刻设计文档全部难点模式 |
| `specforge/testdata/ground-truth.yaml` | 人工标注真值 |
| `specforge/testdata/sample-repo/.specforge/out/` | 已生成的 openapi.yaml + report.md 样例 |

## 快速开始

```bash
cd specforge

# 构建（需要 Go 1.27+）
go build -o specforge ./cmd/specforge

# 对任何 Go + fiber 仓库生成 OpenAPI 3.1
./specforge gen --repo path/to/your/repo

# 带约定画像（信封/中间件语义/通配符规则）
./specforge gen --repo . --profile .specforge/profile.yaml

# 评测（对照 ground truth）
./specforge eval --truth testdata/ground-truth.yaml \
                 --spec testdata/sample-repo/.specforge/out/openapi.yaml

# 运行全部测试（单元 + 端到端精度回归守卫）
go test ./...
```

## 验收指标（已达精度上限）

| 指标 | 结果 | P0 门限 |
|---|---|---|
| Route Recall | 1.000 | ≥ 0.98 |
| Route Precision | 1.000 | — |
| Param F1 | 1.000 | — |
| Request Field F1 | 1.000 | ≥ 0.95 |
| Response Field F1 | 1.000 | ≥ 0.90 |
| Envelope Recall | 1.000 | ≥ 0.85 |
| Hallucination | 0.000 | < 0.01 |

确定性：双编译自检（byte-identical）+ 跨进程两次运行逐字节一致。

## 环境要求

- Go 1.27+（goproxy.cn 代理可加速国内依赖下载）
- 依赖：golang.org/x/tools v0.50、gopkg.in/yaml.v3、gofiber/fiber v2.52.5（样本仓库用）

## 详细文档

工程结构与设计原则的落地位置见 `specforge/README.md`；
完整设计（Fact 生命周期、增量引擎、LLM 编排、成本模型）见设计文档。
