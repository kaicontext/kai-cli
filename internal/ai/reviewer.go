package ai

import (
	"encoding/json"
	"fmt"
	"strings"

	"kai/internal/diff"
	"kai/internal/review"
)

// Reviewer provides AI-powered code review.
type Reviewer struct {
	client *Client
}

// NewReviewer creates a new AI reviewer.
func NewReviewer() (*Reviewer, error) {
	client, err := NewClient()
	if err != nil {
		return nil, err
	}
	return &Reviewer{client: client}, nil
}

// ReviewResult contains the AI review output.
type ReviewResult struct {
	Suggestions []review.AISuggestion
	Summary     string
	RiskLevel   string
}

// Review analyzes a semantic diff and returns suggestions.
func (r *Reviewer) Review(sd *diff.SemanticDiff) (*ReviewResult, error) {
	// Build the prompt from the semantic diff
	prompt := buildReviewPrompt(sd)

	system := `You are an expert code reviewer. Analyze the code changes and provide actionable suggestions.

Focus on:
- Security vulnerabilities (SQL injection, XSS, auth issues, secrets exposure)
- Performance issues (N+1 queries, unnecessary allocations, blocking calls)
- Bugs (nil pointer dereference, race conditions, logic errors)
- Best practices (error handling, naming, idiomatic code)

Respond with JSON in this exact format:
{
  "summary": "One sentence summary of the changes",
  "risk_level": "low|medium|high",
  "suggestions": [
    {
      "level": "info|warning|error",
      "category": "security|performance|bug|style",
      "file": "path/to/file.go",
      "symbol": "FunctionName",
      "message": "Specific, actionable suggestion"
    }
  ]
}

If there are no issues, return an empty suggestions array. Be concise and specific.`

	messages := []Message{
		{Role: "user", Content: prompt},
	}

	response, err := r.client.Complete(system, messages, 2000)
	if err != nil {
		return nil, fmt.Errorf("AI review failed: %w", err)
	}

	return parseReviewResponse(response)
}

func buildReviewPrompt(sd *diff.SemanticDiff) string {
	var sb strings.Builder

	sb.WriteString("Review these code changes:\n\n")

	for _, file := range sd.Files {
		sb.WriteString(fmt.Sprintf("## %s (%s)\n", file.Path, file.Action))

		for _, unit := range file.Units {
			action := string(unit.Action)
			if unit.Action == diff.ActionAdded {
				action = "+"
			} else if unit.Action == diff.ActionRemoved {
				action = "-"
			} else {
				action = "~"
			}

			sb.WriteString(fmt.Sprintf("\n%s %s %s\n", action, unit.Kind, unit.Name))

			if unit.BeforeSig != "" && unit.AfterSig != "" && unit.BeforeSig != unit.AfterSig {
				sb.WriteString(fmt.Sprintf("  Signature: %s -> %s\n", unit.BeforeSig, unit.AfterSig))
			} else if unit.AfterSig != "" {
				sb.WriteString(fmt.Sprintf("  Signature: %s\n", unit.AfterSig))
			}

			if unit.AfterBody != "" {
				// Truncate long bodies
				body := unit.AfterBody
				if len(body) > 1000 {
					body = body[:1000] + "\n... (truncated)"
				}
				sb.WriteString(fmt.Sprintf("  Body:\n```\n%s\n```\n", body))
			}
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

func parseReviewResponse(response string) (*ReviewResult, error) {
	// Extract JSON from response (handle markdown code blocks)
	jsonStr := response
	if idx := strings.Index(response, "```json"); idx != -1 {
		start := idx + 7
		end := strings.Index(response[start:], "```")
		if end != -1 {
			jsonStr = response[start : start+end]
		}
	} else if idx := strings.Index(response, "```"); idx != -1 {
		start := idx + 3
		end := strings.Index(response[start:], "```")
		if end != -1 {
			jsonStr = response[start : start+end]
		}
	}

	jsonStr = strings.TrimSpace(jsonStr)

	var parsed struct {
		Summary     string `json:"summary"`
		RiskLevel   string `json:"risk_level"`
		Suggestions []struct {
			Level    string `json:"level"`
			Category string `json:"category"`
			File     string `json:"file"`
			Symbol   string `json:"symbol"`
			Message  string `json:"message"`
			Line     int    `json:"line,omitempty"`
		} `json:"suggestions"`
	}

	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		// If JSON parsing fails, return empty result with the raw response as summary
		return &ReviewResult{
			Summary:   response,
			RiskLevel: "unknown",
		}, nil
	}

	result := &ReviewResult{
		Summary:   parsed.Summary,
		RiskLevel: parsed.RiskLevel,
	}

	for _, s := range parsed.Suggestions {
		result.Suggestions = append(result.Suggestions, review.AISuggestion{
			Level:    s.Level,
			Category: s.Category,
			File:     s.File,
			Symbol:   s.Symbol,
			Message:  s.Message,
			Line:     s.Line,
		})
	}

	return result, nil
}
