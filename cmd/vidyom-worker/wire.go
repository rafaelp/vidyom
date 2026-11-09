package main

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/rafaelp/vidyom/connectors/ai/bgem3"
	"github.com/rafaelp/vidyom/connectors/storage/gcs"
	"github.com/rafaelp/vidyom/connectors/storage/local"
	fasterwhisper "github.com/rafaelp/vidyom/connectors/transcription/faster-whisper"
	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/queue"
	ainodes "github.com/rafaelp/vidyom/nodes/ai"
	audionodes "github.com/rafaelp/vidyom/nodes/audio"
	storagenodes "github.com/rafaelp/vidyom/nodes/storage"
	videonodes "github.com/rafaelp/vidyom/nodes/video"
)

func init() {
	wireRegistries = registerWorker
	wireQueue = openQueue
}

// registerWorker adds what this binary can run. nodes/publishing links cgo through
// internal/publicexport and never registers here (I7). ainodes also registers its
// daemon-pool nodes, which a worker never leases.
func registerWorker(nodes node.Registrar, connectors connector.Registrar) error {
	storagenodes.Register(nodes)
	videonodes.Register(nodes)
	audionodes.Register(nodes)
	ainodes.Register(nodes)

	local.Register(connectors)
	gcs.Register(connectors)
	fasterwhisper.Register(connectors)
	bgem3.Register(connectors)
	return nil
}

func openQueue(_ context.Context, cfg config.Redis) (jobqueue.Queue, func(), error) {
	options, err := redis.ParseURL(cfg.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("parse the Redis URL: %w", err)
	}
	client := redis.NewClient(options)
	return queue.NewRedis(client), func() { _ = client.Close() }, nil
}
