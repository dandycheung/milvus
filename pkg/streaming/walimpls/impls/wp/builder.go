package wp

import (
	"context"
	"fmt"

	"github.com/minio/minio-go/v7"
	"github.com/zilliztech/woodpecker/common/config"
	wpMetrics "github.com/zilliztech/woodpecker/common/metrics"
	wpMinioHandler "github.com/zilliztech/woodpecker/common/minio"
	"github.com/zilliztech/woodpecker/woodpecker"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/metrics"
	"github.com/milvus-io/milvus/pkg/v2/objectstorage"
	"github.com/milvus-io/milvus/pkg/v2/streaming/util/message"
	"github.com/milvus-io/milvus/pkg/v2/streaming/walimpls"
	"github.com/milvus-io/milvus/pkg/v2/streaming/walimpls/registry"
	"github.com/milvus-io/milvus/pkg/v2/util/etcd"
	"github.com/milvus-io/milvus/pkg/v2/util/paramtable"
)

const (
	WALName = "woodpecker"
)

func init() {
	// register the builder to the wal registry.
	registry.RegisterBuilder(&builderImpl{})
	// register the unmarshaler to the message registry.
	message.RegisterMessageIDUnmsarshaler(WALName, UnmarshalMessageID)
}

// builderImpl is the builder for woodpecker opener.
type builderImpl struct{}

// Name of the wal builder, should be a lowercase string.
func (b *builderImpl) Name() string {
	return WALName
}

// Build build a wal instance.
func (b *builderImpl) Build() (walimpls.OpenerImpls, error) {
	cfg, err := b.getWpConfig()
	if err != nil {
		return nil, err
	}
	var minioHandler wpMinioHandler.MinioHandler
	if cfg.Woodpecker.Storage.IsStorageMinio() {
		minioCli, err := b.getMinioClient(context.TODO())
		if err != nil {
			return nil, err
		}
		minioHandler, err = wpMinioHandler.NewMinioHandlerWithClient(context.Background(), minioCli)
		if err != nil {
			return nil, err
		}
		log.Ctx(context.Background()).Info("create minio handler finish while building wp opener")
	}
	etcdCli, err := b.getEtcdClient(context.TODO())
	if err != nil {
		return nil, err
	}
	log.Ctx(context.Background()).Info("create etcd client finish while building wp opener")
	wpClient, err := woodpecker.NewEmbedClient(context.Background(), cfg, etcdCli, minioHandler, true)
	if err != nil {
		return nil, err
	}
	log.Ctx(context.Background()).Info("build wp opener finish", zap.String("wpClientInstance", fmt.Sprintf("%p", wpClient)))
	wpMetrics.RegisterWoodpeckerWithRegisterer(metrics.GetRegisterer())
	return &openerImpl{
		c: wpClient,
	}, nil
}

func (b *builderImpl) getWpConfig() (*config.Configuration, error) {
	wpConfig, err := config.NewConfiguration()
	if err != nil {
		return nil, err
	}
	err = b.setCustomWpConfig(wpConfig, &paramtable.Get().WoodpeckerCfg)
	if err != nil {
		return nil, err
	}
	return wpConfig, nil
}

func (b *builderImpl) setCustomWpConfig(wpConfig *config.Configuration, cfg *paramtable.WoodpeckerConfig) error {
	// set the rootPath as the prefix for wp object storage
	wpConfig.Woodpecker.Meta.Prefix = fmt.Sprintf("%s/wp", paramtable.Get().EtcdCfg.RootPath.GetValue())
	// logClient
	wpConfig.Woodpecker.Client.Auditor.MaxInterval = int(cfg.AuditorMaxInterval.GetAsDurationByParse().Seconds())
	wpConfig.Woodpecker.Client.SegmentAppend.MaxRetries = cfg.AppendMaxRetries.GetAsInt()
	wpConfig.Woodpecker.Client.SegmentAppend.QueueSize = cfg.AppendQueueSize.GetAsInt()
	wpConfig.Woodpecker.Client.SegmentRollingPolicy.MaxSize = cfg.SegmentRollingMaxSize.GetAsSize()
	wpConfig.Woodpecker.Client.SegmentRollingPolicy.MaxInterval = int(cfg.SegmentRollingMaxTime.GetAsDurationByParse().Seconds())
	wpConfig.Woodpecker.Client.SegmentRollingPolicy.MaxBlocks = cfg.SegmentRollingMaxBlocks.GetAsInt64()
	// logStore
	wpConfig.Woodpecker.Logstore.SegmentSyncPolicy.MaxInterval = int(cfg.SyncMaxInterval.GetAsDurationByParse().Milliseconds())
	wpConfig.Woodpecker.Logstore.SegmentSyncPolicy.MaxIntervalForLocalStorage = int(cfg.SyncMaxIntervalForLocalStorage.GetAsDurationByParse().Milliseconds())
	wpConfig.Woodpecker.Logstore.SegmentSyncPolicy.MaxEntries = cfg.SyncMaxEntries.GetAsInt()
	wpConfig.Woodpecker.Logstore.SegmentSyncPolicy.MaxBytes = cfg.SyncMaxBytes.GetAsSize()
	wpConfig.Woodpecker.Logstore.SegmentSyncPolicy.MaxFlushRetries = cfg.FlushMaxRetries.GetAsInt()
	wpConfig.Woodpecker.Logstore.SegmentSyncPolicy.MaxFlushSize = cfg.FlushMaxSize.GetAsSize()
	wpConfig.Woodpecker.Logstore.SegmentSyncPolicy.MaxFlushThreads = cfg.FlushMaxThreads.GetAsInt()
	wpConfig.Woodpecker.Logstore.SegmentSyncPolicy.RetryInterval = int(cfg.RetryInterval.GetAsDurationByParse().Milliseconds())
	wpConfig.Woodpecker.Logstore.SegmentCompactionPolicy.MaxBytes = cfg.CompactionSize.GetAsSize()
	wpConfig.Woodpecker.Logstore.SegmentCompactionPolicy.MaxParallelUploads = cfg.CompactionMaxParallelUploads.GetAsInt()
	wpConfig.Woodpecker.Logstore.SegmentCompactionPolicy.MaxParallelReads = cfg.CompactionMaxParallelReads.GetAsInt()
	wpConfig.Woodpecker.Logstore.SegmentReadPolicy.MaxBatchSize = cfg.ReaderMaxBatchSize.GetAsSize()
	wpConfig.Woodpecker.Logstore.SegmentReadPolicy.MaxFetchThreads = cfg.ReaderMaxFetchThreads.GetAsInt()
	// storage
	wpConfig.Woodpecker.Storage.Type = cfg.StorageType.GetValue()
	wpConfig.Woodpecker.Storage.RootPath = cfg.RootPath.GetValue()

	// set bucketName
	wpConfig.Minio.BucketName = paramtable.Get().MinioCfg.BucketName.GetValue()
	wpConfig.Minio.RootPath = fmt.Sprintf("%s/wp", paramtable.Get().MinioCfg.RootPath.GetValue())

	// set log
	wpConfig.Log.Level = paramtable.Get().LogCfg.Level.GetValue()
	wpConfig.Log.Format = paramtable.Get().LogCfg.Format.GetValue()
	wpConfig.Log.Stdout = paramtable.Get().LogCfg.Stdout.GetAsBool()
	wpConfig.Log.File.RootPath = paramtable.Get().LogCfg.RootPath.GetValue()
	wpConfig.Log.File.MaxSize = paramtable.Get().LogCfg.MaxSize.GetAsInt()
	wpConfig.Log.File.MaxAge = paramtable.Get().LogCfg.MaxAge.GetAsInt()
	wpConfig.Log.File.MaxBackups = paramtable.Get().LogCfg.MaxBackups.GetAsInt()

	return nil
}

func (b *builderImpl) getMinioClient(ctx context.Context) (*minio.Client, error) {
	c := objectstorage.NewDefaultConfig()
	params := paramtable.Get()
	opts := []objectstorage.Option{
		objectstorage.RootPath(params.MinioCfg.RootPath.GetValue()),
		objectstorage.Address(params.MinioCfg.Address.GetValue()),
		objectstorage.AccessKeyID(params.MinioCfg.AccessKeyID.GetValue()),
		objectstorage.SecretAccessKeyID(params.MinioCfg.SecretAccessKey.GetValue()),
		objectstorage.UseSSL(params.MinioCfg.UseSSL.GetAsBool()),
		objectstorage.SslCACert(params.MinioCfg.SslCACert.GetValue()),
		objectstorage.BucketName(params.MinioCfg.BucketName.GetValue()),
		objectstorage.UseIAM(params.MinioCfg.UseIAM.GetAsBool()),
		objectstorage.CloudProvider(params.MinioCfg.CloudProvider.GetValue()),
		objectstorage.IAMEndpoint(params.MinioCfg.IAMEndpoint.GetValue()),
		objectstorage.UseVirtualHost(params.MinioCfg.UseVirtualHost.GetAsBool()),
		objectstorage.Region(params.MinioCfg.Region.GetValue()),
		objectstorage.RequestTimeout(params.MinioCfg.RequestTimeoutMs.GetAsInt64()),
		objectstorage.CreateBucket(true),
		objectstorage.GcpCredentialJSON(params.MinioCfg.GcpCredentialJSON.GetValue()),
	}
	for _, opt := range opts {
		opt(c)
	}
	return objectstorage.NewMinioClient(ctx, c)
}

func (b *builderImpl) getEtcdClient(ctx context.Context) (*clientv3.Client, error) {
	params := paramtable.Get()
	etcdConfig := &params.EtcdCfg
	log := log.Ctx(ctx)

	etcdCli, err := etcd.CreateEtcdClient(
		etcdConfig.UseEmbedEtcd.GetAsBool(),
		etcdConfig.EtcdEnableAuth.GetAsBool(),
		etcdConfig.EtcdAuthUserName.GetValue(),
		etcdConfig.EtcdAuthPassword.GetValue(),
		etcdConfig.EtcdUseSSL.GetAsBool(),
		etcdConfig.Endpoints.GetAsStrings(),
		etcdConfig.EtcdTLSCert.GetValue(),
		etcdConfig.EtcdTLSKey.GetValue(),
		etcdConfig.EtcdTLSCACert.GetValue(),
		etcdConfig.EtcdTLSMinVersion.GetValue())
	if err != nil {
		log.Warn("Woodpecker create connection to etcd failed", zap.Error(err))
		return nil, err
	}
	return etcdCli, nil
}
