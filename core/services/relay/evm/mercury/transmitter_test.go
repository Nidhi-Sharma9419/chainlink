package mercury

import (
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2plus/types"

	"github.com/smartcontractkit/chainlink/v2/core/internal/testutils"
	"github.com/smartcontractkit/chainlink/v2/core/logger"
	"github.com/smartcontractkit/chainlink/v2/core/services/relay/evm/mercury/wsrpc"
	"github.com/smartcontractkit/chainlink/v2/core/services/relay/evm/mercury/wsrpc/pb"
)

type MockWSRPCClient struct {
	transmit func(ctx context.Context, in *pb.TransmitRequest) (*pb.TransmitResponse, error)
}

func (m MockWSRPCClient) Name() string                   { return "" }
func (m MockWSRPCClient) Start(context.Context) error    { return nil }
func (m MockWSRPCClient) Close() error                   { return nil }
func (m MockWSRPCClient) HealthReport() map[string]error { return map[string]error{} }
func (m MockWSRPCClient) Ready() error                   { return nil }
func (m MockWSRPCClient) Transmit(ctx context.Context, in *pb.TransmitRequest) (*pb.TransmitResponse, error) {
	return m.transmit(ctx, in)
}

var _ wsrpc.Client = &MockWSRPCClient{}

type MockTracker struct {
	latestConfigDetails func(ctx context.Context) (changedInBlock uint64, configDigest ocrtypes.ConfigDigest, err error)
}

func (m MockTracker) LatestConfigDetails(ctx context.Context) (changedInBlock uint64, configDigest ocrtypes.ConfigDigest, err error) {
	return m.latestConfigDetails(ctx)
}

var _ ConfigTracker = &MockTracker{}

func Test_MercuryTransmitter_Transmit(t *testing.T) {
	t.Parallel()

	lggr := logger.TestLogger(t)

	t.Run("successful transmit", func(t *testing.T) {
		c := MockWSRPCClient{
			transmit: func(ctx context.Context, in *pb.TransmitRequest) (out *pb.TransmitResponse, err error) {
				require.NotNil(t, in)
				assert.Equal(t, samplePayloadHex, hexutil.Encode(in.Payload))
				out = new(pb.TransmitResponse)
				out.Code = 42
				out.Error = ""
				return out, nil
			},
		}
		mt := NewTransmitter(lggr, nil, c, sampleClientPubKey, sampleFeedID)
		err := mt.Transmit(testutils.Context(t), sampleReportContext, sampleReport, sampleSigs)

		require.NoError(t, err)
	})

	t.Run("failing transmit", func(t *testing.T) {
		c := MockWSRPCClient{
			transmit: func(ctx context.Context, in *pb.TransmitRequest) (out *pb.TransmitResponse, err error) {
				return nil, errors.New("foo error")
			},
		}
		mt := NewTransmitter(lggr, nil, c, sampleClientPubKey, sampleFeedID)
		err := mt.Transmit(testutils.Context(t), sampleReportContext, sampleReport, sampleSigs)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "foo error")
	})
}
