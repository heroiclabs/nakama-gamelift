package fleetmanager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/gamelift"
	"github.com/aws/aws-sdk-go-v2/service/gamelift/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/heroiclabs/nakama-common/runtime"
)

const (
	StorageGameLiftIndex               = "_gamelift_instances_idx"
	StorageGameLiftInstancesCollection = "_gamelift_instances"
)

const (
	GameSessionDataKey = "GameSessionData"
	GamePropertiesKey  = "GameProperties"
	GameSessionNameKey = "GameSessionName"
)

const (
	RpcIdUpdateInstanceInfo = "update_instance_info"
	RpcIdDeleteInstanceInfo = "delete_instance_info"
)

var _ runtime.FleetManagerInitializer = &GameLiftFleetManager{}

type GameLiftFleetManager struct {
	ctx             context.Context
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
	IndexMaxEntries          int           // Nakama storage index max entries - limits the number of game sessions that can be indexed.
	HttpRequestTimeout       time.Duration // AWS Http Client request timeout
	HttpDialerTimeout        time.Duration // AWS Http Client request dialing timeout
	SqsVisibilityTimeout     time.Duration // AWS SQS Client Visibility Timeout
	SqsWaitTime              time.Duration // AWS SQS Client Wait Time
}

func NewGameLiftConfig(awsKey, awsSecret, awsRegion, awsGameLiftFleetAlias, awsGameLiftPlacementQueueName, awsGameLiftPlacementEventsSqsUrl string) GameLiftConfig {
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

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	return nil
}

func NewGameLiftFleetManager(ctx context.Context, logger runtime.Logger, db *sql.DB, initializer runtime.Initializer, nk runtime.NakamaModule, conf GameLiftConfig) (runtime.FleetManagerInitializer, error) {
	nakamaNode, ok := ctx.Value(runtime.RUNTIME_CTX_NODE).(string)
	if !ok || !strings.HasPrefix(nakamaNode, "nakama") {
		return nil, errors.New("failed to get nakama node from ctx")
	}
	conf.NakamaNode = nakamaNode
	if err := conf.Validate(); err != nil {
		return nil, err
	}

	if err := initializer.RegisterStorageIndex(StorageGameLiftIndex, StorageGameLiftInstancesCollection, "", []string{"id", "create_time", "player_count", "metadata"}, []string{"create_time", "player_count"}, 1_000_000, false); err != nil {
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

	// NOTE: This RPC is required and must be invoked from a Game Session whenever a player connects/disconnects.
	// Refer to the documentation for more information.
	if err := initializer.RegisterRpc(RpcIdUpdateInstanceInfo, glfm.UpdateInstanceInfo); err != nil {
		return nil, err
	}
	// NOTE: This RPC is required and must be invoked from the Game Session whenever a Game Session terminates.
	// Refer to the documentation for more information.
	if err := initializer.RegisterRpc(RpcIdDeleteInstanceInfo, glfm.DeleteInstanceInfo); err != nil {
		return nil, err
	}

	// TODO: Delete
	if err := initializer.RegisterRpc("find_game_session", glfm.findGameSession); err != nil {
		return nil, err
	}

	return glfm, nil
}

func (fm *GameLiftFleetManager) Init(nk runtime.NakamaModule, callbackHandler runtime.FmCallbackHandler) error {
	fm.nk = nk
	fm.callbackHandler = callbackHandler

	// Initialise sqs background consumer here as it depends on the callbackHandler
	go fm.processSqsPlacementEvents()
	go fm.syncInstancesWorker()

	return nil
}

func (fm *GameLiftFleetManager) Create(ctx context.Context, maxPlayers int, userIds []string, latencies []runtime.FleetUserLatencies, metadata map[string]any, callback runtime.FmCreateCallbackFn) error {
	var desiredPlayerSessions []types.DesiredPlayerSession
	if len(userIds) > 0 {
		desiredPlayerSessions = make([]types.DesiredPlayerSession, 0, len(userIds))
		for _, id := range userIds {
			var playerData *string
			if m := metadata[id]; m != nil {
				if s, ok := m.(string); ok {
					playerData = new(string)
					*playerData = s
				}
			}

			desiredPlayerSessions = append(desiredPlayerSessions, types.DesiredPlayerSession{
				PlayerData: playerData,
				PlayerId:   aws.String(id),
			})
		}
	}
	fm.logger.WithField("desired_sessions", desiredPlayerSessions).Debug("desired player sessions")

	var gameSessionData *string
	if m := metadata[GameSessionDataKey]; m != nil {
		if s, ok := m.(string); ok {
			gameSessionData = new(string)
			*gameSessionData = s
		}
	}

	var gameProperties []types.GameProperty
	if m := metadata[GamePropertiesKey]; m != nil {
		if gp, ok := m.(map[string]string); ok {
			props := make([]types.GameProperty, 0, len(gp))
			for k, v := range gp {
				pk, pv := new(string), new(string)
				*pk, *pv = k, v
				props = append(props, types.GameProperty{
					Key:   pk,
					Value: pv,
				})
			}
			gameProperties = props
		} else {
			return errors.New("invalid metadata key 'GameProperties' value: must be map[string]string")
		}
	}

	var gameSessionName *string
	if m := metadata[GameSessionNameKey]; m != "" {
		if s, ok := m.(string); ok {
			gameSessionName = new(string)
			*gameSessionName = s
		}
	}

	var playerLatencies []types.PlayerLatency
	if len(latencies) > 0 {
		playerLatencies = make([]types.PlayerLatency, 0, len(playerLatencies))
		for _, l := range latencies {
			playerLatencies = append(playerLatencies, types.PlayerLatency{
				LatencyInMilliseconds: aws.Float32(l.LatencyInMilliseconds),
				PlayerId:              aws.String(l.UserId),
				RegionIdentifier:      aws.String(l.RegionIdentifier),
			})
		}
	}

	placementId := fm.callbackHandler.GenerateCallbackId()
	fm.logger.WithField("placement_id", placementId).WithField("game_properties", gameProperties).Debug("placement input")

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
		return err
	}

	switch placementOutput.GameSessionPlacement.Status {
	case types.GameSessionPlacementStatePending:
		fallthrough
	case types.GameSessionPlacementStateFulfilled:
		fm.logger.WithField("placement_id", placementId).Debug("placement started")
	case types.GameSessionPlacementStateCancelled:
		fallthrough
	case types.GameSessionPlacementStateTimedOut:
		fallthrough
	case types.GameSessionPlacementStateFailed:
		return errors.New("failed to start game session placement")
	}

	if callback != nil {
		fm.callbackHandler.SetCallback(placementId, callback)
	}

	return nil
}

func (fm *GameLiftFleetManager) Get(ctx context.Context, id string) (instance *runtime.InstanceInfo, err error) {
	params := &gamelift.DescribeGameSessionsInput{
		GameSessionId: &id,
		Limit:         aws.Int32(1),
	}

	out, err := fm.glService.DescribeGameSessions(ctx, params)
	if err != nil {
		return nil, err
	}

	if len(out.GameSessions) == 0 {
		return nil, runtime.NewError(fmt.Sprintf("game session with id: %q not found", id), 5)
	}

	gs := out.GameSessions[0]

	instance = NewInstanceInfo(gs)

	switch gs.Status {
	case types.GameSessionStatusActive:
		if err = fm.updateStorageGameSessions(ctx, []*runtime.InstanceInfo{instance}); err != nil {
			return nil, errors.New("failed to update gamelift game session instance info")
		}
	case types.GameSessionStatusTerminated, types.GameSessionStatusTerminating, types.GameSessionStatusError:
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

	entries, err := fm.nk.StorageIndexList(ctx, "", StorageGameLiftIndex, query, limit, []string{"player_count", "-create_time"}) // Emptiest matches, most recently created first.
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
		objects, nextCursor, err := fm.nk.StorageList(ctx, "", "", StorageGameLiftInstancesCollection, 1_000, cursor)
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
		Collection: StorageGameLiftInstancesCollection,
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
			Collection: StorageGameLiftInstancesCollection,
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
			Collection: StorageGameLiftInstancesCollection,
			Key:        key,
		})
	}

	if err := fm.nk.StorageDelete(ctx, deletes); err != nil {
		return err
	}

	return nil
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

type indexListRequest struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
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

	var gameProps []types.GameProperty
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
			gameProps = make([]types.GameProperty, 0, len(mmap))
			for k, v := range mmap {
				kk, vv := new(string), new(string)
				*kk, *vv = k, v.(string)
				gameProps = append(gameProps, types.GameProperty{
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
		MaxNumberOfMessages: 1,
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

				fm.logger.WithField("error", err.Error()).Error("failed to fetch aws sqs messages from queue.")
				continue
			}

			if len(message.Messages) == 0 {
				continue
			}

			m := message.Messages[0]
			logger := fm.logger.WithField("message_id", *m.MessageId)
			logger.WithField("message_body", *m.Body).Debug("Sqs placement notification received.")

			var queueMsg SqsMessage
			if err = json.Unmarshal([]byte(*m.Body), &queueMsg); err != nil {
				logger.WithField("error", err.Error()).Error("failed to unmarshal sqs queue message body.")
				continue
			}

			var placementEventMsg *SqsMessagePlacementEventBody
			if err = json.Unmarshal([]byte(queueMsg.Message), &placementEventMsg); err != nil {
				logger.WithField("error", err.Error()).Error("failed to unmarshal sqs event body.")
				continue
			}

			placementId := placementEventMsg.Detail.PlacementId

			eventMetadata := map[string]any{"event": *m.Body}

			switch placementEventMsg.Detail.Type {
			case GameLiftEventIdPlacementFulfilled:
				instanceInfo, err := fm.Get(ctx, placementEventMsg.Detail.GameSessionArn)
				if err != nil {
					logger.WithField("error", err.Error()).Error("failed to get session info from gamelift")
					continue
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

				go fm.callbackHandler.InvokeCallback(placementId, runtime.CreateSuccess, instanceInfo, sessions, eventMetadata, nil)
			case GameLiftEventIdPlacementTimedOut:
				go fm.callbackHandler.InvokeCallback(placementId, runtime.CreateTimeout, nil, nil, eventMetadata, errors.New(placementEventMsg.Detail.Type))
			case GameLiftEventIdPlacementCancelled:
				fallthrough
			case GameLiftEventIdPlacementFailed:
				go fm.callbackHandler.InvokeCallback(placementId, runtime.CreateError, nil, nil, eventMetadata, errors.New(placementEventMsg.Detail.Type))
			default:
				logger.Warn("unhandled game session placement state: %s", placementEventMsg.Detail.Type)
			}

			if _, err = fm.sqsService.DeleteMessage(ctx, &sqs.DeleteMessageInput{
				QueueUrl:      aws.String(fm.cfg.AwsPlacementEventsSqsUrl),
				ReceiptHandle: m.ReceiptHandle,
			}); err != nil {
				logger.WithField("error", err.Error()).Error("failed to delete sqs message")
				continue
			}
		}
	}
}

func (fm *GameLiftFleetManager) syncInstancesWorker() {
	deleteTerminatedInstancesFn := func() {
		cursor := ""
		for {
			activeInstances, nextCursor, err := fm.listGameliftActiveInstances(fm.ctx, cursor)
			if err != nil {
				fm.logger.WithField("error", err.Error()).Error("failed to list gamelift terminated instances")
				return
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
			for _, dbInfo := range dbInstances {
				if _, ok := activeInstancesMap[dbInfo.Id]; !ok {
					instancesToRemove = append(instancesToRemove, dbInfo.Id)
				}
			}

			if err = fm.updateStorageGameSessions(fm.ctx, activeInstances); err != nil {
				fm.logger.WithField("error", err.Error()).Error("failed to update db instances")
				return
			}

			if err = fm.deleteStorageGameSessions(fm.ctx, instancesToRemove); err != nil {
				fm.logger.WithField("error", err.Error()).Error("failed to delete a game instances")
				return
			}
			if nextCursor == "" {
				return
			}
			cursor = nextCursor
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
		StatusFilter: aws.String(string(types.GameSessionStatusActive)),
	}
	if cursor != "" {
		params.NextToken = aws.String(cursor)
	}

	out, err := fm.glService.DescribeGameSessions(ctx, params)
	if err != nil {
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

type findGameSessionRequest struct {
	Query  string `json:"query"`
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor"`
}

func (fm *GameLiftFleetManager) findGameSession(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, payload string) (string, error) {
	userId, ok := ctx.Value(runtime.RUNTIME_CTX_USER_ID).(string)
	if !ok {
		return "", ErrInvalidInput
	}

	var req *findGameSessionRequest
	if payload != "" {
		if err := json.Unmarshal([]byte(payload), &req); err != nil {
			logger.WithField("error", err.Error()).Error("failed to unmarshal findOrCreate request")
			return "", ErrInternalError
		}
	} else {
		req = &findGameSessionRequest{
			Limit: 10,
		}
	}

	instances, _, err := fm.List(ctx, req.Query, req.Limit, req.Cursor)
	if err != nil {
		logger.WithField("error", err.Error()).Error("failed to list gamelift instances")
		return "", ErrInternalError
	}

	if len(instances) > 0 {
		instance := instances[0]
		joinInfo, err := fm.Join(ctx, instance.Id, []string{userId}, nil)
		if err != nil {
			logger.WithField("error", err.Error()).Error("failed to join gamelift instance")
			return "", ErrInternalError
		}

		out, err := json.Marshal(joinInfo)
		if err != nil {
			logger.WithField("error", err.Error()).Error("failed to marshal instances payload")
			return "", ErrInternalError
		}

		return string(out), nil
	}

	var callback runtime.FmCreateCallbackFn = func(status runtime.FmCreateStatus, instanceInfo *runtime.InstanceInfo, sessionInfo []*runtime.SessionInfo, metadata map[string]any, createErr error) {
		switch status {
		case runtime.CreateSuccess:
			info, _ := json.Marshal(instanceInfo)
			logger.Info("GameLift instance created: %s", info)
			return
		default:
			logger.WithField("error", createErr.Error()).Error("Failed to create GameLift instance")
			return
		}
	}

	if err = fm.Create(ctx, 10, []string{userId}, nil, nil, callback); err != nil {
		logger.WithField("error", err.Error()).Error("failed to create new fleet game session")
		return "", ErrInternalError
	}

	return "", nil
}

func NewInstanceInfo(gs types.GameSession) *runtime.InstanceInfo {
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

func InstanceIdToStorageKey(id string) (string, error) {
	tokens := strings.Split(id, "/")
	if len(tokens) < 2 {
		return "", errors.New("failed to convert instance id: %s to storage key")
	}

	return tokens[len(tokens)-1], nil
}
