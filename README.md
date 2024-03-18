Nakama and AWS GameLift Integration
===
Nakama Fleet Manager implementation for AWS GameLift.

## Introduction

The `fleetmanager` package in this repository implements [Nakama](https://heroiclabs.com/nakama)'s Go runtime Fleet Manager interface to interact with an existing [AWS Gamelift](https://aws.amazon.com/gamelift/) Fleet.

This enables the use of Nakama's social gameplay and matchmaking features for GameLift Game Session discovery and
creation, facilitating the process of Fleet Management orchestration to direct players to existing or new Game Sessions as needed.

## Prerequisites
The Nakama GameLift Fleet Manager expects an AWS Fleet to be up and running with a specific setup, including:

* A Fleet Alias;
* A Placement Queue;
  * An SNS Topic to publish Placement Events;
* An SQS Queue subscribed to the above Topic;
* Invocation of [status update RPCs](#status-update-rpcs) exposed by the Fleet Manager implementation.

Follow [our setup guide](//TODO) to create a minimal test Fleet deployment on AWS, with instructions on how to set up the above.

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
glfm, err := fleetmanager.NewGameLiftFleetManager(ctx, logger, db, initializer, nk, cfg)
if err != nil {
    return err
}

if err = initializer.RegisterFleetManager(glfm); err != nil {
    logger.WithField("error", err).Error("failed to register aws gamelift fleet manager")
    return err
}
```

### Access the Fleet Manager
Once registered, it can be retrieved throughout your Nakama plugin code via the `runtime.NakamaModule` `GetFleetManager()` function:
```go
fm := nk.GetFleetManager() // If fm is nil the FleetManager was not registered.
```

The `fm` instance can then be used to list or create Game Sessions.

### Create
Notice that `Create` receives a callback function as part of its signature - the creation process is asynchronous, so the callback is invoked once the creation process either succeeds, times-out or fails.
The callback can be used to notify any interested parties on the status of the creation process:
```go
var callback runtime.FmCreateCallbackFn = func(status runtime.FmCreateStatus, instanceInfo *runtime.InstanceInfo, sessionInfo []*runtime.SessionInfo, metadata map[string]any, createErr error) {
// createErr is not nil only if status is != runtime.CreateSuccess.
// the original AWS Placement Event can be retrieved from `metadata` under the 'event' key.
switch status {
    case runtime.CreateSuccess:
        // Create was successful, instanceInfo contains the instance information for player connection.
        // sessionInfo contains Player Session info if a list of userIds is passed to the Create function.
        info, _ := json.Marshal(instanceInfo)
        logger.Info("GameLift instance created: %s", info)
        // Notify any interested party.
        return
    case runtime.CreateTimeout:
        // AWS GameLift was not able to successfully create the placed Game Session request within the timeout
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
// Expects the value of GameSessionDataKey and GameSessionNameKey to be strings. GamePropertiesKey's value should be a map[string]string
metadata := map[string]any{GamePropertiesKey: map[string]string{"key1":"value1"}}
// latencies - Optional: - A list of latencies to different aws regions, per userId, experienced from the client.
latencies := []runtime.FleetUserLatencies{{
    UserId:                userId,
    LatencyInMilliseconds: 100,
    RegionIdentifier:      "us-east-1",
}}

err = fm.Create(ctx, maxPlayers, playerIds, latencies, metadata, callback)
```

### List
To query for existing Game Sessions, `List` can be used with the [Query Syntax](https://heroiclabs.com/docs/nakama/concepts/multiplayer/query-syntax/):
```go
query := "+value.playerCount:2" // Query to list Game Sessions currently containing 2 Player Sessions. An empty query will list all Game Sessions.
limit := 10 // Number of results per page (does not apply if a query is != ""
cursor := "" // Pagination cursor
instances, nextCursor, err := fm.List(ctx, query, limit, cursor)
```

### Get
Grab the information of a Game Session:
```go
id := "<Game Session ARN>"
instance, err := fm.Get(ctx, id)
```

### Join
To reserve a seat on an existing Game Session and retrieve the corresponding Player Session data needed for a client to connect to it:
```go
id := "<game session ARN>"
userIds := []string{userId}
metadata := map[string]string{
  userId: "<player data>",
} // metadata is optional. It can be used to set an arbitrary string that can be retrieved from the Game Session. Each key needs to match an userId present in userIds, otherwise it is ignored.
joinInfo, err := fm.Join(ctx, id string, userIds []string, metadata map[string]string)
```

### Delete
Delete should be used to delete an InstanceInfo data regarding a Game Session that was terminated on GameLift:
```go
id := "<game session ARN>"
err := fm.Delete(ctx, id)
```

### Configuration
The `fleetmanager` instance expects a number of required configurations to be provided upon creation:

* AWS Key;
* AWS Secret;
* AWS Fleet Region;
* AWS Fleet Alias ID;
* AWS Fleet Placement Queue Name;
* AWS GameLift Placement Events SQS URL;

Typically, these values would be passed exposed to Nakama via its [Nakama's runtime env configuration](https://heroiclabs.com/docs/nakama/getting-started/configuration/#runtime.env).

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
  glfm, err := fleetmanager.NewGameLiftFleetManager(ctx, logger, db, initializer, nk, cfg)
  if err != nil {
    return err
  }

  if err = initializer.RegisterFleetManager(glfm); err != nil {
  logger.WithField("error", err).Error("failed to register aws gamelift fleet manager")
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
### Configuration
To connect to your existing fleet, make sure you modify all the required runtime env parameters in the `local.yml` file.

The plugin sample code in this repository can be loaded and run into a Nakama container using the following Docker Compose command:
```bash
docker compose up
```

## Status Update RPCs
For Nakama to be up-to-date on the number of Player Sessions currently connected to a Game Session and for it to be aware of when a Game Session is terminated, the Game Session SDK should invoke two RPCs that are automatically exposed as part of the `fleetmanager` registration. These RPCs can be invoked as [Server-to-Server calls](https://heroiclabs.com/docs/nakama/server-framework/runtime-examples/#server-to-server).

### Update RPC
* RpcId: `update_instance_info`

This RPC should be invoked any time a player connects or disconnects from the Game Session to update the playerCount by passing the current number of connected players.

It can also be used to update a Game Session's `GameSessionName`, `GameSessionData` or `GameProperties`. Updating these metadata fields allow the filtering of Game Sessions in the `List` function based on the properties values.
Each of metadata's expected keys are optional, and only if present with a value will overwrite the current value in the Nakama storage index.
```json
{
  "id": "<Game Session ARN>",
  "playerCount": 10,
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
* RpcId: `delete_instance_info`

This RPC should be invoked any time a Game Session terminates, and it expects the following payload:
```json
{
  "id": "<Game Session ARN>"
}
```

