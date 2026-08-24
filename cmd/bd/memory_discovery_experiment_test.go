package main

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const discoveryTestBody = `---
title: Run the Dolt recovery playbook
aliases: [phantom database, dolt recovery]
lifecycle: active
navigation_rank: 2
references: [mem-storage-root, task-incident-42]
provenance: human
---
When a Dolt phantom database appears, inspect the data directory before restarting the server.`

func TestParseExperimentalMemory(t *testing.T) {
	got := parseExperimentalMemory("mem-dolt-phantom", discoveryTestBody)
	if got.ID != "mem-dolt-phantom" || got.Key != "mem-dolt-phantom" {
		t.Fatalf("identity = %q/%q, want key-backed canonical identity", got.ID, got.Key)
	}
	if got.Title != "Run the Dolt recovery playbook" {
		t.Fatalf("title = %q", got.Title)
	}
	if !reflect.DeepEqual(got.Aliases, []string{"phantom database", "dolt recovery"}) {
		t.Fatalf("aliases = %#v", got.Aliases)
	}
	if got.Lifecycle != "active" || got.NavigationRank == nil || *got.NavigationRank != 2 {
		t.Fatalf("lifecycle/rank = %q/%v", got.Lifecycle, got.NavigationRank)
	}
	if !reflect.DeepEqual(got.References, []string{"mem-storage-root", "task-incident-42"}) {
		t.Fatalf("references = %#v", got.References)
	}
	if strings.Contains(got.Body, "navigation_rank") || !strings.HasPrefix(got.Body, "When a Dolt") {
		t.Fatalf("body did not exclude frontmatter: %q", got.Body)
	}
}

func TestExperimentalOrderingCandidateParityAndFixedProjection(t *testing.T) {
	memories := map[string]string{
		"a-body-hit":  memoryFixture("Weak title", nil, "active", 30, "deploy rollback lives in the body"),
		"b-title-hit": memoryFixture("Deploy rollback procedure", []string{"release"}, "active", 20, "use the runbook"),
		"c-alias-hit": memoryFixture("Release notes", []string{"deploy rollback"}, "archived", 10, "superseded entry"),
	}
	weights := defaultExperimentalBM25FConfig()
	pages := make(map[experimentalMemoryOrder]experimentalMemoryPage)
	for _, order := range []experimentalMemoryOrder{memoryOrderKey, memoryOrderNavigation, memoryOrderBM25F} {
		page, err := buildExperimentalMemoryPage(memories, experimentalDiscoveryRequest{
			Search:   "deploy rollback",
			Order:    order,
			PageSize: 10,
			BM25F:    weights,
		})
		if err != nil {
			t.Fatalf("%s: %v", order, err)
		}
		pages[order] = page
	}

	wantSets := []string{"a-body-hit", "b-title-hit", "c-alias-hit"}
	for order, page := range pages {
		got := experimentalItemIDs(page.Items)
		sortedStrings(got)
		if !reflect.DeepEqual(got, wantSets) {
			t.Fatalf("%s candidate set = %v, want %v", order, got, wantSets)
		}
		if page.TotalMatched != 3 || !page.Complete || page.Continuation != "" {
			t.Fatalf("%s bounds = total %d complete %v continuation %q", order, page.TotalMatched, page.Complete, page.Continuation)
		}
	}

	if got := experimentalItemIDs(pages[memoryOrderKey].Items); !reflect.DeepEqual(got, wantSets) {
		t.Fatalf("key order = %v", got)
	}
	if got := experimentalItemIDs(pages[memoryOrderNavigation].Items); !reflect.DeepEqual(got, []string{"c-alias-hit", "b-title-hit", "a-body-hit"}) {
		t.Fatalf("navigation order = %v", got)
	}
	if got := experimentalItemIDs(pages[memoryOrderBM25F].Items); got[0] != "c-alias-hit" {
		t.Fatalf("BM25F should put strongest alias match first, got %v", got)
	}

	// Ordering may change only rank and sequence. Every compact discovery field
	// for a given candidate must be byte-for-byte equal across arms.
	keyProjection := itemsByID(pages[memoryOrderKey].Items)
	for _, order := range []experimentalMemoryOrder{memoryOrderNavigation, memoryOrderBM25F} {
		for id, item := range itemsByID(pages[order].Items) {
			baseline := keyProjection[id]
			item.Rank, baseline.Rank = 0, 0
			if !reflect.DeepEqual(item, baseline) {
				t.Fatalf("%s projection for %s differs: %#v vs %#v", order, id, item, baseline)
			}
		}
	}
}

func TestExperimentalBM25FWeightsAreConfigurable(t *testing.T) {
	memories := map[string]string{
		"alpha": memoryFixture("release", nil, "active", 1, "ordinary body"),
		"beta":  memoryFixture("ordinary", nil, "active", 2, "release release release"),
	}
	base := defaultExperimentalBM25FConfig()
	page, err := buildExperimentalMemoryPage(memories, experimentalDiscoveryRequest{Search: "release", Order: memoryOrderBM25F, PageSize: 10, BM25F: base})
	if err != nil {
		t.Fatal(err)
	}
	if got := experimentalItemIDs(page.Items); got[0] != "alpha" {
		t.Fatalf("default title weight should win, got %v", got)
	}

	bodyHeavy := base
	bodyHeavy.TitleWeight = 0.01
	bodyHeavy.BodyWeight = 20
	page, err = buildExperimentalMemoryPage(memories, experimentalDiscoveryRequest{Search: "release", Order: memoryOrderBM25F, PageSize: 10, BM25F: bodyHeavy})
	if err != nil {
		t.Fatal(err)
	}
	if got := experimentalItemIDs(page.Items); got[0] != "beta" {
		t.Fatalf("body-heavy config should change only order, got %v", got)
	}
}

func TestExperimentalContinuationIsStableAndFailsAfterMutation(t *testing.T) {
	memories := map[string]string{
		"a": memoryFixture("Deploy A", nil, "active", 1, "deploy"),
		"b": memoryFixture("Deploy B", nil, "active", 2, "deploy"),
		"c": memoryFixture("Deploy C", nil, "active", 3, "deploy"),
	}
	req := experimentalDiscoveryRequest{Search: "deploy", Order: memoryOrderKey, PageSize: 2, BM25F: defaultExperimentalBM25FConfig()}
	first, err := buildExperimentalMemoryPage(memories, req)
	if err != nil {
		t.Fatal(err)
	}
	if got := experimentalItemIDs(first.Items); !reflect.DeepEqual(got, []string{"a", "b"}) || first.Complete || first.Continuation == "" {
		t.Fatalf("first page = %v complete=%v cursor=%q", got, first.Complete, first.Continuation)
	}

	req.Continuation = first.Continuation
	second, err := buildExperimentalMemoryPage(memories, req)
	if err != nil {
		t.Fatal(err)
	}
	if got := experimentalItemIDs(second.Items); !reflect.DeepEqual(got, []string{"c"}) || !second.Complete {
		t.Fatalf("second page = %v complete=%v", got, second.Complete)
	}

	mutated := make(map[string]string, len(memories))
	for key, value := range memories {
		mutated[key] = value
	}
	mutated["c"] += " corrected"
	if _, err := buildExperimentalMemoryPage(mutated, req); err == nil || !strings.Contains(err.Error(), "Memory state changed") {
		t.Fatalf("mutation error = %v, want explicit state-change refusal", err)
	}
}

func TestExperimentalContinuationRejectsIncompatibleReuse(t *testing.T) {
	memories := map[string]string{
		"a": memoryFixture("Deploy A", nil, "active", 1, "deploy"),
		"b": memoryFixture("Deploy B", nil, "active", 2, "deploy"),
	}
	base := experimentalDiscoveryRequest{Search: "deploy", Order: memoryOrderKey, PageSize: 1, BM25F: defaultExperimentalBM25FConfig()}
	first, err := buildExperimentalMemoryPage(memories, base)
	if err != nil {
		t.Fatal(err)
	}
	for name, changed := range map[string]experimentalDiscoveryRequest{
		"query": {Search: "Deploy", Order: memoryOrderKey, PageSize: 1, BM25F: base.BM25F, Continuation: first.Continuation},
		"order": {Search: "deploy", Order: memoryOrderNavigation, PageSize: 1, BM25F: base.BM25F, Continuation: first.Continuation},
		"size":  {Search: "deploy", Order: memoryOrderKey, PageSize: 2, BM25F: base.BM25F, Continuation: first.Continuation},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := buildExperimentalMemoryPage(memories, changed); err == nil || !strings.Contains(err.Error(), "incompatible continuation") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestExperimentalExcerptIsRuneBounded(t *testing.T) {
	body := strings.Repeat("界", experimentalExcerptRunes+5)
	page, err := buildExperimentalMemoryPage(map[string]string{
		"unicode": memoryFixture("Unicode", nil, "active", 1, body+" needle"),
	}, experimentalDiscoveryRequest{Search: "needle", Order: memoryOrderKey, PageSize: 1, BM25F: defaultExperimentalBM25FConfig()})
	if err != nil {
		t.Fatal(err)
	}
	runes := []rune(page.Items[0].Excerpt)
	if len(runes) != experimentalExcerptRunes || !strings.HasSuffix(page.Items[0].Excerpt, "...") {
		t.Fatalf("excerpt rune count/suffix = %d/%q", len(runes), page.Items[0].Excerpt)
	}
}

func TestExperimentalUnboundedReturnsEveryCandidateInOnePage(t *testing.T) {
	memories := map[string]string{
		"a": memoryFixture("Deploy A", nil, "active", 1, "deploy"),
		"b": memoryFixture("Deploy B", nil, "active", 2, "deploy"),
		"c": memoryFixture("Deploy C", nil, "active", 3, "deploy"),
	}
	page, err := buildExperimentalMemoryPage(memories, experimentalDiscoveryRequest{
		Search: "deploy", Order: memoryOrderKey, PageSize: 1, Unbounded: true,
		BM25F: defaultExperimentalBM25FConfig(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := experimentalItemIDs(page.Items); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("unbounded ids = %v", got)
	}
	if !page.Unbounded || page.PageSize != 3 || !page.Complete || page.Continuation != "" {
		t.Fatalf("unbounded bounds = unbounded=%v size=%d complete=%v cursor=%q", page.Unbounded, page.PageSize, page.Complete, page.Continuation)
	}
}

func memoryFixture(title string, aliases []string, lifecycle string, rank int, body string) string {
	return "---\n" +
		"title: " + title + "\n" +
		"aliases: [" + strings.Join(aliases, ", ") + "]\n" +
		"lifecycle: " + lifecycle + "\n" +
		"navigation_rank: " + strconv.Itoa(rank) + "\n" +
		"---\n" + body
}

func experimentalItemIDs(items []experimentalMemoryItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func itemsByID(items []experimentalMemoryItem) map[string]experimentalMemoryItem {
	byID := make(map[string]experimentalMemoryItem, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	return byID
}

func sortedStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
