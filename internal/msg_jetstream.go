package internal

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/WelcomerTeam/Sandwich-Daemon/internal/structs"
	"github.com/WelcomerTeam/Sandwich-Daemon/sandwichjson"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func init() {
	MQClients = append(MQClients, "jetstream")
}

type JetStreamMQClient struct {
	JetStreamClient jetstream.JetStream `json:"-"`
	JetStreamStream jetstream.Stream    `json:"-"`

	channel  string
	isClosed bool
}

func (jetstreamMQ *JetStreamMQClient) String() string {
	return "jetstream"
}

func (jetstreamMQ *JetStreamMQClient) Channel() string {
	return jetstreamMQ.channel
}

func (jetstreamMQ *JetStreamMQClient) Connect(ctx context.Context, manager *Manager, clientName string, args map[string]interface{}) error {
	var ok bool

	var address string

	if address, ok = GetEntry(args, "Address").(string); !ok {
		return errors.New("jetstreamMQ connect: string type assertion failed for Address")
	}

	var channel string

	if channel, ok = GetEntry(args, "Channel").(string); !ok {
		return errors.New("jetstreamMQ connect: string type assertion failed for Channel")
	}

	jetstreamMQ.channel = channel

	nc, err := nats.Connect(address)
	if err != nil {
		return fmt.Errorf("jetstreamMQ connect nats: %w", err)
	}

	jetstreamMQ.JetStreamClient, err = jetstream.New(nc)
	if err != nil {
		return fmt.Errorf("jetstreamMQ new: %w", err)
	}

	retention := jetstream.WorkQueuePolicy

	if v := mustParseBool(os.Getenv("JETSTREAM_USE_INTEREST_POLICY")); v {
		retention = jetstream.InterestPolicy
	}

	jetstreamMQ.JetStreamStream, err = jetstreamMQ.JetStreamClient.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:              jetstreamMQ.channel,
		Subjects:          []string{jetstreamMQ.channel + ".*"},
		Retention:         retention,
		Discard:           jetstream.DiscardOld,
		MaxAge:            5 * time.Minute,
		Storage:           jetstream.MemoryStorage,
		MaxMsgsPerSubject: 1_000_000,
		MaxMsgSize:        math.MaxInt32,
		NoAck:             false,
	})
	if err != nil {
		return fmt.Errorf("jetstreamMQ create stream: %w", err)
	}

	jetstreamMQ.isClosed = false
	return nil
}

func mustParseBool(str string) bool {
	boolean, _ := strconv.ParseBool(str)

	return boolean
}

func (jetstreamMQ *JetStreamMQClient) Publish(ctx context.Context, packet *structs.SandwichPayload, channelName string) error {
	data, err := sandwichjson.Marshal(packet)

	if err != nil {
		return err
	}

	_, err = jetstreamMQ.JetStreamClient.Publish(
		ctx,
		jetstreamMQ.channel+"."+channelName,
		data,
	)

	return err
}

func (jetstreamMQ *JetStreamMQClient) IsClosed() bool {
	return jetstreamMQ.isClosed
}

func (jetstreamMQ *JetStreamMQClient) CloseShard(shardID int32, reason MQCloseShardReason) {
	// No-op for kafka
}

func (jetstreamMQ *JetStreamMQClient) Close() {
	jetstreamMQ.isClosed = true
}

func (jetstreamMQ *JetStreamMQClient) StopSession(sessionID string) {
	// No-op
}
