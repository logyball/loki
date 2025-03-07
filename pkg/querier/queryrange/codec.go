package queryrange

import (
	"bytes"
	"container/heap"
	"context"
	"errors"
	"fmt"
	io "io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	strings "strings"
	"time"

	"github.com/grafana/loki/pkg/storage/stores/index/seriesvolume"

	"github.com/grafana/dskit/httpgrpc"
	"github.com/grafana/dskit/user"
	json "github.com/json-iterator/go"
	"github.com/opentracing/opentracing-go"
	otlog "github.com/opentracing/opentracing-go/log"
	"github.com/prometheus/prometheus/model/timestamp"

	"github.com/grafana/loki/pkg/loghttp"
	"github.com/grafana/loki/pkg/logproto"
	"github.com/grafana/loki/pkg/logql"
	"github.com/grafana/loki/pkg/logql/syntax"
	"github.com/grafana/loki/pkg/logqlmodel"
	"github.com/grafana/loki/pkg/logqlmodel/stats"
	"github.com/grafana/loki/pkg/querier/queryrange/queryrangebase"
	indexStats "github.com/grafana/loki/pkg/storage/stores/index/stats"
	"github.com/grafana/loki/pkg/util"
	"github.com/grafana/loki/pkg/util/httpreq"
	"github.com/grafana/loki/pkg/util/marshal"
	marshal_legacy "github.com/grafana/loki/pkg/util/marshal/legacy"
	"github.com/grafana/loki/pkg/util/querylimits"
)

var DefaultCodec = &Codec{}

type Codec struct{}

type RequestProtobufCodec struct {
	Codec
}

func (r *LokiRequest) GetEnd() time.Time {
	return r.EndTs
}

func (r *LokiRequest) GetStart() time.Time {
	return r.StartTs
}

func (r *LokiRequest) WithStartEnd(s time.Time, e time.Time) queryrangebase.Request {
	clone := *r
	clone.StartTs = s
	clone.EndTs = e
	return &clone
}

func (r *LokiRequest) WithStartEndTime(s time.Time, e time.Time) *LokiRequest {
	clone := *r
	clone.StartTs = s
	clone.EndTs = e
	return &clone
}

func (r *LokiRequest) WithQuery(query string) queryrangebase.Request {
	clone := *r
	clone.Query = query
	return &clone
}

func (r *LokiRequest) WithShards(shards logql.Shards) *LokiRequest {
	clone := *r
	clone.Shards = shards.Encode()
	return &clone
}

func (r *LokiRequest) LogToSpan(sp opentracing.Span) {
	sp.LogFields(
		otlog.String("query", r.GetQuery()),
		otlog.String("start", timestamp.Time(r.GetStart().UnixNano()).String()),
		otlog.String("end", timestamp.Time(r.GetEnd().UnixNano()).String()),
		otlog.Int64("step (ms)", r.GetStep()),
		otlog.Int64("interval (ms)", r.GetInterval()),
		otlog.Int64("limit", int64(r.GetLimit())),
		otlog.String("direction", r.GetDirection().String()),
		otlog.String("shards", strings.Join(r.GetShards(), ",")),
	)
}

func (*LokiRequest) GetCachingOptions() (res queryrangebase.CachingOptions) { return }

func (r *LokiInstantRequest) GetStep() int64 {
	return 0
}

func (r *LokiInstantRequest) GetEnd() time.Time {
	return r.TimeTs
}

func (r *LokiInstantRequest) GetStart() time.Time {
	return r.TimeTs
}

func (r *LokiInstantRequest) WithStartEnd(s time.Time, _ time.Time) queryrangebase.Request {
	clone := *r
	clone.TimeTs = s
	return &clone
}

func (r *LokiInstantRequest) WithQuery(query string) queryrangebase.Request {
	clone := *r
	clone.Query = query
	return &clone
}

func (r *LokiInstantRequest) WithShards(shards logql.Shards) *LokiInstantRequest {
	clone := *r
	clone.Shards = shards.Encode()
	return &clone
}

func (r *LokiInstantRequest) LogToSpan(sp opentracing.Span) {
	sp.LogFields(
		otlog.String("query", r.GetQuery()),
		otlog.String("ts", timestamp.Time(r.GetStart().UnixMilli()).String()),
		otlog.Int64("limit", int64(r.GetLimit())),
		otlog.String("direction", r.GetDirection().String()),
		otlog.String("shards", strings.Join(r.GetShards(), ",")),
	)
}

func (*LokiInstantRequest) GetCachingOptions() (res queryrangebase.CachingOptions) { return }

func (r *LokiSeriesRequest) GetEnd() time.Time {
	return r.EndTs
}

func (r *LokiSeriesRequest) GetStart() time.Time {
	return r.StartTs
}

func (r *LokiSeriesRequest) WithStartEnd(s, e time.Time) queryrangebase.Request {
	clone := *r
	clone.StartTs = s
	clone.EndTs = e
	return &clone
}

func (r *LokiSeriesRequest) WithQuery(_ string) queryrangebase.Request {
	clone := *r
	return &clone
}

func (r *LokiSeriesRequest) GetQuery() string {
	return ""
}

func (r *LokiSeriesRequest) GetStep() int64 {
	return 0
}

func (r *LokiSeriesRequest) LogToSpan(sp opentracing.Span) {
	sp.LogFields(
		otlog.String("matchers", strings.Join(r.GetMatch(), ",")),
		otlog.String("start", timestamp.Time(r.GetStart().UnixNano()).String()),
		otlog.String("end", timestamp.Time(r.GetEnd().UnixNano()).String()),
		otlog.String("shards", strings.Join(r.GetShards(), ",")),
	)
}

func (*LokiSeriesRequest) GetCachingOptions() (res queryrangebase.CachingOptions) { return }

// In some other world LabelRequest could implement queryrangebase.Request.
type LabelRequest struct {
	path string
	logproto.LabelRequest
}

func NewLabelRequest(start, end time.Time, query, name, path string) *LabelRequest {
	return &LabelRequest{
		LabelRequest: logproto.LabelRequest{
			Start:  &start,
			End:    &end,
			Query:  query,
			Name:   name,
			Values: name != "",
		},
		path: path,
	}
}

func (r *LabelRequest) AsProto() *logproto.LabelRequest {
	return &r.LabelRequest
}

func (r *LabelRequest) GetEnd() time.Time {
	return *r.End
}

func (r *LabelRequest) GetEndTs() time.Time {
	return *r.End
}

func (r *LabelRequest) GetStart() time.Time {
	return *r.Start
}

func (r *LabelRequest) GetStartTs() time.Time {
	return *r.Start
}

func (r *LabelRequest) GetStep() int64 {
	return 0
}

func (r *LabelRequest) WithStartEnd(s, e time.Time) queryrangebase.Request {
	clone := *r
	tmp := s
	clone.Start = &tmp
	tmp = e
	clone.End = &tmp
	return &clone
}

func (r *LabelRequest) WithQuery(query string) queryrangebase.Request {
	clone := *r
	clone.Query = query
	return &clone
}

func (r *LabelRequest) LogToSpan(sp opentracing.Span) {
	sp.LogFields(
		otlog.String("start", timestamp.Time(r.GetStart().UnixNano()).String()),
		otlog.String("end", timestamp.Time(r.GetEnd().UnixNano()).String()),
	)
}

func (r *LabelRequest) Path() string {
	return r.path
}

func (*LabelRequest) GetCachingOptions() (res queryrangebase.CachingOptions) { return }

func (Codec) DecodeRequest(_ context.Context, r *http.Request, _ []string) (queryrangebase.Request, error) {
	if err := r.ParseForm(); err != nil {
		return nil, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
	}

	switch op := getOperation(r.URL.Path); op {
	case QueryRangeOp:
		rangeQuery, err := loghttp.ParseRangeQuery(r)
		if err != nil {
			return nil, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
		}

		return &LokiRequest{
			Query:     rangeQuery.Query,
			Limit:     rangeQuery.Limit,
			Direction: rangeQuery.Direction,
			StartTs:   rangeQuery.Start.UTC(),
			EndTs:     rangeQuery.End.UTC(),
			Step:      rangeQuery.Step.Milliseconds(),
			Interval:  rangeQuery.Interval.Milliseconds(),
			Path:      r.URL.Path,
			Shards:    rangeQuery.Shards,
		}, nil
	case InstantQueryOp:
		req, err := loghttp.ParseInstantQuery(r)
		if err != nil {
			return nil, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
		}
		return &LokiInstantRequest{
			Query:     req.Query,
			Limit:     req.Limit,
			Direction: req.Direction,
			TimeTs:    req.Ts.UTC(),
			Path:      r.URL.Path,
			Shards:    req.Shards,
		}, nil
	case SeriesOp:
		req, err := loghttp.ParseAndValidateSeriesQuery(r)
		if err != nil {
			return nil, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
		}
		return &LokiSeriesRequest{
			Match:   req.Groups,
			StartTs: req.Start.UTC(),
			EndTs:   req.End.UTC(),
			Path:    r.URL.Path,
			Shards:  req.Shards,
		}, nil
	case LabelNamesOp:
		req, err := loghttp.ParseLabelQuery(r)
		if err != nil {
			return nil, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
		}

		return &LabelRequest{
			LabelRequest: *req,
			path:         r.URL.Path,
		}, nil
	case IndexStatsOp:
		req, err := loghttp.ParseIndexStatsQuery(r)
		if err != nil {
			return nil, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
		}
		from, through := util.RoundToMilliseconds(req.Start, req.End)
		return &logproto.IndexStatsRequest{
			From:     from,
			Through:  through,
			Matchers: req.Query,
		}, err
	case VolumeOp:
		req, err := loghttp.ParseVolumeInstantQuery(r)
		if err != nil {
			return nil, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
		}
		from, through := util.RoundToMilliseconds(req.Start, req.End)
		return &logproto.VolumeRequest{
			From:         from,
			Through:      through,
			Matchers:     req.Query,
			Limit:        int32(req.Limit),
			Step:         0,
			TargetLabels: req.TargetLabels,
			AggregateBy:  req.AggregateBy,
		}, err
	case VolumeRangeOp:
		req, err := loghttp.ParseVolumeRangeQuery(r)
		if err != nil {
			return nil, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
		}
		from, through := util.RoundToMilliseconds(req.Start, req.End)
		return &logproto.VolumeRequest{
			From:         from,
			Through:      through,
			Matchers:     req.Query,
			Limit:        int32(req.Limit),
			Step:         req.Step.Milliseconds(),
			TargetLabels: req.TargetLabels,
			AggregateBy:  req.AggregateBy,
		}, err
	default:
		return nil, httpgrpc.Errorf(http.StatusNotFound, fmt.Sprintf("unknown request path: %s", r.URL.Path))
	}
}

// labelNamesRoutes is used to extract the name for querying label values.
var labelNamesRoutes = regexp.MustCompile(`/loki/api/v1/label/(?P<name>[^/]+)/values`)

// DecodeHTTPGrpcRequest decodes an httpgrp.HTTPRequest to queryrangebase.Request.
func (Codec) DecodeHTTPGrpcRequest(ctx context.Context, r *httpgrpc.HTTPRequest) (queryrangebase.Request, context.Context, error) {
	httpReq, err := http.NewRequest(r.Method, r.Url, io.NopCloser(bytes.NewBuffer(r.Body)))
	if err != nil {
		return nil, ctx, httpgrpc.Errorf(http.StatusInternalServerError, err.Error())
	}
	httpReq = httpReq.WithContext(ctx)
	httpReq.RequestURI = r.Url
	httpReq.ContentLength = int64(len(r.Body))

	// Note that the org ID should be injected by the scheduler processor.
	for _, h := range r.Headers {
		httpReq.Header[h.Key] = h.Values
	}

	// If there is not org ID in the context, we try the HTTP request.
	_, err = user.ExtractOrgID(ctx)
	if err != nil {
		_, ctx, err = user.ExtractOrgIDFromHTTPRequest(httpReq)
		if err != nil {
			return nil, nil, err
		}
	}

	// Add query tags
	if queryTags := httpreq.ExtractQueryTagsFromHTTP(httpReq); queryTags != "" {
		ctx = httpreq.InjectQueryTags(ctx, queryTags)
	}

	// Add query metrics
	if queueTimeHeader := httpReq.Header.Get(string(httpreq.QueryQueueTimeHTTPHeader)); queueTimeHeader != "" {
		queueTime, err := time.ParseDuration(queueTimeHeader)
		if err == nil {
			ctx = context.WithValue(ctx, httpreq.QueryQueueTimeHTTPHeader, queueTime)
		}
	}

	// If there is not encoding flags in the context, we try the HTTP request.
	if encFlags := httpreq.ExtractEncodingFlagsFromCtx(ctx); encFlags == nil {
		encFlags = httpreq.ExtractEncodingFlagsFromProto(r)
		if encFlags != nil {
			ctx = httpreq.AddEncodingFlagsToContext(ctx, encFlags)
		}
	}

	if err := httpReq.ParseForm(); err != nil {
		return nil, ctx, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
	}

	switch op := getOperation(httpReq.URL.Path); op {
	case QueryRangeOp:
		req, err := loghttp.ParseRangeQuery(httpReq)
		if err != nil {
			return nil, ctx, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
		}
		return &LokiRequest{
			Query:     req.Query,
			Limit:     req.Limit,
			Direction: req.Direction,
			StartTs:   req.Start.UTC(),
			EndTs:     req.End.UTC(),
			Step:      req.Step.Milliseconds(),
			Interval:  req.Interval.Milliseconds(),
			Path:      r.Url,
			Shards:    req.Shards,
		}, ctx, nil
	case InstantQueryOp:
		req, err := loghttp.ParseInstantQuery(httpReq)
		if err != nil {
			return nil, ctx, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
		}
		return &LokiInstantRequest{
			Query:     req.Query,
			Limit:     req.Limit,
			Direction: req.Direction,
			TimeTs:    req.Ts.UTC(),
			Path:      r.Url,
			Shards:    req.Shards,
		}, ctx, nil
	case SeriesOp:
		req, err := loghttp.ParseAndValidateSeriesQuery(httpReq)
		if err != nil {
			return nil, ctx, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
		}
		return &LokiSeriesRequest{
			Match:   req.Groups,
			StartTs: req.Start.UTC(),
			EndTs:   req.End.UTC(),
			Path:    r.Url,
			Shards:  req.Shards,
		}, ctx, nil
	case LabelNamesOp:
		req, err := loghttp.ParseLabelQuery(httpReq)
		if err != nil {
			return nil, ctx, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
		}

		if req.Name == "" {
			if match := labelNamesRoutes.FindSubmatch([]byte(httpReq.URL.Path)); len(match) > 1 {
				req.Name = string(match[1])
				req.Values = true
			}
		}

		return &LabelRequest{
			LabelRequest: *req,
			path:         httpReq.URL.Path,
		}, ctx, nil
	case IndexStatsOp:
		req, err := loghttp.ParseIndexStatsQuery(httpReq)
		if err != nil {
			return nil, ctx, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
		}
		from, through := util.RoundToMilliseconds(req.Start, req.End)
		return &logproto.IndexStatsRequest{
			From:     from,
			Through:  through,
			Matchers: req.Query,
		}, ctx, err
	case VolumeOp:
		req, err := loghttp.ParseVolumeInstantQuery(httpReq)
		if err != nil {
			return nil, ctx, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
		}
		from, through := util.RoundToMilliseconds(req.Start, req.End)
		return &logproto.VolumeRequest{
			From:         from,
			Through:      through,
			Matchers:     req.Query,
			Limit:        int32(req.Limit),
			Step:         0,
			TargetLabels: req.TargetLabels,
			AggregateBy:  req.AggregateBy,
		}, ctx, err
	case VolumeRangeOp:
		req, err := loghttp.ParseVolumeRangeQuery(httpReq)
		if err != nil {
			return nil, ctx, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
		}
		from, through := util.RoundToMilliseconds(req.Start, req.End)
		return &logproto.VolumeRequest{
			From:         from,
			Through:      through,
			Matchers:     req.Query,
			Limit:        int32(req.Limit),
			Step:         req.Step.Milliseconds(),
			TargetLabels: req.TargetLabels,
			AggregateBy:  req.AggregateBy,
		}, ctx, err
	default:
		return nil, ctx, httpgrpc.Errorf(http.StatusBadRequest, fmt.Sprintf("unknown request path: %s", r.Url))
	}
}

// DecodeHTTPGrpcResponse decodes an httpgrp.HTTPResponse to queryrangebase.Response.
func (Codec) DecodeHTTPGrpcResponse(r *httpgrpc.HTTPResponse, req queryrangebase.Request) (queryrangebase.Response, error) {
	if r.Code/100 != 2 {
		return nil, httpgrpc.Errorf(int(r.Code), string(r.Body))
	}

	headers := make(http.Header)
	for _, header := range r.Headers {
		headers[header.Key] = header.Values
	}
	return decodeResponseJSONFrom(r.Body, req, headers)
}

func (Codec) EncodeHTTPGrpcResponse(_ context.Context, req *httpgrpc.HTTPRequest, res queryrangebase.Response) (*httpgrpc.HTTPResponse, error) {
	version := loghttp.GetVersion(req.Url)
	var buf bytes.Buffer

	encodingFlags := httpreq.ExtractEncodingFlagsFromProto(req)

	err := encodeResponseJSONTo(version, res, &buf, encodingFlags)
	if err != nil {
		return nil, err
	}

	httpRes := &httpgrpc.HTTPResponse{
		Code: int32(http.StatusOK),
		Body: buf.Bytes(),
		Headers: []*httpgrpc.Header{
			{Key: "Content-Type", Values: []string{"application/json; charset=UTF-8"}},
		},
	}

	for _, h := range res.GetHeaders() {
		httpRes.Headers = append(httpRes.Headers, &httpgrpc.Header{Key: h.Name, Values: h.Values})
	}

	return httpRes, nil
}

func (c Codec) EncodeRequest(ctx context.Context, r queryrangebase.Request) (*http.Request, error) {
	header := make(http.Header)

	// Add query tags
	if queryTags := getQueryTags(ctx); queryTags != "" {
		header.Set(string(httpreq.QueryTagsHTTPHeader), queryTags)
	}

	if encodingFlags := httpreq.ExtractHeader(ctx, httpreq.LokiEncodingFlagsHeader); encodingFlags != "" {
		header.Set(httpreq.LokiEncodingFlagsHeader, encodingFlags)
	}

	// Add actor path
	if actor := httpreq.ExtractHeader(ctx, httpreq.LokiActorPathHeader); actor != "" {
		header.Set(httpreq.LokiActorPathHeader, actor)
	}

	// Add limits
	if limits := querylimits.ExtractQueryLimitsContext(ctx); limits != nil {
		err := querylimits.InjectQueryLimitsHeader(&header, limits)
		if err != nil {
			return nil, err
		}
	}

	// Add org id
	orgID, err := user.ExtractOrgID(ctx)
	if err != nil {
		return nil, err
	}
	header.Set(user.OrgIDHeaderName, orgID)

	switch request := r.(type) {
	case *LokiRequest:
		params := url.Values{
			"start":     []string{fmt.Sprintf("%d", request.StartTs.UnixNano())},
			"end":       []string{fmt.Sprintf("%d", request.EndTs.UnixNano())},
			"query":     []string{request.Query},
			"direction": []string{request.Direction.String()},
			"limit":     []string{fmt.Sprintf("%d", request.Limit)},
		}
		if len(request.Shards) > 0 {
			params["shards"] = request.Shards
		}
		if request.Step != 0 {
			params["step"] = []string{fmt.Sprintf("%f", float64(request.Step)/float64(1e3))}
		}
		if request.Interval != 0 {
			params["interval"] = []string{fmt.Sprintf("%f", float64(request.Interval)/float64(1e3))}
		}
		u := &url.URL{
			// the request could come /api/prom/query but we want to only use the new api.
			Path:     "/loki/api/v1/query_range",
			RawQuery: params.Encode(),
		}
		req := &http.Request{
			Method:     "GET",
			RequestURI: u.String(), // This is what the httpgrpc code looks at.
			URL:        u,
			Body:       http.NoBody,
			Header:     header,
		}

		return req.WithContext(ctx), nil
	case *LokiSeriesRequest:
		params := url.Values{
			"start":   []string{fmt.Sprintf("%d", request.StartTs.UnixNano())},
			"end":     []string{fmt.Sprintf("%d", request.EndTs.UnixNano())},
			"match[]": request.Match,
		}
		if len(request.Shards) > 0 {
			params["shards"] = request.Shards
		}
		u := &url.URL{
			Path:     "/loki/api/v1/series",
			RawQuery: params.Encode(),
		}
		req := &http.Request{
			Method:     "GET",
			RequestURI: u.String(), // This is what the httpgrpc code looks at.
			URL:        u,
			Body:       http.NoBody,
			Header:     header,
		}
		return req.WithContext(ctx), nil
	case *LabelRequest:
		params := url.Values{
			"start": []string{fmt.Sprintf("%d", request.Start.UnixNano())},
			"end":   []string{fmt.Sprintf("%d", request.End.UnixNano())},
			"query": []string{request.GetQuery()},
		}

		u := &url.URL{
			Path:     request.Path(), // NOTE: this could be either /label or /label/{name}/values endpoint. So forward the original path as it is.
			RawQuery: params.Encode(),
		}
		req := &http.Request{
			Method:     "GET",
			RequestURI: u.String(), // This is what the httpgrpc code looks at.
			URL:        u,
			Body:       http.NoBody,
			Header:     header,
		}
		return req.WithContext(ctx), nil
	case *LokiInstantRequest:
		params := url.Values{
			"query":     []string{request.Query},
			"direction": []string{request.Direction.String()},
			"limit":     []string{fmt.Sprintf("%d", request.Limit)},
			"time":      []string{fmt.Sprintf("%d", request.TimeTs.UnixNano())},
		}
		if len(request.Shards) > 0 {
			params["shards"] = request.Shards
		}
		u := &url.URL{
			// the request could come /api/prom/query but we want to only use the new api.
			Path:     "/loki/api/v1/query",
			RawQuery: params.Encode(),
		}
		req := &http.Request{
			Method:     "GET",
			RequestURI: u.String(), // This is what the httpgrpc code looks at.
			URL:        u,
			Body:       http.NoBody,
			Header:     header,
		}

		return req.WithContext(ctx), nil
	case *logproto.IndexStatsRequest:
		params := url.Values{
			"start": []string{fmt.Sprintf("%d", request.From.Time().UnixNano())},
			"end":   []string{fmt.Sprintf("%d", request.Through.Time().UnixNano())},
			"query": []string{request.GetQuery()},
		}
		u := &url.URL{
			Path:     "/loki/api/v1/index/stats",
			RawQuery: params.Encode(),
		}
		req := &http.Request{
			Method:     "GET",
			RequestURI: u.String(), // This is what the httpgrpc code looks at.
			URL:        u,
			Body:       http.NoBody,
			Header:     header,
		}
		return req.WithContext(ctx), nil
	case *logproto.VolumeRequest:
		params := url.Values{
			"start":       []string{fmt.Sprintf("%d", request.From.Time().UnixNano())},
			"end":         []string{fmt.Sprintf("%d", request.Through.Time().UnixNano())},
			"query":       []string{request.GetQuery()},
			"limit":       []string{fmt.Sprintf("%d", request.Limit)},
			"aggregateBy": []string{request.AggregateBy},
		}

		if len(request.TargetLabels) > 0 {
			params["targetLabels"] = []string{strings.Join(request.TargetLabels, ",")}
		}

		var u *url.URL
		if request.Step != 0 {
			params["step"] = []string{fmt.Sprintf("%f", float64(request.Step)/float64(1e3))}
			u = &url.URL{
				Path:     "/loki/api/v1/index/volume_range",
				RawQuery: params.Encode(),
			}
		} else {
			u = &url.URL{
				Path:     "/loki/api/v1/index/volume",
				RawQuery: params.Encode(),
			}
		}
		req := &http.Request{
			Method:     "GET",
			RequestURI: u.String(),
			URL:        u,
			Body:       http.NoBody,
			Header:     header,
		}
		return req.WithContext(ctx), nil
	default:
		return nil, httpgrpc.Errorf(http.StatusInternalServerError, fmt.Sprintf("invalid request format, got (%T)", r))
	}
}

// nolint:goconst
func (c Codec) Path(r queryrangebase.Request) string {
	switch request := r.(type) {
	case *LokiRequest:
		return "loki/api/v1/query_range"
	case *LokiSeriesRequest:
		return "loki/api/v1/series"
	case *LabelRequest:
		return request.Path() // NOTE: this could be either /label or /label/{name}/values endpoint. So forward the original path as it is.
	case *LokiInstantRequest:
		return "/loki/api/v1/query"
	case *logproto.IndexStatsRequest:
		return "/loki/api/v1/index/stats"
	case *logproto.VolumeRequest:
		return "/loki/api/v1/index/volume_range"
	}

	return "other"
}

func (p RequestProtobufCodec) EncodeRequest(ctx context.Context, r queryrangebase.Request) (*http.Request, error) {
	req, err := p.Codec.EncodeRequest(ctx, r)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/vnd.google.protobuf")
	return req, nil
}

type Buffer interface {
	Bytes() []byte
}

func (Codec) DecodeResponse(_ context.Context, r *http.Response, req queryrangebase.Request) (queryrangebase.Response, error) {
	if r.StatusCode/100 != 2 {
		body, _ := io.ReadAll(r.Body)
		return nil, httpgrpc.Errorf(r.StatusCode, string(body))
	}

	if r.Header.Get("Content-Type") == ProtobufType {
		return decodeResponseProtobuf(r, req)
	}

	// Default to JSON.
	return decodeResponseJSON(r, req)
}

func decodeResponseJSON(r *http.Response, req queryrangebase.Request) (queryrangebase.Response, error) {
	var buf []byte
	var err error
	if buffer, ok := r.Body.(Buffer); ok {
		buf = buffer.Bytes()
	} else {
		buf, err = io.ReadAll(r.Body)
		if err != nil {
			return nil, httpgrpc.Errorf(http.StatusInternalServerError, "error decoding response: %v", err)
		}
	}

	return decodeResponseJSONFrom(buf, req, r.Header)
}

func decodeResponseJSONFrom(buf []byte, req queryrangebase.Request, headers http.Header) (queryrangebase.Response, error) {

	switch req := req.(type) {
	case *LokiSeriesRequest:
		var resp loghttp.SeriesResponse
		if err := json.Unmarshal(buf, &resp); err != nil {
			return nil, httpgrpc.Errorf(http.StatusInternalServerError, "error decoding response: %v", err)
		}

		data := make([]logproto.SeriesIdentifier, 0, len(resp.Data))
		for _, label := range resp.Data {
			d := logproto.SeriesIdentifier{
				Labels: label.Map(),
			}
			data = append(data, d)
		}

		return &LokiSeriesResponse{
			Status:  resp.Status,
			Version: uint32(loghttp.GetVersion(req.Path)),
			Data:    data,
			Headers: httpResponseHeadersToPromResponseHeaders(headers),
		}, nil
	case *LabelRequest:
		var resp loghttp.LabelResponse
		if err := json.Unmarshal(buf, &resp); err != nil {
			return nil, httpgrpc.Errorf(http.StatusInternalServerError, "error decoding response: %v", err)
		}
		return &LokiLabelNamesResponse{
			Status:  resp.Status,
			Version: uint32(loghttp.GetVersion(req.Path())),
			Data:    resp.Data,
			Headers: httpResponseHeadersToPromResponseHeaders(headers),
		}, nil
	case *logproto.IndexStatsRequest:
		var resp logproto.IndexStatsResponse
		if err := json.Unmarshal(buf, &resp); err != nil {
			return nil, httpgrpc.Errorf(http.StatusInternalServerError, "error decoding response: %v", err)
		}
		return &IndexStatsResponse{
			Response: &resp,
			Headers:  httpResponseHeadersToPromResponseHeaders(headers),
		}, nil
	case *logproto.VolumeRequest:
		var resp logproto.VolumeResponse
		if err := json.Unmarshal(buf, &resp); err != nil {
			return nil, httpgrpc.Errorf(http.StatusInternalServerError, "error decoding response: %v", err)
		}
		return &VolumeResponse{
			Response: &resp,
			Headers:  httpResponseHeadersToPromResponseHeaders(headers),
		}, nil
	default:
		var resp loghttp.QueryResponse
		if err := resp.UnmarshalJSON(buf); err != nil {
			return nil, httpgrpc.Errorf(http.StatusInternalServerError, "error decoding response: %v", err)
		}
		switch string(resp.Data.ResultType) {
		case loghttp.ResultTypeMatrix:
			return &LokiPromResponse{
				Response: &queryrangebase.PrometheusResponse{
					Status: resp.Status,
					Data: queryrangebase.PrometheusData{
						ResultType: loghttp.ResultTypeMatrix,
						Result:     toProtoMatrix(resp.Data.Result.(loghttp.Matrix)),
					},
					Headers: convertPrometheusResponseHeadersToPointers(httpResponseHeadersToPromResponseHeaders(headers)),
				},
				Statistics: resp.Data.Statistics,
			}, nil
		case loghttp.ResultTypeStream:
			// This is the same as in querysharding.go
			params, err := ParamsFromRequest(req)
			if err != nil {
				return nil, err
			}

			var path string
			switch r := req.(type) {
			case *LokiRequest:
				path = r.GetPath()
			case *LokiInstantRequest:
				path = r.GetPath()
			default:
				return nil, fmt.Errorf("expected *LokiRequest or *LokiInstantRequest, got (%T)", r)
			}
			return &LokiResponse{
				Status:     resp.Status,
				Direction:  params.Direction(),
				Limit:      params.Limit(),
				Version:    uint32(loghttp.GetVersion(path)),
				Statistics: resp.Data.Statistics,
				Data: LokiData{
					ResultType: loghttp.ResultTypeStream,
					Result:     resp.Data.Result.(loghttp.Streams).ToProto(),
				},
				Headers: httpResponseHeadersToPromResponseHeaders(headers),
			}, nil
		case loghttp.ResultTypeVector:
			return &LokiPromResponse{
				Response: &queryrangebase.PrometheusResponse{
					Status: resp.Status,
					Data: queryrangebase.PrometheusData{
						ResultType: loghttp.ResultTypeVector,
						Result:     toProtoVector(resp.Data.Result.(loghttp.Vector)),
					},
					Headers: convertPrometheusResponseHeadersToPointers(httpResponseHeadersToPromResponseHeaders(headers)),
				},
				Statistics: resp.Data.Statistics,
			}, nil
		case loghttp.ResultTypeScalar:
			return &LokiPromResponse{
				Response: &queryrangebase.PrometheusResponse{
					Status: resp.Status,
					Data: queryrangebase.PrometheusData{
						ResultType: loghttp.ResultTypeScalar,
						Result:     toProtoScalar(resp.Data.Result.(loghttp.Scalar)),
					},
					Headers: convertPrometheusResponseHeadersToPointers(httpResponseHeadersToPromResponseHeaders(headers)),
				},
				Statistics: resp.Data.Statistics,
			}, nil
		default:
			return nil, httpgrpc.Errorf(http.StatusInternalServerError, "unsupported response type, got (%s)", string(resp.Data.ResultType))
		}
	}
}

func decodeResponseProtobuf(r *http.Response, req queryrangebase.Request) (queryrangebase.Response, error) {
	var buf []byte
	var err error
	if buffer, ok := r.Body.(Buffer); ok {
		buf = buffer.Bytes()
	} else {
		buf, err = io.ReadAll(r.Body)
		if err != nil {
			return nil, httpgrpc.Errorf(http.StatusInternalServerError, "error decoding response: %v", err)
		}
	}

	// Shortcut series responses without deserialization.
	if _, ok := req.(*LokiSeriesRequest); ok {
		return GetLokiSeriesResponseView(buf)
	}

	resp := &QueryResponse{}
	err = resp.Unmarshal(buf)
	if err != nil {
		return nil, httpgrpc.Errorf(http.StatusInternalServerError, "error decoding response: %v", err)
	}

	headers := httpResponseHeadersToPromResponseHeaders(r.Header)
	switch req.(type) {
	case *LokiSeriesRequest:
		return resp.GetSeries().WithHeaders(headers), nil
	case *LabelRequest:
		return resp.GetLabels().WithHeaders(headers), nil
	case *logproto.IndexStatsRequest:
		return resp.GetStats().WithHeaders(headers), nil
	default:
		switch concrete := resp.Response.(type) {
		case *QueryResponse_Prom:
			return concrete.Prom.WithHeaders(headers), nil
		case *QueryResponse_Streams:
			return concrete.Streams.WithHeaders(headers), nil
		case *QueryResponse_TopkSketches:
			return concrete.TopkSketches.WithHeaders(headers), nil
		case *QueryResponse_QuantileSketches:
			return concrete.QuantileSketches.WithHeaders(headers), nil
		default:
			return nil, httpgrpc.Errorf(http.StatusInternalServerError, "unsupported response type, got (%T)", resp.Response)
		}
	}
}

func (Codec) EncodeResponse(ctx context.Context, req *http.Request, res queryrangebase.Response) (*http.Response, error) {
	if req.Header.Get("Accept") == ProtobufType {
		return encodeResponseProtobuf(ctx, res)
	}

	// Default to JSON.
	version := loghttp.GetVersion(req.RequestURI)
	encodingFlags := httpreq.ExtractEncodingFlags(req)
	return encodeResponseJSON(ctx, version, res, encodingFlags)
}

func encodeResponseJSON(ctx context.Context, version loghttp.Version, res queryrangebase.Response, encodeFlags httpreq.EncodingFlags) (*http.Response, error) {
	sp, _ := opentracing.StartSpanFromContext(ctx, "codec.EncodeResponse")
	defer sp.Finish()
	var buf bytes.Buffer

	err := encodeResponseJSONTo(version, res, &buf, encodeFlags)
	if err != nil {
		return nil, err
	}

	sp.LogFields(otlog.Int("bytes", buf.Len()))

	resp := http.Response{
		Header: http.Header{
			"Content-Type": []string{"application/json; charset=UTF-8"},
		},
		Body:       io.NopCloser(&buf),
		StatusCode: http.StatusOK,
	}
	return &resp, nil
}

func encodeResponseJSONTo(version loghttp.Version, res queryrangebase.Response, w io.Writer, encodeFlags httpreq.EncodingFlags) error {
	switch response := res.(type) {
	case *LokiPromResponse:
		return response.encodeTo(w)
	case *LokiResponse:
		streams := make([]logproto.Stream, len(response.Data.Result))

		for i, stream := range response.Data.Result {
			streams[i] = logproto.Stream{
				Labels:  stream.Labels,
				Entries: stream.Entries,
			}
		}
		if version == loghttp.VersionLegacy {
			result := logqlmodel.Result{
				Data:       logqlmodel.Streams(streams),
				Statistics: response.Statistics,
			}
			if err := marshal_legacy.WriteQueryResponseJSON(result, w); err != nil {
				return err
			}
		} else {
			if err := marshal.WriteQueryResponseJSON(logqlmodel.Streams(streams), response.Statistics, w, encodeFlags); err != nil {
				return err
			}
		}
	case *MergedSeriesResponseView:
		if err := WriteSeriesResponseViewJSON(response, w); err != nil {
			return err
		}
	case *LokiSeriesResponse:
		if err := marshal.WriteSeriesResponseJSON(response.Data, w); err != nil {
			return err
		}
	case *LokiLabelNamesResponse:
		if loghttp.Version(response.Version) == loghttp.VersionLegacy {
			if err := marshal_legacy.WriteLabelResponseJSON(logproto.LabelResponse{Values: response.Data}, w); err != nil {
				return err
			}
		} else {
			if err := marshal.WriteLabelResponseJSON(response.Data, w); err != nil {
				return err
			}
		}
	case *IndexStatsResponse:
		if err := marshal.WriteIndexStatsResponseJSON(response.Response, w); err != nil {
			return err
		}
	case *VolumeResponse:
		if err := marshal.WriteVolumeResponseJSON(response.Response, w); err != nil {
			return err
		}
	default:
		return httpgrpc.Errorf(http.StatusInternalServerError, fmt.Sprintf("invalid response format, got (%T)", res))
	}

	return nil
}

func encodeResponseProtobuf(ctx context.Context, res queryrangebase.Response) (*http.Response, error) {
	sp, _ := opentracing.StartSpanFromContext(ctx, "codec.EncodeResponse")
	defer sp.Finish()

	p, err := QueryResponseWrap(res)
	if err != nil {
		return nil, httpgrpc.Errorf(http.StatusInternalServerError, err.Error())
	}

	buf, err := p.Marshal()
	if err != nil {
		return nil, fmt.Errorf("could not marshal protobuf: %w", err)
	}

	resp := http.Response{
		Header: http.Header{
			"Content-Type": []string{ProtobufType},
		},
		Body:       io.NopCloser(bytes.NewBuffer(buf)),
		StatusCode: http.StatusOK,
	}
	return &resp, nil
}

// NOTE: When we would start caching response from non-metric queries we would have to consider cache gen headers as well in
// MergeResponse implementation for Loki codecs same as it is done in Cortex at https://github.com/cortexproject/cortex/blob/21bad57b346c730d684d6d0205efef133422ab28/pkg/querier/queryrange/query_range.go#L170
func (Codec) MergeResponse(responses ...queryrangebase.Response) (queryrangebase.Response, error) {
	if len(responses) == 0 {
		return nil, errors.New("merging responses requires at least one response")
	}
	var mergedStats stats.Result
	switch responses[0].(type) {
	case *LokiPromResponse:

		promResponses := make([]queryrangebase.Response, 0, len(responses))
		for _, res := range responses {
			mergedStats.MergeSplit(res.(*LokiPromResponse).Statistics)
			promResponses = append(promResponses, res.(*LokiPromResponse).Response)
		}
		promRes, err := queryrangebase.PrometheusCodec.MergeResponse(promResponses...)
		if err != nil {
			return nil, err
		}
		return &LokiPromResponse{
			Response:   promRes.(*queryrangebase.PrometheusResponse),
			Statistics: mergedStats,
		}, nil
	case *LokiResponse:
		return mergeLokiResponse(responses...), nil
	case *LokiSeriesResponse:
		lokiSeriesRes := responses[0].(*LokiSeriesResponse)

		var lokiSeriesData []logproto.SeriesIdentifier
		uniqueSeries := make(map[uint64]struct{})

		// The buffers are used by `series.Hash`. They are allocated
		// outside of the method in order to reuse them for the next
		// iteration. This saves a lot of allocations.
		// 1KB is used for `b` after some experimentation. The
		// benchmarks are ~10% faster in comparison to no buffer with
		// little overhead. A run with 4MB should the same speedup but
		// much much more overhead.
		b := make([]byte, 0, 1024)
		keyBuffer := make([]string, 0, 32)
		var key uint64

		// only unique series should be merged
		for _, res := range responses {
			lokiResult := res.(*LokiSeriesResponse)
			mergedStats.MergeSplit(lokiResult.Statistics)
			for _, series := range lokiResult.Data {
				// Use series hash as the key and reuse key
				// buffer to avoid extra allocations.
				key, keyBuffer = series.Hash(b, keyBuffer)

				// TODO(karsten): There is a chance that the
				// keys match but not the labels due to hash
				// collision. Ideally there's an else block the
				// compares the series labels. However, that's
				// not trivial. Besides, instance.Series has the
				// same issue in its deduping logic.
				if _, ok := uniqueSeries[key]; !ok {
					lokiSeriesData = append(lokiSeriesData, series)
					uniqueSeries[key] = struct{}{}
				}
			}
		}

		return &LokiSeriesResponse{
			Status:     lokiSeriesRes.Status,
			Version:    lokiSeriesRes.Version,
			Data:       lokiSeriesData,
			Statistics: mergedStats,
		}, nil
	case *LokiSeriesResponseView:
		v := &MergedSeriesResponseView{}
		for _, r := range responses {
			v.responses = append(v.responses, r.(*LokiSeriesResponseView))
		}
		return v, nil
	case *MergedSeriesResponseView:
		v := &MergedSeriesResponseView{}
		for _, r := range responses {
			v.responses = append(v.responses, r.(*MergedSeriesResponseView).responses...)
		}
		return v, nil
	case *LokiLabelNamesResponse:
		labelNameRes := responses[0].(*LokiLabelNamesResponse)
		uniqueNames := make(map[string]struct{})
		names := []string{}

		// only unique name should be merged
		for _, res := range responses {
			lokiResult := res.(*LokiLabelNamesResponse)
			mergedStats.MergeSplit(lokiResult.Statistics)
			for _, labelName := range lokiResult.Data {
				if _, ok := uniqueNames[labelName]; !ok {
					names = append(names, labelName)
					uniqueNames[labelName] = struct{}{}
				}
			}
		}

		return &LokiLabelNamesResponse{
			Status:     labelNameRes.Status,
			Version:    labelNameRes.Version,
			Data:       names,
			Statistics: mergedStats,
		}, nil
	case *IndexStatsResponse:
		headers := responses[0].(*IndexStatsResponse).Headers
		stats := make([]*indexStats.Stats, len(responses))
		for i, res := range responses {
			stats[i] = res.(*IndexStatsResponse).Response
		}

		mergedIndexStats := indexStats.MergeStats(stats...)

		return &IndexStatsResponse{
			Response: &mergedIndexStats,
			Headers:  headers,
		}, nil
	case *VolumeResponse:
		resp0 := responses[0].(*VolumeResponse)
		headers := resp0.Headers

		resps := make([]*logproto.VolumeResponse, 0, len(responses))
		for _, r := range responses {
			resps = append(resps, r.(*VolumeResponse).Response)
		}

		return &VolumeResponse{
			Response: seriesvolume.Merge(resps, resp0.Response.Limit),
			Headers:  headers,
		}, nil
	default:
		return nil, fmt.Errorf("unknown response type (%T) in merging responses", responses[0])
	}
}

// mergeOrderedNonOverlappingStreams merges a set of ordered, nonoverlapping responses by concatenating matching streams then running them through a heap to pull out limit values
func mergeOrderedNonOverlappingStreams(resps []*LokiResponse, limit uint32, direction logproto.Direction) []logproto.Stream {
	var total int

	// turn resps -> map[labels] []entries
	groups := make(map[string]*byDir)
	for _, resp := range resps {
		for _, stream := range resp.Data.Result {
			s, ok := groups[stream.Labels]
			if !ok {
				s = &byDir{
					direction: direction,
					labels:    stream.Labels,
				}
				groups[stream.Labels] = s
			}

			s.markers = append(s.markers, stream.Entries)
			total += len(stream.Entries)
		}

		// optimization: since limit has been reached, no need to append entries from subsequent responses
		if total >= int(limit) {
			break
		}
	}

	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	if direction == logproto.BACKWARD {
		sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	} else {
		sort.Strings(keys)
	}

	// escape hatch, can just return all the streams
	if total <= int(limit) {
		results := make([]logproto.Stream, 0, len(keys))
		for _, key := range keys {
			results = append(results, logproto.Stream{
				Labels:  key,
				Entries: groups[key].merge(),
			})
		}
		return results
	}

	pq := &priorityqueue{
		direction: direction,
	}

	for _, key := range keys {
		stream := &logproto.Stream{
			Labels:  key,
			Entries: groups[key].merge(),
		}
		if len(stream.Entries) > 0 {
			pq.streams = append(pq.streams, stream)
		}
	}

	heap.Init(pq)

	resultDict := make(map[string]*logproto.Stream)

	// we want the min(limit, num_entries)
	for i := 0; i < int(limit) && pq.Len() > 0; i++ {
		// grab the next entry off the queue. This will be a stream (to preserve labels) with one entry.
		next := heap.Pop(pq).(*logproto.Stream)

		s, ok := resultDict[next.Labels]
		if !ok {
			s = &logproto.Stream{
				Labels:  next.Labels,
				Entries: make([]logproto.Entry, 0, int(limit)/len(keys)), // allocation hack -- assume uniform distribution across labels
			}
			resultDict[next.Labels] = s
		}
		// TODO: make allocation friendly
		s.Entries = append(s.Entries, next.Entries...)
	}

	results := make([]logproto.Stream, 0, len(resultDict))
	for _, key := range keys {
		stream, ok := resultDict[key]
		if ok {
			results = append(results, *stream)
		}
	}

	return results
}

func toProtoMatrix(m loghttp.Matrix) []queryrangebase.SampleStream {
	res := make([]queryrangebase.SampleStream, 0, len(m))

	if len(m) == 0 {
		return res
	}

	for _, stream := range m {
		samples := make([]logproto.LegacySample, 0, len(stream.Values))
		for _, s := range stream.Values {
			samples = append(samples, logproto.LegacySample{
				Value:       float64(s.Value),
				TimestampMs: int64(s.Timestamp),
			})
		}
		res = append(res, queryrangebase.SampleStream{
			Labels:  logproto.FromMetricsToLabelAdapters(stream.Metric),
			Samples: samples,
		})
	}
	return res
}

func toProtoVector(v loghttp.Vector) []queryrangebase.SampleStream {
	res := make([]queryrangebase.SampleStream, 0, len(v))

	if len(v) == 0 {
		return res
	}
	for _, s := range v {
		res = append(res, queryrangebase.SampleStream{
			Samples: []logproto.LegacySample{{
				Value:       float64(s.Value),
				TimestampMs: int64(s.Timestamp),
			}},
			Labels: logproto.FromMetricsToLabelAdapters(s.Metric),
		})
	}
	return res
}

func toProtoScalar(v loghttp.Scalar) []queryrangebase.SampleStream {
	res := make([]queryrangebase.SampleStream, 0, 1)

	res = append(res, queryrangebase.SampleStream{
		Samples: []logproto.LegacySample{{
			Value:       float64(v.Value),
			TimestampMs: v.Timestamp.UnixNano() / 1e6,
		}},
		Labels: nil,
	})
	return res
}

func (res LokiResponse) Count() int64 {
	var result int64
	for _, s := range res.Data.Result {
		result += int64(len(s.Entries))
	}
	return result
}

func ParamsFromRequest(req queryrangebase.Request) (logql.Params, error) {
	switch r := req.(type) {
	case *LokiRequest:
		return &paramsRangeWrapper{
			LokiRequest: r,
		}, nil
	case *logproto.VolumeRequest:
		return &paramsRangeWrapper{
			LokiRequest: &LokiRequest{
				Query:   r.GetQuery(),
				Limit:   uint32(r.GetLimit()),
				Step:    r.GetStep(),
				StartTs: time.UnixMilli(r.GetStart().UnixNano()),
				EndTs:   time.UnixMilli(r.GetEnd().UnixNano()),
			},
		}, nil
	case *LokiInstantRequest:
		return &paramsInstantWrapper{
			LokiInstantRequest: r,
		}, nil
	case *LokiSeriesRequest:
		return &paramsSeriesWrapper{
			LokiSeriesRequest: r,
		}, nil
	case *LabelRequest:
		return &paramsLabelWrapper{
			LabelRequest: r,
		}, nil
	case *logproto.IndexStatsRequest:
		return &paramsStatsWrapper{
			IndexStatsRequest: r,
		}, nil
	default:
		return nil, fmt.Errorf("expected one of the *LokiRequest, *LokiInstantRequest, *LokiSeriesRequest, *LokiLabelNamesRequest, got (%T)", r)
	}
}

type paramsRangeWrapper struct {
	*LokiRequest
}

func (p paramsRangeWrapper) Query() string {
	return p.GetQuery()
}

func (p paramsRangeWrapper) Start() time.Time {
	return p.GetStartTs()
}

func (p paramsRangeWrapper) End() time.Time {
	return p.GetEndTs()
}

func (p paramsRangeWrapper) Step() time.Duration {
	return time.Duration(p.GetStep() * 1e6)
}
func (p paramsRangeWrapper) Interval() time.Duration {
	return time.Duration(p.GetInterval() * 1e6)
}
func (p paramsRangeWrapper) Direction() logproto.Direction {
	return p.GetDirection()
}
func (p paramsRangeWrapper) Limit() uint32 { return p.LokiRequest.Limit }
func (p paramsRangeWrapper) Shards() []string {
	return p.GetShards()
}

type paramsInstantWrapper struct {
	*LokiInstantRequest
}

func (p paramsInstantWrapper) Query() string {
	return p.GetQuery()
}

func (p paramsInstantWrapper) Start() time.Time {
	return p.LokiInstantRequest.GetTimeTs()
}

func (p paramsInstantWrapper) End() time.Time {
	return p.LokiInstantRequest.GetTimeTs()
}

func (p paramsInstantWrapper) Step() time.Duration {
	return time.Duration(p.GetStep() * 1e6)
}
func (p paramsInstantWrapper) Interval() time.Duration { return 0 }
func (p paramsInstantWrapper) Direction() logproto.Direction {
	return p.GetDirection()
}
func (p paramsInstantWrapper) Limit() uint32 { return p.LokiInstantRequest.Limit }
func (p paramsInstantWrapper) Shards() []string {
	return p.GetShards()
}

type paramsSeriesWrapper struct {
	*LokiSeriesRequest
}

func (p paramsSeriesWrapper) Query() string {
	return p.GetQuery()
}

func (p paramsSeriesWrapper) Start() time.Time {
	return p.LokiSeriesRequest.GetStartTs()
}

func (p paramsSeriesWrapper) End() time.Time {
	return p.LokiSeriesRequest.GetEndTs()
}

func (p paramsSeriesWrapper) Step() time.Duration {
	return time.Duration(p.GetStep() * 1e6)
}
func (p paramsSeriesWrapper) Interval() time.Duration { return 0 }
func (p paramsSeriesWrapper) Direction() logproto.Direction {
	return logproto.FORWARD
}
func (p paramsSeriesWrapper) Limit() uint32 { return 0 }
func (p paramsSeriesWrapper) Shards() []string {
	return p.GetShards()
}

type paramsLabelWrapper struct {
	*LabelRequest
}

func (p paramsLabelWrapper) Query() string {
	return p.GetQuery()
}

func (p paramsLabelWrapper) Start() time.Time {
	return p.LabelRequest.GetStartTs()
}

func (p paramsLabelWrapper) End() time.Time {
	return p.LabelRequest.GetEndTs()
}

func (p paramsLabelWrapper) Step() time.Duration {
	return time.Duration(p.GetStep() * 1e6)
}
func (p paramsLabelWrapper) Interval() time.Duration { return 0 }
func (p paramsLabelWrapper) Direction() logproto.Direction {
	return logproto.FORWARD
}
func (p paramsLabelWrapper) Limit() uint32 { return 0 }
func (p paramsLabelWrapper) Shards() []string {
	return make([]string, 0)
}

type paramsStatsWrapper struct {
	*logproto.IndexStatsRequest
}

func (p paramsStatsWrapper) Query() string {
	return p.GetQuery()
}

func (p paramsStatsWrapper) Start() time.Time {
	return p.From.Time()
}

func (p paramsStatsWrapper) End() time.Time {
	return p.Through.Time()
}

func (p paramsStatsWrapper) Step() time.Duration {
	return time.Duration(p.GetStep() * 1e6)
}
func (p paramsStatsWrapper) Interval() time.Duration { return 0 }
func (p paramsStatsWrapper) Direction() logproto.Direction {
	return logproto.FORWARD
}
func (p paramsStatsWrapper) Limit() uint32 { return 0 }
func (p paramsStatsWrapper) Shards() []string {
	return make([]string, 0)
}

func httpResponseHeadersToPromResponseHeaders(httpHeaders http.Header) []queryrangebase.PrometheusResponseHeader {
	var promHeaders []queryrangebase.PrometheusResponseHeader
	for h, hv := range httpHeaders {
		promHeaders = append(promHeaders, queryrangebase.PrometheusResponseHeader{Name: h, Values: hv})
	}

	return promHeaders
}

func getQueryTags(ctx context.Context) string {
	v, _ := ctx.Value(httpreq.QueryTagsHTTPHeader).(string) // it's ok to be empty
	return v
}

func NewEmptyResponse(r queryrangebase.Request) (queryrangebase.Response, error) {
	switch req := r.(type) {
	case *LokiSeriesRequest:
		return &LokiSeriesResponse{
			Status:  loghttp.QueryStatusSuccess,
			Version: uint32(loghttp.GetVersion(req.Path)),
		}, nil
	case *LabelRequest:
		return &LokiLabelNamesResponse{
			Status:  loghttp.QueryStatusSuccess,
			Version: uint32(loghttp.GetVersion(req.Path())),
		}, nil
	case *LokiInstantRequest:
		// instant queries in the frontend are always metrics queries.
		return &LokiPromResponse{
			Response: &queryrangebase.PrometheusResponse{
				Status: loghttp.QueryStatusSuccess,
				Data: queryrangebase.PrometheusData{
					ResultType: loghttp.ResultTypeVector,
				},
			},
		}, nil
	case *LokiRequest:
		// range query can either be metrics or logs
		expr, err := syntax.ParseExpr(req.Query)
		if err != nil {
			return nil, httpgrpc.Errorf(http.StatusBadRequest, err.Error())
		}
		if _, ok := expr.(syntax.SampleExpr); ok {
			return &LokiPromResponse{
				Response: queryrangebase.NewEmptyPrometheusResponse(),
			}, nil
		}
		return &LokiResponse{
			Status:    loghttp.QueryStatusSuccess,
			Direction: req.Direction,
			Limit:     req.Limit,
			Version:   uint32(loghttp.GetVersion(req.Path)),
			Data: LokiData{
				ResultType: loghttp.ResultTypeStream,
			},
		}, nil
	case *logproto.IndexStatsRequest:
		return &IndexStatsResponse{}, nil
	case *logproto.VolumeRequest:
		return &VolumeResponse{}, nil
	default:
		return nil, fmt.Errorf("unsupported request type %T", req)
	}
}

func mergeLokiResponse(responses ...queryrangebase.Response) *LokiResponse {
	if len(responses) == 0 {
		return nil
	}
	var (
		lokiRes       = responses[0].(*LokiResponse)
		mergedStats   stats.Result
		lokiResponses = make([]*LokiResponse, 0, len(responses))
	)

	for _, res := range responses {
		lokiResult := res.(*LokiResponse)
		mergedStats.MergeSplit(lokiResult.Statistics)
		lokiResponses = append(lokiResponses, lokiResult)
	}

	return &LokiResponse{
		Status:     loghttp.QueryStatusSuccess,
		Direction:  lokiRes.Direction,
		Limit:      lokiRes.Limit,
		Version:    lokiRes.Version,
		ErrorType:  lokiRes.ErrorType,
		Error:      lokiRes.Error,
		Statistics: mergedStats,
		Data: LokiData{
			ResultType: loghttp.ResultTypeStream,
			Result:     mergeOrderedNonOverlappingStreams(lokiResponses, lokiRes.Limit, lokiRes.Direction),
		},
	}
}
