// SPDX-License-Identifier: AGPL-3.0-only

package ruler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/crypto/tls"
	"github.com/grafana/mimir/pkg/tenant"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	otgrpc "github.com/opentracing-contrib/go-grpc"
	"github.com/opentracing/opentracing-go"
	"github.com/prometheus/common/model"
	prommodel "github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/rules"
	"github.com/weaveworks/common/httpgrpc"
	"github.com/weaveworks/common/middleware"
	"github.com/weaveworks/common/user"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

const (
	queryEndpointPath = "/api/v1/query"

	keepAlive        = time.Second * 10
	keepAliveTimeout = time.Second * 5

	mimeTypeFormPost = "application/x-www-form-urlencoded"

	statusSuccess = "success"
)

type apiResponse struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data"`
	ErrorType string          `json:"errorType"`
	Error     string          `json:"error"`
}

type RemoteQuerier interface {
	Query(ctx context.Context, query string, ts time.Time) (model.Value, error)
}

type RemoteQuerierConfig struct {
	Address    string           `yaml:"address"`
	TLSEnabled bool             `yaml:"tls_enabled" category:"advanced"`
	TLS        tls.ClientConfig `yaml:",inline"`
}

func (c *RemoteQuerierConfig) RegisterFlags(f *flag.FlagSet) {
	f.StringVar(&c.Address,
		"ruler.remote-querier.address",
		"dns:///:9095",
		"GRPC listen address of the remote querier(s). Must be a DNS address (prefixed with dns:///) "+
			"to enable client side load balancing.")

	f.BoolVar(&c.TLSEnabled, "ruler.remote-querier.tls-enabled", false, "Set to true if remote querier connection requires TLS.")

	c.TLS.RegisterFlagsWithPrefix("ruler.remote-querier", f)
}

type remoteQuerier struct {
	client         httpgrpc.HTTPClient
	conn           *grpc.ClientConn
	promHTTPPrefix string
	logger         log.Logger
}

func NewRemoteQuerier(cfg RemoteQuerierConfig, prometheusHTTPPrefix string, logger log.Logger) (RemoteQuerier, error) {
	tlsDialOptions, err := cfg.TLS.GetGRPCDialOptions(cfg.TLSEnabled)
	if err != nil {
		return nil, err
	}
	dialOptions := append(
		[]grpc.DialOption{
			grpc.WithKeepaliveParams(
				keepalive.ClientParameters{
					Time:                keepAlive,
					Timeout:             keepAliveTimeout,
					PermitWithoutStream: true,
				},
			),
			grpc.WithUnaryInterceptor(
				grpc_middleware.ChainUnaryClient(
					otgrpc.OpenTracingClientInterceptor(opentracing.GlobalTracer()),
					middleware.ClientUserHeaderInterceptor,
				),
			),
			grpc.WithDefaultServiceConfig(`{"loadBalancingPolicy":"round_robin"}`),
		},
		tlsDialOptions...,
	)

	conn, err := grpc.Dial(cfg.Address, dialOptions...)
	if err != nil {
		return nil, err
	}
	return &remoteQuerier{
		client:         httpgrpc.NewHTTPClient(conn),
		conn:           conn,
		promHTTPPrefix: prometheusHTTPPrefix,
		logger:         logger,
	}, nil
}

func (r *remoteQuerier) Query(ctx context.Context, query string, ts time.Time) (model.Value, error) {
	args := url.Values{}
	args.Set("query", query)
	if !ts.IsZero() {
		args.Set("time", formatQueryTime(ts))
	}
	body := []byte(args.Encode())

	headers, err := requestHeaderFromContext(ctx, len(body))
	if err != nil {
		return nil, err
	}
	req := httpgrpc.HTTPRequest{
		Method:  http.MethodPost,
		Url:     r.promHTTPPrefix + queryEndpointPath,
		Body:    body,
		Headers: headers,
	}

	resp, err := r.client.Handle(ctx, &req)
	if err != nil {
		level.Warn(r.logger).Log("msg", "failed to remotely evaluate rule expression", "err", err)
		return nil, err
	}
	if resp != nil && resp.Code/100 != 2 {
		return nil, fmt.Errorf("unexpected %d response status code: %s", resp.Code, string(resp.Body))
	}

	var apiResp apiResponse
	if err := json.NewDecoder(bytes.NewReader(resp.Body)).Decode(&apiResp); err != nil {
		return nil, err
	}
	if apiResp.Status != statusSuccess {
		return nil, fmt.Errorf("unexpected response status '%s'", apiResp.Status)
	}
	return decodeQueryResponse(apiResp.Data)
}

func requestHeaderFromContext(ctx context.Context, contentLength int) ([]*httpgrpc.Header, error) {
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
	return []*httpgrpc.Header{
		{
			Key:    textproto.CanonicalMIMEHeaderKey("Content-Type"),
			Values: []string{mimeTypeFormPost},
		},
		{
			Key:    textproto.CanonicalMIMEHeaderKey("Content-Length"),
			Values: []string{strconv.Itoa(contentLength)},
		},
		{
			Key:    textproto.CanonicalMIMEHeaderKey(user.OrgIDHeaderName),
			Values: []string{orgID},
		},
	}, nil
}

func decodeQueryResponse(b []byte) (model.Value, error) {
	v := struct {
		Type   model.ValueType `json:"resultType"`
		Result json.RawMessage `json:"result"`
	}{}

	err := json.Unmarshal(b, &v)
	if err != nil {
		return nil, err
	}

	switch v.Type {
	case model.ValScalar:
		var sv model.Scalar
		if err = json.Unmarshal(v.Result, &sv); err != nil {
			return nil, err
		}
		return &sv, nil

	case model.ValVector:
		var vv model.Vector
		if err = json.Unmarshal(v.Result, &vv); err != nil {
			return nil, err
		}
		return vv, nil

	default:
		return nil, fmt.Errorf("unexpected value type %q", v.Type)
	}
}

func formatQueryTime(t time.Time) string {
	return strconv.FormatFloat(float64(t.Unix())+float64(t.Nanosecond())/1e9, 'f', -1, 64)
}

// RemoteQueryFunc returns a new query function that remotely executes an instant query passing an altered timestamp.
func RemoteQueryFunc(remoteQuerier RemoteQuerier, overrides RulesLimits, userID string) rules.QueryFunc {
	return func(ctx context.Context, qs string, t time.Time) (promql.Vector, error) {
		evaluationDelay := overrides.EvaluationDelay(userID)

		val, err := remoteQuerier.Query(ctx, qs, t.Add(-evaluationDelay))
		if err != nil {
			return nil, err
		}
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
