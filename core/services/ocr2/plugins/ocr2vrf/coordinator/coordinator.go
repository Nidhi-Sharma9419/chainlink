package coordinator

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/pkg/errors"
	"github.com/smartcontractkit/ocr2vrf/dkg"
	ocr2vrftypes "github.com/smartcontractkit/ocr2vrf/types"

	evmclient "github.com/smartcontractkit/chainlink/core/chains/evm/client"
	"github.com/smartcontractkit/chainlink/core/chains/evm/logpoller"
	evmtypes "github.com/smartcontractkit/chainlink/core/chains/evm/types"
	dkg_wrapper "github.com/smartcontractkit/chainlink/core/gethwrappers/ocr2vrf/generated/dkg"
	vrf_wrapper "github.com/smartcontractkit/chainlink/core/gethwrappers/ocr2vrf/generated/vrf_beacon_coordinator"
	"github.com/smartcontractkit/chainlink/core/logger"
	"github.com/smartcontractkit/chainlink/core/services/pg"
)

var _ ocr2vrftypes.CoordinatorInterface = &coordinator{}

var (
	dkgABI = evmtypes.MustGetABI(dkg_wrapper.DKGMetaData.ABI)
	vrfABI = evmtypes.MustGetABI(vrf_wrapper.VRFBeaconCoordinatorMetaData.ABI)
)

const (
	// VRF-only events.
	randomnessRequestedEvent            string = "RandomnessRequested"
	randomnessFulfillmentRequestedEvent string = "RandomnessFulfillmentRequested"
	randomWordsFulfilledEvent           string = "RandomWordsFulfilled"
	newTransmissionEvent                string = "NewTransmission"

	// Both VRF and DKG contracts emit this, it's an OCR event.
	configSetEvent = "ConfigSet"

	// TODO: Add these to the off-chain config.
	cacheEvictionWindowSeconds = 60
	batchGasLimit              = int64(5_000_000)
	blockGasOverhead           = int64(50_000)
	callbackGasOverhead        = int64(50_000)
	coordinatorOverhead        = int64(200_000)
)

// block is used to key into a set that tracks beacon blocks.
type block struct {
	blockNumber uint64
	confDelay   uint32
}

type blockInReport struct {
	block
	recentBlockHeight uint64
	recentBlockHash   common.Hash
}

type callback struct {
	blockNumber uint64
	requestID   uint64
}

type callbackInReport struct {
	callback
	recentBlockHeight uint64
	recentBlockHash   common.Hash
}

type coordinator struct {
	lggr logger.Logger

	lp logpoller.LogPoller
	topics
	lookbackBlocks int64
	finalityDepth  uint32

	coordinatorContract VRFBeaconCoordinator
	coordinatorAddress  common.Address

	// We need to keep track of DKG ConfigSet events as well.
	dkgAddress common.Address

	evmClient evmclient.Client

	// set of blocks that have been scheduled for transmission.
	toBeTransmittedBlocks *blockCache[blockInReport]
	// set of request id's that have been scheduled for transmission.
	toBeTransmittedCallbacks *blockCache[callbackInReport]
	transmittedMu sync.Mutex
}

// New creates a new CoordinatorInterface implementor.
func New(
	lggr logger.Logger,
	coordinatorAddress common.Address,
	dkgAddress common.Address,
	client evmclient.Client,
	lookbackBlocks int64,
	logPoller logpoller.LogPoller,
	finalityDepth uint32,
) (ocr2vrftypes.CoordinatorInterface, error) {
	coordinatorContract, err := vrf_wrapper.NewVRFBeaconCoordinator(coordinatorAddress, client)
	if err != nil {
		return nil, errors.Wrap(err, "coordinator wrapper creation")
	}

	t := newTopics()

	// Add log filters for the log poller so that it can poll and find the logs that
	// we need.
	err = logPoller.MergeFilter([]common.Hash{
		t.randomnessRequestedTopic,
		t.randomnessFulfillmentRequestedTopic,
		t.randomWordsFulfilledTopic,
		t.configSetTopic,
		t.newTransmissionTopic}, []common.Address{coordinatorAddress, dkgAddress})
	if err != nil {
		return nil, err
	}

	return &coordinator{
		coordinatorContract:      coordinatorContract,
		coordinatorAddress:       coordinatorAddress,
		dkgAddress:               dkgAddress,
		lp:                       logPoller,
		topics:                   t,
		lookbackBlocks:           lookbackBlocks,
		finalityDepth:            finalityDepth,
		evmClient:                client,
		lggr:                     lggr.Named("OCR2VRFCoordinator"),
		transmittedMu:            sync.Mutex{},
		toBeTransmittedBlocks:    NewBlockCache[blockInReport](cacheEvictionWindowSeconds),
		toBeTransmittedCallbacks: NewBlockCache[callbackInReport](cacheEvictionWindowSeconds),
	}, nil
}

// ReportIsOnchain returns true iff a report for the given OCR epoch/round is
// present onchain.
func (c *coordinator) ReportIsOnchain(ctx context.Context, epoch uint32, round uint8) (presentOnchain bool, err error) {
	// Check if a NewTransmission event was emitted on-chain with the
	// provided epoch and round.

	epochAndRound := toEpochAndRoundUint40(epoch, round)

	// this is technically NOT a hash in the regular meaning,
	// however it has the same size as a common.Hash. We need
	// to left-pad by bytes because it has to be 256 (or 32 bytes)
	// long in order to use as a topic filter.
	enrTopic := common.BytesToHash(common.LeftPadBytes(epochAndRound.Bytes(), 32))

	c.lggr.Info(fmt.Sprintf("epoch and round: %s %s", epochAndRound.String(), enrTopic.String()))
	logs, err := c.lp.IndexedLogs(
		c.topics.newTransmissionTopic,
		c.coordinatorAddress,
		2,
		[]common.Hash{
			enrTopic,
		},
		1,
		pg.WithParentCtx(ctx))
	if err != nil {
		return false, errors.Wrap(err, "log poller IndexedLogs")
	}

	c.lggr.Info(fmt.Sprintf("NewTransmission logs: %+v", logs))

	return len(logs) >= 1, nil
}

// ReportBlocks returns the heights and hashes of the blocks which require VRF
// proofs in the current report, and the callback requests which should be
// served as part of processing that report. Everything returned by this
// should concern blocks older than the corresponding confirmationDelay.
// Blocks and callbacks it has returned previously may be returned again, as
// long as retransmissionDelay blocks have passed since they were last
// returned. The callbacks returned do not have to correspond to the blocks.
//
// The implementor is responsible for only returning well-funded callback
// requests, and blocks for which clients have actually requested random output
//
// This can be implemented on ethereum using the RandomnessRequested and
// RandomnessFulfillmentRequested events, to identify which blocks and
// callbacks need to be served, along with the NewTransmission and
// RandomWordsFulfilled events, to identify which have already been served.
func (c *coordinator) ReportBlocks(
	ctx context.Context,
	slotInterval uint16, // TODO: unused for now
	confirmationDelays map[uint32]struct{},
	retransmissionDelay time.Duration, // TODO: unused for now
	maxBlocks, // TODO: unused for now
	maxCallbacks int, // TODO: unused for now
) (blocks []ocr2vrftypes.Block, callbacks []ocr2vrftypes.AbstractCostedCallbackRequest, err error) {
	// Instantiate the gas used by this batch.
	currentBatchGasLimit := coordinatorOverhead

	// TODO: use head broadcaster instead?
	currentHeight, err := c.lp.LatestBlock(pg.WithParentCtx(ctx))
	if err != nil {
		err = errors.Wrap(err, "header by number")
		return
	}

	// Evict expired items from the cache.
	now := time.Now().Unix()
	c.toBeTransmittedBlocks.EvictExpiredItems(now)
	c.toBeTransmittedCallbacks.EvictExpiredItems(now)

	c.lggr.Infow("current chain height", "currentHeight", currentHeight)

	logs, err := c.lp.LogsWithSigs(
		currentHeight-c.lookbackBlocks,
		currentHeight,
		[]common.Hash{
			c.randomnessRequestedTopic,
			c.randomnessFulfillmentRequestedTopic,
			c.randomWordsFulfilledTopic,
			c.newTransmissionTopic,
		},
		c.coordinatorAddress,
		pg.WithParentCtx(ctx))
	if err != nil {
		err = errors.Wrap(err, "logs with topics")
		return
	}

	// c.lggr.Info(fmt.Sprintf("vrf LogsWithSigs: %+v", logs))

	randomnessRequestedLogs,
		randomnessFulfillmentRequestedLogs,
		randomWordsFulfilledLogs,
		newTransmissionLogs,
		err := c.unmarshalLogs(logs)
	if err != nil {
		err = errors.Wrap(err, "unmarshal logs")
		return
	}

	c.lggr.Info(fmt.Sprintf("finished unmarshalLogs: RandomnessRequested: %+v , RandomnessFulfillmentRequested: %+v , RandomWordsFulfilled: %+v , NewTransmission: %+v",
		randomnessRequestedLogs, randomWordsFulfilledLogs, newTransmissionLogs, randomnessFulfillmentRequestedLogs))

	// Get blockhashes that pertain to requested blocks.
	blockhashesMapping, err := c.getBlockhashesMappingFromRequests(ctx, randomnessRequestedLogs, randomnessFulfillmentRequestedLogs, currentHeight)
	if err != nil {
		err = errors.Wrap(err, "get blockhashes in ReportBlocks")
		return
	}

	blocksRequested := make(map[block]struct{})
	unfulfilled, err := c.filterEligibleRandomnessRequests(randomnessRequestedLogs, confirmationDelays, currentHeight, blockhashesMapping)
	if err != nil {
		err = errors.Wrap(err, "filter requests in ReportBlocks")
		return
	}
	for _, uf := range unfulfilled {
		blocksRequested[uf] = struct{}{}
	}

	c.lggr.Info(fmt.Sprintf("number of randomness requests: %+v", len(unfulfilled)))

	callbacksRequested, unfulfilled, err := c.filterEligibleCallbacks(randomnessFulfillmentRequestedLogs, confirmationDelays, currentHeight, blockhashesMapping)
	if err != nil {
		err = errors.Wrap(err, "filter clalbacks in ReportBlocks")
		return
	}
	for _, uf := range unfulfilled {
		blocksRequested[uf] = struct{}{}
	}

	c.lggr.Info(fmt.Sprintf("number of callbacks: %+v", len(callbacksRequested)))

	// Remove blocks that have already received responses so that we don't
	// respond to them again.
	fulfilledBlocks := c.getFulfilledBlocks(newTransmissionLogs)
	for _, f := range fulfilledBlocks {
		delete(blocksRequested, f)
	}

	c.lggr.Info(fmt.Sprintf("number of fulfilled blocks: %+v", len(fulfilledBlocks)))

	// Fill blocks slice with valid requested blocks.
	blocks = []ocr2vrftypes.Block{}
	for block := range blocksRequested {
		if batchGasLimit-currentBatchGasLimit >= blockGasOverhead {
			blocks = append(blocks, ocr2vrftypes.Block{
				Hash:              blockhashesMapping[block.blockNumber],
				Height:            block.blockNumber,
				ConfirmationDelay: block.confDelay,
			})
			currentBatchGasLimit += blockGasOverhead
		} else {
			break
		}
	}

	c.lggr.Info(fmt.Sprintf("number of blocks to fill: %+v", len(blocks)))

	// Find unfulfilled callback requests by filtering out already fulfilled callbacks.
	fulfilledRequestIDs := c.getFulfilledRequestIDs(randomWordsFulfilledLogs)
	callbacks = c.filterUnfulfilledCallbacks(callbacksRequested, fulfilledRequestIDs, confirmationDelays, currentHeight, currentBatchGasLimit)

	if len(callbacksRequested) > 0 {
		c.lggr.Info(fmt.Sprintf("newest callback beacon output height %+v", callbacksRequested[len(callbacksRequested)-1].NextBeaconOutputHeight+callbacksRequested[len(callbacksRequested)-1].ConfDelay.Uint64()))
	}
	c.lggr.Info(fmt.Sprintf("number of unfulfilled elligible callbacks: %+v, fulfilled: %+v", len(callbacks), len(fulfilledRequestIDs)))

	return
}

// getBlockhashesMappingFromRequests returns the blockhashes for enqueued request blocks.
func (c *coordinator) getBlockhashesMappingFromRequests(
	ctx context.Context,
	randomnessRequestedLogs []*vrf_wrapper.VRFBeaconCoordinatorRandomnessRequested,
	randomnessFulfillmentRequestedLogs []*vrf_wrapper.VRFBeaconCoordinatorRandomnessFulfillmentRequested,
	currentHeight int64,
) (blockhashesMapping map[uint64]common.Hash, err error) {

	// Get all request + callback requests into a mapping.
	rawBlocksRequested := make(map[uint64]struct{})
	for _, l := range randomnessRequestedLogs {
		if isBlockEligible(l.NextBeaconOutputHeight, l.ConfDelay, currentHeight) {
			rawBlocksRequested[l.NextBeaconOutputHeight] = struct{}{}

			// Also get the blockhash for the most recent cached transmission on this block,
			// if ones exists.
			cacheKey := getBlockCacheKey(int64(l.NextBeaconOutputHeight), l.ConfDelay.Int64())
			t, _ := c.toBeTransmittedBlocks.GetItem(cacheKey)
			if t != nil {
				rawBlocksRequested[t.recentBlockHeight] = struct{}{}
			}
		}
	}
	for _, l := range randomnessFulfillmentRequestedLogs {
		if isBlockEligible(l.NextBeaconOutputHeight, l.ConfDelay, currentHeight) {
			rawBlocksRequested[l.NextBeaconOutputHeight] = struct{}{}

			// Also get the blockhash for the most recent cached transmission on this callback,
			// if one exists.
			cacheKey := getCallbackCacheKey(l.Callback.RequestID.Int64())
			t, _ := c.toBeTransmittedCallbacks.GetItem(cacheKey)
			if t != nil {
				rawBlocksRequested[t.recentBlockHeight] = struct{}{}
			}
		}
	}

	// Fill a unique list of request blocks.
	requestedBlockNumbers := []uint64{}
	for k := range rawBlocksRequested {
		requestedBlockNumbers = append(requestedBlockNumbers, k)
	}

	// Get a mapping of block numbers to block hashes.
	blockhashesMapping, err = c.getBlockhashesMapping(ctx, requestedBlockNumbers)
	if err != nil {
		err = errors.Wrap(err, "get blockhashes for ReportBlocks")
	}
	return
}

func (c *coordinator) getFulfilledBlocks(newTransmissionLogs []*vrf_wrapper.VRFBeaconCoordinatorNewTransmission) (fulfilled []block) {
	for _, r := range newTransmissionLogs {
		for _, o := range r.OutputsServed {
			fulfilled = append(fulfilled, block{
				blockNumber: o.Height,
				confDelay:   uint32(o.ConfirmationDelay.Uint64()),
			})
		}
	}
	return
}

// getBlockhashesMapping returns the blockhashes corresponding to a slice of block numbers.
func (c *coordinator) getBlockhashesMapping(
	ctx context.Context,
	blockNumbers []uint64,
) (blockhashesMapping map[uint64]common.Hash, err error) {
	// GetBlocks doesn't necessarily need a sorted blockNumbers array,
	// but sorting it is helpful for testing.
	sort.Slice(blockNumbers, func(a, b int) bool {
		return blockNumbers[a] < blockNumbers[b]
	})
	heads, err := c.lp.GetBlocks(ctx, blockNumbers, pg.WithParentCtx(ctx))
	if len(heads) != len(blockNumbers) {
		err = fmt.Errorf("could not find all heads in db: want %d got %d", len(blockNumbers), len(heads))
		return
	}

	blockhashesMapping = make(map[uint64]common.Hash)
	for _, head := range heads {
		blockhashesMapping[uint64(head.BlockNumber)] = head.BlockHash
	}
	return
}

// getFulfilledRequestIDs returns the request IDs referenced by the given RandomWordsFulfilled logs slice.
func (c *coordinator) getFulfilledRequestIDs(randomWordsFulfilledLogs []*vrf_wrapper.VRFBeaconCoordinatorRandomWordsFulfilled) map[uint64]struct{} {
	fulfilledRequestIDs := make(map[uint64]struct{})
	for _, r := range randomWordsFulfilledLogs {
		for i, requestID := range r.RequestIDs {
			if r.SuccessfulFulfillment[i] == 1 {
				fulfilledRequestIDs[requestID.Uint64()] = struct{}{}
			}
		}
	}
	return fulfilledRequestIDs
}

// filterUnfulfilledCallbacks returns unfulfilled callback requests given the
// callback request logs and the already fulfilled callback request IDs.
func (c *coordinator) filterUnfulfilledCallbacks(
	callbacksRequested []*vrf_wrapper.VRFBeaconCoordinatorRandomnessFulfillmentRequested,
	fulfilledRequestIDs map[uint64]struct{},
	confirmationDelays map[uint32]struct{},
	currentHeight int64,
	currentBatchGasLimit int64,
) (callbacks []ocr2vrftypes.AbstractCostedCallbackRequest) {

	// Sort callbacksRequested first by time requested (oldest -> newest), then by gas allowance (lowest -> highest).
	sort.Slice(callbacksRequested, func(a, b int) bool {
		aHeight := callbacksRequested[a].NextBeaconOutputHeight + callbacksRequested[a].ConfDelay.Uint64()
		bHeight := callbacksRequested[b].NextBeaconOutputHeight + callbacksRequested[b].ConfDelay.Uint64()
		if aHeight == bHeight {
			return callbacksRequested[a].Callback.GasAllowance.Int64() < callbacksRequested[b].Callback.GasAllowance.Int64()
		}
		return aHeight < bHeight
	})

	for _, r := range callbacksRequested {
		// Check if there is room left in the batch.
		if batchGasLimit-currentBatchGasLimit < r.Callback.GasAllowance.Int64() {
			continue
		}

		requestID := r.Callback.RequestID
		if _, ok := fulfilledRequestIDs[requestID.Uint64()]; !ok {
			// The on-chain machinery will revert requests that specify an unsupported
			// confirmation delay, so this is more of a sanity check than anything else.
			if _, ok := confirmationDelays[uint32(r.ConfDelay.Uint64())]; !ok {
				// if we can't find the conf delay in the map then just ignore this request
				c.lggr.Errorw("ignoring bad request, unsupported conf delay",
					"confDelay", r.ConfDelay.String(),
					"supportedConfDelays", confirmationDelays)
				continue
			}

			// NOTE: we already check if the callback has been fulfilled in filterEligibleCallbacks,
			// so we don't need to do that again here.
			if isBlockEligible(r.NextBeaconOutputHeight, r.ConfDelay, currentHeight) {
				callbacks = append(callbacks, ocr2vrftypes.AbstractCostedCallbackRequest{
					BeaconHeight:      r.NextBeaconOutputHeight,
					ConfirmationDelay: uint32(r.ConfDelay.Uint64()),
					SubscriptionID:    r.SubID,
					Price:             big.NewInt(0), // TODO: no price tracking
					RequestID:         requestID.Uint64(),
					NumWords:          r.Callback.NumWords,
					Requester:         r.Callback.Requester,
					Arguments:         r.Callback.Arguments,
					GasAllowance:      r.Callback.GasAllowance,
					RequestHeight:     r.Raw.BlockNumber,
					RequestBlockHash:  r.Raw.BlockHash,
				})
				currentBatchGasLimit += r.Callback.GasAllowance.Int64()
			}
		}
	}
	return callbacks
}

// filterEligibleCallbacks extracts valid callback requests from the given logs,
// based on their readiness to be fulfilled. It also returns any unfulfilled blocks
// associated with those callbacks.
func (c *coordinator) filterEligibleCallbacks(
	randomnessFulfillmentRequestedLogs []*vrf_wrapper.VRFBeaconCoordinatorRandomnessFulfillmentRequested,
	confirmationDelays map[uint32]struct{},
	currentHeight int64,
	blockhashesMapping map[uint64]common.Hash,
) (callbacks []*vrf_wrapper.VRFBeaconCoordinatorRandomnessFulfillmentRequested, unfulfilled []block, err error) {
	c.transmittedMu.Lock()
	defer c.transmittedMu.Unlock()

	for _, r := range randomnessFulfillmentRequestedLogs {
		// The on-chain machinery will revert requests that specify an unsupported
		// confirmation delay, so this is more of a sanity check than anything else.
		if _, ok := confirmationDelays[uint32(r.ConfDelay.Uint64())]; !ok {
			// if we can't find the conf delay in the map then just ignore this request
			c.lggr.Errorw("ignoring bad request, unsupported conf delay",
				"confDelay", r.ConfDelay.String(),
				"supportedConfDelays", confirmationDelays)
			continue
		}

		// Check that the callback is elligible, and that it hasn't been scheduled for a valid transmission.
		// A re-orged transmission can be ignored.
		if isBlockEligible(r.NextBeaconOutputHeight, r.ConfDelay, currentHeight) {
			cacheKey := getCallbackCacheKey(r.Callback.RequestID.Int64())
			t, _ := c.toBeTransmittedCallbacks.GetItem(cacheKey)
			validTransmission := (t != nil) && (t.recentBlockHash == blockhashesMapping[t.recentBlockHeight])
			if validTransmission {
				continue
			}

			callbacks = append(callbacks, r)

			// We could have a callback request that was made in a different block than what we
			// have possibly already received from regular requests.
			unfulfilled = append(unfulfilled, block{
				blockNumber: r.NextBeaconOutputHeight,
				confDelay:   uint32(r.ConfDelay.Uint64()),
			})
		}
	}
	return
}

// filterEligibleRandomnessRequests extracts valid randomness requests from the given logs,
// based on their readiness to be fulfilled.
func (c *coordinator) filterEligibleRandomnessRequests(
	randomnessRequestedLogs []*vrf_wrapper.VRFBeaconCoordinatorRandomnessRequested,
	confirmationDelays map[uint32]struct{},
	currentHeight int64,
	blockhashesMapping map[uint64]common.Hash,
) (unfulfilled []block, err error) {
	c.transmittedMu.Lock()
	defer c.transmittedMu.Unlock()

	for _, r := range randomnessRequestedLogs {
		// The on-chain machinery will revert requests that specify an unsupported
		// confirmation delay, so this is more of a sanity check than anything else.
		if _, ok := confirmationDelays[uint32(r.ConfDelay.Uint64())]; !ok {
			// if we can't find the conf delay in the map then just ignore this request
			c.lggr.Errorw("ignoring bad request, unsupported conf delay",
				"confDelay", r.ConfDelay.String(),
				"supportedConfDelays", confirmationDelays)
			continue
		}

		// Check that the block is elligible, and that it hasn't been scheduled for a valid transmission.
		// A re-orged transmission can be ignored.
		if isBlockEligible(r.NextBeaconOutputHeight, r.ConfDelay, currentHeight) {
			cacheKey := getBlockCacheKey(int64(r.NextBeaconOutputHeight), r.ConfDelay.Int64())
			t, _ := c.toBeTransmittedBlocks.GetItem(cacheKey)
			validTransmission := (t != nil) && (t.recentBlockHash == blockhashesMapping[t.recentBlockHeight])
			if validTransmission {
				continue
			}

			unfulfilled = append(unfulfilled, block{
				blockNumber: r.NextBeaconOutputHeight,
				confDelay:   uint32(r.ConfDelay.Uint64()),
			})
		}
	}
	return
}

func (c *coordinator) unmarshalLogs(
	logs []logpoller.Log,
) (
	randomnessRequestedLogs []*vrf_wrapper.VRFBeaconCoordinatorRandomnessRequested,
	randomnessFulfillmentRequestedLogs []*vrf_wrapper.VRFBeaconCoordinatorRandomnessFulfillmentRequested,
	randomWordsFulfilledLogs []*vrf_wrapper.VRFBeaconCoordinatorRandomWordsFulfilled,
	newTransmissionLogs []*vrf_wrapper.VRFBeaconCoordinatorNewTransmission,
	err error,
) {
	for _, lg := range logs {
		rawLog := toGethLog(lg)
		switch {
		case bytes.Equal(lg.EventSig, c.randomnessRequestedTopic[:]):
			unpacked, err2 := c.coordinatorContract.ParseLog(rawLog)
			if err2 != nil {
				// should never happen
				err = errors.Wrap(err2, "unmarshal RandomnessRequested log")
				return
			}
			rr, ok := unpacked.(*vrf_wrapper.VRFBeaconCoordinatorRandomnessRequested)
			if !ok {
				// should never happen
				err = errors.New("cast to *VRFBeaconCoordinatorRandomnessRequested")
				return
			}
			randomnessRequestedLogs = append(randomnessRequestedLogs, rr)
		case bytes.Equal(lg.EventSig, c.randomnessFulfillmentRequestedTopic[:]):
			unpacked, err2 := c.coordinatorContract.ParseLog(rawLog)
			if err2 != nil {
				// should never happen
				err = errors.Wrap(err2, "unmarshal RandomnessFulfillmentRequested log")
				return
			}
			rfr, ok := unpacked.(*vrf_wrapper.VRFBeaconCoordinatorRandomnessFulfillmentRequested)
			if !ok {
				// should never happen
				err = errors.New("cast to *VRFBeaconCoordinatorRandomnessFulfillmentRequested")
				return
			}
			randomnessFulfillmentRequestedLogs = append(randomnessFulfillmentRequestedLogs, rfr)
		case bytes.Equal(lg.EventSig, c.randomWordsFulfilledTopic[:]):
			unpacked, err2 := c.coordinatorContract.ParseLog(rawLog)
			if err2 != nil {
				// should never happen
				err = errors.Wrap(err2, "unmarshal RandomWordsFulfilled log")
				return
			}
			rwf, ok := unpacked.(*vrf_wrapper.VRFBeaconCoordinatorRandomWordsFulfilled)
			if !ok {
				// should never happen
				err = errors.New("cast to *VRFBeaconCoordinatorRandomWordsFulfilled")
				return
			}
			randomWordsFulfilledLogs = append(randomWordsFulfilledLogs, rwf)
		case bytes.Equal(lg.EventSig, c.newTransmissionTopic[:]):
			unpacked, err2 := c.coordinatorContract.ParseLog(rawLog)
			if err2 != nil {
				// should never happen
				err = errors.Wrap(err2, "unmarshal NewTransmission log")
				return
			}
			nt, ok := unpacked.(*vrf_wrapper.VRFBeaconCoordinatorNewTransmission)
			if !ok {
				// should never happen
				err = errors.New("cast to *vrf_wrapper.VRFBeaconCoordinatorNewTransmission")
			}
			newTransmissionLogs = append(newTransmissionLogs, nt)
		default:
			c.lggr.Error(fmt.Sprintf("Unexpected event sig: %s", hexutil.Encode(lg.EventSig)))
			c.lggr.Error(fmt.Sprintf("expected one of: %s %s %s %s",
				hexutil.Encode(c.randomnessRequestedTopic[:]), hexutil.Encode(c.randomnessFulfillmentRequestedTopic[:]),
				hexutil.Encode(c.randomWordsFulfilledTopic[:]), hexutil.Encode(c.newTransmissionTopic[:])))
		}
	}
	return
}

// ReportWillBeTransmitted registers to the CoordinatorInterface that the
// local node has accepted the AbstractReport for transmission, so that its
// blocks and callbacks can be tracked for possible later retransmission
func (c *coordinator) ReportWillBeTransmitted(ctx context.Context, report ocr2vrftypes.AbstractReport) error {
	c.transmittedMu.Lock()
	defer c.transmittedMu.Unlock()

	now := time.Now().Unix()

	// Evict expired items from the cache.
	c.toBeTransmittedBlocks.EvictExpiredItems(now)
	c.toBeTransmittedCallbacks.EvictExpiredItems(now)

	// Check for a re-org, and return an error if one is present.
	blockhashesMapping, err := c.getBlockhashesMapping(ctx, []uint64{report.RecentBlockHeight})
	if err != nil {
		return errors.Wrap(err, "getting blockhash mapping in ReportWillBeTransmitted")
	}
	if blockhashesMapping[report.RecentBlockHeight].String() != report.RecentBlockHash.String() {
		return errors.Errorf("blockhash of report does not match most recent blockhash in ReportWillBeTransmitted")
	}

	blocksRequested := []blockInReport{}
	callbacksRequested := []callbackInReport{}

	// Get all requested blocks and callbacks.
	for _, output := range report.Outputs {
		// If the VRF proof size is 0, the beacon output is already on chain, so there is no block to add.
		if len(output.VRFProof) > 0 {
			bR := blockInReport{
				block: block{
					blockNumber: output.BlockHeight,
					confDelay:   output.ConfirmationDelay,
				},
				recentBlockHeight: report.RecentBlockHeight,
				recentBlockHash:   report.RecentBlockHash,
			}
			// Store block in blocksRequested.br
			blocksRequested = append(blocksRequested, bR)
		}

		// Iterate through callbacks for output.
		for _, cb := range output.Callbacks {
			cbR := callbackInReport{
				callback: callback{
					blockNumber: cb.BeaconHeight,
					requestID:   cb.RequestID,
				},
				recentBlockHeight: report.RecentBlockHeight,
				recentBlockHash:   report.RecentBlockHash,
			}

			// Add callback to callbacksRequested.
			callbacksRequested = append(callbacksRequested, cbR)
		}
	}

	// Apply blockhashes to blocks and mark them as transmitted.
	for _, b := range blocksRequested {
		cacheKey := getBlockCacheKey(int64(b.blockNumber), int64(b.confDelay))
		err := c.toBeTransmittedBlocks.CacheItem(b, cacheKey, now)
		if err != nil {
			return errors.Wrap(err, "adding block to the cache in ReportWillBeTransmitted")
		}
	}

	// Add the corresponding blockhashes to callbacks and mark them as transmitted.
	for _, cb := range callbacksRequested {
		cacheKey := getCallbackCacheKey(int64(cb.requestID))
		err := c.toBeTransmittedCallbacks.CacheItem(cb, cacheKey, now)
		if err != nil {
			return errors.Wrap(err, "adding callback to the cache in ReportWillBeTransmitted")
		}
	}

	return nil
}

// DKGVRFCommittees returns the addresses of the signers and transmitters
// for the DKG and VRF OCR committees. On ethereum, these can be retrieved
// from the most recent ConfigSet events for each contract.
func (c *coordinator) DKGVRFCommittees(ctx context.Context) (dkgCommittee, vrfCommittee ocr2vrftypes.OCRCommittee, err error) {
	latestVRF, err := c.lp.LatestLogByEventSigWithConfs(
		c.configSetTopic,
		c.coordinatorAddress,
		int(c.finalityDepth),
	)
	if err != nil {
		err = errors.Wrap(err, "latest vrf ConfigSet by sig with confs")
		return
	}

	latestDKG, err := c.lp.LatestLogByEventSigWithConfs(
		c.configSetTopic,
		c.dkgAddress,
		int(c.finalityDepth),
	)
	if err != nil {
		err = errors.Wrap(err, "latest dkg ConfigSet by sig with confs")
		return
	}

	var vrfConfigSetLog vrf_wrapper.VRFBeaconCoordinatorConfigSet
	err = vrfABI.UnpackIntoInterface(&vrfConfigSetLog, configSetEvent, latestVRF.Data)
	if err != nil {
		err = errors.Wrap(err, "unpack vrf ConfigSet into interface")
		return
	}

	var dkgConfigSetLog dkg_wrapper.DKGConfigSet
	err = dkgABI.UnpackIntoInterface(&dkgConfigSetLog, configSetEvent, latestDKG.Data)
	if err != nil {
		err = errors.Wrap(err, "unpack dkg ConfigSet into interface")
		return
	}

	// len(signers) == len(transmitters), this is guaranteed by libocr.
	for i := range vrfConfigSetLog.Signers {
		vrfCommittee.Signers = append(vrfCommittee.Signers, vrfConfigSetLog.Signers[i])
		vrfCommittee.Transmitters = append(vrfCommittee.Transmitters, vrfConfigSetLog.Transmitters[i])
	}

	for i := range dkgConfigSetLog.Signers {
		dkgCommittee.Signers = append(dkgCommittee.Signers, dkgConfigSetLog.Signers[i])
		dkgCommittee.Transmitters = append(dkgCommittee.Transmitters, dkgConfigSetLog.Transmitters[i])
	}

	return
}

// ProvingKeyHash returns the VRF current proving block, in view of the local
// node. On ethereum this can be retrieved from the VRF contract's attribute
// s_provingKeyHash
func (c *coordinator) ProvingKeyHash(ctx context.Context) (common.Hash, error) {
	h, err := c.coordinatorContract.SProvingKeyHash(&bind.CallOpts{
		Context: ctx,
	})
	if err != nil {
		return [32]byte{}, errors.Wrap(err, "get proving block hash")
	}

	return h, nil
}

// BeaconPeriod returns the period used in the coordinator's contract
func (c *coordinator) BeaconPeriod(ctx context.Context) (uint16, error) {
	beaconPeriodBlocks, err := c.coordinatorContract.IBeaconPeriodBlocks(&bind.CallOpts{
		Context: ctx,
	})
	if err != nil {
		return 0, errors.Wrap(err, "get beacon period blocks")
	}

	return uint16(beaconPeriodBlocks.Int64()), nil
}

// ConfirmationDelays returns the list of confirmation delays defined in the coordinator's contract
func (c *coordinator) ConfirmationDelays(ctx context.Context) ([]uint32, error) {
	confDelays, err := c.coordinatorContract.GetConfirmationDelays(&bind.CallOpts{
		Context: ctx,
	})
	if err != nil {
		return nil, errors.Wrap(err, "could not get confirmation delays")
	}
	var result []uint32
	for _, c := range confDelays {
		result = append(result, uint32(c.Uint64()))
	}
	return result, nil
}

// KeyID returns the key ID from coordinator's contract
func (c *coordinator) KeyID(ctx context.Context) (dkg.KeyID, error) {
	keyID, err := c.coordinatorContract.SKeyID(&bind.CallOpts{Context: ctx})
	if err != nil {
		return dkg.KeyID{}, errors.Wrap(err, "could not get key ID")
	}
	return keyID, nil
}

// isBlockEligible returns true if and only if the nextBeaconOutputHeight plus
// the confDelay is less than the current blockchain height, meaning that the beacon
// output height has enough confirmations.
//
// NextBeaconOutputHeight is always greater than the request block, therefore
// a number of confirmations on the beacon block is always enough confirmations
// for the request block.
func isBlockEligible(nextBeaconOutputHeight uint64, confDelay *big.Int, currentHeight int64) bool {
	cond := confDelay.Int64() < currentHeight // Edge case: for simulated chains with low block numbers
	cond = cond && (nextBeaconOutputHeight+confDelay.Uint64()) < uint64(currentHeight)
	return cond
}

// toEpochAndRoundUint40 returns a single unsigned 40 bit big.Int object
// that has the epoch in the first 32 bytes and the round in the last 8 bytes,
// in a big-endian fashion.
func toEpochAndRoundUint40(epoch uint32, round uint8) *big.Int {
	return big.NewInt((int64(epoch) << 8) + int64(round))
}

func toGethLog(lg logpoller.Log) types.Log {
	var topics []common.Hash
	for _, b := range lg.Topics {
		topics = append(topics, common.BytesToHash(b))
	}
	return types.Log{
		Data:        lg.Data,
		Address:     lg.Address,
		BlockHash:   lg.BlockHash,
		BlockNumber: uint64(lg.BlockNumber),
		Topics:      topics,
		TxHash:      lg.TxHash,
		Index:       uint(lg.LogIndex),
	}
}

// getBlockCacheKey returns a cache key for a requested block
func getBlockCacheKey(blockNumber int64, confDelay int64) common.Hash {
	return common.BigToHash(big.NewInt(blockNumber + confDelay))
}

// getBlockCacheKey returns a cache key for a requested callback
func getCallbackCacheKey(requestID int64) common.Hash {
	return common.BigToHash(big.NewInt(requestID))
}
