// Package filter decides whether an event is addressed to a subscription.
//
// Filters use globs so a consumer can subscribe to a whole family of event
// types without the event center having to pre-declare every type:
//
//   - event types are matched segment by segment on "." — "*" matches exactly
//     one segment, "**" matches one or more (e.g. "github.*" matches
//     "github.push", "github.**" also matches "github.pull_request.opened")
//   - providers are matched as a single segment
//   - subjects are matched like paths ("repo:kaulie/*")
//
// An omitted filter matches everything.
package filter

import (
	"path"
	"strings"

	"github.com/kaulie/event-center/internal/model"
)

// Matches reports whether the event satisfies every filter of the
// subscription.
func Matches(sub *model.Subscription, ev *model.Event) bool {
	if sub.Stream != "" && sub.Stream != model.StreamAll && sub.Stream != ev.Stream {
		return false
	}
	if len(sub.ProviderFilters) > 0 && !matchAny(sub.ProviderFilters, ev.Provider, matchSegment) {
		return false
	}
	if len(sub.TypeFilters) > 0 && !matchAny(sub.TypeFilters, ev.Type, matchType) {
		return false
	}
	if sub.SubjectPattern != "" && !matchPath(sub.SubjectPattern, ev.Subject) {
		return false
	}
	return true
}

type matcher func(pattern, value string) bool

func matchAny(patterns []string, value string, m matcher) bool {
	for _, p := range patterns {
		if p == "" || p == "*" || p == "**" {
			return true
		}
		if m(p, value) {
			return true
		}
	}
	return false
}

// matchType matches a dotted type path where "*" matches exactly one segment
// and "**" matches one or more segments.
func matchType(pattern, value string) bool {
	if pattern == value {
		return true
	}
	return matchTypeSegments(strings.Split(pattern, "."), strings.Split(value, "."))
}

func matchTypeSegments(ps, vs []string) bool {
	if len(ps) == 0 {
		return len(vs) == 0
	}
	if ps[0] == "**" {
		// "**" must consume at least one segment.
		for i := 1; i <= len(vs); i++ {
			if matchTypeSegments(ps[1:], vs[i:]) {
				return true
			}
		}
		return false
	}
	if len(vs) == 0 {
		return false
	}
	if !matchSegment(ps[0], vs[0]) {
		return false
	}
	return matchTypeSegments(ps[1:], vs[1:])
}

// matchSegment matches a single segment with shell style wildcards.
func matchSegment(pattern, value string) bool {
	if pattern == value || pattern == "*" {
		return true
	}
	if !strings.ContainsAny(pattern, "*?[") {
		return false
	}
	ok, err := path.Match(pattern, value)
	if err != nil {
		// A malformed pattern degrades to prefix matching instead of silently
		// dropping every event.
		return strings.HasPrefix(value, strings.TrimSuffix(pattern, "*"))
	}
	return ok
}

// matchPath matches a subject using path semantics, where "*" does not cross
// the "/" separator.
func matchPath(pattern, value string) bool {
	if pattern == value {
		return true
	}
	if !strings.ContainsAny(pattern, "*?[") {
		return false
	}
	ok, err := path.Match(pattern, value)
	if err != nil {
		return strings.HasPrefix(value, strings.TrimSuffix(pattern, "*"))
	}
	return ok
}
