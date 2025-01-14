package retriever

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/filecoin-project/lassie/pkg/events"
	"github.com/filecoin-project/lassie/pkg/retriever/prioritywaitqueue"
	"github.com/filecoin-project/lassie/pkg/types"
	"github.com/ipfs/go-cid"
	"github.com/ipni/go-libipni/metadata"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multicodec"
	"go.uber.org/multierr"
)

type GetStorageProviderTimeout func(peer peer.ID) time.Duration

// TransportProtocol implements the protocol-specific portions of a parallel-
// peer retriever. It is responsible for communicating with individual peers
// and also bears responsibility for some of the peer-selection logic.
type TransportProtocol interface {
	Code() multicodec.Code
	GetMergedMetadata(cid cid.Cid, currentMetadata, newMetadata metadata.Protocol) metadata.Protocol
	Connect(ctx context.Context, retrieval *retrieval, phaseStartTime time.Time, candidate types.RetrievalCandidate) (time.Duration, error)
	Retrieve(
		ctx context.Context,
		retrieval *retrieval,
		shared *retrievalShared,
		phaseStartTime time.Time,
		timeout time.Duration,
		candidate types.RetrievalCandidate,
	) (*types.RetrievalStats, error)
}

var _ types.CandidateRetriever = (*parallelPeerRetriever)(nil)
var _ types.CandidateRetrieval = (*retrieval)(nil)

// parallelPeerRetriever is an abstract utility type that implements a retrieval
// flow that retrieves from multiple peers separately but needs to manage that
// flow in parallel. Unlike a Bitswap retrieval, in which all peers are managed
// as a group and may all be collectively retrieved from, parallelPeerRetriever
// is used for protocols where a retrieval is performed directly with a single
// peer, but many peers may be prioritised for attempts.
//
// The concrete implementation of the retrieval protocol is provided by the
// TransportProtocol interface. parallelPeerRetriever manages candidates and
// the parallel+serial flow of connect+retrieve.
type parallelPeerRetriever struct {
	Protocol          TransportProtocol
	Session           Session
	Clock             clock.Clock
	QueueInitialPause time.Duration

	// this is purely for testing purposes, to ensure that we receive all candidates
	awaitReceivedCandidates chan<- struct{}
}

// retrieval handles state on a per-retrieval (across multiple candidates) basis
type retrieval struct {
	*parallelPeerRetriever
	ctx                context.Context
	request            types.RetrievalRequest
	eventsCallback     func(types.RetrievalEvent)
	candidateMetadata  map[peer.ID]metadata.Protocol
	candidateMetdataLk sync.RWMutex
}

type retrievalResult struct {
	PeerID     peer.ID
	PhaseStart time.Time
	Stats      *types.RetrievalStats
	Event      *types.RetrievalEvent
	Err        error
}

// retrievalShared is the shared state and coordination between the per-SP
// retrieval goroutines.
type retrievalShared struct {
	waitQueue  prioritywaitqueue.PriorityWaitQueue[peer.ID]
	resultChan chan retrievalResult
	finishChan chan struct{}
}

// canSendResult will indicate whether a result is likely to be accepted (true)
// or whether the retrieval is already finished (likely by a success)
func (shared *retrievalShared) canSendResult() bool {
	select {
	case <-shared.finishChan:
		return false
	default:
	}
	return true
}

// sendResult will only send a result to the parent goroutine if a retrieval has
// finished (likely by a success), otherwise it will send the result
func (shared *retrievalShared) sendResult(result retrievalResult) bool {
	select {
	case <-shared.finishChan:
		return false
	case shared.resultChan <- result:
		if result.Stats != nil {
			// signals to goroutines to bail, this has to be done here, rather than on
			// the receiving parent end, because immediately after this call we instruct
			// the prioritywaitqueue that we're done and another may start
			close(shared.finishChan)
		}
	}
	return true
}

func (shared *retrievalShared) sendEvent(event types.RetrievalEvent) {
	shared.sendResult(retrievalResult{PeerID: event.StorageProviderId(), Event: &event})
}

func (cfg *parallelPeerRetriever) Retrieve(
	ctx context.Context,
	retrievalRequest types.RetrievalRequest,
	eventsCallback func(types.RetrievalEvent),
) types.CandidateRetrieval {

	if eventsCallback == nil {
		eventsCallback = func(re types.RetrievalEvent) {}
	}
	return &retrieval{
		parallelPeerRetriever: cfg,
		ctx:                   ctx,
		request:               retrievalRequest,
		eventsCallback:        eventsCallback,
		candidateMetadata:     make(map[peer.ID]metadata.Protocol),
	}
}

func (retrieval *retrieval) RetrieveFromAsyncCandidates(asyncCandidates types.InboundAsyncCandidates) (*types.RetrievalStats, error) {
	ctx, cancelCtx := context.WithCancel(retrieval.ctx)

	pwqOpts := []prioritywaitqueue.Option[peer.ID]{prioritywaitqueue.WithClock[peer.ID](retrieval.Clock)}
	if retrieval.QueueInitialPause > 0 {
		pwqOpts = append(pwqOpts, prioritywaitqueue.WithInitialPause[peer.ID](retrieval.QueueInitialPause))
	}

	shared := &retrievalShared{
		resultChan: make(chan retrievalResult),
		finishChan: make(chan struct{}),
		waitQueue:  prioritywaitqueue.New(retrieval.candidateCompare, pwqOpts...),
	}

	// start retrievals
	phaseStartTime := retrieval.Clock.Now()
	var waitGroup sync.WaitGroup
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		for {
			active, candidates, err := retrieval.filterCandidates(ctx, asyncCandidates)
			if !active || err != nil {
				return
			}
			for _, candidate := range candidates {
				// start the retrieval with the candidate
				candidate := candidate
				waitGroup.Add(1)
				go func() {
					defer waitGroup.Done()
					retrieval.runRetrievalCandidate(ctx, shared, phaseStartTime, candidate)
				}()
			}
		}
	}()

	finishAll := make(chan struct{}, 1)
	go func() {
		waitGroup.Wait()
		close(shared.resultChan)
		finishAll <- struct{}{}
	}()

	stats, err := collectResults(ctx, shared, retrieval.eventsCallback)
	cancelCtx()
	// optimistically try to wait for all routines to finish
	select {
	case <-finishAll:
	case <-time.After(100 * time.Millisecond):
		logger.Warn("Unable to successfully cancel all retrieval attempts withing 100ms")
	}
	return stats, err
}

// candidateCompare compares two peer.IDs and returns true if the first is
// preferable to the second. This is used for the PriorityWaitQueue that will
// prioritise execution of retrievals if two candidates are available to compare
// at the same time.
func (retrieval *retrieval) candidateCompare(a, b peer.ID) bool {
	retrieval.candidateMetdataLk.RLock()
	defer retrieval.candidateMetdataLk.RUnlock()

	mdA, ok := retrieval.candidateMetadata[a]
	if !ok {
		return false
	}
	mdB, ok := retrieval.candidateMetadata[b]
	if !ok {
		return true
	}

	return retrieval.Session.CompareStorageProviders(retrieval.Protocol.Code(), a, b, mdA, mdB)
}

// filterCandidates is needed because we can receive duplicate candidates in
// a single batch or across different batches. We need to filter out duplicates
// and make sure we have the best information from candidate metadata across
// those duplicates.
func (retrieval *retrieval) filterCandidates(ctx context.Context, asyncCandidates types.InboundAsyncCandidates) (bool, []types.RetrievalCandidate, error) {
	filtered := make([]types.RetrievalCandidate, 0)
	active, candidates, err := asyncCandidates.Next(ctx)
	if !active || err != nil {
		if retrieval.awaitReceivedCandidates != nil {
			select {
			case <-retrieval.ctx.Done():
			case retrieval.awaitReceivedCandidates <- struct{}{}:
			}
		}
		return false, nil, err
	}

	retrieval.candidateMetdataLk.Lock()
	defer retrieval.candidateMetdataLk.Unlock()

	for _, candidate := range candidates {
		// update or add new candidate metadata
		currMetadata, seenCandidate := retrieval.candidateMetadata[candidate.MinerPeer.ID]
		newMetadata := candidate.Metadata.Get(multicodec.Code(retrieval.Protocol.Code()))
		candidateMetadata := retrieval.Protocol.GetMergedMetadata(retrieval.request.Cid, currMetadata, newMetadata)
		retrieval.candidateMetadata[candidate.MinerPeer.ID] = candidateMetadata
		// if it's a new candidate, include it, otherwise don't start a new retrieval for it
		if !seenCandidate {
			filtered = append(filtered, candidate)
		}
	}

	return true, filtered, nil
}

// collectResults is responsible for receiving query errors, retrieval errors
// and retrieval results and aggregating into an appropriate return of either
// a complete RetrievalStats or an bundled multi-error
func collectResults(ctx context.Context, shared *retrievalShared, eventsCallback func(types.RetrievalEvent)) (*types.RetrievalStats, error) {
	var retrievalErrors error
	for {
		select {
		case result, ok := <-shared.resultChan:
			// have we got all responses but no success?
			if !ok {
				// we failed, and got only retrieval errors
				retrievalErrors = multierr.Append(retrievalErrors, ErrAllRetrievalsFailed)
				return nil, retrievalErrors
			}

			if result.Event != nil {
				eventsCallback(*result.Event)
				break
			}
			if result.Err != nil {
				retrievalErrors = multierr.Append(retrievalErrors, result.Err)
			}
			if result.Stats != nil {
				return result.Stats, nil
			}
		case <-ctx.Done():
			return nil, context.Canceled
		}
	}
}

// runRetrievalCandidate is a singular CID:SP retrieval, expected to be run in a goroutine
// and coordinate with other candidate retrievals to block after query phase and
// only attempt one retrieval-proper at a time.
func (retrieval *retrieval) runRetrievalCandidate(
	ctx context.Context,
	shared *retrievalShared,
	phaseStartTime time.Time,
	candidate types.RetrievalCandidate,
) {

	timeout := retrieval.Session.GetStorageProviderTimeout(candidate.MinerPeer.ID)

	var stats *types.RetrievalStats
	var retrievalErr error
	var done func()

	shared.sendEvent(events.Started(retrieval.parallelPeerRetriever.Clock.Now(), retrieval.request.RetrievalID, phaseStartTime, types.RetrievalPhase, candidate))
	connectCtx := ctx
	if timeout != 0 {
		var timeoutFunc func()
		connectCtx, timeoutFunc = retrieval.parallelPeerRetriever.Clock.WithDeadline(ctx, retrieval.parallelPeerRetriever.Clock.Now().Add(timeout))
		defer timeoutFunc()
	}

	// Setup in parallel
	connectTime, err := retrieval.Protocol.Connect(connectCtx, retrieval, phaseStartTime, candidate)
	if err != nil {
		if ctx.Err() == nil { // not cancelled, maybe timed out though
			logger.Warnf("Failed to connect to SP %s: %v", candidate.MinerPeer.ID, err)
			retrievalErr = fmt.Errorf("%w: %v", ErrConnectFailed, err)
			shared.sendEvent(events.Failed(retrieval.parallelPeerRetriever.Clock.Now(), retrieval.request.RetrievalID, phaseStartTime, types.RetrievalPhase, candidate, retrievalErr.Error()))
		}
	} else {
		shared.sendEvent(events.Connected(retrieval.parallelPeerRetriever.Clock.Now(), retrieval.request.RetrievalID, phaseStartTime, types.RetrievalPhase, candidate))

		retrieval.Session.RegisterConnectTime(candidate.MinerPeer.ID, connectTime)

		// Form a queue and run retrieval in serial
		done = shared.waitQueue.Wait(candidate.MinerPeer.ID)

		if shared.canSendResult() { // move on to retrieval phase
			stats, retrievalErr = retrieval.Protocol.Retrieve(ctx, retrieval, shared, phaseStartTime, timeout, candidate)

			if retrievalErr != nil {
				msg := retrievalErr.Error()
				if errors.Is(retrievalErr, ErrRetrievalTimedOut) {
					msg = fmt.Sprintf("timeout after %s", timeout)
				}
				shared.sendEvent(events.Failed(retrieval.parallelPeerRetriever.Clock.Now(), retrieval.request.RetrievalID, phaseStartTime, types.RetrievalPhase, candidate, msg))
			} else {
				shared.sendEvent(events.Success(
					retrieval.parallelPeerRetriever.Clock.Now(),
					retrieval.request.RetrievalID,
					phaseStartTime,
					candidate,
					stats.Size,
					stats.Blocks,
					stats.Duration,
					stats.TotalPayment,
					0,
				),
				)
			}
		} // else we didn't get to retrieval phase because we were cancelled
	}

	if shared.canSendResult() {
		if retrievalErr != nil {
			if ctx.Err() != nil { // cancelled, don't report the error
				shared.sendResult(retrievalResult{PhaseStart: phaseStartTime, PeerID: candidate.MinerPeer.ID})
			} else {
				// an error of some kind to report
				shared.sendResult(retrievalResult{PhaseStart: phaseStartTime, PeerID: candidate.MinerPeer.ID, Err: retrievalErr})
			}
		} else { // success, we have stats and no errors
			shared.sendResult(retrievalResult{PhaseStart: phaseStartTime, PeerID: candidate.MinerPeer.ID, Stats: stats})
		}
	} // else nothing to do, we were cancelled

	if done != nil {
		done() // allow prioritywaitqueue to move on to next candidate
	}
}
