package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	experimentalExcerptRunes  = 160
	experimentalCursorVersion = "expmem-v1"
)

type experimentalMemoryOrder string

const (
	memoryOrderKey        experimentalMemoryOrder = "key"
	memoryOrderNavigation experimentalMemoryOrder = "navigation"
	memoryOrderBM25F      experimentalMemoryOrder = "bm25f"
)

type experimentalBM25FConfig struct {
	KeyWeight   float64 `json:"key_weight"`
	AliasWeight float64 `json:"alias_weight"`
	TitleWeight float64 `json:"title_weight"`
	BodyWeight  float64 `json:"body_weight"`
	K1          float64 `json:"k1"`
	B           float64 `json:"b"`
}

func defaultExperimentalBM25FConfig() experimentalBM25FConfig {
	return experimentalBM25FConfig{
		KeyWeight: 6, AliasWeight: 5, TitleWeight: 3, BodyWeight: 1,
		K1: 1.2, B: 0.75,
	}
}

type experimentalMemory struct {
	ID             string
	Key            string
	Title          string
	Aliases        []string
	Lifecycle      string
	NavigationRank *int
	References     []string
	Provenance     string
	Body           string
}

type experimentalMemoryItem struct {
	ID            string   `json:"id"`
	Key           string   `json:"key,omitempty"`
	Title         string   `json:"title"`
	Lifecycle     string   `json:"lifecycle"`
	Excerpt       string   `json:"excerpt"`
	MatchedFields []string `json:"matched_fields"`
	Rank          int      `json:"rank"`
}

type experimentalMemoryPage struct {
	Items                 []experimentalMemoryItem `json:"items"`
	Query                 string                   `json:"query"`
	Order                 experimentalMemoryOrder  `json:"order"`
	PageSize              int                      `json:"page_size"`
	Unbounded             bool                     `json:"unbounded"`
	TotalMatched          int                      `json:"total_matched"`
	Complete              bool                     `json:"complete"`
	Continuation          string                   `json:"continuation,omitempty"`
	CandidateDigest       string                   `json:"candidate_digest"`
	BM25F                 experimentalBM25FConfig  `json:"bm25f"`
	CandidateGenerationMs float64                  `json:"candidate_generation_ms"`
	OrderingMs            float64                  `json:"ordering_ms"`
}

type experimentalDiscoveryRequest struct {
	Search       string
	Order        experimentalMemoryOrder
	PageSize     int
	Unbounded    bool
	Continuation string
	BM25F        experimentalBM25FConfig
}

type experimentalCursor struct {
	Version         string                  `json:"version"`
	Search          string                  `json:"search"`
	Order           experimentalMemoryOrder `json:"order"`
	PageSize        int                     `json:"page_size"`
	Unbounded       bool                    `json:"unbounded"`
	ConfigDigest    string                  `json:"config_digest"`
	CandidateDigest string                  `json:"candidate_digest"`
	Offset          int                     `json:"offset"`
}

// parseExperimentalMemory reads the deliberately small frontmatter subset used
// by this experiment. Unstructured legacy values remain valid and get key/active
// fallbacks. Frontmatter is corpus data; this does not create a Beads schema.
func parseExperimentalMemory(key, value string) experimentalMemory {
	memory := experimentalMemory{
		ID: key, Key: key, Title: key, Lifecycle: "active", Body: value,
	}
	normalized := strings.ReplaceAll(value, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return memory
	}
	end := strings.Index(normalized[4:], "\n---\n")
	if end < 0 {
		return memory
	}
	header := normalized[4 : 4+end]
	memory.Body = normalized[4+end+5:]
	for _, line := range strings.Split(header, "\n") {
		name, raw, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		raw = strings.TrimSpace(raw)
		switch name {
		case "title":
			if title := trimFrontmatterScalar(raw); title != "" {
				memory.Title = title
			}
		case "aliases":
			memory.Aliases = parseFrontmatterList(raw)
		case "lifecycle":
			if lifecycle := trimFrontmatterScalar(raw); lifecycle != "" {
				memory.Lifecycle = lifecycle
			}
		case "navigation_rank":
			if rank, err := strconv.Atoi(trimFrontmatterScalar(raw)); err == nil {
				memory.NavigationRank = &rank
			}
		case "references":
			memory.References = parseFrontmatterList(raw)
		case "provenance":
			memory.Provenance = trimFrontmatterScalar(raw)
		}
	}
	return memory
}

func parseFrontmatterList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		raw = strings.TrimSpace(raw[1 : len(raw)-1])
	}
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	values := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := trimFrontmatterScalar(part); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func trimFrontmatterScalar(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) >= 2 && ((raw[0] == '"' && raw[len(raw)-1] == '"') || (raw[0] == '\'' && raw[len(raw)-1] == '\'')) {
		return raw[1 : len(raw)-1]
	}
	return raw
}

func buildExperimentalMemoryPage(memories map[string]string, req experimentalDiscoveryRequest) (experimentalMemoryPage, error) {
	orderingStarted := time.Now()
	if req.PageSize < 1 {
		return experimentalMemoryPage{}, errors.New("experimental page size must be >= 1")
	}
	if req.Order != memoryOrderKey && req.Order != memoryOrderNavigation && req.Order != memoryOrderBM25F {
		return experimentalMemoryPage{}, fmt.Errorf("unknown experimental Memory order %q", req.Order)
	}
	if err := validateExperimentalBM25F(req.BM25F); err != nil {
		return experimentalMemoryPage{}, err
	}

	candidateDigest := digestExperimentalCandidates(memories)
	configDigest := digestExperimentalConfig(req)
	offset := 0
	if req.Continuation != "" {
		cursor, err := decodeExperimentalCursor(req.Continuation)
		if err != nil {
			return experimentalMemoryPage{}, fmt.Errorf("invalid experimental continuation: %w", err)
		}
		if cursor.Search != req.Search || cursor.Order != req.Order || cursor.PageSize != req.PageSize || cursor.Unbounded != req.Unbounded || cursor.ConfigDigest != configDigest {
			return experimentalMemoryPage{}, errors.New("incompatible continuation: query, ordering, page size, or scorer configuration changed")
		}
		if cursor.CandidateDigest != candidateDigest {
			return experimentalMemoryPage{}, errors.New("Memory state changed since the first page; restart discovery instead of mixing result orders")
		}
		offset = cursor.Offset
	}

	parsed := make([]experimentalMemory, 0, len(memories))
	for key, value := range memories {
		parsed = append(parsed, parseExperimentalMemory(key, value))
	}
	scores := map[string]float64(nil)
	if req.Order == memoryOrderBM25F {
		scores = scoreExperimentalBM25F(parsed, req.Search, req.BM25F)
	}
	sort.Slice(parsed, func(i, j int) bool {
		left, right := parsed[i], parsed[j]
		switch req.Order {
		case memoryOrderNavigation:
			leftRank, rightRank := navigationRank(left), navigationRank(right)
			if leftRank != rightRank {
				return leftRank < rightRank
			}
		case memoryOrderBM25F:
			if scores[left.ID] != scores[right.ID] {
				return scores[left.ID] > scores[right.ID]
			}
		}
		return left.ID < right.ID
	})

	if offset < 0 || offset > len(parsed) {
		return experimentalMemoryPage{}, errors.New("invalid experimental continuation: offset is outside the candidate set")
	}
	effectivePageSize := req.PageSize
	if req.Unbounded {
		effectivePageSize = len(parsed)
		if effectivePageSize == 0 {
			effectivePageSize = 1
		}
	}
	end := offset + effectivePageSize
	if end > len(parsed) {
		end = len(parsed)
	}
	items := make([]experimentalMemoryItem, 0, end-offset)
	for index := offset; index < end; index++ {
		memory := parsed[index]
		items = append(items, experimentalMemoryItem{
			ID: memory.ID, Key: memory.Key, Title: memory.Title,
			Lifecycle: memory.Lifecycle, Excerpt: experimentalExcerpt(memory.Body),
			MatchedFields: experimentalMatchedFields(memory, req.Search), Rank: index + 1,
		})
	}
	page := experimentalMemoryPage{
		Items: items, Query: req.Search, Order: req.Order, PageSize: effectivePageSize,
		Unbounded:    req.Unbounded,
		TotalMatched: len(parsed), Complete: end == len(parsed),
		CandidateDigest: candidateDigest, BM25F: req.BM25F,
	}
	if !page.Complete {
		page.Continuation = encodeExperimentalCursor(experimentalCursor{
			Version: experimentalCursorVersion, Search: req.Search, Order: req.Order,
			PageSize: req.PageSize, Unbounded: req.Unbounded, ConfigDigest: configDigest,
			CandidateDigest: candidateDigest, Offset: end,
		})
	}
	page.OrderingMs = float64(time.Since(orderingStarted).Nanoseconds()) / 1_000_000
	return page, nil
}

func validateExperimentalBM25F(config experimentalBM25FConfig) error {
	weights := []float64{config.KeyWeight, config.AliasWeight, config.TitleWeight, config.BodyWeight}
	for _, weight := range weights {
		if math.IsNaN(weight) || math.IsInf(weight, 0) || weight < 0 {
			return errors.New("experimental BM25F field weights must be finite and >= 0")
		}
	}
	if config.K1 <= 0 || math.IsNaN(config.K1) || math.IsInf(config.K1, 0) {
		return errors.New("experimental BM25F k1 must be finite and > 0")
	}
	if config.B < 0 || config.B > 1 || math.IsNaN(config.B) || math.IsInf(config.B, 0) {
		return errors.New("experimental BM25F b must be between 0 and 1")
	}
	return nil
}

func navigationRank(memory experimentalMemory) int {
	if memory.NavigationRank == nil {
		return int(^uint(0) >> 1)
	}
	return *memory.NavigationRank
}

func experimentalMatchedFields(memory experimentalMemory, search string) []string {
	if search == "" {
		return nil
	}
	needle := strings.ToLower(search)
	fields := make([]string, 0, 4)
	if strings.Contains(strings.ToLower(memory.Key), needle) {
		fields = append(fields, "key")
	}
	for _, alias := range memory.Aliases {
		if strings.Contains(strings.ToLower(alias), needle) {
			fields = append(fields, "alias")
			break
		}
	}
	if strings.Contains(strings.ToLower(memory.Title), needle) {
		fields = append(fields, "title")
	}
	if strings.Contains(strings.ToLower(memory.Body), needle) {
		fields = append(fields, "body")
	}
	if len(fields) == 0 {
		return []string{"frontmatter"}
	}
	return fields
}

func experimentalExcerpt(body string) string {
	compact := strings.Join(strings.Fields(body), " ")
	runes := []rune(compact)
	if len(runes) <= experimentalExcerptRunes {
		return compact
	}
	return string(runes[:experimentalExcerptRunes-3]) + "..."
}

func scoreExperimentalBM25F(memories []experimentalMemory, query string, config experimentalBM25FConfig) map[string]float64 {
	type fields struct {
		key, aliases, title, body []string
	}
	docFields := make(map[string]fields, len(memories))
	queryTerms := uniqueExperimentalTokens(query)
	averages := [4]float64{}
	for _, memory := range memories {
		f := fields{
			key: experimentalTokens(memory.Key), aliases: experimentalTokens(strings.Join(memory.Aliases, " ")),
			title: experimentalTokens(memory.Title), body: experimentalTokens(memory.Body),
		}
		docFields[memory.ID] = f
		averages[0] += float64(len(f.key))
		averages[1] += float64(len(f.aliases))
		averages[2] += float64(len(f.title))
		averages[3] += float64(len(f.body))
	}
	if len(memories) > 0 {
		for index := range averages {
			averages[index] /= float64(len(memories))
			if averages[index] == 0 {
				averages[index] = 1
			}
		}
	}

	scores := make(map[string]float64, len(memories))
	for _, term := range queryTerms {
		documentFrequency := 0
		for _, memory := range memories {
			f := docFields[memory.ID]
			if tokenFrequency(f.key, term)+tokenFrequency(f.aliases, term)+tokenFrequency(f.title, term)+tokenFrequency(f.body, term) > 0 {
				documentFrequency++
			}
		}
		idf := math.Log(1 + (float64(len(memories)-documentFrequency)+0.5)/(float64(documentFrequency)+0.5))
		for _, memory := range memories {
			f := docFields[memory.ID]
			weightedTF := normalizedFieldTF(f.key, term, config.KeyWeight, averages[0], config.B) +
				normalizedFieldTF(f.aliases, term, config.AliasWeight, averages[1], config.B) +
				normalizedFieldTF(f.title, term, config.TitleWeight, averages[2], config.B) +
				normalizedFieldTF(f.body, term, config.BodyWeight, averages[3], config.B)
			if weightedTF > 0 {
				scores[memory.ID] += idf * ((config.K1 + 1) * weightedTF) / (config.K1 + weightedTF)
			}
		}
	}
	return scores
}

func normalizedFieldTF(tokens []string, term string, weight, averageLength, b float64) float64 {
	frequency := float64(tokenFrequency(tokens, term))
	if frequency == 0 || weight == 0 {
		return 0
	}
	normalizer := 1 - b + b*float64(len(tokens))/averageLength
	return weight * frequency / normalizer
}

func tokenFrequency(tokens []string, term string) int {
	count := 0
	for _, token := range tokens {
		if token == term {
			count++
		}
	}
	return count
}

func uniqueExperimentalTokens(value string) []string {
	seen := make(map[string]struct{})
	unique := make([]string, 0)
	for _, token := range experimentalTokens(value) {
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		unique = append(unique, token)
	}
	return unique
}

func experimentalTokens(value string) []string {
	return strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func digestExperimentalCandidates(memories map[string]string) string {
	keys := make([]string, 0, len(memories))
	for key := range memories {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	hash := sha256.New()
	for _, key := range keys {
		fmt.Fprintf(hash, "%d:%s%d:%s", len(key), key, len(memories[key]), memories[key])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func digestExperimentalConfig(req experimentalDiscoveryRequest) string {
	payload, _ := json.Marshal(struct {
		Search    string                  `json:"search"`
		Order     experimentalMemoryOrder `json:"order"`
		PageSize  int                     `json:"page_size"`
		Unbounded bool                    `json:"unbounded"`
		BM25F     experimentalBM25FConfig `json:"bm25f"`
	}{req.Search, req.Order, req.PageSize, req.Unbounded, req.BM25F})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func encodeExperimentalCursor(cursor experimentalCursor) string {
	payload, _ := json.Marshal(cursor)
	return experimentalCursorVersion + "." + base64.RawURLEncoding.EncodeToString(payload)
}

func decodeExperimentalCursor(token string) (experimentalCursor, error) {
	encoded, ok := strings.CutPrefix(token, experimentalCursorVersion+".")
	if !ok {
		return experimentalCursor{}, errors.New("unknown cursor version")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return experimentalCursor{}, errors.New("malformed cursor encoding")
	}
	var cursor experimentalCursor
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return experimentalCursor{}, errors.New("malformed cursor payload")
	}
	if cursor.Version != experimentalCursorVersion || cursor.PageSize < 1 || cursor.Offset < 1 || cursor.CandidateDigest == "" || cursor.ConfigDigest == "" {
		return experimentalCursor{}, errors.New("incomplete cursor payload")
	}
	return cursor, nil
}
