package filter_test

import (
	"testing"

	"github.com/kaulie/event-center/internal/filter"
	"github.com/kaulie/event-center/internal/model"
)

func event(provider, eventType, stream, subject string) *model.Event {
	return &model.Event{Provider: provider, Type: eventType, Stream: stream, Subject: subject}
}

func TestMatchesEmptyFiltersMatchEverything(t *testing.T) {
	sub := &model.Subscription{Stream: model.StreamAll}
	if !filter.Matches(sub, event("github", "github.push", "github", "repo:a/b")) {
		t.Fatal("subscription without filters must match")
	}
}

func TestMatchesExactAndWildcardTypes(t *testing.T) {
	cases := []struct {
		pattern string
		evType  string
		want    bool
	}{
		{"github.push", "github.push", true},
		{"github.push", "github.pull_request.opened", false},
		{"github.*", "github.push", true},
		{"github.*", "github.pull_request.opened", false},
		{"github.**", "github.pull_request.opened", true},
		{"github.pull_request.*", "github.pull_request.opened", true},
		{"github.pull_request.*", "github.push", false},
		{"github.pull_*", "github.pull_request", true},
		{"*", "anything.at.all", true},
		{"**", "anything.at.all", true},
		{"k8s.**", "github.push", false},
	}
	for _, tc := range cases {
		sub := &model.Subscription{Stream: model.StreamAll, TypeFilters: []string{tc.pattern}}
		if got := filter.Matches(sub, event("github", tc.evType, "github", "")); got != tc.want {
			t.Errorf("pattern %q vs %q = %v, want %v", tc.pattern, tc.evType, got, tc.want)
		}
	}
}

func TestMatchesProviderStreamAndSubject(t *testing.T) {
	sub := &model.Subscription{
		Stream:          "github",
		ProviderFilters: []string{"github"},
		SubjectPattern:  "repo:kaulie/*",
	}
	if !filter.Matches(sub, event("github", "github.push", "github", "repo:kaulie/event-center")) {
		t.Fatal("expected match")
	}
	if filter.Matches(sub, event("github", "github.push", "k8s", "repo:kaulie/event-center")) {
		t.Fatal("stream mismatch must not match")
	}
	if filter.Matches(sub, event("gitlab", "github.push", "github", "repo:kaulie/event-center")) {
		t.Fatal("provider mismatch must not match")
	}
	if filter.Matches(sub, event("github", "github.push", "github", "repo:other/thing")) {
		t.Fatal("subject mismatch must not match")
	}
	if filter.Matches(sub, event("github", "github.push", "github", "")) {
		t.Fatal("empty subject must not satisfy a subject pattern")
	}
}
