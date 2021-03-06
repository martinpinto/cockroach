// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Raphael 'kena' Poss (knz@cockroachlabs.com)

package sql

import (
	"bytes"
	"fmt"

	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util"
)

const (
	leftSide = iota
	rightSide
)

type joinPredicate interface {
	// eval tests whether the current combination of rows passes the
	// predicate. The result argument is an array pre-allocated to the
	// right size, which can be used as intermediate buffer.
	eval(result, leftRow, rightRow parser.DTuple) (bool, error)

	// prepareRow prepares the output row by combining values from the
	// input data sources.
	prepareRow(result, leftRow, rightRow parser.DTuple)

	// encode returns the encoding of a row from a given side (left or right),
	// according to the columns specified by the equality constraints.
	encode(scratch []byte, row parser.DTuple, side int) (encoding []byte, containsNull bool, err error)

	// expand and start propagate to embedded sub-queries.
	expand() error
	start() error

	// format pretty-prints the predicate for EXPLAIN.
	format(buf *bytes.Buffer)
	// explainTypes registers the expression types for EXPLAIN.
	explainTypes(f func(string, string))
}

var _ joinPredicate = &onPredicate{}
var _ joinPredicate = &crossPredicate{}
var _ joinPredicate = &equalityPredicate{}

// prepareRowConcat implement the simple case of CROSS JOIN or JOIN
// with an ON clause, where the rows of the two inputs are simply
// concatenated.
func prepareRowConcat(result parser.DTuple, leftRow parser.DTuple, rightRow parser.DTuple) {
	copy(result, leftRow)
	copy(result[len(leftRow):], rightRow)
}

// crossPredicate implements the predicate logic for CROSS JOIN. The
// predicate is always true, the work done here is thus minimal.
type crossPredicate struct{}

func (p *crossPredicate) eval(_, _, _ parser.DTuple) (bool, error) {
	return true, nil
}
func (p *crossPredicate) prepareRow(result, leftRow, rightRow parser.DTuple) {
	prepareRowConcat(result, leftRow, rightRow)
}
func (p *crossPredicate) start() error                        { return nil }
func (p *crossPredicate) expand() error                       { return nil }
func (p *crossPredicate) format(_ *bytes.Buffer)              {}
func (p *crossPredicate) explainTypes(_ func(string, string)) {}
func (p *crossPredicate) encode(_ []byte, _ parser.DTuple, _ int) ([]byte, bool, error) {
	return nil, false, nil
}

// onPredicate implements the predicate logic for joins with an ON clause.
type onPredicate struct {
	p      *planner
	filter parser.TypedExpr
	info   *dataSourceInfo
	curRow parser.DTuple

	// This struct must be allocated on the heap and its location stay
	// stable after construction because it implements
	// IndexedVarContainer and the IndexedVar objects in sub-expressions
	// will link to it by reference after checkRenderStar / analyzeExpr.
	// Enforce this using NoCopy.
	noCopy util.NoCopy
}

// IndexedVarEval implements the parser.IndexedVarContainer interface.
func (p *onPredicate) IndexedVarEval(idx int, ctx *parser.EvalContext) (parser.Datum, error) {
	return p.curRow[idx].Eval(ctx)
}

// IndexedVarResolvedType implements the parser.IndexedVarContainer interface.
func (p *onPredicate) IndexedVarResolvedType(idx int) parser.Type {
	return p.info.sourceColumns[idx].Typ
}

// IndexedVarFormat implements the parser.IndexedVarContainer interface.
func (p *onPredicate) IndexedVarFormat(buf *bytes.Buffer, f parser.FmtFlags, idx int) {
	p.info.FormatVar(buf, f, idx)
}

func (p *onPredicate) encode(_ []byte, _ parser.DTuple, _ int) ([]byte, bool, error) {
	panic("ON predicate extraction unimplemented")
}

// eval for onPredicate uses an arbitrary SQL expression to determine
// whether the left and right input row can join.
func (p *onPredicate) eval(result, leftRow, rightRow parser.DTuple) (bool, error) {
	p.curRow = result
	prepareRowConcat(p.curRow, leftRow, rightRow)
	return sqlbase.RunFilter(p.filter, &p.p.evalCtx)
}

func (p *onPredicate) prepareRow(result, leftRow, rightRow parser.DTuple) {
	prepareRowConcat(result, leftRow, rightRow)
}

func (p *onPredicate) expand() error {
	return p.p.expandSubqueryPlans(p.filter)
}

func (p *onPredicate) start() error {
	return p.p.startSubqueryPlans(p.filter)
}

func (p *onPredicate) format(buf *bytes.Buffer) {
	buf.WriteString(" ON ")
	p.filter.Format(buf, parser.FmtQualify)
}

func (p *onPredicate) explainTypes(regTypes func(string, string)) {
	if p.filter != nil {
		regTypes("filter", parser.AsStringWithFlags(p.filter, parser.FmtShowTypes))
	}
}

// makeOnPredicate constructs a joinPredicate object for joins with a
// ON clause.
func (p *planner) makeOnPredicate(
	left, right *dataSourceInfo, expr parser.Expr,
) (joinPredicate, *dataSourceInfo, error) {
	// Output rows are the concatenation of input rows.
	info, err := concatDataSourceInfos(left, right)
	if err != nil {
		return nil, nil, err
	}

	pred := &onPredicate{
		p:    p,
		info: info,
	}

	// Determine the filter expression.
	colInfo := multiSourceInfo{left, right}
	iVarHelper := parser.MakeIndexedVarHelper(pred, len(info.sourceColumns))
	filter, err := p.analyzeExpr(expr, colInfo, iVarHelper, parser.TypeBool, true, "ON")
	if err != nil {
		return nil, nil, err
	}
	pred.filter = filter

	return pred, info, nil
}

// equalityPredicate implements the predicate logic for joins with a USING clause.
type equalityPredicate struct {
	// The list of leftColumn names given to USING.
	leftColNames parser.NameList
	// The list of rightColumn names given to USING.
	rightColNames parser.NameList

	// The comparison function to use for each column. We need
	// different functions because each USING column may have a different
	// type (and they may be heterogeneous between left and right).
	usingCmp []func(*parser.EvalContext, parser.Datum, parser.Datum) (parser.DBool, error)
	// evalCtx is needed to evaluate the functions in usingCmp.
	evalCtx *parser.EvalContext

	// left/rightUsingIndices give the position of USING columns
	// on the left and right input row arrays, respectively.
	leftUsingIndices  []int
	rightUsingIndices []int

	// left/rightUsingIndices give the position of non-USING columns on
	// the left and right input row arrays, respectively.
	leftRestIndices  []int
	rightRestIndices []int
}

func (p *equalityPredicate) format(buf *bytes.Buffer) {
	buf.WriteString(" ON EQUALS((")
	p.leftColNames.Format(buf, parser.FmtSimple)
	buf.WriteString("),(")
	p.rightColNames.Format(buf, parser.FmtSimple)
	buf.WriteString("))")
}
func (p *equalityPredicate) start() error                        { return nil }
func (p *equalityPredicate) expand() error                       { return nil }
func (p *equalityPredicate) explainTypes(_ func(string, string)) {}

// eval for equalityPredicate compares the USING columns, returning true
// if and only if all USING columns are equal on both sides.
func (p *equalityPredicate) eval(_, leftRow, rightRow parser.DTuple) (bool, error) {
	eq := true
	for i := range p.leftColNames {
		leftVal := leftRow[p.leftUsingIndices[i]]
		rightVal := rightRow[p.rightUsingIndices[i]]
		if leftVal == parser.DNull || rightVal == parser.DNull {
			eq = false
			break
		}
		res, err := p.usingCmp[i](p.evalCtx, leftVal, rightVal)
		if err != nil {
			return false, err
		}
		if res != parser.DBool(true) {
			eq = false
			break
		}
	}
	return eq, nil
}

// prepareRow for equalityPredicate has more work to do than for ON
// clauses and CROSS JOIN: a result row contains first the values for
// the USING columns; then the non-USING values from the left input
// row, then the non-USING values from the right input row.
func (p *equalityPredicate) prepareRow(result, leftRow, rightRow parser.DTuple) {
	d := 0
	for k, j := range p.leftUsingIndices {
		// The result for USING columns must be computed as per COALESCE().
		if leftRow[j] != parser.DNull {
			result[d] = leftRow[j]
		} else {
			result[d] = rightRow[p.rightUsingIndices[k]]
		}
		d++
	}
	for _, j := range p.leftRestIndices {
		result[d] = leftRow[j]
		d++
	}
	for _, j := range p.rightRestIndices {
		result[d] = rightRow[j]
		d++
	}
}

func (p *equalityPredicate) encode(b []byte, row parser.DTuple, side int) ([]byte, bool, error) {
	var cols []int
	switch side {
	case rightSide:
		cols = p.rightUsingIndices
	case leftSide:
		cols = p.leftUsingIndices
	default:
		panic("invalid side provided, only leftSide or rightSide applicable")
	}

	var err error
	containsNull := false
	for _, colIdx := range cols {
		if row[colIdx] == parser.DNull {
			containsNull = true
		}
		b, err = sqlbase.EncodeDatum(b, row[colIdx])
		if err != nil {
			return nil, false, err
		}
	}
	return b, containsNull, nil
}

// pickUsingColumn searches for a column whose name matches colName.
// The column index and type are returned if found, otherwise an error
// is reported.
func pickUsingColumn(cols ResultColumns, colName string, context string) (int, parser.Type, error) {
	idx := invalidColIdx
	for j, col := range cols {
		if col.hidden {
			continue
		}
		if parser.ReNormalizeName(col.Name) == colName {
			idx = j
		}
	}
	if idx == invalidColIdx {
		return idx, nil, fmt.Errorf("column \"%s\" specified in USING clause does not exist in %s table", colName, context)
	}
	return idx, cols[idx].Typ, nil
}

// makeUsingPredicate constructs a joinPredicate object for joins with
// a USING clause.
func (p *planner) makeUsingPredicate(
	left *dataSourceInfo, right *dataSourceInfo, colNames parser.NameList,
) (joinPredicate, *dataSourceInfo, error) {
	seenNames := make(map[string]struct{})

	for _, unnormalizedColName := range colNames {
		colName := unnormalizedColName.Normalize()
		// Check for USING(x,x)
		if _, ok := seenNames[colName]; ok {
			return nil, nil, fmt.Errorf("column %q appears more than once in USING clause", colName)
		}
		seenNames[colName] = struct{}{}
	}
	return p.makeEqualityPredicate(left, right, colNames, colNames)
}

// makeEqualityPredicate constructs a joinPredicate object for joins.
func (p *planner) makeEqualityPredicate(
	left *dataSourceInfo,
	right *dataSourceInfo,
	leftColNames parser.NameList,
	rightColNames parser.NameList,
) (joinPredicate, *dataSourceInfo, error) {
	if len(leftColNames) != len(rightColNames) {
		panic(fmt.Errorf("left columns' length %q doesn't match right columns' length %q in EqualityPredicate",
			len(leftColNames), len(rightColNames)))
	}
	cmpOps := make([]func(*parser.EvalContext, parser.Datum, parser.Datum) (parser.DBool, error), len(leftColNames))
	leftUsingIndices := make([]int, len(leftColNames))
	rightUsingIndices := make([]int, len(rightColNames))
	usedLeft := make([]int, len(left.sourceColumns))
	for i := range usedLeft {
		usedLeft[i] = invalidColIdx
	}
	usedRight := make([]int, len(right.sourceColumns))
	for i := range usedRight {
		usedRight[i] = invalidColIdx
	}
	columns := make(ResultColumns, 0, len(left.sourceColumns)+len(right.sourceColumns)-len(leftColNames))

	// Find out which columns are involved in EqualityPredicate.
	for i := range leftColNames {
		leftColName := leftColNames[i].Normalize()
		rightColName := rightColNames[i].Normalize()

		// Find the column name on the left.
		leftIdx, leftType, err := pickUsingColumn(left.sourceColumns, leftColName, "left")
		if err != nil {
			return nil, nil, err
		}
		usedLeft[leftIdx] = i

		// Find the column name on the right.
		rightIdx, rightType, err := pickUsingColumn(right.sourceColumns, rightColName, "right")
		if err != nil {
			return nil, nil, err
		}
		usedRight[rightIdx] = i

		// Remember the indices.
		leftUsingIndices[i] = leftIdx
		rightUsingIndices[i] = rightIdx

		// Memoize the comparison function.
		fn, found := parser.FindEqualComparisonFunction(leftType, rightType)
		if !found {
			return nil, nil, fmt.Errorf("JOIN/USING types %s for left column %s and %s for right column %s cannot be matched",
				leftType, leftColName, rightType, rightColName)
		}
		cmpOps[i] = fn

		// Prepare the output column for EqualityPredicate.
		columns = append(columns, left.sourceColumns[leftIdx])
	}

	// Find out which columns are not involved in the EqualityPredicate.
	leftRestIndices := make([]int, 0, len(left.sourceColumns)-1)
	for i := range left.sourceColumns {
		if usedLeft[i] == invalidColIdx {
			leftRestIndices = append(leftRestIndices, i)
			usedLeft[i] = len(columns)
			columns = append(columns, left.sourceColumns[i])
		}
	}
	rightRestIndices := make([]int, 0, len(right.sourceColumns)-1)
	for i := range right.sourceColumns {
		if usedRight[i] == invalidColIdx {
			rightRestIndices = append(rightRestIndices, i)
			usedRight[i] = len(columns)
			columns = append(columns, right.sourceColumns[i])
		}
	}

	// Merge the mappings from table aliases to column sets from both
	// sides into a new alias-columnset mapping for the result rows.
	aliases := make(sourceAliases)
	for alias, colRange := range left.sourceAliases {
		newRange := make([]int, len(colRange))
		for i, colIdx := range colRange {
			newRange[i] = usedLeft[colIdx]
		}
		aliases[alias] = newRange
	}
	for alias, colRange := range right.sourceAliases {
		newRange := make([]int, len(colRange))
		for i, colIdx := range colRange {
			newRange[i] = usedRight[colIdx]
		}
		aliases[alias] = newRange
	}

	info := &dataSourceInfo{
		sourceColumns: columns,
		sourceAliases: aliases,
	}

	return &equalityPredicate{
		evalCtx:           &p.evalCtx,
		leftColNames:      leftColNames,
		rightColNames:     rightColNames,
		usingCmp:          cmpOps,
		leftUsingIndices:  leftUsingIndices,
		rightUsingIndices: rightUsingIndices,
		leftRestIndices:   leftRestIndices,
		rightRestIndices:  rightRestIndices,
	}, info, nil
}
