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
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package statistics

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/parser/model"
	"github.com/pingcap/tidb/parser/mysql"
	"github.com/pingcap/tidb/planner/util/debugtrace"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/sessionctx/stmtctx"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/codec"
	"github.com/pingcap/tidb/util/collate"
	"github.com/pingcap/tidb/util/mathutil"
	"github.com/pingcap/tidb/util/ranger"
	"go.uber.org/atomic"
)

const (
	pseudoEqualRate   = 1000
	pseudoLessRate    = 3
	pseudoBetweenRate = 40
	pseudoColSize     = 8.0

	outOfRangeBetweenRate = 100
)

const (
	// PseudoVersion means the pseudo statistics version is 0.
	PseudoVersion uint64 = 0

	// PseudoRowCount export for other pkg to use.
	// When we haven't analyzed a table, we use pseudo statistics to estimate costs.
	// It has row count 10000, equal condition selects 1/1000 of total rows, less condition selects 1/3 of total rows,
	// between condition selects 1/40 of total rows.
	PseudoRowCount = 10000
)

var (
	// Below functions are used to solve cycle import problem.
	// TODO: resolve the cycle import problem in a more graceful way.

	// GetRowCountByIndexRanges is a function type to get row count by index ranges.
	GetRowCountByIndexRanges func(sctx sessionctx.Context, coll *HistColl, idxID int64, indexRanges []*ranger.Range) (result float64, err error)

	// GetRowCountByIntColumnRanges is a function type to get row count by int column ranges.
	GetRowCountByIntColumnRanges func(sctx sessionctx.Context, coll *HistColl, colID int64, intRanges []*ranger.Range) (result float64, err error)

	// GetRowCountByColumnRanges is a function type to get row count by column ranges.
	GetRowCountByColumnRanges func(sctx sessionctx.Context, coll *HistColl, colID int64, colRanges []*ranger.Range) (result float64, err error)
)

// Table represents statistics for a table.
type Table struct {
	ExtendedStats *ExtendedStatsColl
	Name          string
	HistColl
	Version uint64
	// TblInfoUpdateTS is the UpdateTS of the TableInfo used when filling this struct.
	// It is the schema version of the corresponding table. It is used to skip redundant
	// loading of stats, i.e, if the cached stats is already update-to-date with mysql.stats_xxx tables,
	// and the schema of the table does not change, we don't need to load the stats for this
	// table again.
	TblInfoUpdateTS uint64
}

// ExtendedStatsItem is the cached item of a mysql.stats_extended record.
type ExtendedStatsItem struct {
	StringVals string
	ColIDs     []int64
	ScalarVals float64
	Tp         uint8
}

// ExtendedStatsColl is a collection of cached items for mysql.stats_extended records.
type ExtendedStatsColl struct {
	Stats             map[string]*ExtendedStatsItem
	LastUpdateVersion uint64
}

// NewExtendedStatsColl allocate an ExtendedStatsColl struct.
func NewExtendedStatsColl() *ExtendedStatsColl {
	return &ExtendedStatsColl{Stats: make(map[string]*ExtendedStatsItem)}
}

const (
	// ExtendedStatsInited is the status for extended stats which are just registered but have not been analyzed yet.
	ExtendedStatsInited uint8 = iota
	// ExtendedStatsAnalyzed is the status for extended stats which have been collected in analyze.
	ExtendedStatsAnalyzed
	// ExtendedStatsDeleted is the status for extended stats which were dropped. These "deleted" records would be removed from storage by GCStats().
	ExtendedStatsDeleted
)

// HistColl is a collection of histogram. It collects enough information for plan to calculate the selectivity.
type HistColl struct {
	Columns map[int64]*Column
	Indices map[int64]*Index
	// Idx2ColumnIDs maps the index id to its column ids. It's used to calculate the selectivity in planner.
	Idx2ColumnIDs map[int64][]int64
	// ColID2IdxIDs maps the column id to a list index ids whose first column is it. It's used to calculate the selectivity in planner.
	ColID2IdxIDs map[int64][]int64
	PhysicalID   int64
	// TODO: add AnalyzeCount here
	RealtimeCount int64 // RealtimeCount is the current table row count, maintained by applying stats delta based on AnalyzeCount.
	ModifyCount   int64 // Total modify count in a table.

	// HavePhysicalID is true means this HistColl is from single table and have its ID's information.
	// The physical id is used when try to load column stats from storage.
	HavePhysicalID bool
	Pseudo         bool
}

// TableMemoryUsage records tbl memory usage
type TableMemoryUsage struct {
	ColumnsMemUsage map[int64]CacheItemMemoryUsage
	IndicesMemUsage map[int64]CacheItemMemoryUsage
	TableID         int64
	TotalMemUsage   int64
}

// TotalIdxTrackingMemUsage returns total indices' tracking memory usage
func (t *TableMemoryUsage) TotalIdxTrackingMemUsage() (sum int64) {
	for _, idx := range t.IndicesMemUsage {
		sum += idx.TrackingMemUsage()
	}
	return sum
}

// TotalColTrackingMemUsage returns total columns' tracking memory usage
func (t *TableMemoryUsage) TotalColTrackingMemUsage() (sum int64) {
	for _, col := range t.ColumnsMemUsage {
		sum += col.TrackingMemUsage()
	}
	return sum
}

// TotalTrackingMemUsage return total tracking memory usage
func (t *TableMemoryUsage) TotalTrackingMemUsage() int64 {
	return t.TotalIdxTrackingMemUsage() + t.TotalColTrackingMemUsage()
}

// TableCacheItem indicates the unit item stored in statsCache, eg: Column/Index
type TableCacheItem interface {
	ItemID() int64
	MemoryUsage() CacheItemMemoryUsage
	IsAllEvicted() bool
	GetEvictedStatus() int

	DropUnnecessaryData()
	IsStatsInitialized() bool
	GetStatsVer() int64
}

// CacheItemMemoryUsage indicates the memory usage of TableCacheItem
type CacheItemMemoryUsage interface {
	ItemID() int64
	TotalMemoryUsage() int64
	TrackingMemUsage() int64
	HistMemUsage() int64
	TopnMemUsage() int64
	CMSMemUsage() int64
}

// ColumnMemUsage records column memory usage
type ColumnMemUsage struct {
	ColumnID          int64
	HistogramMemUsage int64
	CMSketchMemUsage  int64
	FMSketchMemUsage  int64
	TopNMemUsage      int64
	TotalMemUsage     int64
}

// TotalMemoryUsage implements CacheItemMemoryUsage
func (c *ColumnMemUsage) TotalMemoryUsage() int64 {
	return c.TotalMemUsage
}

// ItemID implements CacheItemMemoryUsage
func (c *ColumnMemUsage) ItemID() int64 {
	return c.ColumnID
}

// TrackingMemUsage implements CacheItemMemoryUsage
func (c *ColumnMemUsage) TrackingMemUsage() int64 {
	return c.CMSketchMemUsage + c.TopNMemUsage + c.HistogramMemUsage
}

// HistMemUsage implements CacheItemMemoryUsage
func (c *ColumnMemUsage) HistMemUsage() int64 {
	return c.HistogramMemUsage
}

// TopnMemUsage implements CacheItemMemoryUsage
func (c *ColumnMemUsage) TopnMemUsage() int64 {
	return c.TopNMemUsage
}

// CMSMemUsage implements CacheItemMemoryUsage
func (c *ColumnMemUsage) CMSMemUsage() int64 {
	return c.CMSketchMemUsage
}

// IndexMemUsage records index memory usage
type IndexMemUsage struct {
	IndexID           int64
	HistogramMemUsage int64
	CMSketchMemUsage  int64
	TopNMemUsage      int64
	TotalMemUsage     int64
}

// TotalMemoryUsage implements CacheItemMemoryUsage
func (c *IndexMemUsage) TotalMemoryUsage() int64 {
	return c.TotalMemUsage
}

// ItemID implements CacheItemMemoryUsage
func (c *IndexMemUsage) ItemID() int64 {
	return c.IndexID
}

// TrackingMemUsage implements CacheItemMemoryUsage
func (c *IndexMemUsage) TrackingMemUsage() int64 {
	return c.CMSketchMemUsage + c.TopNMemUsage + c.HistogramMemUsage
}

// HistMemUsage implements CacheItemMemoryUsage
func (c *IndexMemUsage) HistMemUsage() int64 {
	return c.HistogramMemUsage
}

// TopnMemUsage implements CacheItemMemoryUsage
func (c *IndexMemUsage) TopnMemUsage() int64 {
	return c.TopNMemUsage
}

// CMSMemUsage implements CacheItemMemoryUsage
func (c *IndexMemUsage) CMSMemUsage() int64 {
	return c.CMSketchMemUsage
}

// MemoryUsage returns the total memory usage of this Table.
// it will only calc the size of Columns and Indices stats data of table.
// We ignore the size of other metadata in Table
func (t *Table) MemoryUsage() *TableMemoryUsage {
	tMemUsage := &TableMemoryUsage{
		TableID:         t.PhysicalID,
		ColumnsMemUsage: make(map[int64]CacheItemMemoryUsage),
		IndicesMemUsage: make(map[int64]CacheItemMemoryUsage),
	}
	for _, col := range t.Columns {
		if col != nil {
			colMemUsage := col.MemoryUsage()
			tMemUsage.ColumnsMemUsage[colMemUsage.ItemID()] = colMemUsage
			tMemUsage.TotalMemUsage += colMemUsage.TotalMemoryUsage()
		}
	}
	for _, index := range t.Indices {
		if index != nil {
			idxMemUsage := index.MemoryUsage()
			tMemUsage.IndicesMemUsage[idxMemUsage.ItemID()] = idxMemUsage
			tMemUsage.TotalMemUsage += idxMemUsage.TotalMemoryUsage()
		}
	}
	return tMemUsage
}

// Copy copies the current table.
func (t *Table) Copy() *Table {
	newHistColl := HistColl{
		PhysicalID:     t.PhysicalID,
		HavePhysicalID: t.HavePhysicalID,
		RealtimeCount:  t.RealtimeCount,
		Columns:        make(map[int64]*Column, len(t.Columns)),
		Indices:        make(map[int64]*Index, len(t.Indices)),
		Pseudo:         t.Pseudo,
		ModifyCount:    t.ModifyCount,
	}
	for id, col := range t.Columns {
		newHistColl.Columns[id] = col
	}
	for id, idx := range t.Indices {
		newHistColl.Indices[id] = idx
	}
	nt := &Table{
		HistColl:        newHistColl,
		Version:         t.Version,
		Name:            t.Name,
		TblInfoUpdateTS: t.TblInfoUpdateTS,
	}
	if t.ExtendedStats != nil {
		newExtStatsColl := &ExtendedStatsColl{
			Stats:             make(map[string]*ExtendedStatsItem),
			LastUpdateVersion: t.ExtendedStats.LastUpdateVersion,
		}
		for name, item := range t.ExtendedStats.Stats {
			newExtStatsColl.Stats[name] = item
		}
		nt.ExtendedStats = newExtStatsColl
	}
	return nt
}

// String implements Stringer interface.
func (t *Table) String() string {
	strs := make([]string, 0, len(t.Columns)+1)
	strs = append(strs, fmt.Sprintf("Table:%d RealtimeCount:%d", t.PhysicalID, t.RealtimeCount))
	cols := make([]*Column, 0, len(t.Columns))
	for _, col := range t.Columns {
		cols = append(cols, col)
	}
	slices.SortFunc(cols, func(i, j *Column) int { return cmp.Compare(i.ID, j.ID) })
	for _, col := range cols {
		strs = append(strs, col.String())
	}
	idxs := make([]*Index, 0, len(t.Indices))
	for _, idx := range t.Indices {
		idxs = append(idxs, idx)
	}
	slices.SortFunc(idxs, func(i, j *Index) int { return cmp.Compare(i.ID, j.ID) })
	for _, idx := range idxs {
		strs = append(strs, idx.String())
	}
	// TODO: concat content of ExtendedStatsColl
	return strings.Join(strs, "\n")
}

// IndexStartWithColumn finds the first index whose first column is the given column.
func (t *Table) IndexStartWithColumn(colName string) *Index {
	for _, index := range t.Indices {
		if index.Info.Columns[0].Name.L == colName {
			return index
		}
	}
	return nil
}

// ColumnByName finds the statistics.Column for the given column.
func (t *Table) ColumnByName(colName string) *Column {
	for _, c := range t.Columns {
		if c.Info.Name.L == colName {
			return c
		}
	}
	return nil
}

// GetStatsInfo returns their statistics according to the ID of the column or index, including histogram, CMSketch, TopN and FMSketch.
func (t *Table) GetStatsInfo(id int64, isIndex bool) (*Histogram, *CMSketch, *TopN, *FMSketch, bool) {
	if isIndex {
		if idxStatsInfo, ok := t.Indices[id]; ok {
			return idxStatsInfo.Histogram.Copy(),
				idxStatsInfo.CMSketch.Copy(), idxStatsInfo.TopN.Copy(), idxStatsInfo.FMSketch.Copy(), true
		}
		// newly added index which is not analyzed yet
		return nil, nil, nil, nil, false
	}
	if colStatsInfo, ok := t.Columns[id]; ok {
		return colStatsInfo.Histogram.Copy(), colStatsInfo.CMSketch.Copy(),
			colStatsInfo.TopN.Copy(), colStatsInfo.FMSketch.Copy(), true
	}
	// newly added column which is not analyzed yet
	return nil, nil, nil, nil, false
}

// GetColRowCount tries to get the row count of the a column if possible.
// This method is useful because this row count doesn't consider the modify count.
func (t *Table) GetColRowCount() float64 {
	ids := make([]int64, 0, len(t.Columns))
	for id := range t.Columns {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		col := t.Columns[id]
		if col != nil && col.IsFullLoad() {
			return col.TotalRowCount()
		}
	}
	return -1
}

// GetStatsHealthy calculates stats healthy if the table stats is not pseudo.
// If the table stats is pseudo, it returns 0, false, otherwise it returns stats healthy, true.
func (t *Table) GetStatsHealthy() (int64, bool) {
	if t == nil || t.Pseudo {
		return 0, false
	}
	var healthy int64
	count := float64(t.RealtimeCount)
	if histCount := t.GetColRowCount(); histCount > 0 {
		count = histCount
	}
	if float64(t.ModifyCount) < count {
		healthy = int64((1.0 - float64(t.ModifyCount)/count) * 100.0)
	} else if t.ModifyCount == 0 {
		healthy = 100
	}
	return healthy, true
}

type neededStatsMap struct {
	items map[model.TableItemID]struct{}
	m     sync.RWMutex
}

func (n *neededStatsMap) AllItems() []model.TableItemID {
	n.m.RLock()
	keys := make([]model.TableItemID, 0, len(n.items))
	for key := range n.items {
		keys = append(keys, key)
	}
	n.m.RUnlock()
	return keys
}

func (n *neededStatsMap) insert(col model.TableItemID) {
	n.m.Lock()
	n.items[col] = struct{}{}
	n.m.Unlock()
}

func (n *neededStatsMap) Delete(col model.TableItemID) {
	n.m.Lock()
	delete(n.items, col)
	n.m.Unlock()
}

func (n *neededStatsMap) Length() int {
	n.m.RLock()
	defer n.m.RUnlock()
	return len(n.items)
}

// RatioOfPseudoEstimate means if modifyCount / statsTblCount is greater than this ratio, we think the stats is invalid
// and use pseudo estimation.
var RatioOfPseudoEstimate = atomic.NewFloat64(0.7)

// IsInitialized returns true if any column/index stats of the table is initialized.
func (t *Table) IsInitialized() bool {
	for _, col := range t.Columns {
		if col != nil && col.IsStatsInitialized() {
			return true
		}
	}
	for _, idx := range t.Indices {
		if idx != nil && idx.IsStatsInitialized() {
			return true
		}
	}
	return false
}

// IsOutdated returns true if the table stats is outdated.
func (t *Table) IsOutdated() bool {
	rowcount := t.GetColRowCount()
	if rowcount < 0 {
		rowcount = float64(t.RealtimeCount)
	}
	if rowcount > 0 && float64(t.ModifyCount)/rowcount > RatioOfPseudoEstimate.Load() {
		return true
	}
	return false
}

// ColumnGreaterRowCount estimates the row count where the column greater than value.
func (t *Table) ColumnGreaterRowCount(sctx sessionctx.Context, value types.Datum, colID int64) float64 {
	c, ok := t.Columns[colID]
	if !ok || c.IsInvalid(sctx, t.Pseudo) {
		return float64(t.RealtimeCount) / pseudoLessRate
	}
	return c.greaterRowCount(value) * c.GetIncreaseFactor(t.RealtimeCount)
}

// ColumnLessRowCount estimates the row count where the column less than value. Note that null values are not counted.
func (t *Table) ColumnLessRowCount(sctx sessionctx.Context, value types.Datum, colID int64) float64 {
	c, ok := t.Columns[colID]
	if !ok || c.IsInvalid(sctx, t.Pseudo) {
		return float64(t.RealtimeCount) / pseudoLessRate
	}
	return c.lessRowCount(sctx, value) * c.GetIncreaseFactor(t.RealtimeCount)
}

// ColumnBetweenRowCount estimates the row count where column greater or equal to a and less than b.
func (t *Table) ColumnBetweenRowCount(sctx sessionctx.Context, a, b types.Datum, colID int64) (float64, error) {
	sc := sctx.GetSessionVars().StmtCtx
	c, ok := t.Columns[colID]
	if !ok || c.IsInvalid(sctx, t.Pseudo) {
		return float64(t.RealtimeCount) / pseudoBetweenRate, nil
	}
	aEncoded, err := codec.EncodeKey(sc, nil, a)
	if err != nil {
		return 0, err
	}
	bEncoded, err := codec.EncodeKey(sc, nil, b)
	if err != nil {
		return 0, err
	}
	count := c.BetweenRowCount(sctx, a, b, aEncoded, bEncoded)
	if a.IsNull() {
		count += float64(c.NullCount)
	}
	return count * c.GetIncreaseFactor(t.RealtimeCount), nil
}

// ColumnEqualRowCount estimates the row count where the column equals to value.
func (t *Table) ColumnEqualRowCount(sctx sessionctx.Context, value types.Datum, colID int64) (float64, error) {
	c, ok := t.Columns[colID]
	if !ok || c.IsInvalid(sctx, t.Pseudo) {
		return float64(t.RealtimeCount) / pseudoEqualRate, nil
	}
	encodedVal, err := codec.EncodeKey(sctx.GetSessionVars().StmtCtx, nil, value)
	if err != nil {
		return 0, err
	}
	result, err := c.equalRowCount(sctx, value, encodedVal, t.ModifyCount)
	result *= c.GetIncreaseFactor(t.RealtimeCount)
	return result, errors.Trace(err)
}

func (coll *HistColl) findAvailableStatsForCol(sctx sessionctx.Context, uniqueID int64) (isIndex bool, idx int64) {
	// try to find available stats in column stats
	if colStats, ok := coll.Columns[uniqueID]; ok && colStats != nil && !colStats.IsInvalid(sctx, coll.Pseudo) && colStats.IsFullLoad() {
		return false, uniqueID
	}
	// try to find available stats in single column index stats (except for prefix index)
	for idxStatsIdx, cols := range coll.Idx2ColumnIDs {
		if len(cols) == 1 && cols[0] == uniqueID {
			idxStats, ok := coll.Indices[idxStatsIdx]
			if ok &&
				idxStats.Info.Columns[0].Length == types.UnspecifiedLength &&
				!idxStats.IsInvalid(sctx, coll.Pseudo) &&
				idxStats.IsFullLoad() {
				return true, idxStatsIdx
			}
		}
	}
	return false, -1
}

// GetSelectivityByFilter try to estimate selectivity of expressions by evaluate the expressions using TopN, Histogram buckets boundaries and NULL.
// Currently, this method can only handle expressions involving a single column.
func (coll *HistColl) GetSelectivityByFilter(sctx sessionctx.Context, filters []expression.Expression) (ok bool, selectivity float64, err error) {
	// 1. Make sure the expressions
	//   (1) are safe to be evaluated here,
	//   (2) involve only one column,
	//   (3) and this column is not a "new collation" string column so that we're able to restore values from the stats.
	for _, filter := range filters {
		if expression.IsMutableEffectsExpr(filter) {
			return false, 0, nil
		}
	}
	if expression.ContainCorrelatedColumn(filters) {
		return false, 0, nil
	}
	cols := expression.ExtractColumnsFromExpressions(nil, filters, nil)
	if len(cols) != 1 {
		return false, 0, nil
	}
	col := cols[0]
	tp := col.RetType
	if types.IsString(tp.GetType()) && collate.NewCollationEnabled() && !collate.IsBinCollation(tp.GetCollate()) {
		return false, 0, nil
	}

	// 2. Get the available stats, make sure it's a ver2 stats and get the needed data structure from it.
	isIndex, i := coll.findAvailableStatsForCol(sctx, col.UniqueID)
	if i < 0 {
		return false, 0, nil
	}
	var statsVer, nullCnt int64
	var histTotalCnt, totalCnt float64
	var topnTotalCnt uint64
	var hist *Histogram
	var topn *TopN
	if isIndex {
		stats := coll.Indices[i]
		statsVer = stats.StatsVer
		hist = &stats.Histogram
		nullCnt = hist.NullCount
		topn = stats.TopN
	} else {
		stats := coll.Columns[i]
		statsVer = stats.StatsVer
		hist = &stats.Histogram
		nullCnt = hist.NullCount
		topn = stats.TopN
	}
	// Only in stats ver2, we can assume that: TopN + Histogram + NULL == All data
	if statsVer != Version2 {
		return false, 0, nil
	}
	topnTotalCnt = topn.TotalCount()
	histTotalCnt = hist.NotNullCount()
	totalCnt = float64(topnTotalCnt) + histTotalCnt + float64(nullCnt)

	var topNSel, histSel, nullSel float64

	// Prepare for evaluation.

	// For execution, we use Column.Index instead of Column.UniqueID to locate a column.
	// We have only one column here, so we set it to 0.
	originalIndex := col.Index
	col.Index = 0
	defer func() {
		// Restore the original Index to avoid unexpected situation.
		col.Index = originalIndex
	}()
	topNLen := 0
	histBucketsLen := hist.Len()
	if topn != nil {
		topNLen = len(topn.TopN)
	}
	c := chunk.NewChunkWithCapacity([]*types.FieldType{tp}, mathutil.Max(1, topNLen))
	selected := make([]bool, 0, mathutil.Max(histBucketsLen, topNLen))

	// 3. Calculate the TopN part selectivity.
	// This stage is considered as the core functionality of this method, errors in this stage would make this entire method fail.
	var topNSelectedCnt uint64
	if topn != nil {
		for _, item := range topn.TopN {
			_, val, err := codec.DecodeOne(item.Encoded)
			if err != nil {
				return false, 0, err
			}
			c.AppendDatum(0, &val)
		}
		selected, err = expression.VectorizedFilter(sctx, filters, chunk.NewIterator4Chunk(c), selected)
		if err != nil {
			return false, 0, err
		}
		for i, isTrue := range selected {
			if isTrue {
				topNSelectedCnt += topn.TopN[i].Count
			}
		}
	}
	topNSel = float64(topNSelectedCnt) / totalCnt

	// 4. Calculate the Histogram part selectivity.
	// The buckets upper bounds and the Bucket.Repeat are used like the TopN above.
	// The buckets lower bounds are used as random samples and are regarded equally.
	if hist != nil && histTotalCnt > 0 {
		selected = selected[:0]
		selected, err = expression.VectorizedFilter(sctx, filters, chunk.NewIterator4Chunk(hist.Bounds), selected)
		if err != nil {
			return false, 0, err
		}
		var bucketRepeatTotalCnt, bucketRepeatSelectedCnt, lowerBoundMatchCnt int64
		for i := range hist.Buckets {
			bucketRepeatTotalCnt += hist.Buckets[i].Repeat
			if len(selected) < 2*i {
				// This should not happen, but we add this check for safety.
				break
			}
			if selected[2*i] {
				lowerBoundMatchCnt++
			}
			if selected[2*i+1] {
				bucketRepeatSelectedCnt += hist.Buckets[i].Repeat
			}
		}
		var lowerBoundsRatio, upperBoundsRatio, lowerBoundsSel, upperBoundsSel float64
		upperBoundsRatio = mathutil.Min(float64(bucketRepeatTotalCnt)/histTotalCnt, 1)
		lowerBoundsRatio = 1 - upperBoundsRatio
		if bucketRepeatTotalCnt > 0 {
			upperBoundsSel = float64(bucketRepeatSelectedCnt) / float64(bucketRepeatTotalCnt)
		}
		lowerBoundsSel = float64(lowerBoundMatchCnt) / float64(histBucketsLen)
		histSel = lowerBoundsSel*lowerBoundsRatio + upperBoundsSel*upperBoundsRatio
		histSel *= histTotalCnt / totalCnt
	}

	// 5. Calculate the NULL part selectivity.
	// Errors in this staged would be returned, but would not make this entire method fail.
	c.Reset()
	c.AppendNull(0)
	selected = selected[:0]
	selected, err = expression.VectorizedFilter(sctx, filters, chunk.NewIterator4Chunk(c), selected)
	if err != nil || len(selected) != 1 || !selected[0] {
		nullSel = 0
	} else {
		nullSel = float64(nullCnt) / totalCnt
	}

	// 6. Get the final result.
	res := topNSel + histSel + nullSel
	return true, res, err
}

// PseudoAvgCountPerValue gets a pseudo average count if histogram not exists.
func (t *Table) PseudoAvgCountPerValue() float64 {
	return float64(t.RealtimeCount) / pseudoEqualRate
}

// GetOrdinalOfRangeCond gets the ordinal of the position range condition,
// if not exist, it returns the end position.
func GetOrdinalOfRangeCond(sc *stmtctx.StatementContext, ran *ranger.Range) int {
	for i := range ran.LowVal {
		a, b := ran.LowVal[i], ran.HighVal[i]
		cmp, err := a.Compare(sc, &b, ran.Collators[0])
		if err != nil {
			return 0
		}
		if cmp != 0 {
			return i
		}
	}
	return len(ran.LowVal)
}

// ID2UniqueID generates a new HistColl whose `Columns` is built from UniqueID of given columns.
func (coll *HistColl) ID2UniqueID(columns []*expression.Column) *HistColl {
	cols := make(map[int64]*Column)
	for _, col := range columns {
		colHist, ok := coll.Columns[col.ID]
		if ok {
			cols[col.UniqueID] = colHist
		}
	}
	newColl := &HistColl{
		PhysicalID:     coll.PhysicalID,
		HavePhysicalID: coll.HavePhysicalID,
		Pseudo:         coll.Pseudo,
		RealtimeCount:  coll.RealtimeCount,
		ModifyCount:    coll.ModifyCount,
		Columns:        cols,
	}
	return newColl
}

// GenerateHistCollFromColumnInfo generates a new HistColl whose ColID2IdxIDs and IdxID2ColIDs is built from the given parameter.
func (coll *HistColl) GenerateHistCollFromColumnInfo(tblInfo *model.TableInfo, columns []*expression.Column) *HistColl {
	newColHistMap := make(map[int64]*Column)
	colInfoID2UniqueID := make(map[int64]int64, len(columns))
	idxID2idxInfo := make(map[int64]*model.IndexInfo)
	for _, col := range columns {
		colInfoID2UniqueID[col.ID] = col.UniqueID
	}
	for id, colHist := range coll.Columns {
		uniqueID, ok := colInfoID2UniqueID[id]
		// Collect the statistics by the given columns.
		if ok {
			newColHistMap[uniqueID] = colHist
		}
	}
	for _, idxInfo := range tblInfo.Indices {
		idxID2idxInfo[idxInfo.ID] = idxInfo
	}
	newIdxHistMap := make(map[int64]*Index)
	idx2Columns := make(map[int64][]int64)
	colID2IdxIDs := make(map[int64][]int64)
	for id, idxHist := range coll.Indices {
		idxInfo := idxID2idxInfo[id]
		if idxInfo == nil {
			continue
		}
		ids := make([]int64, 0, len(idxInfo.Columns))
		for _, idxCol := range idxInfo.Columns {
			uniqueID, ok := colInfoID2UniqueID[tblInfo.Columns[idxCol.Offset].ID]
			if !ok {
				break
			}
			ids = append(ids, uniqueID)
		}
		// If the length of the id list is 0, this index won't be used in this query.
		if len(ids) == 0 {
			continue
		}
		colID2IdxIDs[ids[0]] = append(colID2IdxIDs[ids[0]], idxHist.ID)
		newIdxHistMap[idxHist.ID] = idxHist
		idx2Columns[idxHist.ID] = ids
	}
	for _, idxIDs := range colID2IdxIDs {
		slices.Sort(idxIDs)
	}
	newColl := &HistColl{
		PhysicalID:     coll.PhysicalID,
		HavePhysicalID: coll.HavePhysicalID,
		Pseudo:         coll.Pseudo,
		RealtimeCount:  coll.RealtimeCount,
		ModifyCount:    coll.ModifyCount,
		Columns:        newColHistMap,
		Indices:        newIdxHistMap,
		ColID2IdxIDs:   colID2IdxIDs,
		Idx2ColumnIDs:  idx2Columns,
	}
	return newColl
}

// isSingleColIdxNullRange checks if a range is [NULL, NULL] on a single-column index.
func isSingleColIdxNullRange(idx *Index, ran *ranger.Range) bool {
	if len(idx.Info.Columns) > 1 {
		return false
	}
	l, h := ran.LowVal[0], ran.HighVal[0]
	if l.IsNull() && h.IsNull() {
		return true
	}
	return false
}

// outOfRangeEQSelectivity estimates selectivities for out-of-range values.
// It assumes all modifications are insertions and all new-inserted rows are uniformly distributed
// and has the same distribution with analyzed rows, which means each unique value should have the
// same number of rows(Tot/NDV) of it.
// The input sctx is just for debug trace, you can pass nil safely if that's not needed.
func outOfRangeEQSelectivity(sctx sessionctx.Context, ndv, realtimeRowCount, columnRowCount int64) (result float64) {
	if sctx != nil && sctx.GetSessionVars().StmtCtx.EnableOptimizerDebugTrace {
		debugtrace.EnterContextCommon(sctx)
		defer func() {
			debugtrace.RecordAnyValuesWithNames(sctx, "Result", result)
			debugtrace.LeaveContextCommon(sctx)
		}()
	}
	increaseRowCount := realtimeRowCount - columnRowCount
	if increaseRowCount <= 0 {
		return 0 // it must be 0 since the histogram contains the whole data
	}
	if ndv < outOfRangeBetweenRate {
		ndv = outOfRangeBetweenRate // avoid inaccurate selectivity caused by small NDV
	}
	selectivity := 1 / float64(ndv)
	if selectivity*float64(columnRowCount) > float64(increaseRowCount) {
		selectivity = float64(increaseRowCount) / float64(columnRowCount)
	}
	return selectivity
}

// crossValidationSelectivity gets the selectivity of multi-column equal conditions by cross validation.
func (coll *HistColl) crossValidationSelectivity(
	sctx sessionctx.Context,
	idx *Index,
	usedColsLen int,
	idxPointRange *ranger.Range,
) (
	minRowCount float64,
	crossValidationSelectivity float64,
	err error,
) {
	if sctx.GetSessionVars().StmtCtx.EnableOptimizerDebugTrace {
		debugtrace.EnterContextCommon(sctx)
		defer func() {
			var idxName string
			if idx != nil && idx.Info != nil {
				idxName = idx.Info.Name.O
			}
			debugtrace.RecordAnyValuesWithNames(
				sctx,
				"Index Name", idxName,
				"minRowCount", minRowCount,
				"crossValidationSelectivity", crossValidationSelectivity,
				"error", err,
			)
			debugtrace.LeaveContextCommon(sctx)
		}()
	}
	minRowCount = math.MaxFloat64
	cols := coll.Idx2ColumnIDs[idx.ID]
	crossValidationSelectivity = 1.0
	totalRowCount := idx.TotalRowCount()
	for i, colID := range cols {
		if i >= usedColsLen {
			break
		}
		if col, ok := coll.Columns[colID]; ok {
			if col.IsInvalid(sctx, coll.Pseudo) {
				continue
			}
			// Since the column range is point range(LowVal is equal to HighVal), we need to set both LowExclude and HighExclude to false.
			// Otherwise we would get 0.0 estRow from GetColumnRowCount.
			rang := ranger.Range{
				LowVal:      []types.Datum{idxPointRange.LowVal[i]},
				LowExclude:  false,
				HighVal:     []types.Datum{idxPointRange.HighVal[i]},
				HighExclude: false,
				Collators:   []collate.Collator{idxPointRange.Collators[i]},
			}

			rowCount, err := col.GetColumnRowCount(sctx, []*ranger.Range{&rang}, coll.RealtimeCount, coll.ModifyCount, col.IsHandle)
			if err != nil {
				return 0, 0, err
			}
			crossValidationSelectivity = crossValidationSelectivity * (rowCount / totalRowCount)

			if rowCount < minRowCount {
				minRowCount = rowCount
			}
		}
	}
	return minRowCount, crossValidationSelectivity, nil
}

// getEqualCondSelectivity gets the selectivity of the equal conditions.
func (coll *HistColl) getEqualCondSelectivity(sctx sessionctx.Context, idx *Index, bytes []byte, usedColsLen int, idxPointRange *ranger.Range) (result float64, err error) {
	if sctx.GetSessionVars().StmtCtx.EnableOptimizerDebugTrace {
		debugtrace.EnterContextCommon(sctx)
		defer func() {
			var idxName string
			if idx != nil && idx.Info != nil {
				idxName = idx.Info.Name.O
			}
			debugtrace.RecordAnyValuesWithNames(
				sctx,
				"Index Name", idxName,
				"Encoded", bytes,
				"UsedColLen", usedColsLen,
				"Range", idxPointRange.String(),
				"Result", result,
				"error", err,
			)
			debugtrace.LeaveContextCommon(sctx)
		}()
	}
	coverAll := len(idx.Info.Columns) == usedColsLen
	// In this case, the row count is at most 1.
	if idx.Info.Unique && coverAll {
		return 1.0 / idx.TotalRowCount(), nil
	}
	val := types.NewBytesDatum(bytes)
	if idx.outOfRange(val) {
		// When the value is out of range, we could not found this value in the CM Sketch,
		// so we use heuristic methods to estimate the selectivity.
		if idx.NDV > 0 && coverAll {
			return outOfRangeEQSelectivity(sctx, idx.NDV, coll.RealtimeCount, int64(idx.TotalRowCount())), nil
		}
		// The equal condition only uses prefix columns of the index.
		colIDs := coll.Idx2ColumnIDs[idx.ID]
		var ndv int64
		for i, colID := range colIDs {
			if i >= usedColsLen {
				break
			}
			if col, ok := coll.Columns[colID]; ok {
				ndv = mathutil.Max(ndv, col.Histogram.NDV)
			}
		}
		return outOfRangeEQSelectivity(sctx, ndv, coll.RealtimeCount, int64(idx.TotalRowCount())), nil
	}

	minRowCount, crossValidationSelectivity, err := coll.crossValidationSelectivity(sctx, idx, usedColsLen, idxPointRange)
	if err != nil {
		return 0, err
	}

	idxCount := float64(idx.QueryBytes(sctx, bytes))
	if minRowCount < idxCount {
		return crossValidationSelectivity, nil
	}
	return idxCount / idx.TotalRowCount(), nil
}

// GetIndexRowCount estimates the row count by the given range.
func (coll *HistColl) GetIndexRowCount(sctx sessionctx.Context, idxID int64, indexRanges []*ranger.Range) (float64, error) {
	sc := sctx.GetSessionVars().StmtCtx
	debugTrace := sc.EnableOptimizerDebugTrace
	if debugTrace {
		debugtrace.EnterContextCommon(sctx)
		defer debugtrace.LeaveContextCommon(sctx)
	}
	idx := coll.Indices[idxID]
	totalCount := float64(0)
	for _, ran := range indexRanges {
		if debugTrace {
			debugTraceStartEstimateRange(sctx, ran, nil, nil, totalCount)
		}
		rangePosition := GetOrdinalOfRangeCond(sc, ran)
		var rangeVals []types.Datum
		// Try to enum the last range values.
		if rangePosition != len(ran.LowVal) {
			rangeVals = enumRangeValues(ran.LowVal[rangePosition], ran.HighVal[rangePosition], ran.LowExclude, ran.HighExclude)
			if rangeVals != nil {
				rangePosition++
			}
		}
		// If first one is range, just use the previous way to estimate; if it is [NULL, NULL] range
		// on single-column index, use previous way as well, because CMSketch does not contain null
		// values in this case.
		if rangePosition == 0 || isSingleColIdxNullRange(idx, ran) {
			count, err := idx.GetRowCount(sctx, nil, []*ranger.Range{ran}, coll.RealtimeCount, coll.ModifyCount)
			if err != nil {
				return 0, errors.Trace(err)
			}
			if debugTrace {
				debugTraceEndEstimateRange(sctx, count, debugTraceRange)
			}
			totalCount += count
			continue
		}
		var selectivity float64
		// use CM Sketch to estimate the equal conditions
		if rangeVals == nil {
			bytes, err := codec.EncodeKey(sc, nil, ran.LowVal[:rangePosition]...)
			if err != nil {
				return 0, errors.Trace(err)
			}
			selectivity, err = coll.getEqualCondSelectivity(sctx, idx, bytes, rangePosition, ran)
			if err != nil {
				return 0, errors.Trace(err)
			}
		} else {
			bytes, err := codec.EncodeKey(sc, nil, ran.LowVal[:rangePosition-1]...)
			if err != nil {
				return 0, errors.Trace(err)
			}
			prefixLen := len(bytes)
			for _, val := range rangeVals {
				bytes = bytes[:prefixLen]
				bytes, err = codec.EncodeKey(sc, bytes, val)
				if err != nil {
					return 0, err
				}
				res, err := coll.getEqualCondSelectivity(sctx, idx, bytes, rangePosition, ran)
				if err != nil {
					return 0, errors.Trace(err)
				}
				selectivity += res
			}
		}
		// use histogram to estimate the range condition
		if rangePosition != len(ran.LowVal) {
			rang := ranger.Range{
				LowVal:      []types.Datum{ran.LowVal[rangePosition]},
				LowExclude:  ran.LowExclude,
				HighVal:     []types.Datum{ran.HighVal[rangePosition]},
				HighExclude: ran.HighExclude,
				Collators:   []collate.Collator{ran.Collators[rangePosition]},
			}
			var count float64
			var err error
			colIDs := coll.Idx2ColumnIDs[idxID]
			var colID int64
			if rangePosition >= len(colIDs) {
				colID = -1
			} else {
				colID = colIDs[rangePosition]
			}
			// prefer index stats over column stats
			if idxIDs, ok := coll.ColID2IdxIDs[colID]; ok && len(idxIDs) > 0 {
				idxID := idxIDs[0]
				count, err = GetRowCountByIndexRanges(sctx, coll, idxID, []*ranger.Range{&rang})
			} else {
				count, err = GetRowCountByColumnRanges(sctx, coll, colID, []*ranger.Range{&rang})
			}
			if err != nil {
				return 0, errors.Trace(err)
			}
			selectivity = selectivity * count / idx.TotalRowCount()
		}
		count := selectivity * idx.TotalRowCount()
		if debugTrace {
			debugTraceEndEstimateRange(sctx, count, debugTraceRange)
		}
		totalCount += count
	}
	if totalCount > idx.TotalRowCount() {
		totalCount = idx.TotalRowCount()
	}
	return totalCount, nil
}

// PseudoTable creates a pseudo table statistics.
func PseudoTable(tblInfo *model.TableInfo) *Table {
	const fakePhysicalID int64 = -1

	pseudoHistColl := HistColl{
		RealtimeCount:  PseudoRowCount,
		PhysicalID:     tblInfo.ID,
		HavePhysicalID: true,
		Columns:        make(map[int64]*Column, len(tblInfo.Columns)),
		Indices:        make(map[int64]*Index, len(tblInfo.Indices)),
		Pseudo:         true,
	}
	t := &Table{
		HistColl: pseudoHistColl,
	}
	for _, col := range tblInfo.Columns {
		// The column is public to use. Also we should check the column is not hidden since hidden means that it's used by expression index.
		// We would not collect stats for the hidden column and we won't use the hidden column to estimate.
		// Thus we don't create pseudo stats for it.
		if col.State == model.StatePublic && !col.Hidden {
			t.Columns[col.ID] = &Column{
				PhysicalID: fakePhysicalID,
				Info:       col,
				IsHandle:   tblInfo.PKIsHandle && mysql.HasPriKeyFlag(col.GetFlag()),
				Histogram:  *NewHistogram(col.ID, 0, 0, 0, &col.FieldType, 0, 0),
			}
		}
	}
	for _, idx := range tblInfo.Indices {
		if idx.State == model.StatePublic {
			t.Indices[idx.ID] = &Index{
				PhysicalID: fakePhysicalID,
				Info:       idx,
				Histogram:  *NewHistogram(idx.ID, 0, 0, 0, types.NewFieldType(mysql.TypeBlob), 0, 0)}
		}
	}
	return t
}

// GetAvgRowSize computes average row size for given columns.
func (coll *HistColl) GetAvgRowSize(ctx sessionctx.Context, cols []*expression.Column, isEncodedKey bool, isForScan bool) (size float64) {
	sessionVars := ctx.GetSessionVars()
	if coll.Pseudo || len(coll.Columns) == 0 || coll.RealtimeCount == 0 {
		size = pseudoColSize * float64(len(cols))
	} else {
		for _, col := range cols {
			colHist, ok := coll.Columns[col.UniqueID]
			// Normally this would not happen, it is for compatibility with old version stats which
			// does not include TotColSize.
			if !ok || (!colHist.IsHandle && colHist.TotColSize == 0 && (colHist.NullCount != coll.RealtimeCount)) {
				size += pseudoColSize
				continue
			}
			// We differentiate if the column is encoded as key or value, because the resulted size
			// is different.
			if sessionVars.EnableChunkRPC && !isForScan {
				size += colHist.AvgColSizeChunkFormat(coll.RealtimeCount)
			} else {
				size += colHist.AvgColSize(coll.RealtimeCount, isEncodedKey)
			}
		}
	}
	if sessionVars.EnableChunkRPC && !isForScan {
		// Add 1/8 byte for each column's nullBitMap byte.
		return size + float64(len(cols))/8
	}
	// Add 1 byte for each column's flag byte. See `encode` for details.
	return size + float64(len(cols))
}

// GetAvgRowSizeListInDisk computes average row size for given columns.
func (coll *HistColl) GetAvgRowSizeListInDisk(cols []*expression.Column) (size float64) {
	if coll.Pseudo || len(coll.Columns) == 0 || coll.RealtimeCount == 0 {
		for _, col := range cols {
			size += float64(chunk.EstimateTypeWidth(col.GetType()))
		}
	} else {
		for _, col := range cols {
			colHist, ok := coll.Columns[col.UniqueID]
			// Normally this would not happen, it is for compatibility with old version stats which
			// does not include TotColSize.
			if !ok || (!colHist.IsHandle && colHist.TotColSize == 0 && (colHist.NullCount != coll.RealtimeCount)) {
				size += float64(chunk.EstimateTypeWidth(col.GetType()))
				continue
			}
			size += colHist.AvgColSizeListInDisk(coll.RealtimeCount)
		}
	}
	// Add 8 byte for each column's size record. See `ListInDisk` for details.
	return size + float64(8*len(cols))
}

// GetTableAvgRowSize computes average row size for a table scan, exclude the index key-value pairs.
func (coll *HistColl) GetTableAvgRowSize(ctx sessionctx.Context, cols []*expression.Column, storeType kv.StoreType, handleInCols bool) (size float64) {
	size = coll.GetAvgRowSize(ctx, cols, false, true)
	switch storeType {
	case kv.TiKV:
		size += tablecodec.RecordRowKeyLen
		// The `cols` for TiKV always contain the row_id, so prefix row size subtract its length.
		size -= 8
	case kv.TiFlash:
		if !handleInCols {
			size += 8 /* row_id length */
		}
	}
	return
}

// GetIndexAvgRowSize computes average row size for a index scan.
func (coll *HistColl) GetIndexAvgRowSize(ctx sessionctx.Context, cols []*expression.Column, isUnique bool) (size float64) {
	size = coll.GetAvgRowSize(ctx, cols, true, true)
	// tablePrefix(1) + tableID(8) + indexPrefix(2) + indexID(8)
	// Because the cols for index scan always contain the handle, so we don't add the rowID here.
	size += 19
	if !isUnique {
		// add the len("_")
		size++
	}
	return
}

// CheckAnalyzeVerOnTable checks whether the given version is the one from the tbl.
// If not, it will return false and set the version to the tbl's.
// We use this check to make sure all the statistics of the table are in the same version.
func CheckAnalyzeVerOnTable(tbl *Table, version *int) bool {
	for _, col := range tbl.Columns {
		if !col.IsAnalyzed() {
			continue
		}
		if col.StatsVer != int64(*version) {
			*version = int(col.StatsVer)
			return false
		}
		// If we found one column and the version is the same, we can directly return since all the versions from this table is the same.
		return true
	}
	for _, idx := range tbl.Indices {
		if !idx.IsAnalyzed() {
			continue
		}
		if idx.StatsVer != int64(*version) {
			*version = int(idx.StatsVer)
			return false
		}
		// If we found one column and the version is the same, we can directly return since all the versions from this table is the same.
		return true
	}
	// This table has no statistics yet. We can directly return true.
	return true
}
