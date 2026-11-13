Nakama and Amazon GameLift Integration
===
Nakama Fleet Manager implementation for Amazon GameLift.

## Introduction

The `fleetmanager` package in this repository implements [Nakama](https://heroiclabs.com/nakama)'s Go runtime Fleet Manager interface to interact with an existing [Amazon Gamelift](https://aws.amazon.com/gamelift/) Fleet.

This enables the use of Nakama's social gameplay and matchmaking features for GameLift Game Session discovery and
creation, facilitating the process of Fleet Management orchestration to direct players to existing or new Game Sessions as needed.

## Prerequisites
The Nakama GameLift Fleet Manager expects a GameLift Fleet to be up and running with a specific setup, including:

* A Fleet Alias;
* A Placement Queue;
  * An SNS Topic to publish Placement Events;
* An SQS Queue subscribed to the above Topic;
  * A [redrive policy](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/sqs-dead-letter-queues.html) with a Dead-Letter Queue is strongly recommended. Placement events that fail to be processed are left on the queue to be redelivered, and without a Dead-Letter Queue they are retried until they expire. Malformed events are discarded;
* Invocation of [status update RPCs](#status-update-rpcs) exposed by the Fleet Manager implementation.

Follow [our setup guide](https://heroiclabs.com/docs/nakama/guides/concepts/gamelift-integration/) to create a minimal test Fleet deployment on AWS, with instructions on how to set up the above.

## Installation
To add the GameLift Fleet Manager implementation to your Nakama Go plugin project use the Go command:
```bash
go get github.com/heroiclabs/nakama-gamelift/fleetmanager
```
This will add it to your project's `go.mod` dependencies.

## Usage
The `fleetmanager` instance has to be created within your Nakama plugin `InitModule` global function and then be registered through the `runtime.Initializer`'s exported `RegisterFleetManager()` function.

### Registration
```go
glfm, err := fleetmanager.NewGameLiftFleetManager(ctx, logger, db, nk, awsCfg)
if err != nil {
    return err
}

if err = initializer.RegisterFleetManager(glfm); err != nil {
    logger.WithField("error", err).Error("failed to register amazon gamelift fleet manager")
    return err
}
```

### Multiple Fleet Managers
To use more than one GameLift Fleet from the same Nakama deployment, register each Fleet Manager under a distinct name using `RegisterFleetManagerByName()`. The name is used as the Fleet Manager's namespace: it scopes its storage collection, storage index and [status update RPC IDs](#namespace).
```go
if err = initializer.RegisterFleetManagerByName("eu", glfmEu); err != nil {
    return err
}
if err = initializer.RegisterFleetManagerByName("us", glfmUs); err != nil {
    return err
}
```

Each namespaced Fleet Manager must be configured with its own Placement Queue and Placement Events SQS Queue. If two Fleet Managers consume from the same SQS Queue, either one can receive the other's placement events, and the resulting Game Session data is stored under the wrong namespace.

### Access the Fleet Manager
Once registered, it can be retrieved throughout your Nakama plugin code via the `runtime.NakamaModule` `GetFleetManager()` function:
```go
fm := nk.GetFleetManager() // If fm is nil the FleetManager was not registered.
```

A Fleet Manager registered by name is retrieved with `GetFleetManagerByName()`:
```go
fm := nk.GetFleetManagerByName("eu")
```

The `fm` instance can then be used to list or create Game Sessions.

### Create
Notice that `Create` receives a callback function as part of its signature - the creation process is asynchronous, so the callback is invoked once the creation process either succeeds, times-out or fails.
The callback can be used to notify any interested parties on the status of the creation process:
```go
var callback runtime.FmCreateCallbackFn = func(status runtime.FmCreateStatus, instanceInfo *runtime.InstanceInfo, sessionInfo []*runtime.SessionInfo, metadata map[string]any, createErr error) {
// createErr is not nil only if status is != runtime.CreateSuccess.
// the original GameLift Placement Event can be retrieved from `metadata` under the 'event' key.
switch status {
    case runtime.CreateSuccess:
        // Create was successful, instanceInfo contains the instance information for player connection.
        // sessionInfo contains Player Session info if a list of userIds is passed to the Create function.
        info, _ := json.Marshal(instanceInfo)
        logger.Info("GameLift instance created: %s", info)
        // Notify any interested party.
        return
    case runtime.CreateTimeout:
        // Amazon GameLift was not able to successfully create the placed Game Session request within the timeout
        // (configurable in the placement queue).
        // The client should be notified to either reattempt to find an available Game Session or retry creation.
        info, _ := json.Marshal(instanceInfo)
        logger.Info("GameLift instance created: %s", info)
        // Notify any interested party.
        return
    default:
        // The request failed to be placed.
        logger.WithField("error", createErr.Error()).Error("Failed to create GameLift instance")
        // Notify any interested party.
        return
    }
}

// maxPlayers: Maximum number of players supported by the game session.
maxPlayers := 10
// playerIds - Optional: Reserves a Player Session for each userId. The reservation expires after 60s if the client doesn't connect.
playerIds := []string{userId}
// metadata - Optional: Can contain the following keys: GameSessionDataKey, GamePropertiesKey and GameSessionNameKey.
// - GameSessionDataKey's value should be a map[string]any. It is serialized as JSON before being sent to GameLift, so the
//   Game Session must deserialize the GameSessionData string it fetches from GameLift. See "Game Session Data" below.
// - GamePropertiesKey's value should be a map[string]string.
// - GameSessionNameKey's value should be a string.
metadata := map[string]any{
    fleetmanager.GameSessionDataKey: map[string]any{"mode": "ranked", "map": "dust"},
    fleetmanager.GamePropertiesKey:  map[string]string{"key1": "value1"},
}
// latencies - Optional: - A list of latencies to different aws regions, per userId, experienced from the client.
latencies := []runtime.FleetUserLatencies{{
    UserId:                userId,
    LatencyInMilliseconds: 100,
    RegionIdentifier:      "us-east-1",
}}

createMetadata, err = fm.Create(ctx, maxPlayers, playerIds, latencies, metadata, callback)
placementId := createMetadata[fleetmanager.PlacementIdKey] // The PlacementIdKey can be used to return the PlacementId of the Game Session creation request.
```

#### Game Session Data
`Create` always sets the Game Session's `GameSessionData`, even if `GameSessionDataKey` is not provided. The `GameSessionDataKey` map is serialized as a JSON object string, with the following keys added by the Fleet Manager:

* `nakama_gamelift_namespace`: The namespace the Fleet Manager was registered with.
* `nakama_gamelift_update_rpc_id`: The ID of the [Update RPC](#update-rpc).
* `nakama_gamelift_delete_rpc_id`: The ID of the [Delete RPC](#delete-rpc).

Any user-provided keys with these names are overwritten. Within the Game Session, deserialize the `GameSessionData` string retrieved from the GameLift Server SDK as JSON to access the values, for example:
```json
{
  "mode": "ranked",
  "map": "dust",
  "nakama_gamelift_namespace": "",
  "nakama_gamelift_update_rpc_id": "gamelift_update_instance_info",
  "nakama_gamelift_delete_rpc_id": "gamelift_delete_instance_info"
}
```

### List
To query for existing Game Sessions, `List` can be used with the [Query Syntax](https://heroiclabs.com/docs/nakama/concepts/multiplayer/query-syntax/):
```go
query := "+value.player_count:2" // Query to list Game Sessions currently containing 2 Player Sessions. An empty query will list all Game Sessions.
limit := 10 // Number of results per page (does not apply if query is != "").
cursor := "" // Pagination cursor.
instances, nextCursor, err := fm.List(ctx, query, limit, cursor)
```

### Get
Get the information of a Game Session:
```go
id := "<Game Session ARN>"
instance, err := fm.Get(ctx, id)
```

### Join
Reserve a seat on an existing Game Session and retrieve the corresponding Player Session needed for a client to connect to it:
```go
id := "<game session ARN>"
userIds := []string{userId}
metadata := map[string]string{
  userId: "<player data>",
} // metadata is optional. It can be used to set an arbitrary string that can be retrieved from the Game Session. Each key needs to match an userId present in userIds, otherwise it is ignored.
joinInfo, err := fm.Join(ctx, id string, userIds []string, metadata map[string]string)
```

### Seat reservations
Nakama is the source of truth for a Game Session's `player_count`. It counts the Player Sessions connected to the Game Session, as reported by the [Update RPC](#update-rpc), plus the seats reserved for Player Sessions that haven't connected yet.

A seat is reserved for each Player Session created by a successful `Join`, and for each placed Player Session when a placement created with `Create` is fulfilled. A reserved seat becomes a connected seat when its Player Session is reported as connected, and is released after 1 minute otherwise, matching the fixed time GameLift waits for a reserved Player Session to be accepted.

Expired reservations are released on the next write to the Game Session, or by the background sync worker, so a Game Session may appear fuller than it is until then. Game Sessions that invoke the Update RPC as a [heartbeat](#heartbeats) at an interval of at most 1 minute release them promptly.

### Delete
Delete should be used to remove an InstanceInfo data of a Game Session that was terminated on GameLift:
```go
id := "<game session ARN>"
err := fm.Delete(ctx, id)
```

### Configuration
The `fleetmanager` instance requires an `AwsConfig` with the following settings on creation:

* `Key`: AWS Key;
* `Secret`: AWS Secret;
* `Region`: AWS Fleet Region;
* `FleetAlias`: AWS Fleet Alias ID;
* `PlacementQueueName`: AWS Fleet Placement Queue Name;
* `PlacementEventsSqsUrl`: AWS GameLift Placement Events SQS URL.

Typically, these values would be passed to Nakama via its [Nakama's runtime env configuration](https://heroiclabs.com/docs/nakama/getting-started/configuration/#runtime.env).

The other settings have defaults, which can be overridden by passing options to `NewGameLiftFleetManager`:

| Option | Default | Description |
| --- | --- | --- |
| `WithSqsMessageBatchSize` | `10` | The max number of placement events fetched and processed per SQS request (1-10). |
| `WithSqsWorkers` | `1` | The number of concurrent consumers polling the SQS queue for placement events (minimum 1). |
| `WithSqsVisibilityTimeout` | 10 seconds | How long a received placement event is hidden from other consumers before it's redelivered if it isn't processed. |
| `WithSqsWaitTime` | 20 seconds | How long an SQS request waits for placement events to arrive (0-20 seconds). |
| `WithPlacementTimeout` | 10 minutes | How long a pending placement is tracked before it's considered stale and cleaned up. It should be greater than or equal to the GameLift Placement Queue timeout (600 seconds by default). |
| `WithGameLiftPollingPeriod` | 15 minutes | How often GameLift is polled to remove terminated Game Sessions and stale placements from storage. |
| `WithHttpRequestTimeout` | 30 seconds | The AWS HTTP client request timeout. It must be greater than the SQS wait time. |
| `WithHttpDialerTimeout` | 15 seconds | The AWS HTTP client dialing timeout. |
| `WithHeartbeatTimeout` | `0` (disabled) | How long a Game Session can go without invoking the [Update RPC](#heartbeats) before it's excluded from `List` queries. It should be at least 3 times the heartbeat interval. Only enable it once all game servers send heartbeats, see the [warning](#heartbeats). |
| `WithIndexMaxEntries` | `1000000` | The max entries of the Nakama storage index, which limits the number of Game Sessions that can be listed. |

### Example
```go
func InitModule(ctx context.Context, logger runtime.Logger, db *sql.DB, nk runtime.NakamaModule, initializer runtime.Initializer) error {
  env, ok := ctx.Value(runtime.RUNTIME_CTX_ENV).(map[string]string)
  if !ok {
    return fmt.Errorf("expects env ctx value to be a map[string]string")
  }

  awsAccessKey, ok := env["GL_AWS_ACCESS_KEY"]
  if !ok {
    return runtime.NewError("missing GL_AWS_ACCESS_KEY environment variable", 3)
  }

  awsSecretAccessKey, ok := env["GL_AWS_SECRET_ACCESS_KEY"]
  if !ok {
    return runtime.NewError("missing GL_AWS_SECRET_ACCESS_KEY environment variable", 3)
  }

  awsRegion, ok := env["GL_AWS_REGION"]
  if !ok {
    return runtime.NewError("missing GL_AWS_REGION environment variable", 3)
  }

  awsAliasId, ok := env["GL_AWS_ALIAS_ID"]
  if !ok {
    return runtime.NewError("missing GL_AWS_ALIAS_ID environment variable", 3)
  }

  awsPlacementQueueName, ok := env["GL_PLACEMENT_QUEUE_NAME"]
  if !ok {
    return runtime.NewError("missing GL_PLACEMENT_QUEUE_NAME environment variable", 3)
  }

  awsGameLiftPlacementEventsQueueUrl, ok := env["GL_PLACEMENT_EVENTS_SQS_URL"]
  if !ok {
    return runtime.NewError("missing GL_PLACEMENT_EVENTS_SQS_URL environment variable", 3)
  }

  awsCfg := fleetmanager.AwsConfig{
    Key:                   awsAccessKey,
    Secret:                awsSecretAccessKey,
    Region:                awsRegion,
    FleetAlias:            awsAliasId,
    PlacementQueueName:    awsPlacementQueueName,
    PlacementEventsSqsUrl: awsGameLiftPlacementEventsQueueUrl,
  }

  glfm, err := fleetmanager.NewGameLiftFleetManager(ctx, logger, db, nk, awsCfg,
    fleetmanager.WithSqsWorkers(2),                    // Number of concurrent SQS consumers (>= 1).
    fleetmanager.WithPlacementTimeout(15*time.Minute), // Should be >= the GameLift Placement Queue timeout.
  )
  if err != nil {
    return err
  }

  if err = initializer.RegisterFleetManager(glfm); err != nil {
    logger.WithField("error", err).Error("failed to register amazon gamelift fleet manager")
    return err
  }

  return nil
}
```

## Quickstart
If you wish to start a new Nakama Go runtime plugin project using the Fleet Manager simply clone this project:
```bash
git clone https://github.com/heroiclabs/nakama-gamelift/
```
To run it locally using [Docker](https://www.docker.com/) follow the steps in the [Docker section](#run-locally-with-docker).

## Run locally with Docker
The plugin must be built with the same `nakama-common` version as the Nakama server that loads it. This version of the Fleet Manager requires Nakama 3.42.0 or later; the image version is set by the `NAKAMA_VERSION` build argument in the `Dockerfile`.

### Configuration
To connect to your existing fleet, make sure you modify all the required runtime env parameters in the `local.yml` file.

The plugin sample code in this repository can be loaded and run into a Nakama container using the following Docker Compose command:
```bash
docker compose up
```

## Status Update RPCs
For Nakama to be up-to-date on the number of Player Sessions currently connected to a Game Session and for it to be aware of when a Game Session is terminated, the Game Session SDK should invoke two RPCs that are automatically exposed as part of the `fleetmanager` registration. These RPCs can be invoked as [Server-to-Server calls](https://heroiclabs.com/docs/nakama/server-framework/runtime-examples/#server-to-server).

The RPC IDs can be retrieved from the Game Session's JSON-encoded [Game Session Data](#game-session-data) under the keys:
* `nakama_gamelift_update_rpc_id`
* `nakama_gamelift_delete_rpc_id`

### Namespace
The RPC IDs are namespaced with the `gamelift_<namespace>` prefix, where `<namespace>` is the name the Fleet Manager was registered with using `RegisterFleetManagerByName()`. If the Fleet Manager was registered with `RegisterFleetManager()`, the namespace is empty and the prefix is simply `gamelift_`. RPC IDs are always lowercase, so a Fleet Manager registered as `EU` exposes `gamelift_eu_update_instance_info`.

### Update RPC
* RpcId: `gamelift_<namespace>_update_instance_info`

This RPC should be invoked any time a player connects or disconnects from the Game Session, and periodically as a [heartbeat](#heartbeats), passing the IDs of all the Player Sessions currently connected to it in `player_session_ids`. These are the Player Session IDs the Game Session accepts with the Server SDK's `AcceptPlayerSession` when clients connect, so no further GameLift calls are needed. Nakama maps the [seats it reserved](#seat-reservations) back to their users, while Player Sessions that weren't created through Nakama are counted as connected players without a known user.

A player's seat is released as soon as its Player Session is missing from `player_session_ids`, but GameLift only frees it when the Game Session calls the Server SDK's `RemovePlayerSession`. Game Sessions must call it when a player disconnects, otherwise GameLift may reject new Player Sessions as the Game Session is full while Nakama lists it as having room.

Since each update replaces the connected Player Sessions, updates applied out of order would overwrite the current ones with outdated ones. To prevent this, Game Sessions should set `seq` to a number that increases with every update, such as a counter or the current time in milliseconds. The connected Player Sessions of an update whose `seq` isn't greater than the last applied one are ignored, while its metadata is still applied. Updates without `seq` are always applied.

The deprecated `player_count` field is still accepted when `player_session_ids` isn't set. Since the connected Player Sessions aren't known, it releases all the Game Session's seat reservations.

It can also be used to update a Game Session's `GameSessionName`, `GameSessionData` or `GameProperties`. Updating these metadata fields allow the filtering of Game Sessions in the `List` function based on the properties values.
Each of metadata's expected keys are optional, and only if any is present with a value will it overwrite the current value in the Nakama Storage Index.
These updates are only stored in Nakama, which owns the Game Session's metadata, and aren't sent to GameLift. The values the Game Session retrieves from the GameLift Server SDK remain the ones it was created with.
The expected RPC payload is the following:
```json
{
  "id": "<Game Session ARN>",
  "seq": 42,
  "player_session_ids": ["<player session id>", "<player session id>"],
  "metadata": {
    "GameSessionData": "<game_session_data>",
    "GameSessionName": "<game_session_name>",
    "GameProperties": {
      "key1": "value1",
      "key2": "value2"
    }
  }
}
```

#### Heartbeats
Invoking the Update RPC periodically, in addition to when players connect or disconnect, corrects any drift in the Game Session's `player_count`, for instance from a missed or failed update, within one interval. It also releases expired seat reservations promptly. An interval of 30 to 60 seconds is recommended. Heartbeats don't need to include metadata, and heartbeats that don't change anything aren't written to storage.

Heartbeat tracking can be enabled with the `WithHeartbeatTimeout` option. `List` queries then exclude Game Sessions that haven't invoked the Update RPC within the timeout, such as unresponsive Game Sessions that GameLift hasn't terminated yet. The timeout should be at least 3 times the heartbeat interval. Game Sessions stored before heartbeat tracking was enabled remain listed until their next update.

> **Warning:** Only enable heartbeat tracking once every game server build running in the fleet invokes the Update RPC periodically. Game Sessions that only invoke it when players connect or disconnect are excluded from `List` queries after the timeout whenever no player connects or disconnects within it, including empty Game Sessions waiting for their first player, and are only listed again on their next update.

### Delete RPC
* RpcId: `gamelift_<namespace>_delete_instance_info`

This RPC should be invoked any time a Game Session terminates.
The expected RPC payload is the following:
```json
{
  "id": "<Game Session ARN>"
}
```
