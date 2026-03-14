// Package cost tracks token usage and estimated costs from Claude CLI output.
//
// Claude's --output-format stream-json emits usage events with token counts.
// This package parses those events, stores per-bead and per-day aggregates
// in state.db, and optionally enforces a daily cost limit.
//
// Token pricing (Claude Sonnet 4, approximate):
//
//	Input:  $3.00 per 1M tokens
//	Output: $15.00 per 1M tokens
//	Cache read: $0.30 per 1M tokens
//	Cache write: $3.75 per 1M tokens
package cost

import (
	"bufio"
	"encoding/json"
	"io"
	"time"
)

// Pricing defines per-token costs in USD per million tokens.
type Pricing struct {
	InputPerM      float64 `json:"input_per_m"`
	OutputPerM     float64 `json:"output_per_m"`
	CacheReadPerM  float64 `json:"cache_read_per_m"`
	CacheWritePerM float64 `json:"cache_write_per_m"`
}

// DefaultPricing returns approximate Claude Sonnet 3.5 pricing.
func DefaultPricing() Pricing {
	return Pricing{
		InputPerM:      3.00,
		OutputPerM:     15.00,
		CacheReadPerM:  0.30,
		CacheWritePerM: 3.75,
	}
}

// CopilotPricing returns approximate GitHub Copilot pricing.
// Copilot runs Claude models under the hood; we use Claude's pricing as a
// reasonable cost estimate for token-level tracking.
func CopilotPricing() Pricing {
	return DefaultPricing()
}

// GeminiPricing returns approximate Gemini 1.5 Pro pricing.
func GeminiPricing() Pricing {
	return Pricing{
		InputPerM:      3.50,
		OutputPerM:     10.50,
		CacheReadPerM:  0.00, // Gemini caching pricing is different
		CacheWritePerM: 0.00,
	}
}

// OpenAIPricing returns approximate OpenAI GPT pricing (GPT-5.x class models).
func OpenAIPricing() Pricing {
	return Pricing{
		InputPerM:      2.50,
		OutputPerM:     10.00,
		CacheReadPerM:  0.00,
		CacheWritePerM: 0.00,
	}
}

// Usage tracks token usage for a single Claude invocation.
type Usage struct {
	InputTokens      int     `json:"input_tokens"`
	OutputTokens     int     `json:"output_tokens"`
	CacheReadTokens  int     `json:"cache_read_tokens"`
	CacheWriteTokens int     `json:"cache_write_tokens"`
	EstimatedCostUSD float64 `json:"estimated_cost_usd"`
}

// Add merges another Usage into this one.
func (u *Usage) Add(other Usage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheReadTokens += other.CacheReadTokens
	u.CacheWriteTokens += other.CacheWriteTokens
	u.EstimatedCostUSD += other.EstimatedCostUSD
}

// Calculate computes the estimated cost based on pricing.
func (u *Usage) Calculate(p Pricing) {
	u.EstimatedCostUSD = float64(u.InputTokens)*p.InputPerM/1_000_000 +
		float64(u.OutputTokens)*p.OutputPerM/1_000_000 +
		float64(u.CacheReadTokens)*p.CacheReadPerM/1_000_000 +
		float64(u.CacheWriteTokens)*p.CacheWritePerM/1_000_000
}

// BeadCost stores cumulative cost data for a specific bead.
type BeadCost struct {
	BeadID    string    `json:"bead_id"`
	Anvil     string    `json:"anvil"`
	Usage     Usage     `json:"usage"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DailyCost stores aggregated cost data for a specific day.
type DailyCost struct {
	Date  string  `json:"date"` // YYYY-MM-DD
	Usage Usage   `json:"usage"`
	Limit float64 `json:"limit,omitempty"` // 0 = no limit
}

// claudeStreamEvent is a partial parse of Claude's stream-json output.
type claudeStreamEvent struct {
	Type  string             `json:"type"`
	Usage *claudeStreamUsage `json:"usage,omitempty"`
}

type claudeStreamUsage struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_input_tokens"`
	CacheWriteTokens int `json:"cache_creation_input_tokens"`
}

// ParseStreamJSON reads Claude stream-json output and extracts total usage.
// It scans for events with "type": "result" or usage fields.
func ParseStreamJSON(r io.Reader) Usage {
	var total Usage
	pricing := DefaultPricing()

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var evt claudeStreamEvent
		if err := json.Unmarshal(line, &evt); err != nil {
			continue
		}

		if evt.Usage != nil {
			// Take the highest values seen (Claude reports cumulative in some events)
			if evt.Usage.InputTokens > total.InputTokens {
				total.InputTokens = evt.Usage.InputTokens
			}
			if evt.Usage.OutputTokens > total.OutputTokens {
				total.OutputTokens = evt.Usage.OutputTokens
			}
			if evt.Usage.CacheReadTokens > total.CacheReadTokens {
				total.CacheReadTokens = evt.Usage.CacheReadTokens
			}
			if evt.Usage.CacheWriteTokens > total.CacheWriteTokens {
				total.CacheWriteTokens = evt.Usage.CacheWriteTokens
			}
		}
	}

	total.Calculate(pricing)
	return total
}

// ParseResultJSON parses a single Claude result JSON (non-streaming).
func ParseResultJSON(data []byte) Usage {
	var result struct {
		Usage *claudeStreamUsage `json:"usage"`
	}
	if err := json.Unmarshal(data, &result); err != nil || result.Usage == nil {
		return Usage{}
	}

	u := Usage{
		InputTokens:      result.Usage.InputTokens,
		OutputTokens:     result.Usage.OutputTokens,
		CacheReadTokens:  result.Usage.CacheReadTokens,
		CacheWriteTokens: result.Usage.CacheWriteTokens,
	}
	u.Calculate(DefaultPricing())
	return u
}

// Today returns today's date string in YYYY-MM-DD format.
func Today() string {
	return time.Now().Format("2006-01-02")
}
