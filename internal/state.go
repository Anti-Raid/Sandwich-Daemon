package internal

import (
	"context"
	"time"

	"github.com/WelcomerTeam/Sandwich-Daemon/discord"
	csmap "github.com/mhmtszr/concurrent-swiss-map"
)

type StateCtx struct {
	context context.Context
	*Shard
	CacheUsers   bool
	CacheMembers bool
	Stateless    bool
	StoreMutuals bool
}

func NewFakeCtx(mg *Manager) StateCtx {
	return StateCtx{
		CacheUsers:   true,
		CacheMembers: true,
		StoreMutuals: true,
		Shard: &Shard{
			ctx:     mg.ctx,
			Manager: mg,
		},
	}
}

// SandwichState stores the collective state of all ShardGroups
// across all Managers.
type SandwichState struct {
	Guilds *csmap.CsMap[discord.Snowflake, discord.Guild]

	GuildMembers *csmap.CsMap[discord.Snowflake, StateGuildMembers]

	GuildChannels *csmap.CsMap[discord.Snowflake, StateGuildChannels]

	GuildRoles *csmap.CsMap[discord.Snowflake, StateGuildRoles]

	GuildEmojis *csmap.CsMap[discord.Snowflake, []discord.Emoji]

	Users *csmap.CsMap[discord.Snowflake, StateUser]

	DmChannels *csmap.CsMap[discord.Snowflake, StateDMChannel]

	Mutuals *csmap.CsMap[discord.Snowflake, StateMutualGuilds]

	GuildVoiceStates *csmap.CsMap[discord.Snowflake, StateGuildVoiceStates]
}

func NewSandwichState() *SandwichState {
	state := &SandwichState{
		Guilds: csmap.Create(
			csmap.WithSize[discord.Snowflake, discord.Guild](0),
		),

		GuildMembers: csmap.Create(
			csmap.WithSize[discord.Snowflake, StateGuildMembers](0),
		),

		GuildChannels: csmap.Create(
			csmap.WithSize[discord.Snowflake, StateGuildChannels](50),
		),

		GuildRoles: csmap.Create(
			csmap.WithSize[discord.Snowflake, StateGuildRoles](50),
		),

		GuildEmojis: csmap.Create(
			csmap.WithSize[discord.Snowflake, []discord.Emoji](50),
		),

		Users: csmap.Create(
			csmap.WithSize[discord.Snowflake, StateUser](50),
		),

		DmChannels: csmap.Create(
			csmap.WithSize[discord.Snowflake, StateDMChannel](50),
		),

		Mutuals: csmap.Create(
			csmap.WithSize[discord.Snowflake, StateMutualGuilds](50),
		),

		GuildVoiceStates: csmap.Create(
			csmap.WithSize[discord.Snowflake, StateGuildVoiceStates](50),
		),
	}

	return state
}

// GetGuild returns the guild with the same ID from the cache.
// Returns a boolean to signify a match or not.
func (ss *SandwichState) GetGuild(guildID discord.Snowflake) (guild discord.Guild, ok bool) {
	guild, ok = ss.Guilds.Load(guildID)

	if !ok {
		return
	}

	// Get list of roles
	roles, ok := ss.GetAllGuildRoles(guildID)

	if !ok {
		return
	}

	guild.Roles = roles

	// Get list of channels
	guildChannels, ok := ss.GetAllGuildChannels(guildID)

	if !ok {
		return
	}

	guild.Channels = guildChannels

	// Get list of voice states
	voiceStates, ok := ss.GuildVoiceStates.Load(guildID)

	if !ok {
		return
	}

	// Pre-allocate the list
	guild.VoiceStates = make([]discord.VoiceState, 0, voiceStates.VoiceStates.Count())

	voiceStates.VoiceStates.Range(func(_ discord.Snowflake, voiceState discord.VoiceState) bool {
		guild.VoiceStates = append(guild.VoiceStates, voiceState)
		return false
	})

	// Get list of emojis
	emojis, ok := ss.GetAllGuildEmojis(guildID)

	if !ok {
		return
	}

	guild.Emojis = emojis

	// Get list of members
	members, ok := ss.GetAllGuildMembers(guildID)

	if !ok {
		return
	}

	// Fix AFK channel
	if guild.AFKChannelID == nil {
		guild.AFKChannelID = &guild.ID
	}

	guild.Members = members
	ok = true

	return
}

// SetGuild creates or updates a guild entry in the cache.
//
// NOT fake-ctx-safe UNLESS
func (ss *SandwichState) SetGuild(ctx StateCtx, guild discord.Guild) {
	ctx.ShardGroup.Guilds.Store(guild.ID, struct{}{})
	ss.Guilds.Store(guild.ID, guild)
	ctx.Guilds.Store(guild.ID, struct{}{})

	// Safety: there is guaranteed to be at least one role
	for _, role := range guild.Roles {
		ss.SetGuildRole(guild.ID, role)
	}

	// Set channel default state
	if len(guild.Channels) == 0 {
		_, ok := ss.GuildChannels.Load(guild.ID)

		if !ok {
			ss.GuildChannels.SetIfAbsent(guild.ID, StateGuildChannels{
				Channels: csmap.Create(
					csmap.WithSize[discord.Snowflake, discord.Channel](0),
				),
			})
		}
	}

	for _, channel := range guild.Channels {
		ss.SetGuildChannel(ctx, &guild.ID, channel)
	}

	ss.SetGuildEmojis(ctx, guild.ID, guild.Emojis)

	for _, member := range guild.Members {
		ss.SetGuildMember(ctx, guild.ID, member)
	}

	// Set voice state default state
	if len(guild.VoiceStates) == 0 {
		_, ok := ss.GuildVoiceStates.Load(guild.ID)

		if !ok {
			ss.GuildVoiceStates.SetIfAbsent(guild.ID, StateGuildVoiceStates{

				VoiceStates: csmap.Create(
					csmap.WithSize[discord.Snowflake, discord.VoiceState](0),
				),
			})
		}
	}

	for _, voiceState := range guild.VoiceStates {
		voiceState.GuildID = &guild.ID
		ss.UpdateVoiceState(ctx, voiceState)
	}

	// Clear out some data that we don't need to cache in guild
	guild.Roles = nil
	guild.Channels = nil
	guild.VoiceStates = nil
	guild.Members = nil // No need to duplicate this data.
	guild.Emojis = nil  // No need to duplicate this data.
}

// RemoveGuild removes a guild from the cache.
//
// NOT fake-ctx-safe
func (ss *SandwichState) RemoveGuild(ctx StateCtx, guildID discord.Snowflake) {
	ss.Guilds.Delete(guildID)

	if !ctx.Stateless {
		ctx.ShardGroup.Guilds.Delete(guildID)
	}

	ss.RemoveAllGuildRoles(guildID)
	ss.RemoveAllGuildChannels(guildID)
	ss.RemoveAllGuildEmojis(guildID)
	ss.RemoveAllGuildMembers(guildID)
}

// GetGuildMember returns the guildMember with the same ID from the cache. Populated user field from cache.
// Returns a boolean to signify a match or not.
func (ss *SandwichState) GetGuildMember(guildID discord.Snowflake, guildMemberID discord.Snowflake) (guildMember discord.GuildMember, ok bool) {
	guildMembers, ok := ss.GuildMembers.Load(guildID)

	if !ok {
		return
	}

	guildMember, ok = guildMembers.Members.Load(guildMemberID)

	if !ok {
		return
	}

	// FIX: Ensure that joined_at is set correctly, it tends to get corrupted for some reason
	//
	// This is common enough to not warrning a log message for it.
	if guildMember.JoinedAt != "" {
		if _, err := time.Parse(time.RFC3339, string(guildMember.JoinedAt)); err != nil {
			guildMember.JoinedAt = ""
		}
	}

	user, ok := ss.GetUser(guildMember.User.ID)
	if ok {
		guildMember.User = &user
	}

	return
}

// SetGuildMember creates or updates a guildMember entry in the cache. Adds user in guildMember object to cache.
//
// fake-ctx-safe
func (ss *SandwichState) SetGuildMember(ctx StateCtx, guildID discord.Snowflake, guildMember discord.GuildMember) {
	// We will always cache the guild member of the bot that receives this event.
	if !ctx.CacheMembers && guildMember.User.ID != ctx.Manager.User.ID {
		return
	}

	guildMembers, ok := ss.GuildMembers.Load(guildID)

	if !ok {
		// Only set if its not already set.
		ss.GuildMembers.SetIfAbsent(guildID, StateGuildMembers{
			Members: csmap.Create(
				csmap.WithSize[discord.Snowflake, discord.GuildMember](100),
			),
		})

		guildMembers, _ = ss.GuildMembers.Load(guildID)
	}

	guildMembers.Members.Store(guildMember.User.ID, guildMember)

	if guildMember.User != nil {
		ss.SetUser(ctx, *guildMember.User)
	}
}

// RemoveGuildMember removes a guildMember from the cache.
func (ss *SandwichState) RemoveGuildMember(guildID discord.Snowflake, guildMemberID discord.Snowflake) {
	guildMembers, ok := ss.GuildMembers.Load(guildID)

	if !ok {
		return
	}

	guildMembers.Members.Delete(guildMemberID)
}

// GetAllGuildMembers returns all guildMembers of a specific guild from the cache.
func (ss *SandwichState) GetAllGuildMembers(guildID discord.Snowflake) (guildMembersList []discord.GuildMember, ok bool) {
	guildMembers, ok := ss.GuildMembers.Load(guildID)

	if !ok {
		return
	}

	// Pre-allocate the list
	guildMembersList = make([]discord.GuildMember, 0, guildMembers.Members.Count())

	guildMembers.Members.Range(func(_ discord.Snowflake, guildMember discord.GuildMember) bool {
		guildMembersList = append(guildMembersList, guildMember)
		return false
	})

	return
}

// RemoveAllGuildMembers removes all guildMembers of a specific guild from the cache.
func (ss *SandwichState) RemoveAllGuildMembers(guildID discord.Snowflake) {
	ss.GuildMembers.Delete(guildID)
}

// GetGuildRole returns the role with the same ID from the cache.
// Returns a boolean to signify a match or not.
func (ss *SandwichState) GetGuildRole(guildID discord.Snowflake, roleID discord.Snowflake) (role discord.Role, ok bool) {
	stateGuildRoles, ok := ss.GuildRoles.Load(roleID)

	if !ok {
		return
	}

	role, ok = stateGuildRoles.Roles.Load(roleID)

	if !ok {
		return
	}

	return
}

// SetGuildRole creates or updates a role entry in the cache.
func (ss *SandwichState) SetGuildRole(guildID discord.Snowflake, role discord.Role) {
	guildRoles, ok := ss.GuildRoles.Load(guildID)

	if !ok {
		ss.GuildRoles.SetIfAbsent(guildID, StateGuildRoles{
			Roles: csmap.Create(
				csmap.WithSize[discord.Snowflake, discord.Role](50),
			),
		})

		guildRoles, _ = ss.GuildRoles.Load(guildID)
	}

	guildRoles.Roles.Store(role.ID, role)
}

// RemoveGuildRole removes a role from the cache.
func (ss *SandwichState) RemoveGuildRole(guildID discord.Snowflake, roleID discord.Snowflake) {
	guildRoles, ok := ss.GuildRoles.Load(guildID)

	if !ok {
		return
	}

	guildRoles.Roles.Delete(roleID)
}

// GetAllGuildRoles returns all guildRoles of a specific guild from the cache.
func (ss *SandwichState) GetAllGuildRoles(guildID discord.Snowflake) (guildRolesList []discord.Role, ok bool) {
	guildRoles, ok := ss.GuildRoles.Load(guildID)

	if !ok {
		return
	}

	// Pre-allocate the list
	guildRolesList = make([]discord.Role, 0, guildRoles.Roles.Count())

	guildRoles.Roles.Range(func(id discord.Snowflake, role discord.Role) bool {
		if role.ID == 0 {
			role.ID = id
		}

		guildRolesList = append(guildRolesList, role)
		return false
	})

	return
}

// RemoveGuildRoles removes all guild roles of a specifi guild from the cache.
func (ss *SandwichState) RemoveAllGuildRoles(guildID discord.Snowflake) {
	ss.GuildRoles.Delete(guildID)
}

//
// Emoji Operations
//

// GetGuildEmoji returns the emoji with the same ID from the cache. Populated user field from cache.
// Returns a boolean to signify a match or not.
func (ss *SandwichState) GetGuildEmoji(guildID discord.Snowflake, emojiID discord.Snowflake) (guildEmoji discord.Emoji, ok bool) {
	guildEmojis, ok := ss.GuildEmojis.Load(guildID)

	if !ok {
		return
	}

	for _, emoji := range guildEmojis {
		if emoji.ID == emojiID {
			guildEmoji = emoji
			ok = true
			break
		}
	}

	if guildEmoji.User != nil {
		user, ok := ss.GetUser(guildEmoji.User.ID)
		if ok {
			guildEmoji.User = &user
		}
	}

	return
}

// SetGuildEmoji sets the list of emoji entries in the cache. Adds user in user object to cache.
//
// fake-ctx-safe
func (ss *SandwichState) SetGuildEmojis(ctx StateCtx, guildID discord.Snowflake, emojis []discord.Emoji) {
	ss.GuildEmojis.Store(guildID, emojis)

	for _, emoji := range emojis {
		if emoji.User != nil {
			ss.SetUser(ctx, *emoji.User)
		}
	}
}

// GetAllGuildEmojis returns all guildEmojis on a specific guild from the cache.
func (ss *SandwichState) GetAllGuildEmojis(guildID discord.Snowflake) (guildEmojisList []discord.Emoji, ok bool) {
	return ss.GuildEmojis.Load(guildID)
}

// RemoveGuildEmojis removes all guildEmojis of a specific guild from the cache.
func (ss *SandwichState) RemoveAllGuildEmojis(guildID discord.Snowflake) {
	ss.GuildEmojis.Delete(guildID)
}

//
// User Operations
//

// UserFromState converts the structs.StateUser into a discord.User, for use within the application.
func (ss *SandwichState) UserFromState(userState StateUser) discord.User {
	return userState.User
}

// UserFromState converts from discord.User to structs.StateUser, for storing in cache.
func (ss *SandwichState) UserToState(user discord.User) StateUser {
	return StateUser{
		User:        user,
		LastUpdated: time.Now(),
	}
}

// GetUser returns the user with the same ID from the cache.
// Returns a boolean to signify a match or not.
func (ss *SandwichState) GetUser(userID discord.Snowflake) (user discord.User, ok bool) {
	stateUser, ok := ss.Users.Load(userID)

	if !ok {
		return
	}

	user = ss.UserFromState(stateUser)

	return
}

// SetUser creates or updates a user entry in the cache.
//
// fake-ctx-safe
func (ss *SandwichState) SetUser(ctx StateCtx, user discord.User) {
	// We will always cache the user of the bot that receives this event.
	if !ctx.CacheUsers && user.ID != ctx.Manager.User.ID {
		return
	}

	ss.Users.Store(user.ID, ss.UserToState(user))
}

// RemoveUser removes a user from the cache.
func (ss *SandwichState) RemoveUser(userID discord.Snowflake) {
	ss.Users.Delete(userID)
}

//
// Channel Operations
//

// GetGuildChannel returns the channel with the same ID from the cache.
// Returns a boolean to signify a match or not.
func (ss *SandwichState) GetGuildChannel(guildIDPtr *discord.Snowflake, channelID discord.Snowflake) (guildChannel discord.Channel, ok bool) {
	var guildID discord.Snowflake

	if guildIDPtr != nil {
		guildID = *guildIDPtr
	} else {
		guildID = discord.Snowflake(0)
	}

	stateChannels, ok := ss.GuildChannels.Load(guildID)

	if !ok {
		return guildChannel, false
	}

	guildChannel, ok = stateChannels.Channels.Load(channelID)
	if !ok {
		return guildChannel, false
	}

	newRecipients := make([]discord.User, 0, len(guildChannel.Recipients))

	for _, recipient := range guildChannel.Recipients {
		recipientUser, ok := ss.GetUser(recipient.ID)
		if ok {
			recipient = recipientUser
		}

		newRecipients = append(newRecipients, recipient)
	}

	guildChannel.Recipients = newRecipients

	return guildChannel, ok
}

// SetGuildChannel creates or updates a channel entry in the cache.
//
// fake-ctx-safe
func (ss *SandwichState) SetGuildChannel(ctx StateCtx, guildIDPtr *discord.Snowflake, channel discord.Channel) {
	var guildID discord.Snowflake

	if guildIDPtr != nil {
		guildID = *guildIDPtr
	} else {
		guildID = discord.Snowflake(0)
	}

	// Ensure channel has guild id set
	channel.GuildID = &guildID

	guildChannels, ok := ss.GuildChannels.Load(guildID)

	if !ok {
		ss.GuildChannels.SetIfAbsent(guildID, StateGuildChannels{
			Channels: csmap.Create(
				csmap.WithSize[discord.Snowflake, discord.Channel](50),
			),
		})

		guildChannels, _ = ss.GuildChannels.Load(guildID)
	}

	guildChannels.Channels.Store(channel.ID, channel)

	for _, recipient := range channel.Recipients {
		recipient := recipient
		ss.SetUser(ctx, recipient)
	}
}

// RemoveGuildChannel removes a channel from the cache.
func (ss *SandwichState) RemoveGuildChannel(guildIDPtr *discord.Snowflake, channelID discord.Snowflake) {
	var guildID discord.Snowflake

	if guildIDPtr != nil {
		guildID = *guildIDPtr
	} else {
		guildID = discord.Snowflake(0)
	}

	guildChannels, ok := ss.GuildChannels.Load(guildID)

	if !ok {
		return
	}

	guildChannels.Channels.Delete(channelID)
}

// GetChannel returns a channel from its ID searching both DMs and guild channels.
//
// Note that guildIdHint must be provided if the channel is not a DM channel otherwise no result will be returned.
func (ss *SandwichState) GetChannel(guildIdHint *discord.Snowflake, channelID discord.Snowflake) (channel *discord.Channel, ok bool) {
	dmChannel, ok := ss.GetDMChannel(channelID)

	if ok {
		return &dmChannel, true
	}

	if guildIdHint != nil {
		_channel, ok := ss.GetGuildChannel(guildIdHint, channelID)
		return &_channel, ok
	} else {
		return nil, false
	}
}

// SetChannelDynamic sets a channel based on its type
func (ss *SandwichState) SetChannelDynamic(ctx StateCtx, channel discord.Channel) {
	if channel.GuildID != nil {
		ss.SetGuildChannel(ctx, channel.GuildID, channel)
	} else if channel.Type == discord.ChannelTypeDM || channel.Type == discord.ChannelTypeGroupDM {
		ss.AddDMChannel(channel.ID, channel)
	}
}

// GetAllGuildChannels returns all guildChannels of a specific guild from the cache.
func (ss *SandwichState) GetAllGuildChannels(guildID discord.Snowflake) (guildChannelsList []discord.Channel, ok bool) {
	guildChannels, ok := ss.GuildChannels.Load(guildID)

	if !ok {
		return
	}

	// Pre-allocate the list
	guildChannelsList = make([]discord.Channel, 0, guildChannels.Channels.Count())

	guildChannels.Channels.Range(func(_ discord.Snowflake, guildChannel discord.Channel) bool {
		guildChannelsList = append(guildChannelsList, guildChannel)
		return false
	})

	return
}

// RemoveAllGuildChannels removes all guildChannels of a specific guild from the cache.
func (ss *SandwichState) RemoveAllGuildChannels(guildID discord.Snowflake) {
	ss.GuildChannels.Delete(guildID)
}

// GetDMChannel returns the DM channel of a user.
func (ss *SandwichState) GetDMChannel(userID discord.Snowflake) (channel discord.Channel, ok bool) {
	dmChannel, ok := ss.DmChannels.Load(userID)

	if !ok || int64(dmChannel.ExpiresAt) < time.Now().Unix() {
		ok = false

		return
	}

	channel = dmChannel.Channel
	dmChannel.ExpiresAt = discord.Int64(time.Now().Add(memberDMExpiration).Unix())

	ss.DmChannels.Store(userID, dmChannel)

	return
}

// AddDMChannel adds a DM channel to a user.
func (ss *SandwichState) AddDMChannel(userID discord.Snowflake, channel discord.Channel) {
	ss.DmChannels.Store(userID, StateDMChannel{
		Channel:   channel,
		ExpiresAt: discord.Int64(time.Now().Add(memberDMExpiration).Unix()),
	})
}

// RemoveDMChannel removes a DM channel from a user.
func (ss *SandwichState) RemoveDMChannel(userID discord.Snowflake) {
	ss.DmChannels.Delete(userID)
}

// GetUserMutualGuilds returns a list of snowflakes of mutual guilds a member is seen on.
func (ss *SandwichState) GetUserMutualGuilds(userID discord.Snowflake) (guildIDs []discord.Snowflake, ok bool) {
	mutualGuilds, ok := ss.Mutuals.Load(userID)

	if !ok {
		return
	}

	// Pre-allocate the list
	guildIDs = make([]discord.Snowflake, 0, mutualGuilds.Guilds.Count())

	mutualGuilds.Guilds.Range(func(guildID discord.Snowflake, _ struct{}) bool {
		guildIDs = append(guildIDs, guildID)
		return false
	})

	return
}

// AddUserMutualGuild adds a mutual guild to a user.
//
// fake-ctx-safe
func (ss *SandwichState) AddUserMutualGuild(ctx StateCtx, userID discord.Snowflake, guildID discord.Snowflake) {
	if !ctx.StoreMutuals {
		return
	}

	mutualGuilds, ok := ss.Mutuals.Load(userID)

	if !ok {
		ss.Mutuals.SetIfAbsent(userID, StateMutualGuilds{
			Guilds: csmap.Create(
				csmap.WithSize[discord.Snowflake, struct{}](1),
			),
		})

		mutualGuilds, _ = ss.Mutuals.Load(userID)
	}

	mutualGuilds.Guilds.Store(guildID, struct{}{})
}

// RemoveUserMutualGuild removes a mutual guild from a user.
func (ss *SandwichState) RemoveUserMutualGuild(userID discord.Snowflake, guildID discord.Snowflake) {
	mutualGuilds, ok := ss.Mutuals.Load(userID)

	if !ok {
		return
	}

	mutualGuilds.Guilds.Delete(guildID)
}

//
// VoiceState Operations
//

// ParseVoiceState parses a voice state info populating it from cache
func (ss *SandwichState) ParseVoiceState(guildID discord.Snowflake, userID discord.Snowflake, voiceStateState discord.VoiceState) (voiceState discord.VoiceState) {
	if voiceStateState.Member == nil {
		gm, _ := ss.GetGuildMember(guildID, userID)

		voiceStateState.Member = &gm
	}

	voiceStateState.UserID = userID

	return voiceStateState
}

func (ss *SandwichState) GetVoiceState(guildID discord.Snowflake, userID discord.Snowflake) (voiceState discord.VoiceState, ok bool) {
	guildVoiceStates, ok := ss.GuildVoiceStates.Load(guildID)

	if !ok {
		return
	}

	stateVoiceState, ok := guildVoiceStates.VoiceStates.Load(userID)

	if !ok {
		return
	}

	voiceState = ss.ParseVoiceState(guildID, userID, stateVoiceState)

	return
}

// UpdateVoiceState updates the voice state of a user in a guild.
//
// fake-ctx-safe
func (ss *SandwichState) UpdateVoiceState(ctx StateCtx, voiceState discord.VoiceState) {
	if voiceState.GuildID == nil {
		return
	}

	guildVoiceStates, ok := ss.GuildVoiceStates.Load(*voiceState.GuildID)

	if !ok {
		ss.GuildVoiceStates.SetIfAbsent(*voiceState.GuildID, StateGuildVoiceStates{
			VoiceStates: csmap.Create(
				csmap.WithSize[discord.Snowflake, discord.VoiceState](50),
			),
		})

		guildVoiceStates, _ = ss.GuildVoiceStates.Load(*voiceState.GuildID)
	}

	beforeVoiceState, _ := ss.GetVoiceState(*voiceState.GuildID, voiceState.UserID)

	if voiceState.ChannelID == 0 {
		// Remove from voice states if leaving voice channel.
		guildVoiceStates.VoiceStates.Delete(voiceState.UserID)
	} else {
		guildVoiceStates.VoiceStates.Store(voiceState.UserID, ss.ParseVoiceState(*voiceState.GuildID, voiceState.UserID, voiceState))
	}

	if voiceState.Member != nil {
		ss.SetGuildMember(ctx, *voiceState.GuildID, *voiceState.Member)
	}

	// Update channel counts

	if !beforeVoiceState.ChannelID.IsNil() {
		voiceChannel, ok := ctx.Sandwich.State.GetGuildChannel(beforeVoiceState.GuildID, beforeVoiceState.ChannelID)
		if ok {
			voiceChannel.MemberCount = ss.CountMembersForVoiceChannel(*beforeVoiceState.GuildID, voiceChannel.ID)

			ctx.Sandwich.State.SetGuildChannel(ctx, beforeVoiceState.GuildID, voiceChannel)
		}
	}

	if !voiceState.ChannelID.IsNil() {
		voiceChannel, ok := ctx.Sandwich.State.GetGuildChannel(voiceState.GuildID, voiceState.ChannelID)
		if ok {
			voiceChannel.MemberCount = ss.CountMembersForVoiceChannel(*voiceState.GuildID, voiceChannel.ID)

			ctx.Sandwich.State.SetGuildChannel(ctx, voiceState.GuildID, voiceChannel)
		}
	}
}

func (ss *SandwichState) RemoveVoiceState(ctx StateCtx, guildID, userID discord.Snowflake) {
	// Check presence of an existing voice state.

	guildVoiceStates, ok := ss.GuildVoiceStates.Load(guildID)

	if !ok {
		return
	}

	stateVoiceState, ok := guildVoiceStates.VoiceStates.Load(userID)

	if !ok {
		return
	}

	// Remove voice state.
	guildVoiceStates.VoiceStates.Delete(userID)

	// Update channel counts.

	voiceChannel, ok := ss.GetGuildChannel(&guildID, stateVoiceState.ChannelID)
	if ok {
		voiceChannel.MemberCount = ss.CountMembersForVoiceChannel(guildID, voiceChannel.ID)

		ss.SetGuildChannel(ctx, &guildID, voiceChannel)
	}
}

func (ss *SandwichState) CountMembersForVoiceChannel(guildID discord.Snowflake, channelID discord.Snowflake) int32 {
	guildVoiceStates, ok := ss.GuildVoiceStates.Load(guildID)

	if !ok {
		return 0
	}

	var count int32

	guildVoiceStates.VoiceStates.Range(func(_ discord.Snowflake, voiceState discord.VoiceState) bool {
		if voiceState.ChannelID == channelID {
			count++
		}
		return false
	})

	return count
}

// Special state structs

type StateDMChannel struct {
	discord.Channel
	ExpiresAt discord.Int64 `json:"expires_at"`
}

type StateMutualGuilds struct {
	Guilds *csmap.CsMap[discord.Snowflake, struct{}] `json:"guilds"`
}

type StateGuildMembers struct {
	Members *csmap.CsMap[discord.Snowflake, discord.GuildMember] `json:"members"`
}

type StateGuildRoles struct {
	Roles *csmap.CsMap[discord.Snowflake, discord.Role] `json:"roles"`
}

type StateGuildChannels struct {
	Channels *csmap.CsMap[discord.Snowflake, discord.Channel] `json:"channels"`
}

type StateGuildVoiceStates struct {
	VoiceStates *csmap.CsMap[discord.Snowflake, discord.VoiceState] `json:"voice_states"`
}

type StateUser struct {
	LastUpdated time.Time `json:"__sandwich_last_updated,omitempty"`
	discord.User
}
