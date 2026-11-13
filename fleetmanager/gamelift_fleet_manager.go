package fleetmanager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/gamelift"
	gltypes "github.com/aws/aws-sdk-go-v2/service/gamelift/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/heroiclabs/nakama-common/runtime"
)

var (
	StorageGameLiftIndex = func(namespace string) string {
		if namespace == "" {
			return "gamelift_instances_idx"
		}
		return "gamelift_" + namespace + "_instances_idx"
	}
	StorageGameLiftInstancesCollection = func(namespace string) string {
		if namespace == "" {
			return "gamelift_instances"
		}

		return "gamelift_" + namespace + "_instances"
	}
)

const (
	PlacementIdKey     = "PlacementId"
	GameSessionDataKey = "GameSessionData"
	GamePropertiesKey  = "GameProperties"
	GameSessionNameKey = "GameSessionName"
)

// GameSessionStatusPlaced is a custom status for storage entries created when a placement request is started and
// GameLift hasn't yet reported the placement outcome. These entries are keyed by placement id and are never returned
// by List queries.
const GameSessionStatusPlaced = "PLACED"

// RPC IDs are lowercased to match how Nakama registers and resolves them.
var (
	RpcIdUpdateInstanceInfo = func(namespace string) string {
		if namespace == "" {
			return "gamelift_update_instance_info"
		}
		return strings.ToLower("gamelift_" + namespace + "_update_instance_info")
	}
	RpcIdDeleteInstanceInfo = func(namespace string) string {
		if namespace == "" {
			return "gamelift_delete_instance_info"
		}
		return strings.ToLower("gamelift_" + namespace + "_delete_instance_info")
	}
)

var _ runtime.FleetManagerInitializer = (*GameLiftFleetManager)(nil)

type GameLiftFleetManager struct {
	ctx             context.Context
	namespace       string
	glService       *gamelift.Client
	sqsService      *sqs.Client
	logger          runtime.Logger
	nk              runtime.NakamaModule
	db              *sql.DB
	cfg             GameLiftConfig
	callbackHandler runtime.FmCallbackHandler
}

type GameLiftConfig struct {
	NakamaNode               string        // Nakama node
	AwsKey                   string        // AWS Key
	AwsSecret                string        // AWS Secret
	AwsRegion                string        // AWS Fleet Region
	AwsFleetAlias            string        // AWS GameLift Fleet Alias
	AwsPlacementQueueName    string        // AWS GameLift Placement Queue Name
	AwsPlacementEventsSqsUrl string        // AWS GameLift Placement Events SQS Queue URL
	AwsGameLiftPollingPeriod time.Duration // Periodicity of the polling to the Aws GameLift APIs
	HttpRequestTimeout       time.Duration // AWS Http Client request timeout
	HttpDialerTimeout        time.Duration // AWS Http Client request dialing timeout
	PlacementTimeout         time.Duration // Max time a placement can stay unresolved before its storage entry is considered stale. Should be >= the GameLift queue timeout.
	SqsVisibilityTimeout     time.Duration // AWS SQS Client Visibility Timeout
	SqsWaitTime              time.Duration // AWS SQS Client Wait Time
	SqsMessageBatchSize      int           // AWS SQS Max Number of Messages to retrieve per request (1-10)
	SqsWorkers               int           // Number of concurrent SQS consumers (>= 1)
	IndexMaxEntries          int           // Nakama storage index max entries - limits the number of game sessions that can be indexed.
}

func NewGameLiftConfig(awsKey, awsSecret, awsRegion, awsGameLiftFleetAlias, awsGameLiftPlacementQueueName, awsGameLiftPlacementEventsSqsUrl string, sqsMessageBatchSize, sqsWorkers int, placementTimeout time.Duration) GameLiftConfig {
	return GameLiftConfig{
		AwsKey:                   awsKey,
		AwsSecret:                awsSecret,
		AwsRegion:                awsRegion,
		AwsFleetAlias:            awsGameLiftFleetAlias,
		AwsPlacementQueueName:    awsGameLiftPlacementQueueName,
		AwsPlacementEventsSqsUrl: awsGameLiftPlacementEventsSqsUrl,
		AwsGameLiftPollingPeriod: 15 * time.Minute,
		IndexMaxEntries:          1_000_000,
		HttpRequestTimeout:       30 * time.Second,
		HttpDialerTimeout:        15 * time.Second,
		SqsVisibilityTimeout:     10 * time.Second,
		SqsWaitTime:              20 * time.Second,
		SqsMessageBatchSize:      sqsMessageBatchSize,
		SqsWorkers:               sqsWorkers,
		PlacementTimeout:         placementTimeout,
	}
}

func (glc *GameLiftConfig) Validate() error {
	errs := make([]error, 0)
	if glc.NakamaNode == "" {
		errs = append(errs, errors.New("node must be set"))
	}
	if glc.AwsKey == "" {
		errs = append(errs, errors.New("aws key must be set"))
	}
	if glc.AwsSecret == "" {
		errs = append(errs, errors.New("aws secret must be set"))
	}
	if glc.AwsRegion == "" {
		errs = append(errs, errors.New("aws region must be set"))
	}
	if glc.AwsFleetAlias == "" {
		errs = append(errs, errors.New("aws fleet alias must be set"))
	}
	if glc.AwsPlacementQueueName == "" {
		errs = append(errs, errors.New("aws fleet placement queue name must be set"))
	}
	if glc.AwsPlacementEventsSqsUrl == "" {
		errs = append(errs, errors.New("aws placement events sqs url must be set"))
	}
	if glc.AwsGameLiftPollingPeriod == 0 {
		errs = append(errs, errors.New("aws gamelift polling period must be > 0"))
	}
	if glc.IndexMaxEntries == 0 {
		errs = append(errs, errors.New("index max entries must be > 0"))
	}
	if glc.SqsWaitTime >= glc.HttpRequestTimeout {
		errs = append(errs, errors.New("sqs wait time must be < http request timeout"))
	}
	if glc.SqsMessageBatchSize < 1 || glc.SqsMessageBatchSize > 10 {
		errs = append(errs, errors.New("sqs message batch size must be between 1 and 10"))
	}
	if glc.SqsWorkers < 1 {
		errs = append(errs, errors.New("sqs workers must be >= 1"))
	}
	if glc.PlacementTimeout <= 0 {
		errs = append(errs, errors.New("placement timeout must be > 0"))
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	return nil
}

func NewGameLiftFleetManager(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, conf GameLiftConfig) (runtime.FleetManagerInitializer, error) {
	nakamaNode, ok := ctx.Value(runtime.RUNTIME_CTX_NODE).(string)
	if !ok || !strings.HasPrefix(nakamaNode, "nakama") {
		return nil, errors.New("failed to get nakama node from ctx")
	}
	conf.NakamaNode = nakamaNode
	if err := conf.Validate(); err != nil {
		return nil, err
	}

	awsConfig := aws.Config{
		Region:      conf.AwsRegion,
		Credentials: credentials.NewStaticCredentialsProvider(conf.AwsKey, conf.AwsSecret, ""),
		HTTPClient: awshttp.NewBuildableClient().
			WithTimeout(conf.HttpRequestTimeout).
			WithDialerOptions(func(d *net.Dialer) { d.Timeout = conf.HttpDialerTimeout }),
	}

	glService := gamelift.NewFromConfig(awsConfig)
	sqsService := sqs.NewFromConfig(awsConfig)

	glfm := &GameLiftFleetManager{
		ctx:        ctx,
		db:         db,
		glService:  glService,
		sqsService: sqsService,
		logger:     logger,
		nk:         nk,
		cfg:        conf,
	}

	logger.WithField("region", conf.AwsRegion).
		WithField("fleet_alias", conf.AwsFleetAlias).
		WithField("placement_queue", conf.AwsPlacementQueueName).
		WithField("sqs_url", conf.AwsPlacementEventsSqsUrl).
		WithField("sqs_batch_size", conf.SqsMessageBatchSize).
		WithField("sqs_workers", conf.SqsWorkers).
		WithField("sqs_visibility_timeout", conf.SqsVisibilityTimeout).
		WithField("sqs_wait_time", conf.SqsWaitTime).
		Info("GameLift fleet manager initialized")

	return glfm, nil
}

func (fm *GameLiftFleetManager) Init(initializer runtime.Initializer, nk runtime.NakamaModule, callbackHandler runtime.FmCallbackHandler, namespace string) error {
	fm.nk = nk
	fm.callbackHandler = callbackHandler
	fm.namespace = namespace

	if err := initializer.RegisterStorageIndex(StorageGameLiftIndex(namespace), StorageGameLiftInstancesCollection(namespace), "", []string{"id", "create_time", "player_count", "status", "metadata"}, []string{"create_time", "player_count"}, fm.cfg.IndexMaxEntries, false); err != nil {
		return err
	}

	// NOTE: This RPC is required and must be invoked from a Game Session whenever a player connects/disconnects.
	// Refer to the documentation for more information.
	if err := initializer.RegisterRpc(RpcIdUpdateInstanceInfo(fm.namespace), fm.UpdateInstanceInfo); err != nil {
		return err
	}
	// NOTE: This RPC is required and must be invoked from the Game Session whenever a Game Session terminates.
	// Refer to the documentation for more information.
	if err := initializer.RegisterRpc(RpcIdDeleteInstanceInfo(fm.namespace), fm.DeleteInstanceInfo); err != nil {
		return err
	}

	// Initialise sqs background consumers here as they depend on the callbackHandler
	for range fm.cfg.SqsWorkers {
		go fm.processSqsPlacementEvents()
	}

	// The sync worker only needs to run on a single node of the cluster.
	if fm.cfg.NakamaNode == "nakama" || fm.cfg.NakamaNode == "nakama-0" {
		go fm.syncInstancesWorker()
	}

	fm.logger.WithField("namespace", namespace).Info("GameLift Fleet Manager initialized")

	return nil
}

func (fm *GameLiftFleetManager) Create(ctx context.Context, maxPlayers int, userIds []string, latencies []runtime.FleetUserLatencies, metadata map[string]any, callback runtime.FmCreateCallbackFn) (map[string]string, error) {
	var desiredPlayerSessions []gltypes.DesiredPlayerSession
	if len(userIds) > 0 {
		desiredPlayerSessions = make([]gltypes.DesiredPlayerSession, 0, len(userIds))
		for _, id := range userIds {
			var playerData *string
			if m := metadata[id]; m != nil {
				if s, ok := m.(string); ok {
					playerData = new(string)
					*playerData = s
				}
			}

			desiredPlayerSessions = append(desiredPlayerSessions, gltypes.DesiredPlayerSession{
				PlayerData: playerData,
				PlayerId:   aws.String(id),
			})
		}
	}
	fm.logger.WithField("desired_sessions", desiredPlayerSessions).Debug("desired player sessions")

	gsData := make(map[string]any)
	if m := metadata[GameSessionDataKey]; m != nil {
		data, ok := m.(map[string]any)
		if !ok {
			return nil, errors.New("invalid metadata key 'GameSessionData' value: must be map[string]any")
		}
		gsData = maps.Clone(data)
	}
	gsData["nakama_gamelift_namespace"] = fm.namespace
	gsData["nakama_gamelift_update_rpc_id"] = RpcIdUpdateInstanceInfo(fm.namespace)
	gsData["nakama_gamelift_delete_rpc_id"] = RpcIdDeleteInstanceInfo(fm.namespace)

	sessionDataJson, err := json.Marshal(gsData)
	if err != nil {
		return nil, errors.New("failed to marshal GameSessionData metadata value: " + err.Error())
	}
	gameSessionData := aws.String(string(sessionDataJson))

	var gameProperties []gltypes.GameProperty
	if m := metadata[GamePropertiesKey]; m != nil {
		if gp, ok := m.(map[string]string); ok {
			props := make([]gltypes.GameProperty, 0, len(gp))
			for k, v := range gp {
				pk, pv := new(string), new(string)
				*pk, *pv = k, v
				props = append(props, gltypes.GameProperty{
					Key:   pk,
					Value: pv,
				})
			}
			gameProperties = props
		} else {
			return nil, errors.New("invalid metadata key 'GameProperties' value: must be map[string]string")
		}
	}

	var gameSessionName *string
	if m := metadata[GameSessionNameKey]; m != "" {
		if s, ok := m.(string); ok {
			gameSessionName = new(string)
			*gameSessionName = s
		}
	}

	var playerLatencies []gltypes.PlayerLatency
	if len(latencies) > 0 {
		playerLatencies = make([]gltypes.PlayerLatency, 0, len(latencies))
		for _, l := range latencies {
			playerLatencies = append(playerLatencies, gltypes.PlayerLatency{
				LatencyInMilliseconds: aws.Float32(l.LatencyInMilliseconds),
				PlayerId:              aws.String(l.UserId),
				RegionIdentifier:      aws.String(l.RegionIdentifier),
			})
		}
	}

	var placementId string
	if metadata != nil {
		if v, ok := metadata[PlacementIdKey]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				placementId = s
				fm.logger.WithField("placement_id", placementId).Debug("using placementId from metadata")
			}
		}
	}
	if placementId == "" {
		placementId = fm.callbackHandler.GenerateCallbackId()
		fm.logger.WithField("placement_id", placementId).Debug("no placementId in metadata; generated fallback")
	}
	fm.logger.WithField("placement_id", placementId).WithField("game_properties", gameProperties).Debug("placement input")

	placedMetadata := make(map[string]any)
	if gameSessionData != nil {
		placedMetadata[GameSessionDataKey] = *gameSessionData
	}
	if len(gameProperties) > 0 {
		props := make(map[string]string, len(gameProperties))
		for _, p := range gameProperties {
			props[*p.Key] = *p.Value
		}
		placedMetadata[GamePropertiesKey] = props
	}
	if gameSessionName != nil {
		placedMetadata[GameSessionNameKey] = *gameSessionName
	}

	// Store the placement inputs before starting the placement so that the fulfilled event handler can build the
	// instance info without having to call GameLift, regardless of which node consumes the event.
	if err := fm.writeStoragePlacement(ctx, &runtime.InstanceInfo{
		Id:             placementId,
		ConnectionInfo: nil,
		CreateTime:     time.Now().UTC(),
		PlayerCount:    0,
		Status:         GameSessionStatusPlaced,
		Metadata:       placedMetadata,
	}); err != nil {
		return nil, fmt.Errorf("failed to store placement: %s", err.Error())
	}

	placementInput := &gamelift.StartGameSessionPlacementInput{
		PlacementId:               aws.String(placementId),
		GameSessionQueueName:      aws.String(fm.cfg.AwsPlacementQueueName),
		MaximumPlayerSessionCount: aws.Int32(int32(maxPlayers)),
		DesiredPlayerSessions:     desiredPlayerSessions,
		GameProperties:            gameProperties,
		GameSessionData:           gameSessionData,
		GameSessionName:           gameSessionName,
		PlayerLatencies:           playerLatencies,
	}

	placementOutput, err := fm.glService.StartGameSessionPlacement(ctx, placementInput)
	if err != nil {
		fm.nk.MetricsCounterAdd("gamelift_api_errors", map[string]string{"operation": "StartGameSessionPlacement"}, 1)
		if err := fm.deleteStoragePlacements(ctx, []string{placementId}); err != nil {
			fm.logger.WithField("error", err.Error()).WithField("placement_id", placementId).Warn("failed to delete placement storage entry")
		}
		return nil, err
	}

	switch placementOutput.GameSessionPlacement.Status {
	case gltypes.GameSessionPlacementStateFulfilled, gltypes.GameSessionPlacementStatePending:
		fm.nk.MetricsCounterAdd("gamelift_placement_requests", map[string]string{"status": "pending"}, 1)
		fm.logger.WithField("placement_id", placementId).Debug("placement started")
	case gltypes.GameSessionPlacementStateCancelled, gltypes.GameSessionPlacementStateTimedOut, gltypes.GameSessionPlacementStateFailed:
		fm.nk.MetricsCounterAdd("gamelift_placement_requests", map[string]string{"status": "failed"}, 1)
		if err := fm.deleteStoragePlacements(ctx, []string{placementId}); err != nil {
			fm.logger.WithField("error", err.Error()).WithField("placement_id", placementId).Warn("failed to delete placement storage entry")
		}
		return nil, errors.New("failed to start game session placement")
	default:
		// Should never happen.
		fm.logger.WithField("status", string(placementOutput.GameSessionPlacement.Status)).Warn("unhandled start game session placement status")
		fm.nk.MetricsCounterAdd("gamelift_placement_requests", map[string]string{"status": "failed"}, 1)
		if err := fm.deleteStoragePlacements(ctx, []string{placementId}); err != nil {
			fm.logger.WithField("error", err.Error()).WithField("placement_id", placementId).Warn("failed to delete placement storage entry")
		}
		return nil, errors.New("failed to start game session placement")
	}

	if callback != nil {
		fm.callbackHandler.SetCallback(placementId, callback)
	}

	createMetadata := map[string]string{PlacementIdKey: *placementOutput.GameSessionPlacement.PlacementId}

	return createMetadata, nil
}

func (fm *GameLiftFleetManager) Get(ctx context.Context, id string) (instance *runtime.InstanceInfo, err error) {
	params := &gamelift.DescribeGameSessionsInput{
		GameSessionId: &id,
		Limit:         aws.Int32(1),
	}

	out, err := fm.glService.DescribeGameSessions(ctx, params)
	if err != nil {
		fm.nk.MetricsCounterAdd("gamelift_api_errors", map[string]string{"operation": "DescribeGameSessions"}, 1)
		return nil, err
	}

	if len(out.GameSessions) == 0 {
		return nil, runtime.NewError(fmt.Sprintf("game session with id: %q not found", id), 5)
	}

	gs := out.GameSessions[0]

	instance = NewInstanceInfo(gs)

	switch gs.Status {
	case gltypes.GameSessionStatusActive:
		if err = fm.updateStorageGameSessions(ctx, []*runtime.InstanceInfo{instance}); err != nil {
			return nil, errors.New("failed to update gamelift game session instance info")
		}
	case gltypes.GameSessionStatusTerminated, gltypes.GameSessionStatusTerminating, gltypes.GameSessionStatusError:
		if err = fm.deleteStorageGameSessions(ctx, []string{id}); err != nil {
			return nil, errors.New("failed to delete gamelift game session instance info")
		}
	}

	return instance, nil
}

func (fm *GameLiftFleetManager) List(ctx context.Context, query string, limit int, prevCursor string) (list []*runtime.InstanceInfo, nextCursor string, err error) {
	if query == "" {
		var instances []*runtime.InstanceInfo
		instances, nextCursor, err = fm.listGameLiftGameSessions(ctx, prevCursor)
		if err != nil {
			return nil, "", err
		}

		if err = fm.updateStorageGameSessions(ctx, instances); err != nil {
			return nil, "", err
		}

		return instances, nextCursor, nil
	}

	// Exclude placements that haven't been fulfilled yet.
	query = query + " -value.status:" + GameSessionStatusPlaced

	// Emptiest matches, most recently created first.
	entries, _, err := fm.nk.StorageIndexList(ctx, "", StorageGameLiftIndex(fm.namespace), query, limit, []string{"player_count", "-create_time"}, "")
	if err != nil {
		return nil, "", err
	}

	results := make([]*runtime.InstanceInfo, 0)
	for _, so := range entries.GetObjects() {
		var info *runtime.InstanceInfo
		if err = json.Unmarshal([]byte(so.Value), &info); err != nil {
			return nil, "", err
		}
		results = append(results, info)
	}

	return results, nextCursor, nil
}

func (fm *GameLiftFleetManager) listGameLiftGameSessions(ctx context.Context, cursor string) ([]*runtime.InstanceInfo, string, error) {
	params := &gamelift.SearchGameSessionsInput{
		AliasId:        aws.String(fm.cfg.AwsFleetAlias),
		Limit:          aws.Int32(int32(20)), // max API limit is 20
		SortExpression: aws.String("creationTimeMillis ASC"),
	}
	if cursor != "" {
		params.NextToken = aws.String(cursor)
	}

	instances := make([]*runtime.InstanceInfo, 0)
	out, err := fm.glService.SearchGameSessions(ctx, params)
	if err != nil {
		fm.nk.MetricsCounterAdd("gamelift_api_errors", map[string]string{"operation": "SearchGameSessions"}, 1)
		return nil, "", err
	}

	for _, i := range out.GameSessions {
		instances = append(instances, NewInstanceInfo(i))
	}

	nextCursor := ""
	if out.NextToken != nil {
		nextCursor = *out.NextToken
	}

	return instances, nextCursor, nil
}

func (fm *GameLiftFleetManager) listDbGameSessions(ctx context.Context) ([]*runtime.InstanceInfo, error) {
	instances := make([]*runtime.InstanceInfo, 0)

	cursor := ""
	for {
		objects, nextCursor, err := fm.nk.StorageList(ctx, "", "", StorageGameLiftInstancesCollection(fm.namespace), 1_000, cursor)
		if err != nil {
			return nil, err
		}

		for _, obj := range objects {
			var info *runtime.InstanceInfo
			if err = json.Unmarshal([]byte(obj.Value), &info); err != nil {
				return nil, err
			}
			instances = append(instances, info)
		}

		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}

	return instances, nil
}

func (fm *GameLiftFleetManager) getDbGameSession(ctx context.Context, id string) (*runtime.InstanceInfo, error) {
	key, err := InstanceIdToStorageKey(id)
	if err != nil {
		return nil, err
	}
	objects, err := fm.nk.StorageRead(ctx, []*runtime.StorageRead{{
		Collection: StorageGameLiftInstancesCollection(fm.namespace),
		Key:        key,
	}})
	if err != nil {
		return nil, err
	}

	if len(objects) == 0 {
		return nil, nil
	}

	obj := objects[0]

	var instance *runtime.InstanceInfo
	if err = json.Unmarshal([]byte(obj.Value), &instance); err != nil {
		return nil, err
	}

	return instance, nil
}

func (fm *GameLiftFleetManager) updateStorageGameSessions(ctx context.Context, instances []*runtime.InstanceInfo) error {
	storageWrites := make([]*runtime.StorageWrite, 0, len(instances))
	for _, i := range instances {
		v, err := json.Marshal(i)
		if err != nil {
			return err
		}

		key, err := InstanceIdToStorageKey(i.Id)
		if err != nil {
			return err
		}
		storageWrites = append(storageWrites, &runtime.StorageWrite{
			Collection: StorageGameLiftInstancesCollection(fm.namespace),
			Key:        key,
			Value:      string(v),
		})
	}

	if _, err := fm.nk.StorageWrite(ctx, storageWrites); err != nil {
		return err
	}

	return nil
}

func (fm *GameLiftFleetManager) deleteStorageGameSessions(ctx context.Context, ids []string) error {
	deletes := make([]*runtime.StorageDelete, 0, len(ids))
	for _, id := range ids {
		key, err := InstanceIdToStorageKey(id)
		if err != nil {
			return err
		}
		deletes = append(deletes, &runtime.StorageDelete{
			Collection: StorageGameLiftInstancesCollection(fm.namespace),
			Key:        key,
		})
	}

	if err := fm.nk.StorageDelete(ctx, deletes); err != nil {
		return err
	}

	return nil
}

// writeStoragePlacement creates a PLACED storage entry for a placement. The write fails if an entry already exists.
func (fm *GameLiftFleetManager) writeStoragePlacement(ctx context.Context, placement *runtime.InstanceInfo) error {
	v, err := json.Marshal(placement)
	if err != nil {
		return err
	}

	_, err = fm.nk.StorageWrite(ctx, []*runtime.StorageWrite{{
		Collection: StorageGameLiftInstancesCollection(fm.namespace),
		Key:        PlacementIdToStorageKey(placement.Id),
		Value:      string(v),
		Version:    "*",
	}})

	return err
}

func (fm *GameLiftFleetManager) getStoragePlacement(ctx context.Context, placementId string) (*runtime.InstanceInfo, error) {
	objects, err := fm.nk.StorageRead(ctx, []*runtime.StorageRead{{
		Collection: StorageGameLiftInstancesCollection(fm.namespace),
		Key:        PlacementIdToStorageKey(placementId),
	}})
	if err != nil {
		return nil, err
	}

	if len(objects) == 0 {
		return nil, nil
	}

	var placement *runtime.InstanceInfo
	if err = json.Unmarshal([]byte(objects[0].Value), &placement); err != nil {
		return nil, err
	}

	return placement, nil
}

func (fm *GameLiftFleetManager) deleteStoragePlacements(ctx context.Context, placementIds []string) error {
	deletes := make([]*runtime.StorageDelete, 0, len(placementIds))
	for _, id := range placementIds {
		deletes = append(deletes, &runtime.StorageDelete{
			Collection: StorageGameLiftInstancesCollection(fm.namespace),
			Key:        PlacementIdToStorageKey(id),
		})
	}

	return fm.nk.StorageDelete(ctx, deletes)
}

func (fm *GameLiftFleetManager) Join(ctx context.Context, id string, userIds []string, metadata map[string]string) (joinInfo *runtime.JoinInfo, err error) {
	if id == "" {
		return nil, errors.New("expects id to be a valid GameSessionId")
	}
	if len(userIds) < 1 {
		return nil, errors.New("expects userIds to have at least one valid user id")
	}

	sessionInfo := make([]*runtime.SessionInfo, 0, len(userIds))
	joinedPlayerCount := 0
	if len(userIds) == 1 {
		var playerData *string
		if metadata != nil {
			playerData = new(string)
			*playerData = metadata[userIds[0]]
		}
		input := &gamelift.CreatePlayerSessionInput{
			GameSessionId: aws.String(id),
			PlayerId:      aws.String(userIds[0]),
			PlayerData:    playerData,
		}

		cps, err := fm.glService.CreatePlayerSession(ctx, input)
		if err != nil {
			fm.nk.MetricsCounterAdd("gamelift_api_errors", map[string]string{"operation": "CreatePlayerSession"}, 1)
			return nil, err
		}

		sessionInfo = append(sessionInfo, &runtime.SessionInfo{
			UserId:    *cps.PlayerSession.PlayerId,
			SessionId: *cps.PlayerSession.PlayerSessionId,
		})
		joinedPlayerCount = 1
	} else {
		input := &gamelift.CreatePlayerSessionsInput{
			GameSessionId: aws.String(id),
			PlayerIds:     userIds,
			PlayerDataMap: metadata,
		}

		cpss, err := fm.glService.CreatePlayerSessions(ctx, input)
		if err != nil {
			fm.nk.MetricsCounterAdd("gamelift_api_errors", map[string]string{"operation": "CreatePlayerSessions"}, 1)
			return nil, err
		}
		for _, s := range cpss.PlayerSessions {
			sessionInfo = append(sessionInfo, &runtime.SessionInfo{
				UserId:    *s.PlayerId,
				SessionId: *s.PlayerSessionId,
			})
			joinedPlayerCount++
		}
	}

	fm.nk.MetricsCounterAdd("gamelift_player_sessions_created", nil, int64(joinedPlayerCount))

	dbinfo, err := fm.getDbGameSession(ctx, id)
	if err != nil {
		return nil, err
	}

	if dbinfo == nil {
		// instance does not exist in db - should not happen but check with AWS regardless.
		dbinfo, err = fm.Get(ctx, id)
		if err != nil {
			return nil, err
		}
	}

	dbinfo.PlayerCount += joinedPlayerCount

	if err = fm.updateStorageGameSessions(ctx, []*runtime.InstanceInfo{dbinfo}); err != nil {
		return nil, err
	}

	return &runtime.JoinInfo{
		InstanceInfo: dbinfo,
		SessionInfo:  sessionInfo,
	}, nil
}

var (
	ErrInvalidInput  = runtime.NewError("input is invalid", 3)       // INVALID_ARGUMENT
	ErrInternalError = runtime.NewError("internal server error", 13) // INTERNAL
)

type UpdateInstanceInfoRequest struct {
	Id          string         `json:"id"`
	PlayerCount int            `json:"player_count"`
	Metadata    map[string]any `json:"metadata"`
}

func (fm *GameLiftFleetManager) UpdateInstanceInfo(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, payload string) (string, error) {
	var req *UpdateInstanceInfoRequest
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		logger.WithField("error", err.Error()).Error("failed to unmarshal updateInstanceInfo request")
		return "", ErrInternalError
	}

	if req.Id == "" {
		return "", ErrInvalidInput
	}

	if err := fm.Update(ctx, req.Id, req.PlayerCount, req.Metadata); err != nil {
		logger.WithField("error", err.Error()).Error("failed to update instance info")
		return "", ErrInternalError
	}

	return "", nil
}

type DeleteInstanceInfoRequest struct {
	Id string `json:"id"`
}

func (fm *GameLiftFleetManager) DeleteInstanceInfo(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, payload string) (string, error) {
	var req *DeleteInstanceInfoRequest
	if err := json.Unmarshal([]byte(payload), &req); err != nil {
		logger.WithField("error", err.Error()).Error("failed to unmarshal deleteInstanceInfo request")
		return "", ErrInternalError
	}

	if len(req.Id) < 1 {
		return "", ErrInvalidInput
	}

	fm.logger.WithField("instance_id", req.Id).Debug("received delete from headless instance")

	if err := fm.Delete(ctx, req.Id); err != nil {
		fm.logger.WithField("error", err.Error()).Error("failed to delete instance info")
		return "", ErrInternalError
	}

	return "", nil
}

func (fm *GameLiftFleetManager) Update(ctx context.Context, id string, playerCount int, metadata map[string]any) error {
	dbInfo, err := fm.getDbGameSession(ctx, id)
	if err != nil {
		return fmt.Errorf("failed to read instance info from db: %s", err.Error())
	}

	if dbInfo == nil {
		// instance info was not found in the db, fetch from AWS and ignore received payload
		dbInfo, err = fm.Get(ctx, id)
		return err
	}

	var gameProps []gltypes.GameProperty
	dbInfo.PlayerCount = playerCount
	if metadata != nil {
		if m := metadata[GameSessionDataKey]; m != nil {
			dbInfo.Metadata[GameSessionDataKey] = m
		}
		if m := metadata[GamePropertiesKey]; m != nil {
			dbInfo.Metadata[GamePropertiesKey] = m
			mmap, ok := m.(map[string]any)
			if !ok {
				return errors.New("failed to type assert 'GameProperties' value to map[string]any")
			}
			gameProps = make([]gltypes.GameProperty, 0, len(mmap))
			for k, v := range mmap {
				kk, vv := new(string), new(string)
				*kk, *vv = k, v.(string)
				gameProps = append(gameProps, gltypes.GameProperty{
					Key:   kk,
					Value: vv,
				})
			}
		}
		if m := metadata[GameSessionNameKey]; m != nil {
			dbInfo.Metadata[GameSessionNameKey] = m
		}
	}

	newInfo, err := json.Marshal(dbInfo)
	if err == nil {
		fm.logger.WithField("update_instance_info", string(newInfo)).Debug("received update from headless instance")
	} else {
		fm.logger.WithField("err", err.Error()).Debug("failed to marshal db info")
	}

	if err = fm.updateStorageGameSessions(ctx, []*runtime.InstanceInfo{dbInfo}); err != nil {
		return fmt.Errorf("failed to update db instance info: %s", err.Error())
	}

	if _, err = fm.glService.UpdateGameSession(ctx, &gamelift.UpdateGameSessionInput{
		GameSessionId:  aws.String(dbInfo.Id),
		GameProperties: gameProps,
	}); err != nil {
		fm.nk.MetricsCounterAdd("gamelift_api_errors", map[string]string{"operation": "UpdateGameSession"}, 1)
		return err
	}

	return nil
}

func (fm *GameLiftFleetManager) Delete(ctx context.Context, id string) error {
	if err := fm.deleteStorageGameSessions(ctx, []string{id}); err != nil {
		return err
	}

	return nil
}

const (
	GameLiftEventIdPlacementFulfilled = "PlacementFulfilled"
	GameLiftEventIdPlacementCancelled = "PlacementCancelled"
	GameLiftEventIdPlacementTimedOut  = "PlacementTimedOut"
	GameLiftEventIdPlacementFailed    = "PlacementFailed"
)

type SqsMessage struct {
	Type             string    `json:"Type"`
	MessageId        string    `json:"MessageId"`
	TopicArn         string    `json:"TopicArn"`
	Message          string    `json:"Message"`
	Timestamp        time.Time `json:"Timestamp"`
	SignatureVersion string    `json:"SignatureVersion"`
	Signature        string    `json:"Signature"`
	SigningCertURL   string    `json:"SigningCertURL"`
	UnsubscribeURL   string    `json:"UnsubscribeURL"`
}

type SqsMessagePlacementEventBody struct {
	Version    string    `json:"version"`
	Id         string    `json:"id"`
	DetailType string    `json:"detail-type"`
	Source     string    `json:"source"`
	Account    string    `json:"account"`
	Time       time.Time `json:"time"`
	Region     string    `json:"region"`
	Resources  []string  `json:"resources"`
	Detail     struct {
		Type                 string    `json:"type"`
		PlacementId          string    `json:"placementId"`
		Port                 string    `json:"port"`
		GameSessionArn       string    `json:"gameSessionArn"`
		IpAddress            string    `json:"ipAddress"`
		DnsName              string    `json:"dnsName"`
		StartTime            time.Time `json:"startTime"`
		EndTime              time.Time `json:"endTime"`
		GameSessionRegion    string    `json:"gameSessionRegion"`
		PlacedPlayerSessions []struct {
			PlayerId        string `json:"playerId"`
			PlayerSessionId string `json:"playerSessionId"`
		} `json:"placedPlayerSessions"`
	} `json:"detail"`
}

func (fm *GameLiftFleetManager) processSqsPlacementEvents() {
	receiveMessageInput := &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(fm.cfg.AwsPlacementEventsSqsUrl),
		MaxNumberOfMessages: int32(fm.cfg.SqsMessageBatchSize),
		VisibilityTimeout:   int32(fm.cfg.SqsVisibilityTimeout.Seconds()),
		WaitTimeSeconds:     int32(fm.cfg.SqsWaitTime.Seconds()),
	}

	ctx := fm.ctx

	for {
		select {
		case <-ctx.Done():
			return
		default:
			message, err := fm.sqsService.ReceiveMessage(ctx, receiveMessageInput)
			if err != nil {
				if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "context canceled") {
					return
				}

				fm.nk.MetricsCounterAdd("gamelift_sqs_errors", map[string]string{"stage": "receive"}, 1)
				fm.logger.WithField("error", err.Error()).Error("failed to fetch aws sqs messages from queue.")
				continue
			}

			if len(message.Messages) == 0 {
				continue
			}

			processed := make([]bool, len(message.Messages))
			wg := sync.WaitGroup{}
			t := time.Now()
			for i, m := range message.Messages {
				wg.Go(func() {
					processed[i] = fm.processSqsPlacementEvent(ctx, m)
				})
			}
			wg.Wait()
			fm.logger.WithField("processed", processed).WithField("total", len(message.Messages)).WithField("duration_ms", time.Since(t).Milliseconds()).Info("sqs placement events processed")

			ackEntries := make([]sqstypes.DeleteMessageBatchRequestEntry, 0, len(message.Messages))
			for i, m := range message.Messages {
				if !processed[i] {
					fm.nk.MetricsCounterAdd("gamelift_sqs_messages_processed", map[string]string{"result": "failure"}, 1)
					continue
				}
				fm.nk.MetricsCounterAdd("gamelift_sqs_messages_processed", map[string]string{"result": "success"}, 1)
				ackEntries = append(ackEntries, sqstypes.DeleteMessageBatchRequestEntry{
					Id:            m.MessageId,
					ReceiptHandle: m.ReceiptHandle,
				})
			}

			if len(ackEntries) > 0 {
				res, err := fm.sqsService.DeleteMessageBatch(ctx, &sqs.DeleteMessageBatchInput{
					QueueUrl: aws.String(fm.cfg.AwsPlacementEventsSqsUrl),
					Entries:  ackEntries,
				})
				if err != nil {
					fm.nk.MetricsCounterAdd("gamelift_sqs_errors", map[string]string{"stage": "delete"}, int64(len(ackEntries)))
					fm.logger.WithField("error", err.Error()).WithField("entries", len(ackEntries)).Error("failed to delete sqs message batch")
				} else if len(res.Failed) > 0 {
					fm.nk.MetricsCounterAdd("gamelift_sqs_errors", map[string]string{"stage": "delete"}, int64(len(res.Failed)))
					fm.logger.WithField("failed", res.Failed).WithField("entries", len(ackEntries)).Error("failed to delete some sqs messages")
				}
			}
		}
	}
}

// processSqsPlacementEvent handles a single placement event message and reports whether it was
// successfully processed and can be acked. Messages that fail are left on the queue to be redelivered.
func (fm *GameLiftFleetManager) processSqsPlacementEvent(ctx context.Context, m sqstypes.Message) bool {
	logger := fm.logger.WithField("message_id", *m.MessageId)
	logger.WithField("message_body", *m.Body).Debug("Sqs placement notification received.")

	var queueMsg SqsMessage
	if err := json.Unmarshal([]byte(*m.Body), &queueMsg); err != nil {
		fm.nk.MetricsCounterAdd("gamelift_sqs_errors", map[string]string{"stage": "unmarshal_queue"}, 1)
		logger.WithField("error", err.Error()).Error("failed to unmarshal sqs queue message body.")
		return false
	}

	var placementEventMsg *SqsMessagePlacementEventBody
	if err := json.Unmarshal([]byte(queueMsg.Message), &placementEventMsg); err != nil {
		fm.nk.MetricsCounterAdd("gamelift_sqs_errors", map[string]string{"stage": "unmarshal_event"}, 1)
		logger.WithField("error", err.Error()).Error("failed to unmarshal sqs event body.")
		return false
	}

	placementId := placementEventMsg.Detail.PlacementId
	eventMetadata := map[string]any{"event": *m.Body}

	switch placementEventMsg.Detail.Type {
	case GameLiftEventIdPlacementFulfilled:
		instanceInfo, err := fm.resolvePlacementFulfilled(ctx, placementEventMsg)
		if err != nil {
			logger.WithField("error", err.Error()).Error("failed to resolve fulfilled placement instance info")
			return false
		}

		var sessions []*runtime.SessionInfo
		if len(placementEventMsg.Detail.PlacedPlayerSessions) > 0 {
			sessions = make([]*runtime.SessionInfo, len(placementEventMsg.Detail.PlacedPlayerSessions))
			for i, s := range placementEventMsg.Detail.PlacedPlayerSessions {
				sessions[i] = &runtime.SessionInfo{
					UserId:    s.PlayerId,
					SessionId: s.PlayerSessionId,
				}
			}
		}

		fm.nk.MetricsCounterAdd("gamelift_placement_requests", map[string]string{"status": "fulfilled"}, 1)
		fm.nk.MetricsCounterAdd("gamelift_player_sessions_created", nil, int64(len(sessions)))
		fm.nk.MetricsCounterAdd("gamelift_placement_events", map[string]string{"event": GameLiftEventIdPlacementFulfilled}, 1)
		fm.nk.MetricsTimerRecord("gamelift_placement_fulfillment_duration", nil, placementEventMsg.Detail.EndTime.Sub(placementEventMsg.Detail.StartTime))
		// Spin callback in separate goroutine to not delay the batch ack.
		go fm.callbackHandler.InvokeCallback(placementId, runtime.CreateSuccess, instanceInfo, sessions, eventMetadata, nil)
	case GameLiftEventIdPlacementTimedOut, GameLiftEventIdPlacementCancelled, GameLiftEventIdPlacementFailed:
		// A failed delete doesn't block the ack, the sync worker eventually removes stale placement entries.
		if err := fm.deleteStoragePlacements(ctx, []string{placementId}); err != nil {
			logger.WithField("error", err.Error()).WithField("placement_id", placementId).Warn("failed to delete placement storage entry")
		}

		status := runtime.CreateError
		if placementEventMsg.Detail.Type == GameLiftEventIdPlacementTimedOut {
			status = runtime.CreateTimeout
		}

		fm.nk.MetricsCounterAdd("gamelift_placement_events", map[string]string{"event": placementEventMsg.Detail.Type}, 1)
		go fm.callbackHandler.InvokeCallback(placementId, status, nil, nil, eventMetadata, errors.New(placementEventMsg.Detail.Type))
	default:
		// Nothing to do for unknown event types; ack them so they aren't redelivered forever.
		logger.WithField("type", placementEventMsg.Detail.Type).Warn("unhandled game session placement state")
	}

	return true
}

// resolvePlacementFulfilled builds the instance info of a fulfilled placement and stores it under the game session id.
func (fm *GameLiftFleetManager) resolvePlacementFulfilled(ctx context.Context, event *SqsMessagePlacementEventBody) (*runtime.InstanceInfo, error) {
	placementId := event.Detail.PlacementId
	gameSessionId := event.Detail.GameSessionArn

	placement, err := fm.getStoragePlacement(ctx, placementId)
	if err != nil {
		return nil, fmt.Errorf("failed to read placement from db: %s", err.Error())
	}

	if placement == nil {
		dbInfo, err := fm.getDbGameSession(ctx, gameSessionId)
		if err != nil {
			return nil, fmt.Errorf("failed to read instance info from db: %s", err.Error())
		}
		if dbInfo != nil {
			fm.nk.MetricsCounterAdd("gamelift_placement_fulfilled_source", map[string]string{"source": "db"}, 1)
			return dbInfo, nil
		}

		fm.nk.MetricsCounterAdd("gamelift_placement_fulfilled_source", map[string]string{"source": "gamelift"}, 1)
		return fm.Get(ctx, gameSessionId)
	}

	port, err := strconv.Atoi(event.Detail.Port)
	if err != nil {
		return nil, fmt.Errorf("invalid placement event port %q: %s", event.Detail.Port, err.Error())
	}

	instance := &runtime.InstanceInfo{
		Id: gameSessionId,
		ConnectionInfo: &runtime.ConnectionInfo{
			IpAddress: event.Detail.IpAddress,
			DnsName:   event.Detail.DnsName,
			Port:      port,
		},
		CreateTime:  event.Detail.EndTime,
		PlayerCount: len(event.Detail.PlacedPlayerSessions),
		Status:      string(gltypes.GameSessionStatusActive),
		Metadata:    placement.Metadata,
	}

	key, err := InstanceIdToStorageKey(gameSessionId)
	if err != nil {
		return nil, err
	}
	v, err := json.Marshal(instance)
	if err != nil {
		return nil, err
	}

	// Create the game session entry and remove the placement entry atomically. The game session entry is only created
	// if it doesn't exist yet, so a more recent update from the game server isn't overwritten.
	if _, _, err = fm.nk.MultiUpdate(ctx, nil, []*runtime.StorageWrite{{
		Collection: StorageGameLiftInstancesCollection(fm.namespace),
		Key:        key,
		Value:      string(v),
		Version:    "*",
	}}, []*runtime.StorageDelete{{
		Collection: StorageGameLiftInstancesCollection(fm.namespace),
		Key:        PlacementIdToStorageKey(placementId),
	}}, nil, false); err != nil {
		dbInfo, readErr := fm.getDbGameSession(ctx, gameSessionId)
		if readErr != nil || dbInfo == nil {
			return nil, fmt.Errorf("failed to store instance info: %s", err.Error())
		}
		// The game session entry already exists. The placement entry is left for the sync worker to remove.
		instance = dbInfo
	}

	fm.nk.MetricsCounterAdd("gamelift_placement_fulfilled_source", map[string]string{"source": "placement"}, 1)

	return instance, nil
}

func (fm *GameLiftFleetManager) syncInstancesWorker() {
	deleteTerminatedInstancesFn := func() {
		// Collect every page before diffing, otherwise entries belonging to later pages would be removed.
		activeInstances := make([]*runtime.InstanceInfo, 0)
		cursor := ""
		for {
			instances, nextCursor, err := fm.listGameliftActiveInstances(fm.ctx, cursor)
			if err != nil {
				fm.logger.WithField("error", err.Error()).Error("failed to list gamelift terminated instances")
				return
			}
			activeInstances = append(activeInstances, instances...)
			if nextCursor == "" {
				break
			}
			cursor = nextCursor
		}
		fm.logger.WithField("active_instances", len(activeInstances)).Debug("fetched active game instances list")

		dbInstances, err := fm.listDbGameSessions(fm.ctx)
		if err != nil {
			fm.logger.WithField("error", err.Error()).Error("failed to read instances from db")
			return
		}

		activeInstancesMap := make(map[string]struct{}, len(activeInstances))
		for _, i := range activeInstances {
			activeInstancesMap[i.Id] = struct{}{}
		}

		instancesToRemove := make([]string, 0)
		placementsToRemove := make([]string, 0)
		for _, dbInfo := range dbInstances {
			if dbInfo.Status == GameSessionStatusPlaced {
				// Placements still in flight are kept, stale ones never got (or lost) their placement event.
				if time.Since(dbInfo.CreateTime) > fm.cfg.PlacementTimeout {
					placementsToRemove = append(placementsToRemove, dbInfo.Id)
				}
				continue
			}
			if _, ok := activeInstancesMap[dbInfo.Id]; !ok {
				instancesToRemove = append(instancesToRemove, dbInfo.Id)
			}
		}

		if len(activeInstances) > 0 {
			if err = fm.updateStorageGameSessions(fm.ctx, activeInstances); err != nil {
				fm.logger.WithField("error", err.Error()).Error("failed to update db instances")
				return
			}
		}

		if len(instancesToRemove) > 0 {
			if err = fm.deleteStorageGameSessions(fm.ctx, instancesToRemove); err != nil {
				fm.logger.WithField("error", err.Error()).Error("failed to delete a game instances")
				return
			}
		}

		if len(placementsToRemove) > 0 {
			fm.logger.WithField("placement_ids", placementsToRemove).Info("removing stale placement entries")
			if err = fm.deleteStoragePlacements(fm.ctx, placementsToRemove); err != nil {
				fm.logger.WithField("error", err.Error()).Error("failed to delete stale placements")
				return
			}
		}
	}

	deleteTerminatedInstancesFn()

	t := time.NewTicker(fm.cfg.AwsGameLiftPollingPeriod)
	for {
		select {
		case <-fm.ctx.Done():
			return
		case <-t.C:
			deleteTerminatedInstancesFn()
		}
	}
}

func (fm *GameLiftFleetManager) listGameliftActiveInstances(ctx context.Context, cursor string) ([]*runtime.InstanceInfo, string, error) {
	params := &gamelift.DescribeGameSessionsInput{
		AliasId:      aws.String(fm.cfg.AwsFleetAlias),
		Limit:        aws.Int32(100), // Max AWS API limit is 100
		StatusFilter: aws.String(string(gltypes.GameSessionStatusActive)),
	}
	if cursor != "" {
		params.NextToken = aws.String(cursor)
	}

	out, err := fm.glService.DescribeGameSessions(ctx, params)
	if err != nil {
		fm.nk.MetricsCounterAdd("gamelift_api_errors", map[string]string{"operation": "DescribeGameSessions"}, 1)
		return nil, "", err
	}

	if len(out.GameSessions) == 0 {
		return nil, "", nil
	}

	activeSessions := make([]*runtime.InstanceInfo, 0, len(out.GameSessions))
	for _, s := range out.GameSessions {
		activeSessions = append(activeSessions, NewInstanceInfo(s))
	}

	var nextCursor string
	if out.NextToken != nil {
		nextCursor = *out.NextToken
	}

	return activeSessions, nextCursor, nil
}

func NewInstanceInfo(gs gltypes.GameSession) *runtime.InstanceInfo {
	metadata := make(map[string]any)
	if gs.GameSessionData != nil {
		metadata[GameSessionDataKey] = *gs.GameSessionData
	}
	if len(gs.GameProperties) > 0 {
		props := make(map[string]string, len(gs.GameProperties))
		for _, p := range gs.GameProperties {
			props[*p.Key] = *p.Value
		}
		metadata[GamePropertiesKey] = props
	}
	if gs.Name != nil {
		metadata[GameSessionNameKey] = *gs.Name
	}

	dns := ""
	if gs.DnsName != nil {
		dns = *gs.DnsName
	}

	return &runtime.InstanceInfo{
		Id: *gs.GameSessionId,
		ConnectionInfo: &runtime.ConnectionInfo{
			IpAddress: *gs.IpAddress,
			DnsName:   dns,
			Port:      int(*gs.Port),
		},
		CreateTime:  *gs.CreationTime,
		PlayerCount: int(*gs.CurrentPlayerSessionCount),
		Status:      string(gs.Status),
		Metadata:    metadata,
	}
}

// PlacementIdToStorageKey returns the storage key of a PLACED entry. The prefix keeps it from ever colliding with a
// game session key.
func PlacementIdToStorageKey(placementId string) string {
	return "placement_" + placementId
}

func InstanceIdToStorageKey(id string) (string, error) {
	tokens := strings.Split(id, "/")
	if len(tokens) < 2 {
		return "", errors.New("failed to convert instance id: %s to storage key")
	}

	return tokens[len(tokens)-1], nil
}

func (fm *GameLiftFleetManager) GeneratePlacementId() string {
	return fm.callbackHandler.GenerateCallbackId()
}
