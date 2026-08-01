package api

import "strings"

// The +tsv media-type family the API speaks. TypeDoc and TypeEdits are the
// document plane's own; the §9 vendor types (TypeSheet through TypeRange) mean
// computed values, in the shapes the language's IMPORT* builtins request —
// TestComputedRangeRowColumnShapes and TestGetValuesAcceptWithoutComputeIs406
// assert the shapes and the source-versus-values refusal.
const (
	TypeDoc     = "application/vnd.tsvsheet.doc+tsv"
	TypeEdits   = "application/vnd.tsvsheet.edits+tsv"
	TypeCells   = "application/vnd.tsvsheet.cells+tsv"
	TypeSheet   = "application/vnd.tsvsheet+tsv"
	TypeCell    = "application/vnd.tsvsheet.cell+tsv"
	TypeRow     = "application/vnd.tsvsheet.row+tsv"
	TypeColumn  = "application/vnd.tsvsheet.column+tsv"
	TypeRange   = "application/vnd.tsvsheet.range+tsv"
	TypeTSV     = "text/tab-separated-values"
	TypeCSV     = "text/csv"
	TypeStream  = "text/event-stream"
	TypeProblem = "application/problem+json"
)

// acceptHeader is a raw Accept header value.
type acceptHeader string

// mediaExpr is one media-type expression within a header.
type mediaExpr string

// acceptSet is the ordered base media types of one Accept header, parameters
// stripped, lowercased.
type acceptSet []string

// parseAccept reads an Accept header into its base types. Quality values and
// other parameters are dropped: the API's negotiation is by the fixed
// precedence its planes define, not by client weighting.
func parseAccept(header acceptHeader) acceptSet {
	var set acceptSet
	for _, part := range strings.Split(string(header), ",") {
		base := mediaBase(mediaExpr(part))
		if base != "" {
			set = append(set, base)
		}
	}
	return set
}

// has reports whether the set names the exact base type.
func (a acceptSet) has(base string) bool {
	for _, t := range a {
		if t == base {
			return true
		}
	}
	return false
}

// hasAny reports whether the set names any of the base types.
func (a acceptSet) hasAny(bases ...string) bool {
	for _, b := range bases {
		if a.has(b) {
			return true
		}
	}
	return false
}

// wildcard reports whether the set accepts anything (empty header included).
func (a acceptSet) wildcard() bool { return len(a) == 0 || a.has("*/*") }

// mediaBase strips parameters and normalizes one media-type expression.
func mediaBase(expr mediaExpr) string {
	base, _, _ := strings.Cut(string(expr), ";")
	return strings.ToLower(strings.TrimSpace(base))
}
