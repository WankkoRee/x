package hosts

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/go-gost/core/hosts"
	"github.com/go-gost/core/logger"
	"github.com/go-gost/x/internal/loader"
)

type Mapping struct {
	Hostname string
	IP       net.IP
}

type options struct {
	mappings    []Mapping
	fileLoader  loader.Loader
	redisLoader loader.Loader
	period      time.Duration
	logger      logger.Logger
}

type Option func(opts *options)

func MappingsOption(mappings []Mapping) Option {
	return func(opts *options) {
		opts.mappings = mappings
	}
}

func ReloadPeriodOption(period time.Duration) Option {
	return func(opts *options) {
		opts.period = period
	}
}

func FileLoaderOption(fileLoader loader.Loader) Option {
	return func(opts *options) {
		opts.fileLoader = fileLoader
	}
}

func RedisLoaderOption(redisLoader loader.Loader) Option {
	return func(opts *options) {
		opts.redisLoader = redisLoader
	}
}

func LoggerOption(logger logger.Logger) Option {
	return func(opts *options) {
		opts.logger = logger
	}
}

// Hosts is a static table lookup for hostnames.
// For each host a single line should be present with the following information:
// IP_address canonical_hostname [aliases...]
// Fields of the entry are separated by any number of blanks and/or tab characters.
// Text from a "#" character until the end of the line is a comment, and is ignored.
type Hosts struct {
	mappings   map[string][]net.IP
	mu         sync.RWMutex
	cancelFunc context.CancelFunc
	options    options
}

func NewHostMapper(opts ...Option) hosts.HostMapper {
	var options options
	for _, opt := range opts {
		opt(&options)
	}

	ctx, cancel := context.WithCancel(context.TODO())
	p := &Hosts{
		mappings:   make(map[string][]net.IP),
		cancelFunc: cancel,
		options:    options,
	}

	if err := p.reload(ctx); err != nil {
		options.logger.Warnf("reload: %v", err)
	}
	if p.options.period > 0 {
		go p.periodReload(ctx)
	}

	return p
}

// Lookup searches the IP address corresponds to the given network and host from the host table.
// The network should be 'ip', 'ip4' or 'ip6', default network is 'ip'.
// the host should be a hostname (example.org) or a hostname with dot prefix (.example.org).
func (h *Hosts) Lookup(network, host string) (ips []net.IP, ok bool) {
	h.options.logger.Debugf("lookup %s/%s", host, network)
	ips = h.lookup(host)
	if ips == nil {
		ips = h.lookup("." + host)
	}
	if ips == nil {
		s := host
		for {
			if index := strings.IndexByte(s, '.'); index > 0 {
				ips = h.lookup(s[index:])
				s = s[index+1:]
				if ips == nil {
					continue
				}
			}
			break
		}
	}

	if ips == nil {
		return
	}

	switch network {
	case "ip4":
		var v []net.IP
		for _, ip := range ips {
			if ip = ip.To4(); ip != nil {
				v = append(v, ip)
			}
		}
		ips = v
	case "ip6":
		var v []net.IP
		for _, ip := range ips {
			if ip.To4() == nil {
				v = append(v, ip)
			}
		}
		ips = v
	default:
	}

	if len(ips) > 0 {
		h.options.logger.Debugf("host mapper: %s/%s -> %s", host, network, ips)
	}

	return
}

func (h *Hosts) lookup(host string) []net.IP {
	if h == nil || len(h.mappings) == 0 {
		return nil
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	return h.mappings[host]
}

func (h *Hosts) periodReload(ctx context.Context) error {
	period := h.options.period
	if period < time.Second {
		period = time.Second
	}
	ticker := time.NewTicker(period)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := h.reload(ctx); err != nil {
				h.options.logger.Warnf("reload: %v", err)
				// return err
			}
			h.options.logger.Debugf("hosts reload done")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (h *Hosts) reload(ctx context.Context) (err error) {
	mappings := make(map[string][]net.IP)

	mapf := func(hostname string, ip net.IP) {
		ips := mappings[hostname]
		found := false
		for i := range ips {
			if ip.Equal(ips[i]) {
				found = true
				break
			}
		}
		if !found {
			ips = append(ips, ip)
		}
		mappings[hostname] = ips
	}

	for _, mapping := range h.options.mappings {
		mapf(mapping.Hostname, mapping.IP)
	}

	m, err := h.load(ctx)
	for i := range m {
		mapf(m[i].Hostname, m[i].IP)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	h.mappings = mappings

	return
}

func (h *Hosts) load(ctx context.Context) (mappings []Mapping, err error) {
	if h.options.fileLoader != nil {
		r, er := h.options.fileLoader.Load(ctx)
		if er != nil {
			h.options.logger.Warnf("file loader: %v", er)
		}
		mappings, _ = h.parseMapping(r)
	}

	if h.options.redisLoader != nil {
		r, er := h.options.redisLoader.Load(ctx)
		if er != nil {
			h.options.logger.Warnf("redis loader: %v", er)
		}
		if m, _ := h.parseMapping(r); m != nil {
			mappings = append(mappings, m...)
		}
	}

	return
}

func (h *Hosts) parseMapping(r io.Reader) (mappings []Mapping, err error) {
	if r == nil {
		return
	}

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.Replace(scanner.Text(), "\t", " ", -1)
		line = strings.TrimSpace(line)
		if n := strings.IndexByte(line, '#'); n >= 0 {
			line = line[:n]
		}
		var sp []string
		for _, s := range strings.Split(line, " ") {
			if s = strings.TrimSpace(s); s != "" {
				sp = append(sp, s)
			}
		}
		if len(sp) < 2 {
			continue // invalid lines are ignored
		}

		ip := net.ParseIP(sp[0])
		if ip == nil {
			continue // invalid IP addresses are ignored
		}

		for _, v := range sp[1:] {
			mappings = append(mappings, Mapping{
				Hostname: v,
				IP:       ip,
			})
		}
	}

	err = scanner.Err()
	return
}

func (h *Hosts) Close() error {
	h.cancelFunc()
	if h.options.fileLoader != nil {
		h.options.fileLoader.Close()
	}
	if h.options.redisLoader != nil {
		h.options.redisLoader.Close()
	}
	return nil
}
