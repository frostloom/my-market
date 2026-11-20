# tools/legacy-ops/

R2.7 服务拆分时原样迁入的旧仓编排/实验脚本（start-all/stop-all、run-*-experiment.ps1、
compare-intent.ps1、load-env.ps1 与 m2/m3/m5 scenario.sh）。它们内含 `go build ./cmd/...`、
`cd ../../` 等旧布局假设，路径语义已失效——仅作历史存档，**勿直接执行**；如需复用请先按
services/<svc> 新布局改写（裁定 F，见 docs/plans/r2.7-mapping.md §9.6/§11-F）。
