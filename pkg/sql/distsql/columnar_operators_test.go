// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package distsql

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/col/coldata"
	"github.com/cockroachdb/cockroach/pkg/col/typeconv"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catenumpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/colexec/colexecagg"
	"github.com/cockroachdb/cockroach/pkg/sql/colexec/colexectestutils"
	"github.com/cockroachdb/cockroach/pkg/sql/colexec/colexecwindow"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra/execagg"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/randgen"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/builtins/builtinsregistry"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/stretchr/testify/require"
)

const nullProbability = 0.2
const randTypesProbability = 0.5

var aggregateFuncToNumArguments = map[execinfrapb.AggregatorSpec_Func]int{
	execinfrapb.AnyNotNull:                  1,
	execinfrapb.Avg:                         1,
	execinfrapb.BoolAnd:                     1,
	execinfrapb.BoolOr:                      1,
	execinfrapb.ConcatAgg:                   1,
	execinfrapb.Count:                       1,
	execinfrapb.Max:                         1,
	execinfrapb.Min:                         1,
	execinfrapb.Stddev:                      1,
	execinfrapb.Sum:                         1,
	execinfrapb.SumInt:                      1,
	execinfrapb.Variance:                    1,
	execinfrapb.XorAgg:                      1,
	execinfrapb.CountRows:                   0,
	execinfrapb.Sqrdiff:                     1,
	execinfrapb.FinalVariance:               3,
	execinfrapb.FinalVarPop:                 3,
	execinfrapb.FinalStddev:                 3,
	execinfrapb.FinalStddevPop:              3,
	execinfrapb.ArrayAgg:                    1,
	execinfrapb.JSONAgg:                     1,
	execinfrapb.JSONBAgg:                    1,
	execinfrapb.StringAgg:                   2,
	execinfrapb.BitAnd:                      1,
	execinfrapb.BitOr:                       1,
	execinfrapb.Corr:                        2,
	execinfrapb.PercentileDiscImpl:          2,
	execinfrapb.PercentileContImpl:          2,
	execinfrapb.JSONObjectAgg:               2,
	execinfrapb.JSONBObjectAgg:              2,
	execinfrapb.VarPop:                      1,
	execinfrapb.StddevPop:                   1,
	execinfrapb.StMakeline:                  1,
	execinfrapb.StExtent:                    1,
	execinfrapb.StUnion:                     1,
	execinfrapb.StCollect:                   1,
	execinfrapb.CovarPop:                    2,
	execinfrapb.CovarSamp:                   2,
	execinfrapb.RegrIntercept:               2,
	execinfrapb.RegrR2:                      2,
	execinfrapb.RegrSlope:                   2,
	execinfrapb.RegrSxx:                     2,
	execinfrapb.RegrSxy:                     2,
	execinfrapb.RegrSyy:                     2,
	execinfrapb.RegrCount:                   2,
	execinfrapb.RegrAvgx:                    2,
	execinfrapb.RegrAvgy:                    2,
	execinfrapb.TransitionRegrAggregate:     2,
	execinfrapb.FinalCovarPop:               1,
	execinfrapb.FinalRegrSxx:                1,
	execinfrapb.FinalRegrSxy:                1,
	execinfrapb.FinalRegrSyy:                1,
	execinfrapb.FinalRegrAvgx:               1,
	execinfrapb.FinalRegrAvgy:               1,
	execinfrapb.FinalRegrIntercept:          1,
	execinfrapb.FinalRegrR2:                 1,
	execinfrapb.FinalRegrSlope:              1,
	execinfrapb.FinalCovarSamp:              1,
	execinfrapb.FinalCorr:                   1,
	execinfrapb.FinalSqrdiff:                3,
	execinfrapb.ArrayCatAgg:                 1,
	execinfrapb.MergeStatsMetadata:          1,
	execinfrapb.MergeStatementStats:         1,
	execinfrapb.MergeTransactionStats:       1,
	execinfrapb.MergeAggregatedStmtMetadata: 1,
}

// TestAggregateFuncToNumArguments ensures that all aggregate functions are
// present in the map above.
func TestAggregateFuncToNumArguments(t *testing.T) {
	defer leaktest.AfterTest(t)()

	checkForOverload := func(t *testing.T, expected int, overloads []tree.Overload) {
		for _, overload := range overloads {
			if overload.Types.Length() == expected {
				return
			}
		}
		t.Fatalf("expected %d inputs, but no matching overload found", expected)
	}
	check := func(t *testing.T, fn execinfrapb.AggregatorSpec_Func) {
		n, ok := aggregateFuncToNumArguments[fn]
		require.Truef(t, ok, "didn't find number of arguments for %s", fn)
		_, overloads := builtinsregistry.GetBuiltinProperties(strings.ToLower(fn.String()))
		checkForOverload(t, n, overloads)
	}

	fns := make([]execinfrapb.AggregatorSpec_Func, 0,
		len(execinfrapb.AggregatorSpec_Func_name))
	for fn := range execinfrapb.AggregatorSpec_Func_name {
		fns = append(fns, execinfrapb.AggregatorSpec_Func(fn))
	}
	sort.Slice(fns, func(i, j int) bool { return fns[i] < fns[j] })
	for _, fn := range fns {
		t.Run(fn.String(), func(t *testing.T) {
			check(t, fn)
		})
	}
}

func TestAggregatorAgainstProcessor(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	st := cluster.MakeTestingClusterSettings()
	evalCtx := eval.MakeTestingEvalContext(st)
	defer evalCtx.Stop(context.Background())

	rng, seed := randutil.NewTestRand()
	nRuns := 20
	nRows := 100
	nAggFnsToTest := 5
	const (
		maxNumGroupingCols = 3
		nextGroupProb      = 0.2
	)
	groupingCols := make([]uint32, maxNumGroupingCols)
	orderingCols := make([]execinfrapb.Ordering_Column, maxNumGroupingCols)
	for i := uint32(0); i < maxNumGroupingCols; i++ {
		groupingCols[i] = i
		orderingCols[i].ColIdx = i
	}
	var da tree.DatumAlloc

	// We need +1 because an entry for index=6 was omitted by mistake.
	numSupportedAggFns := len(execinfrapb.AggregatorSpec_Func_name) + 1
	aggregations := make([]execinfrapb.AggregatorSpec_Aggregation, 0, nAggFnsToTest)
	for len(aggregations) < nAggFnsToTest {
		var aggFn execinfrapb.AggregatorSpec_Func
		found := false
		for !found {
			aggFn = execinfrapb.AggregatorSpec_Func(rng.Intn(numSupportedAggFns))
			if _, valid := aggregateFuncToNumArguments[aggFn]; !valid {
				continue
			}
			switch aggFn {
			case execinfrapb.AnyNotNull:
				// We skip ANY_NOT_NULL aggregate function because it returns
				// non-deterministic results.
			case execinfrapb.PercentileDiscImpl,
				execinfrapb.PercentileContImpl:
				// We skip percentile functions because those can only be
				// planned as window functions.
			case execinfrapb.MergeStatsMetadata,
				execinfrapb.MergeStatementStats,
				execinfrapb.MergeTransactionStats,
				execinfrapb.MergeAggregatedStmtMetadata:
				// We skip merge statistics functions because they
				// require custom JSON objects.
			default:
				found = true
			}

		}
		aggregations = append(aggregations, execinfrapb.AggregatorSpec_Aggregation{Func: aggFn})
	}
	for _, spillForced := range []bool{false, true} {
		for _, hashAgg := range []bool{false, true} {
			if !hashAgg && spillForced {
				// There is no point in making the ordered aggregation spill to
				// disk.
				continue
			}
			// We currently support filtering aggregation only for the in-memory
			// hash aggregator, and colbuilder.NewColOperator will attempt to
			// instantiate an external hash aggregator which requires the
			// filtering support in the ordered aggregator.
			filteringAggOptions := []bool{false}
			for _, filteringAgg := range filteringAggOptions {
				numFilteringCols := 0
				if filteringAgg {
					numFilteringCols = 1
				}
				for numGroupingCols := 1; numGroupingCols <= maxNumGroupingCols; numGroupingCols++ {
					// We will be grouping based on the first numGroupingCols columns
					// (which will be of INT types) with the values for the columns set
					// manually below.
					numUtilityCols := numGroupingCols + numFilteringCols
					inputTypes := make([]*types.T, 0, numUtilityCols+len(aggregations))
					for i := 0; i < numGroupingCols; i++ {
						inputTypes = append(inputTypes, types.Int)
					}
					// Check whether we want to add a column for FILTER clause.
					var filteringColIdx uint32
					if filteringAgg {
						filteringColIdx = uint32(len(inputTypes))
						inputTypes = append(inputTypes, types.Bool)
					}
					// After all utility columns, we will have input columns for each
					// of the aggregate functions. Here, we will set up the column
					// indices, and the types will be generated below.
					numColsSoFar := numUtilityCols
					for i := range aggregations {
						numArguments := aggregateFuncToNumArguments[aggregations[i].Func]
						aggregations[i].ColIdx = make([]uint32, numArguments)
						for j := range aggregations[i].ColIdx {
							aggregations[i].ColIdx[j] = uint32(numColsSoFar)
							numColsSoFar++
						}
					}
					outputTypes := make([]*types.T, len(aggregations))

					for run := 0; run < nRuns; run++ {
						inputTypes = inputTypes[:numUtilityCols]
						var rows rowenc.EncDatumRows
						hasJSONColumn := false
						for i := range aggregations {
							aggFn := aggregations[i].Func
							aggFnInputTypes := make([]*types.T, len(aggregations[i].ColIdx))
							for {
								for j := range aggFnInputTypes {
									aggFnInputTypes[j] = randgen.RandType(rng)
								}
								// There is a special case for some functions when at
								// least one argument is a tuple or an array of
								// tuples.
								// Such cases pass GetAggregateOutputType check below,
								// but they are actually invalid, and during normal
								// execution it is caught during type-checking.
								// However, we don't want to do fully-fledged type
								// checking, so we hard-code an exception here.
								invalid := false
								switch aggFn {
								case execinfrapb.ConcatAgg,
									execinfrapb.StringAgg,
									execinfrapb.StMakeline,
									execinfrapb.StExtent,
									execinfrapb.StUnion,
									execinfrapb.StCollect,
									execinfrapb.ArrayAgg,
									execinfrapb.ArrayCatAgg:
									for _, typ := range aggFnInputTypes {
										if typ.Family() == types.TupleFamily || (typ.Family() == types.ArrayFamily && typ.ArrayContents().Family() == types.TupleFamily) {
											invalid = true
											break
										}
									}
								}
								if invalid {
									continue
								}
								for _, typ := range aggFnInputTypes {
									hasJSONColumn = hasJSONColumn || typ.Family() == types.JsonFamily
								}
								if outputType, err := execagg.GetAggregateOutputType(aggFn, aggFnInputTypes); err == nil {
									outputTypes[i] = outputType
									break
								}
							}
							inputTypes = append(inputTypes, aggFnInputTypes...)
						}
						rows = randgen.RandEncDatumRowsOfTypes(rng, nRows, inputTypes)
						groupIdx := 0
						for _, row := range rows {
							for i := 0; i < numGroupingCols; i++ {
								if rng.Float64() < nullProbability {
									row[i] = rowenc.EncDatum{Datum: tree.DNull}
								} else {
									row[i] = rowenc.EncDatum{Datum: tree.NewDInt(tree.DInt(groupIdx))}
									if rng.Float64() < nextGroupProb {
										groupIdx++
									}
								}
							}
						}

						// Update the specifications of aggregate functions to
						// possibly include DISTINCT and/or FILTER clauses.
						for _, aggFn := range aggregations {
							distinctProb := 0.5
							if hasJSONColumn {
								// We currently cannot encode json columns, so we
								// don't support distinct aggregation in both
								// row-by-row and vectorized engines.
								distinctProb = 0
							}
							aggFn.Distinct = rng.Float64() < distinctProb
							if filteringAgg {
								aggFn.FilterColIdx = &filteringColIdx
							} else {
								aggFn.FilterColIdx = nil
							}
						}
						aggregatorSpec := &execinfrapb.AggregatorSpec{
							Type:         execinfrapb.AggregatorSpec_NON_SCALAR,
							GroupCols:    groupingCols[:numGroupingCols],
							Aggregations: aggregations,
						}
						if hashAgg {
							// Let's shuffle the rows for the hash aggregator.
							rng.Shuffle(nRows, func(i, j int) {
								rows[i], rows[j] = rows[j], rows[i]
							})
						} else {
							aggregatorSpec.OrderedGroupCols = groupingCols[:numGroupingCols]
							orderedCols := execinfrapb.ConvertToColumnOrdering(
								execinfrapb.Ordering{Columns: orderingCols[:numGroupingCols]},
							)
							// Although we build the input rows in "non-decreasing" order, it is
							// possible that some NULL values are present here and there, so we
							// need to sort the rows to satisfy the ordering conditions.
							sort.Slice(rows, func(i, j int) bool {
								cmp, err := rows[i].Compare(context.Background(), inputTypes, &da, orderedCols, &evalCtx, rows[j])
								if err != nil {
									t.Fatal(err)
								}
								return cmp < 0
							})
						}
						pspec := &execinfrapb.ProcessorSpec{
							Input:       []execinfrapb.InputSyncSpec{{ColumnTypes: inputTypes}},
							Core:        execinfrapb.ProcessorCoreUnion{Aggregator: aggregatorSpec},
							ResultTypes: outputTypes,
						}
						args := verifyColOperatorArgs{
							anyOrder:       hashAgg,
							inputTypes:     [][]*types.T{inputTypes},
							inputs:         []rowenc.EncDatumRows{rows},
							pspec:          pspec,
							forceDiskSpill: spillForced,
						}
						if err := verifyColOperator(t, args); err != nil {
							if strings.Contains(err.Error(), "different errors returned") {
								// Columnar and row-based aggregators are likely to hit
								// different errors, and we will swallow those and move
								// on.
								continue
							}
							fmt.Printf("--- seed = %d run = %d filter = %t hash = %t ---\n",
								seed, run, filteringAgg, hashAgg)
							var aggFnNames string
							for i, agg := range aggregations {
								if i > 0 {
									aggFnNames += " "
								}
								aggFnNames += agg.Func.String()
							}
							fmt.Printf("--- %s ---\n", aggFnNames)
							prettyPrintTypes(inputTypes, "t" /* tableName */)
							prettyPrintInput(rows, inputTypes, "t" /* tableName */)
							t.Fatal(err)
						}
					}
				}
			}
		}
	}
}

func TestDistinctAgainstProcessor(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	var da tree.DatumAlloc
	evalCtx := eval.MakeTestingEvalContext(cluster.MakeTestingClusterSettings())
	defer evalCtx.Stop(context.Background())

	rng, seed := randutil.NewTestRand()
	nRuns := 10
	nRows := 10
	maxCols := 3
	maxNum := 3
	intTyps := make([]*types.T, maxCols)
	for i := range intTyps {
		intTyps[i] = types.Int
	}

	for _, spillForced := range []bool{false, true} {
		for run := 0; run < nRuns; run++ {
			for nCols := 1; nCols <= maxCols; nCols++ {
				for nDistinctCols := 1; nDistinctCols <= nCols; nDistinctCols++ {
					for nOrderedCols := 0; nOrderedCols <= nDistinctCols; nOrderedCols++ {
						if spillForced && nOrderedCols == nDistinctCols {
							// The ordered distinct is a streaming operator that
							// doesn't support spilling to disk (nor does it
							// need to), so we'll skip the config where we're
							// trying to spill to disk the ordered distinct.
							continue
						}
						var (
							rows       rowenc.EncDatumRows
							inputTypes []*types.T
							ordCols    []execinfrapb.Ordering_Column
						)
						if rng.Float64() < randTypesProbability {
							inputTypes = generateRandomSupportedTypes(rng, nCols)
							rows = randgen.RandEncDatumRowsOfTypes(rng, nRows, inputTypes)
						} else {
							inputTypes = intTyps[:nCols]
							rows = randgen.MakeRandIntRowsInRange(rng, nRows, nCols, maxNum, nullProbability)
						}
						distinctCols := make([]uint32, nDistinctCols)
						for i, distinctCol := range rng.Perm(nCols)[:nDistinctCols] {
							distinctCols[i] = uint32(distinctCol)
						}
						orderedCols := make([]uint32, nOrderedCols)
						for i, orderedColIdx := range rng.Perm(nDistinctCols)[:nOrderedCols] {
							// From the set of distinct columns we need to choose nOrderedCols
							// to be in the ordered columns set.
							orderedCols[i] = distinctCols[orderedColIdx]
						}
						ordCols = make([]execinfrapb.Ordering_Column, nOrderedCols)
						for i, col := range orderedCols {
							ordCols[i] = execinfrapb.Ordering_Column{
								ColIdx: col,
							}
						}
						var outputOrdering execinfrapb.Ordering
						if spillForced && rng.Float64() < 0.5 {
							// In order to produce deterministic output
							// ordering, we will include all input columns into
							// ordCols. Note that orderedCols (used in the spec)
							// still has the desired number of columns set
							// above.
							for inputCol := 0; inputCol < nCols; inputCol++ {
								found := false
								for _, orderedCol := range orderedCols {
									if inputCol == int(orderedCol) {
										found = true
										break
									}
								}
								if !found {
									ordCols = append(ordCols, execinfrapb.Ordering_Column{
										ColIdx: uint32(inputCol),
									})
								}
							}
							outputOrdering.Columns = ordCols
						}
						sort.Slice(rows, func(i, j int) bool {
							cmp, err := rows[i].Compare(context.Background(), inputTypes, &da, execinfrapb.ConvertToColumnOrdering(execinfrapb.Ordering{Columns: ordCols}), &evalCtx, rows[j])
							if err != nil {
								t.Fatal(err)
							}
							return cmp < 0
						})

						spec := &execinfrapb.DistinctSpec{
							DistinctColumns: distinctCols,
							OrderedColumns:  orderedCols,
							OutputOrdering:  outputOrdering,
						}
						pspec := &execinfrapb.ProcessorSpec{
							Input:       []execinfrapb.InputSyncSpec{{ColumnTypes: inputTypes}},
							Core:        execinfrapb.ProcessorCoreUnion{Distinct: spec},
							ResultTypes: inputTypes,
						}
						args := verifyColOperatorArgs{
							// If we spilled the unordered distinct to disk and
							// didn't require the output ordering on all input
							// columns, we can get the output in an arbitrary
							// order.
							anyOrder:       spillForced && len(outputOrdering.Columns) == 0,
							inputTypes:     [][]*types.T{inputTypes},
							inputs:         []rowenc.EncDatumRows{rows},
							pspec:          pspec,
							forceDiskSpill: spillForced,
						}
						if err := verifyColOperator(t, args); err != nil {
							fmt.Printf("--- seed = %d run = %d nCols = %d distinct cols = %v ordered cols = %v spilled = %t ---\n",
								seed, run, nCols, distinctCols, orderedCols, spillForced)
							prettyPrintTypes(inputTypes, "t" /* tableName */)
							prettyPrintInput(rows, inputTypes, "t" /* tableName */)
							t.Fatal(err)
						}
					}
				}
			}
		}
	}
}

func TestSorterAgainstProcessor(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	st := cluster.MakeTestingClusterSettings()
	evalCtx := eval.MakeTestingEvalContext(st)
	defer evalCtx.Stop(context.Background())

	rng, seed := randutil.NewTestRand()
	nRuns := 5
	nRows := 8 * coldata.BatchSize()
	maxCols := 5
	maxNum := 10
	intTyps := make([]*types.T, maxCols)
	for i := range intTyps {
		intTyps[i] = types.Int
	}

	for _, spillForced := range []bool{false, true} {
		for run := 0; run < nRuns; run++ {
			for nCols := 1; nCols <= maxCols; nCols++ {
				// We will try both general sort and top K sort.
				for _, topK := range []uint64{0, uint64(1 + rng.Intn(64))} {
					var (
						rows       rowenc.EncDatumRows
						inputTypes []*types.T
					)
					if rng.Float64() < randTypesProbability {
						inputTypes = generateRandomSupportedTypes(rng, nCols)
						rows = randgen.RandEncDatumRowsOfTypes(rng, nRows, inputTypes)
					} else {
						inputTypes = intTyps[:nCols]
						rows = randgen.MakeRandIntRowsInRange(rng, nRows, nCols, maxNum, nullProbability)
					}

					// Note: we're only generating column orderings on all nCols columns since
					// if there are columns not in the ordering, the results are not fully
					// deterministic.
					orderingCols := generateColumnOrdering(rng, nCols, nCols)
					sorterSpec := &execinfrapb.SorterSpec{
						OutputOrdering: execinfrapb.Ordering{Columns: orderingCols},
					}
					var offset uint64
					if topK > 0 {
						offset = uint64(rng.Intn(int(topK)))
						sorterSpec.Limit = int64(topK - offset)
					}
					pspec := &execinfrapb.ProcessorSpec{
						Input:       []execinfrapb.InputSyncSpec{{ColumnTypes: inputTypes}},
						Core:        execinfrapb.ProcessorCoreUnion{Sorter: sorterSpec},
						Post:        execinfrapb.PostProcessSpec{Offset: offset},
						ResultTypes: inputTypes,
					}
					args := verifyColOperatorArgs{
						inputTypes:     [][]*types.T{inputTypes},
						inputs:         []rowenc.EncDatumRows{rows},
						pspec:          pspec,
						forceDiskSpill: spillForced,
					}
					if spillForced {
						args.numForcedRepartitions = 2 + rng.Intn(3)
					}
					if err := verifyColOperator(t, args); err != nil {
						fmt.Printf("--- seed = %d spillForced = %t nCols = %d K = %d ---\n",
							seed, spillForced, nCols, topK)
						prettyPrintTypes(inputTypes, "t" /* tableName */)
						prettyPrintInput(rows, inputTypes, "t" /* tableName */)
						t.Fatal(err)
					}
				}
			}
		}
	}
}

func TestSortChunksAgainstProcessor(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	var da tree.DatumAlloc
	st := cluster.MakeTestingClusterSettings()
	evalCtx := eval.MakeTestingEvalContext(st)
	defer evalCtx.Stop(context.Background())

	rng, seed := randutil.NewTestRand()
	nRuns := 5
	nRows := 5 * coldata.BatchSize() / 4
	maxCols := 3
	maxNum := 10
	intTyps := make([]*types.T, maxCols)
	for i := range intTyps {
		intTyps[i] = types.Int
	}

	for _, spillForced := range []bool{false, true} {
		for run := 0; run < nRuns; run++ {
			for nCols := 2; nCols <= maxCols; nCols++ {
				for matchLen := 1; matchLen < nCols; matchLen++ {
					var (
						rows       rowenc.EncDatumRows
						inputTypes []*types.T
					)
					if rng.Float64() < randTypesProbability {
						inputTypes = generateRandomSupportedTypes(rng, nCols)
						rows = randgen.RandEncDatumRowsOfTypes(rng, nRows, inputTypes)
					} else {
						inputTypes = intTyps[:nCols]
						rows = randgen.MakeRandIntRowsInRange(rng, nRows, nCols, maxNum, nullProbability)
					}

					// Note: we're only generating column orderings on all nCols columns since
					// if there are columns not in the ordering, the results are not fully
					// deterministic.
					orderingCols := generateColumnOrdering(rng, nCols, nCols)
					matchedCols := execinfrapb.ConvertToColumnOrdering(execinfrapb.Ordering{Columns: orderingCols[:matchLen]})
					// Presort the input on first matchLen columns.
					sort.Slice(rows, func(i, j int) bool {
						cmp, err := rows[i].Compare(context.Background(), inputTypes, &da, matchedCols, &evalCtx, rows[j])
						if err != nil {
							t.Fatal(err)
						}
						return cmp < 0
					})

					sorterSpec := &execinfrapb.SorterSpec{
						OutputOrdering:   execinfrapb.Ordering{Columns: orderingCols},
						OrderingMatchLen: uint32(matchLen),
					}
					pspec := &execinfrapb.ProcessorSpec{
						Input:       []execinfrapb.InputSyncSpec{{ColumnTypes: inputTypes}},
						Core:        execinfrapb.ProcessorCoreUnion{Sorter: sorterSpec},
						ResultTypes: inputTypes,
					}
					args := verifyColOperatorArgs{
						inputTypes:     [][]*types.T{inputTypes},
						inputs:         []rowenc.EncDatumRows{rows},
						pspec:          pspec,
						forceDiskSpill: spillForced,
					}
					if err := verifyColOperator(t, args); err != nil {
						fmt.Printf("--- seed = %d spillForced = %t orderingCols = %v matchLen = %d run = %d ---\n",
							seed, spillForced, orderingCols, matchLen, run)
						prettyPrintTypes(inputTypes, "t" /* tableName */)
						prettyPrintInput(rows, inputTypes, "t" /* tableName */)
						t.Fatal(err)
					}
				}
			}
		}
	}
}

func TestHashJoinerAgainstProcessor(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	evalCtx := eval.MakeTestingEvalContext(cluster.MakeTestingClusterSettings())
	defer evalCtx.Stop(context.Background())

	type hjTestSpec struct {
		joinType        descpb.JoinType
		onExprSupported bool
	}
	testSpecs := []hjTestSpec{
		{
			joinType:        descpb.InnerJoin,
			onExprSupported: true,
		},
		{
			joinType: descpb.LeftOuterJoin,
		},
		{
			joinType: descpb.RightOuterJoin,
		},
		{
			joinType: descpb.FullOuterJoin,
		},
		{
			joinType: descpb.LeftSemiJoin,
		},
		{
			joinType: descpb.LeftAntiJoin,
		},
		{
			joinType: descpb.IntersectAllJoin,
		},
		{
			joinType: descpb.ExceptAllJoin,
		},
		{
			joinType: descpb.RightSemiJoin,
		},
		{
			joinType: descpb.RightAntiJoin,
		},
	}

	rng, seed := randutil.NewTestRand()
	nRuns := 3
	nRows := 10
	maxCols := 3
	maxNum := 5
	intTyps := make([]*types.T, maxCols)
	for i := range intTyps {
		intTyps[i] = types.Int
	}

	for _, spillForced := range []bool{false, true} {
		for run := 0; run < nRuns; run++ {
			for _, testSpec := range testSpecs {
				for nCols := 1; nCols <= maxCols; nCols++ {
					for nEqCols := 0; nEqCols <= nCols; nEqCols++ {
						triedWithoutOnExpr, triedWithOnExpr := false, false
						if !testSpec.onExprSupported {
							triedWithOnExpr = true
						}
						for !triedWithoutOnExpr || !triedWithOnExpr {
							var (
								lRows, rRows             rowenc.EncDatumRows
								lEqCols, rEqCols         []uint32
								lInputTypes, rInputTypes []*types.T
								usingRandomTypes         bool
							)
							if rng.Float64() < randTypesProbability {
								lInputTypes = generateRandomSupportedTypes(rng, nCols)
								lEqCols = generateEqualityColumns(rng, nCols, nEqCols)
								rInputTypes = append(rInputTypes[:0], lInputTypes...)
								rEqCols = append(rEqCols[:0], lEqCols...)
								rng.Shuffle(nEqCols, func(i, j int) {
									iColIdx, jColIdx := rEqCols[i], rEqCols[j]
									rInputTypes[iColIdx], rInputTypes[jColIdx] = rInputTypes[jColIdx], rInputTypes[iColIdx]
									rEqCols[i], rEqCols[j] = rEqCols[j], rEqCols[i]
								})
								rInputTypes = randomizeJoinRightTypes(rng, rInputTypes)
								lRows = randgen.RandEncDatumRowsOfTypes(rng, nRows, lInputTypes)
								rRows = randgen.RandEncDatumRowsOfTypes(rng, nRows, rInputTypes)
								usingRandomTypes = true
							} else {
								lInputTypes = intTyps[:nCols]
								rInputTypes = lInputTypes
								lRows = randgen.MakeRandIntRowsInRange(rng, nRows, nCols, maxNum, nullProbability)
								rRows = randgen.MakeRandIntRowsInRange(rng, nRows, nCols, maxNum, nullProbability)
								lEqCols = generateEqualityColumns(rng, nCols, nEqCols)
								rEqCols = generateEqualityColumns(rng, nCols, nEqCols)
							}

							outputTypes := testSpec.joinType.MakeOutputTypes(lInputTypes, rInputTypes)
							outputColumns := make([]uint32, len(outputTypes))
							for i := range outputColumns {
								outputColumns[i] = uint32(i)
							}

							var onExpr execinfrapb.Expression
							if triedWithoutOnExpr {
								colTypes := append(lInputTypes, rInputTypes...)
								onExpr = generateFilterExpr(
									rng, nCols, nEqCols, colTypes, usingRandomTypes, false, /* forceSingleSide */
								)
							}
							hjSpec := &execinfrapb.HashJoinerSpec{
								LeftEqColumns:  lEqCols,
								RightEqColumns: rEqCols,
								OnExpr:         onExpr,
								Type:           testSpec.joinType,
							}
							pspec := &execinfrapb.ProcessorSpec{
								Input: []execinfrapb.InputSyncSpec{
									{ColumnTypes: lInputTypes},
									{ColumnTypes: rInputTypes},
								},
								Core: execinfrapb.ProcessorCoreUnion{HashJoiner: hjSpec},
								Post: execinfrapb.PostProcessSpec{
									Projection:    true,
									OutputColumns: outputColumns,
								},
								ResultTypes: outputTypes,
							}
							args := verifyColOperatorArgs{
								anyOrder:       true,
								inputTypes:     [][]*types.T{lInputTypes, rInputTypes},
								inputs:         []rowenc.EncDatumRows{lRows, rRows},
								pspec:          pspec,
								forceDiskSpill: spillForced,
								// It is possible that we have a filter that is always false, and this
								// will allow us to plan a zero operator which always returns a zero
								// batch. In such case, the spilling might not occur and that's ok.
								//
								// We also won't be able to "detect" that the spilling occurred in case
								// of the cross joins since they use spilling queues directly that don't
								// take in a spilling callback (unlike the diskSpiller-based operators).
								// TODO(yuzefovich): add a callback for when the spilling occurs to
								// spillingQueues.
								forcedDiskSpillMightNotOccur: !onExpr.Empty() || len(lEqCols) == 0,
								numForcedRepartitions:        2,
								rng:                          rng,
							}
							if testSpec.joinType.IsSetOpJoin() && nEqCols < nCols {
								// The output of set operation joins is not fully
								// deterministic when there are non-equality
								// columns, however, the rows must match on the
								// equality columns between vectorized and row
								// executions.
								args.colIdxsToCheckForEquality = make([]int, nEqCols)
								for i := range args.colIdxsToCheckForEquality {
									args.colIdxsToCheckForEquality[i] = int(lEqCols[i])
								}
							}

							if err := verifyColOperator(t, args); err != nil {
								fmt.Printf("--- spillForced = %t join type = %s onExpr = %q"+
									" q seed = %d run = %d ---\n",
									spillForced, testSpec.joinType.String(), onExpr.Expr, seed, run)
								fmt.Printf("--- lEqCols = %v rEqCols = %v ---\n", lEqCols, rEqCols)
								prettyPrintTypes(lInputTypes, "left_table" /* tableName */)
								prettyPrintTypes(rInputTypes, "right_table" /* tableName */)
								prettyPrintInput(lRows, lInputTypes, "left_table" /* tableName */)
								prettyPrintInput(rRows, rInputTypes, "right_table" /* tableName */)
								t.Fatal(err)
							}
							if onExpr.Expr == "" {
								triedWithoutOnExpr = true
							} else {
								triedWithOnExpr = true
							}
						}
					}
				}
			}
		}
	}
}

// generateEqualityColumns produces a random permutation of nEqCols random
// columns on a table with nCols columns, so nEqCols must be not greater than
// nCols.
func generateEqualityColumns(rng *rand.Rand, nCols int, nEqCols int) []uint32 {
	if nEqCols > nCols {
		panic("nEqCols > nCols in generateEqualityColumns")
	}
	eqCols := make([]uint32, 0, nEqCols)
	for _, eqCol := range rng.Perm(nCols)[:nEqCols] {
		eqCols = append(eqCols, uint32(eqCol))
	}
	return eqCols
}

func TestMergeJoinerAgainstProcessor(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)
	var da tree.DatumAlloc
	evalCtx := eval.MakeTestingEvalContext(cluster.MakeTestingClusterSettings())
	defer evalCtx.Stop(context.Background())

	type mjTestSpec struct {
		joinType        descpb.JoinType
		anyOrder        bool
		onExprSupported bool
	}
	testSpecs := []mjTestSpec{
		{
			joinType:        descpb.InnerJoin,
			onExprSupported: true,
		},
		{
			joinType: descpb.LeftOuterJoin,
		},
		{
			joinType: descpb.RightOuterJoin,
		},
		{
			joinType: descpb.FullOuterJoin,
			// FULL OUTER JOIN doesn't guarantee any ordering on its output (since it
			// is ambiguous), so we're comparing the outputs as sets.
			anyOrder: true,
		},
		{
			joinType: descpb.LeftSemiJoin,
		},
		{
			joinType: descpb.LeftAntiJoin,
		},
		{
			joinType: descpb.IntersectAllJoin,
		},
		{
			joinType: descpb.ExceptAllJoin,
		},
		{
			joinType: descpb.RightSemiJoin,
		},
		{
			joinType: descpb.RightAntiJoin,
		},
	}

	rng, seed := randutil.NewTestRand()
	nRuns := 3
	nRows := 10
	maxCols := 3
	maxNum := 5
	intTyps := make([]*types.T, maxCols)
	for i := range intTyps {
		intTyps[i] = types.Int
	}

	for run := 0; run < nRuns; run++ {
		for _, testSpec := range testSpecs {
			for nCols := 1; nCols <= maxCols; nCols++ {
				for nOrderingCols := 1; nOrderingCols <= nCols; nOrderingCols++ {
					triedWithoutOnExpr, triedWithOnExpr := false, false
					if !testSpec.onExprSupported {
						triedWithOnExpr = true
					}
					for !triedWithoutOnExpr || !triedWithOnExpr {
						var (
							lRows, rRows                 rowenc.EncDatumRows
							lInputTypes, rInputTypes     []*types.T
							lOrderingCols, rOrderingCols []execinfrapb.Ordering_Column
							usingRandomTypes             bool
						)
						if rng.Float64() < randTypesProbability {
							lInputTypes = generateRandomSupportedTypes(rng, nCols)
							lOrderingCols = generateColumnOrdering(rng, nCols, nOrderingCols)
							rInputTypes = append(rInputTypes[:0], lInputTypes...)
							rOrderingCols = append(rOrderingCols[:0], lOrderingCols...)
							rng.Shuffle(nOrderingCols, func(i, j int) {
								iColIdx, jColIdx := rOrderingCols[i].ColIdx, rOrderingCols[j].ColIdx
								rInputTypes[iColIdx], rInputTypes[jColIdx] = rInputTypes[jColIdx], rInputTypes[iColIdx]
								rOrderingCols[i], rOrderingCols[j] = rOrderingCols[j], rOrderingCols[i]
							})
							rInputTypes = randomizeJoinRightTypes(rng, rInputTypes)
							lRows = randgen.RandEncDatumRowsOfTypes(rng, nRows, lInputTypes)
							rRows = randgen.RandEncDatumRowsOfTypes(rng, nRows, rInputTypes)
							usingRandomTypes = true
						} else {
							lInputTypes = intTyps[:nCols]
							rInputTypes = lInputTypes
							lRows = randgen.MakeRandIntRowsInRange(rng, nRows, nCols, maxNum, nullProbability)
							rRows = randgen.MakeRandIntRowsInRange(rng, nRows, nCols, maxNum, nullProbability)
							lOrderingCols = generateColumnOrdering(rng, nCols, nOrderingCols)
							rOrderingCols = generateColumnOrdering(rng, nCols, nOrderingCols)
						}
						// Set the directions of both columns to be the same.
						for i, lCol := range lOrderingCols {
							rOrderingCols[i].Direction = lCol.Direction
						}

						lMatchedCols := execinfrapb.ConvertToColumnOrdering(execinfrapb.Ordering{Columns: lOrderingCols})
						rMatchedCols := execinfrapb.ConvertToColumnOrdering(execinfrapb.Ordering{Columns: rOrderingCols})
						sort.Slice(lRows, func(i, j int) bool {
							cmp, err := lRows[i].Compare(context.Background(), lInputTypes, &da, lMatchedCols, &evalCtx, lRows[j])
							if err != nil {
								t.Fatal(err)
							}
							return cmp < 0
						})
						sort.Slice(rRows, func(i, j int) bool {
							cmp, err := rRows[i].Compare(context.Background(), rInputTypes, &da, rMatchedCols, &evalCtx, rRows[j])
							if err != nil {
								t.Fatal(err)
							}
							return cmp < 0
						})
						outputTypes := testSpec.joinType.MakeOutputTypes(lInputTypes, rInputTypes)
						outputColumns := make([]uint32, len(outputTypes))
						for i := range outputColumns {
							outputColumns[i] = uint32(i)
						}

						var onExpr execinfrapb.Expression
						if triedWithoutOnExpr {
							colTypes := append(lInputTypes, rInputTypes...)
							onExpr = generateFilterExpr(
								rng, nCols, nOrderingCols, colTypes, usingRandomTypes, false, /* forceSingleSide */
							)
						}
						mjSpec := &execinfrapb.MergeJoinerSpec{
							OnExpr:        onExpr,
							LeftOrdering:  execinfrapb.Ordering{Columns: lOrderingCols},
							RightOrdering: execinfrapb.Ordering{Columns: rOrderingCols},
							Type:          testSpec.joinType,
							NullEquality:  testSpec.joinType.IsSetOpJoin(),
						}
						pspec := &execinfrapb.ProcessorSpec{
							Input:       []execinfrapb.InputSyncSpec{{ColumnTypes: lInputTypes}, {ColumnTypes: rInputTypes}},
							Core:        execinfrapb.ProcessorCoreUnion{MergeJoiner: mjSpec},
							Post:        execinfrapb.PostProcessSpec{Projection: true, OutputColumns: outputColumns},
							ResultTypes: outputTypes,
						}
						args := verifyColOperatorArgs{
							anyOrder:   testSpec.anyOrder,
							inputTypes: [][]*types.T{lInputTypes, rInputTypes},
							inputs:     []rowenc.EncDatumRows{lRows, rRows},
							pspec:      pspec,
							rng:        rng,
						}
						if testSpec.joinType.IsSetOpJoin() && nOrderingCols < nCols {
							// The output of set operation joins is not fully
							// deterministic when there are non-equality
							// columns, however, the rows must match on the
							// equality columns between vectorized and row
							// executions.
							args.colIdxsToCheckForEquality = make([]int, nOrderingCols)
							for i := range args.colIdxsToCheckForEquality {
								args.colIdxsToCheckForEquality[i] = int(lOrderingCols[i].ColIdx)
							}
						}
						if err := verifyColOperator(t, args); err != nil {
							fmt.Printf("--- join type = %s onExpr = %q seed = %d run = %d ---\n",
								testSpec.joinType.String(), onExpr.Expr, seed, run)
							fmt.Printf("--- left ordering = %v right ordering = %v ---\n", lOrderingCols, rOrderingCols)
							prettyPrintTypes(lInputTypes, "left_table" /* tableName */)
							prettyPrintTypes(rInputTypes, "right_table" /* tableName */)
							prettyPrintInput(lRows, lInputTypes, "left_table" /* tableName */)
							prettyPrintInput(rRows, rInputTypes, "right_table" /* tableName */)
							t.Fatal(err)
						}
						if onExpr.Expr == "" {
							triedWithoutOnExpr = true
						} else {
							triedWithOnExpr = true
						}
					}
				}
			}
		}
	}
}

// generateColumnOrdering produces a random ordering of nOrderingCols columns
// on a table with nCols columns, so nOrderingCols must be not greater than
// nCols.
func generateColumnOrdering(
	rng *rand.Rand, nCols int, nOrderingCols int,
) []execinfrapb.Ordering_Column {
	if nOrderingCols > nCols {
		panic("nOrderingCols > nCols in generateColumnOrdering")
	}

	orderingCols := make([]execinfrapb.Ordering_Column, nOrderingCols)
	for i, col := range rng.Perm(nCols)[:nOrderingCols] {
		orderingCols[i] = execinfrapb.Ordering_Column{
			ColIdx:    uint32(col),
			Direction: execinfrapb.Ordering_Column_Direction(rng.Intn(2)),
		}
	}
	return orderingCols
}

// generateFilterExpr populates an execinfrapb.Expression that contains a
// single comparison which can be either comparing a column from the left
// against a column from the right or comparing a column from either side
// against a constant.
// If forceConstComparison is true, then the comparison against the constant
// will be used.
// If forceSingleSide is true, then the comparison of a column from the single
// side against a constant will be used ("single" meaning that the join type
// doesn't output columns from both sides).
func generateFilterExpr(
	rng *rand.Rand,
	nCols int,
	nEqCols int,
	colTypes []*types.T,
	forceConstComparison bool,
	forceSingleSide bool,
) execinfrapb.Expression {
	var colIdx int
	// When all columns are used in equality comparison between inputs, there is
	// only one interesting case when a column from either side is compared
	// against a constant. The second conditional is us choosing to compare
	// against a constant.
	singleSideComparison := nCols == nEqCols || rng.Float64() < 0.33 || forceConstComparison || forceSingleSide
	colIdx = rng.Intn(nCols)
	if singleSideComparison {
		if !forceSingleSide && rng.Float64() >= 0.5 {
			// Use right side.
			colIdx += nCols
		}
	}
	colType := colTypes[colIdx]

	var comparison string
	comparisons := []string{"=", "<>", "<", ">"}
	n := len(comparisons)
	if colType.Family() == types.JsonFamily {
		// JSON can't be sorted, only != and = compared. Pick from the first half
		// of the list
		n = 2
	}
	comparison = comparisons[rng.Intn(n)]
	if singleSideComparison {
		constDatum := randgen.RandDatum(rng, colTypes[colIdx], true /* nullOk */)
		constDatumString := constDatum.String()
		switch colTypes[colIdx].Family() {
		case types.FloatFamily, types.DecimalFamily:
			if strings.Contains(strings.ToLower(constDatumString), "nan") ||
				strings.Contains(strings.ToLower(constDatumString), "inf") {
				// We need to surround special numerical values with quotes.
				constDatumString = fmt.Sprintf("'%s'", constDatumString)
			}
		case types.JsonFamily:
			constDatumString = fmt.Sprintf("%s::json", constDatumString)
		}
		return execinfrapb.Expression{Expr: fmt.Sprintf("@%d %s %s", colIdx+1, comparison, constDatumString)}
	}
	// We will compare a column from the left against a column from the right.
	rightColIdx := rng.Intn(nCols) + nCols
	return execinfrapb.Expression{Expr: fmt.Sprintf("@%d %s @%d", colIdx+1, comparison, rightColIdx+1)}
}

func TestWindowFunctionsAgainstProcessor(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	rng, seed := randutil.NewTestRand()

	const manyRowsProbability = 0.05
	const fewRows = 10
	var manyRows = 2*coldata.BatchSize() + rng.Intn(coldata.BatchSize())
	var usedManyRows bool

	maxCols := 4
	maxArgs := 3
	typs := make([]*types.T, maxCols, maxCols+maxArgs)
	for i := range typs {
		typs[i] = types.Int
	}

	runTests := func(
		fun execinfrapb.WindowerSpec_Func,
		funcName string,
		argTypes []*types.T,
		orderNonPartitionCols bool,
	) {
		nRows := fewRows
		if !usedManyRows && rng.Float64() < manyRowsProbability {
			nRows = manyRows
			usedManyRows = true
		}
		for _, partitionBy := range [][]uint32{
			{},     // No PARTITION BY clause.
			{0},    // Partitioning on the first input column.
			{0, 1}, // Partitioning on the first and second input columns.
		} {
			for _, nOrderingCols := range []int{
				0, // No ORDER BY clause.
				1, // ORDER BY on at most one column.
				2, // ORDER BY on at most two columns.
			} {
				for nCols := 1; nCols <= maxCols; nCols++ {
					if len(partitionBy) > nCols || nOrderingCols > nCols {
						continue
					}

					var argsIdxs []uint32
					inputTypes := make([]*types.T, nCols, nCols+len(argTypes))
					copy(inputTypes, typs[:nCols])
					inputTypes = append(inputTypes, argTypes...)
					for i := range argTypes {
						// The arg columns will be appended to the end of the other columns.
						argsIdxs = append(argsIdxs, uint32(nCols+i))
					}

					rows := randgen.RandEncDatumRowsOfTypes(rng, nRows, inputTypes)
					for _, row := range rows {
						for i := 0; i < len(inputTypes); i++ {
							if rng.Float64() < nullProbability {
								row[i] = rowenc.EncDatum{Datum: tree.DNull}
							}
						}
					}

					if orderNonPartitionCols {
						// The output of this window function is not deterministic if there
						// are columns that are not present in either PARTITION BY or
						// ORDER BY clauses, so we require that all non-partitioning columns
						// are ordering columns.
						nOrderingCols = len(inputTypes)
					}

					ordering := generateOrderingGivenPartitionBy(rng, len(inputTypes), nOrderingCols, partitionBy)
					windowerSpec := &execinfrapb.WindowerSpec{
						PartitionBy: partitionBy,
						WindowFns: []execinfrapb.WindowerSpec_WindowFn{
							{
								Func:         fun,
								ArgsIdxs:     argsIdxs,
								Ordering:     ordering,
								OutputColIdx: uint32(len(inputTypes)),
								FilterColIdx: tree.NoColumnIdx,
							},
						},
					}
					windowerSpec.WindowFns[0].Frame = generateWindowFrame(t, rng, &ordering, inputTypes)

					_, outputType, err := execagg.GetWindowFunctionInfo(fun, argTypes...)
					require.NoError(t, err)
					pspec := &execinfrapb.ProcessorSpec{
						Input:       []execinfrapb.InputSyncSpec{{ColumnTypes: inputTypes}},
						Core:        execinfrapb.ProcessorCoreUnion{Windower: windowerSpec},
						ResultTypes: append(inputTypes, outputType),
					}
					args := verifyColOperatorArgs{
						rng:        rng,
						anyOrder:   true,
						inputTypes: [][]*types.T{inputTypes},
						inputs:     []rowenc.EncDatumRows{rows},
						pspec:      pspec,
						// Some window functions don't buffer anything, so they
						// won't ever spill to disk. Rather than examining each
						// function and checking whether it buffers or not,
						// we're being lazy and don't require the spilling to
						// occur.
						forcedDiskSpillMightNotOccur: true,
					}
					for _, spillForced := range []bool{false, true} {
						if spillForced && nRows == manyRows {
							// Don't force disk spilling with many rows since it
							// might take a while.
							continue
						}
						args.forceDiskSpill = spillForced
						if err := verifyColOperator(t, args); err != nil {
							if strings.Contains(err.Error(), "different errors returned") {
								// Columnar and row-based windowers are likely to hit
								// different errors, and we will swallow those and move
								// on.
								continue
							}
							if strings.Contains(err.Error(), "Err:windower-limited: memory budget exceeded") {
								// The row-based windower can hit a memory error
								// because some of its state cannot be spilled
								// to disk. Ignore such cases.
								continue
							}
							if strings.Contains(err.Error(), "integer out of range") &&
								fun.AggregateFunc != nil && *fun.AggregateFunc == execinfrapb.SumInt {
								// The columnar implementation of this window function uses the
								// sliding window optimization, but the row engine version
								// doesn't. As a result, in some cases the row engine will
								// overflow while the vectorized engine doesn't.
								continue
							}
							fmt.Printf("force disk spill: %t\n", spillForced)
							fmt.Printf("window function: %s\n", funcName)
							fmt.Printf("partitionCols: %v\n", partitionBy)
							fmt.Print("ordering: ")
							for i := range ordering.Columns {
								fmt.Printf("%v %v, ", ordering.Columns[i].ColIdx, ordering.Columns[i].Direction)
							}
							fmt.Println()
							fmt.Printf("argIdxs: %v\n", argsIdxs)
							frame := windowerSpec.WindowFns[0].Frame
							fmt.Printf("frame mode: %v\n", frame.Mode)
							fmt.Printf("start bound: %v\n", frame.Bounds.Start)
							fmt.Printf("end bound: %v\n", *frame.Bounds.End)
							fmt.Printf("frame exclusion: %v\n", frame.Exclusion)
							fmt.Printf("seed = %d\n", seed)
							prettyPrintTypes(inputTypes, "t" /* tableName */)
							prettyPrintInput(rows, inputTypes, "t" /* tableName */)
							t.Fatal(err)
						}
					}
				}
			}
		}
	}

	for windowFnIdx := 0; windowFnIdx < len(execinfrapb.WindowerSpec_WindowFunc_name); windowFnIdx++ {
		windowFn := execinfrapb.WindowerSpec_WindowFunc(windowFnIdx)
		var argTypes []*types.T
		randArgType := types.Int
		if rand.Float64() < randTypesProbability {
			randArgType = generateRandomSupportedTypes(rng, 1 /* nCols */)[0]
		}
		switch windowFn {
		case execinfrapb.WindowerSpec_NTILE:
			argTypes = []*types.T{types.Int}
		case execinfrapb.WindowerSpec_LAG, execinfrapb.WindowerSpec_LEAD:
			argTypes = []*types.T{randArgType, types.Int, randArgType}
		case execinfrapb.WindowerSpec_FIRST_VALUE, execinfrapb.WindowerSpec_LAST_VALUE:
			argTypes = []*types.T{randArgType}
		case execinfrapb.WindowerSpec_NTH_VALUE:
			argTypes = []*types.T{randArgType, types.Int}
		}
		orderNonPartitionCols := windowFn == execinfrapb.WindowerSpec_ROW_NUMBER ||
			windowFn == execinfrapb.WindowerSpec_NTILE ||
			windowFn == execinfrapb.WindowerSpec_LAG ||
			windowFn == execinfrapb.WindowerSpec_LEAD ||
			windowFn == execinfrapb.WindowerSpec_FIRST_VALUE ||
			windowFn == execinfrapb.WindowerSpec_LAST_VALUE ||
			windowFn == execinfrapb.WindowerSpec_NTH_VALUE
		runTests(execinfrapb.WindowerSpec_Func{WindowFunc: &windowFn},
			windowFn.String(), argTypes, orderNonPartitionCols)
	}

	for aggFnIdx := 0; aggFnIdx < len(execinfrapb.AggregatorSpec_Func_name); aggFnIdx++ {
		aggFn := execinfrapb.AggregatorSpec_Func(aggFnIdx)
		if !colexecagg.IsAggOptimized(aggFn) || aggFn == execinfrapb.AnyNotNull {
			// any_not_null is an internal function.
			continue
		}
		var argTypes []*types.T
		switch aggFn {
		case execinfrapb.CountRows:
			// count_rows takes no arguments.
		case execinfrapb.BoolOr, execinfrapb.BoolAnd:
			argTypes = []*types.T{types.Bool}
		case execinfrapb.ConcatAgg:
			argTypes = []*types.T{types.String}
		default:
			argTypes = []*types.T{types.Int}
			if rand.Float64() < randTypesProbability &&
				(aggFn == execinfrapb.Min || aggFn == execinfrapb.Max) {
				argTypes[0] = generateRandomSupportedTypes(rng, 1 /* nCols */)[0]
			}
		}
		runTests(execinfrapb.WindowerSpec_Func{AggregateFunc: &aggFn},
			aggFn.String(), argTypes, true /* orderNonPartitionCols */)
	}
}

// generateRandomSupportedTypes generates nCols random types that are supported
// by the vectorized engine natively (i.e. datum-backed types are skipped).
func generateRandomSupportedTypes(rng *rand.Rand, nCols int) []*types.T {
	typs := make([]*types.T, 0, nCols)
	for len(typs) < nCols {
		typ := randgen.RandType(rng)
		family := typeconv.TypeFamilyToCanonicalTypeFamily(typ.Family())
		if family == typeconv.DatumVecCanonicalTypeFamily {
			// At the moment, we disallow datum-backed types.
			// TODO(yuzefovich): remove this.
			continue
		}
		typs = append(typs, typ)
	}
	return typs
}

// randomizeJoinRightTypes returns somewhat random types to be used for the
// right side of the join such that they would have produced equality
// conditions in the non-test environment (currently, due to #43060, we don't
// support joins of different types without pushing the mixed-type equality
// checks into the ON condition).
func randomizeJoinRightTypes(rng *rand.Rand, leftTypes []*types.T) []*types.T {
	typs := make([]*types.T, len(leftTypes))
	for i, inputType := range leftTypes {
		switch inputType.Family() {
		case types.IntFamily:
			// We want to randomize integer types because they have different
			// physical representations.
			switch rng.Intn(3) {
			case 0:
				typs[i] = types.Int2
			case 1:
				typs[i] = types.Int4
			default:
				typs[i] = types.Int
			}
		default:
			typs[i] = inputType
		}
	}
	return typs
}

// generateOrderingGivenPartitionBy produces a random ordering of up to
// nOrderingCols columns on a table with nCols columns such that only columns
// not present in partitionBy are used. This is useful to simulate how
// optimizer plans window functions - for example, with an OVER clause as
// (PARTITION BY a ORDER BY a DESC), the optimizer will omit the ORDER BY
// clause entirely.
func generateOrderingGivenPartitionBy(
	rng *rand.Rand, nCols int, nOrderingCols int, partitionBy []uint32,
) execinfrapb.Ordering {
	var ordering execinfrapb.Ordering
	if nOrderingCols == 0 || len(partitionBy) == nCols {
		return ordering
	}
	ordering = execinfrapb.Ordering{Columns: make([]execinfrapb.Ordering_Column, 0, nOrderingCols)}
	for len(ordering.Columns) == 0 {
		for _, ordCol := range generateColumnOrdering(rng, nCols, nOrderingCols) {
			usedInPartitionBy := false
			for _, p := range partitionBy {
				if p == ordCol.ColIdx {
					usedInPartitionBy = true
					break
				}
			}
			if !usedInPartitionBy {
				ordering.Columns = append(ordering.Columns, ordCol)
			}
		}
	}
	return ordering
}

func generateWindowFrame(
	t *testing.T, rng *rand.Rand, ordering *execinfrapb.Ordering, inputTypes []*types.T,
) *execinfrapb.WindowerSpec_Frame {
	var modes = []execinfrapb.WindowerSpec_Frame_Mode{
		execinfrapb.WindowerSpec_Frame_RANGE,
		execinfrapb.WindowerSpec_Frame_ROWS,
		execinfrapb.WindowerSpec_Frame_GROUPS,
	}
	var boundTypes = []execinfrapb.WindowerSpec_Frame_BoundType{
		execinfrapb.WindowerSpec_Frame_UNBOUNDED_PRECEDING,
		execinfrapb.WindowerSpec_Frame_OFFSET_PRECEDING,
		execinfrapb.WindowerSpec_Frame_CURRENT_ROW,
		execinfrapb.WindowerSpec_Frame_OFFSET_FOLLOWING,
		execinfrapb.WindowerSpec_Frame_UNBOUNDED_FOLLOWING,
	}
	var exclusionTypes = []execinfrapb.WindowerSpec_Frame_Exclusion{
		execinfrapb.WindowerSpec_Frame_NO_EXCLUSION,
		execinfrapb.WindowerSpec_Frame_EXCLUDE_CURRENT_ROW,
		execinfrapb.WindowerSpec_Frame_EXCLUDE_GROUP,
		execinfrapb.WindowerSpec_Frame_EXCLUDE_TIES,
	}

	mode := modes[rng.Intn(len(modes))]

	// Ensure that start and end bound types are syntactically valid.
	startBoundIdx := rng.Intn(len(boundTypes) - 1)
	var endBoundIdx int
	for {
		endBoundIdx = rng.Intn(len(boundTypes)-1) + 1
		if endBoundIdx >= startBoundIdx {
			break
		}
	}
	startBoundType := boundTypes[startBoundIdx]
	endBoundType := boundTypes[endBoundIdx]

	windowFrameBoundIsOffset := func(boundType execinfrapb.WindowerSpec_Frame_BoundType) bool {
		return boundType == execinfrapb.WindowerSpec_Frame_OFFSET_PRECEDING ||
			boundType == execinfrapb.WindowerSpec_Frame_OFFSET_FOLLOWING
	}

	if len(ordering.Columns) == 0 {
		// RANGE and GROUPS modes require at least one ordering column.
		mode = execinfrapb.WindowerSpec_Frame_ROWS
	}
	if mode == execinfrapb.WindowerSpec_Frame_RANGE &&
		(len(ordering.Columns) != 1 || !types.IsAdditiveType(inputTypes[ordering.Columns[0].ColIdx])) {
		// RANGE mode with OFFSET PRECEDING or OFFSET FOLLOWING requires there to
		// be exactly one ordering column that is numeric or datetime.
		if windowFrameBoundIsOffset(startBoundType) {
			startBoundType = execinfrapb.WindowerSpec_Frame_UNBOUNDED_PRECEDING
		}
		if windowFrameBoundIsOffset(endBoundType) {
			endBoundType = execinfrapb.WindowerSpec_Frame_UNBOUNDED_FOLLOWING
		}
	}

	exclusion := exclusionTypes[rng.Intn(len(exclusionTypes))]

	frame := &execinfrapb.WindowerSpec_Frame{
		Mode: mode,
		Bounds: execinfrapb.WindowerSpec_Frame_Bounds{
			Start: execinfrapb.WindowerSpec_Frame_Bound{BoundType: startBoundType},
			End:   &execinfrapb.WindowerSpec_Frame_Bound{BoundType: endBoundType},
		},
		Exclusion: exclusion,
	}

	const maxUInt64Offset = 10
	if windowFrameBoundIsOffset(startBoundType) || windowFrameBoundIsOffset(endBoundType) {
		if frame.Mode == execinfrapb.WindowerSpec_Frame_ROWS ||
			frame.Mode == execinfrapb.WindowerSpec_Frame_GROUPS {
			frame.Bounds.Start.IntOffset = rng.Uint64() % maxUInt64Offset
			frame.Bounds.End.IntOffset = rng.Uint64() % maxUInt64Offset
		} else {
			// We can assume that there is exactly one ordering column of an additive
			// type, since we checked above.
			colIdx := ordering.Columns[0].ColIdx
			colEncoding := catenumpb.DatumEncoding_ASCENDING_KEY
			if ordering.Columns[0].Direction == execinfrapb.Ordering_Column_DESC {
				colEncoding = catenumpb.DatumEncoding_DESCENDING_KEY
			}
			offsetType := colexecwindow.GetOffsetTypeFromOrderColType(t, inputTypes[colIdx])
			startOffset := colexectestutils.MakeRandWindowFrameRangeOffset(t, rng, offsetType)
			endOffset := colexectestutils.MakeRandWindowFrameRangeOffset(t, rng, offsetType)
			frame.Bounds.Start.TypedOffset = colexectestutils.EncodeWindowFrameOffset(t, startOffset)
			frame.Bounds.End.TypedOffset = colexectestutils.EncodeWindowFrameOffset(t, endOffset)
			frame.Bounds.Start.OffsetType = execinfrapb.DatumInfo{Encoding: colEncoding, Type: offsetType}
			frame.Bounds.End.OffsetType = execinfrapb.DatumInfo{Encoding: colEncoding, Type: offsetType}
		}
	}

	return frame
}

// prettyPrintTypes prints out typs as a CREATE TABLE statement.
func prettyPrintTypes(typs []*types.T, tableName string) {
	fmt.Printf("CREATE TABLE %s(", tableName)
	colName := byte('a')
	for typIdx, typ := range typs {
		if typIdx < len(typs)-1 {
			fmt.Printf("%c %s, ", colName, typ.SQLStandardName())
		} else {
			fmt.Printf("%c %s);\n", colName, typ.SQLStandardName())
		}
		colName++
	}
}

// prettyPrintInput prints out rows as INSERT INTO tableName VALUES statement.
func prettyPrintInput(rows rowenc.EncDatumRows, inputTypes []*types.T, tableName string) {
	fmt.Printf("INSERT INTO %s VALUES\n", tableName)
	for rowIdx, row := range rows {
		fmt.Printf("(%s", row[0].String(inputTypes[0]))
		for i := range row[1:] {
			fmt.Printf(", %s", row[i+1].String(inputTypes[i+1]))
		}
		if rowIdx < len(rows)-1 {
			fmt.Printf("),\n")
		} else {
			fmt.Printf(");\n")
		}
	}
}
