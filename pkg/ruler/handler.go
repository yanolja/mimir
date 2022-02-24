// SPDX-License-Identifier: AGPL-3.0-only

package ruler

import (
	"context"
	"errors"
	"github.com/go-kit/log/level"

	"github.com/go-kit/log"
	"github.com/grafana/mimir/pkg/mimirpb"
	"github.com/grafana/mimir/pkg/util"
	"github.com/grafana/mimir/pkg/util/spanlogger"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/storage"

	"github.com/grafana/mimir/pkg/frontend/querymiddleware"
)

const (
	// statusSuccess Prometheus success result.
	statusSuccess = "success"
)

type ruleEvalHandler struct {
	eng    *promql.Engine
	q      storage.Queryable
	logger log.Logger
}

func newRuleEvalHandler(eng *promql.Engine, q storage.Queryable, logger log.Logger) *ruleEvalHandler {
	return &ruleEvalHandler{
		eng:    eng,
		q:      q,
		logger: logger,
	}
}

func (r *ruleEvalHandler) Do(ctx context.Context, req querymiddleware.Request) (querymiddleware.Response, error) {
	logger, ctx := spanlogger.NewWithLogger(ctx, r.logger, "ruleEvalHandler.Do")
	defer logger.Span.Finish()

	q, err := r.eng.NewInstantQuery(r.q, req.GetQuery(), util.TimeFromMillis(req.GetStart()))
	if err != nil {
		return nil, err
	}
	res := q.Exec(ctx)
	if res.Err != nil {
		return nil, res.Err
	}
	var retVal promql.Vector

	switch v := res.Value.(type) {
	case promql.Vector:
		retVal = v
	case promql.Scalar:
		retVal = promql.Vector{
			promql.Sample{
				Point:  promql.Point(v),
				Metric: labels.Labels{},
			}}
	default:
		return nil, errors.New("rule result is not a vector or scalar")
	}

	level.Debug(logger).Log("msg", "successfully evaluated expression query", "query", req.GetQuery(), "time", req.GetStart())

	return &querymiddleware.PrometheusResponse{
		Status: statusSuccess,
		Data: &querymiddleware.PrometheusData{
			ResultType: model.ValVector.String(),
			Result:     vecToPrometheusData(retVal),
		},
	}, nil
}

func vecToPrometheusData(v promql.Vector) []querymiddleware.SampleStream {
	res := make([]querymiddleware.SampleStream, 0, len(v))
	for _, sample := range v {
		res = append(res, querymiddleware.SampleStream{
			Labels:  mimirpb.FromLabelsToLabelAdapters(sample.Metric),
			Samples: []mimirpb.Sample{{TimestampMs: sample.Point.T, Value: sample.Point.V}}})
	}
	return res
}
