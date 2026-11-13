package fleetmanager

import (
	"errors"
	"time"
)

const (
	defaultGameLiftPollingPeriod = 15 * time.Minute
	defaultHttpRequestTimeout    = 30 * time.Second
	defaultHttpDialerTimeout     = 15 * time.Second
	defaultPlacementTimeout      = 10 * time.Minute // Matches the GameLift queue's default timeout.
	defaultSqsVisibilityTimeout  = 10 * time.Second
	defaultSqsWaitTime           = 20 * time.Second
	defaultSqsMessageBatchSize   = 10
	defaultSqsWorkers            = 1
	defaultIndexMaxEntries       = 1_000_000

	// reservationTimeout is how long a reserved seat counts towards a game session's player count until the user
	// connects to it. It matches GameLift's fixed 60s timeout for a reserved player session to be accepted, so it
	// isn't configurable.
	reservationTimeout = time.Minute
)

// AwsConfig holds the AWS settings required by the GameLift fleet manager.
type AwsConfig struct {
	Key                   string // AWS Key
	Secret                string // AWS Secret
	Region                string // AWS Fleet Region
	FleetAlias            string // AWS GameLift Fleet Alias
	PlacementQueueName    string // AWS GameLift Placement Queue Name
	PlacementEventsSqsUrl string // AWS GameLift Placement Events SQS Queue URL
}

type config struct {
	aws                   AwsConfig
	nakamaNode            string
	gameLiftPollingPeriod time.Duration
	httpRequestTimeout    time.Duration
	httpDialerTimeout     time.Duration
	placementTimeout      time.Duration
	reservationTimeout    time.Duration
	heartbeatTimeout      time.Duration
	sqsVisibilityTimeout  time.Duration
	sqsWaitTime           time.Duration
	sqsMessageBatchSize   int
	sqsWorkers            int
	indexMaxEntries       int
}

func newConfig(awsCfg AwsConfig, nakamaNode string, opts ...Option) *config {
	cfg := &config{
		aws:                   awsCfg,
		nakamaNode:            nakamaNode,
		gameLiftPollingPeriod: defaultGameLiftPollingPeriod,
		httpRequestTimeout:    defaultHttpRequestTimeout,
		httpDialerTimeout:     defaultHttpDialerTimeout,
		placementTimeout:      defaultPlacementTimeout,
		reservationTimeout:    reservationTimeout,
		sqsVisibilityTimeout:  defaultSqsVisibilityTimeout,
		sqsWaitTime:           defaultSqsWaitTime,
		sqsMessageBatchSize:   defaultSqsMessageBatchSize,
		sqsWorkers:            defaultSqsWorkers,
		indexMaxEntries:       defaultIndexMaxEntries,
	}
	for _, opt := range opts {
		opt(cfg)
	}

	return cfg
}

// Option overrides a default setting of the GameLift fleet manager.
type Option func(*config)

// WithGameLiftPollingPeriod sets how often GameLift is polled to remove terminated game sessions and stale placements
// from storage. Defaults to 15 minutes.
func WithGameLiftPollingPeriod(d time.Duration) Option {
	return func(c *config) { c.gameLiftPollingPeriod = d }
}

// WithHttpRequestTimeout sets the AWS HTTP client request timeout. It must be greater than the SQS wait time. Defaults
// to 30 seconds.
func WithHttpRequestTimeout(d time.Duration) Option {
	return func(c *config) { c.httpRequestTimeout = d }
}

// WithHttpDialerTimeout sets the AWS HTTP client dialing timeout. Defaults to 15 seconds.
func WithHttpDialerTimeout(d time.Duration) Option {
	return func(c *config) { c.httpDialerTimeout = d }
}

// WithPlacementTimeout sets the max time a placement can stay unresolved before its storage entry is considered
// stale. It should be greater than or equal to the GameLift queue timeout. Defaults to 10 minutes.
func WithPlacementTimeout(d time.Duration) Option {
	return func(c *config) { c.placementTimeout = d }
}

// WithHeartbeatTimeout enables heartbeat tracking: game sessions that haven't invoked the update RPC within the
// timeout are excluded from List queries. Game sessions are expected to invoke it periodically, at an interval of at
// most a third of the timeout. Defaults to 0, which disables heartbeat tracking.
func WithHeartbeatTimeout(d time.Duration) Option {
	return func(c *config) { c.heartbeatTimeout = d }
}

// WithSqsVisibilityTimeout sets how long a received SQS message is hidden from other consumers before it's redelivered
// if it isn't processed. Defaults to 10 seconds.
func WithSqsVisibilityTimeout(d time.Duration) Option {
	return func(c *config) { c.sqsVisibilityTimeout = d }
}

// WithSqsWaitTime sets how long an SQS receive request waits for messages to arrive (0-20 seconds). Defaults to 20
// seconds.
func WithSqsWaitTime(d time.Duration) Option {
	return func(c *config) { c.sqsWaitTime = d }
}

// WithSqsMessageBatchSize sets the max number of SQS messages retrieved and processed per request (1-10). Defaults to
// 10.
func WithSqsMessageBatchSize(n int) Option {
	return func(c *config) { c.sqsMessageBatchSize = n }
}

// WithSqsWorkers sets the number of concurrent SQS consumers (>= 1). Defaults to 1.
func WithSqsWorkers(n int) Option {
	return func(c *config) { c.sqsWorkers = n }
}

// WithIndexMaxEntries sets the max entries of the Nakama storage index, which limits the number of game sessions
// that can be indexed. Defaults to 1,000,000.
func WithIndexMaxEntries(n int) Option {
	return func(c *config) { c.indexMaxEntries = n }
}

func (c *config) validate() error {
	errs := make([]error, 0)
	if c.nakamaNode == "" {
		errs = append(errs, errors.New("node must be set"))
	}
	if c.aws.Key == "" {
		errs = append(errs, errors.New("aws key must be set"))
	}
	if c.aws.Secret == "" {
		errs = append(errs, errors.New("aws secret must be set"))
	}
	if c.aws.Region == "" {
		errs = append(errs, errors.New("aws region must be set"))
	}
	if c.aws.FleetAlias == "" {
		errs = append(errs, errors.New("aws fleet alias must be set"))
	}
	if c.aws.PlacementQueueName == "" {
		errs = append(errs, errors.New("aws fleet placement queue name must be set"))
	}
	if c.aws.PlacementEventsSqsUrl == "" {
		errs = append(errs, errors.New("aws placement events sqs url must be set"))
	}
	if c.gameLiftPollingPeriod <= 0 {
		errs = append(errs, errors.New("aws gamelift polling period must be > 0"))
	}
	if c.httpDialerTimeout <= 0 {
		errs = append(errs, errors.New("http dialer timeout must be > 0"))
	}
	if c.indexMaxEntries <= 0 {
		errs = append(errs, errors.New("index max entries must be > 0"))
	}
	if c.sqsVisibilityTimeout <= 0 {
		errs = append(errs, errors.New("sqs visibility timeout must be > 0"))
	}
	if c.sqsWaitTime < 0 || c.sqsWaitTime > 20*time.Second {
		errs = append(errs, errors.New("sqs wait time must be between 0 and 20 seconds"))
	}
	if c.sqsWaitTime >= c.httpRequestTimeout {
		errs = append(errs, errors.New("sqs wait time must be < http request timeout"))
	}
	if c.sqsMessageBatchSize < 1 || c.sqsMessageBatchSize > 10 {
		errs = append(errs, errors.New("sqs message batch size must be between 1 and 10"))
	}
	if c.sqsWorkers < 1 {
		errs = append(errs, errors.New("sqs workers must be >= 1"))
	}
	if c.heartbeatTimeout < 0 {
		errs = append(errs, errors.New("heartbeat timeout must be >= 0"))
	}
	if c.placementTimeout <= 0 {
		errs = append(errs, errors.New("placement timeout must be > 0"))
	}

	return errors.Join(errs...)
}
