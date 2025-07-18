package tsdb

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/thanos-io/thanos/pkg/cache"
	"github.com/thanos-io/thanos/pkg/cacheutil"
)

type multiLevelBucketCache struct {
	name   string
	caches []cache.Cache

	backfillProcessor    *cacheutil.AsyncOperationProcessor
	fetchLatency         *prometheus.HistogramVec
	backFillLatency      *prometheus.HistogramVec
	storeDroppedItems    prometheus.Counter
	backfillDroppedItems prometheus.Counter
	maxBackfillItems     int
	backfillTTL          time.Duration
}

type MultiLevelBucketCacheConfig struct {
	MaxAsyncConcurrency int `yaml:"max_async_concurrency"`
	MaxAsyncBufferSize  int `yaml:"max_async_buffer_size"`
	MaxBackfillItems    int `yaml:"max_backfill_items"`

	BackFillTTL time.Duration `yaml:"-"`
}

func (cfg *MultiLevelBucketCacheConfig) Validate() error {
	if cfg.MaxAsyncBufferSize <= 0 {
		return errInvalidMaxAsyncBufferSize
	}
	if cfg.MaxAsyncConcurrency <= 0 {
		return errInvalidMaxAsyncConcurrency
	}
	if cfg.MaxBackfillItems <= 0 {
		return errInvalidMaxBackfillItems
	}
	return nil
}

func (cfg *MultiLevelBucketCacheConfig) RegisterFlagsWithPrefix(f *flag.FlagSet, prefix string) {
	f.IntVar(&cfg.MaxAsyncConcurrency, prefix+"max-async-concurrency", 3, "The maximum number of concurrent asynchronous operations can occur when backfilling cache items.")
	f.IntVar(&cfg.MaxAsyncBufferSize, prefix+"max-async-buffer-size", 10000, "The maximum number of enqueued asynchronous operations allowed when backfilling cache items.")
	f.IntVar(&cfg.MaxBackfillItems, prefix+"max-backfill-items", 10000, "The maximum number of items to backfill per asynchronous operation.")
}

func newMultiLevelBucketCache(name string, cfg MultiLevelBucketCacheConfig, reg prometheus.Registerer, c ...cache.Cache) cache.Cache {
	if len(c) == 1 {
		return c[0]
	}

	itemName := ""
	metricHelpText := ""
	switch name {
	case "chunks-cache":
		itemName = "chunks_cache"
		metricHelpText = "chunks cache"
	case "metadata-cache":
		itemName = "metadata_cache"
		metricHelpText = "metadata cache"
	case "parquet-labels-cache":
		itemName = "parquet_labels_cache"
		metricHelpText = "parquet labels cache"
	default:
		itemName = name
	}

	return &multiLevelBucketCache{
		name:              name,
		caches:            c,
		backfillProcessor: cacheutil.NewAsyncOperationProcessor(cfg.MaxAsyncBufferSize, cfg.MaxAsyncConcurrency),
		fetchLatency: promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
			Name:    fmt.Sprintf("cortex_store_multilevel_%s_fetch_duration_seconds", itemName),
			Help:    fmt.Sprintf("Histogram to track latency to fetch items from multi level %s", metricHelpText),
			Buckets: []float64{0.01, 0.1, 0.3, 0.6, 1, 3, 6, 10, 15, 20, 25, 30, 40, 50, 60, 90},
		}, nil),
		backFillLatency: promauto.With(reg).NewHistogramVec(prometheus.HistogramOpts{
			Name:    fmt.Sprintf("cortex_store_multilevel_%s_backfill_duration_seconds", itemName),
			Help:    fmt.Sprintf("Histogram to track latency to backfill items from multi level %s", metricHelpText),
			Buckets: []float64{0.01, 0.1, 0.3, 0.6, 1, 3, 6, 10, 15, 20, 25, 30, 40, 50, 60, 90},
		}, nil),
		storeDroppedItems: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: fmt.Sprintf("cortex_store_multilevel_%s_backfill_dropped_items_total", itemName),
			Help: fmt.Sprintf("Total number of items dropped due to async buffer full when backfilling multilevel %s", metricHelpText),
		}),
		backfillDroppedItems: promauto.With(reg).NewCounter(prometheus.CounterOpts{
			Name: fmt.Sprintf("cortex_store_multilevel_%s_store_dropped_items_total", itemName),
			Help: fmt.Sprintf("Total number of items dropped due to async buffer full when storing multilevel %s", metricHelpText),
		}),
		maxBackfillItems: cfg.MaxBackfillItems,
		backfillTTL:      cfg.BackFillTTL,
	}
}

func (m *multiLevelBucketCache) Store(data map[string][]byte, ttl time.Duration) {
	for _, c := range m.caches {
		if err := m.backfillProcessor.EnqueueAsync(func() {
			c.Store(data, ttl)
		}); errors.Is(err, cacheutil.ErrAsyncBufferFull) {
			m.storeDroppedItems.Inc()
		}
	}
}

func (m *multiLevelBucketCache) Fetch(ctx context.Context, keys []string) map[string][]byte {
	timer := prometheus.NewTimer(m.fetchLatency.WithLabelValues())
	defer timer.ObserveDuration()

	missingKeys := keys
	hits := map[string][]byte{}
	backfillItems := make([]map[string][]byte, len(m.caches)-1)

	for i, c := range m.caches {
		if i < len(m.caches)-1 {
			backfillItems[i] = map[string][]byte{}
		}
		if ctx.Err() != nil {
			return nil
		}
		if data := c.Fetch(ctx, missingKeys); len(data) > 0 {
			for k, d := range data {
				hits[k] = d
			}

			if i > 0 && len(hits) > 0 {
				// lets fetch only the mising keys
				m := missingKeys[:0]
				for _, key := range missingKeys {
					if _, ok := hits[key]; !ok {
						m = append(m, key)
					}
				}

				missingKeys = m

				for k, b := range hits {
					backfillItems[i-1][k] = b
				}
			}

			if len(hits) == len(keys) {
				// fetch done
				break
			}
		}
	}

	defer func() {
		backFillTimer := prometheus.NewTimer(m.backFillLatency.WithLabelValues())
		defer backFillTimer.ObserveDuration()

		for i, values := range backfillItems {
			if len(values) == 0 {
				continue
			}

			if err := m.backfillProcessor.EnqueueAsync(func() {
				m.caches[i].Store(values, m.backfillTTL)
			}); errors.Is(err, cacheutil.ErrAsyncBufferFull) {
				m.backfillDroppedItems.Inc()
			}
		}
	}()

	return hits
}

func (m *multiLevelBucketCache) Name() string {
	return m.name
}
