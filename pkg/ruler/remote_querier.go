// SPDX-License-Identifier: AGPL-3.0-only

package ruler

import (
	"context"
	"flag"
	"net/http"
	"time"

	kitlog "github.com/go-kit/log"
	"github.com/grafana/mimir/pkg/tenant"
	"github.com/grafana/mimir/pkg/util/spanlogger"
	"github.com/pkg/errors"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	prommodel "github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/rules"
	"github.com/weaveworks/common/user"
)

type RemoteQuerierConfig struct {
	Enabled bool   `yaml:"enabled"`
	Address string `yaml:"address"`
}

func (cfg *RemoteQuerierConfig) RegisterFlags(f *flag.FlagSet) {
	f.BoolVar(&cfg.Enabled, "ruler.remote-querier.enabled", false, "Enable running rule groups evaluation against a remote querier.")
	f.StringVar(&cfg.Address, "ruler.remote-querier.address", "", "The remote querier address of the Prometheus to connect to.")
}

type OrgIDRoundTripper struct {
	Next http.RoundTripper
}

func (r *OrgIDRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()

	var orgID string
	if sourceTenants, _ := ctx.Value(federatedGroupSourceTenants).([]string); len(sourceTenants) > 0 {
		orgID = tenant.JoinTenantIDs(sourceTenants)
	} else {
		var err error
		orgID, err = user.ExtractOrgID(ctx)
		if err != nil {
			return nil, err
		}
	}
	req.Header.Set("X-Scope-OrgID", orgID)

	return r.Next.RoundTrip(req)
}

// RemoteQueryFunc returns a new query function that remotely executes an instant query passing an altered timestamp.
func RemoteQueryFunc(queryableClient promv1.API, overrides RulesLimits, userID string, logger kitlog.Logger) rules.QueryFunc {
	return func(ctx context.Context, qs string, t time.Time) (promql.Vector, error) {
		logger, ctx := spanlogger.NewWithLogger(ctx, logger, "RemoteQueryFunc")
		defer logger.Span.Finish()

		evaluationDelay := overrides.EvaluationDelay(userID)

		val, _, err := queryableClient.Query(ctx, qs, t.Add(-evaluationDelay))
		if err != nil {
			return nil, err
		}
		logger.Log("msg", "rule expression successfully evaluated", "qs", qs, "time", t.String())

		switch v := val.(type) {
		case prommodel.Vector:
			return vectorToPromQLVector(v), nil

		case *prommodel.Scalar:
			return scalarToPromQLVector(v), nil

		default:
			return nil, errors.New("rule result is not a vector or scalar")
		}
	}
}

func vectorToPromQLVector(vec prommodel.Vector) promql.Vector {
	var retVal promql.Vector
	for _, p := range vec {
		var sm promql.Sample

		sm.V = float64(p.Value)
		sm.T = int64(p.Timestamp)

		var lbl labels.Labels
		for ln, lv := range p.Metric {
			lbl = append(lbl, labels.Label{Name: string(ln), Value: string(lv)})
		}
		sm.Metric = lbl

		retVal = append(retVal, sm)
	}
	return retVal
}

func scalarToPromQLVector(sc *prommodel.Scalar) promql.Vector {
	return promql.Vector{promql.Sample{
		Point: promql.Point{
			V: float64(sc.Value),
			T: int64(sc.Timestamp),
		},
		Metric: labels.Labels{},
	}}
}
