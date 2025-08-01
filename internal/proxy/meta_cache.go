// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package proxy

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync"

	"github.com/cockroachdb/errors"
	"github.com/samber/lo"
	"go.uber.org/atomic"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"github.com/milvus-io/milvus/internal/types"
	"github.com/milvus-io/milvus/pkg/v2/common"
	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/metrics"
	"github.com/milvus-io/milvus/pkg/v2/proto/internalpb"
	"github.com/milvus-io/milvus/pkg/v2/proto/querypb"
	"github.com/milvus-io/milvus/pkg/v2/proto/rootcoordpb"
	"github.com/milvus-io/milvus/pkg/v2/util"
	"github.com/milvus-io/milvus/pkg/v2/util/commonpbutil"
	"github.com/milvus-io/milvus/pkg/v2/util/conc"
	"github.com/milvus-io/milvus/pkg/v2/util/expr"
	"github.com/milvus-io/milvus/pkg/v2/util/funcutil"
	"github.com/milvus-io/milvus/pkg/v2/util/merr"
	"github.com/milvus-io/milvus/pkg/v2/util/paramtable"
	"github.com/milvus-io/milvus/pkg/v2/util/timerecord"
	"github.com/milvus-io/milvus/pkg/v2/util/typeutil"
)

// Cache is the interface for system meta data cache
//
//go:generate mockery --name=Cache --filename=mock_cache_test.go --outpkg=proxy --output=. --inpackage --structname=MockCache --with-expecter
type Cache interface {
	// GetCollectionID get collection's id by name.
	GetCollectionID(ctx context.Context, database, collectionName string) (typeutil.UniqueID, error)
	// GetCollectionName get collection's name and database by id
	GetCollectionName(ctx context.Context, database string, collectionID int64) (string, error)
	// GetCollectionInfo get collection's information by name or collection id, such as schema, and etc.
	GetCollectionInfo(ctx context.Context, database, collectionName string, collectionID int64) (*collectionInfo, error)
	// GetPartitionID get partition's identifier of specific collection.
	GetPartitionID(ctx context.Context, database, collectionName string, partitionName string) (typeutil.UniqueID, error)
	// GetPartitions get all partitions' id of specific collection.
	GetPartitions(ctx context.Context, database, collectionName string) (map[string]typeutil.UniqueID, error)
	// GetPartitionInfo get partition's info.
	GetPartitionInfo(ctx context.Context, database, collectionName string, partitionName string) (*partitionInfo, error)
	// GetPartitionsIndex returns a partition names in partition key indexed order.
	GetPartitionsIndex(ctx context.Context, database, collectionName string) ([]string, error)
	// GetCollectionSchema get collection's schema.
	GetCollectionSchema(ctx context.Context, database, collectionName string) (*schemaInfo, error)
	GetShard(ctx context.Context, withCache bool, database, collectionName string, collectionID int64, channel string) ([]nodeInfo, error)
	GetShardLeaderList(ctx context.Context, database, collectionName string, collectionID int64, withCache bool) ([]string, error)
	DeprecateShardCache(database, collectionName string)
	InvalidateShardLeaderCache(collections []int64)
	ListShardLocation() map[int64]nodeInfo
	RemoveCollection(ctx context.Context, database, collectionName string)
	RemoveCollectionsByID(ctx context.Context, collectionID UniqueID, version uint64, removeVersion bool) []string

	// GetCredentialInfo operate credential cache
	GetCredentialInfo(ctx context.Context, username string) (*internalpb.CredentialInfo, error)
	RemoveCredential(username string)
	UpdateCredential(credInfo *internalpb.CredentialInfo)

	GetPrivilegeInfo(ctx context.Context) []string
	GetUserRole(username string) []string
	RefreshPolicyInfo(op typeutil.CacheOp) error
	InitPolicyInfo(info []string, userRoles []string)

	RemoveDatabase(ctx context.Context, database string)
	HasDatabase(ctx context.Context, database string) bool
	GetDatabaseInfo(ctx context.Context, database string) (*databaseInfo, error)
	// AllocID is only using on requests that need to skip timestamp allocation, don't overuse it.
	AllocID(ctx context.Context) (int64, error)
}

type collectionInfo struct {
	collID                typeutil.UniqueID
	schema                *schemaInfo
	partInfo              *partitionInfos
	createdTimestamp      uint64
	createdUtcTimestamp   uint64
	consistencyLevel      commonpb.ConsistencyLevel
	partitionKeyIsolation bool
	replicateID           string
	updateTimestamp       uint64
	collectionTTL         uint64
	numPartitions         int64
	vChannels             []string
	pChannels             []string
	shardsNum             int32
	aliases               []string
	properties            []*commonpb.KeyValuePair
}

type databaseInfo struct {
	dbID             typeutil.UniqueID
	properties       []*commonpb.KeyValuePair
	createdTimestamp uint64
}

// schemaInfo is a helper function wraps *schemapb.CollectionSchema
// with extra fields mapping and methods
type schemaInfo struct {
	*schemapb.CollectionSchema
	fieldMap             *typeutil.ConcurrentMap[string, int64] // field name to id mapping
	hasPartitionKeyField bool
	pkField              *schemapb.FieldSchema
	schemaHelper         *typeutil.SchemaHelper
}

func newSchemaInfo(schema *schemapb.CollectionSchema) *schemaInfo {
	fieldMap := typeutil.NewConcurrentMap[string, int64]()
	hasPartitionkey := false
	var pkField *schemapb.FieldSchema
	for _, field := range schema.GetFields() {
		fieldMap.Insert(field.GetName(), field.GetFieldID())
		if field.GetIsPartitionKey() {
			hasPartitionkey = true
		}
		if field.GetIsPrimaryKey() {
			pkField = field
		}
	}
	for _, structField := range schema.GetStructArrayFields() {
		fieldMap.Insert(structField.GetName(), structField.GetFieldID())
		for _, field := range structField.GetFields() {
			fieldMap.Insert(field.GetName(), field.GetFieldID())
		}
	}
	// skip load fields logic for now
	// partial load shall be processed as hint after tiered storage feature
	schemaHelper, _ := typeutil.CreateSchemaHelper(schema)
	return &schemaInfo{
		CollectionSchema:     schema,
		fieldMap:             fieldMap,
		hasPartitionKeyField: hasPartitionkey,
		pkField:              pkField,
		schemaHelper:         schemaHelper,
	}
}

func (s *schemaInfo) MapFieldID(name string) (int64, bool) {
	return s.fieldMap.Get(name)
}

func (s *schemaInfo) IsPartitionKeyCollection() bool {
	return s.hasPartitionKeyField
}

func (s *schemaInfo) GetPkField() (*schemapb.FieldSchema, error) {
	if s.pkField == nil {
		return nil, merr.WrapErrServiceInternal("pk field not found")
	}
	return s.pkField, nil
}

// GetLoadFieldIDs returns field id for load field list.
// If input `loadFields` is empty, use collection schema definition.
// Otherwise, perform load field list constraint check then return field id.
func (s *schemaInfo) GetLoadFieldIDs(loadFields []string, skipDynamicField bool) ([]int64, error) {
	if len(loadFields) == 0 {
		// skip check logic since create collection already did the rule check already
		return common.GetCollectionLoadFields(s.CollectionSchema, skipDynamicField), nil
	}

	fieldIDs := typeutil.NewSet[int64]()
	// fieldIDs := make([]int64, 0, len(loadFields))
	fields := make([]*schemapb.FieldSchema, 0, len(loadFields))
	for _, name := range loadFields {
		// todo(SpadeA): check struct field
		if structArrayField := s.schemaHelper.GetStructArrayFieldFromName(name); structArrayField != nil {
			for _, field := range structArrayField.GetFields() {
				fields = append(fields, field)
				fieldIDs.Insert(field.GetFieldID())
			}
			continue
		}

		fieldSchema, err := s.schemaHelper.GetFieldFromName(name)
		if err != nil {
			return nil, err
		}

		fields = append(fields, fieldSchema)
		fieldIDs.Insert(fieldSchema.GetFieldID())
	}

	// only append dynamic field when skipFlag == false
	if !skipDynamicField {
		// find dynamic field
		dynamicField := lo.FindOrElse(s.Fields, nil, func(field *schemapb.FieldSchema) bool {
			return field.IsDynamic
		})

		// if dynamic field not nil
		if dynamicField != nil {
			fieldIDs.Insert(dynamicField.GetFieldID())
			fields = append(fields, dynamicField)
		}
	}

	// validate load fields list
	if err := s.validateLoadFields(loadFields, fields); err != nil {
		return nil, err
	}

	return fieldIDs.Collect(), nil
}

func (s *schemaInfo) validateLoadFields(names []string, fields []*schemapb.FieldSchema) error {
	// ignore error if not found
	partitionKeyField, _ := s.schemaHelper.GetPartitionKeyField()
	clusteringKeyField, _ := s.schemaHelper.GetClusteringKeyField()

	var hasPrimaryKey, hasPartitionKey, hasClusteringKey, hasVector bool
	for _, field := range fields {
		if field.GetFieldID() == s.pkField.GetFieldID() {
			hasPrimaryKey = true
		}
		if typeutil.IsVectorType(field.GetDataType()) {
			hasVector = true
		}
		if field.IsPartitionKey {
			hasPartitionKey = true
		}
		if field.IsClusteringKey {
			hasClusteringKey = true
		}
	}

	if !hasPrimaryKey {
		return merr.WrapErrParameterInvalidMsg("load field list %v does not contain primary key field %s", names, s.pkField.GetName())
	}
	if !hasVector {
		return merr.WrapErrParameterInvalidMsg("load field list %v does not contain vector field", names)
	}
	if partitionKeyField != nil && !hasPartitionKey {
		return merr.WrapErrParameterInvalidMsg("load field list %v does not contain partition key field %s", names, partitionKeyField.GetName())
	}
	if clusteringKeyField != nil && !hasClusteringKey {
		return merr.WrapErrParameterInvalidMsg("load field list %v does not contain clustering key field %s", names, clusteringKeyField.GetName())
	}
	return nil
}

func (s *schemaInfo) CanRetrieveRawFieldData(field *schemapb.FieldSchema) bool {
	return s.schemaHelper.CanRetrieveRawFieldData(field)
}

// partitionInfos contains the cached collection partition informations.
type partitionInfos struct {
	partitionInfos        []*partitionInfo
	name2Info             map[string]*partitionInfo // map[int64]*partitionInfo
	name2ID               map[string]int64          // map[int64]*partitionInfo
	indexedPartitionNames []string
}

// partitionInfo single model for partition information.
type partitionInfo struct {
	name                string
	partitionID         typeutil.UniqueID
	createdTimestamp    uint64
	createdUtcTimestamp uint64
	isDefault           bool
}

func (info *collectionInfo) isCollectionCached() bool {
	return info != nil && info.collID != UniqueID(0) && info.schema != nil
}

// shardLeaders wraps shard leader mapping for iteration.
type shardLeaders struct {
	idx          *atomic.Int64
	collectionID int64
	shardLeaders map[string][]nodeInfo
}

func (sl *shardLeaders) Get(channel string) []nodeInfo {
	return sl.shardLeaders[channel]
}

func (sl *shardLeaders) GetShardLeaderList() []string {
	return lo.Keys(sl.shardLeaders)
}

type shardLeadersReader struct {
	leaders *shardLeaders
	idx     int64
}

// Shuffle returns the shuffled shard leader list.
func (it shardLeadersReader) Shuffle() map[string][]nodeInfo {
	result := make(map[string][]nodeInfo)
	for channel, leaders := range it.leaders.shardLeaders {
		l := len(leaders)
		// shuffle all replica at random order
		shuffled := make([]nodeInfo, l)
		for i, randIndex := range rand.Perm(l) {
			shuffled[i] = leaders[randIndex]
		}

		// make each copy has same probability to be first replica
		for index, leader := range shuffled {
			if leader == leaders[int(it.idx)%l] {
				shuffled[0], shuffled[index] = shuffled[index], shuffled[0]
			}
		}

		result[channel] = shuffled
	}
	return result
}

// GetReader returns shuffer reader for shard leader.
func (sl *shardLeaders) GetReader() shardLeadersReader {
	idx := sl.idx.Inc()
	return shardLeadersReader{
		leaders: sl,
		idx:     idx,
	}
}

// make sure MetaCache implements Cache.
var _ Cache = (*MetaCache)(nil)

// MetaCache implements Cache, provides collection meta cache based on internal RootCoord
type MetaCache struct {
	mixCoord types.MixCoordClient

	dbInfo         map[string]*databaseInfo              // database -> db_info
	collInfo       map[string]map[string]*collectionInfo // database -> collectionName -> collection_info
	collLeader     map[string]map[string]*shardLeaders   // database -> collectionName -> collection_leaders
	credMap        map[string]*internalpb.CredentialInfo // cache for credential, lazy load
	privilegeInfos map[string]struct{}                   // privileges cache
	userToRoles    map[string]map[string]struct{}        // user to role cache
	mu             sync.RWMutex
	credMut        sync.RWMutex
	leaderMut      sync.RWMutex
	shardMgr       shardClientMgr
	sfGlobal       conc.Singleflight[*collectionInfo]
	sfDB           conc.Singleflight[*databaseInfo]

	IDStart int64
	IDCount int64
	IDIndex int64
	IDLock  sync.RWMutex

	collectionCacheVersion map[UniqueID]uint64 // collectionID -> cacheVersion
}

// globalMetaCache is singleton instance of Cache
var globalMetaCache Cache

// InitMetaCache initializes globalMetaCache
func InitMetaCache(ctx context.Context, mixCoord types.MixCoordClient, shardMgr shardClientMgr) error {
	var err error
	globalMetaCache, err = NewMetaCache(mixCoord, shardMgr)
	if err != nil {
		return err
	}
	expr.Register("cache", globalMetaCache)

	// The privilege info is a little more. And to get this info, the query operation of involving multiple table queries is required.
	resp, err := mixCoord.ListPolicy(ctx, &internalpb.ListPolicyRequest{})
	if err = merr.CheckRPCCall(resp, err); err != nil {
		log.Error("fail to init meta cache", zap.Error(err))
		return err
	}
	globalMetaCache.InitPolicyInfo(resp.PolicyInfos, resp.UserRoles)
	log.Info("success to init meta cache", zap.Strings("policy_infos", resp.PolicyInfos))
	return nil
}

// NewMetaCache creates a MetaCache with provided RootCoord and QueryNode
func NewMetaCache(mixCoord types.MixCoordClient, shardMgr shardClientMgr) (*MetaCache, error) {
	return &MetaCache{
		mixCoord:               mixCoord,
		dbInfo:                 map[string]*databaseInfo{},
		collInfo:               map[string]map[string]*collectionInfo{},
		collLeader:             map[string]map[string]*shardLeaders{},
		credMap:                map[string]*internalpb.CredentialInfo{},
		shardMgr:               shardMgr,
		privilegeInfos:         map[string]struct{}{},
		userToRoles:            map[string]map[string]struct{}{},
		collectionCacheVersion: make(map[UniqueID]uint64),
	}, nil
}

func (m *MetaCache) getCollection(database, collectionName string, collectionID UniqueID) (*collectionInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	db, ok := m.collInfo[database]
	if !ok {
		return nil, false
	}
	if collectionName == "" {
		for _, collection := range db {
			if collection.collID == collectionID {
				return collection, collection.isCollectionCached()
			}
		}
	} else {
		if collection, ok := db[collectionName]; ok {
			return collection, collection.isCollectionCached()
		}
	}

	return nil, false
}

func (m *MetaCache) update(ctx context.Context, database, collectionName string, collectionID UniqueID) (*collectionInfo, error) {
	if collInfo, ok := m.getCollection(database, collectionName, collectionID); ok {
		return collInfo, nil
	}

	collection, err := m.describeCollection(ctx, database, collectionName, collectionID)
	if err != nil {
		return nil, err
	}

	partitions, err := m.showPartitions(ctx, database, collectionName, collectionID)
	if err != nil {
		return nil, err
	}

	// check partitionID, createdTimestamp and utcstamp has sam element numbers
	if len(partitions.PartitionNames) != len(partitions.CreatedTimestamps) || len(partitions.PartitionNames) != len(partitions.CreatedUtcTimestamps) {
		return nil, merr.WrapErrParameterInvalidMsg("partition names and timestamps number is not aligned, response: %s", partitions.String())
	}

	defaultPartitionName := Params.CommonCfg.DefaultPartitionName.GetValue()
	infos := lo.Map(partitions.GetPartitionIDs(), func(partitionID int64, idx int) *partitionInfo {
		return &partitionInfo{
			name:                partitions.PartitionNames[idx],
			partitionID:         partitions.PartitionIDs[idx],
			createdTimestamp:    partitions.CreatedTimestamps[idx],
			createdUtcTimestamp: partitions.CreatedUtcTimestamps[idx],
			isDefault:           partitions.PartitionNames[idx] == defaultPartitionName,
		}
	})

	if collectionName == "" {
		collectionName = collection.Schema.GetName()
	}
	if database == "" {
		log.Warn("database is empty, use default database name", zap.String("collectionName", collectionName), zap.Stack("stack"))
	}
	isolation, err := common.IsPartitionKeyIsolationKvEnabled(collection.Properties...)
	if err != nil {
		return nil, err
	}

	schemaInfo := newSchemaInfo(collection.Schema)

	m.mu.Lock()
	defer m.mu.Unlock()
	curVersion := m.collectionCacheVersion[collection.GetCollectionID()]
	// Compatibility logic: if the rootcoord version is lower(requestTime = 0), update the cache directly.
	if collection.GetRequestTime() < curVersion && collection.GetRequestTime() != 0 {
		log.Debug("describe collection timestamp less than version, don't update cache",
			zap.String("collectionName", collectionName),
			zap.Uint64("version", collection.GetRequestTime()), zap.Uint64("cache version", curVersion))
		return &collectionInfo{
			collID:                collection.CollectionID,
			schema:                schemaInfo,
			partInfo:              parsePartitionsInfo(infos, schemaInfo.hasPartitionKeyField),
			createdTimestamp:      collection.CreatedTimestamp,
			createdUtcTimestamp:   collection.CreatedUtcTimestamp,
			consistencyLevel:      collection.ConsistencyLevel,
			partitionKeyIsolation: isolation,
			updateTimestamp:       collection.UpdateTimestamp,
			collectionTTL:         getCollectionTTL(schemaInfo.CollectionSchema.GetProperties()),
			vChannels:             collection.VirtualChannelNames,
			pChannels:             collection.PhysicalChannelNames,
			numPartitions:         collection.NumPartitions,
			shardsNum:             collection.ShardsNum,
			aliases:               collection.Aliases,
			properties:            collection.Properties,
		}, nil
	}
	_, dbOk := m.collInfo[database]
	if !dbOk {
		m.collInfo[database] = make(map[string]*collectionInfo)
	}

	replicateID, _ := common.GetReplicateID(collection.Properties)
	m.collInfo[database][collectionName] = &collectionInfo{
		collID:                collection.CollectionID,
		schema:                schemaInfo,
		partInfo:              parsePartitionsInfo(infos, schemaInfo.hasPartitionKeyField),
		createdTimestamp:      collection.CreatedTimestamp,
		createdUtcTimestamp:   collection.CreatedUtcTimestamp,
		consistencyLevel:      collection.ConsistencyLevel,
		partitionKeyIsolation: isolation,
		replicateID:           replicateID,
		updateTimestamp:       collection.UpdateTimestamp,
		collectionTTL:         getCollectionTTL(schemaInfo.CollectionSchema.GetProperties()),
		vChannels:             collection.VirtualChannelNames,
		pChannels:             collection.PhysicalChannelNames,
		numPartitions:         collection.NumPartitions,
		shardsNum:             collection.ShardsNum,
		aliases:               collection.Aliases,
		properties:            collection.Properties,
	}

	log.Ctx(ctx).Info("meta update success", zap.String("database", database), zap.String("collectionName", collectionName),
		zap.String("actual collection Name", collection.Schema.GetName()), zap.Int64("collectionID", collection.CollectionID),
		zap.Strings("partition", partitions.PartitionNames), zap.Uint64("currentVersion", curVersion),
		zap.Uint64("version", collection.GetRequestTime()), zap.Any("aliases", collection.Aliases),
	)

	m.collectionCacheVersion[collection.GetCollectionID()] = collection.GetRequestTime()
	collInfo := m.collInfo[database][collectionName]

	return collInfo, nil
}

func buildSfKeyByName(database, collectionName string) string {
	return database + "-" + collectionName
}

func buildSfKeyById(database string, collectionID UniqueID) string {
	return database + "--" + fmt.Sprint(collectionID)
}

func (m *MetaCache) UpdateByName(ctx context.Context, database, collectionName string) (*collectionInfo, error) {
	collection, err, _ := m.sfGlobal.Do(buildSfKeyByName(database, collectionName), func() (*collectionInfo, error) {
		return m.update(ctx, database, collectionName, 0)
	})
	return collection, err
}

func (m *MetaCache) UpdateByID(ctx context.Context, database string, collectionID UniqueID) (*collectionInfo, error) {
	collection, err, _ := m.sfGlobal.Do(buildSfKeyById(database, collectionID), func() (*collectionInfo, error) {
		return m.update(ctx, database, "", collectionID)
	})
	return collection, err
}

// GetCollectionID returns the corresponding collection id for provided collection name
func (m *MetaCache) GetCollectionID(ctx context.Context, database, collectionName string) (UniqueID, error) {
	method := "GetCollectionID"
	collInfo, ok := m.getCollection(database, collectionName, 0)
	if !ok {
		metrics.ProxyCacheStatsCounter.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), method, metrics.CacheMissLabel).Inc()
		tr := timerecord.NewTimeRecorder("UpdateCache")

		collInfo, err := m.UpdateByName(ctx, database, collectionName)
		if err != nil {
			return UniqueID(0), err
		}

		metrics.ProxyUpdateCacheLatency.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), method).Observe(float64(tr.ElapseSpan().Milliseconds()))
		return collInfo.collID, nil
	}
	metrics.ProxyCacheStatsCounter.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), method, metrics.CacheHitLabel).Inc()

	return collInfo.collID, nil
}

// GetCollectionName returns the corresponding collection name for provided collection id
func (m *MetaCache) GetCollectionName(ctx context.Context, database string, collectionID int64) (string, error) {
	method := "GetCollectionName"
	collInfo, ok := m.getCollection(database, "", collectionID)

	if !ok {
		metrics.ProxyCacheStatsCounter.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), method, metrics.CacheMissLabel).Inc()
		tr := timerecord.NewTimeRecorder("UpdateCache")

		collInfo, err := m.UpdateByID(ctx, database, collectionID)
		if err != nil {
			return "", err
		}

		metrics.ProxyUpdateCacheLatency.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), method).Observe(float64(tr.ElapseSpan().Milliseconds()))
		return collInfo.schema.Name, nil
	}
	metrics.ProxyCacheStatsCounter.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), method, metrics.CacheHitLabel).Inc()

	return collInfo.schema.Name, nil
}

func (m *MetaCache) GetCollectionInfo(ctx context.Context, database string, collectionName string, collectionID int64) (*collectionInfo, error) {
	collInfo, ok := m.getCollection(database, collectionName, 0)

	method := "GetCollectionInfo"
	// if collInfo.collID != collectionID, means that the cache is not trustable
	// try to get collection according to collectionID
	// Why use collectionID? Because the collectionID is not always provided in the proxy.
	if !ok || (collectionID != 0 && collInfo.collID != collectionID) {
		tr := timerecord.NewTimeRecorder("UpdateCache")
		metrics.ProxyCacheStatsCounter.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), method, metrics.CacheMissLabel).Inc()

		if collectionID == 0 {
			collInfo, err := m.UpdateByName(ctx, database, collectionName)
			if err != nil {
				return nil, err
			}
			metrics.ProxyUpdateCacheLatency.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), method).Observe(float64(tr.ElapseSpan().Milliseconds()))
			return collInfo, nil
		}
		collInfo, err := m.UpdateByID(ctx, database, collectionID)
		if err != nil {
			return nil, err
		}
		metrics.ProxyUpdateCacheLatency.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), method).Observe(float64(tr.ElapseSpan().Milliseconds()))
		return collInfo, nil
	}

	metrics.ProxyCacheStatsCounter.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), method, metrics.CacheHitLabel).Inc()
	return collInfo, nil
}

// GetCollectionInfo returns the collection information related to provided collection name
// If the information is not found, proxy will try to fetch information for other source (RootCoord for now)
// TODO: may cause data race of this implementation, should be refactored in future.
func (m *MetaCache) getFullCollectionInfo(ctx context.Context, database, collectionName string, collectionID int64) (*collectionInfo, error) {
	collInfo, ok := m.getCollection(database, collectionName, collectionID)

	method := "GetCollectionInfo"
	// if collInfo.collID != collectionID, means that the cache is not trustable
	// try to get collection according to collectionID
	if !ok || collInfo.collID != collectionID {
		tr := timerecord.NewTimeRecorder("UpdateCache")
		metrics.ProxyCacheStatsCounter.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), method, metrics.CacheMissLabel).Inc()

		collInfo, err := m.UpdateByID(ctx, database, collectionID)
		if err != nil {
			return nil, err
		}
		metrics.ProxyUpdateCacheLatency.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), method).Observe(float64(tr.ElapseSpan().Milliseconds()))
		return collInfo, nil
	}

	metrics.ProxyCacheStatsCounter.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), method, metrics.CacheHitLabel).Inc()
	return collInfo, nil
}

func (m *MetaCache) GetCollectionSchema(ctx context.Context, database, collectionName string) (*schemaInfo, error) {
	collInfo, ok := m.getCollection(database, collectionName, 0)

	method := "GetCollectionSchema"
	if !ok {
		tr := timerecord.NewTimeRecorder("UpdateCache")
		metrics.ProxyCacheStatsCounter.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), method, metrics.CacheMissLabel).Inc()

		collInfo, err := m.UpdateByName(ctx, database, collectionName)
		if err != nil {
			return nil, err
		}
		metrics.ProxyUpdateCacheLatency.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), method).Observe(float64(tr.ElapseSpan().Milliseconds()))
		log.Ctx(ctx).Debug("Reload collection from root coordinator ",
			zap.String("collectionName", collectionName),
			zap.Int64("time (milliseconds) take ", tr.ElapseSpan().Milliseconds()))
		return collInfo.schema, nil
	}
	metrics.ProxyCacheStatsCounter.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), method, metrics.CacheHitLabel).Inc()

	return collInfo.schema, nil
}

func (m *MetaCache) GetPartitionID(ctx context.Context, database, collectionName string, partitionName string) (typeutil.UniqueID, error) {
	partInfo, err := m.GetPartitionInfo(ctx, database, collectionName, partitionName)
	if err != nil {
		return 0, err
	}
	return partInfo.partitionID, nil
}

func (m *MetaCache) GetPartitions(ctx context.Context, database, collectionName string) (map[string]typeutil.UniqueID, error) {
	partitions, err := m.GetPartitionInfos(ctx, database, collectionName)
	if err != nil {
		return nil, err
	}

	return partitions.name2ID, nil
}

func (m *MetaCache) GetPartitionInfo(ctx context.Context, database, collectionName string, partitionName string) (*partitionInfo, error) {
	partitions, err := m.GetPartitionInfos(ctx, database, collectionName)
	if err != nil {
		return nil, err
	}

	if partitionName == "" {
		for _, info := range partitions.partitionInfos {
			if info.isDefault {
				return info, nil
			}
		}
	}

	info, ok := partitions.name2Info[partitionName]
	if !ok {
		return nil, merr.WrapErrPartitionNotFound(partitionName)
	}
	return info, nil
}

func (m *MetaCache) GetPartitionsIndex(ctx context.Context, database, collectionName string) ([]string, error) {
	partitions, err := m.GetPartitionInfos(ctx, database, collectionName)
	if err != nil {
		return nil, err
	}

	if partitions.indexedPartitionNames == nil {
		return nil, merr.WrapErrServiceInternal("partitions not in partition key naming pattern")
	}

	return partitions.indexedPartitionNames, nil
}

func (m *MetaCache) GetPartitionInfos(ctx context.Context, database, collectionName string) (*partitionInfos, error) {
	method := "GetPartitionInfo"
	collInfo, ok := m.getCollection(database, collectionName, 0)

	if !ok {
		tr := timerecord.NewTimeRecorder("UpdateCache")
		metrics.ProxyCacheStatsCounter.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), method, metrics.CacheMissLabel).Inc()

		collInfo, err := m.UpdateByName(ctx, database, collectionName)
		if err != nil {
			return nil, err
		}

		metrics.ProxyUpdateCacheLatency.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), method).Observe(float64(tr.ElapseSpan().Milliseconds()))
		return collInfo.partInfo, nil
	}
	return collInfo.partInfo, nil
}

// Get the collection information from rootcoord.
func (m *MetaCache) describeCollection(ctx context.Context, database, collectionName string, collectionID int64) (*milvuspb.DescribeCollectionResponse, error) {
	req := &milvuspb.DescribeCollectionRequest{
		Base: commonpbutil.NewMsgBase(
			commonpbutil.WithMsgType(commonpb.MsgType_DescribeCollection),
		),
		DbName:         database,
		CollectionName: collectionName,
		CollectionID:   collectionID,
	}
	coll, err := m.mixCoord.DescribeCollection(ctx, req)
	if err != nil {
		return nil, err
	}
	err = merr.Error(coll.GetStatus())
	if err != nil {
		return nil, err
	}
	userFields := make([]*schemapb.FieldSchema, 0)
	for _, field := range coll.Schema.Fields {
		if field.FieldID >= common.StartOfUserFieldID {
			userFields = append(userFields, field)
		}
	}
	coll.Schema.Fields = userFields
	return coll, nil
}

func (m *MetaCache) showPartitions(ctx context.Context, dbName string, collectionName string, collectionID UniqueID) (*milvuspb.ShowPartitionsResponse, error) {
	req := &milvuspb.ShowPartitionsRequest{
		Base: commonpbutil.NewMsgBase(
			commonpbutil.WithMsgType(commonpb.MsgType_ShowPartitions),
		),
		DbName:         dbName,
		CollectionName: collectionName,
		CollectionID:   collectionID,
	}

	partitions, err := m.mixCoord.ShowPartitions(ctx, req)
	if err != nil {
		return nil, err
	}

	if err := merr.Error(partitions.GetStatus()); err != nil {
		return nil, err
	}

	if len(partitions.PartitionIDs) != len(partitions.PartitionNames) {
		return nil, fmt.Errorf("partition ids len: %d doesn't equal Partition name len %d",
			len(partitions.PartitionIDs), len(partitions.PartitionNames))
	}

	return partitions, nil
}

func (m *MetaCache) describeDatabase(ctx context.Context, dbName string) (*rootcoordpb.DescribeDatabaseResponse, error) {
	req := &rootcoordpb.DescribeDatabaseRequest{
		DbName: dbName,
	}

	resp, err := m.mixCoord.DescribeDatabase(ctx, req)
	if err = merr.CheckRPCCall(resp, err); err != nil {
		return nil, err
	}

	return resp, nil
}

// parsePartitionsInfo parse partitionInfo list to partitionInfos struct.
// prepare all name to id & info map
// try parse partition names to partitionKey index.
func parsePartitionsInfo(infos []*partitionInfo, hasPartitionKey bool) *partitionInfos {
	name2ID := lo.SliceToMap(infos, func(info *partitionInfo) (string, int64) {
		return info.name, info.partitionID
	})
	name2Info := lo.SliceToMap(infos, func(info *partitionInfo) (string, *partitionInfo) {
		return info.name, info
	})

	result := &partitionInfos{
		partitionInfos: infos,
		name2ID:        name2ID,
		name2Info:      name2Info,
	}

	if !hasPartitionKey {
		return result
	}

	// Make sure the order of the partition names got every time is the same
	partitionNames := make([]string, len(infos))
	for _, info := range infos {
		partitionName := info.name
		splits := strings.Split(partitionName, "_")
		if len(splits) < 2 {
			log.Info("partition group not in partitionKey pattern", zap.String("partitionName", partitionName))
			return result
		}
		index, err := strconv.ParseInt(splits[len(splits)-1], 10, 64)
		if err != nil {
			log.Info("partition group not in partitionKey pattern", zap.String("partitionName", partitionName), zap.Error(err))
			return result
		}
		partitionNames[index] = partitionName
	}

	result.indexedPartitionNames = partitionNames
	return result
}

func (m *MetaCache) RemoveCollection(ctx context.Context, database, collectionName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, dbOk := m.collInfo[database]
	if dbOk {
		delete(m.collInfo[database], collectionName)
	}
	if database == "" {
		delete(m.collInfo[defaultDB], collectionName)
	}
	log.Ctx(ctx).Debug("remove collection", zap.String("db", database), zap.String("collection", collectionName), zap.Bool("dbok", dbOk))
}

func (m *MetaCache) RemoveCollectionsByID(ctx context.Context, collectionID UniqueID, version uint64, removeVersion bool) []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	curVersion := m.collectionCacheVersion[collectionID]
	var collNames []string
	for database, db := range m.collInfo {
		for k, v := range db {
			if v.collID == collectionID {
				if version == 0 || curVersion <= version {
					delete(m.collInfo[database], k)
					collNames = append(collNames, k)
				}
			}
		}
	}
	if removeVersion {
		delete(m.collectionCacheVersion, collectionID)
	} else if version != 0 {
		m.collectionCacheVersion[collectionID] = version
	}
	log.Ctx(ctx).Debug("remove collection by id", zap.Int64("id", collectionID),
		zap.Strings("collection", collNames), zap.Uint64("currentVersion", curVersion),
		zap.Uint64("version", version), zap.Bool("removeVersion", removeVersion))
	return collNames
}

// GetCredentialInfo returns the credential related to provided username
// If the cache missed, proxy will try to fetch from storage
func (m *MetaCache) GetCredentialInfo(ctx context.Context, username string) (*internalpb.CredentialInfo, error) {
	m.credMut.RLock()
	var credInfo *internalpb.CredentialInfo
	credInfo, ok := m.credMap[username]
	m.credMut.RUnlock()

	if !ok {
		req := &rootcoordpb.GetCredentialRequest{
			Base: commonpbutil.NewMsgBase(
				commonpbutil.WithMsgType(commonpb.MsgType_GetCredential),
			),
			Username: username,
		}
		resp, err := m.mixCoord.GetCredential(ctx, req)
		if err != nil {
			return &internalpb.CredentialInfo{}, err
		}
		credInfo = &internalpb.CredentialInfo{
			Username:          resp.Username,
			EncryptedPassword: resp.Password,
		}
	}

	return credInfo, nil
}

func (m *MetaCache) RemoveCredential(username string) {
	m.credMut.Lock()
	defer m.credMut.Unlock()
	// delete pair in credMap
	delete(m.credMap, username)
}

func (m *MetaCache) UpdateCredential(credInfo *internalpb.CredentialInfo) {
	m.credMut.Lock()
	defer m.credMut.Unlock()
	username := credInfo.Username
	_, ok := m.credMap[username]
	if !ok {
		m.credMap[username] = &internalpb.CredentialInfo{}
	}

	// Do not cache encrypted password content
	m.credMap[username].Username = username
	m.credMap[username].Sha256Password = credInfo.Sha256Password
}

func (m *MetaCache) GetShard(ctx context.Context, withCache bool, database, collectionName string, collectionID int64, channel string) ([]nodeInfo, error) {
	method := "GetShard"
	// check cache first
	cacheShardLeaders := m.getCachedShardLeaders(database, collectionName, method)
	if cacheShardLeaders == nil || !withCache {
		// refresh shard leader cache
		newShardLeaders, err := m.updateShardLocationCache(ctx, database, collectionName, collectionID)
		if err != nil {
			return nil, err
		}
		cacheShardLeaders = newShardLeaders
	}

	return cacheShardLeaders.Get(channel), nil
}

func (m *MetaCache) GetShardLeaderList(ctx context.Context, database, collectionName string, collectionID int64, withCache bool) ([]string, error) {
	method := "GetShardLeaderList"
	// check cache first
	cacheShardLeaders := m.getCachedShardLeaders(database, collectionName, method)
	if cacheShardLeaders == nil || !withCache {
		// refresh shard leader cache
		newShardLeaders, err := m.updateShardLocationCache(ctx, database, collectionName, collectionID)
		if err != nil {
			return nil, err
		}
		cacheShardLeaders = newShardLeaders
	}

	return cacheShardLeaders.GetShardLeaderList(), nil
}

func (m *MetaCache) getCachedShardLeaders(database, collectionName, caller string) *shardLeaders {
	m.leaderMut.RLock()
	var cacheShardLeaders *shardLeaders
	db, ok := m.collLeader[database]
	if !ok {
		cacheShardLeaders = nil
	} else {
		cacheShardLeaders = db[collectionName]
	}
	m.leaderMut.RUnlock()

	if cacheShardLeaders != nil {
		metrics.ProxyCacheStatsCounter.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), caller, metrics.CacheHitLabel).Inc()
	} else {
		metrics.ProxyCacheStatsCounter.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), caller, metrics.CacheMissLabel).Inc()
	}

	return cacheShardLeaders
}

func (m *MetaCache) updateShardLocationCache(ctx context.Context, database, collectionName string, collectionID int64) (*shardLeaders, error) {
	log := log.Ctx(ctx).With(
		zap.String("db", database),
		zap.String("collectionName", collectionName),
		zap.Int64("collectionID", collectionID))

	method := "updateShardLocationCache"
	tr := timerecord.NewTimeRecorder(method)
	defer metrics.ProxyUpdateCacheLatency.WithLabelValues(fmt.Sprint(paramtable.GetNodeID()), method).
		Observe(float64(tr.ElapseSpan().Milliseconds()))

	req := &querypb.GetShardLeadersRequest{
		Base: commonpbutil.NewMsgBase(
			commonpbutil.WithMsgType(commonpb.MsgType_GetShardLeaders),
			commonpbutil.WithSourceID(paramtable.GetNodeID()),
		),
		CollectionID:            collectionID,
		WithUnserviceableShards: true,
	}
	resp, err := m.mixCoord.GetShardLeaders(ctx, req)
	if err := merr.CheckRPCCall(resp.GetStatus(), err); err != nil {
		log.Error("failed to get shard locations",
			zap.Int64("collectionID", collectionID),
			zap.Error(err))
		return nil, err
	}

	shards := parseShardLeaderList2QueryNode(resp.GetShards())

	// convert shards map to string for logging
	if log.Logger.Level() == zap.DebugLevel {
		shardStr := make([]string, 0, len(shards))
		for channel, nodes := range shards {
			nodeStrs := make([]string, 0, len(nodes))
			for _, node := range nodes {
				nodeStrs = append(nodeStrs, node.String())
			}
			shardStr = append(shardStr, fmt.Sprintf("%s:[%s]", channel, strings.Join(nodeStrs, ", ")))
		}
		log.Debug("update shard leader cache", zap.String("newShardLeaders", strings.Join(shardStr, ", ")))
	}

	newShardLeaders := &shardLeaders{
		collectionID: collectionID,
		shardLeaders: shards,
		idx:          atomic.NewInt64(0),
	}

	m.leaderMut.Lock()
	if _, ok := m.collLeader[database]; !ok {
		m.collLeader[database] = make(map[string]*shardLeaders)
	}
	m.collLeader[database][collectionName] = newShardLeaders
	m.leaderMut.Unlock()

	return newShardLeaders, nil
}

func parseShardLeaderList2QueryNode(shardsLeaders []*querypb.ShardLeadersList) map[string][]nodeInfo {
	shard2QueryNodes := make(map[string][]nodeInfo)

	for _, leaders := range shardsLeaders {
		qns := make([]nodeInfo, len(leaders.GetNodeIds()))

		for j := range qns {
			qns[j] = nodeInfo{leaders.GetNodeIds()[j], leaders.GetNodeAddrs()[j], leaders.GetServiceable()[j]}
		}

		shard2QueryNodes[leaders.GetChannelName()] = qns
	}

	return shard2QueryNodes
}

// used for Garbage collection shard client
func (m *MetaCache) ListShardLocation() map[int64]nodeInfo {
	m.leaderMut.RLock()
	defer m.leaderMut.RUnlock()
	shardLeaderInfo := make(map[int64]nodeInfo)

	for _, dbInfo := range m.collLeader {
		for _, shardLeaders := range dbInfo {
			for _, nodeInfos := range shardLeaders.shardLeaders {
				for _, node := range nodeInfos {
					shardLeaderInfo[node.nodeID] = node
				}
			}
		}
	}
	return shardLeaderInfo
}

// DeprecateShardCache clear the shard leader cache of a collection
func (m *MetaCache) DeprecateShardCache(database, collectionName string) {
	log.Info("deprecate shard cache for collection", zap.String("collectionName", collectionName))
	m.leaderMut.Lock()
	defer m.leaderMut.Unlock()
	dbInfo, ok := m.collLeader[database]
	if ok {
		delete(dbInfo, collectionName)
		if len(dbInfo) == 0 {
			delete(m.collLeader, database)
		}
	}
}

// InvalidateShardLeaderCache called when Shard leader balance happened
func (m *MetaCache) InvalidateShardLeaderCache(collections []int64) {
	log.Info("Invalidate shard cache for collections", zap.Int64s("collectionIDs", collections))
	m.leaderMut.Lock()
	defer m.leaderMut.Unlock()
	collectionSet := typeutil.NewUniqueSet(collections...)
	for dbName, dbInfo := range m.collLeader {
		for collectionName, shardLeaders := range dbInfo {
			if collectionSet.Contain(shardLeaders.collectionID) {
				delete(dbInfo, collectionName)
			}
		}
		if len(dbInfo) == 0 {
			delete(m.collLeader, dbName)
		}
	}
}

func (m *MetaCache) InitPolicyInfo(info []string, userRoles []string) {
	defer func() {
		err := getEnforcer().LoadPolicy()
		if err != nil {
			log.Error("failed to load policy after RefreshPolicyInfo", zap.Error(err))
		}
		CleanPrivilegeCache()
	}()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unsafeInitPolicyInfo(info, userRoles)
}

func (m *MetaCache) unsafeInitPolicyInfo(info []string, userRoles []string) {
	m.privilegeInfos = util.StringSet(info)
	for _, userRole := range userRoles {
		user, role, err := funcutil.DecodeUserRoleCache(userRole)
		if err != nil {
			log.Warn("invalid user-role key", zap.String("user-role", userRole), zap.Error(err))
			continue
		}
		if m.userToRoles[user] == nil {
			m.userToRoles[user] = make(map[string]struct{})
		}
		m.userToRoles[user][role] = struct{}{}
	}
}

func (m *MetaCache) GetPrivilegeInfo(ctx context.Context) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return util.StringList(m.privilegeInfos)
}

func (m *MetaCache) GetUserRole(user string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return util.StringList(m.userToRoles[user])
}

func (m *MetaCache) RefreshPolicyInfo(op typeutil.CacheOp) (err error) {
	defer func() {
		if err == nil {
			le := getEnforcer().LoadPolicy()
			if le != nil {
				log.Error("failed to load policy after RefreshPolicyInfo", zap.Error(le))
			}
			CleanPrivilegeCache()
		}
	}()
	if op.OpType != typeutil.CacheRefresh {
		m.mu.Lock()
		defer m.mu.Unlock()
		if op.OpKey == "" {
			return errors.New("empty op key")
		}
	}

	switch op.OpType {
	case typeutil.CacheGrantPrivilege:
		keys := funcutil.PrivilegesForPolicy(op.OpKey)
		for _, key := range keys {
			m.privilegeInfos[key] = struct{}{}
		}
	case typeutil.CacheRevokePrivilege:
		keys := funcutil.PrivilegesForPolicy(op.OpKey)
		for _, key := range keys {
			delete(m.privilegeInfos, key)
		}
	case typeutil.CacheAddUserToRole:
		user, role, err := funcutil.DecodeUserRoleCache(op.OpKey)
		if err != nil {
			return fmt.Errorf("invalid opKey, fail to decode, op_type: %d, op_key: %s", int(op.OpType), op.OpKey)
		}
		if m.userToRoles[user] == nil {
			m.userToRoles[user] = make(map[string]struct{})
		}
		m.userToRoles[user][role] = struct{}{}
	case typeutil.CacheRemoveUserFromRole:
		user, role, err := funcutil.DecodeUserRoleCache(op.OpKey)
		if err != nil {
			return fmt.Errorf("invalid opKey, fail to decode, op_type: %d, op_key: %s", int(op.OpType), op.OpKey)
		}
		if m.userToRoles[user] != nil {
			delete(m.userToRoles[user], role)
		}
	case typeutil.CacheDeleteUser:
		delete(m.userToRoles, op.OpKey)
	case typeutil.CacheDropRole:
		for user := range m.userToRoles {
			delete(m.userToRoles[user], op.OpKey)
		}

		for policy := range m.privilegeInfos {
			if funcutil.PolicyCheckerWithRole(policy, op.OpKey) {
				delete(m.privilegeInfos, policy)
			}
		}
	case typeutil.CacheRefresh:
		resp, err := m.mixCoord.ListPolicy(context.Background(), &internalpb.ListPolicyRequest{})
		if err != nil {
			log.Error("fail to init meta cache", zap.Error(err))
			return err
		}

		if !merr.Ok(resp.GetStatus()) {
			log.Error("fail to init meta cache",
				zap.String("error_code", resp.GetStatus().GetErrorCode().String()),
				zap.String("reason", resp.GetStatus().GetReason()))
			return merr.Error(resp.Status)
		}

		m.mu.Lock()
		defer m.mu.Unlock()
		m.userToRoles = make(map[string]map[string]struct{})
		m.privilegeInfos = make(map[string]struct{})
		m.unsafeInitPolicyInfo(resp.PolicyInfos, resp.UserRoles)
	default:
		return fmt.Errorf("invalid opType, op_type: %d, op_key: %s", int(op.OpType), op.OpKey)
	}
	return nil
}

func (m *MetaCache) RemoveDatabase(ctx context.Context, database string) {
	log.Ctx(ctx).Debug("remove database", zap.String("name", database))
	m.mu.Lock()
	delete(m.collInfo, database)
	delete(m.dbInfo, database)
	m.mu.Unlock()

	m.leaderMut.Lock()
	delete(m.collLeader, database)
	m.leaderMut.Unlock()
}

func (m *MetaCache) HasDatabase(ctx context.Context, database string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.collInfo[database]
	return ok
}

func (m *MetaCache) GetDatabaseInfo(ctx context.Context, database string) (*databaseInfo, error) {
	dbInfo := m.safeGetDBInfo(database)
	if dbInfo != nil {
		return dbInfo, nil
	}

	dbInfo, err, _ := m.sfDB.Do(database, func() (*databaseInfo, error) {
		resp, err := m.describeDatabase(ctx, database)
		if err != nil {
			return nil, err
		}

		m.mu.Lock()
		defer m.mu.Unlock()
		dbInfo := &databaseInfo{
			dbID:             resp.GetDbID(),
			properties:       resp.Properties,
			createdTimestamp: resp.GetCreatedTimestamp(),
		}
		m.dbInfo[database] = dbInfo
		return dbInfo, nil
	})

	return dbInfo, err
}

func (m *MetaCache) safeGetDBInfo(database string) *databaseInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	db, ok := m.dbInfo[database]
	if !ok {
		return nil
	}
	return db
}

func (m *MetaCache) AllocID(ctx context.Context) (int64, error) {
	m.IDLock.Lock()
	defer m.IDLock.Unlock()

	if m.IDIndex == m.IDCount {
		resp, err := m.mixCoord.AllocID(ctx, &rootcoordpb.AllocIDRequest{
			Count: 1000000,
		})
		if err != nil {
			log.Warn("Refreshing ID cache from rootcoord failed", zap.Error(err))
			return 0, err
		}
		if resp.GetStatus().GetCode() != 0 {
			log.Warn("Refreshing ID cache from rootcoord failed", zap.String("failed detail", resp.GetStatus().GetDetail()))
			return 0, merr.WrapErrServiceInternal(resp.GetStatus().GetDetail())
		}
		m.IDStart, m.IDCount = resp.GetID(), int64(resp.GetCount())
		m.IDIndex = 0
	}
	id := m.IDStart + m.IDIndex
	m.IDIndex++
	return id, nil
}
