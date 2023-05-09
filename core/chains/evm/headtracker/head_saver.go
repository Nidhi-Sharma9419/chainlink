package headtracker

import (
	"context"

	"github.com/ethereum/go-ethereum/common"

	"github.com/smartcontractkit/chainlink/v2/core/chains/evm/client"
	httypes "github.com/smartcontractkit/chainlink/v2/core/chains/evm/headtracker/types"
	evmtypes "github.com/smartcontractkit/chainlink/v2/core/chains/evm/types"
	"github.com/smartcontractkit/chainlink/v2/core/logger"
	"github.com/smartcontractkit/chainlink/v2/core/utils/mathutil"
)

type headSaver struct {
	orm    ORM
	config Config
	logger logger.Logger
	heads  Heads
	client client.Client
}

func NewHeadSaver(lggr logger.Logger, orm ORM, config Config, client client.Client) httypes.HeadSaver {
	return &headSaver{
		orm:    orm,
		config: config,
		logger: lggr.Named("HeadSaver"),
		heads:  NewHeads(),
		client: client,
	}
}

func (hs *headSaver) Save(ctx context.Context, head *evmtypes.Head) error {
	if err := hs.orm.IdempotentInsertHead(ctx, head); err != nil {
		return err
	}

	latestFinalized, err := hs.LatestFinalizedBlockNumber(ctx, head.Number)
	if err != nil {
		return err
	}
	hs.heads.AddHeads(latestFinalized, head)
	if head.ChainLength() < hs.config.EvmHeadTrackerTotalUnfinalizedHeadsLimit() {
		hs.logger.Warnw("chain larger than EvmHeadTrackerHistoryDepth. In memory heads exceed limit.",
			"chainLen", head.ChainLength(), "evmHeadTrackerHistoryDepth", hs.config.EvmHeadTrackerTotalUnfinalizedHeadsLimit())
	}

	return hs.orm.TrimOldHeads(ctx, uint(latestFinalized))
}

func (hs *headSaver) LoadFromDB(ctx context.Context) (chain *evmtypes.Head, err error) {
	heads, err := hs.orm.LatestHeads(ctx, uint(hs.config.EvmHeadTrackerTotalUnfinalizedHeadsLimit()))
	if err != nil {
		return nil, err
	}

	latestFinalized, err := hs.LatestFinalizedBlockNumber(ctx, heads[0].Number)
	if err != nil {
		return nil, err
	}
	hs.heads.AddHeads(latestFinalized, heads...)
	return hs.heads.LatestHead(), nil
}

func (hs *headSaver) LatestHeadFromDB(ctx context.Context) (head *evmtypes.Head, err error) {
	return hs.orm.LatestHead(ctx)
}

func (hs *headSaver) LatestChain() *evmtypes.Head {
	head := hs.heads.LatestHead()
	if head == nil {
		return nil
	}
	if head.ChainLength() < hs.config.EvmFinalityDepth() {
		hs.logger.Debugw("chain shorter than EvmFinalityDepth", "chainLen", head.ChainLength(), "evmFinalityDepth", hs.config.EvmFinalityDepth())
	}
	return head
}

func (hs *headSaver) Chain(hash common.Hash) *evmtypes.Head {
	return hs.heads.HeadByHash(hash)
}

func (hs *headSaver) LatestFinalizedBlockNumber(ctx context.Context, currentHeadNumber int64) (int64, error) {
	var err error
	if !hs.config.EvmFinalityTag() {
		latestHead := mathutil.Max(hs.LatestChain().Number, currentHeadNumber)
		return mathutil.Max(latestHead-int64(hs.config.EvmFinalityDepth()+1), 0), err
	}
	return hs.client.HeightByBlockType(ctx, "finalized")
}

var NullSaver httypes.HeadSaver = &nullSaver{}

type nullSaver struct{}

func (*nullSaver) Save(ctx context.Context, head *evmtypes.Head) error          { return nil }
func (*nullSaver) LoadFromDB(ctx context.Context) (*evmtypes.Head, error)       { return nil, nil }
func (*nullSaver) LatestHeadFromDB(ctx context.Context) (*evmtypes.Head, error) { return nil, nil }
func (*nullSaver) LatestChain() *evmtypes.Head                                  { return nil }
func (*nullSaver) Chain(hash common.Hash) *evmtypes.Head                        { return nil }
func (*nullSaver) LatestFinalizedBlockNumber(ctx context.Context, currentHeadNumber int64) (int64, error) {
	return 0, nil
}
