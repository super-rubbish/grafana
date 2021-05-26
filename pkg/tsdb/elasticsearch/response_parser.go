package elasticsearch

import (
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/data"
	"github.com/grafana/grafana/pkg/components/simplejson"
	"github.com/grafana/grafana/pkg/plugins"
	es "github.com/grafana/grafana/pkg/tsdb/elasticsearch/client"
)

const (
	// Metric types
	countType         = "count"
	percentilesType   = "percentiles"
	extendedStatsType = "extended_stats"
	// Bucket types
	dateHistType    = "date_histogram"
	histogramType   = "histogram"
	filtersType     = "filters"
	termsType       = "terms"
	geohashGridType = "geohash_grid"
)

type responseParser struct {
	Responses []*es.SearchResponse
	Targets   []*Query
	DebugInfo *es.SearchDebugInfo
}

var newResponseParser = func(responses []*es.SearchResponse, targets []*Query, debugInfo *es.SearchDebugInfo) *responseParser {
	return &responseParser{
		Responses: responses,
		Targets:   targets,
		DebugInfo: debugInfo,
	}
}

// nolint:staticcheck // plugins.DataResponse deprecated
func (rp *responseParser) getTimeSeries() (plugins.DataResponse, error) {
	result := plugins.DataResponse{
		Results: make(map[string]plugins.DataQueryResult),
	}
	if rp.Responses == nil {
		return result, nil
	}

	for i, res := range rp.Responses {
		target := rp.Targets[i]

		var debugInfo *simplejson.Json
		if rp.DebugInfo != nil && i == 0 {
			debugInfo = simplejson.NewFromAny(rp.DebugInfo)
		}

		if res.Error != nil {
			errRslt := getErrorFromElasticResponse(res)
			errRslt.Meta = debugInfo
			result.Results[target.RefID] = errRslt
			continue
		}

		queryRes := plugins.DataQueryResult{
			Meta: debugInfo,
		}
		props := make(map[string]string)
		table := plugins.DataTable{
			Columns: make([]plugins.DataTableColumn, 0),
			Rows:    make([]plugins.DataRowValues, 0),
		}
		err := rp.processBuckets(res.Aggregations, target, &queryRes, &table, props, 0)
		if err != nil {
			return plugins.DataResponse{}, err
		}
		rp.nameFields(queryRes, target)
		rp.trimDatapoints(queryRes, target)

		if len(table.Rows) > 0 {
			queryRes.Tables = append(queryRes.Tables, table)
		}

		result.Results[target.RefID] = queryRes
	}
	return result, nil
}

// nolint:staticcheck // plugins.* deprecated
func (rp *responseParser) processBuckets(aggs map[string]interface{}, target *Query,
	queryResult *plugins.DataQueryResult, table *plugins.DataTable, props map[string]string, depth int) error {
	var err error
	maxDepth := len(target.BucketAggs) - 1

	aggIDs := make([]string, 0)
	for k := range aggs {
		aggIDs = append(aggIDs, k)
	}
	sort.Strings(aggIDs)
	for _, aggID := range aggIDs {
		v := aggs[aggID]
		aggDef, _ := findAgg(target, aggID)
		esAgg := simplejson.NewFromAny(v)
		if aggDef == nil {
			continue
		}

		if depth == maxDepth {
			if aggDef.Type == dateHistType {
				err = rp.processMetrics(esAgg, target, queryResult, props)
			} else {
				err = rp.processAggregationDocs(esAgg, aggDef, target, table, props)
			}
			if err != nil {
				return err
			}
		} else {
			for _, b := range esAgg.Get("buckets").MustArray() {
				bucket := simplejson.NewFromAny(b)
				newProps := make(map[string]string)

				for k, v := range props {
					newProps[k] = v
				}

				if key, err := bucket.Get("key").String(); err == nil {
					newProps[aggDef.Field] = key
				} else if key, err := bucket.Get("key").Int64(); err == nil {
					newProps[aggDef.Field] = strconv.FormatInt(key, 10)
				}

				if key, err := bucket.Get("key_as_string").String(); err == nil {
					newProps[aggDef.Field] = key
				}
				err = rp.processBuckets(bucket.MustMap(), target, queryResult, table, newProps, depth+1)
				if err != nil {
					return err
				}
			}

			buckets := esAgg.Get("buckets").MustMap()
			bucketKeys := make([]string, 0)
			for k := range buckets {
				bucketKeys = append(bucketKeys, k)
			}
			sort.Strings(bucketKeys)

			for _, bucketKey := range bucketKeys {
				bucket := simplejson.NewFromAny(buckets[bucketKey])
				newProps := make(map[string]string)

				for k, v := range props {
					newProps[k] = v
				}

				newProps["filter"] = bucketKey

				err = rp.processBuckets(bucket.MustMap(), target, queryResult, table, newProps, depth+1)
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// nolint:staticcheck // plugins.* deprecated
func (rp *responseParser) processMetrics(esAgg *simplejson.Json, target *Query, query *plugins.DataQueryResult,
	props map[string]string) error {
	frames := data.Frames{}
	esAggBuckets := esAgg.Get("buckets").MustArray()

	for _, metric := range target.Metrics {
		if metric.Hide {
			continue
		}

		tags := make(map[string]string, len(props))
		timeVector := make([]time.Time, 0, len(esAggBuckets))
		values := make([]*float64, 0, len(esAggBuckets))

		switch metric.Type {
		case countType:
			for _, v := range esAggBuckets {
				bucket := simplejson.NewFromAny(v)
				value := castToFloat(bucket.Get("doc_count"))
				key := castToFloat(bucket.Get("key"))
				timeVector = append(timeVector, time.Unix(int64(*key)/1000, 0).UTC())
				values = append(values, value)
			}

			for k, v := range props {
				tags[k] = v
			}
			tags["metric"] = countType
			frames = append(frames, data.NewFrame(metric.Field,
				data.NewField("time", nil, timeVector),
				data.NewField("value", tags, values).SetConfig(&data.FieldConfig{DisplayNameFromDS: metric.Field})))
		case percentilesType:
			buckets := esAggBuckets
			if len(buckets) == 0 {
				break
			}

			firstBucket := simplejson.NewFromAny(buckets[0])
			percentiles := firstBucket.GetPath(metric.ID, "values").MustMap()

			percentileKeys := make([]string, 0)
			for k := range percentiles {
				percentileKeys = append(percentileKeys, k)
			}
			sort.Strings(percentileKeys)
			for _, percentileName := range percentileKeys {
				tags := make(map[string]string, len(props))

				for k, v := range props {
					tags[k] = v
				}
				tags["metric"] = "p" + percentileName
				tags["field"] = metric.Field
				for _, v := range buckets {
					bucket := simplejson.NewFromAny(v)
					value := castToFloat(bucket.GetPath(metric.ID, "values", percentileName))
					key := castToFloat(bucket.Get("key"))
					timeVector = append(timeVector, time.Unix(int64(*key)/1000, 0).UTC())
					values = append(values, value)
				}
				frames = append(frames, data.NewFrame(metric.Field,
					data.NewField("time", nil, timeVector),
					data.NewField("value", tags, values).SetConfig(&data.FieldConfig{DisplayNameFromDS: tags["metric"] + metric.Field})))
			}
		case extendedStatsType:
			buckets := esAggBuckets

			metaKeys := make([]string, 0)
			meta := metric.Meta.MustMap()
			for k := range meta {
				metaKeys = append(metaKeys, k)
			}
			sort.Strings(metaKeys)
			for _, statName := range metaKeys {
				v := meta[statName]
				if enabled, ok := v.(bool); !ok || !enabled {
					continue
				}

				tags := make(map[string]string, len(props))

				for k, v := range props {
					tags[k] = v
				}
				tags["metric"] = statName
				tags["field"] = metric.Field

				for _, v := range buckets {
					bucket := simplejson.NewFromAny(v)
					key := castToFloat(bucket.Get("key"))
					var value *float64
					switch statName {
					case "std_deviation_bounds_upper":
						value = castToFloat(bucket.GetPath(metric.ID, "std_deviation_bounds", "upper"))
					case "std_deviation_bounds_lower":
						value = castToFloat(bucket.GetPath(metric.ID, "std_deviation_bounds", "lower"))
					default:
						value = castToFloat(bucket.GetPath(metric.ID, statName))
					}
					timeVector = append(timeVector, time.Unix(int64(*key)/1000, 0).UTC())
					values = append(values, value)
				}
				labels := tags
				frames = append(frames, data.NewFrame(metric.Field,
					data.NewField("time", nil, timeVector),
					data.NewField("value", labels, values).SetConfig(&data.FieldConfig{DisplayNameFromDS: metric.Field})))
			}
		default:
			for k, v := range props {
				tags[k] = v
			}

			tags["metric"] = metric.Type
			tags["field"] = metric.Field
			tags["metricId"] = metric.ID
			for _, v := range esAggBuckets {
				bucket := simplejson.NewFromAny(v)
				key := castToFloat(bucket.Get("key"))
				valueObj, err := bucket.Get(metric.ID).Map()
				if err != nil {
					continue
				}
				var value *float64
				if _, ok := valueObj["normalized_value"]; ok {
					value = castToFloat(bucket.GetPath(metric.ID, "normalized_value"))
				} else {
					value = castToFloat(bucket.GetPath(metric.ID, "value"))
				}
				timeVector = append(timeVector, time.Unix(int64(*key)/1000, 0).UTC())
				values = append(values, value)
			}
			frames = append(frames, data.NewFrame(metric.Field,
				data.NewField("time", nil, timeVector),
				data.NewField("value", tags, values).SetConfig(&data.FieldConfig{DisplayNameFromDS: metric.Field})))
		}
	}
	if query.Dataframes != nil {
		oldFrames, err := query.Dataframes.Decoded()
		if err != nil {
			return err
		}
		frames = append(oldFrames, frames...)
	}
	query.Dataframes = plugins.NewDecodedDataFrames(frames)
	return nil
}

// nolint:staticcheck // plugins.* deprecated
func (rp *responseParser) processAggregationDocs(esAgg *simplejson.Json, aggDef *BucketAgg, target *Query,
	table *plugins.DataTable, props map[string]string) error {
	propKeys := make([]string, 0)
	for k := range props {
		propKeys = append(propKeys, k)
	}
	sort.Strings(propKeys)

	if len(table.Columns) == 0 {
		for _, propKey := range propKeys {
			table.Columns = append(table.Columns, plugins.DataTableColumn{Text: propKey})
		}
		table.Columns = append(table.Columns, plugins.DataTableColumn{Text: aggDef.Field})
	}

	addMetricValue := func(values *plugins.DataRowValues, metricName string, value *float64) {
		found := false
		for _, c := range table.Columns {
			if c.Text == metricName {
				found = true
				break
			}
		}
		if !found {
			table.Columns = append(table.Columns, plugins.DataTableColumn{Text: metricName})
		}
	}

	for _, v := range esAgg.Get("buckets").MustArray() {
		bucket := simplejson.NewFromAny(v)
		values := make(plugins.DataRowValues, 0)

		for _, propKey := range propKeys {
			values = append(values, props[propKey])
		}

		if key, err := bucket.Get("key").String(); err == nil {
			values = append(values, key)
		} else {
			values = append(values, castToFloat(bucket.Get("key")))
		}

		for _, metric := range target.Metrics {
			switch metric.Type {
			case countType:
				addMetricValue(&values, rp.getMetricName(metric.Type), castToFloat(bucket.Get("doc_count")))
			case extendedStatsType:
				metaKeys := make([]string, 0)
				meta := metric.Meta.MustMap()
				for k := range meta {
					metaKeys = append(metaKeys, k)
				}
				sort.Strings(metaKeys)
				for _, statName := range metaKeys {
					v := meta[statName]
					if enabled, ok := v.(bool); !ok || !enabled {
						continue
					}

					var value *float64
					switch statName {
					case "std_deviation_bounds_upper":
						value = castToFloat(bucket.GetPath(metric.ID, "std_deviation_bounds", "upper"))
					case "std_deviation_bounds_lower":
						value = castToFloat(bucket.GetPath(metric.ID, "std_deviation_bounds", "lower"))
					default:
						value = castToFloat(bucket.GetPath(metric.ID, statName))
					}

					addMetricValue(&values, rp.getMetricName(metric.Type), value)
					break
				}
			default:
				metricName := rp.getMetricName(metric.Type)
				otherMetrics := make([]*MetricAgg, 0)

				for _, m := range target.Metrics {
					if m.Type == metric.Type {
						otherMetrics = append(otherMetrics, m)
					}
				}

				if len(otherMetrics) > 1 {
					metricName += " " + metric.Field
					if metric.Type == "bucket_script" {
						// Use the formula in the column name
						metricName = metric.Settings.Get("script").MustString("")
					}
				}

				addMetricValue(&values, metricName, castToFloat(bucket.GetPath(metric.ID, "value")))
			}
		}

		table.Rows = append(table.Rows, values)
	}

	return nil
}

// TODO remove deprecations
// nolint:staticcheck // plugins.DataQueryResult deprecated
func (rp *responseParser) trimDatapoints(queryResult plugins.DataQueryResult, target *Query) {
	var histogram *BucketAgg
	for _, bucketAgg := range target.BucketAggs {
		if bucketAgg.Type == dateHistType {
			histogram = bucketAgg
			break
		}
	}

	if histogram == nil {
		return
	}

	trimEdges, err := histogram.Settings.Get("trimEdges").Int()
	if err != nil {
		return
	}

	frames, err := queryResult.Dataframes.Decoded()
	if err != nil {
		return
	}

	for _, frame := range frames {
		for _, field := range frame.Fields {
			if field.Len() > trimEdges*2 {
				for i := 0; i < field.Len(); i++ {
					if i < trimEdges || i > field.Len()-trimEdges {
						field.Delete(i)
					}
				}
			}
		}
	}
}

// nolint:staticcheck // plugins.DataQueryResult deprecated
func (rp *responseParser) nameFields(queryResult plugins.DataQueryResult, target *Query) {
	set := make(map[string]struct{})
	frames, err := queryResult.Dataframes.Decoded()
	if err != nil {
		return
	}
	for _, v := range frames {
		for _, vv := range v.Fields {
			if metricType, exists := vv.Labels["metric"]; exists {
				if _, ok := set[metricType]; !ok {
					set[metricType] = struct{}{}
				}
			}
		}
	}
	metricTypeCount := len(set)
	for i := range frames {
		frames[i].Name = rp.getFieldName(*frames[i].Fields[1], target, metricTypeCount)
	}
}

var aliasPatternRegex = regexp.MustCompile(`\{\{([\s\S]+?)\}\}`)

// nolint:staticcheck // plugins.* deprecated
func (rp *responseParser) getFieldName(dataField data.Field, target *Query, metricTypeCount int) string {
	metricType := dataField.Labels["metric"]
	metricName := rp.getMetricName(metricType)
	delete(dataField.Labels, "metric")

	field := ""
	if v, ok := dataField.Labels["field"]; ok {
		field = v
		delete(dataField.Labels, "field")
	}

	if target.Alias != "" {
		seriesName := target.Alias

		subMatches := aliasPatternRegex.FindAllStringSubmatch(target.Alias, -1)
		for _, subMatch := range subMatches {
			group := subMatch[0]

			if len(subMatch) > 1 {
				group = subMatch[1]
			}

			if strings.Index(group, "term ") == 0 {
				seriesName = strings.Replace(seriesName, subMatch[0], dataField.Labels[group[5:]], 1)
			}
			if v, ok := dataField.Labels[group]; ok {
				seriesName = strings.Replace(seriesName, subMatch[0], v, 1)
			}
			if group == "metric" {
				seriesName = strings.Replace(seriesName, subMatch[0], metricName, 1)
			}
			if group == "field" {
				seriesName = strings.Replace(seriesName, subMatch[0], field, 1)
			}
		}

		return seriesName
	}
	// todo, if field and pipelineAgg
	if field != "" && isPipelineAgg(metricType) {
		if isPipelineAggWithMultipleBucketPaths(metricType) {
			metricID := ""
			if v, ok := dataField.Labels["metricId"]; ok {
				metricID = v
			}

			for _, metric := range target.Metrics {
				if metric.ID == metricID {
					metricName = metric.Settings.Get("script").MustString()
					for name, pipelineAgg := range metric.PipelineVariables {
						for _, m := range target.Metrics {
							if m.ID == pipelineAgg {
								metricName = strings.ReplaceAll(metricName, "params."+name, describeMetric(m.Type, m.Field))
							}
						}
					}
				}
			}
		} else {
			found := false
			for _, metric := range target.Metrics {
				if metric.ID == field {
					metricName += " " + describeMetric(metric.Type, field)
					found = true
				}
			}
			if !found {
				metricName = "Unset"
			}
		}
	} else if field != "" {
		metricName += " " + field
	}

	delete(dataField.Labels, "metricId")

	if len(dataField.Labels) == 0 {
		return metricName
	}

	name := ""
	for _, v := range dataField.Labels {
		name += v + " "
	}

	if metricTypeCount == 1 {
		return strings.TrimSpace(name)
	}

	return strings.TrimSpace(name) + " " + metricName
}

func (rp *responseParser) getMetricName(metric string) string {
	if text, ok := metricAggType[metric]; ok {
		return text
	}

	if text, ok := extendedStats[metric]; ok {
		return text
	}

	return metric
}

func castToFloat(j *simplejson.Json) *float64 {
	f, err := j.Float64()
	if err == nil {
		return &f
	}

	if s, err := j.String(); err == nil {
		if strings.ToLower(s) == "nan" {
			return nil
		}

		if v, err := strconv.ParseFloat(s, 64); err == nil {
			return &v
		}
	}

	return nil
}

func findAgg(target *Query, aggID string) (*BucketAgg, error) {
	for _, v := range target.BucketAggs {
		if aggID == v.ID {
			return v, nil
		}
	}
	return nil, errors.New("can't found aggDef, aggID:" + aggID)
}

// nolint:staticcheck // plugins.DataQueryResult deprecated
func getErrorFromElasticResponse(response *es.SearchResponse) plugins.DataQueryResult {
	var result plugins.DataQueryResult
	json := simplejson.NewFromAny(response.Error)
	reason := json.Get("reason").MustString()
	rootCauseReason := json.Get("root_cause").GetIndex(0).Get("reason").MustString()

	switch {
	case rootCauseReason != "":
		result.ErrorString = rootCauseReason
	case reason != "":
		result.ErrorString = reason
	default:
		result.ErrorString = "Unknown elasticsearch error response"
	}

	return result
}
