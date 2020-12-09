package structs

import (
	"encoding/json"
	"math"
	"time"

	"github.com/TheRockettek/Sandwich-Daemon/pkg/snowflake"
)

// Gateway represents a GET /gateway response.
type Gateway struct {
	URL string `json:"url" msgpack:"url"`
}

// GatewayBot represents a GET /gateway/bot response.
type GatewayBot struct {
	URL               string `json:"url" msgpack:"url"`
	Shards            int    `json:"shards" msgpack:"shards"`
	SessionStartLimit struct {
		Total          int `json:"total" msgpack:"total"`
		Remaining      int `json:"remaining" msgpack:"remaining"`
		ResetAfter     int `json:"reset_after" msgpack:"reset_after"`
		MaxConcurrency int `json:"max_concurrency" msgpack:"max_concurrency"`
	} `json:"session_start_limit" msgpack:"session_start_limit"`
}

// GatewayOp represents a packets operation.
type GatewayOp uint8

// Operation Codes for gateway messages.
const (
	GatewayOpDispatch GatewayOp = iota
	GatewayOpHeartbeat
	GatewayOpIdentify
	GatewayOpStatusUpdate
	GatewayOpVoiceStateUpdate
	_
	GatewayOpResume
	GatewayOpReconnect
	GatewayOpRequestGuildMembers
	GatewayOpInvalidSession
	GatewayOpHello
	GatewayOpHeartbeatACK
)

// The different gateway intents.
const (
	IntentGuilds uint = 1 << iota
	IntentGuildMembers
	IntentGuildBans
	IntentGuildEmojis
	IntentGuildIntegrations
	IntentGuildWebhooks
	IntentGuildInvites
	IntentGuildVoiceStates
	IntentGuildPresences
	IntentGuildMessages
	IntentGuildMessageReactions
	IntentGuildMessageTyping
	IntentDirectMessages
	IntentDirectMessageReactions
	IntentDirectMessageTyping
)

// The gateway's close codes.
const (
	CloseUnknownError = 4000 + iota
	CloseUnknownOpCode
	CloseDecodeError
	CloseNotAuthenticated
	CloseAuthenticationFailed
	CloseAlreadyAuthenticated
	_
	CloseInvalidSeq
	CloseRateLimited
	CloseSessionTimeout
	CloseInvalidShard
	CloseShardingRequired
	CloseInvalidAPIVersion
	CloseInvalidIntents
	CloseDisallowedIntents
)

// StateResult represents the data a state handler would return which would be converted to
// a sandwich payload
type StateResult struct {
	Data  interface{}
	Extra map[string]interface{}
}

// SandwichPayload represents the data that is sent to consumers
type SandwichPayload struct {
	ReceivedPayload

	Data  interface{}            `json:"d,omitempty" msgpack:"d,omitempty"`
	Extra map[string]interface{} `json:"e,omitempty" msgpack:"e,omitempty"`

	Metadata SandwichMetadata `json:"_sandwich" msgpack:"_sandwich"`
	Trace    map[string]int   `json:"trace" msgpack:"trace"`
}

// SandwichMetadata represents the identification information that consumers will use
type SandwichMetadata struct {
	Identifier string `json:"identifier" msgpack:"identifier"`
	Shard      [3]int `json:"s" msgpack:"s"` // ShardGroup ID, Shard ID, Shard Count
}

// ReceivedPayload is the base of a JSON packet received from discord.
type ReceivedPayload struct {
	Op       GatewayOp       `json:"op" msgpack:"op"`
	Data     json.RawMessage `json:"d,omitempty" msgpack:"d,omitempty"`
	Sequence int64           `json:"s,omitempty" msgpack:"s,omitempty"`
	Type     string          `json:"t,omitempty" msgpack:"t,omitempty"`

	// Used for trace tracking
	TraceTime time.Time      `json:"-"`
	Trace     map[string]int `json:"-"`
}

// ReceivedPayload adds a trace entry and overwrites the current trace time.
func (rp *ReceivedPayload) AddTrace(name string, now time.Time) {
	rp.Trace[name] = int(math.Ceil(float64(now.Sub(rp.TraceTime).Milliseconds())))
	rp.TraceTime = now
}

// SentPayload is the base of a JSON packet we sent to discord.
type SentPayload struct {
	Op   int         `json:"op" msgpack:"op"`
	Data interface{} `json:"d" msgpack:"d"`
}

// Identify represents an identify packet.
type Identify struct {
	Intents            int                 `json:"intents,omitempty" msgpack:"intents,omitempty"`
	LargeThreshold     int                 `json:"large_threshold,omitempty" msgpack:"large_threshold,omitempty"`
	Shard              [2]int              `json:"shard,omitempty" msgpack:"shard,omitempty"`
	Token              string              `json:"token" msgpack:"token"`
	Properties         *IdentifyProperties `json:"properties" msgpack:"properties"`
	Presence           *UpdateStatus       `json:"presence,omitempty" msgpack:"presence,omitempty"`
	Compress           bool                `json:"compress,omitempty" msgpack:"compress,omitempty"`
	GuildSubscriptions bool                `json:"guild_subscriptions,omitempty" msgpack:"guild_subscriptions,omitempty"`
}

// IdentifyProperties is the properties sent in the identify packet.
type IdentifyProperties struct {
	OS      string `json:"$os" msgpack:"$os"`
	Browser string `json:"$browser" msgpack:"$browser"`
	Device  string `json:"$device" msgpack:"$device"`
}

// RequestGuildMembers represents a request guild members packet.
type RequestGuildMembers struct {
	GuildID []int64 `json:"guild_id" msgpack:"guild_id"`
	Query   string  `json:"query" msgpack:"query"`
	Limit   int     `json:"limit" msgpack:"limit"`
}

// UpdateVoiceState represents an update voice state packet.
type UpdateVoiceState struct {
	GuildID   snowflake.ID `json:"guild_id" msgpack:"guild_id"`
	ChannelID snowflake.ID `json:"channel_id" msgpack:"channel_id"`
	SelfMute  bool         `json:"self_mute" msgpack:"self_mute"`
	SelfDeaf  bool         `json:"self_deaf" msgpack:"self_deaf"`
}

// Available statuses.
const (
	StatusOnline    = "online"
	StatusDND       = "dnd"
	StatusIdle      = "idle"
	StatusInvisible = "invisible"
	StatusOffline   = "offline"
)

// UpdateStatus represents an update status packet.
type UpdateStatus struct {
	Since  int       `json:"since,omitempty" msgpack:"since,omitempty"`
	Game   *Activity `json:"game,omitempty" msgpack:"game,omitempty"`
	Status string    `json:"status" msgpack:"status"`
	AFK    bool      `json:"afk" msgpack:"afk"`
}

// Hello represents a hello packet.
type Hello struct {
	HeartbeatInterval time.Duration `json:"heartbeat_interval" msgpack:"heartbeat_interval"`
}

// Ready represents a ready packet.
type Ready struct {
	Version   int      `json:"v" msgpack:"v"`
	User      *User    `json:"user" msgpack:"user"`
	Guilds    []*Guild `json:"guilds" msgpack:"guilds"`
	SessionID string   `json:"session_id" msgpack:"session_id"`
}

// Resume represents a resume packet.
type Resume struct {
	Token     string `json:"token" msgpack:"token"`
	SessionID string `json:"session_id" msgpack:"session_id"`
	Sequence  int64  `json:"seq" msgpack:"seq"`
}
