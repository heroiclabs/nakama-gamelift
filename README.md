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
glfm, err := fleetmanager.NewGameLiftFleetManager(ctx, logger, db, nk, cfg)
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

### Delete
Delete should be used to remove an InstanceInfo data of a Game Session that was terminated on GameLift:
```go
id := "<game session ARN>"
err := fm.Delete(ctx, id)
```

### Configuration
The `fleetmanager` instance expects a number of required configurations to be provided on creation:

* AWS Key;
* AWS Secret;
* AWS Fleet Region;
* AWS Fleet Alias ID;
* AWS Fleet Placement Queue Name;
* AWS GameLift Placement Events SQS URL;

Typically, these values would be passed to Nakama via its [Nakama's runtime env configuration](https://heroiclabs.com/docs/nakama/getting-started/configuration/#runtime.env).

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

  cfg := fleetmanager.NewGameLiftConfig(awsAccessKey, awsSecretAccessKey, awsRegion, awsAliasId, awsPlacementQueueName, awsGameLiftPlacementEventsQueueUrl)

  glfm, err := fleetmanager.NewGameLiftFleetManager(ctx, logger, db, nk, cfg)
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

This RPC should be invoked any time a player connects or disconnects from the Game Session to update the `player_count` by passing the current number of connected players.

It can also be used to update a Game Session's `GameSessionName`, `GameSessionData` or `GameProperties`. Updating these metadata fields allow the filtering of Game Sessions in the `List` function based on the properties values.
Each of metadata's expected keys are optional, and only if any is present with a value will it overwrite the current value in the Nakama Storage Index.
The expected RPC payload is the following:
```json
{
  "id": "<Game Session ARN>",
  "player_count": 10,
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

### Delete RPC
* RpcId: `gamelift_<namespace>_delete_instance_info`

This RPC should be invoked any time a Game Session terminates.
The expected RPC payload is the following:
```json
{
  "id": "<Game Session ARN>"
}
```
