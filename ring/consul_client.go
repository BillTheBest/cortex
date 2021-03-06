package ring

import (
	"flag"
	"fmt"
	"net/http"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/golang/snappy"
	consul "github.com/hashicorp/consul/api"
	cleanhttp "github.com/hashicorp/go-cleanhttp"
	"github.com/prometheus/common/log"
)

const (
	longPollDuration = 10 * time.Second
)

// ConsulConfig to create a ConsulClient
type ConsulConfig struct {
	Host              string
	Prefix            string
	HTTPClientTimeout time.Duration

	Mock ConsulClient
}

// RegisterFlags adds the flags required to config this to the given FlagSet
func (cfg *ConsulConfig) RegisterFlags(f *flag.FlagSet) {
	f.StringVar(&cfg.Host, "consul.hostname", "localhost:8500", "Hostname and port of Consul.")
	f.StringVar(&cfg.Prefix, "consul.prefix", "collectors/", "Prefix for keys in Consul.")
	f.DurationVar(&cfg.HTTPClientTimeout, "consul.client-timeout", 2*longPollDuration, "HTTP timeout when talking to consul")
}

// ConsulClient is a high-level client for Consul, that exposes operations
// such as CAS and Watch which take callbacks.  It also deals with serialisation
// by having an instance factory passed in to methods and deserialising into that.
type ConsulClient interface {
	CAS(key string, f CASCallback) error
	WatchPrefix(path string, done <-chan struct{}, f func(string, interface{}) bool)
	WatchKey(key string, done <-chan struct{}, f func(interface{}) bool)
	Get(key string) (interface{}, error)
	PutBytes(key string, buf []byte) error
}

// CASCallback is the type of the callback to CAS.  If err is nil, out must be non-nil.
type CASCallback func(in interface{}) (out interface{}, retry bool, err error)

// Codec allows the consult client to serialise and deserialise values.
type Codec interface {
	Decode([]byte) (interface{}, error)
	Encode(interface{}) ([]byte, error)
}

type kv interface {
	CAS(p *consul.KVPair, q *consul.WriteOptions) (bool, *consul.WriteMeta, error)
	Get(key string, q *consul.QueryOptions) (*consul.KVPair, *consul.QueryMeta, error)
	List(path string, q *consul.QueryOptions) (consul.KVPairs, *consul.QueryMeta, error)
	Put(p *consul.KVPair, q *consul.WriteOptions) (*consul.WriteMeta, error)
}

type consulClient struct {
	kv
	codec Codec
}

// NewConsulClient returns a new ConsulClient.
func NewConsulClient(cfg ConsulConfig, codec Codec) (ConsulClient, error) {
	if cfg.Mock != nil {
		return cfg.Mock, nil
	}

	client, err := consul.NewClient(&consul.Config{
		Address: cfg.Host,
		Scheme:  "http",
		HttpClient: &http.Client{
			Transport: cleanhttp.DefaultPooledTransport(),
			// See https://blog.cloudflare.com/the-complete-guide-to-golang-net-http-timeouts/
			Timeout: cfg.HTTPClientTimeout,
		},
	})
	if err != nil {
		return nil, err
	}
	var c ConsulClient = &consulClient{
		kv:    client.KV(),
		codec: codec,
	}
	if cfg.Prefix != "" {
		c = PrefixClient(c, cfg.Prefix)
	}
	return c, nil
}

var (
	queryOptions = &consul.QueryOptions{
		RequireConsistent: true,
	}
	writeOptions = &consul.WriteOptions{}

	// ErrNotFound is returned by ConsulClient.Get.
	ErrNotFound = fmt.Errorf("Not found")
)

// ProtoCodec is a Codec for proto/snappy
type ProtoCodec struct {
	Factory func() proto.Message
}

// Decode implements Codec
func (p ProtoCodec) Decode(bytes []byte) (interface{}, error) {
	out := p.Factory()
	bytes, err := snappy.Decode(nil, bytes)
	if err != nil {
		return nil, err
	}
	if err := proto.Unmarshal(bytes, out); err != nil {
		return nil, err
	}
	return out, nil
}

// Encode implements Codec
func (p ProtoCodec) Encode(msg interface{}) ([]byte, error) {
	bytes, err := proto.Marshal(msg.(proto.Message))
	if err != nil {
		return nil, err
	}
	return snappy.Encode(nil, bytes), nil
}

// CAS atomically modifies a value in a callback.
// If value doesn't exist you'll get nil as an argument to your callback.
func (c *consulClient) CAS(key string, f CASCallback) error {
	var (
		index   = uint64(0)
		retries = 10
		retry   = true
	)
	for i := 0; i < retries; i++ {
		kvp, _, err := c.kv.Get(key, queryOptions)
		if err != nil {
			log.Errorf("Error getting %s: %v", key, err)
			continue
		}
		var intermediate interface{}
		if kvp != nil {
			out, err := c.codec.Decode(kvp.Value)
			if err != nil {
				log.Errorf("Error decoding %s: %v", key, err)
				continue
			}
			// If key doesn't exist, index will be 0.
			index = kvp.ModifyIndex
			intermediate = out
		}

		intermediate, retry, err = f(intermediate)
		if err != nil {
			log.Errorf("Error CASing %s: %v", key, err)
			if !retry {
				return err
			}
			continue
		}

		if intermediate == nil {
			panic("Callback must instantiate value!")
		}

		bytes, err := c.codec.Encode(intermediate)
		if err != nil {
			log.Errorf("Error serialising value for %s: %v", key, err)
			continue
		}
		ok, _, err := c.kv.CAS(&consul.KVPair{
			Key:         key,
			Value:       bytes,
			ModifyIndex: index,
		}, writeOptions)
		if err != nil {
			log.Errorf("Error CASing %s: %v", key, err)
			continue
		}
		if !ok {
			log.Errorf("Error CASing %s, trying again %d", key, index)
			continue
		}
		return nil
	}
	return fmt.Errorf("failed to CAS %s", key)
}

const (
	initialBackoff = 1 * time.Second
	maxBackoff     = 1 * time.Minute
)

type backoff struct {
	done    <-chan struct{}
	backoff time.Duration
}

func newBackoff(done <-chan struct{}) *backoff {
	return &backoff{
		done:    done,
		backoff: initialBackoff,
	}
}

func (b *backoff) reset() {
	b.backoff = initialBackoff
}

func (b *backoff) wait() {
	select {
	case <-b.done:
	case <-time.After(b.backoff):
		b.backoff = b.backoff * 2
		if b.backoff > maxBackoff {
			b.backoff = maxBackoff
		}
	}
}

func isClosed(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// WatchPrefix will watch a given prefix in consul for changes. When a value
// under said prefix changes, the f callback is called with the deserialised
// value. To construct the deserialised value, a factory function should be
// supplied which generates an empty struct for WatchPrefix to deserialise
// into. Values in Consul are assumed to be JSON. This function blocks until
// the done channel is closed.
func (c *consulClient) WatchPrefix(prefix string, done <-chan struct{}, f func(string, interface{}) bool) {
	var (
		backoff = newBackoff(done)
		index   = uint64(0)
	)
	for {
		if isClosed(done) {
			return
		}
		kvps, meta, err := c.kv.List(prefix, &consul.QueryOptions{
			RequireConsistent: true,
			WaitIndex:         index,
			WaitTime:          longPollDuration,
		})
		if err != nil {
			log.Errorf("Error getting path %s: %v", prefix, err)
			backoff.wait()
			continue
		}
		backoff.reset()

		// Skip if the index is the same as last time, because the key value is
		// guaranteed to be the same as last time
		if index == meta.LastIndex {
			continue
		}
		index = meta.LastIndex

		for _, kvp := range kvps {
			out, err := c.codec.Decode(kvp.Value)
			if err != nil {
				log.Errorf("Error decoding %s: %v", kvp.Key, err)
				continue
			}
			if !f(kvp.Key, out) {
				return
			}
		}
	}
}

// WatchKey will watch a given key in consul for changes. When the value
// under said key changes, the f callback is called with the deserialised
// value. To construct the deserialised value, a factory function should be
// supplied which generates an empty struct for WatchKey to deserialise
// into. Values in Consul are assumed to be JSON. This function blocks until
// the done channel is closed.
func (c *consulClient) WatchKey(key string, done <-chan struct{}, f func(interface{}) bool) {
	var (
		backoff = newBackoff(done)
		index   = uint64(0)
	)
	for {
		if isClosed(done) {
			return
		}
		kvp, meta, err := c.kv.Get(key, &consul.QueryOptions{
			RequireConsistent: true,
			WaitIndex:         index,
			WaitTime:          longPollDuration,
		})
		if err != nil {
			log.Errorf("Error getting path %s: %v", key, err)
			backoff.wait()
			continue
		}
		backoff.reset()

		// Skip if the index is the same as last time, because the key value is
		// guaranteed to be the same as last time
		if index == meta.LastIndex {
			continue
		}
		index = meta.LastIndex

		var out interface{}
		if kvp != nil {
			var err error
			out, err = c.codec.Decode(kvp.Value)
			if err != nil {
				log.Errorf("Error decoding %s: %v", key, err)
				continue
			}
		}
		if !f(out) {
			return
		}
	}
}

func (c *consulClient) PutBytes(key string, buf []byte) error {
	_, err := c.kv.Put(&consul.KVPair{
		Key:   key,
		Value: buf,
	}, &consul.WriteOptions{})
	return err
}

func (c *consulClient) Get(key string) (interface{}, error) {
	kvp, _, err := c.kv.Get(key, &consul.QueryOptions{})
	if err != nil {
		return nil, err
	}
	return c.codec.Decode(kvp.Value)
}

type prefixedConsulClient struct {
	prefix string
	consul ConsulClient
}

// PrefixClient takes a ConsulClient and forces a prefix on all its operations.
func PrefixClient(client ConsulClient, prefix string) ConsulClient {
	return &prefixedConsulClient{prefix, client}
}

// CAS atomically modifies a value in a callback. If the value doesn't exist,
// you'll get 'nil' as an argument to your callback.
func (c *prefixedConsulClient) CAS(key string, f CASCallback) error {
	return c.consul.CAS(c.prefix+key, f)
}

// WatchPrefix watches a prefix. This is in addition to the prefix we already have.
func (c *prefixedConsulClient) WatchPrefix(path string, done <-chan struct{}, f func(string, interface{}) bool) {
	c.consul.WatchPrefix(c.prefix+path, done, f)
}

// WatchKey watches a key.
func (c *prefixedConsulClient) WatchKey(key string, done <-chan struct{}, f func(interface{}) bool) {
	c.consul.WatchKey(c.prefix+key, done, f)
}

// PutBytes writes bytes to Consul.
func (c *prefixedConsulClient) PutBytes(key string, buf []byte) error {
	return c.consul.PutBytes(c.prefix+key, buf)
}

func (c *prefixedConsulClient) Get(key string) (interface{}, error) {
	return c.consul.Get(c.prefix + key)
}
