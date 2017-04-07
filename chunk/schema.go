package chunk

import (
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"

	"github.com/weaveworks/cortex/util"
)

var (
	rangeKeyV1 = []byte{'1'}
	rangeKeyV2 = []byte{'2'}
	rangeKeyV3 = []byte{'3'}
	rangeKeyV4 = []byte{'4'}
	rangeKeyV5 = []byte{'5'}
	rangeKeyV6 = []byte{'6'}
)

// Schema interface defines methods to calculate the hash and range keys needed
// to write or read chunks from the external index.
type Schema interface {
	// When doing a write, use this method to return the list of entries you should write to.
	GetWriteEntries(from, through model.Time, userID string, metricName model.LabelValue, labels model.Metric, chunkID string) ([]IndexEntry, error)

	// When doing a read, use these methods to return the list of entries you should query
	GetReadQueries(from, through model.Time, userID string) ([]IndexQuery, error)
	GetReadQueriesForMetric(from, through model.Time, userID string, metricName model.LabelValue) ([]IndexQuery, error)
	GetReadQueriesForMetricLabel(from, through model.Time, userID string, metricName model.LabelValue, labelName model.LabelName) ([]IndexQuery, error)
	GetReadQueriesForMetricLabelValue(from, through model.Time, userID string, metricName model.LabelValue, labelName model.LabelName, labelValue model.LabelValue) ([]IndexQuery, error)
}

// IndexQuery describes a query for entries
type IndexQuery struct {
	TableName string
	HashValue string

	// One of RangeValuePrefix or RangeValueStart might be set:
	// - If RangeValuePrefix is not nil, must read all keys with that prefix.
	// - If RangeValueStart is not nil, must read all keys from there onwards.
	// - If neither is set, must read all keys for that row.
	RangeValuePrefix []byte
	RangeValueStart  []byte
}

// IndexEntry describes an entry in the chunk index
type IndexEntry struct {
	TableName string
	HashValue string

	// For writes, RangeValue will always be set.
	RangeValue []byte

	// New for v6 schema, label value is not written as part of the range key.
	Value []byte
}

// SchemaConfig contains the config for our chunk index schemas
type SchemaConfig struct {
	PeriodicTableConfig
	OriginalTableName string

	// After midnight on this day, we start bucketing indexes by day instead of by
	// hour.  Only the day matters, not the time within the day.
	DailyBucketsFrom util.DayValue

	// After this time, we will only query for base64-encoded label values.
	Base64ValuesFrom util.DayValue

	// After this time, we will read and write v4 schemas.
	V4SchemaFrom util.DayValue

	// After this time, we will read and write v5 schemas.
	V5SchemaFrom util.DayValue

	// After this time, we will read and write v6 schemas.
	V6SchemaFrom util.DayValue

	// After this time, we will read and write v7 schemas.
	V7SchemaFrom util.DayValue
}

// RegisterFlags adds the flags required to config this to the given FlagSet
func (cfg *SchemaConfig) RegisterFlags(f *flag.FlagSet) {
	cfg.PeriodicTableConfig.RegisterFlags(f)

	flag.StringVar(&cfg.OriginalTableName, "dynamodb.original-table-name", "", "The name of the DynamoDB table used before versioned schemas were introduced.")
	f.Var(&cfg.DailyBucketsFrom, "dynamodb.daily-buckets-from", "The date (in the format YYYY-MM-DD) of the first day for which DynamoDB index buckets should be day-sized vs. hour-sized.")
	f.Var(&cfg.Base64ValuesFrom, "dynamodb.base64-buckets-from", "The date (in the format YYYY-MM-DD) after which we will stop querying to non-base64 encoded values.")
	f.Var(&cfg.V4SchemaFrom, "dynamodb.v4-schema-from", "The date (in the format YYYY-MM-DD) after which we enable v4 schema.")
	f.Var(&cfg.V5SchemaFrom, "dynamodb.v5-schema-from", "The date (in the format YYYY-MM-DD) after which we enable v5 schema.")
	f.Var(&cfg.V6SchemaFrom, "dynamodb.v6-schema-from", "The date (in the format YYYY-MM-DD) after which we enable v6 schema.")
	f.Var(&cfg.V7SchemaFrom, "dynamodb.v7-schema-from", "The date (in the format YYYY-MM-DD) after which we enable v7 schema.")
}

// v1Schema was:
// - hash key: <userid>:<hour bucket>:<metric name>
// - range key: <label name>\0<label value>\0<chunk name>
func v1Schema(cfg SchemaConfig) Schema {
	return schema{
		cfg.hourlyBuckets,
		originalEntries{},
	}
}

// v2Schema went to daily buckets in the hash key
// - hash key: <userid>:d<day bucket>:<metric name>
func v2Schema(cfg SchemaConfig) Schema {
	return schema{
		cfg.dailyBuckets,
		originalEntries{},
	}
}

// v3Schema went to base64 encoded label values & a version ID
// - range key: <label name>\0<base64(label value)>\0<chunk name>\0<version 1>
func v3Schema(cfg SchemaConfig) Schema {
	return schema{
		cfg.dailyBuckets,
		base64Entries{originalEntries{}},
	}
}

// v4 schema went to two schemas in one:
// 1) - hash key: <userid>:<hour bucket>:<metric name>:<label name>
//    - range key: \0<base64(label value)>\0<chunk name>\0<version 2>
// 2) - hash key: <userid>:<hour bucket>:<metric name>
//    - range key: \0\0<chunk name>\0<version 3>
func v4Schema(cfg SchemaConfig) Schema {
	return schema{
		cfg.dailyBuckets,
		labelNameInHashKeyEntries{},
	}
}

// v5 schema is an extension of v4, with the chunk end time in the
// range key to improve query latency.  However, it did it wrong
// so the chunk end times are ignored.
func v5Schema(cfg SchemaConfig) Schema {
	return schema{
		cfg.dailyBuckets,
		v5Entries{},
	}
}

// v6 schema is an extension of v5, with correct chunk end times, and
// the label value moved out of the range key.
func v6Schema(cfg SchemaConfig) Schema {
	return schema{
		cfg.dailyBuckets,
		v6Entries{},
	}
}

// v7 schema is an extension of v6, with support for queries with no metric names
func v7Schema(cfg SchemaConfig) Schema {
	return schema{
		cfg.dailyBuckets,
		v7Entries{},
	}
}

// schema implements Schema given a bucketing function and and set of range key callbacks
type schema struct {
	buckets func(from, through model.Time, userID string) []Bucket
	entries entries
}

func (s schema) GetWriteEntries(from, through model.Time, userID string, metricName model.LabelValue, labels model.Metric, chunkID string) ([]IndexEntry, error) {
	var result []IndexEntry

	buckets := s.buckets(from, through, userID)
	for _, bucket := range buckets {
		entries, err := s.entries.GetWriteEntries(bucket.from, bucket.through, bucket.tableName, bucket.hashKey, metricName, labels, chunkID)
		if err != nil {
			return nil, err
		}
		result = append(result, entries...)
	}
	return result, nil
}

func (s schema) GetReadQueries(from, through model.Time, userID string) ([]IndexQuery, error) {
	var result []IndexQuery

	buckets := s.buckets(from, through, userID)
	for _, bucket := range buckets {
		entries, err := s.entries.GetReadQueries(bucket.from, bucket.through, bucket.tableName, bucket.hashKey)
		if err != nil {
			return nil, err
		}
		result = append(result, entries...)
	}
	return result, nil
}

func (s schema) GetReadQueriesForMetric(from, through model.Time, userID string, metricName model.LabelValue) ([]IndexQuery, error) {
	var result []IndexQuery

	buckets := s.buckets(from, through, userID)
	for _, bucket := range buckets {
		entries, err := s.entries.GetReadMetricQueries(bucket.from, bucket.through, bucket.tableName, bucket.hashKey, metricName)
		if err != nil {
			return nil, err
		}
		result = append(result, entries...)
	}
	return result, nil
}

func (s schema) GetReadQueriesForMetricLabel(from, through model.Time, userID string, metricName model.LabelValue, labelName model.LabelName) ([]IndexQuery, error) {
	var result []IndexQuery

	buckets := s.buckets(from, through, userID)
	for _, bucket := range buckets {
		entries, err := s.entries.GetReadMetricLabelQueries(bucket.from, bucket.through, bucket.tableName, bucket.hashKey, metricName, labelName)
		if err != nil {
			return nil, err
		}
		result = append(result, entries...)
	}
	return result, nil
}

func (s schema) GetReadQueriesForMetricLabelValue(from, through model.Time, userID string, metricName model.LabelValue, labelName model.LabelName, labelValue model.LabelValue) ([]IndexQuery, error) {
	var result []IndexQuery

	buckets := s.buckets(from, through, userID)
	for _, bucket := range buckets {
		entries, err := s.entries.GetReadMetricLabelValueQueries(bucket.from, bucket.through, bucket.tableName, bucket.hashKey, metricName, labelName, labelValue)
		if err != nil {
			return nil, err
		}
		result = append(result, entries...)
	}
	return result, nil
}

type entries interface {
	GetWriteEntries(from, through uint32, tableName, hashKey string, metricName model.LabelValue, labels model.Metric, chunkID string) ([]IndexEntry, error)
	GetReadQueries(from, through uint32, tableName, hashKey string) ([]IndexQuery, error)
	GetReadMetricQueries(from, through uint32, tableName, hashKey string, metricName model.LabelValue) ([]IndexQuery, error)
	GetReadMetricLabelQueries(from, through uint32, tableName, hashKey string, metricName model.LabelValue, labelName model.LabelName) ([]IndexQuery, error)
	GetReadMetricLabelValueQueries(from, through uint32, tableName, hashKey string, metricName model.LabelValue, labelName model.LabelName, labelValue model.LabelValue) ([]IndexQuery, error)
}

type originalEntries struct{}

func (originalEntries) GetWriteEntries(_, _ uint32, tableName, bucketHashKey string, metricName model.LabelValue, labels model.Metric, chunkID string) ([]IndexEntry, error) {
	chunkIDBytes := []byte(chunkID)
	result := []IndexEntry{}
	for key, value := range labels {
		if key == model.MetricNameLabel {
			continue
		}
		if strings.ContainsRune(string(value), '\x00') {
			return nil, fmt.Errorf("label values cannot contain null byte")
		}
		result = append(result, IndexEntry{
			TableName:  tableName,
			HashValue:  bucketHashKey + ":" + string(metricName),
			RangeValue: buildRangeKey([]byte(key), []byte(value), chunkIDBytes),
		})
	}
	return result, nil
}

func (originalEntries) GetReadQueries(_, _ uint32, _, _ string) ([]IndexQuery, error) {
	return nil, fmt.Errorf("originalEntries does not support GetReadQueries")
}

func (originalEntries) GetReadMetricQueries(_, _ uint32, tableName, bucketHashKey string, metricName model.LabelValue) ([]IndexQuery, error) {
	return []IndexQuery{
		{
			TableName:        tableName,
			HashValue:        bucketHashKey + ":" + string(metricName),
			RangeValuePrefix: nil,
		},
	}, nil
}

func (originalEntries) GetReadMetricLabelQueries(_, _ uint32, tableName, bucketHashKey string, metricName model.LabelValue, labelName model.LabelName) ([]IndexQuery, error) {
	return []IndexQuery{
		{
			TableName:        tableName,
			HashValue:        bucketHashKey + ":" + string(metricName),
			RangeValuePrefix: buildRangeKey([]byte(labelName)),
		},
	}, nil
}

func (originalEntries) GetReadMetricLabelValueQueries(_, _ uint32, tableName, bucketHashKey string, metricName model.LabelValue, labelName model.LabelName, labelValue model.LabelValue) ([]IndexQuery, error) {
	if strings.ContainsRune(string(labelValue), '\x00') {
		return nil, fmt.Errorf("label values cannot contain null byte")
	}
	return []IndexQuery{
		{
			TableName:        tableName,
			HashValue:        bucketHashKey + ":" + string(metricName),
			RangeValuePrefix: buildRangeKey([]byte(labelName), []byte(labelValue)),
		},
	}, nil
}

type base64Entries struct {
	originalEntries
}

func (base64Entries) GetWriteEntries(_, _ uint32, tableName, bucketHashKey string, metricName model.LabelValue, labels model.Metric, chunkID string) ([]IndexEntry, error) {
	chunkIDBytes := []byte(chunkID)
	result := []IndexEntry{}
	for key, value := range labels {
		if key == model.MetricNameLabel {
			continue
		}

		encodedBytes := encodeBase64Value(value)
		result = append(result, IndexEntry{
			TableName:  tableName,
			HashValue:  bucketHashKey + ":" + string(metricName),
			RangeValue: buildRangeKey([]byte(key), encodedBytes, chunkIDBytes, rangeKeyV1),
		})
	}
	return result, nil
}

func (base64Entries) GetReadQueries(_, _ uint32, _, _ string) ([]IndexQuery, error) {
	return nil, fmt.Errorf("base64Entries does not support GetReadQueries")
}

func (base64Entries) GetReadMetricLabelValueQueries(_, _ uint32, tableName, bucketHashKey string, metricName model.LabelValue, labelName model.LabelName, labelValue model.LabelValue) ([]IndexQuery, error) {
	encodedBytes := encodeBase64Value(labelValue)
	return []IndexQuery{
		{
			TableName:        tableName,
			HashValue:        bucketHashKey + ":" + string(metricName),
			RangeValuePrefix: buildRangeKey([]byte(labelName), encodedBytes),
		},
	}, nil
}

type labelNameInHashKeyEntries struct{}

func (labelNameInHashKeyEntries) GetWriteEntries(_, _ uint32, tableName, bucketHashKey string, metricName model.LabelValue, labels model.Metric, chunkID string) ([]IndexEntry, error) {
	chunkIDBytes := []byte(chunkID)
	entries := []IndexEntry{
		{
			TableName:  tableName,
			HashValue:  bucketHashKey + ":" + string(metricName),
			RangeValue: buildRangeKey(nil, nil, chunkIDBytes, rangeKeyV2),
		},
	}

	for key, value := range labels {
		if key == model.MetricNameLabel {
			continue
		}
		encodedBytes := encodeBase64Value(value)
		entries = append(entries, IndexEntry{
			TableName:  tableName,
			HashValue:  bucketHashKey + ":" + string(metricName) + ":" + string(key),
			RangeValue: buildRangeKey(nil, encodedBytes, chunkIDBytes, rangeKeyV1),
		})
	}

	return entries, nil
}

func (labelNameInHashKeyEntries) GetReadQueries(_, _ uint32, _, _ string) ([]IndexQuery, error) {
	return nil, fmt.Errorf("labelNameInHashKeyEntries does not support GetReadQueries")
}

func (labelNameInHashKeyEntries) GetReadMetricQueries(_, _ uint32, tableName, bucketHashKey string, metricName model.LabelValue) ([]IndexQuery, error) {
	return []IndexQuery{
		{
			TableName: tableName,
			HashValue: bucketHashKey + ":" + string(metricName),
		},
	}, nil
}

func (labelNameInHashKeyEntries) GetReadMetricLabelQueries(_, _ uint32, tableName, bucketHashKey string, metricName model.LabelValue, labelName model.LabelName) ([]IndexQuery, error) {
	return []IndexQuery{
		{
			TableName: tableName,
			HashValue: bucketHashKey + ":" + string(metricName) + ":" + string(labelName),
		},
	}, nil
}

func (labelNameInHashKeyEntries) GetReadMetricLabelValueQueries(_, _ uint32, tableName, bucketHashKey string, metricName model.LabelValue, labelName model.LabelName, labelValue model.LabelValue) ([]IndexQuery, error) {
	encodedBytes := encodeBase64Value(labelValue)
	return []IndexQuery{
		{
			TableName:        tableName,
			HashValue:        bucketHashKey + ":" + string(metricName) + ":" + string(labelName),
			RangeValuePrefix: buildRangeKey(nil, encodedBytes),
		},
	}, nil
}

// v5Entries includes chunk end time in range key - see #298.
type v5Entries struct{}

func encodeTime(t uint32) []byte {
	// timestamps are hex encoded such that it doesn't contain null byte,
	// but is still lexicographically sortable.
	throughBytes := make([]byte, 4, 4)
	binary.BigEndian.PutUint32(throughBytes, t)
	encodedThroughBytes := make([]byte, 8, 8)
	hex.Encode(encodedThroughBytes, throughBytes)
	return encodedThroughBytes
}

func decodeTime(bs []byte) uint32 {
	buf := make([]byte, 4, 4)
	hex.Decode(buf, bs)
	return binary.BigEndian.Uint32(buf)
}

func (v5Entries) GetWriteEntries(_, through uint32, tableName, bucketHashKey string, metricName model.LabelValue, labels model.Metric, chunkID string) ([]IndexEntry, error) {
	chunkIDBytes := []byte(chunkID)
	encodedThroughBytes := encodeTime(through)

	entries := []IndexEntry{
		{
			TableName:  tableName,
			HashValue:  bucketHashKey + ":" + string(metricName),
			RangeValue: buildRangeKey(encodedThroughBytes, nil, chunkIDBytes, rangeKeyV3),
		},
	}

	for key, value := range labels {
		if key == model.MetricNameLabel {
			continue
		}
		encodedValueBytes := encodeBase64Value(value)
		entries = append(entries, IndexEntry{
			TableName:  tableName,
			HashValue:  bucketHashKey + ":" + string(metricName) + ":" + string(key),
			RangeValue: buildRangeKey(encodedThroughBytes, encodedValueBytes, chunkIDBytes, rangeKeyV4),
		})
	}

	return entries, nil
}

func (v5Entries) GetReadQueries(_, _ uint32, _, _ string) ([]IndexQuery, error) {
	return nil, fmt.Errorf("v5Entries does not support GetReadQueries")
}

func (v5Entries) GetReadMetricQueries(_, _ uint32, tableName, bucketHashKey string, metricName model.LabelValue) ([]IndexQuery, error) {
	return []IndexQuery{
		{
			TableName: tableName,
			HashValue: bucketHashKey + ":" + string(metricName),
		},
	}, nil
}

func (v5Entries) GetReadMetricLabelQueries(_, _ uint32, tableName, bucketHashKey string, metricName model.LabelValue, labelName model.LabelName) ([]IndexQuery, error) {
	return []IndexQuery{
		{
			TableName: tableName,
			HashValue: bucketHashKey + ":" + string(metricName) + ":" + string(labelName),
		},
	}, nil
}

func (v5Entries) GetReadMetricLabelValueQueries(_, _ uint32, tableName, bucketHashKey string, metricName model.LabelValue, labelName model.LabelName, _ model.LabelValue) ([]IndexQuery, error) {
	return []IndexQuery{
		{
			TableName: tableName,
			HashValue: bucketHashKey + ":" + string(metricName) + ":" + string(labelName),
		},
	}, nil
}

// v6Entries fixes issues with v5 time encoding being wrong (see #337), and
// moves label value out of range key (see #199).
type v6Entries struct{}

func (v6Entries) GetWriteEntries(_, through uint32, tableName, bucketHashKey string, metricName model.LabelValue, labels model.Metric, chunkID string) ([]IndexEntry, error) {
	chunkIDBytes := []byte(chunkID)
	encodedThroughBytes := encodeTime(through)

	entries := []IndexEntry{
		{
			TableName:  tableName,
			HashValue:  bucketHashKey + ":" + string(metricName),
			RangeValue: buildRangeKey(encodedThroughBytes, nil, chunkIDBytes, rangeKeyV3),
		},
	}

	for key, value := range labels {
		if key == model.MetricNameLabel {
			continue
		}
		entries = append(entries, IndexEntry{
			TableName:  tableName,
			HashValue:  bucketHashKey + ":" + string(metricName) + ":" + string(key),
			RangeValue: buildRangeKey(encodedThroughBytes, nil, chunkIDBytes, rangeKeyV5),
			Value:      []byte(value),
		})
	}

	return entries, nil
}

func (v6Entries) GetReadQueries(_, _ uint32, _, _ string) ([]IndexQuery, error) {
	return nil, fmt.Errorf("v6Entries does not support GetReadQueries")
}

func (v6Entries) GetReadMetricQueries(from, _ uint32, tableName, bucketHashKey string, metricName model.LabelValue) ([]IndexQuery, error) {
	encodedFromBytes := encodeTime(from)
	return []IndexQuery{
		{
			TableName:       tableName,
			HashValue:       bucketHashKey + ":" + string(metricName),
			RangeValueStart: buildRangeKey(encodedFromBytes),
		},
	}, nil
}

func (v6Entries) GetReadMetricLabelQueries(from, _ uint32, tableName, bucketHashKey string, metricName model.LabelValue, labelName model.LabelName) ([]IndexQuery, error) {
	encodedFromBytes := encodeTime(from)
	return []IndexQuery{
		{
			TableName:       tableName,
			HashValue:       bucketHashKey + ":" + string(metricName) + ":" + string(labelName),
			RangeValueStart: buildRangeKey(encodedFromBytes),
		},
	}, nil
}

func (v6Entries) GetReadMetricLabelValueQueries(from, _ uint32, tableName, bucketHashKey string, metricName model.LabelValue, labelName model.LabelName, labelValue model.LabelValue) ([]IndexQuery, error) {
	encodedFromBytes := encodeTime(from)
	return []IndexQuery{
		{
			TableName:       tableName,
			HashValue:       bucketHashKey + ":" + string(metricName) + ":" + string(labelName),
			RangeValueStart: buildRangeKey(encodedFromBytes),
		},
	}, nil
}

// v7Entries supports queries with no metric name
type v7Entries struct {
	v6Entries
}

func (v7Entries) GetWriteEntries(_, through uint32, tableName, bucketHashKey string, metricName model.LabelValue, labels model.Metric, chunkID string) ([]IndexEntry, error) {
	metricName, err := util.ExtractMetricNameFromMetric(labels)
	if err != nil {
		return nil, err
	}

	chunkIDBytes := []byte(chunkID)
	encodedThroughBytes := encodeTime(through)
	metricNameHashBytes := sha1.Sum([]byte(metricName))

	// Add IndexEntry with userID:bigBucket HashValue
	entries := []IndexEntry{
		{
			TableName:  tableName,
			HashValue:  bucketHashKey,
			RangeValue: buildRangeKey(nil, nil, metricNameHashBytes[:], rangeKeyV6),
			Value:      []byte(metricName),
		},
	}

	// Add IndexEntry with userID:bigBucket:metricName HashValue
	entries = append(entries, IndexEntry{
		TableName:  tableName,
		HashValue:  bucketHashKey + ":" + string(metricName),
		RangeValue: buildRangeKey(encodedThroughBytes, nil, chunkIDBytes, rangeKeyV3),
	})

	// Add IndexEntries with userID:bigBucket:metricName:labelName HashValue
	for key, value := range labels {
		if key == model.MetricNameLabel {
			continue
		}
		entries = append(entries, IndexEntry{
			TableName:  tableName,
			HashValue:  bucketHashKey + ":" + string(metricName) + ":" + string(key),
			RangeValue: buildRangeKey(encodedThroughBytes, nil, chunkIDBytes, rangeKeyV5),
			Value:      []byte(value),
		})
	}

	return entries, nil
}

func (v7Entries) GetReadQueries(from, _ uint32, tableName, bucketHashKey string) ([]IndexQuery, error) {
	return []IndexQuery{
		{
			TableName: tableName,
			HashValue: bucketHashKey,
		},
	}, nil
}

const (
	secondsInHour      = int64(time.Hour / time.Second)
	secondsInDay       = int64(24 * time.Hour / time.Second)
	millisecondsInHour = int64(time.Hour / time.Millisecond)
	millisecondsInDay  = int64(24 * time.Hour / time.Millisecond)
)

func (cfg *SchemaConfig) tableForBucket(bucketStart int64) string {
	if !cfg.UsePeriodicTables || bucketStart < (cfg.PeriodicTableStartAt.Unix()) {
		return cfg.OriginalTableName
	}
	// TODO remove reference to time package here
	return cfg.TablePrefix + strconv.Itoa(int(bucketStart/int64(cfg.TablePeriod/time.Second)))
}

// Bucket is a range of time with a tableName and a hashKey
type Bucket struct {
	from      uint32
	through   uint32
	tableName string
	hashKey   string
}

func (cfg SchemaConfig) hourlyBuckets(from, through model.Time, userID string) []Bucket {
	var (
		fromHour    = from.Unix() / secondsInHour
		throughHour = through.Unix() / secondsInHour
		result      = []Bucket{}
	)

	for i := fromHour; i <= throughHour; i++ {
		relativeFrom := util.Max64(0, int64(from)-(i*millisecondsInHour))
		relativeThrough := util.Min64(millisecondsInHour, int64(through)-(i*millisecondsInDay))
		result = append(result, Bucket{
			from:      uint32(relativeFrom),
			through:   uint32(relativeThrough),
			tableName: cfg.tableForBucket(i * secondsInHour),
			hashKey:   fmt.Sprintf("%s:%d", userID, i),
		})
	}
	return result
}

func (cfg SchemaConfig) dailyBuckets(from, through model.Time, userID string) []Bucket {
	var (
		fromDay    = from.Unix() / secondsInDay
		throughDay = through.Unix() / secondsInDay
		result     = []Bucket{}
	)

	for i := fromDay; i <= throughDay; i++ {
		// The idea here is that the hash key contains the bucket start time (rounded to
		// the nearest day).  The range key can contain the offset from that, to the
		// (start/end) of the chunk. For chunks that span multiple buckets, these
		// offsets will be capped to the bucket boundaries, i.e. start will be
		// positive in the first bucket, then zero in the next etc.
		//
		// The reason for doing all this is to reduce the size of the time stamps we
		// include in the range keys - we use a uint32 - as we then have to base 32
		// encode it.

		relativeFrom := util.Max64(0, int64(from)-(i*millisecondsInDay))
		relativeThrough := util.Min64(millisecondsInDay, int64(through)-(i*millisecondsInDay))
		result = append(result, Bucket{
			from:      uint32(relativeFrom),
			through:   uint32(relativeThrough),
			tableName: cfg.tableForBucket(i * secondsInDay),
			hashKey:   fmt.Sprintf("%s:d%d", userID, i),
		})
	}
	return result
}
