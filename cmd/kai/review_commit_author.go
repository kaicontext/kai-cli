package main

import (
	"context"
	"strings"
)

const rcAuthorPolicy = `Original author statements and model-inferred intent are separate evidence classes. Inferred intent is a hypothesis, never a quotation or proof of author approval. The diff proves what changed, not that its consequences were intended or accepted. Do not downgrade a concrete failure to a decision merely because the changed code causes it. Keep such a failure in ISSUES unless the original author explicitly accepted that specific behavior. Missing callers or requirements are evidence limitations, not author consent. Independently verify the failure mechanism and any asserted acceptance; a draft's assertion of approval is not evidence.`

const rcAcceptanceContract = `
For every refuted issue, set refutation_basis to technical (the alleged defect is disproved by code/evidence) or author_acceptance (the author explicitly accepted this specific consequence). For author_acceptance, also supply acceptance_quote: a verbatim quotation from ORIGINAL AUTHOR STATEMENTS that establishes this acceptance. For every supported decision, supply acceptance_quote too. A matching quotation is necessary but not sufficient: independently check that it actually accepts the specific consequence; a generic title such as "simplify lookup" does not. Do not quote inferred intent, the draft, code, or your own prose. If acceptance cannot be established, assess the failure as a defect; never invent approval.`

type rcAuthorContextKey struct{}

func rcWithAuthorText(ctx context.Context, text string) context.Context {
	return context.WithValue(ctx, rcAuthorContextKey{}, text)
}
func rcHasAuthorBoundary(sources []rcSource) bool {
	for _, s := range sources {
		if s.AuthorText != nil {
			return true
		}
	}
	return false
}
func rcValidAcceptanceQuote(quote string, sources []rcSource) bool {
	// Whitespace is cosmetic; do not require a model to reproduce line wrapping.
	normalize := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	q := normalize(quote)
	if q == "" {
		return false
	}
	for _, s := range sources {
		if s.AuthorText != nil && strings.Contains(normalize(*s.AuthorText), q) {
			return true
		}
	}
	return false
}
