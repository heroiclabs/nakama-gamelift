module github.com/heroiclabs/nakama-gamelift

go 1.21

require (
	github.com/aws/aws-sdk-go-v2 v1.24.1
	github.com/aws/aws-sdk-go-v2/credentials v1.16.14
	github.com/aws/aws-sdk-go-v2/service/gamelift v1.28.1
	github.com/aws/aws-sdk-go-v2/service/sqs v1.29.7
	github.com/heroiclabs/nakama-common v1.30.2-0.20240213191416-bdb912a5aec6
	google.golang.org/protobuf v1.31.0
)

require (
	github.com/aws/aws-sdk-go-v2/internal/configsources v1.2.10 // indirect
	github.com/aws/aws-sdk-go-v2/internal/endpoints/v2 v2.5.10 // indirect
	github.com/aws/smithy-go v1.19.0 // indirect
	google.golang.org/protobuf v1.31.0 // indirect
)

replace github.com/heroiclabs/nakama-common => ../nakama-common
