package forwarder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/cloudwatchiface"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/ssmiface"
)

// Forwarder forwards metrics of AWS CloudWatch to Mackerel
type Forwarder struct {
	Config aws.Config
	APIKey string

	mu            sync.Mutex
	svcssm        ssmiface.SSMAPI
	svccloudwatch cloudwatchiface.CloudWatchAPI
}

func (f *Forwarder) ssm() ssmiface.SSMAPI {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.svcssm == nil {
		f.svcssm = ssm.New(f.Config)
	}
	return f.svcssm
}

func (f *Forwarder) cloudwatch() cloudwatchiface.CloudWatchAPI {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.svccloudwatch == nil {
		f.svccloudwatch = cloudwatch.New(f.Config)
	}
	return f.svccloudwatch
}

// ForwardMetrics forwards metrics of AWS CloudWatch to Mackerel
func (f *Forwarder) ForwardMetrics(ctx context.Context, event ForwardMetricsEvent) error {
	c := &MackerelClient{}
	now := time.Now()
	prev := now.Add(-2 * time.Minute) // 2 min (to fetch at least 1 data-point)

	for _, def := range event.ServiceMetrics {
		template := &cloudwatch.GetMetricStatisticsInput{
			Dimensions: []cloudwatch.Dimension{},
			// TODO: support ExtendedStatistics
			Statistics: []cloudwatch.Statistic{cloudwatch.Statistic(def.Stat)},
			StartTime:  aws.Time(prev),
			EndTime:    aws.Time(now),
			Period:     aws.Int64(60),
		}
		input, err := ParseMetric(template, def.Metric)
		if err != nil {
			return err
		}
		req := f.cloudwatch().GetMetricStatisticsRequest(input)
		req.SetContext(ctx)
		resp, err := req.Send() // TODO: Send request in parallel
		if err != nil {
			return err
		}
		for _, p := range resp.Datapoints {
			log.Println(p)
			c.PostServiceMetricValues(ctx, def.Service, []*ServiceMetricValue{{
				Name:  def.Name,
				Time:  p.Timestamp.Unix(),
				Value: aws.Float64Value(p.Sum), // TODO: read from def.Stat
			}})
		}
	}
	return nil
}

// ForwardMetricsEvent is an event of ForwardMetrics.
type ForwardMetricsEvent struct {
	ServiceMetrics []ServiceMetricDefinition `json:"service_metrics"`
	HostMetrics    []HostMetricDefinition    `json:"host_metrics"`
}

// ServiceMetricDefinition is a definition for converting a metric of AWS CloudWatch to Mackerel's Service Metrics.
// https://mackerel.io/api-docs/entry/service-metrics
type ServiceMetricDefinition struct {
	Service string      `json:"service"`
	Name    string      `json:"name"`
	Metric  interface{} `json:"metric"`
	Stat    string      `json:"stat"`
}

// HostMetricDefinition is a definition for converting a metric of AWS CloudWatch to Mackerel's Host Metrics.
// https://mackerel.io/api-docs/entry/host-metrics
type HostMetricDefinition struct {
	HostID string      `json:"hostId"`
	Name   string      `json:"name"`
	Metric interface{} `json:"metric"`
	Stat   string      `json:"stat"`
}

// ParseMetric parses the metrics definitions.
// See https://docs.aws.amazon.com/AmazonCloudWatch/latest/APIReference/CloudWatch-Dashboard-Body-Structure.html#CloudWatch-Dashboard-Properties-Metrics-Array-Format
// The rendering properties object will be ignored.
func ParseMetric(template *cloudwatch.GetMetricStatisticsInput, def interface{}) (*cloudwatch.GetMetricStatisticsInput, error) {
	var ret cloudwatch.GetMetricStatisticsInput
	ret = *template

	var array []interface{}
	switch def := def.(type) {
	case []interface{}:
		array = def
	case []string:
		array = make([]interface{}, 0, len(def))
		for _, v := range def {
			array = append(array, v)
		}
	case string:
		if err := json.Unmarshal([]byte(def), &array); err != nil {
			return nil, err
		}
	case []byte:
		if err := json.Unmarshal(def, &array); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("forwarder: type of metrics definition is invalid: %T", def)
	}

	if len(array) < 2 {
		return nil, errors.New("forwarder: Namespace and MetricName are required")
	}

	namespace, ok := array[0].(string)
	if !ok {
		return nil, fmt.Errorf("forwarder: invalid type of Namespace: %T", array[0])
	}
	ret.Namespace = aws.String(namespace)

	metricName, ok := array[1].(string)
	if !ok {
		return nil, fmt.Errorf("forwarder: invalid type of MetricName: %T", array[1])
	}
	ret.MetricName = aws.String(metricName)

	dimensions := []cloudwatch.Dimension{}
	for i := 2; i+1 < len(array); i += 2 {
		name, ok := array[i].(string)
		if !ok {
			return nil, fmt.Errorf("forwarder: invalid type of DimensionName: %T", array[i])
		}
		value, ok := array[i+1].(string)
		if !ok {
			return nil, fmt.Errorf("forwarder: invalid type of DimensionValue: %T", array[i+1])
		}
		dimensions = append(dimensions, cloudwatch.Dimension{
			Name:  aws.String(name),
			Value: aws.String(value),
		})
	}
	ret.Dimensions = dimensions

	return &ret, nil
}
