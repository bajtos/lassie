package retriever

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/lassie/pkg/events"
	"github.com/filecoin-project/lassie/pkg/types"
	"github.com/filecoin-project/lassie/pkg/verifiedcar"
	"github.com/ipfs/go-cid"
	"github.com/ipld/go-ipld-prime/traversal/selector"
	"github.com/ipni/go-libipni/metadata"
	"github.com/multiformats/go-multicodec"
)

var (
	ErrHttpSelectorRequest = errors.New("HTTP retrieval for an explicit selector request")
	ErrNoHttpForPeer       = errors.New("no HTTP url for peer")
	ErrBadPathForRequest   = errors.New("bad path for request")
)

type ErrHttpRequestFailure struct {
	Code int
}

func (e ErrHttpRequestFailure) Error() string {
	return fmt.Sprintf("HTTP request failed, remote response code: %d", e.Code)
}

const HttpDefaultInitialWait time.Duration = 0

const DefaultUserAgent = "lassie"

var _ TransportProtocol = &ProtocolHttp{}

type ProtocolHttp struct {
	Client *http.Client
}

// NewHttpRetriever makes a new CandidateRetriever for verified CAR HTTP
// retrievals (transport-ipfs-gateway-http).
func NewHttpRetriever(session Session, client *http.Client) types.CandidateRetriever {
	return NewHttpRetrieverWithDeps(session, client, clock.New(), nil, HttpDefaultInitialWait)
}

func NewHttpRetrieverWithDeps(
	session Session,
	client *http.Client,
	clock clock.Clock,
	awaitReceivedCandidates chan<- struct{},
	initialPause time.Duration,
) types.CandidateRetriever {
	return &parallelPeerRetriever{
		Protocol:                &ProtocolHttp{Client: client},
		Session:                 session,
		Clock:                   clock,
		QueueInitialPause:       initialPause,
		awaitReceivedCandidates: awaitReceivedCandidates,
	}
}

func (ph ProtocolHttp) Code() multicodec.Code {
	return multicodec.TransportIpfsGatewayHttp
}

func (ph ProtocolHttp) GetMergedMetadata(cid cid.Cid, currentMetadata, newMetadata metadata.Protocol) metadata.Protocol {
	return &metadata.IpfsGatewayHttp{}
}

func (ph *ProtocolHttp) Connect(ctx context.Context, retrieval *retrieval, phaseStartTime time.Time, candidate types.RetrievalCandidate) (time.Duration, error) {
	// We could begin the request here by moving ph.beginRequest() to this function.
	// That would result in parallel connections to candidates as they are received,
	// then serial reading of bodies.
	// If/when we need to share connection state between a Connect() and Retrieve()
	// call, we'll need a shared state that we can pass - either return a Context
	// here that we pick up in Retrieve, or have something on `retrieval` that can
	// be keyed by `candidate` to do this; or similar. ProtocolHttp is not
	// per-connection, it's per-protocol, and `retrieval` is not per-candidate
	// either, it's per-retrieval.
	return 0, nil
}

func (ph *ProtocolHttp) Retrieve(
	ctx context.Context,
	retrieval *retrieval,
	shared *retrievalShared,
	phaseStartTime time.Time,
	timeout time.Duration,
	candidate types.RetrievalCandidate,
) (*types.RetrievalStats, error) {
	// Connect and read body in one flow, we can move ph.beginRequest() to Connect()
	// to parallelise connections if we have confidence in not wasting server time
	// by requesting but not reading bodies (or delayed reading which may result in
	// timeouts).
	resp, err := ph.beginRequest(ctx, retrieval.request, candidate)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, ErrHttpRequestFailure{Code: resp.StatusCode}
	}
	var ttfb time.Duration
	rdr := newTimeToFirstByteReader(resp.Body, func() {
		ttfb = retrieval.Clock.Since(phaseStartTime)
		shared.sendEvent(events.FirstByte(retrieval.Clock.Now(), retrieval.request.RetrievalID, phaseStartTime, candidate))
	})

	sel, err := selector.CompileSelector(retrieval.request.GetSelector())
	if err != nil {
		return nil, err
	}

	cfg := verifiedcar.Config{
		Root:     retrieval.request.Cid,
		Selector: sel,
	}

	blockCount, byteCount, err := cfg.Verify(ctx, rdr, retrieval.request.LinkSystem)
	if err != nil {
		return nil, err
	}

	duration := retrieval.Clock.Since(phaseStartTime)
	speed := uint64(float64(byteCount) / duration.Seconds())

	return &types.RetrievalStats{
		RootCid:           candidate.RootCid,
		StorageProviderId: candidate.MinerPeer.ID,
		Size:              byteCount,
		Blocks:            blockCount,
		Duration:          duration,
		AverageSpeed:      speed,
		TotalPayment:      big.Zero(),
		NumPayments:       0,
		AskPrice:          big.Zero(),
		TimeToFirstByte:   ttfb,
	}, nil
}

func (ph *ProtocolHttp) beginRequest(ctx context.Context, request types.RetrievalRequest, candidate types.RetrievalCandidate) (resp *http.Response, err error) {
	var req *http.Request
	req, err = makeRequest(ctx, request, candidate)
	if err == nil {
		resp, err = ph.Client.Do(req)
	}
	return resp, err
}

func makeRequest(ctx context.Context, request types.RetrievalRequest, candidate types.RetrievalCandidate) (*http.Request, error) {
	candidateURL, err := candidate.ToURL()
	if err != nil {
		logger.Warnf("Couldn't construct a url for miner %s: %v", candidate.MinerPeer.ID, err)
		return nil, fmt.Errorf("%w: %v", ErrNoHttpForPeer, err)
	}

	path, err := request.GetUrlPath()
	if err != nil {
		logger.Warnf("Couldn't construct a url path for request: %v", err)
		return nil, fmt.Errorf("%w: %v", ErrBadPathForRequest, err)
	}

	reqURL := fmt.Sprintf("%s/ipfs/%s%s", candidateURL, request.Cid, path)
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		logger.Warnf("Couldn't construct a http request %s: %v", candidate.MinerPeer.ID, err)
		return nil, fmt.Errorf("%w for peer %s: %v", ErrBadPathForRequest, candidate.MinerPeer.ID, err)
	}
	req.Header.Add("Accept", request.Scope.AcceptHeader())
	req.Header.Add("User-Agent", DefaultUserAgent)
	req.Header.Add("X-Request-Id", request.RetrievalID.String())

	return req, nil
}

var _ io.Reader = (*timeToFirstByteReader)(nil)

type timeToFirstByteReader struct {
	r     io.Reader
	first bool
	cb    func()
}

func newTimeToFirstByteReader(r io.Reader, cb func()) *timeToFirstByteReader {
	return &timeToFirstByteReader{
		r:  r,
		cb: cb,
	}
}

func (t *timeToFirstByteReader) Read(p []byte) (n int, err error) {
	if !t.first {
		t.first = true
		defer t.cb()
	}
	return t.r.Read(p)
}
