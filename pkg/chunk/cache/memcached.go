package cache

import (
	"context"
	"encoding/hex"
	"flag"
	"hash/fnv"
	"sync"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/go-kit/kit/log/level"
	opentracing "github.com/opentracing/opentracing-go"
	otlog "github.com/opentracing/opentracing-go/log"
	"github.com/prometheus/client_golang/prometheus"
	instr "github.com/weaveworks/common/instrument"
	"github.com/weaveworks/cortex/pkg/util"
)

var (
	memcacheRequestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "cortex",
		Name:      "memcache_request_duration_seconds",
		Help:      "Total time spent in seconds doing memcache requests.",
		// Memecache requests are very quick: smallest bucket is 16us, biggest is 1s
		Buckets: prometheus.ExponentialBuckets(0.000016, 4, 8),
	}, []string{"method", "status_code"})
)

func init() {
	prometheus.MustRegister(memcacheRequestDuration)
}

// MemcachedConfig is config to make a Memcached
type MemcachedConfig struct {
	Expiration time.Duration

	BatchSize   int
	Parallelism int
}

// RegisterFlags adds the flags required to config this to the given FlagSet
func (cfg *MemcachedConfig) RegisterFlags(f *flag.FlagSet) {
	cfg.RegisterFlagsWithPrefix("", "", f)
}

// RegisterFlagsWithPrefix adds the flags required to config this to the given FlagSet
func (cfg *MemcachedConfig) RegisterFlagsWithPrefix(prefix, description string, f *flag.FlagSet) {
	if prefix != "" {
		prefix += "."
	}

	f.DurationVar(&cfg.Expiration, prefix+"memcached.expiration", 0, description+"How long keys stay in the memcache.")
	f.IntVar(&cfg.BatchSize, prefix+"memcached.batchsize", 0, description+"How many keys to fetch in each batch.")
	f.IntVar(&cfg.Parallelism, prefix+"memcached.parallelism", 100, description+"Maximum active requests to memcache.")
}

// Memcached type caches chunks in memcached
type Memcached struct {
	cfg      MemcachedConfig
	memcache MemcachedClient

	wg      sync.WaitGroup
	inputCh chan *work
}

// NewMemcached makes a new Memcache
func NewMemcached(cfg MemcachedConfig, client MemcachedClient) *Memcached {
	c := &Memcached{
		cfg:      cfg,
		memcache: client,
	}

	if cfg.BatchSize == 0 || cfg.Parallelism == 0 {
		return c
	}

	c.inputCh = make(chan *work)
	c.wg.Add(cfg.Parallelism)

	for i := 0; i < cfg.Parallelism; i++ {
		go func() {
			for input := range c.inputCh {
				res := &result{
					batchID: input.batchID,
				}
				res.found, res.bufs, res.missed = c.fetch(input.ctx, input.keys)
				input.resultCh <- res
			}

			c.wg.Done()
		}()
	}

	return c
}

type work struct {
	keys     []string
	ctx      context.Context
	resultCh chan<- *result
	batchID  int // For ordering results.
}

type result struct {
	found   []string
	bufs    [][]byte
	missed  []string
	batchID int // For ordering results.
}

func memcacheStatusCode(err error) string {
	// See https://godoc.org/github.com/bradfitz/gomemcache/memcache#pkg-variables
	switch err {
	case nil:
		return "200"
	case memcache.ErrCacheMiss:
		return "404"
	case memcache.ErrMalformedKey:
		return "400"
	default:
		return "500"
	}
}

// Fetch gets keys from the cache. The keys that are found must be in the order of the keys requested.
func (c *Memcached) Fetch(ctx context.Context, keys []string) (found []string, bufs [][]byte, missed []string) {
	instr.TimeRequestHistogramStatus(ctx, "Memcache.Get", memcacheRequestDuration, memcacheStatusCode, func(ctx context.Context) error {
		if c.cfg.BatchSize == 0 {
			found, bufs, missed = c.fetch(ctx, keys)
			return nil
		}

		found, bufs, missed = c.fetchKeysBatched(ctx, keys)
		return nil
	})
	return
}

func (c *Memcached) fetch(ctx context.Context, keys []string) (found []string, bufs [][]byte, missed []string) {
	var items map[string]*memcache.Item
	instr.TimeRequestHistogramStatus(ctx, "Memcache.GetMulti", memcacheRequestDuration, memcacheStatusCode, func(_ context.Context) error {
		sp := opentracing.SpanFromContext(ctx)
		sp.LogFields(otlog.Int("keys requested", len(keys)))

		var err error
		items, err = c.memcache.GetMulti(keys)

		sp.LogFields(otlog.Int("keys found", len(items)))

		// Memcached returns partial results even on error.
		if err != nil {
			sp.LogFields(otlog.Error(err))
			level.Error(util.Logger).Log("msg", "Failed to get keys from memcached", "err", err)
		}
		return err
	})

	for _, key := range keys {
		item, ok := items[key]
		if ok {
			found = append(found, key)
			bufs = append(bufs, item.Value)
		} else {
			missed = append(missed, key)
		}
	}
	return
}

func (c *Memcached) fetchKeysBatched(ctx context.Context, keys []string) (found []string, bufs [][]byte, missed []string) {
	resultsCh := make(chan *result)
	batchSize := c.cfg.BatchSize

	go func() {
		for i, j := 0, 0; i < len(keys); i += batchSize {
			batchKeys := keys[i:util.Min(i+batchSize, len(keys))]
			c.inputCh <- &work{
				keys:     batchKeys,
				ctx:      ctx,
				resultCh: resultsCh,
				batchID:  j,
			}
			j++
		}
	}()

	// Read all values from this channel to avoid blocking upstream.
	numResults := len(keys) / batchSize
	if len(keys)%batchSize != 0 {
		numResults++
	}

	// We need to order found by the input keys order.
	results := make([]*result, numResults)
	for i := 0; i < numResults; i++ {
		result := <-resultsCh
		results[result.batchID] = result
	}
	close(resultsCh)

	for _, result := range results {
		found = append(found, result.found...)
		bufs = append(bufs, result.bufs...)
		missed = append(missed, result.missed...)
	}

	return
}

// Store stores the key in the cache.
func (c *Memcached) Store(ctx context.Context, keys []string, bufs [][]byte) {
	for i := range keys {
		err := instr.TimeRequestHistogramStatus(ctx, "Memcache.Put", memcacheRequestDuration, memcacheStatusCode, func(_ context.Context) error {
			item := memcache.Item{
				Key:        keys[i],
				Value:      bufs[i],
				Expiration: int32(c.cfg.Expiration.Seconds()),
			}
			return c.memcache.Set(&item)
		})
		if err != nil {
			sp := opentracing.SpanFromContext(ctx)
			sp.LogFields(otlog.Error(err))
			level.Error(util.Logger).Log("msg", "failed to put to memcached", "err", err)
		}
	}
}

// Stop does nothing.
func (c *Memcached) Stop() error {
	if c.inputCh == nil {
		return nil
	}

	close(c.inputCh)
	c.wg.Wait()
	return nil
}

// HashKey hashes key into something you can store in memcached.
func HashKey(key string) string {
	hasher := fnv.New64a()
	hasher.Write([]byte(key)) // This'll never error.

	// Hex because memcache errors for the bytes produced by the hash.
	return hex.EncodeToString(hasher.Sum(nil))
}
