// Copyright 2017 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package rowcontainer

import (
	"bytes"
	"context"

	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/diskmap"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catenumpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/colinfo"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/bufalloc"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
	"github.com/cockroachdb/cockroach/pkg/util/errorutil/unimplemented"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/mon"
	"github.com/cockroachdb/errors"
)

// DiskRowContainer is a SortableRowContainer that stores rows on disk according
// to the ordering specified in DiskRowContainer.ordering. The underlying store
// is a SortedDiskMap so the sorting itself is delegated. Use an iterator
// created through NewIterator() to read the rows in sorted order.
type DiskRowContainer struct {
	diskMap diskmap.SortedDiskMap
	// diskAcc keeps track of disk usage.
	diskAcc mon.BoundAccount
	// bufferedRows buffers writes to the diskMap.
	bufferedRows diskmap.SortedDiskMapBatchWriter
	// memAcc keeps track of memory usage of scratchKey, scratchVal, and keys
	// stored in deDupCache if applicable.
	memAcc     mon.BoundAccount
	scratchKey []byte
	scratchVal []byte

	// For computing mean encoded row bytes.
	totalEncodedRowBytes uint64

	// lastReadKey is used to implement NewFinalIterator. Refer to the method's
	// comment for more information.
	lastReadKey []byte

	// topK is set by callers through InitTopK. Since rows are kept in sorted
	// order, topK will simply limit iterators to read the first k rows.
	topK int

	// rowID is used as a key suffix to prevent duplicate rows from overwriting
	// each other.
	rowID uint64

	// types is the schema of rows in the container.
	types []*types.T
	// ordering is the order in which rows should be sorted.
	ordering colinfo.ColumnOrdering
	// encodings keeps around the DatumEncoding equivalents of the encoding
	// directions in ordering to avoid conversions in hot paths.
	encodings []catenumpb.DatumEncoding
	// valueIdxs holds the indexes of the columns that we encode as values. The
	// columns described by ordering will be encoded as keys. See
	// MakeDiskRowContainer() for more encoding specifics.
	valueIdxs []int

	// See comment in DoDeDuplicate().
	deDuplicate bool
	// A mapping from a key to the dense row index assigned to the key. It
	// contains all the key strings that are potentially buffered in bufferedRows.
	// Since we need to de-duplicate for every insert attempt, we don't want to
	// keep flushing bufferedRows after every insert.
	deDupCache map[string]int

	deDupCacheAccountedFor int64

	diskMonitor *mon.BytesMonitor
	engine      diskmap.Factory

	datumAlloc *tree.DatumAlloc
}

var _ SortableRowContainer = &DiskRowContainer{}
var _ DeDupingRowContainer = &DiskRowContainer{}

// MakeDiskRowContainer creates a DiskRowContainer with the given engine as the
// underlying store that rows are stored on.
// Arguments:
//   - memAcc is used to monitor this DiskRowContainer's memory usage. It must
//     be bound to an unlimited memory monitor. The container takes ownership of
//     the account.
//   - diskMonitor is used to monitor this DiskRowContainer's disk usage.
//   - types is the schema of rows that will be added to this container.
//   - ordering is the output ordering; the order in which rows should be sorted.
//   - e is the underlying store that rows are stored on.
func MakeDiskRowContainer(
	ctx context.Context,
	memAcc mon.BoundAccount,
	diskMonitor *mon.BytesMonitor,
	typs []*types.T,
	ordering colinfo.ColumnOrdering,
	e diskmap.Factory,
) (_ DiskRowContainer, retErr error) {
	diskMap := e.NewSortedDiskMap()
	d := DiskRowContainer{
		diskMap:      diskMap,
		diskAcc:      diskMonitor.MakeBoundAccount(),
		bufferedRows: diskMap.NewBatchWriter(),
		memAcc:       memAcc,
		types:        typs,
		ordering:     ordering,
		diskMonitor:  diskMonitor,
		engine:       e,
		datumAlloc:   &tree.DatumAlloc{},
	}
	defer func() {
		if retErr != nil {
			// Ensure to close the container since we're not returning it to the
			// caller.
			d.Close(ctx)
		}
	}()

	// The ordering is specified for a subset of the columns. These will be
	// encoded as a key in the given order according to the given direction so
	// that the sorting can be delegated to the underlying SortedDiskMap. To
	// avoid converting encoding.Direction to catenumpb.DatumEncoding we do this
	// once at initialization and store the conversions in d.encodings.
	// We encode the other columns as values. The indexes of these columns are
	// kept around in d.valueIdxs to have them ready in hot paths.
	// For composite columns that are specified in d.ordering, the Datum is
	// encoded both in the key for comparison and in the value for decoding.
	orderingIdxs := make(map[int]struct{})
	for _, orderInfo := range d.ordering {
		orderingIdxs[orderInfo.ColIdx] = struct{}{}
	}
	d.valueIdxs = make([]int, 0, len(d.types))
	for i := range d.types {
		// TODO(asubiotto): A datum of a type for which CanHaveCompositeKeyEncoding
		// returns true may not necessarily need to be encoded in the value, so
		// make this more fine-grained. See IsComposite() methods in
		// pkg/sql/parser/datum.go.
		if _, ok := orderingIdxs[i]; !ok || colinfo.CanHaveCompositeKeyEncoding(d.types[i]) {
			d.valueIdxs = append(d.valueIdxs, i)
		}
	}

	d.encodings = make([]catenumpb.DatumEncoding, len(d.ordering))
	for i, orderInfo := range ordering {
		d.encodings[i] = rowenc.EncodingDirToDatumEncoding(orderInfo.Direction)
		switch t := typs[orderInfo.ColIdx]; t.Family() {
		case types.TSQueryFamily, types.TSVectorFamily, types.PGVectorFamily:
			return DiskRowContainer{}, unimplemented.NewWithIssueDetailf(
				92165, "", "can't order by column type %s", t.SQLStringForError(),
			)
		case types.TupleFamily:
			return DiskRowContainer{}, unimplemented.NewWithIssueDetailf(
				49975, "", "can't spill column type %s to disk", t.SQLStringForError(),
			)
		}
	}

	return d, nil
}

// DoDeDuplicate causes DiskRowContainer to behave as an implementation of
// DeDupingRowContainer. It should not be mixed with calls to AddRow() (except
// when the AddRow() already represent deduplicated rows). It de-duplicates
// the keys such that only the first row with the given key will be stored.
// The index returned in AddRowWithDedup() is a dense index starting from 0,
// representing when that key was first added. This feature does not combine
// with Sort(), Reorder() etc., and only to be used for assignment of these
// dense indexes. The main reason to add this to DiskBackedRowContainer is to
// avoid significant code duplication in constructing another row container.
func (d *DiskRowContainer) DoDeDuplicate() {
	d.deDuplicate = true
	d.deDupCache = make(map[string]int)
}

// Len is part of the SortableRowContainer interface.
func (d *DiskRowContainer) Len() int {
	return int(d.rowID)
}

// Writing extremely large keys to pebble can lead to undefined behavior
// (overflows and / or OOMs), so we'll prohibit keys larger than 1.5 GiB.
const maxPebbleKeySize = 1536 << 20 /* 1.5 GiB */

var maxPebbleKeySizeExceededErr = pgerror.Newf(pgcode.OutOfMemory, "temporary storage doesn't support keys larger than 1.5 GiB")

// resetScratch prepares the scratch space for reuse. If the slice is too large
// to keep, it's lost and the memory account is updated accordingly.
func (d *DiskRowContainer) resetScratch(ctx context.Context) {
	// Do not keep very large scratch space across rows (we're trying to
	// minimize RAM usage after all since we've spilled to disk).
	const maxKeptSize = 1 << 20 /* 1 MiB */
	if cap(d.scratchKey) > maxKeptSize {
		d.memAcc.Shrink(ctx, int64(cap(d.scratchKey)))
		d.scratchKey = nil
	}
	if cap(d.scratchVal) > maxKeptSize {
		d.memAcc.Shrink(ctx, int64(cap(d.scratchVal)))
		d.scratchVal = nil
	}
	d.scratchKey = d.scratchKey[:0]
	d.scratchVal = d.scratchVal[:0]
}

// AddRow is part of the SortableRowContainer interface.
//
// It is additionally used in de-duping mode by DiskBackedRowContainer when
// switching from a memory container to this disk container, since it is
// adding rows that are already de-duped. Once it has added all the already
// de-duped rows, it should switch to using AddRowWithDeDup() and never call
// AddRow() again.
//
// Note: if key calculation changes, computeKey() of hashMemRowIterator should
// be changed accordingly.
func (d *DiskRowContainer) AddRow(ctx context.Context, row rowenc.EncDatumRow) error {
	if err := d.encodeRow(ctx, row); err != nil {
		return err
	}
	defer d.resetScratch(ctx)
	if len(d.scratchKey) > maxPebbleKeySize {
		return maxPebbleKeySizeExceededErr
	}
	if err := d.diskAcc.Grow(ctx, int64(len(d.scratchKey)+len(d.scratchVal))); err != nil {
		// TODO(yuzefovich): this error wrapping is redundant - err should be
		// produced by the disk monitor.
		return pgerror.Wrapf(err, pgcode.OutOfMemory,
			"this query requires additional disk space")
	}
	if err := d.bufferedRows.Put(d.scratchKey, d.scratchVal); err != nil {
		return err
	}
	// See comment above on when this is used for already de-duplicated
	// rows -- we need to track these in the de-dup cache so that later
	// calls to AddRowWithDeDup() de-duplicate wrt this cache.
	if d.deDuplicate {
		if d.bufferedRows.NumPutsSinceFlush() == 0 {
			d.clearDeDupCache(ctx)
		} else {
			if err := d.memAcc.Grow(ctx, int64(len(d.scratchKey))); err != nil {
				return err
			}
			d.deDupCacheAccountedFor += int64(len(d.scratchKey))
			d.deDupCache[string(d.scratchKey)] = int(d.rowID)
		}
	}
	d.totalEncodedRowBytes += uint64(len(d.scratchKey) + len(d.scratchVal))
	d.rowID++
	return nil
}

// AddRowWithDeDup is part of the DeDupingRowContainer interface.
func (d *DiskRowContainer) AddRowWithDeDup(
	ctx context.Context, row rowenc.EncDatumRow,
) (int, error) {
	if err := d.encodeRow(ctx, row); err != nil {
		return 0, err
	}
	defer d.resetScratch(ctx)
	// First use the cache to de-dup.
	entry, ok := d.deDupCache[string(d.scratchKey)]
	if ok {
		return entry, nil
	}
	// Since not in cache, we need to use an iterator to de-dup.
	// TODO(sumeer): this read is expensive:
	// - if there is a significant  fraction of duplicates, we can do better
	//   with a larger cache
	// - if duplicates are rare, use a bloom filter for all the keys in the
	//   diskMap, since a miss in the bloom filter allows us to write to the
	//   diskMap without reading.
	iter := d.diskMap.NewIterator()
	defer iter.Close()
	iter.SeekGE(d.scratchKey)
	valid, err := iter.Valid()
	if err != nil {
		return 0, err
	}
	if valid && bytes.Equal(iter.UnsafeKey(), d.scratchKey) {
		// Found the key. Note that as documented in DeDupingRowContainer,
		// this feature is limited to the case where the whole row is
		// encoded into the key. The value only contains the dense RowID
		// assigned to the key.
		_, idx, err := encoding.DecodeUvarintAscending(iter.UnsafeValue())
		if err != nil {
			return 0, err
		}
		return int(idx), nil
	}
	if len(d.scratchKey) > maxPebbleKeySize {
		return 0, maxPebbleKeySizeExceededErr
	}
	if err := d.diskAcc.Grow(ctx, int64(len(d.scratchKey)+len(d.scratchVal))); err != nil {
		// TODO(yuzefovich): this error wrapping is redundant - err should be
		// produced by the disk monitor.
		return 0, pgerror.Wrapf(err, pgcode.OutOfMemory,
			"this query requires additional disk space")
	}
	if err := d.bufferedRows.Put(d.scratchKey, d.scratchVal); err != nil {
		return 0, err
	}
	if d.bufferedRows.NumPutsSinceFlush() == 0 {
		d.clearDeDupCache(ctx)
	} else {
		if err = d.memAcc.Grow(ctx, int64(len(d.scratchKey))); err != nil {
			return 0, err
		}
		d.deDupCacheAccountedFor += int64(len(d.scratchKey))
		d.deDupCache[string(d.scratchKey)] = int(d.rowID)
	}
	d.totalEncodedRowBytes += uint64(len(d.scratchKey) + len(d.scratchVal))
	idx := int(d.rowID)
	d.rowID++
	return idx, nil
}

func (d *DiskRowContainer) clearDeDupCache(ctx context.Context) {
	for k := range d.deDupCache {
		delete(d.deDupCache, k)
	}
	d.memAcc.Shrink(ctx, d.deDupCacheAccountedFor)
	d.deDupCacheAccountedFor = 0
}

func (d *DiskRowContainer) testingFlushBuffer(ctx context.Context) {
	if err := d.bufferedRows.Flush(); err != nil {
		log.Fatalf(ctx, "%v", err)
	}
	d.clearDeDupCache(ctx)
}

// encodeRow encodes the given row into scratchKey and scratchVal fields. The
// memory account is updated according to the new capacity of these slices.
func (d *DiskRowContainer) encodeRow(ctx context.Context, row rowenc.EncDatumRow) (retErr error) {
	if len(row) != len(d.types) {
		log.Fatalf(ctx, "invalid row length %d, expected %d", len(row), len(d.types))
	}
	oldScratchMemUse := int64(cap(d.scratchKey) + cap(d.scratchVal))
	defer func() {
		if retErr == nil {
			newScratchMemUse := int64(cap(d.scratchKey) + cap(d.scratchVal))
			retErr = d.memAcc.Resize(ctx, oldScratchMemUse, newScratchMemUse)
		}
	}()

	for i, orderInfo := range d.ordering {
		col := orderInfo.ColIdx
		var err error
		d.scratchKey, err = row[col].Encode(d.types[col], d.datumAlloc, d.encodings[i], d.scratchKey)
		if err != nil {
			return err
		}
	}
	if !d.deDuplicate {
		for _, i := range d.valueIdxs {
			var err error
			d.scratchVal, err = row[i].Encode(d.types[i], d.datumAlloc, catenumpb.DatumEncoding_VALUE, d.scratchVal)
			if err != nil {
				return err
			}
		}
		// Put a unique row to keep track of duplicates. Note that this will not
		// mess with key decoding.
		d.scratchKey = encoding.EncodeUvarintAscending(d.scratchKey, d.rowID)
	} else {
		// Add the row id to the value. Note that in this de-duping case the
		// row id is the only thing in the value since the whole row is encoded
		// into the key. Note that the key could have types for which
		// CanHaveCompositeKeyEncoding() returns true and we do not encode them
		// into the value (only in the key) for this DeDupingRowContainer. This
		// is ok since:
		// - The DeDupingRowContainer never needs to return the original row
		//   (there is no get method).
		// - The columns encoded into the key are the primary key columns
		//   of the original table, so the key encoding represents a unique
		//   row in the original table (the key encoding here is not only
		//   a determinant of sort ordering).
		d.scratchVal = encoding.EncodeUvarintAscending(d.scratchVal, d.rowID)
	}
	return nil
}

// Sort is a noop because the use of a SortedDiskMap as the underlying store
// keeps the rows in sorted order.
func (d *DiskRowContainer) Sort(context.Context) {}

// Reorder implements ReorderableRowContainer. It creates a new
// DiskRowContainer with the requested ordering and adds a row one by one from
// the current DiskRowContainer, the latter is closed at the end.
func (d *DiskRowContainer) Reorder(ctx context.Context, ordering colinfo.ColumnOrdering) error {
	// We need to create a new DiskRowContainer since its ordering can only be
	// changed at initialization.
	memAcc := d.memAcc.Monitor().MakeBoundAccount()
	newContainer, err := MakeDiskRowContainer(ctx, memAcc, d.diskMonitor, d.types, ordering, d.engine)
	if err != nil {
		return err
	}
	i := d.NewFinalIterator(ctx)
	defer i.Close()
	for i.Rewind(); ; i.Next() {
		if ok, err := i.Valid(); err != nil {
			return err
		} else if !ok {
			break
		}
		row, err := i.EncRow()
		if err != nil {
			return err
		}
		if err := newContainer.AddRow(ctx, row); err != nil {
			return err
		}
	}
	d.Close(ctx)
	*d = newContainer
	return nil
}

// InitTopK limits iterators to read the first k rows.
func (d *DiskRowContainer) InitTopK(context.Context) {
	d.topK = d.Len()
}

// MaybeReplaceMax adds row to the DiskRowContainer. The SortedDiskMap will
// sort this row into the top k if applicable.
func (d *DiskRowContainer) MaybeReplaceMax(ctx context.Context, row rowenc.EncDatumRow) error {
	return d.AddRow(ctx, row)
}

// MeanEncodedRowBytes returns the mean bytes consumed by an encoded row stored in
// this container.
func (d *DiskRowContainer) MeanEncodedRowBytes() int {
	if d.rowID == 0 {
		return 0
	}
	return int(d.totalEncodedRowBytes / d.rowID)
}

// UnsafeReset is part of the SortableRowContainer interface.
func (d *DiskRowContainer) UnsafeReset(ctx context.Context) error {
	_ = d.bufferedRows.Close(ctx)
	if err := d.diskMap.Clear(); err != nil {
		return err
	}
	d.diskAcc.Clear(ctx)
	d.bufferedRows = d.diskMap.NewBatchWriter()
	d.scratchKey = nil
	d.scratchVal = nil
	d.clearDeDupCache(ctx)
	d.memAcc.Clear(ctx)
	d.lastReadKey = nil
	d.rowID = 0
	d.totalEncodedRowBytes = 0
	return nil
}

// Close is part of the SortableRowContainer interface.
func (d *DiskRowContainer) Close(ctx context.Context) {
	if d.diskMap != nil {
		// diskMap and bufferedRows could be nil in some error paths.

		// We can ignore the error here because the flushed data is immediately
		// cleared in the following Close.
		_ = d.bufferedRows.Close(ctx)
		d.diskMap.Close(ctx)
	}
	d.diskAcc.Close(ctx) // diskAcc is never nil
	d.memAcc.Close(ctx)  // memAcc is never nil
}

// diskRowIterator iterates over the rows in a DiskRowContainer.
type diskRowIterator struct {
	rowContainer  *DiskRowContainer
	rowBuf        bufalloc.ByteAllocator
	scratchEncRow rowenc.EncDatumRow
	scratchRow    tree.Datums
	da            tree.DatumAlloc
	diskmap.SortedDiskMapIterator
}

var _ RowIterator = &diskRowIterator{}

func (d *DiskRowContainer) newIterator(ctx context.Context) diskRowIterator {
	if err := d.bufferedRows.Flush(); err != nil {
		log.Fatalf(ctx, "%v", err)
	}
	return diskRowIterator{
		rowContainer:          d,
		scratchEncRow:         make(rowenc.EncDatumRow, len(d.types)),
		scratchRow:            make(tree.Datums, len(d.types)),
		SortedDiskMapIterator: d.diskMap.NewIterator(),
	}
}

// NewIterator is part of the SortableRowContainer interface.
func (d *DiskRowContainer) NewIterator(ctx context.Context) RowIterator {
	i := d.newIterator(ctx)
	if d.topK > 0 {
		return &diskRowTopKIterator{RowIterator: &i, k: d.topK}
	}
	return &i
}

// EncRow returns the current row. The returned rowenc.EncDatumRow is only valid
// until the next call to EncRow().
func (r *diskRowIterator) EncRow() (rowenc.EncDatumRow, error) {
	if ok, err := r.Valid(); err != nil {
		return nil, errors.NewAssertionErrorWithWrappedErrf(err, "unable to check row validity")
	} else if !ok {
		return nil, errors.AssertionFailedf("invalid row")
	}

	k := r.UnsafeKey()
	v := r.UnsafeValue()
	// We will use the encoded key and value bytes as is by shoving them
	// directly into the EncDatum, so we need to make a copy here. We cannot
	// reuse the same byte slice across EncRow() calls because it would lead to
	// modification of the EncDatums (which is not allowed).
	var buf []byte
	r.rowBuf, buf = r.rowBuf.Alloc(len(k) + len(v))
	copy(buf, k)
	k, buf = buf[:len(k):len(k)], buf[len(k):]
	copy(buf, v)
	v = buf

	for i, orderInfo := range r.rowContainer.ordering {
		// Types with composite key encodings are decoded from the value.
		if colinfo.CanHaveCompositeKeyEncoding(r.rowContainer.types[orderInfo.ColIdx]) {
			// Skip over the encoded key.
			encLen, err := encoding.PeekLength(k)
			if err != nil {
				return nil, err
			}
			k = k[encLen:]
			continue
		}
		var err error
		col := orderInfo.ColIdx
		r.scratchEncRow[col], k, err = rowenc.EncDatumFromBuffer(r.rowContainer.encodings[i], k)
		if err != nil {
			return nil, errors.NewAssertionErrorWithWrappedErrf(err,
				"unable to decode row, column idx %d", errors.Safe(col))
		}
	}
	for _, i := range r.rowContainer.valueIdxs {
		var err error
		r.scratchEncRow[i], v, err = rowenc.EncDatumFromBuffer(catenumpb.DatumEncoding_VALUE, v)
		if err != nil {
			return nil, errors.NewAssertionErrorWithWrappedErrf(err,
				"unable to decode row, value idx %d", errors.Safe(i))
		}
	}
	return r.scratchEncRow, nil
}

// Row returns the current row. The returned row is only valid until the next
// call to Row().
func (r *diskRowIterator) Row() (tree.Datums, error) {
	encRow, err := r.EncRow()
	if err != nil {
		return nil, err
	}
	err = rowenc.EncDatumRowToDatums(r.rowContainer.types, r.scratchRow, encRow, &r.da)
	return r.scratchRow, err
}

func (r *diskRowIterator) Close() {
	if r.SortedDiskMapIterator != nil {
		r.SortedDiskMapIterator.Close()
	}
}

// numberedRowIterator is a specialization of diskRowIterator that is
// only for the case where the key is the rowID assigned in AddRow().
type numberedRowIterator struct {
	*diskRowIterator
	scratchKey []byte
}

func (d *DiskRowContainer) newNumberedIterator(ctx context.Context) *numberedRowIterator {
	i := d.newIterator(ctx)
	return &numberedRowIterator{diskRowIterator: &i}
}

func (n numberedRowIterator) seekToIndex(idx int) {
	n.scratchKey = encoding.EncodeUvarintAscending(n.scratchKey, uint64(idx))
	n.SeekGE(n.scratchKey)
}

type diskRowFinalIterator struct {
	diskRowIterator
}

var _ RowIterator = &diskRowFinalIterator{}

// NewFinalIterator returns an iterator that reads rows exactly once throughout
// the lifetime of a DiskRowContainer. Rows are not actually discarded from the
// DiskRowContainer, but the lastReadKey is kept track of in order to serve as
// the start key for future diskRowFinalIterators.
// NOTE: Don't use NewFinalIterator if you passed in an ordering for the rows
// and will be adding rows between iterations. New rows could sort before the
// current row.
func (d *DiskRowContainer) NewFinalIterator(ctx context.Context) RowIterator {
	i := diskRowFinalIterator{diskRowIterator: d.newIterator(ctx)}
	if d.topK > 0 {
		return &diskRowTopKIterator{RowIterator: &i, k: d.topK}
	}
	return &i
}

func (r *diskRowFinalIterator) Rewind() {
	r.SeekGE(r.diskRowIterator.rowContainer.lastReadKey)
	if r.diskRowIterator.rowContainer.lastReadKey != nil {
		r.Next()
	}
}

func (r *diskRowFinalIterator) EncRow() (rowenc.EncDatumRow, error) {
	row, err := r.diskRowIterator.EncRow()
	if err != nil {
		return nil, err
	}
	r.diskRowIterator.rowContainer.lastReadKey =
		append(r.diskRowIterator.rowContainer.lastReadKey[:0], r.UnsafeKey()...)
	return row, nil
}

func (r *diskRowFinalIterator) Row() (tree.Datums, error) {
	row, err := r.diskRowIterator.Row()
	if err != nil {
		return nil, err
	}
	r.diskRowIterator.rowContainer.lastReadKey =
		append(r.diskRowIterator.rowContainer.lastReadKey[:0], r.UnsafeKey()...)
	return row, nil
}

type diskRowTopKIterator struct {
	RowIterator
	position int
	// k is the limit of rows to read.
	k int
}

var _ RowIterator = &diskRowTopKIterator{}

func (d *diskRowTopKIterator) Rewind() {
	d.RowIterator.Rewind()
	d.position = 0
}

func (d *diskRowTopKIterator) Valid() (bool, error) {
	if d.position >= d.k {
		return false, nil
	}
	return d.RowIterator.Valid()
}

func (d *diskRowTopKIterator) Next() {
	d.position++
	d.RowIterator.Next()
}
