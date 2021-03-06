// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package statistics

import (
	"fmt"
	"math"
	"strings"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/pingcap/tidb/context"
	"github.com/pingcap/tidb/model"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/util/sqlexec"
	"github.com/pingcap/tidb/util/types"
)

const (
	// Default number of buckets a column histogram has.
	defaultBucketCount = 256

	// When we haven't analyzed a table, we use pseudo statistics to estimate costs.
	// It has row count 10000000, equal condition selects 1/1000 of total rows, less condition selects 1/3 of total rows,
	// between condition selects 1/40 of total rows.
	pseudoRowCount    = 10000000
	pseudoEqualRate   = 1000
	pseudoLessRate    = 3
	pseudoBetweenRate = 40
)

// Table represents statistics for a table.
type Table struct {
	Info    *model.TableInfo
	Columns map[int64]*Column
	Indices map[int64]*Index
	Count   int64 // Total row count in a table.
	Pseudo  bool
}

// SaveToStorage saves stats table to storage.
func (h *Handle) SaveToStorage(ctx context.Context, t *Table) error {
	_, err := ctx.(sqlexec.SQLExecutor).Execute("begin")
	if err != nil {
		return errors.Trace(err)
	}
	txn := ctx.Txn()
	version := txn.StartTS()
	deleteSQL := fmt.Sprintf("delete from mysql.stats_meta where table_id = %d", t.Info.ID)
	_, err = ctx.(sqlexec.SQLExecutor).Execute(deleteSQL)
	if err != nil {
		return errors.Trace(err)
	}
	insertSQL := fmt.Sprintf("insert into mysql.stats_meta (version, table_id, count) values (%d, %d, %d)", version, t.Info.ID, t.Count)
	_, err = ctx.(sqlexec.SQLExecutor).Execute(insertSQL)
	if err != nil {
		return errors.Trace(err)
	}
	deleteSQL = fmt.Sprintf("delete from mysql.stats_histograms where table_id = %d", t.Info.ID)
	_, err = ctx.(sqlexec.SQLExecutor).Execute(deleteSQL)
	if err != nil {
		return errors.Trace(err)
	}
	deleteSQL = fmt.Sprintf("delete from mysql.stats_buckets where table_id = %d", t.Info.ID)
	_, err = ctx.(sqlexec.SQLExecutor).Execute(deleteSQL)
	if err != nil {
		return errors.Trace(err)
	}
	for _, col := range t.Columns {
		err = col.saveToStorage(ctx, t.Info.ID, 0)
		if err != nil {
			return errors.Trace(err)
		}
	}
	for _, idx := range t.Indices {
		err = idx.saveToStorage(ctx, t.Info.ID, 1)
		if err != nil {
			return errors.Trace(err)
		}
	}
	_, err = ctx.(sqlexec.SQLExecutor).Execute("commit")
	return errors.Trace(err)
}

// TableStatsFromStorage loads table stats info from storage.
func (h *Handle) TableStatsFromStorage(ctx context.Context, info *model.TableInfo, count int64) (*Table, error) {
	table := &Table{
		Info:    info,
		Count:   count,
		Columns: make(map[int64]*Column, len(info.Columns)),
		Indices: make(map[int64]*Index, len(info.Indices)),
	}
	selSQL := fmt.Sprintf("select table_id, is_index, hist_id, distinct_count from mysql.stats_histograms where table_id = %d", info.ID)
	rows, _, err := ctx.(sqlexec.RestrictedSQLExecutor).ExecRestrictedSQL(ctx, selSQL)
	if err != nil {
		return nil, errors.Trace(err)
	}
	// indexCount and columnCount record the number of indices and columns in table stats. If the number don't match with
	// tableInfo, we will return pseudo table.
	// TODO: In fact, we can return pseudo column.
	indexCount, columnCount := 0, 0
	for _, row := range rows {
		distinct := row.Data[3].GetInt64()
		histID := row.Data[2].GetInt64()
		if row.Data[1].GetInt64() > 0 {
			// process index
			var idx *Index
			for _, idxInfo := range info.Indices {
				if histID == idxInfo.ID {
					hg, err1 := histogramFromStorage(ctx, info.ID, histID, nil, distinct, 1)
					if err1 != nil {
						return nil, errors.Trace(err1)
					}
					idx = &Index{Histogram: *hg}
					break
				}
			}
			if idx != nil {
				table.Indices[idx.ID] = idx
				indexCount++
			} else {
				log.Warnf("We cannot find index id %d in table %s now. It may be deleted.", histID, info.Name)
			}
		} else {
			// process column
			var col *Column
			for _, colInfo := range info.Columns {
				if histID == colInfo.ID {
					var hg *Histogram
					hg, err = histogramFromStorage(ctx, info.ID, histID, &colInfo.FieldType, distinct, 0)
					if err != nil {
						return nil, errors.Trace(err)
					}
					col = &Column{Histogram: *hg}
					break
				}
			}
			if col != nil {
				table.Columns[col.ID] = col
				columnCount++
			} else {
				log.Warnf("We cannot find column id %d in table %s now. It may be deleted.", histID, info.Name)
			}
		}
	}
	if indexCount != len(info.Indices) {
		return nil, errors.New("The number of indices doesn't match with the schema")
	}
	if columnCount != len(info.Columns) {
		return nil, errors.New("The number of columns doesn't match with the schema")
	}
	return table, nil
}

// String implements Stringer interface.
func (t *Table) String() string {
	strs := make([]string, 0, len(t.Columns)+1)
	strs = append(strs, fmt.Sprintf("Table:%d count:%d", t.Info.ID, t.Count))
	for _, col := range t.Columns {
		strs = append(strs, col.String())
	}
	return strings.Join(strs, "\n")
}

// columnIsInvalid checks if this column is invalid.
func (t *Table) columnIsInvalid(colInfo *model.ColumnInfo) bool {
	if t.Pseudo {
		return true
	}
	_, ok := t.Columns[colInfo.ID]
	return !ok
}

// ColumnGreaterRowCount estimates the row count where the column greater than value.
func (t *Table) ColumnGreaterRowCount(sc *variable.StatementContext, value types.Datum, colInfo *model.ColumnInfo) (float64, error) {
	if t.columnIsInvalid(colInfo) {
		return float64(t.Count) / pseudoLessRate, nil
	}
	return t.Columns[colInfo.ID].greaterRowCount(sc, value)
}

// ColumnLessRowCount estimates the row count where the column less than value.
func (t *Table) ColumnLessRowCount(sc *variable.StatementContext, value types.Datum, colInfo *model.ColumnInfo) (float64, error) {
	if t.columnIsInvalid(colInfo) {
		return float64(t.Count) / pseudoLessRate, nil
	}
	return t.Columns[colInfo.ID].lessRowCount(sc, value)
}

// ColumnBetweenRowCount estimates the row count where column greater or equal to a and less than b.
func (t *Table) ColumnBetweenRowCount(sc *variable.StatementContext, a, b types.Datum, colInfo *model.ColumnInfo) (float64, error) {
	if t.columnIsInvalid(colInfo) {
		return float64(t.Count) / pseudoBetweenRate, nil
	}
	return t.Columns[colInfo.ID].betweenRowCount(sc, a, b)
}

// ColumnEqualRowCount estimates the row count where the column equals to value.
func (t *Table) ColumnEqualRowCount(sc *variable.StatementContext, value types.Datum, colInfo *model.ColumnInfo) (float64, error) {
	if t.columnIsInvalid(colInfo) {
		return float64(t.Count) / pseudoEqualRate, nil
	}
	return t.Columns[colInfo.ID].equalRowCount(sc, value)
}

// GetRowCountByIntColumnRanges estimates the row count by a slice of IntColumnRange.
func (t *Table) GetRowCountByIntColumnRanges(sc *variable.StatementContext, colID int64, intRanges []types.IntColumnRange) (float64, error) {
	c := t.Columns[colID]
	if t.Pseudo || c == nil || len(c.Buckets) == 0 {
		return getPseudoRowCountByIntRanges(intRanges, float64(t.Count)), nil
	}
	return c.getIntColumnRowCount(sc, intRanges, float64(t.Count))
}

// GetRowCountByIndexRanges estimates the row count by a slice of IndexRange.
func (t *Table) GetRowCountByIndexRanges(sc *variable.StatementContext, idxID int64, indexRanges []*types.IndexRange, inAndEQCnt int) (float64, error) {
	idx := t.Indices[idxID]
	if t.Pseudo || idx == nil || len(idx.Buckets) == 0 {
		return getPseudoRowCountByIndexRanges(sc, indexRanges, inAndEQCnt, float64(t.Count))
	}
	return idx.getRowCount(sc, indexRanges, inAndEQCnt)
}

// PseudoTable creates a pseudo table statistics when statistic can not be found in KV store.
func PseudoTable(ti *model.TableInfo) *Table {
	t := &Table{Info: ti, Pseudo: true}
	t.Count = pseudoRowCount
	t.Columns = make(map[int64]*Column, len(ti.Columns))
	t.Indices = make(map[int64]*Index, len(ti.Indices))
	for _, v := range ti.Columns {
		c := &Column{
			Histogram: Histogram{
				ID:  v.ID,
				NDV: pseudoRowCount / 2,
			},
		}
		t.Columns[v.ID] = c
	}
	for _, v := range ti.Indices {
		idx := &Index{
			Histogram: Histogram{
				ID:  v.ID,
				NDV: pseudoRowCount / 2,
			},
		}
		t.Indices[v.ID] = idx
	}
	return t
}

func getPseudoRowCountByIndexRanges(sc *variable.StatementContext, indexRanges []*types.IndexRange, inAndEQCnt int,
	tableRowCount float64) (float64, error) {
	var totalCount float64
	for _, indexRange := range indexRanges {
		count := tableRowCount
		i := len(indexRange.LowVal) - 1
		if i > inAndEQCnt {
			i = inAndEQCnt
		}
		colRange := types.ColumnRange{Low: indexRange.LowVal[i], High: indexRange.HighVal[i]}
		rowCount, err := getPseudoRowCountByColumnRange(sc, tableRowCount, colRange)
		if err != nil {
			return 0, errors.Trace(err)
		}
		count = count / float64(tableRowCount) * float64(rowCount)
		// If the condition is a = 1, b = 1, c = 1, d = 1, we think every a=1, b=1, c=1 only filtrate 1/100 data,
		// so as to avoid collapsing too fast.
		for j := 0; j < i; j++ {
			count = count / float64(100)
		}
		totalCount += count
	}
	// To avoid the totalCount become too small.
	if uint64(totalCount) < 1000 {
		// We will not let the row count less than 1000 to avoid collapsing too fast in the future calculation.
		totalCount = 1000.0
	}
	if totalCount > tableRowCount {
		totalCount = tableRowCount / 3.0
	}
	return totalCount, nil
}

func getPseudoRowCountByColumnRange(sc *variable.StatementContext, tableRowCount float64, ran types.ColumnRange) (float64, error) {
	var rowCount float64
	var err error
	if ran.Low.Kind() == types.KindNull && ran.High.Kind() == types.KindMaxValue {
		return tableRowCount, nil
	} else if ran.Low.Kind() == types.KindMinNotNull {
		var nullCount float64
		nullCount = tableRowCount / pseudoEqualRate
		if ran.High.Kind() == types.KindMaxValue {
			rowCount = tableRowCount - nullCount
		} else if err == nil {
			lessCount := tableRowCount / pseudoLessRate
			rowCount = lessCount - nullCount
		}
	} else if ran.High.Kind() == types.KindMaxValue {
		rowCount = tableRowCount / pseudoLessRate
	} else {
		compare, err1 := ran.Low.CompareDatum(sc, ran.High)
		if err1 != nil {
			return 0, errors.Trace(err1)
		}
		if compare == 0 {
			rowCount = tableRowCount / pseudoEqualRate
		} else {
			rowCount = tableRowCount / pseudoBetweenRate
		}
	}
	if err != nil {
		return 0, errors.Trace(err)
	}
	return rowCount, nil
}

func getPseudoRowCountByIntRanges(intRanges []types.IntColumnRange, tableRowCount float64) float64 {
	var rowCount float64
	for _, rg := range intRanges {
		var cnt float64
		if rg.LowVal == math.MinInt64 && rg.HighVal == math.MaxInt64 {
			cnt = tableRowCount
		} else if rg.LowVal == math.MinInt64 {
			cnt = tableRowCount / pseudoLessRate
		} else if rg.HighVal == math.MaxInt64 {
			cnt = tableRowCount / pseudoLessRate
		} else {
			if rg.LowVal == rg.HighVal {
				cnt = tableRowCount / pseudoEqualRate
			} else {
				cnt = tableRowCount / pseudoBetweenRate
			}
		}
		if rg.HighVal-rg.LowVal > 0 && cnt > float64(rg.HighVal-rg.LowVal) {
			cnt = float64(rg.HighVal - rg.LowVal)
		}
		rowCount += cnt
	}
	if rowCount > tableRowCount {
		rowCount = tableRowCount
	}
	return rowCount
}
