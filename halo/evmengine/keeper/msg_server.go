package keeper

import (
	"context"
	"encoding/json"
	"time"

	"github.com/omni-network/omni/halo/evmengine/types"
	"github.com/omni-network/omni/lib/errors"
	"github.com/omni-network/omni/lib/ethclient"
	"github.com/omni-network/omni/lib/log"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	etypes "github.com/ethereum/go-ethereum/core/types"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

type msgServer struct {
	*Keeper
	types.UnimplementedMsgServiceServer
}

// ExecutionPayload handles a new execution payload included in the current finalized block.
func (s msgServer) ExecutionPayload(ctx context.Context, msg *types.MsgExecutionPayload,
) (*types.ExecutionPayloadResponse, error) {
	sdkCtx := sdk.UnwrapSDKContext(ctx)
	if sdkCtx.ExecMode() != sdk.ExecModeFinalize {
		return nil, errors.New("only allowed in finalize mode")
	}

	payload, err := pushPayload(ctx, s.engineCl, msg)
	if err != nil {
		return nil, err
	}

	// CometBFT has instant finality, so head/safe/finalized is latest height.
	fcs := engine.ForkchoiceStateV1{
		HeadBlockHash:      payload.BlockHash,
		SafeBlockHash:      payload.BlockHash,
		FinalizedBlockHash: payload.BlockHash,
	}

	// Maybe also start building the next block if we are the next proposer.
	isNext, nextHeight, err := s.isNextProposer(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "next proposer")
	}

	var attrs *engine.PayloadAttributes
	if s.buildOptimistic && isNext {
		log.Debug(ctx, "Triggering optimistic EVM payload build", "next_height", nextHeight)
		ts := uint64(time.Now().Unix())
		if ts <= payload.Timestamp {
			ts = payload.Timestamp + 1 // Subsequent blocks must have a higher timestamp.
		}
		attrs = &engine.PayloadAttributes{
			Timestamp:             ts,
			Random:                fcs.HeadBlockHash, // We use head block hash as randao.
			SuggestedFeeRecipient: s.addrProvider.LocalAddress(),
			Withdrawals:           []*etypes.Withdrawal{}, // Withdrawals not supported yet.
			BeaconRoot:            nil,
		}
	}

	fcr, err := s.engineCl.ForkchoiceUpdatedV2(ctx, fcs, attrs)
	if err != nil {
		return nil, err
	} else if fcr.PayloadStatus.Status != engine.VALID {
		return nil, errors.New("status not valid")
	}

	if err := s.deliverLogs(ctx, payload.Number-1, payload.ParentHash, msg.PrevPayloadLogs); err != nil {
		return nil, errors.Wrap(err, "deliver logs")
	}

	if isNext {
		s.setOptimisticPayload(fcr.PayloadID, nextHeight)
	}

	return &types.ExecutionPayloadResponse{}, nil
}

// deliverLogs delivers the given logs to the registered log providers.
func (s msgServer) deliverLogs(ctx context.Context, height uint64, blockHash common.Hash, logs []*types.EVMLog) error {
	providers := make(map[common.Address]types.EvmLogProvider)
	for _, provider := range s.logProviders {
		for _, addr := range provider.Addresses() {
			providers[addr] = provider
		}
	}

	for _, evmLog := range logs {
		if err := evmLog.Verify(); err != nil {
			return errors.Wrap(err, "verify log [BUG]") // This shouldn't happen
		}

		p, ok := providers[common.BytesToAddress(evmLog.Address)]
		if !ok {
			return errors.New("unknown log address [BUG]", log.Hex7("address", evmLog.Address))
		}

		if err := p.DeliverLog(ctx, blockHash, evmLog); err != nil {
			return errors.Wrap(err, "deliver log")
		}
	}

	log.Debug(ctx, "Delivered evm logs", "height", height, "count", len(logs))

	return nil
}

// pushPayload creates a new payload from the given message and pushes it to the execution client.
// It returns the new forkchoice state.
func pushPayload(ctx context.Context, engineCl ethclient.EngineClient, msg *types.MsgExecutionPayload,
) (engine.ExecutableData, error) {
	var payload engine.ExecutableData
	if err := json.Unmarshal(msg.ExecutionPayload, &payload); err != nil {
		return engine.ExecutableData{}, errors.Wrap(err, "unmarshal payload")
	}

	// Push it back to the execution client (mark it as possible new head).
	status, err := engineCl.NewPayloadV2(ctx, payload)
	if err != nil {
		return engine.ExecutableData{}, errors.Wrap(err, "new payload")
	} else if status.Status != engine.VALID {
		msg := "unknown"
		if status.ValidationError != nil {
			msg = *status.ValidationError
		}

		return engine.ExecutableData{}, errors.New("new payload invalid", "msg", msg)
	}

	return payload, nil
}

// NewMsgServerImpl returns an implementation of the MsgServer interface
// for the provided Keeper.
func NewMsgServerImpl(keeper *Keeper) types.MsgServiceServer {
	return &msgServer{Keeper: keeper}
}

var _ types.MsgServiceServer = msgServer{}
