package api

import (
	"encoding/csv"
	"strings"

	tsvsheet "github.com/tsvsheet/go-tsvsheet"

	"github.com/tsvsheet/tsvsheet.api/internal/constants"
)

// refSpan is a parsed reference suffix (`!B7`, `!A1:C10`) resolved to a
// rectangle of 0-based coordinates.
type refSpan struct {
	from tsvsheet.Address
	to   tsvsheet.Address
}

// refText is a reference suffix as written in a URL.
type refText string

// parseRef reads a reference suffix: one address, or two joined by ":".
func parseRef(ref refText) (refSpan, error) {
	fromText, toText, isRange := strings.Cut(string(ref), ":")
	from, err := tsvsheet.ParseAddress(tsvsheet.AddressText(fromText))
	if err != nil {
		return refSpan{}, constants.ErrDocPath.With(err, "ref", string(ref))
	}
	if !isRange {
		return refSpan{from: from, to: from}, nil
	}
	to, err := tsvsheet.ParseAddress(tsvsheet.AddressText(toText))
	if err != nil {
		return refSpan{}, constants.ErrDocPath.With(err, "ref", string(ref))
	}
	return refSpan{from: from, to: to}, nil
}

// shape classifies the rectangle for §9's one-type-per-shape rule.
type shape int

// The four §9 result shapes.
const (
	shapeCell shape = iota
	shapeRow
	shapeColumn
	shapeRange
)

// shapeOf classifies the span.
func (s refSpan) shapeOf() shape {
	oneRow, oneCol := s.from.Row == s.to.Row, s.from.Col == s.to.Col
	switch {
	case oneRow && oneCol:
		return shapeCell
	case oneRow:
		return shapeRow
	case oneCol:
		return shapeColumn
	default:
		return shapeRange
	}
}

// typeOf is the vendor media type serving the shape.
func (s shape) typeOf() string {
	return map[shape]string{
		shapeCell: TypeCell, shapeRow: TypeRow, shapeColumn: TypeColumn, shapeRange: TypeRange,
	}[s]
}

// gridIndex is a 0-based row or column coordinate.
type gridIndex int

// valueAt reads one computed value, empty for a coordinate beyond the grid.
func valueAt(grid tsvsheet.Grid, row, col gridIndex) string {
	if int(row) < len(grid) && int(col) < len(grid[row]) {
		return grid[row][col]
	}
	return ""
}

// renderSpan serializes the span's computed values as a TSV fragment: rows
// tab-joined, one per line.
func renderSpan(grid tsvsheet.Grid, span refSpan) string {
	var b strings.Builder
	for row := span.from.Row; row <= span.to.Row; row++ {
		cells := make([]string, 0, span.to.Col-span.from.Col+1)
		for col := span.from.Col; col <= span.to.Col; col++ {
			cells = append(cells, valueAt(grid, gridIndex(row), gridIndex(col)))
		}
		_, _ = b.WriteString(strings.Join(cells, "\t"))
		_, _ = b.WriteString("\n")
	}
	return b.String()
}

// renderGridTSV serializes a whole computed grid as TSV.
func renderGridTSV(grid tsvsheet.Grid) string {
	var b strings.Builder
	for _, row := range grid {
		_, _ = b.WriteString(strings.Join(row, "\t"))
		_, _ = b.WriteString("\n")
	}
	return b.String()
}

// renderGridCSV serializes a whole computed grid as RFC 4180 CSV.
func renderGridCSV(grid tsvsheet.Grid) string {
	var b strings.Builder
	writer := csv.NewWriter(&b)
	for _, row := range grid {
		_ = writer.Write(row)
	}
	writer.Flush()
	return b.String()
}

// cellsDelta renders the computed cells that differ between two grids as the
// cells+tsv format: one `address<TAB>value` line per changed cell, in grid
// order. A cell present only in the old grid appears with its new (empty)
// value.
func cellsDelta(old, next tsvsheet.Grid) string {
	rows := max(len(old), len(next))
	var b strings.Builder
	for row := range rows {
		for col := range max(rowWidth(old, gridIndex(row)), rowWidth(next, gridIndex(row))) {
			before, after := valueAt(old, gridIndex(row), gridIndex(col)), valueAt(next, gridIndex(row), gridIndex(col))
			if before == after {
				continue
			}
			_, _ = b.WriteString(string(addressText(gridIndex(row), gridIndex(col))) + "\t" + after + "\n")
		}
	}
	return b.String()
}

// rowWidth is the number of cells in the grid's row (0 beyond the grid).
func rowWidth(grid tsvsheet.Grid, row gridIndex) int {
	if int(row) < len(grid) {
		return len(grid[row])
	}
	return 0
}

// addressText renders 0-based coordinates in A1 notation through the engine's
// own Address form.
func addressText(row, col gridIndex) tsvsheet.AddressText {
	return tsvsheet.AddressText(tsvsheet.Address{Row: int(row), Col: int(col)}.String())
}
