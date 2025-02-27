// Copyright 2019 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package metamorphic

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/keyspan"
	"github.com/cockroachdb/pebble/internal/private"
	"github.com/cockroachdb/pebble/internal/rangekey"
	"github.com/cockroachdb/pebble/internal/testkeys"
	"github.com/cockroachdb/pebble/objstorage/objstorageprovider"
	"github.com/cockroachdb/pebble/sstable"
	"github.com/cockroachdb/pebble/vfs/errorfs"
)

// Ops holds a sequence of operations to be executed by the metamorphic tests.
type Ops []op

// op defines the interface for a single operation, such as creating a batch,
// or advancing an iterator.
type op interface {
	String() string

	run(t *Test, h historyRecorder)

	// receiver returns the object ID of the object the operation is performed
	// on. Every operation has a receiver (eg, batch0.Set(...) has `batch0` as
	// its receiver). Receivers are used for synchronization when running with
	// concurrency.
	receiver() objID

	// syncObjs returns an additional set of object IDs—excluding the
	// receiver—that the operation must synchronize with. At execution time,
	// the operation will run serially with respect to all other operations
	// that return these objects from their own syncObjs or receiver methods.
	syncObjs() objIDSlice

	// keys returns all user keys used by the operation, as pointers to slices.
	// The caller can then modify these slices to rewrite the keys.
	//
	// Used for simplification of operations for easier investigations.
	keys() []*[]byte

	// diagramKeyRanges() returns key spans associated with this operation, to be
	// shown on an ASCII diagram of operations.
	diagramKeyRanges() []pebble.KeyRange
}

// initOp performs test initialization
type initOp struct {
	dbSlots       uint32
	batchSlots    uint32
	iterSlots     uint32
	snapshotSlots uint32
}

func (o *initOp) run(t *Test, h historyRecorder) {
	t.batches = make([]*pebble.Batch, o.batchSlots)
	t.iters = make([]*retryableIter, o.iterSlots)
	t.snapshots = make([]readerCloser, o.snapshotSlots)
	h.Recordf("%s", o)
}

func (o *initOp) String() string {
	return fmt.Sprintf("Init(%d /* dbs */, %d /* batches */, %d /* iters */, %d /* snapshots */)",
		o.dbSlots, o.batchSlots, o.iterSlots, o.snapshotSlots)
}

func (o *initOp) receiver() objID { return makeObjID(dbTag, 1) }
func (o *initOp) syncObjs() objIDSlice {
	syncObjs := make([]objID, 0)
	// Add any additional DBs to syncObjs.
	for i := uint32(2); i < o.dbSlots+1; i++ {
		syncObjs = append(syncObjs, makeObjID(dbTag, i))
	}
	return syncObjs
}

func (o *initOp) keys() []*[]byte                     { return nil }
func (o *initOp) diagramKeyRanges() []pebble.KeyRange { return nil }

// applyOp models a Writer.Apply operation.
type applyOp struct {
	writerID objID
	batchID  objID
}

func (o *applyOp) run(t *Test, h historyRecorder) {
	b := t.getBatch(o.batchID)
	w := t.getWriter(o.writerID)
	var err error
	if o.writerID.tag() == dbTag && t.testOpts.asyncApplyToDB && t.writeOpts.Sync {
		err = w.(*pebble.DB).ApplyNoSyncWait(b, t.writeOpts)
		if err == nil {
			err = b.SyncWait()
		}
	} else {
		err = w.Apply(b, t.writeOpts)
	}
	h.Recordf("%s // %v", o, err)
	// batch will be closed by a closeOp which is guaranteed to be generated
}

func (o *applyOp) String() string  { return fmt.Sprintf("%s.Apply(%s)", o.writerID, o.batchID) }
func (o *applyOp) receiver() objID { return o.writerID }
func (o *applyOp) syncObjs() objIDSlice {
	// Apply should not be concurrent with operations that are mutating the
	// batch.
	return []objID{o.batchID}
}

func (o *applyOp) keys() []*[]byte                     { return nil }
func (o *applyOp) diagramKeyRanges() []pebble.KeyRange { return nil }

// checkpointOp models a DB.Checkpoint operation.
type checkpointOp struct {
	dbID objID
	// If non-empty, the checkpoint is restricted to these spans.
	spans []pebble.CheckpointSpan
}

func (o *checkpointOp) run(t *Test, h historyRecorder) {
	// TODO(josh): db.Checkpoint does not work with shared storage yet.
	// It would be better to filter out ahead of calling run on the op,
	// by setting the weight that generator.go uses to zero, or similar.
	// But IIUC the ops are shared for ALL the metamorphic test runs, so
	// not sure how to do that easily:
	// https://github.com/cockroachdb/pebble/blob/master/metamorphic/meta.go#L177
	if t.testOpts.sharedStorageEnabled {
		h.Recordf("%s // %v", o, nil)
		return
	}
	var opts []pebble.CheckpointOption
	if len(o.spans) > 0 {
		opts = append(opts, pebble.WithRestrictToSpans(o.spans))
	}
	db := t.getDB(o.dbID)
	err := t.withRetries(func() error {
		return db.Checkpoint(o.dir(t.dir, h.op), opts...)
	})
	h.Recordf("%s // %v", o, err)
}

func (o *checkpointOp) dir(dataDir string, idx int) string {
	return filepath.Join(dataDir, "checkpoints", fmt.Sprintf("op-%06d", idx))
}

func (o *checkpointOp) String() string {
	var spanStr bytes.Buffer
	for i, span := range o.spans {
		if i > 0 {
			spanStr.WriteString(",")
		}
		fmt.Fprintf(&spanStr, "%q,%q", span.Start, span.End)
	}
	return fmt.Sprintf("%s.Checkpoint(%s)", o.dbID, spanStr.String())
}

func (o *checkpointOp) receiver() objID      { return o.dbID }
func (o *checkpointOp) syncObjs() objIDSlice { return nil }

func (o *checkpointOp) keys() []*[]byte {
	var res []*[]byte
	for i := range o.spans {
		res = append(res, &o.spans[i].Start, &o.spans[i].End)
	}
	return res
}

func (o *checkpointOp) diagramKeyRanges() []pebble.KeyRange {
	var res []pebble.KeyRange
	for i := range o.spans {
		res = append(res, pebble.KeyRange{
			Start: o.spans[i].Start,
			End:   o.spans[i].End,
		})
	}
	return res
}

// closeOp models a {Batch,Iterator,Snapshot}.Close operation.
type closeOp struct {
	objID objID

	// affectedObjects is the list of additional objects that are affected by this
	// operation, and which syncObjs() must return so that we don't perform the
	// close in parallel with other operations to affected objects.
	affectedObjects []objID
}

func (o *closeOp) run(t *Test, h historyRecorder) {
	c := t.getCloser(o.objID)
	if o.objID.tag() == dbTag && t.opts.DisableWAL {
		// Special case: If WAL is disabled, do a flush right before DB Close. This
		// allows us to reuse this run's data directory as initial state for
		// future runs without losing any mutations.
		_ = t.getDB(o.objID).Flush()
	}
	t.clearObj(o.objID)
	err := c.Close()
	h.Recordf("%s // %v", o, err)
}

func (o *closeOp) String() string  { return fmt.Sprintf("%s.Close()", o.objID) }
func (o *closeOp) receiver() objID { return o.objID }
func (o *closeOp) syncObjs() objIDSlice {
	return o.affectedObjects
}

func (o *closeOp) keys() []*[]byte                     { return nil }
func (o *closeOp) diagramKeyRanges() []pebble.KeyRange { return nil }

// compactOp models a DB.Compact operation.
type compactOp struct {
	dbID        objID
	start       []byte
	end         []byte
	parallelize bool
}

func (o *compactOp) run(t *Test, h historyRecorder) {
	err := t.withRetries(func() error {
		return t.getDB(o.dbID).Compact(o.start, o.end, o.parallelize)
	})
	h.Recordf("%s // %v", o, err)
}

func (o *compactOp) String() string {
	return fmt.Sprintf("%s.Compact(%q, %q, %t /* parallelize */)", o.dbID, o.start, o.end, o.parallelize)
}

func (o *compactOp) receiver() objID      { return o.dbID }
func (o *compactOp) syncObjs() objIDSlice { return nil }

func (o *compactOp) keys() []*[]byte {
	return []*[]byte{&o.start, &o.end}
}

func (o *compactOp) diagramKeyRanges() []pebble.KeyRange {
	return []pebble.KeyRange{{Start: o.start, End: o.end}}
}

// deleteOp models a Write.Delete operation.
type deleteOp struct {
	writerID objID
	key      []byte

	derivedDBID objID
}

func (o *deleteOp) run(t *Test, h historyRecorder) {
	w := t.getWriter(o.writerID)
	var err error
	if t.testOpts.deleteSized && t.isFMV(o.derivedDBID, pebble.FormatDeleteSizedAndObsolete) {
		// Call DeleteSized with a deterministic size derived from the index.
		// The size does not need to be accurate for correctness.
		err = w.DeleteSized(o.key, hashSize(t.idx), t.writeOpts)
	} else {
		err = w.Delete(o.key, t.writeOpts)
	}
	h.Recordf("%s // %v", o, err)
}

func hashSize(index int) uint32 {
	// Fibonacci hash https://probablydance.com/2018/06/16/fibonacci-hashing-the-optimization-that-the-world-forgot-or-a-better-alternative-to-integer-modulo/
	return uint32((11400714819323198485 * uint64(index)) % maxValueSize)
}

func (o *deleteOp) String() string {
	return fmt.Sprintf("%s.Delete(%q)", o.writerID, o.key)
}
func (o *deleteOp) receiver() objID      { return o.writerID }
func (o *deleteOp) syncObjs() objIDSlice { return nil }

func (o *deleteOp) keys() []*[]byte {
	return []*[]byte{&o.key}
}

func (o *deleteOp) diagramKeyRanges() []pebble.KeyRange {
	return []pebble.KeyRange{{Start: o.key, End: o.key}}
}

// singleDeleteOp models a Write.SingleDelete operation.
type singleDeleteOp struct {
	writerID           objID
	key                []byte
	maybeReplaceDelete bool
}

func (o *singleDeleteOp) run(t *Test, h historyRecorder) {
	w := t.getWriter(o.writerID)
	var err error
	if t.testOpts.replaceSingleDelete && o.maybeReplaceDelete {
		err = w.Delete(o.key, t.writeOpts)
	} else {
		err = w.SingleDelete(o.key, t.writeOpts)
	}
	// NOTE: even if the SINGLEDEL was replaced with a DELETE, we must still
	// write the former to the history log. The log line will indicate whether
	// or not the delete *could* have been replaced. The OPTIONS file should
	// also be consulted to determine what happened at runtime (i.e. by taking
	// the logical AND).
	h.Recordf("%s // %v", o, err)
}

func (o *singleDeleteOp) String() string {
	return fmt.Sprintf("%s.SingleDelete(%q, %v /* maybeReplaceDelete */)", o.writerID, o.key, o.maybeReplaceDelete)
}

func (o *singleDeleteOp) receiver() objID      { return o.writerID }
func (o *singleDeleteOp) syncObjs() objIDSlice { return nil }

func (o *singleDeleteOp) keys() []*[]byte {
	return []*[]byte{&o.key}
}

func (o *singleDeleteOp) diagramKeyRanges() []pebble.KeyRange {
	return []pebble.KeyRange{{Start: o.key, End: o.key}}
}

// deleteRangeOp models a Write.DeleteRange operation.
type deleteRangeOp struct {
	writerID objID
	start    []byte
	end      []byte
}

func (o *deleteRangeOp) run(t *Test, h historyRecorder) {
	w := t.getWriter(o.writerID)
	err := w.DeleteRange(o.start, o.end, t.writeOpts)
	h.Recordf("%s // %v", o, err)
}

func (o *deleteRangeOp) String() string {
	return fmt.Sprintf("%s.DeleteRange(%q, %q)", o.writerID, o.start, o.end)
}

func (o *deleteRangeOp) receiver() objID      { return o.writerID }
func (o *deleteRangeOp) syncObjs() objIDSlice { return nil }

func (o *deleteRangeOp) keys() []*[]byte {
	return []*[]byte{&o.start, &o.end}
}

func (o *deleteRangeOp) diagramKeyRanges() []pebble.KeyRange {
	return []pebble.KeyRange{{Start: o.start, End: o.end}}
}

// flushOp models a DB.Flush operation.
type flushOp struct {
	db objID
}

func (o *flushOp) run(t *Test, h historyRecorder) {
	db := t.getDB(o.db)
	err := db.Flush()
	h.Recordf("%s // %v", o, err)
}

func (o *flushOp) String() string                      { return fmt.Sprintf("%s.Flush()", o.db) }
func (o *flushOp) receiver() objID                     { return o.db }
func (o *flushOp) syncObjs() objIDSlice                { return nil }
func (o *flushOp) keys() []*[]byte                     { return nil }
func (o *flushOp) diagramKeyRanges() []pebble.KeyRange { return nil }

// mergeOp models a Write.Merge operation.
type mergeOp struct {
	writerID objID
	key      []byte
	value    []byte
}

func (o *mergeOp) run(t *Test, h historyRecorder) {
	w := t.getWriter(o.writerID)
	err := w.Merge(o.key, o.value, t.writeOpts)
	h.Recordf("%s // %v", o, err)
}

func (o *mergeOp) String() string       { return fmt.Sprintf("%s.Merge(%q, %q)", o.writerID, o.key, o.value) }
func (o *mergeOp) receiver() objID      { return o.writerID }
func (o *mergeOp) syncObjs() objIDSlice { return nil }

func (o *mergeOp) keys() []*[]byte {
	return []*[]byte{&o.key}
}

func (o *mergeOp) diagramKeyRanges() []pebble.KeyRange {
	return []pebble.KeyRange{{Start: o.key, End: o.key}}
}

// setOp models a Write.Set operation.
type setOp struct {
	writerID objID
	key      []byte
	value    []byte
}

func (o *setOp) run(t *Test, h historyRecorder) {
	w := t.getWriter(o.writerID)
	err := w.Set(o.key, o.value, t.writeOpts)
	h.Recordf("%s // %v", o, err)
}

func (o *setOp) String() string       { return fmt.Sprintf("%s.Set(%q, %q)", o.writerID, o.key, o.value) }
func (o *setOp) receiver() objID      { return o.writerID }
func (o *setOp) syncObjs() objIDSlice { return nil }

func (o *setOp) keys() []*[]byte {
	return []*[]byte{&o.key}
}

func (o *setOp) diagramKeyRanges() []pebble.KeyRange {
	return []pebble.KeyRange{{Start: o.key, End: o.key}}
}

// rangeKeyDeleteOp models a Write.RangeKeyDelete operation.
type rangeKeyDeleteOp struct {
	writerID objID
	start    []byte
	end      []byte
}

func (o *rangeKeyDeleteOp) run(t *Test, h historyRecorder) {
	w := t.getWriter(o.writerID)
	err := w.RangeKeyDelete(o.start, o.end, t.writeOpts)
	h.Recordf("%s // %v", o, err)
}

func (o *rangeKeyDeleteOp) String() string {
	return fmt.Sprintf("%s.RangeKeyDelete(%q, %q)", o.writerID, o.start, o.end)
}

func (o *rangeKeyDeleteOp) receiver() objID      { return o.writerID }
func (o *rangeKeyDeleteOp) syncObjs() objIDSlice { return nil }

func (o *rangeKeyDeleteOp) keys() []*[]byte {
	return []*[]byte{&o.start, &o.end}
}

func (o *rangeKeyDeleteOp) diagramKeyRanges() []pebble.KeyRange {
	return []pebble.KeyRange{{Start: o.start, End: o.end}}
}

// rangeKeySetOp models a Write.RangeKeySet operation.
type rangeKeySetOp struct {
	writerID objID
	start    []byte
	end      []byte
	suffix   []byte
	value    []byte
}

func (o *rangeKeySetOp) run(t *Test, h historyRecorder) {
	w := t.getWriter(o.writerID)
	err := w.RangeKeySet(o.start, o.end, o.suffix, o.value, t.writeOpts)
	h.Recordf("%s // %v", o, err)
}

func (o *rangeKeySetOp) String() string {
	return fmt.Sprintf("%s.RangeKeySet(%q, %q, %q, %q)",
		o.writerID, o.start, o.end, o.suffix, o.value)
}

func (o *rangeKeySetOp) receiver() objID      { return o.writerID }
func (o *rangeKeySetOp) syncObjs() objIDSlice { return nil }

func (o *rangeKeySetOp) keys() []*[]byte {
	return []*[]byte{&o.start, &o.end}
}

func (o *rangeKeySetOp) diagramKeyRanges() []pebble.KeyRange {
	return []pebble.KeyRange{{Start: o.start, End: o.end}}
}

// rangeKeyUnsetOp models a Write.RangeKeyUnset operation.
type rangeKeyUnsetOp struct {
	writerID objID
	start    []byte
	end      []byte
	suffix   []byte
}

func (o *rangeKeyUnsetOp) run(t *Test, h historyRecorder) {
	w := t.getWriter(o.writerID)
	err := w.RangeKeyUnset(o.start, o.end, o.suffix, t.writeOpts)
	h.Recordf("%s // %v", o, err)
}

func (o *rangeKeyUnsetOp) String() string {
	return fmt.Sprintf("%s.RangeKeyUnset(%q, %q, %q)",
		o.writerID, o.start, o.end, o.suffix)
}

func (o *rangeKeyUnsetOp) receiver() objID      { return o.writerID }
func (o *rangeKeyUnsetOp) syncObjs() objIDSlice { return nil }

func (o *rangeKeyUnsetOp) keys() []*[]byte {
	return []*[]byte{&o.start, &o.end}
}

func (o *rangeKeyUnsetOp) diagramKeyRanges() []pebble.KeyRange {
	return []pebble.KeyRange{{Start: o.start, End: o.end}}
}

// newBatchOp models a Write.NewBatch operation.
type newBatchOp struct {
	dbID    objID
	batchID objID
}

func (o *newBatchOp) run(t *Test, h historyRecorder) {
	b := t.getDB(o.dbID).NewBatch()
	t.setBatch(o.batchID, b)
	h.Recordf("%s", o)
}

func (o *newBatchOp) String() string  { return fmt.Sprintf("%s = %s.NewBatch()", o.batchID, o.dbID) }
func (o *newBatchOp) receiver() objID { return o.dbID }
func (o *newBatchOp) syncObjs() objIDSlice {
	// NewBatch should not be concurrent with operations that interact with that
	// same batch.
	return []objID{o.batchID}
}

func (o *newBatchOp) keys() []*[]byte                     { return nil }
func (o *newBatchOp) diagramKeyRanges() []pebble.KeyRange { return nil }

// newIndexedBatchOp models a Write.NewIndexedBatch operation.
type newIndexedBatchOp struct {
	dbID    objID
	batchID objID
}

func (o *newIndexedBatchOp) run(t *Test, h historyRecorder) {
	b := t.getDB(o.dbID).NewIndexedBatch()
	t.setBatch(o.batchID, b)
	h.Recordf("%s", o)
}

func (o *newIndexedBatchOp) String() string {
	return fmt.Sprintf("%s = %s.NewIndexedBatch()", o.batchID, o.dbID)
}
func (o *newIndexedBatchOp) receiver() objID { return o.dbID }
func (o *newIndexedBatchOp) syncObjs() objIDSlice {
	// NewIndexedBatch should not be concurrent with operations that interact
	// with that same batch.
	return []objID{o.batchID}
}

func (o *newIndexedBatchOp) keys() []*[]byte                     { return nil }
func (o *newIndexedBatchOp) diagramKeyRanges() []pebble.KeyRange { return nil }

// batchCommitOp models a Batch.Commit operation.
type batchCommitOp struct {
	dbID    objID
	batchID objID
}

func (o *batchCommitOp) run(t *Test, h historyRecorder) {
	b := t.getBatch(o.batchID)
	err := b.Commit(t.writeOpts)
	h.Recordf("%s // %v", o, err)
}

func (o *batchCommitOp) String() string  { return fmt.Sprintf("%s.Commit()", o.batchID) }
func (o *batchCommitOp) receiver() objID { return o.batchID }
func (o *batchCommitOp) syncObjs() objIDSlice {
	// Synchronize on the database so that NewIters wait for the commit.
	return []objID{o.dbID}
}

func (o *batchCommitOp) keys() []*[]byte                     { return nil }
func (o *batchCommitOp) diagramKeyRanges() []pebble.KeyRange { return nil }

// ingestOp models a DB.Ingest operation.
type ingestOp struct {
	dbID     objID
	batchIDs []objID

	derivedDBIDs []objID
}

func (o *ingestOp) run(t *Test, h historyRecorder) {
	// We can only use apply as an alternative for ingestion if we are ingesting
	// a single batch. If we are ingesting multiple batches, the batches may
	// overlap which would cause ingestion to fail but apply would succeed.
	if t.testOpts.ingestUsingApply && len(o.batchIDs) == 1 && o.derivedDBIDs[0] == o.dbID {
		id := o.batchIDs[0]
		b := t.getBatch(id)
		iter, rangeDelIter, rangeKeyIter := private.BatchSort(b)
		db := t.getDB(o.dbID)
		c, err := o.collapseBatch(t, db, iter, rangeDelIter, rangeKeyIter, b)
		if err == nil {
			err = db.Apply(c, t.writeOpts)
		}
		_ = b.Close()
		_ = c.Close()
		t.clearObj(id)
		h.Recordf("%s // %v", o, err)
		return
	}

	var paths []string
	var err error
	for i, id := range o.batchIDs {
		b := t.getBatch(id)
		t.clearObj(id)
		path, err2 := o.build(t, h, b, i)
		if err2 != nil {
			h.Recordf("Build(%s) // %v", id, err2)
		}
		err = firstError(err, err2)
		if err2 == nil {
			paths = append(paths, path)
		}
		err = firstError(err, b.Close())
	}

	err = firstError(err, t.withRetries(func() error {
		return t.getDB(o.dbID).Ingest(paths)
	}))

	h.Recordf("%s // %v", o, err)
}

func buildForIngest(
	t *Test, dbID objID, h historyRecorder, b *pebble.Batch, i int,
) (string, *sstable.WriterMetadata, error) {
	path := t.opts.FS.PathJoin(t.tmpDir, fmt.Sprintf("ext%d-%d", dbID.slot(), i))
	f, err := t.opts.FS.Create(path)
	if err != nil {
		return "", nil, err
	}
	db := t.getDB(dbID)

	iter, rangeDelIter, rangeKeyIter := private.BatchSort(b)
	defer closeIters(iter, rangeDelIter, rangeKeyIter)

	equal := t.opts.Comparer.Equal
	tableFormat := db.FormatMajorVersion().MaxTableFormat()
	w := sstable.NewWriter(
		objstorageprovider.NewFileWritable(f),
		t.opts.MakeWriterOptions(0, tableFormat),
	)

	var lastUserKey []byte
	for key, value := iter.First(); key != nil; key, value = iter.Next() {
		// Ignore duplicate keys.
		if equal(lastUserKey, key.UserKey) {
			continue
		}
		// NB: We don't have to copy the key or value since we're reading from a
		// batch which doesn't do prefix compression.
		lastUserKey = key.UserKey

		key.SetSeqNum(base.SeqNumZero)
		// It's possible that we wrote the key on a batch from a db that supported
		// DeleteSized, but are now ingesting into a db that does not. Detect
		// this case and translate the key to an InternalKeyKindDelete.
		if key.Kind() == pebble.InternalKeyKindDeleteSized && !t.isFMV(dbID, pebble.FormatDeleteSizedAndObsolete) {
			value = pebble.LazyValue{}
			key.SetKind(pebble.InternalKeyKindDelete)
		}
		if err := w.Add(*key, value.InPlaceValue()); err != nil {
			return "", nil, err
		}
	}
	if err := iter.Close(); err != nil {
		return "", nil, err
	}
	iter = nil

	if rangeDelIter != nil {
		// NB: The range tombstones have already been fragmented by the Batch.
		for t := rangeDelIter.First(); t != nil; t = rangeDelIter.Next() {
			// NB: We don't have to copy the key or value since we're reading from a
			// batch which doesn't do prefix compression.
			if err := w.DeleteRange(t.Start, t.End); err != nil {
				return "", nil, err
			}
		}
		if err := rangeDelIter.Close(); err != nil {
			return "", nil, err
		}
		rangeDelIter = nil
	}

	if rangeKeyIter != nil {
		for span := rangeKeyIter.First(); span != nil; span = rangeKeyIter.Next() {
			// Coalesce the keys of this span and then zero the sequence
			// numbers. This is necessary in order to make the range keys within
			// the ingested sstable internally consistent at the sequence number
			// it's ingested at. The individual keys within a batch are
			// committed at unique sequence numbers, whereas all the keys of an
			// ingested sstable are given the same sequence number. A span
			// contaning keys that both set and unset the same suffix at the
			// same sequence number is nonsensical, so we "coalesce" or collapse
			// the keys.
			collapsed := keyspan.Span{
				Start: span.Start,
				End:   span.End,
				Keys:  make([]keyspan.Key, 0, len(span.Keys)),
			}
			err = rangekey.Coalesce(t.opts.Comparer.Compare, equal, span.Keys, &collapsed.Keys)
			if err != nil {
				return "", nil, err
			}
			for i := range collapsed.Keys {
				collapsed.Keys[i].Trailer = base.MakeTrailer(0, collapsed.Keys[i].Kind())
			}
			keyspan.SortKeysByTrailer(&collapsed.Keys)
			if err := rangekey.Encode(&collapsed, w.AddRangeKey); err != nil {
				return "", nil, err
			}
		}
		if err := rangeKeyIter.Error(); err != nil {
			return "", nil, err
		}
		if err := rangeKeyIter.Close(); err != nil {
			return "", nil, err
		}
		rangeKeyIter = nil
	}

	if err := w.Close(); err != nil {
		return "", nil, err
	}
	meta, err := w.Metadata()
	return path, meta, err
}

func (o *ingestOp) build(t *Test, h historyRecorder, b *pebble.Batch, i int) (string, error) {
	path, _, err := buildForIngest(t, o.dbID, h, b, i)
	return path, err
}

func (o *ingestOp) receiver() objID { return o.dbID }
func (o *ingestOp) syncObjs() objIDSlice {
	// Ingest should not be concurrent with mutating the batches that will be
	// ingested as sstables.
	objs := make([]objID, 0, len(o.batchIDs)+1)
	objs = append(objs, o.batchIDs...)
	addedDBs := make(map[objID]struct{})
	for i := range o.derivedDBIDs {
		_, ok := addedDBs[o.derivedDBIDs[i]]
		if !ok && o.derivedDBIDs[i] != o.dbID {
			objs = append(objs, o.derivedDBIDs[i])
			addedDBs[o.derivedDBIDs[i]] = struct{}{}
		}
	}
	return objs
}

func closeIters(
	pointIter base.InternalIterator,
	rangeDelIter keyspan.FragmentIterator,
	rangeKeyIter keyspan.FragmentIterator,
) {
	if pointIter != nil {
		pointIter.Close()
	}
	if rangeDelIter != nil {
		rangeDelIter.Close()
	}
	if rangeKeyIter != nil {
		rangeKeyIter.Close()
	}
}

// collapseBatch collapses the mutations in a batch to be equivalent to an
// sstable ingesting those mutations. Duplicate updates to a key are collapsed
// so that only the latest update is performed. All range deletions are
// performed first in the batch to match the semantics of ingestion where a
// range deletion does not delete a point record contained in the sstable.
func (o *ingestOp) collapseBatch(
	t *Test,
	db *pebble.DB,
	pointIter base.InternalIterator,
	rangeDelIter, rangeKeyIter keyspan.FragmentIterator,
	b *pebble.Batch,
) (*pebble.Batch, error) {
	defer closeIters(pointIter, rangeDelIter, rangeKeyIter)
	equal := t.opts.Comparer.Equal
	collapsed := db.NewBatch()

	if rangeDelIter != nil {
		// NB: The range tombstones have already been fragmented by the Batch.
		for t := rangeDelIter.First(); t != nil; t = rangeDelIter.Next() {
			// NB: We don't have to copy the key or value since we're reading from a
			// batch which doesn't do prefix compression.
			if err := collapsed.DeleteRange(t.Start, t.End, nil); err != nil {
				return nil, err
			}
		}
		if err := rangeDelIter.Close(); err != nil {
			return nil, err
		}
		rangeDelIter = nil
	}

	if pointIter != nil {
		var lastUserKey []byte
		for key, value := pointIter.First(); key != nil; key, value = pointIter.Next() {
			// Ignore duplicate keys.
			//
			// Note: this is necessary due to MERGE keys, otherwise it would be
			// fine to include all the keys in the batch and let the normal
			// sequence number precedence determine which of the keys "wins".
			// But the code to build the ingested sstable will only keep the
			// most recent internal key and will not merge across internal keys.
			if equal(lastUserKey, key.UserKey) {
				continue
			}
			// NB: We don't have to copy the key or value since we're reading from a
			// batch which doesn't do prefix compression.
			lastUserKey = key.UserKey

			var err error
			switch key.Kind() {
			case pebble.InternalKeyKindDelete:
				err = collapsed.Delete(key.UserKey, nil)
			case pebble.InternalKeyKindDeleteSized:
				v, _ := binary.Uvarint(value.InPlaceValue())
				// Batch.DeleteSized takes just the length of the value being
				// deleted and adds the key's length to derive the overall entry
				// size of the value being deleted. This has already been done
				// to the key we're reading from the batch, so we must subtract
				// the key length from the encoded value before calling
				// collapsed.DeleteSized, which will again add the key length
				// before encoding.
				err = collapsed.DeleteSized(key.UserKey, uint32(v-uint64(len(key.UserKey))), nil)
			case pebble.InternalKeyKindSingleDelete:
				err = collapsed.SingleDelete(key.UserKey, nil)
			case pebble.InternalKeyKindSet:
				err = collapsed.Set(key.UserKey, value.InPlaceValue(), nil)
			case pebble.InternalKeyKindMerge:
				err = collapsed.Merge(key.UserKey, value.InPlaceValue(), nil)
			case pebble.InternalKeyKindLogData:
				err = collapsed.LogData(key.UserKey, nil)
			default:
				err = errors.Errorf("unknown batch record kind: %d", key.Kind())
			}
			if err != nil {
				return nil, err
			}
		}
		if err := pointIter.Close(); err != nil {
			return nil, err
		}
		pointIter = nil
	}

	// There's no equivalent of a MERGE operator for range keys, so there's no
	// need to collapse the range keys here. Rather than reading the range keys
	// from `rangeKeyIter`, which will already be fragmented, read the range
	// keys from the batch and copy them verbatim. This marginally improves our
	// test coverage over the alternative approach of pre-fragmenting and
	// pre-coalescing before writing to the batch.
	//
	// The `rangeKeyIter` is used only to determine if there are any range keys
	// in the batch at all, and only because we already have it handy from
	// private.BatchSort.
	if rangeKeyIter != nil {
		for r := b.Reader(); ; {
			kind, key, value, ok, err := r.Next()
			if !ok {
				if err != nil {
					return nil, err
				}
				break
			} else if !rangekey.IsRangeKey(kind) {
				continue
			}
			ik := base.MakeInternalKey(key, 0, kind)
			if err := collapsed.AddInternalKey(&ik, value, nil); err != nil {
				return nil, err
			}
		}
		if err := rangeKeyIter.Close(); err != nil {
			return nil, err
		}
		rangeKeyIter = nil
	}

	return collapsed, nil
}

func (o *ingestOp) String() string {
	var buf strings.Builder
	buf.WriteString(o.dbID.String())
	buf.WriteString(".Ingest(")
	for i, id := range o.batchIDs {
		if i > 0 {
			buf.WriteString(", ")
		}
		buf.WriteString(id.String())
	}
	buf.WriteString(")")
	return buf.String()
}

func (o *ingestOp) keys() []*[]byte                     { return nil }
func (o *ingestOp) diagramKeyRanges() []pebble.KeyRange { return nil }

type ingestAndExciseOp struct {
	dbID                   objID
	batchID                objID
	derivedDBID            objID
	exciseStart, exciseEnd []byte
}

func (o *ingestAndExciseOp) run(t *Test, h historyRecorder) {
	var err error
	b := t.getBatch(o.batchID)
	t.clearObj(o.batchID)
	if t.testOpts.Opts.Comparer.Compare(o.exciseEnd, o.exciseStart) <= 0 {
		panic("non-well-formed excise span")
	}
	if b.Empty() {
		// No-op.
		h.Recordf("%s // %v", o, err)
		return
	}
	path, writerMeta, err2 := o.build(t, h, b, 0 /* i */)
	if err2 != nil {
		h.Recordf("Build(%s) // %v", o.batchID, err2)
		return
	}
	err = firstError(err, err2)
	err = firstError(err, b.Close())

	if writerMeta.Properties.NumEntries == 0 && writerMeta.Properties.NumRangeKeys() == 0 {
		// No-op.
		h.Recordf("%s // %v", o, err)
		return
	}
	db := t.getDB(o.dbID)
	if !t.testOpts.useExcise {
		// Do a rangedel and rangekeydel before the ingestion. This mimics the
		// behaviour of an excise.
		err = firstError(err, db.DeleteRange(o.exciseStart, o.exciseEnd, t.writeOpts))
		err = firstError(err, db.RangeKeyDelete(o.exciseStart, o.exciseEnd, t.writeOpts))
	}

	if t.testOpts.useExcise {
		err = firstError(err, t.withRetries(func() error {
			_, err := t.getDB(o.dbID).IngestAndExcise([]string{path}, nil /* sharedSSTs */, pebble.KeyRange{
				Start: o.exciseStart,
				End:   o.exciseEnd,
			})
			return err
		}))
	} else {
		err = firstError(err, t.withRetries(func() error {
			return t.getDB(o.dbID).Ingest([]string{path})
		}))
	}

	h.Recordf("%s // %v", o, err)
}

func (o *ingestAndExciseOp) build(
	t *Test, h historyRecorder, b *pebble.Batch, i int,
) (string, *sstable.WriterMetadata, error) {
	return buildForIngest(t, o.dbID, h, b, i)
}

func (o *ingestAndExciseOp) receiver() objID { return o.dbID }
func (o *ingestAndExciseOp) syncObjs() objIDSlice {
	// Ingest should not be concurrent with mutating the batches that will be
	// ingested as sstables.
	objs := []objID{o.batchID}
	if o.derivedDBID != o.dbID {
		objs = append(objs, o.derivedDBID)
	}
	return objs
}

func (o *ingestAndExciseOp) String() string {
	return fmt.Sprintf("%s.IngestAndExcise(%s, %q, %q)", o.dbID, o.batchID, o.exciseStart, o.exciseEnd)
}

func (o *ingestAndExciseOp) keys() []*[]byte {
	return []*[]byte{&o.exciseStart, &o.exciseEnd}
}

func (o *ingestAndExciseOp) diagramKeyRanges() []pebble.KeyRange {
	return []pebble.KeyRange{{Start: o.exciseStart, End: o.exciseEnd}}
}

// getOp models a Reader.Get operation.
type getOp struct {
	readerID    objID
	key         []byte
	derivedDBID objID
}

func (o *getOp) run(t *Test, h historyRecorder) {
	r := t.getReader(o.readerID)
	var val []byte
	var closer io.Closer
	err := t.withRetries(func() (err error) {
		val, closer, err = r.Get(o.key)
		return err
	})
	h.Recordf("%s // [%q] %v", o, val, err)
	if closer != nil {
		closer.Close()
	}
}

func (o *getOp) String() string  { return fmt.Sprintf("%s.Get(%q)", o.readerID, o.key) }
func (o *getOp) receiver() objID { return o.readerID }
func (o *getOp) syncObjs() objIDSlice {
	if o.readerID.tag() == dbTag {
		return nil
	}
	// batch.Get reads through to the current database state.
	if o.derivedDBID != 0 {
		return []objID{o.derivedDBID}
	}
	return nil
}

func (o *getOp) keys() []*[]byte {
	return []*[]byte{&o.key}
}

func (o *getOp) diagramKeyRanges() []pebble.KeyRange {
	return []pebble.KeyRange{{Start: o.key, End: o.key}}
}

// newIterOp models a Reader.NewIter operation.
type newIterOp struct {
	readerID objID
	iterID   objID
	iterOpts
	derivedDBID objID
}

// Enable this to enable debug logging of range key iterator operations.
const debugIterators = false

func (o *newIterOp) run(t *Test, h historyRecorder) {
	r := t.getReader(o.readerID)
	opts := iterOptions(o.iterOpts)
	if debugIterators {
		opts.DebugRangeKeyStack = true
	}

	var i *pebble.Iterator
	for {
		i, _ = r.NewIter(opts)
		if err := i.Error(); !errors.Is(err, errorfs.ErrInjected) {
			break
		}
		// close this iter and retry NewIter
		_ = i.Close()
	}
	t.setIter(o.iterID, i)

	// Trash the bounds to ensure that Pebble doesn't rely on the stability of
	// the user-provided bounds.
	if opts != nil {
		rand.Read(opts.LowerBound[:])
		rand.Read(opts.UpperBound[:])
	}
	h.Recordf("%s // %v", o, i.Error())
}

func (o *newIterOp) String() string {
	return fmt.Sprintf("%s = %s.NewIter(%q, %q, %d /* key types */, %d, %d, %t /* use L6 filters */, %q /* masking suffix */)",
		o.iterID, o.readerID, o.lower, o.upper, o.keyTypes, o.filterMin, o.filterMax, o.useL6Filters, o.maskSuffix)
}

func (o *newIterOp) receiver() objID { return o.readerID }
func (o *newIterOp) syncObjs() objIDSlice {
	// Prevent o.iterID ops from running before it exists.
	objs := []objID{o.iterID}
	// If reading through a batch or snapshot, the new iterator will also observe database
	// state, and we must synchronize on the database state for a consistent
	// view.
	if o.readerID.tag() == batchTag || o.readerID.tag() == snapTag {
		objs = append(objs, o.derivedDBID)
	}
	return objs
}

func (o *newIterOp) keys() []*[]byte                     { return nil }
func (o *newIterOp) diagramKeyRanges() []pebble.KeyRange { return nil }

// newIterUsingCloneOp models a Iterator.Clone operation.
type newIterUsingCloneOp struct {
	existingIterID objID
	iterID         objID
	refreshBatch   bool
	iterOpts

	// derivedReaderID is the ID of the underlying reader that backs both the
	// existing iterator and the new iterator. The derivedReaderID is NOT
	// serialized by String and is derived from other operations during parse.
	derivedReaderID objID
}

func (o *newIterUsingCloneOp) run(t *Test, h historyRecorder) {
	iter := t.getIter(o.existingIterID)
	cloneOpts := pebble.CloneOptions{
		IterOptions:      iterOptions(o.iterOpts),
		RefreshBatchView: o.refreshBatch,
	}
	i, err := iter.iter.Clone(cloneOpts)
	if err != nil {
		panic(err)
	}
	t.setIter(o.iterID, i)
	h.Recordf("%s // %v", o, i.Error())
}

func (o *newIterUsingCloneOp) String() string {
	return fmt.Sprintf("%s = %s.Clone(%t, %q, %q, %d /* key types */, %d, %d, %t /* use L6 filters */, %q /* masking suffix */)",
		o.iterID, o.existingIterID, o.refreshBatch, o.lower, o.upper,
		o.keyTypes, o.filterMin, o.filterMax, o.useL6Filters, o.maskSuffix)
}

func (o *newIterUsingCloneOp) receiver() objID { return o.existingIterID }

func (o *newIterUsingCloneOp) syncObjs() objIDSlice {
	objIDs := []objID{o.iterID}
	// If the underlying reader is a batch, we must synchronize with the batch.
	// If refreshBatch=true, synchronizing is necessary to observe all the
	// mutations up to until this op and no more. Even when refreshBatch=false,
	// we must synchronize because iterator construction may access state cached
	// on the indexed batch to avoid refragmenting range tombstones or range
	// keys.
	if o.derivedReaderID.tag() == batchTag {
		objIDs = append(objIDs, o.derivedReaderID)
	}
	return objIDs
}

func (o *newIterUsingCloneOp) keys() []*[]byte                     { return nil }
func (o *newIterUsingCloneOp) diagramKeyRanges() []pebble.KeyRange { return nil }

// iterSetBoundsOp models an Iterator.SetBounds operation.
type iterSetBoundsOp struct {
	iterID objID
	lower  []byte
	upper  []byte
}

func (o *iterSetBoundsOp) run(t *Test, h historyRecorder) {
	i := t.getIter(o.iterID)
	var lower, upper []byte
	if o.lower != nil {
		lower = append(lower, o.lower...)
	}
	if o.upper != nil {
		upper = append(upper, o.upper...)
	}
	i.SetBounds(lower, upper)

	// Trash the bounds to ensure that Pebble doesn't rely on the stability of
	// the user-provided bounds.
	rand.Read(lower[:])
	rand.Read(upper[:])

	h.Recordf("%s // %v", o, i.Error())
}

func (o *iterSetBoundsOp) String() string {
	return fmt.Sprintf("%s.SetBounds(%q, %q)", o.iterID, o.lower, o.upper)
}

func (o *iterSetBoundsOp) receiver() objID      { return o.iterID }
func (o *iterSetBoundsOp) syncObjs() objIDSlice { return nil }

func (o *iterSetBoundsOp) keys() []*[]byte {
	return []*[]byte{&o.lower, &o.upper}
}

func (o *iterSetBoundsOp) diagramKeyRanges() []pebble.KeyRange {
	return []pebble.KeyRange{{Start: o.lower, End: o.upper}}
}

// iterSetOptionsOp models an Iterator.SetOptions operation.
type iterSetOptionsOp struct {
	iterID objID
	iterOpts

	// derivedReaderID is the ID of the underlying reader that backs the
	// iterator. The derivedReaderID is NOT serialized by String and is derived
	// from other operations during parse.
	derivedReaderID objID
}

func (o *iterSetOptionsOp) run(t *Test, h historyRecorder) {
	i := t.getIter(o.iterID)

	opts := iterOptions(o.iterOpts)
	if opts == nil {
		opts = &pebble.IterOptions{}
	}
	i.SetOptions(opts)

	// Trash the bounds to ensure that Pebble doesn't rely on the stability of
	// the user-provided bounds.
	rand.Read(opts.LowerBound[:])
	rand.Read(opts.UpperBound[:])

	h.Recordf("%s // %v", o, i.Error())
}

func (o *iterSetOptionsOp) String() string {
	return fmt.Sprintf("%s.SetOptions(%q, %q, %d /* key types */, %d, %d, %t /* use L6 filters */, %q /* masking suffix */)",
		o.iterID, o.lower, o.upper, o.keyTypes, o.filterMin, o.filterMax, o.useL6Filters, o.maskSuffix)
}

func iterOptions(o iterOpts) *pebble.IterOptions {
	if o.IsZero() {
		return nil
	}
	var lower, upper []byte
	if o.lower != nil {
		lower = append(lower, o.lower...)
	}
	if o.upper != nil {
		upper = append(upper, o.upper...)
	}
	opts := &pebble.IterOptions{
		LowerBound: lower,
		UpperBound: upper,
		KeyTypes:   pebble.IterKeyType(o.keyTypes),
		RangeKeyMasking: pebble.RangeKeyMasking{
			Suffix: o.maskSuffix,
		},
		UseL6Filters: o.useL6Filters,
	}
	if opts.RangeKeyMasking.Suffix != nil {
		opts.RangeKeyMasking.Filter = func() pebble.BlockPropertyFilterMask {
			return sstable.NewTestKeysMaskingFilter()
		}
	}
	if o.filterMax > 0 {
		opts.PointKeyFilters = []pebble.BlockPropertyFilter{
			sstable.NewTestKeysBlockPropertyFilter(o.filterMin, o.filterMax),
		}
		// Enforce the timestamp bounds in SkipPoint, so that the iterator never
		// returns a key outside the filterMin, filterMax bounds. This provides
		// deterministic iteration.
		opts.SkipPoint = func(k []byte) (skip bool) {
			n := testkeys.Comparer.Split(k)
			if n == len(k) {
				// No suffix, don't skip it.
				return false
			}
			v, err := testkeys.ParseSuffix(k[n:])
			if err != nil {
				panic(err)
			}
			ts := uint64(v)
			return ts < o.filterMin || ts >= o.filterMax
		}
	}
	return opts
}

func (o *iterSetOptionsOp) receiver() objID { return o.iterID }

func (o *iterSetOptionsOp) syncObjs() objIDSlice {
	if o.derivedReaderID.tag() == batchTag {
		// If the underlying reader is a batch, we must synchronize with the
		// batch so that we observe all the mutations up until this operation
		// and no more.
		return []objID{o.derivedReaderID}
	}
	return nil
}

func (o *iterSetOptionsOp) keys() []*[]byte                     { return nil }
func (o *iterSetOptionsOp) diagramKeyRanges() []pebble.KeyRange { return nil }

// iterSeekGEOp models an Iterator.SeekGE[WithLimit] operation.
type iterSeekGEOp struct {
	iterID objID
	key    []byte
	limit  []byte

	derivedReaderID objID
}

func iteratorPos(i *retryableIter) string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%q", i.Key())
	hasPoint, hasRange := i.HasPointAndRange()
	if hasPoint {
		fmt.Fprintf(&buf, ",%q", i.Value())
	} else {
		fmt.Fprint(&buf, ",<no point>")
	}
	if hasRange {
		start, end := i.RangeBounds()
		fmt.Fprintf(&buf, ",[%q,%q)=>{", start, end)
		for i, rk := range i.RangeKeys() {
			if i > 0 {
				fmt.Fprint(&buf, ",")
			}
			fmt.Fprintf(&buf, "%q=%q", rk.Suffix, rk.Value)
		}
		fmt.Fprint(&buf, "}")
	} else {
		fmt.Fprint(&buf, ",<no range>")
	}
	if i.RangeKeyChanged() {
		fmt.Fprint(&buf, "*")
	}
	return buf.String()
}

func validBoolToStr(valid bool) string {
	return fmt.Sprintf("%t", valid)
}

func validityStateToStr(validity pebble.IterValidityState) (bool, string) {
	// We can't distinguish between IterExhausted and IterAtLimit in a
	// deterministic manner.
	switch validity {
	case pebble.IterExhausted, pebble.IterAtLimit:
		return false, "invalid"
	case pebble.IterValid:
		return true, "valid"
	default:
		panic("unknown validity")
	}
}

func (o *iterSeekGEOp) run(t *Test, h historyRecorder) {
	i := t.getIter(o.iterID)
	var valid bool
	var validStr string
	if o.limit == nil {
		valid = i.SeekGE(o.key)
		validStr = validBoolToStr(valid)
	} else {
		valid, validStr = validityStateToStr(i.SeekGEWithLimit(o.key, o.limit))
	}
	if valid {
		h.Recordf("%s // [%s,%s] %v", o, validStr, iteratorPos(i), i.Error())
	} else {
		h.Recordf("%s // [%s] %v", o, validStr, i.Error())
	}
}

func (o *iterSeekGEOp) String() string {
	return fmt.Sprintf("%s.SeekGE(%q, %q)", o.iterID, o.key, o.limit)
}
func (o *iterSeekGEOp) receiver() objID      { return o.iterID }
func (o *iterSeekGEOp) syncObjs() objIDSlice { return onlyBatchIDs(o.derivedReaderID) }

func (o *iterSeekGEOp) keys() []*[]byte {
	return []*[]byte{&o.key}
}

func (o *iterSeekGEOp) diagramKeyRanges() []pebble.KeyRange {
	return []pebble.KeyRange{{Start: o.key, End: o.key}}
}

func onlyBatchIDs(ids ...objID) objIDSlice {
	var ret objIDSlice
	for _, id := range ids {
		if id.tag() == batchTag {
			ret = append(ret, id)
		}
	}
	return ret
}

// iterSeekPrefixGEOp models an Iterator.SeekPrefixGE operation.
type iterSeekPrefixGEOp struct {
	iterID objID
	key    []byte

	derivedReaderID objID
}

func (o *iterSeekPrefixGEOp) run(t *Test, h historyRecorder) {
	i := t.getIter(o.iterID)
	valid := i.SeekPrefixGE(o.key)
	if valid {
		h.Recordf("%s // [%t,%s] %v", o, valid, iteratorPos(i), i.Error())
	} else {
		h.Recordf("%s // [%t] %v", o, valid, i.Error())
	}
}

func (o *iterSeekPrefixGEOp) String() string {
	return fmt.Sprintf("%s.SeekPrefixGE(%q)", o.iterID, o.key)
}
func (o *iterSeekPrefixGEOp) receiver() objID      { return o.iterID }
func (o *iterSeekPrefixGEOp) syncObjs() objIDSlice { return onlyBatchIDs(o.derivedReaderID) }

func (o *iterSeekPrefixGEOp) keys() []*[]byte {
	return []*[]byte{&o.key}
}

func (o *iterSeekPrefixGEOp) diagramKeyRanges() []pebble.KeyRange {
	return []pebble.KeyRange{{Start: o.key, End: o.key}}
}

// iterSeekLTOp models an Iterator.SeekLT[WithLimit] operation.
type iterSeekLTOp struct {
	iterID objID
	key    []byte
	limit  []byte

	derivedReaderID objID
}

func (o *iterSeekLTOp) run(t *Test, h historyRecorder) {
	i := t.getIter(o.iterID)
	var valid bool
	var validStr string
	if o.limit == nil {
		valid = i.SeekLT(o.key)
		validStr = validBoolToStr(valid)
	} else {
		valid, validStr = validityStateToStr(i.SeekLTWithLimit(o.key, o.limit))
	}
	if valid {
		h.Recordf("%s // [%s,%s] %v", o, validStr, iteratorPos(i), i.Error())
	} else {
		h.Recordf("%s // [%s] %v", o, validStr, i.Error())
	}
}

func (o *iterSeekLTOp) String() string {
	return fmt.Sprintf("%s.SeekLT(%q, %q)", o.iterID, o.key, o.limit)
}

func (o *iterSeekLTOp) receiver() objID      { return o.iterID }
func (o *iterSeekLTOp) syncObjs() objIDSlice { return onlyBatchIDs(o.derivedReaderID) }

func (o *iterSeekLTOp) keys() []*[]byte {
	return []*[]byte{&o.key}
}

func (o *iterSeekLTOp) diagramKeyRanges() []pebble.KeyRange {
	return []pebble.KeyRange{{Start: o.key, End: o.key}}
}

// iterFirstOp models an Iterator.First operation.
type iterFirstOp struct {
	iterID objID

	derivedReaderID objID
}

func (o *iterFirstOp) run(t *Test, h historyRecorder) {
	i := t.getIter(o.iterID)
	valid := i.First()
	if valid {
		h.Recordf("%s // [%t,%s] %v", o, valid, iteratorPos(i), i.Error())
	} else {
		h.Recordf("%s // [%t] %v", o, valid, i.Error())
	}
}

func (o *iterFirstOp) String() string       { return fmt.Sprintf("%s.First()", o.iterID) }
func (o *iterFirstOp) receiver() objID      { return o.iterID }
func (o *iterFirstOp) syncObjs() objIDSlice { return onlyBatchIDs(o.derivedReaderID) }

func (o *iterFirstOp) keys() []*[]byte                     { return nil }
func (o *iterFirstOp) diagramKeyRanges() []pebble.KeyRange { return nil }

// iterLastOp models an Iterator.Last operation.
type iterLastOp struct {
	iterID objID

	derivedReaderID objID
}

func (o *iterLastOp) run(t *Test, h historyRecorder) {
	i := t.getIter(o.iterID)
	valid := i.Last()
	if valid {
		h.Recordf("%s // [%t,%s] %v", o, valid, iteratorPos(i), i.Error())
	} else {
		h.Recordf("%s // [%t] %v", o, valid, i.Error())
	}
}

func (o *iterLastOp) String() string       { return fmt.Sprintf("%s.Last()", o.iterID) }
func (o *iterLastOp) receiver() objID      { return o.iterID }
func (o *iterLastOp) syncObjs() objIDSlice { return onlyBatchIDs(o.derivedReaderID) }

func (o *iterLastOp) keys() []*[]byte                     { return nil }
func (o *iterLastOp) diagramKeyRanges() []pebble.KeyRange { return nil }

// iterNextOp models an Iterator.Next[WithLimit] operation.
type iterNextOp struct {
	iterID objID
	limit  []byte

	derivedReaderID objID
}

func (o *iterNextOp) run(t *Test, h historyRecorder) {
	i := t.getIter(o.iterID)
	var valid bool
	var validStr string
	if o.limit == nil {
		valid = i.Next()
		validStr = validBoolToStr(valid)
	} else {
		valid, validStr = validityStateToStr(i.NextWithLimit(o.limit))
	}
	if valid {
		h.Recordf("%s // [%s,%s] %v", o, validStr, iteratorPos(i), i.Error())
	} else {
		h.Recordf("%s // [%s] %v", o, validStr, i.Error())
	}
}

func (o *iterNextOp) String() string       { return fmt.Sprintf("%s.Next(%q)", o.iterID, o.limit) }
func (o *iterNextOp) receiver() objID      { return o.iterID }
func (o *iterNextOp) syncObjs() objIDSlice { return onlyBatchIDs(o.derivedReaderID) }

func (o *iterNextOp) keys() []*[]byte                     { return nil }
func (o *iterNextOp) diagramKeyRanges() []pebble.KeyRange { return nil }

// iterNextPrefixOp models an Iterator.NextPrefix operation.
type iterNextPrefixOp struct {
	iterID objID

	derivedReaderID objID
}

func (o *iterNextPrefixOp) run(t *Test, h historyRecorder) {
	i := t.getIter(o.iterID)
	valid := i.NextPrefix()
	validStr := validBoolToStr(valid)
	if valid {
		h.Recordf("%s // [%s,%s] %v", o, validStr, iteratorPos(i), i.Error())
	} else {
		h.Recordf("%s // [%s] %v", o, validStr, i.Error())
	}
}

func (o *iterNextPrefixOp) String() string       { return fmt.Sprintf("%s.NextPrefix()", o.iterID) }
func (o *iterNextPrefixOp) receiver() objID      { return o.iterID }
func (o *iterNextPrefixOp) syncObjs() objIDSlice { return onlyBatchIDs(o.derivedReaderID) }

func (o *iterNextPrefixOp) keys() []*[]byte                     { return nil }
func (o *iterNextPrefixOp) diagramKeyRanges() []pebble.KeyRange { return nil }

// iterCanSingleDelOp models a call to CanDeterministicallySingleDelete with an
// Iterator.
type iterCanSingleDelOp struct {
	iterID objID

	derivedReaderID objID
}

func (o *iterCanSingleDelOp) run(t *Test, h historyRecorder) {
	// TODO(jackson): When we perform error injection, we'll need to rethink
	// this.
	_, err := pebble.CanDeterministicallySingleDelete(t.getIter(o.iterID).iter)
	// The return value of CanDeterministicallySingleDelete is dependent on
	// internal LSM state and non-deterministic, so we don't record it.
	// Including the operation within the metamorphic test at all helps ensure
	// that it does not change the result of any other Iterator operation that
	// should be deterministic, regardless of its own outcome.
	//
	// We still record the value of the error because it's deterministic, at
	// least for now. The possible error cases are:
	//  - The iterator was already in an error state when the operation ran.
	//  - The operation is deterministically invalid (like using an InternalNext
	//    to change directions.)
	h.Recordf("%s // %v", o, err)
}

func (o *iterCanSingleDelOp) String() string       { return fmt.Sprintf("%s.InternalNext()", o.iterID) }
func (o *iterCanSingleDelOp) receiver() objID      { return o.iterID }
func (o *iterCanSingleDelOp) syncObjs() objIDSlice { return onlyBatchIDs(o.derivedReaderID) }

func (o *iterCanSingleDelOp) keys() []*[]byte                     { return nil }
func (o *iterCanSingleDelOp) diagramKeyRanges() []pebble.KeyRange { return nil }

// iterPrevOp models an Iterator.Prev[WithLimit] operation.
type iterPrevOp struct {
	iterID objID
	limit  []byte

	derivedReaderID objID
}

func (o *iterPrevOp) run(t *Test, h historyRecorder) {
	i := t.getIter(o.iterID)
	var valid bool
	var validStr string
	if o.limit == nil {
		valid = i.Prev()
		validStr = validBoolToStr(valid)
	} else {
		valid, validStr = validityStateToStr(i.PrevWithLimit(o.limit))
	}
	if valid {
		h.Recordf("%s // [%s,%s] %v", o, validStr, iteratorPos(i), i.Error())
	} else {
		h.Recordf("%s // [%s] %v", o, validStr, i.Error())
	}
}

func (o *iterPrevOp) String() string       { return fmt.Sprintf("%s.Prev(%q)", o.iterID, o.limit) }
func (o *iterPrevOp) receiver() objID      { return o.iterID }
func (o *iterPrevOp) syncObjs() objIDSlice { return onlyBatchIDs(o.derivedReaderID) }

func (o *iterPrevOp) keys() []*[]byte                     { return nil }
func (o *iterPrevOp) diagramKeyRanges() []pebble.KeyRange { return nil }

// newSnapshotOp models a DB.NewSnapshot operation.
type newSnapshotOp struct {
	dbID   objID
	snapID objID
	// If nonempty, this snapshot must not be used to read any keys outside of
	// the provided bounds. This allows some implementations to use 'Eventually
	// file-only snapshots,' which require bounds.
	bounds []pebble.KeyRange
}

func (o *newSnapshotOp) run(t *Test, h historyRecorder) {
	bounds := o.bounds
	if len(bounds) == 0 {
		panic("bounds unexpectedly unset for newSnapshotOp")
	}
	// Fibonacci hash https://probablydance.com/2018/06/16/fibonacci-hashing-the-optimization-that-the-world-forgot-or-a-better-alternative-to-integer-modulo/
	createEfos := ((11400714819323198485 * uint64(t.idx) * t.testOpts.seedEFOS) >> 63) == 1
	// If either of these options is true, an EFOS _must_ be created, regardless
	// of what the fibonacci hash returned.
	excisePossible := t.testOpts.useSharedReplicate || t.testOpts.useExcise
	if createEfos || excisePossible {
		s := t.getDB(o.dbID).NewEventuallyFileOnlySnapshot(bounds)
		t.setSnapshot(o.snapID, s)
		// If the EFOS isn't guaranteed to always create iterators, we must force
		// a flush on this DB so it transitions this EFOS into a file-only snapshot.
		if excisePossible && !t.testOpts.efosAlwaysCreatesIters {
			err := t.getDB(o.dbID).Flush()
			if err != nil {
				h.Recordf("%s // %v", o, err)
			}
		}
	} else {
		s := t.getDB(o.dbID).NewSnapshot()
		t.setSnapshot(o.snapID, s)
	}
	h.Recordf("%s", o)
}

func (o *newSnapshotOp) String() string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s = %s.NewSnapshot(", o.snapID, o.dbID)
	for i := range o.bounds {
		if i > 0 {
			fmt.Fprint(&buf, ", ")
		}
		fmt.Fprintf(&buf, "%q, %q", o.bounds[i].Start, o.bounds[i].End)
	}
	fmt.Fprint(&buf, ")")
	return buf.String()
}
func (o *newSnapshotOp) receiver() objID      { return o.dbID }
func (o *newSnapshotOp) syncObjs() objIDSlice { return []objID{o.snapID} }

func (o *newSnapshotOp) keys() []*[]byte {
	var res []*[]byte
	for i := range o.bounds {
		res = append(res, &o.bounds[i].Start, &o.bounds[i].End)
	}
	return res
}

func (o *newSnapshotOp) diagramKeyRanges() []pebble.KeyRange {
	return o.bounds
}

type dbRatchetFormatMajorVersionOp struct {
	dbID objID
	vers pebble.FormatMajorVersion
}

func (o *dbRatchetFormatMajorVersionOp) run(t *Test, h historyRecorder) {
	var err error
	// NB: We no-op the operation if we're already at or above the provided
	// format major version. Different runs start at different format major
	// versions, making the presence of an error and the error message itself
	// non-deterministic if we attempt to upgrade to an older version.
	//
	//Regardless, subsequent operations should behave identically, which is what
	//we're really aiming to test by including this format major version ratchet
	//operation.
	if t.getDB(o.dbID).FormatMajorVersion() < o.vers {
		err = t.getDB(o.dbID).RatchetFormatMajorVersion(o.vers)
	}
	h.Recordf("%s // %v", o, err)
}

func (o *dbRatchetFormatMajorVersionOp) String() string {
	return fmt.Sprintf("%s.RatchetFormatMajorVersion(%s)", o.dbID, o.vers)
}
func (o *dbRatchetFormatMajorVersionOp) receiver() objID      { return o.dbID }
func (o *dbRatchetFormatMajorVersionOp) syncObjs() objIDSlice { return nil }

func (o *dbRatchetFormatMajorVersionOp) keys() []*[]byte                     { return nil }
func (o *dbRatchetFormatMajorVersionOp) diagramKeyRanges() []pebble.KeyRange { return nil }

type dbRestartOp struct {
	dbID objID

	// affectedObjects is the list of additional objects that are affected by this
	// operation, and which syncObjs() must return so that we don't perform the
	// restart in parallel with other operations to affected objects.
	affectedObjects []objID
}

func (o *dbRestartOp) run(t *Test, h historyRecorder) {
	if err := t.restartDB(o.dbID); err != nil {
		h.Recordf("%s // %v", o, err)
		h.history.err.Store(errors.Wrap(err, "dbRestartOp"))
	} else {
		h.Recordf("%s", o)
	}
}

func (o *dbRestartOp) String() string       { return fmt.Sprintf("%s.Restart()", o.dbID) }
func (o *dbRestartOp) receiver() objID      { return o.dbID }
func (o *dbRestartOp) syncObjs() objIDSlice { return o.affectedObjects }

func (o *dbRestartOp) keys() []*[]byte                     { return nil }
func (o *dbRestartOp) diagramKeyRanges() []pebble.KeyRange { return nil }

func formatOps(ops []op) string {
	var buf strings.Builder
	for _, op := range ops {
		fmt.Fprintf(&buf, "%s\n", op)
	}
	return buf.String()
}

// replicateOp models an operation that could copy keys from one db to
// another through either an IngestAndExcise, or an Ingest.
type replicateOp struct {
	source, dest objID
	start, end   []byte
}

func (r *replicateOp) runSharedReplicate(
	t *Test, h historyRecorder, source, dest *pebble.DB, w *sstable.Writer, sstPath string,
) {
	var sharedSSTs []pebble.SharedSSTMeta
	var err error
	err = source.ScanInternal(context.TODO(), sstable.CategoryAndQoS{}, r.start, r.end,
		func(key *pebble.InternalKey, value pebble.LazyValue, _ pebble.IteratorLevel) error {
			val, _, err := value.Value(nil)
			if err != nil {
				panic(err)
			}
			return w.Add(base.MakeInternalKey(key.UserKey, 0, key.Kind()), val)
		},
		func(start, end []byte, seqNum uint64) error {
			return w.DeleteRange(start, end)
		},
		func(start, end []byte, keys []keyspan.Key) error {
			s := keyspan.Span{
				Start: start,
				End:   end,
				Keys:  keys,
			}
			return rangekey.Encode(&s, w.AddRangeKey)
		},
		func(sst *pebble.SharedSSTMeta) error {
			sharedSSTs = append(sharedSSTs, *sst)
			return nil
		},
	)
	if err != nil {
		h.Recordf("%s // %v", r, err)
		return
	}

	err = w.Close()
	if err != nil {
		h.Recordf("%s // %v", r, err)
		return
	}
	meta, err := w.Metadata()
	if err != nil {
		h.Recordf("%s // %v", r, err)
		return
	}
	if len(sharedSSTs) == 0 && meta.Properties.NumEntries == 0 && meta.Properties.NumRangeKeys() == 0 {
		// IngestAndExcise below will be a no-op. We should do a
		// DeleteRange+RangeKeyDel to mimic the behaviour of the non-shared-replicate
		// case.
		//
		// TODO(bilal): Remove this when we support excises with no matching ingests.
		if err := dest.RangeKeyDelete(r.start, r.end, t.writeOpts); err != nil {
			h.Recordf("%s // %v", r, err)
			return
		}
		err := dest.DeleteRange(r.start, r.end, t.writeOpts)
		h.Recordf("%s // %v", r, err)
		return
	}

	_, err = dest.IngestAndExcise([]string{sstPath}, sharedSSTs, pebble.KeyRange{Start: r.start, End: r.end})
	h.Recordf("%s // %v", r, err)
}

func (r *replicateOp) run(t *Test, h historyRecorder) {
	// Shared replication only works if shared storage is enabled.
	useSharedIngest := t.testOpts.useSharedReplicate
	if !t.testOpts.sharedStorageEnabled {
		useSharedIngest = false
	}

	source := t.getDB(r.source)
	dest := t.getDB(r.dest)
	sstPath := path.Join(t.tmpDir, fmt.Sprintf("ext-replicate%d.sst", t.idx))
	f, err := t.opts.FS.Create(sstPath)
	if err != nil {
		h.Recordf("%s // %v", r, err)
		return
	}
	w := sstable.NewWriter(objstorageprovider.NewFileWritable(f), t.opts.MakeWriterOptions(0, dest.FormatMajorVersion().MaxTableFormat()))

	if useSharedIngest {
		r.runSharedReplicate(t, h, source, dest, w, sstPath)
		return
	}

	// First, do a RangeKeyDelete and DeleteRange on the whole span.
	if err := dest.RangeKeyDelete(r.start, r.end, t.writeOpts); err != nil {
		h.Recordf("%s // %v", r, err)
		return
	}
	if err := dest.DeleteRange(r.start, r.end, t.writeOpts); err != nil {
		h.Recordf("%s // %v", r, err)
		return
	}
	iter, err := source.NewIter(&pebble.IterOptions{
		LowerBound: r.start,
		UpperBound: r.end,
		KeyTypes:   pebble.IterKeyTypePointsAndRanges,
	})
	if err != nil {
		panic(err)
	}
	defer iter.Close()

	for ok := iter.SeekGE(r.start); ok && iter.Error() == nil; ok = iter.Next() {
		hasPoint, hasRange := iter.HasPointAndRange()
		if hasPoint {
			val, err := iter.ValueAndErr()
			if err != nil {
				panic(err)
			}
			if err := w.Set(iter.Key(), val); err != nil {
				panic(err)
			}
		}
		if hasRange && iter.RangeKeyChanged() {
			rangeKeys := iter.RangeKeys()
			rkStart, rkEnd := iter.RangeBounds()

			span := &keyspan.Span{Start: rkStart, End: rkEnd, Keys: make([]keyspan.Key, len(rangeKeys))}
			for i := range rangeKeys {
				span.Keys[i] = keyspan.Key{
					Trailer: base.MakeTrailer(0, base.InternalKeyKindRangeKeySet),
					Suffix:  rangeKeys[i].Suffix,
					Value:   rangeKeys[i].Value,
				}
			}
			keyspan.SortKeysByTrailer(&span.Keys)
			if err := rangekey.Encode(span, w.AddRangeKey); err != nil {
				panic(err)
			}
		}
	}
	if err := iter.Error(); err != nil {
		h.Recordf("%s // %v", r, err)
		return
	}
	if err := w.Close(); err != nil {
		panic(err)
	}

	err = dest.Ingest([]string{sstPath})
	h.Recordf("%s // %v", r, err)
}

func (r *replicateOp) String() string {
	return fmt.Sprintf("%s.Replicate(%s, %q, %q)", r.source, r.dest, r.start, r.end)
}

func (r *replicateOp) receiver() objID      { return r.source }
func (r *replicateOp) syncObjs() objIDSlice { return objIDSlice{r.dest} }

func (r *replicateOp) keys() []*[]byte {
	return []*[]byte{&r.start, &r.end}
}

func (r *replicateOp) diagramKeyRanges() []pebble.KeyRange {
	return []pebble.KeyRange{{Start: r.start, End: r.end}}
}
