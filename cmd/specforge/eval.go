package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/specforge/specforge/internal/eval"
)

// defaultMaxHallucination --max-hallucination 默认值（设计文档 §1.6 幻觉率 < 1%）。
const defaultMaxHallucination = 0.01

// evalResult eval 的机器模式输出：指标 + 闸门结论。
type evalResult struct {
	Metrics   *eval.Metrics `json:"metrics"`             // 全部指标与漏报/误报明细
	Gate      *float64      `json:"gate,omitempty"`      // --fail-under 阈值（未设置时省略）
	GatePass  *bool         `json:"gate_pass,omitempty"` // 闸门是否通过（未设置时省略）
	GateFails []string      `json:"gate_failures"`       // 未达标的指标（name=value）
}

// newEvalCmd eval：生成 spec 对照 ground truth 打分；--fail-under 作为 CI 质量闸门。
func newEvalCmd(a *app) *cobra.Command {
	var truth, spec string
	var failUnder, maxHalluc float64
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Score a generated spec against a ground-truth spec",
		Long: `Compare a generated OpenAPI spec with a ground-truth spec (hand-written or exported from a
trusted source, e.g. swag) and report accuracy metrics with the exact misses and extras.

Metrics:
  route_recall / route_precision   operations (METHOD path) found / not invented
  param_f1                         parameters matched on (in, name, required, type)
  req_field_f1                     request body fields, nested fields flattened to a.b[].c paths
  resp_field_f1                    response data fields including 'required' (resp_shape_f1 ignores it)
  envelope_recall                  business error codes / status codes found
  hallucination                    share of generated fields with no counterpart in the truth

--fail-under turns eval into a CI gate: exit 40 if route_recall, param_f1, req_field_f1,
resp_field_f1 or envelope_recall is below the threshold, or hallucination exceeds
--max-hallucination. Metrics are always printed, also when the gate fails.`,
		Example: `  specforge eval --truth api/truth.yaml --spec .specforge/out/openapi.yaml
  specforge eval --truth api/truth.yaml --spec .specforge/out/openapi.yaml --fail-under 0.9
  specforge eval --truth t.yaml --spec s.yaml --json | jq '.data.metrics.field_misses'`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if truth == "" || spec == "" {
				return newErr(exitUsage, codeInvalidArgs, "--truth and --spec are required", "see 'specforge eval --help'")
			}
			m, err := eval.Evaluate(truth, spec)
			if err != nil {
				return newErr(exitConfig, codeConfigInvalid, err.Error(), "check that both files exist and are OpenAPI YAML")
			}
			res := &evalResult{Metrics: m, GateFails: []string{}}
			if failUnder > 0 {
				res.Gate = &failUnder
				res.GateFails = gateFailures(m, failUnder, maxHalluc)
				pass := len(res.GateFails) == 0
				res.GatePass = &pass
			}
			a.emit(res, func(w io.Writer) { _ = eval.WriteReport(m, w) })
			if len(res.GateFails) > 0 {
				return newErr(exitGateFailed, codeGateFailed, "quality gate failed: "+strings.Join(res.GateFails, ", "), "")
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&truth, "truth", "", "ground-truth OpenAPI YAML (required)")
	f.StringVar(&spec, "spec", "", "generated OpenAPI YAML to score (required)")
	f.Float64Var(&failUnder, "fail-under", 0, "CI gate: exit 40 if any recall/F1 metric is below this value, 0..1 (0 = no gate)")
	f.Float64Var(&maxHalluc, "max-hallucination", defaultMaxHallucination, "CI gate (with --fail-under): maximum allowed hallucination rate, 0..1")
	return cmd
}

// gateFailures 未达标的指标（name=value，顺序固定）。
func gateFailures(m *eval.Metrics, failUnder, maxHalluc float64) []string {
	failed := []string{}
	for _, c := range []struct {
		name string
		v    float64
	}{
		{"route_recall", m.RouteRecall}, {"param_f1", m.ParamF1}, {"req_field_f1", m.ReqFieldF1},
		{"resp_field_f1", m.RespFieldF1}, {"envelope_recall", m.EnvelopeRecall},
	} {
		if c.v < failUnder {
			failed = append(failed, fmt.Sprintf("%s=%.3f", c.name, c.v))
		}
	}
	if m.HallucRate > maxHalluc {
		failed = append(failed, fmt.Sprintf("hallucination=%.3f", m.HallucRate))
	}
	return failed
}
