// Package metrics implements a tiny dependency-free Prometheus exposition
// registry. The event center only needs a handful of counters and gauges, so a
// hand rolled registry keeps the binary small.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry collects counters and gauges keyed by name plus label set.
type Registry struct {
	mu     sync.RWMutex
	series map[string]*series
}

type series struct {
	name   string
	labels map[string]string
	kind   string // counter | gauge
	value  atomic.Int64
}

// New builds an empty registry.
func New() *Registry {
	return &Registry{series: map[string]*series{}}
}

// Inc adds delta to a counter.
func (r *Registry) Inc(name string, labels map[string]string, delta int64) {
	r.get(name, labels, "counter").value.Add(delta)
}

// Set stores a gauge value.
func (r *Registry) Set(name string, labels map[string]string, value int64) {
	r.get(name, labels, "gauge").value.Store(value)
}

func (r *Registry) get(name string, labels map[string]string, kind string) *series {
	key := name + "|" + labelKey(labels)
	r.mu.RLock()
	s, ok := r.series[key]
	r.mu.RUnlock()
	if ok {
		return s
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.series[key]; ok {
		return s
	}
	s = &series{name: name, labels: labels, kind: kind}
	r.series[key] = s
	return s
}

// Render writes the registry in Prometheus text exposition format.
func (r *Registry) Render(w io.Writer) {
	r.mu.RLock()
	all := make([]*series, 0, len(r.series))
	for _, s := range r.series {
		all = append(all, s)
	}
	r.mu.RUnlock()

	sort.Slice(all, func(i, j int) bool {
		if all[i].name != all[j].name {
			return all[i].name < all[j].name
		}
		return labelKey(all[i].labels) < labelKey(all[j].labels)
	})

	seenType := map[string]bool{}
	for _, s := range all {
		if !seenType[s.name] {
			fmt.Fprintf(w, "# TYPE %s %s\n", s.name, s.kind)
			seenType[s.name] = true
		}
		fmt.Fprintf(w, "%s%s %d\n", s.name, renderLabels(s.labels), s.value.Load())
	}
}

func labelKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
		b.WriteByte(',')
	}
	return b.String()
}

func renderLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(strings.ReplaceAll(labels[k], `"`, `\"`))
		b.WriteString(`"`)
	}
	b.WriteByte('}')
	return b.String()
}
