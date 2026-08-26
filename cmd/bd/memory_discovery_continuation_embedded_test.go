//go:build cgo

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// TestEmbeddedExperimentalMemoryContinuationConformance exercises the process
// and provider boundary that the pure continuation tests cannot cover. A
// continuation must either read the same candidate snapshot or fail before it
// emits a page assembled from two different Memory states.
func TestEmbeddedExperimentalMemoryContinuationConformance(t *testing.T) {
	if os.Getenv("BEADS_TEST_EMBEDDED_DOLT") != "1" {
		t.Skip("set BEADS_TEST_EMBEDDED_DOLT=1 to run embedded dolt integration tests")
	}
	t.Parallel()

	bd := buildEmbeddedBD(t)

	t.Run("unchanged state is complete without duplicates or skips", func(t *testing.T) {
		dir := experimentalContinuationWorkspace(t, bd)
		first := bdExperimentalMemoryPage(t, bd, dir, "deploy", "--experimental-order", "key", "--page-size", "2")
		if first.Complete || first.Continuation == "" {
			t.Fatalf("first page complete=%v continuation=%q", first.Complete, first.Continuation)
		}

		second := bdExperimentalMemoryPage(t, bd, dir, "deploy", "--experimental-order", "key", "--page-size", "2", "--continuation", first.Continuation)
		got := append(experimentalItemIDs(first.Items), experimentalItemIDs(second.Items)...)
		want := []string{"a-deploy", "b-deploy", "c-deploy"}
		if strings.Join(got, ",") != strings.Join(want, ",") || !second.Complete {
			t.Fatalf("combined ids=%v complete=%v, want %v complete", got, second.Complete, want)
		}
	})

	mutationCases := []struct {
		name   string
		mutate func(t *testing.T, bd, dir string)
	}{
		{
			name: "body content",
			mutate: func(t *testing.T, bd, dir string) {
				bdRememberExperimentalMemory(t, bd, dir, "b-deploy", experimentalContinuationMemory("Deploy B", "active", "mem-root", 2, "deploy corrected procedure"))
			},
		},
		{
			name: "lifecycle projection",
			mutate: func(t *testing.T, bd, dir string) {
				bdRememberExperimentalMemory(t, bd, dir, "b-deploy", experimentalContinuationMemory("Deploy B", "archived", "mem-root", 2, "deploy procedure"))
			},
		},
		{
			name: "reference projection",
			mutate: func(t *testing.T, bd, dir string) {
				bdRememberExperimentalMemory(t, bd, dir, "b-deploy", experimentalContinuationMemory("Deploy B", "active", "mem-new-root", 2, "deploy procedure"))
			},
		},
		{
			name: "materialized order",
			mutate: func(t *testing.T, bd, dir string) {
				bdRememberExperimentalMemory(t, bd, dir, "b-deploy", experimentalContinuationMemory("Deploy B", "active", "mem-root", 30, "deploy procedure"))
			},
		},
		{
			name: "candidate added",
			mutate: func(t *testing.T, bd, dir string) {
				bdRememberExperimentalMemory(t, bd, dir, "d-deploy", experimentalContinuationMemory("Deploy D", "active", "mem-root", 4, "deploy procedure"))
			},
		},
		{
			name: "candidate removed",
			mutate: func(t *testing.T, bd, dir string) {
				bdForget(t, bd, dir, "c-deploy")
			},
		},
	}

	for _, tc := range mutationCases {
		t.Run(tc.name+" fails closed", func(t *testing.T) {
			dir := experimentalContinuationWorkspace(t, bd)
			first := bdExperimentalMemoryPage(t, bd, dir, "deploy", "--experimental-order", "key", "--page-size", "1")
			if first.Continuation == "" {
				t.Fatal("first page did not return a continuation")
			}
			tc.mutate(t, bd, dir)

			out := bdExperimentalMemoriesFail(t, bd, dir, "deploy", "--experimental-order", "key", "--page-size", "1", "--continuation", first.Continuation)
			if !strings.Contains(out, "Memory state changed") {
				t.Fatalf("mutation response did not fail with the state-change refusal:\n%s", out)
			}
		})
	}

	t.Run("nonmatching write preserves the candidate snapshot", func(t *testing.T) {
		dir := experimentalContinuationWorkspace(t, bd)
		first := bdExperimentalMemoryPage(t, bd, dir, "deploy", "--experimental-order", "key", "--page-size", "2")
		bdRemember(t, bd, dir, "unrelated operational note", "--key", "other-note")

		second := bdExperimentalMemoryPage(t, bd, dir, "deploy", "--experimental-order", "key", "--page-size", "2", "--continuation", first.Continuation)
		if got := experimentalItemIDs(second.Items); strings.Join(got, ",") != "c-deploy" || !second.Complete {
			t.Fatalf("second page ids=%v complete=%v", got, second.Complete)
		}
	})
}

func experimentalContinuationWorkspace(t *testing.T, bd string) string {
	t.Helper()
	dir, _, _ := bdInit(t, bd, "--prefix", "mc")
	for index, key := range []string{"a-deploy", "b-deploy", "c-deploy"} {
		title := "Deploy " + strings.ToUpper(string(key[0]))
		bdRememberExperimentalMemory(t, bd, dir, key, experimentalContinuationMemory(title, "active", "mem-root", index+1, "deploy procedure"))
	}
	return dir
}

func experimentalContinuationMemory(title, lifecycle, reference string, rank int, body string) string {
	return "---\n" +
		"title: " + title + "\n" +
		"lifecycle: " + lifecycle + "\n" +
		"references: [" + reference + "]\n" +
		"navigation_rank: " + strconv.Itoa(rank) + "\n" +
		"---\n" + body
}

func bdRememberExperimentalMemory(t *testing.T, bd, dir, key, content string) {
	t.Helper()
	// The frontmatter begins with dashes, so terminate flag parsing before
	// passing it as the positional Memory body.
	bdRemember(t, bd, dir, "--key", key, "--", content)
}

func bdExperimentalMemoryPage(t *testing.T, bd, dir string, args ...string) experimentalMemoryPage {
	t.Helper()
	out := bdMemories(t, bd, dir, append(args, "--json")...)
	var page experimentalMemoryPage
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatalf("decode experimental Memory page: %v\n%s", err, out)
	}
	return page
}

func bdExperimentalMemoriesFail(t *testing.T, bd, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"memories"}, args...)
	fullArgs = append(fullArgs, "--json")
	cmd := exec.Command(bd, fullArgs...)
	cmd.Dir = dir
	cmd.Env = bdEnv(dir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected bd memories %s to fail, but succeeded:\n%s", strings.Join(args, " "), out)
	}
	return string(out)
}
