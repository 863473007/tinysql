// Copyright 2016 PingCAP, Inc.
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

package executor

import (
	"context"
	"sync"

	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/expression"
	plannercore "github.com/pingcap/tidb/planner/core"
	"github.com/pingcap/tidb/util"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/codec"
)

var _ Executor = &HashJoinExec{}

// HashJoinExec implements the hash join algorithm.
type HashJoinExec struct {
	baseExecutor

	probeSideExec     Executor
	buildSideExec     Executor
	buildSideEstCount float64
	probeSideFilter   expression.CNFExprs
	probeKeys         []*expression.Column
	buildKeys         []*expression.Column

	// concurrency is the number of partition, build and join workers.
	concurrency  uint
	rowContainer *hashRowContainer
	// joinWorkerWaitGroup is for sync multiple join workers.
	joinWorkerWaitGroup sync.WaitGroup
	// closeCh add a lock for closing executor.
	closeCh  chan struct{}
	joinType plannercore.JoinType

	// We build individual joiner for each join worker when use chunk-based
	// execution, to avoid the concurrency of joiner.chk and joiner.selected.
	joiners []joiner

	probeChkResourceCh chan *probeChkResource
	probeResultChs     []chan *chunk.Chunk
	joinChkResourceCh  []chan *chunk.Chunk
	joinResultCh       chan *hashjoinWorkerResult

	prepared bool
}

// probeChkResource stores the result of the join probe side fetch worker,
// `dest` is for Chunk reuse: after join workers process the probe side chunk which is read from `dest`,
// they'll store the used chunk as `chk`, and then the probe side fetch worker will put new data into `chk` and write `chk` into dest.
type probeChkResource struct {
	chk  *chunk.Chunk
	dest chan<- *chunk.Chunk
}

// hashjoinWorkerResult stores the result of join workers,
// `src` is for Chunk reuse: the main goroutine will get the join result chunk `chk`,
// and push `chk` into `src` after processing, join worker goroutines get the empty chunk from `src`
// and push new data into this chunk.
type hashjoinWorkerResult struct {
	chk *chunk.Chunk
	err error
	src chan<- *chunk.Chunk
}

// Close implements the Executor Close interface.
func (e *HashJoinExec) Close() error {
	close(e.closeCh)
	if e.prepared {
		if e.joinResultCh != nil {
			for range e.joinResultCh {
			}
		}
		if e.probeChkResourceCh != nil {
			close(e.probeChkResourceCh)
			for range e.probeChkResourceCh {
			}
		}
		for i := range e.probeResultChs {
			for range e.probeResultChs[i] {
			}
		}
		for i := range e.joinChkResourceCh {
			close(e.joinChkResourceCh[i])
			for range e.joinChkResourceCh[i] {
			}
		}
		e.probeChkResourceCh = nil
		e.joinChkResourceCh = nil
	}
	err := e.baseExecutor.Close()
	return err
}

// Open implements the Executor Open interface.
func (e *HashJoinExec) Open(ctx context.Context) error {
	if err := e.baseExecutor.Open(ctx); err != nil {
		return err
	}

	e.prepared = false
	e.closeCh = make(chan struct{})
	e.joinWorkerWaitGroup = sync.WaitGroup{}
	return nil
}

// Next implements the Executor Next interface.
// hash join constructs the result following these steps:
// step 1. fetch data from build side child and build a hash table;
// step 2. fetch data from probe child in a background goroutine and probe the hash table in multiple join workers.
func (e *HashJoinExec) Next(ctx context.Context, req *chunk.Chunk) (err error) {
	if !e.prepared {
		err := e.fetchAndBuildHashTable(ctx)
		if err != nil {
			return err
		}
		e.fetchAndProbeHashTable(ctx)
		e.prepared = true
	}
	req.Reset()

	result, ok := <-e.joinResultCh
	if !ok {
		return nil
	}
	if result.err != nil {
		return result.err
	}
	req.SwapColumns(result.chk)
	result.src <- result.chk
	return nil
}

func (e *HashJoinExec) fetchAndBuildHashTable(ctx context.Context) error {
	buildKeyColIdx := make([]int, len(e.buildKeys))
	for i := range e.buildKeys {
		buildKeyColIdx[i] = e.buildKeys[i].Index
	}
	allTypes := e.buildSideExec.base().retFieldTypes
	hCtx := &hashContext{
		allTypes:  allTypes,
		keyColIdx: buildKeyColIdx,
	}
	initList := chunk.NewList(allTypes, e.initCap, e.maxChunkSize)
	e.rowContainer = newHashRowContainer(e.ctx, int(e.buildSideEstCount), hCtx, initList)

	for {
		chk := chunk.NewChunkWithCapacity(e.buildSideExec.base().retFieldTypes, e.ctx.GetSessionVars().MaxChunkSize)
		err := Next(ctx, e.buildSideExec, chk)
		if err != nil {
			return err
		}
		if chk.NumRows() == 0 {
			return nil
		}
		err = e.rowContainer.PutChunk(chk)
		if err != nil {
			return err
		}
	}
}

func (e *HashJoinExec) initializeForProbe() {
	// e.probeResultChs is for transmitting the chunks which store the data of
	// probeSideExec, it'll be written by probe side worker goroutine, and read by join
	// workers.
	e.probeResultChs = make([]chan *chunk.Chunk, e.concurrency)
	for i := uint(0); i < e.concurrency; i++ {
		e.probeResultChs[i] = make(chan *chunk.Chunk, 1)
	}

	// e.probeChkResourceCh is for transmitting the used probeSideExec chunks from
	// join workers to probeSideExec worker.
	e.probeChkResourceCh = make(chan *probeChkResource, e.concurrency)
	for i := uint(0); i < e.concurrency; i++ {
		e.probeChkResourceCh <- &probeChkResource{
			chk:  newFirstChunk(e.probeSideExec),
			dest: e.probeResultChs[i],
		}
	}

	// e.joinChkResourceCh is for transmitting the reused join result chunks
	// from the main thread to join worker goroutines.
	e.joinChkResourceCh = make([]chan *chunk.Chunk, e.concurrency)
	for i := uint(0); i < e.concurrency; i++ {
		e.joinChkResourceCh[i] = make(chan *chunk.Chunk, 1)
		e.joinChkResourceCh[i] <- newFirstChunk(e)
	}

	// e.joinResultCh is for transmitting the join result chunks to the main
	// thread.
	e.joinResultCh = make(chan *hashjoinWorkerResult, e.concurrency+1)
}

// fetchProbeSideChunks get chunks from fetches chunks from the big table in a background goroutine
// and sends the chunks to multiple channels which will be read by multiple join workers.
func (e *HashJoinExec) fetchProbeSideChunks(ctx context.Context) {
	for {
		var probeSideResource *probeChkResource
		var ok bool
		select {
		case <-e.closeCh:
			return
		case probeSideResource, ok = <-e.probeChkResourceCh:
			if !ok {
				return
			}
		}
		probeSideResult := probeSideResource.chk
		err := Next(ctx, e.probeSideExec, probeSideResult)
		if err != nil {
			e.joinResultCh <- &hashjoinWorkerResult{
				err: err,
			}
			return
		}

		if probeSideResult.NumRows() == 0 {
			return
		}

		probeSideResource.dest <- probeSideResult
	}
}

func (e *HashJoinExec) fetchAndProbeHashTable(ctx context.Context) {
	e.initializeForProbe()
	e.joinWorkerWaitGroup.Add(1)
	go util.WithRecovery(func() { e.fetchProbeSideChunks(ctx) }, e.handleProbeSideFetcherPanic)

	probeKeyColIdx := make([]int, len(e.probeKeys))
	for i := range e.probeKeys {
		probeKeyColIdx[i] = e.probeKeys[i].Index
	}

	// Start e.concurrency join workers to probe hash table and join build side and
	// probe side rows.
	for i := uint(0); i < e.concurrency; i++ {
		e.joinWorkerWaitGroup.Add(1)
		workID := i
		go util.WithRecovery(func() { e.runJoinWorker(workID, probeKeyColIdx) }, e.handleJoinWorkerPanic)
	}
	go util.WithRecovery(e.waitJoinWorkersAndCloseResultChan, nil)
}

func (e *HashJoinExec) runJoinWorker(workerID uint, probeKeyColIdx []int) {
	var (
		probeSideResult *chunk.Chunk
		selected        = make([]bool, 0, chunk.InitialCapacity)
	)
	ok, joinResult := e.getNewJoinResult(workerID)
	if !ok {
		return
	}

	// Read and filter probeSideResult, and join the probeSideResult with the build side rows.
	emptyProbeSideResult := &probeChkResource{
		dest: e.probeResultChs[workerID],
	}
	hCtx := &hashContext{
		allTypes:  retTypes(e.probeSideExec),
		keyColIdx: probeKeyColIdx,
	}
	for ok := true; ok; {
		select {
		case <-e.closeCh:
			return
		case probeSideResult, ok = <-e.probeResultChs[workerID]:
		}
		if !ok {
			break
		}
		ok, joinResult = e.join2Chunk(workerID, probeSideResult, hCtx, joinResult, selected)
		if !ok {
			break
		}
		probeSideResult.Reset()
		emptyProbeSideResult.chk = probeSideResult
		e.probeChkResourceCh <- emptyProbeSideResult
	}
	if joinResult == nil {
		return
	} else if joinResult.err != nil || (joinResult.chk != nil && joinResult.chk.NumRows() > 0) {
		e.joinResultCh <- joinResult
	}
}

func (e *HashJoinExec) getNewJoinResult(workerID uint) (bool, *hashjoinWorkerResult) {
	joinResult := &hashjoinWorkerResult{
		src: e.joinChkResourceCh[workerID],
	}
	ok := true
	select {
	case <-e.closeCh:
		ok = false
	case joinResult.chk, ok = <-e.joinChkResourceCh[workerID]:
	}
	return ok, joinResult
}

func (e *HashJoinExec) waitJoinWorkersAndCloseResultChan() {
	e.joinWorkerWaitGroup.Wait()
	close(e.joinResultCh)
}

func (e *HashJoinExec) handleProbeSideFetcherPanic(r interface{}) {
	for i := range e.probeResultChs {
		close(e.probeResultChs[i])
	}
	if r != nil {
		e.joinResultCh <- &hashjoinWorkerResult{err: errors.Errorf("%v", r)}
	}
	e.joinWorkerWaitGroup.Done()
}

func (e *HashJoinExec) handleJoinWorkerPanic(r interface{}) {
	if r != nil {
		e.joinResultCh <- &hashjoinWorkerResult{err: errors.Errorf("%v", r)}
	}
	e.joinWorkerWaitGroup.Done()
}

func (e *HashJoinExec) joinMatchedProbeSideRow2Chunk(workerID uint, probeKey uint64, probeSideRow chunk.Row, hCtx *hashContext,
	joinResult *hashjoinWorkerResult) (bool, *hashjoinWorkerResult) {
	buildSideRows, err := e.rowContainer.GetMatchedRows(probeKey, probeSideRow, hCtx)
	if err != nil {
		joinResult.err = err
		return false, joinResult
	}
	if len(buildSideRows) == 0 {
		e.joiners[workerID].onMissMatch(probeSideRow, joinResult.chk)
		return true, joinResult
	}
	iter := chunk.NewIterator4Slice(buildSideRows)
	hasMatch := false
	for iter.Begin(); iter.Current() != iter.End(); {
		matched, _, err := e.joiners[workerID].tryToMatchInners(probeSideRow, iter, joinResult.chk)
		if err != nil {
			joinResult.err = err
			return false, joinResult
		}
		hasMatch = hasMatch || matched

		if joinResult.chk.IsFull() {
			e.joinResultCh <- joinResult
			ok, joinResult := e.getNewJoinResult(workerID)
			if !ok {
				return false, joinResult
			}
		}
	}
	if !hasMatch {
		e.joiners[workerID].onMissMatch(probeSideRow, joinResult.chk)
	}
	return true, joinResult
}

func (e *HashJoinExec) join2Chunk(workerID uint, probeSideChk *chunk.Chunk, hCtx *hashContext, joinResult *hashjoinWorkerResult,
	selected []bool) (ok bool, _ *hashjoinWorkerResult) {
	var err error
	selected, err = expression.VectorizedFilter(e.ctx, e.probeSideFilter, chunk.NewIterator4Chunk(probeSideChk), selected)
	if err != nil {
		joinResult.err = err
		return false, joinResult
	}

	hCtx.initHash(probeSideChk.NumRows())
	for _, i := range hCtx.keyColIdx {
		err = codec.HashChunkSelected(e.rowContainer.sc, hCtx.hashVals, probeSideChk, hCtx.allTypes[i], i, hCtx.buf, hCtx.hasNull, selected)
		if err != nil {
			joinResult.err = err
			return false, joinResult
		}
	}

	for i := range selected {
		if !selected[i] || hCtx.hasNull[i] { // process unmatched probe side rows
			e.joiners[workerID].onMissMatch(probeSideChk.GetRow(i), joinResult.chk)
		} else { // process matched probe side rows
			probeKey, probeRow := hCtx.hashVals[i].Sum64(), probeSideChk.GetRow(i)
			ok, joinResult = e.joinMatchedProbeSideRow2Chunk(workerID, probeKey, probeRow, hCtx, joinResult)
			if !ok {
				return false, joinResult
			}
		}
		if joinResult.chk.IsFull() {
			e.joinResultCh <- joinResult
			ok, joinResult = e.getNewJoinResult(workerID)
			if !ok {
				return false, joinResult
			}
		}
	}
	return true, joinResult
}
