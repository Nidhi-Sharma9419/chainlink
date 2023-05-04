package median

import (
	"context"
	"encoding/json"
	"os/exec"
	"time"

	libocr2 "github.com/smartcontractkit/libocr/offchainreporting2"
	"github.com/smartcontractkit/libocr/offchainreporting2/reportingplugin/median"
	ocrtypes "github.com/smartcontractkit/libocr/offchainreporting2/types"

	"github.com/smartcontractkit/chainlink-relay/pkg/loop"
	"github.com/smartcontractkit/chainlink-relay/pkg/types"

	"github.com/smartcontractkit/chainlink/v2/plugins"

	v2 "github.com/smartcontractkit/chainlink/v2/core/config/v2"
	"github.com/smartcontractkit/chainlink/v2/core/logger"
	"github.com/smartcontractkit/chainlink/v2/core/services"
	"github.com/smartcontractkit/chainlink/v2/core/services/job"
	"github.com/smartcontractkit/chainlink/v2/core/services/ocr2/plugins/median/config"
	"github.com/smartcontractkit/chainlink/v2/core/services/ocrcommon"
	"github.com/smartcontractkit/chainlink/v2/core/services/pipeline"
	"github.com/smartcontractkit/chainlink/v2/core/utils"
)

type MedianConfig interface {
	JobPipelineMaxSuccessfulRuns() uint64
	plugins.EnvConfig
}

func NewMedianServices(ctx context.Context,
	jb job.Job,
	isNewlyCreatedJob bool,
	relayer loop.Relayer,
	pipelineRunner pipeline.Runner,
	runResults chan pipeline.Run,
	lggr logger.Logger,
	argsNoPlugin libocr2.OracleArgs,
	cfg MedianConfig,
	chEnhancedTelem chan ocrcommon.EnhancedTelemetryData,
	errorLog loop.ErrorLog,
) (srvs []job.ServiceCtx, err error) {
	var pluginConfig config.PluginConfig
	err = json.Unmarshal(jb.OCR2OracleSpec.PluginConfig.Bytes(), &pluginConfig)
	if err != nil {
		return
	}
	err = config.ValidatePluginConfig(pluginConfig)
	if err != nil {
		return
	}
	spec := jb.OCR2OracleSpec

	provider, err := relayer.NewMedianProvider(ctx, types.RelayArgs{
		ExternalJobID: jb.ExternalJobID,
		JobID:         spec.ID,
		ContractID:    spec.ContractID,
		New:           isNewlyCreatedJob,
		RelayConfig:   spec.RelayConfig.Bytes(),
	}, types.PluginArgs{
		TransmitterID: spec.TransmitterID.String,
		PluginConfig:  spec.PluginConfig.Bytes(),
	})
	if err != nil {
		return
	}
	srvs = append(srvs, provider)
	argsNoPlugin.ContractTransmitter = provider.ContractTransmitter()
	argsNoPlugin.ContractConfigTracker = provider.ContractConfigTracker()
	argsNoPlugin.OffchainConfigDigester = provider.OffchainConfigDigester()

	abort := func() {
		var mc services.MultiClose
		for i := range srvs {
			mc = append(mc, srvs[i])
		}
		if cerr := mc.Close(); err != nil {
			lggr.Errorw("Error closing unused services", "err", cerr)
		}
	}

	var median loop.PluginMedian
	if cmdName := v2.EnvMedianPluginCmd.Get(); cmdName != "" {
		median = loop.NewMedianService(lggr, func() *exec.Cmd {
			cmd := exec.Command(cmdName)
			plugins.SetEnvConfig(cmd, cfg)
			return cmd
		})
	} else {
		median = NewPlugin(lggr)
	}
	srvs = append(srvs, median)
	argsNoPlugin.ReportingPluginFactory, err = median.NewMedianPluginFactory(ctx, provider, ocrcommon.NewDataSourceV2(pipelineRunner,
		jb,
		*jb.PipelineSpec,
		lggr,
		runResults,
		chEnhancedTelem,
	), ocrcommon.NewInMemoryDataSource(pipelineRunner, jb, pipeline.Spec{
		ID:           jb.ID,
		DotDagSource: pluginConfig.JuelsPerFeeCoinPipeline,
		CreatedAt:    time.Now(),
	}, lggr), errorLog)
	if err != nil {
		abort()
		return
	}

	var oracle *libocr2.Oracle
	oracle, err = libocr2.NewOracle(argsNoPlugin)
	if err != nil {
		abort()
		return
	}
	runSaver := ocrcommon.NewResultRunSaver(
		runResults,
		pipelineRunner,
		make(chan struct{}),
		lggr,
		cfg.JobPipelineMaxSuccessfulRuns(),
	)
	srvs = append(srvs, runSaver, job.NewServiceAdapter(oracle))
	if !jb.OCR2OracleSpec.CaptureEATelemetry {
		lggr.Infof("Enhanced EA telemetry is disabled for job %s", jb.Name.ValueOrZero())
	}
	return
}

type Plugin struct {
	utils.StartStopOnce
	lggr logger.Logger
	stop utils.StopChan
}

func (m *Plugin) Name() string { return m.lggr.Name() }

func (m *Plugin) Start(ctx context.Context) error {
	return m.StartOnce("PluginMedian", func() error { return nil })
}

func (m *Plugin) HealthReport() map[string]error {
	return map[string]error{m.Name(): nil}
}

func NewPlugin(lggr logger.Logger) *Plugin {
	return &Plugin{lggr: lggr, stop: make(utils.StopChan)}
}

func (m *Plugin) NewMedianPluginFactory(ctx context.Context, provider types.MedianProvider, dataSource, juelsPerFeeCoin median.DataSource, errorLog loop.ErrorLog) (ocrtypes.ReportingPluginFactory, error) {
	var ctxVals loop.ContextValues
	ctxVals.SetValues(ctx)
	lggr := m.lggr.With(ctxVals.Args()...)
	factory := median.NumericalMedianFactory{
		ContractTransmitter:       provider.MedianContract(),
		DataSource:                dataSource,
		JuelsPerFeeCoinDataSource: juelsPerFeeCoin,
		Logger: logger.NewOCRWrapper(lggr, true, func(msg string) {
			ctx, cancelFn := m.stop.NewCtx()
			defer cancelFn()
			if err := errorLog.SaveError(ctx, msg); err != nil {
				lggr.Errorw("Unable to save error", "err", msg)
			}
		}),
		OnchainConfigCodec: provider.OnchainConfigCodec(),
		ReportCodec:        provider.ReportCodec(),
	}
	return factory, nil
}

func (m *Plugin) Close() error {
	return m.StopOnce("PluginMedian", func() (err error) {
		m.lggr.Debug("Stopping")

		close(m.stop)

		return
	})
}
