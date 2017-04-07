package chunk

import (
	"fmt"
	"sort"

	"github.com/prometheus/common/model"
)

// compositeSchema is a Schema which delegates to various schemas depending
// on when they were activated.
type compositeSchema struct {
	schemas []compositeSchemaEntry
}

type compositeSchemaEntry struct {
	start model.Time
	Schema
}

type byStart []compositeSchemaEntry

func (a byStart) Len() int           { return len(a) }
func (a byStart) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byStart) Less(i, j int) bool { return a[i].start < a[j].start }

func newCompositeSchema(cfg SchemaConfig) (Schema, error) {
	schemas := []compositeSchemaEntry{
		{0, v1Schema(cfg)},
	}

	if cfg.DailyBucketsFrom.IsSet() {
		schemas = append(schemas, compositeSchemaEntry{cfg.DailyBucketsFrom.Time, v2Schema(cfg)})
	}

	if cfg.Base64ValuesFrom.IsSet() {
		schemas = append(schemas, compositeSchemaEntry{cfg.Base64ValuesFrom.Time, v3Schema(cfg)})
	}

	if cfg.V4SchemaFrom.IsSet() {
		schemas = append(schemas, compositeSchemaEntry{cfg.V4SchemaFrom.Time, v4Schema(cfg)})
	}

	if cfg.V5SchemaFrom.IsSet() {
		schemas = append(schemas, compositeSchemaEntry{cfg.V5SchemaFrom.Time, v5Schema(cfg)})
	}

	if cfg.V6SchemaFrom.IsSet() {
		schemas = append(schemas, compositeSchemaEntry{cfg.V6SchemaFrom.Time, v6Schema(cfg)})
	}

	if cfg.V7SchemaFrom.IsSet() {
		schemas = append(schemas, compositeSchemaEntry{cfg.V7SchemaFrom.Time, v7Schema(cfg)})
	}

	if !sort.IsSorted(byStart(schemas)) {
		return nil, fmt.Errorf("schemas not in time-sorted order")
	}

	return compositeSchema{schemas}, nil
}

func (c compositeSchema) forSchemasIndexQuery(from, through model.Time, callback func(from, through model.Time, schema Schema) ([]IndexQuery, error)) ([]IndexQuery, error) {
	if len(c.schemas) == 0 {
		return nil, nil
	}

	// first, find the schema with the highest start _before or at_ from
	i := sort.Search(len(c.schemas), func(i int) bool {
		return c.schemas[i].start > from
	})
	if i > 0 {
		i--
	} else {
		// This could happen if we get passed a sample from before 1970.
		i = 0
		from = c.schemas[0].start
	}

	// next, find the schema with the lowest start _after_ through
	j := sort.Search(len(c.schemas), func(j int) bool {
		return c.schemas[j].start > through
	})

	min := func(a, b model.Time) model.Time {
		if a < b {
			return a
		}
		return b
	}

	start := from
	result := []IndexQuery{}
	for ; i < j; i++ {
		nextSchemaStarts := model.Latest
		if i+1 < len(c.schemas) {
			nextSchemaStarts = c.schemas[i+1].start
		}

		// If the next schema starts at the same time as this one,
		// skip this one.
		if nextSchemaStarts == c.schemas[i].start {
			continue
		}

		end := min(through, nextSchemaStarts-1)
		entries, err := callback(start, end, c.schemas[i].Schema)
		if err != nil {
			return nil, err
		}

		result = append(result, entries...)
		start = nextSchemaStarts
	}

	return result, nil
}

func (c compositeSchema) forSchemasIndexEntry(from, through model.Time, callback func(from, through model.Time, schema Schema) ([]IndexEntry, error)) ([]IndexEntry, error) {
	if len(c.schemas) == 0 {
		return nil, nil
	}

	// first, find the schema with the highest start _before or at_ from
	i := sort.Search(len(c.schemas), func(i int) bool {
		return c.schemas[i].start > from
	})
	if i > 0 {
		i--
	} else {
		// This could happen if we get passed a sample from before 1970.
		i = 0
		from = c.schemas[0].start
	}

	// next, find the schema with the lowest start _after_ through
	j := sort.Search(len(c.schemas), func(j int) bool {
		return c.schemas[j].start > through
	})

	min := func(a, b model.Time) model.Time {
		if a < b {
			return a
		}
		return b
	}

	start := from
	result := []IndexEntry{}
	for ; i < j; i++ {
		nextSchemaStarts := model.Latest
		if i+1 < len(c.schemas) {
			nextSchemaStarts = c.schemas[i+1].start
		}

		// If the next schema starts at the same time as this one,
		// skip this one.
		if nextSchemaStarts == c.schemas[i].start {
			continue
		}

		end := min(through, nextSchemaStarts-1)
		entries, err := callback(start, end, c.schemas[i].Schema)
		if err != nil {
			return nil, err
		}

		result = append(result, entries...)
		start = nextSchemaStarts
	}

	return result, nil
}

func (c compositeSchema) GetWriteEntries(from, through model.Time, userID string, metricName model.LabelValue, labels model.Metric, chunkID string) ([]IndexEntry, error) {
	return c.forSchemasIndexEntry(from, through, func(from, through model.Time, schema Schema) ([]IndexEntry, error) {
		return schema.GetWriteEntries(from, through, userID, metricName, labels, chunkID)
	})
}

func (c compositeSchema) GetReadQueries(from, through model.Time, userID string) ([]IndexQuery, error) {
	return c.forSchemasIndexQuery(from, through, func(from, through model.Time, schema Schema) ([]IndexQuery, error) {
		return schema.GetReadQueries(from, through, userID)
	})
}

func (c compositeSchema) GetReadQueriesForMetric(from, through model.Time, userID string, metricName model.LabelValue) ([]IndexQuery, error) {
	return c.forSchemasIndexQuery(from, through, func(from, through model.Time, schema Schema) ([]IndexQuery, error) {
		return schema.GetReadQueriesForMetric(from, through, userID, metricName)
	})
}

func (c compositeSchema) GetReadQueriesForMetricLabel(from, through model.Time, userID string, metricName model.LabelValue, labelName model.LabelName) ([]IndexQuery, error) {
	return c.forSchemasIndexQuery(from, through, func(from, through model.Time, schema Schema) ([]IndexQuery, error) {
		return schema.GetReadQueriesForMetricLabel(from, through, userID, metricName, labelName)
	})
}

func (c compositeSchema) GetReadQueriesForMetricLabelValue(from, through model.Time, userID string, metricName model.LabelValue, labelName model.LabelName, labelValue model.LabelValue) ([]IndexQuery, error) {
	return c.forSchemasIndexQuery(from, through, func(from, through model.Time, schema Schema) ([]IndexQuery, error) {
		return schema.GetReadQueriesForMetricLabelValue(from, through, userID, metricName, labelName, labelValue)
	})
}
