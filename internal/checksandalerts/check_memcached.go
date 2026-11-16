package checksandalerts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"mmwatchdog/internal/primitives"
)

// Memcached is the inputs of the memcached check module: the address of a
// memcached server, written as host:port. The check asks the server for its
// statistics and is healthy when the server gives them.
type Memcached string

// memcachedCounters are the statistics of a memcached server that only grow
// while the server runs, in the order of the reading that a check keeps
// between its runs: the gets that found their item, the gets that found
// none, the items that the server removed to make room, and the seconds
// since the server started. A restart of the server sets all four back to
// zero. The seconds fall back also when the new server has already counted
// more gets than the old one, so the check then takes a fresh sample in
// place of a growth that mixes two servers.
var memcachedCounters = []string{"get_hits", "get_misses", "evictions", "uptime"}

func (m *Memcached) Validate(InputValidation) *FieldError {
	if e := HostPort(string(*m), 1); e != nil {
		return &FieldError{"", e.Error()}
	}
	return nil
}

// TimeOfEachRun is the sample of the first run: a rate needs two readings of
// the server's counters.
func (m *Memcached) TimeOfEachRun() (time.Duration, string) {
	return CPUSampleWindow, "the initial sample of the memcached server's counters"
}

func (m *Memcached) Run(ctx context.Context, r *CheckRun) Observation {
	address := string(*m)
	statistics, failed := memcachedStatistics(ctx, address)
	if failed != nil {
		return *failed
	}
	o := Result("ok", "the memcached server at "+address+" gave its statistics")
	o.Measurements = map[string]Measurement{}
	unavailable := map[string]string{}
	if connections, given := statistics["curr_connections"]; given {
		o.Measurements["current_connections"] = Measurement{connections, "connections"}
	} else {
		unavailable["current_connections"] = "the memcached server gave no curr_connections"
	}
	if items, given := statistics["curr_items"]; given {
		o.Measurements["cached_items"] = Measurement{items, "items"}
	} else {
		unavailable["cached_items"] = "the memcached server gave no curr_items"
	}
	if used, given := statistics["bytes"]; !given {
		unavailable["cache_used_percent"] = "the memcached server gave no bytes"
	} else if limit := statistics["limit_maxbytes"]; limit > 0 {
		o.Measurements["cache_used_percent"] = Measurement{100 * used / limit, "percent"}
	} else {
		unavailable["cache_used_percent"] = "the memcached server gave no limit_maxbytes"
	}

	// The hits, the misses, and the evictions are the growth of the
	// server's counters since the run before. The first reading is the
	// answer above; the sample of a first run asks the server once more.
	answer := statistics
	read := func() ([]uint64, error) {
		if answer == nil {
			var again *Observation
			if answer, again = memcachedStatistics(ctx, address); again != nil {
				return nil, errors.New(again.Message)
			}
		}
		counters := []uint64{}
		for _, name := range memcachedCounters {
			value, given := answer[name]
			if !given {
				return nil, errors.New("the memcached server gave no " + name)
			}
			counters = append(counters, uint64(value))
		}
		answer = nil
		return counters, nil
	}
	before, now, window, problem := r.CounterReadings(ctx, read)
	if problem != "" {
		// The server answered, so the check stays healthy; only a limit on
		// one of these measurements makes the run unknown.
		for _, name := range []string{"cache_hits_per_second", "cache_misses_per_second", "cache_hit_percent", "cache_evictions_per_second"} {
			unavailable[name] = problem
		}
		return withLimits(r.Check, o, unavailable)
	}
	seconds := window.Seconds()
	hits, misses, evictions := float64(now[0]-before[0]), float64(now[1]-before[1]), float64(now[2]-before[2]) //nolint:nilaway // CounterReadings returns nil samples only with a problem; the problem guard returns before indexing the counters.
	o.Measurements["cache_hits_per_second"] = Measurement{hits / seconds, "hits per second"}
	o.Measurements["cache_misses_per_second"] = Measurement{misses / seconds, "misses per second"}
	o.Measurements["cache_evictions_per_second"] = Measurement{evictions / seconds, "evictions per second"}
	if hits+misses > 0 {
		o.Measurements["cache_hit_percent"] = Measurement{100 * hits / (hits + misses), "percent"}
	} else {
		unavailable["cache_hit_percent"] = fmt.Sprintf("the memcached server answered no gets in the last %.3fs, so cache_hit_percent cannot be measured", seconds)
	}
	o.SampleWindow = window
	o.Message += fmt.Sprintf("; hits, misses, and evictions over %.3fs", seconds)
	return withLimits(r.Check, o, unavailable)
}

// memcachedStatistics asks the memcached server at address for its
// statistics and returns those that are numbers, by their names. A server
// that does not give them returns the failed observation of the check.
func memcachedStatistics(ctx context.Context, address string) (map[string]float64, *Observation) {
	// The server ends its statistics with a line that holds END.
	reply, err := primitives.TCPExchangeUntil(ctx, address, "stats\r\n", func(received []byte) bool {
		return bytes.HasSuffix(received, []byte("END\r\n")) || bytes.HasPrefix(received, []byte("ERROR"))
	})
	if o := serverThatDidNotAnswer(ctx, address, reply, err); o != nil {
		return nil, o
	}
	statistics := map[string]float64{}
	for _, line := range strings.Split(reply, "\r\n") {
		if fields := strings.Fields(line); len(fields) == 3 && fields[0] == "STAT" {
			if value, err := strconv.ParseFloat(fields[2], 64); err == nil {
				statistics[fields[1]] = value
			}
		}
	}
	if !strings.HasSuffix(reply, "END\r\n") || len(statistics) == 0 {
		o := Result("failed", "the server at "+address+" did not answer with the statistics of a memcached server")
		if reply != "" {
			o.Details = []string{"the server sent: " + primitives.DetailLine(primitives.Clip(reply, primitives.DetailLineLimit-20))}
		}
		return nil, &o
	}
	return statistics, nil
}

// memcachedDescription is what "mmwatchdog describe-module memcached" shows, and what
// validation knows about the module's inputs.
var memcachedDescription = ModuleDescription{
	Summary: "Asks a memcached server for its statistics, and is healthy when the server gives them. It measures the server's connections, items, and memory, and its hits, misses, and evictions since the run before.",
	UsedAs:  UsedAsCheck,
	Value:   &ModuleInput{Type: "string", Required: true, Description: "The address of the memcached server, as host:port."},
	Measurements: []ModuleMeasurement{
		{Name: "current_connections", Unit: "connections", Description: "The connections that are open to the server now."},
		{Name: "cached_items", Unit: "items", Description: "The items that the server holds now."},
		{Name: "cache_used_percent", Unit: "percent", Description: "The percentage of the server's configured cache memory used to store items."},
		{Name: "cache_hits_per_second", Unit: "hits per second", Description: "The number of cache lookups per second since the previous run that found their item."},
		{Name: "cache_misses_per_second", Unit: "misses per second", Description: "The number of cache lookups per second since the previous run that found no item, because the server never had the item, removed the item, or let the item expire."},
		{Name: "cache_hit_percent", Unit: "percent", Description: "The percentage of cache lookups since the previous run that found an item, from 0 to 100. No reading is available without lookups."},
		{Name: "cache_evictions_per_second", Unit: "evictions per second", Description: "The number of items per second removed before expiry to make room in the cache, measured since the previous run."},
	},
	Examples: []ModuleExample{{
		Description:   "The memcached server of this machine answers.",
		Configuration: "memcached: 127.0.0.1:11211\nalert_severity: warn\n",
	}},
}
