package internal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/WelcomerTeam/Sandwich-Daemon/discord"
	sandwich_structs "github.com/WelcomerTeam/Sandwich-Daemon/internal/structs"
)

// OnReady handles the READY event.
// It will go and mark guilds as unavailable and go through
// any GUILD_CREATE events for the next few seconds.
func OnReady(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	defer ctx.OnDispatchEvent(msg.Type)

	var readyPayload discord.Ready

	err = ctx.decodeContent(msg, &readyPayload)
	if err != nil {
		return result, false, err
	}

	ctx.Logger.Info().Msg("Received READY payload")
	ctx.Shard.IsReady = true
	ctx.ResumeGatewayURL.Store(readyPayload.ResumeGatewayUrl)
	ctx.SessionID.Store(readyPayload.SessionID)

	ctx.ShardGroup.userMu.Lock()

	ctx.ShardGroup.User = &readyPayload.User
	ctx.Manager.UserID.Store(int64(readyPayload.User.ID))
	ctx.Manager.userMu.Lock()
	ctx.Manager.User = readyPayload.User
	ctx.Manager.userMu.Unlock()

	ctx.ShardGroup.userMu.Unlock()

	for _, guild := range readyPayload.Guilds {
		ctx.Lazy.Store(guild.ID, struct{}{})
		ctx.Guilds.Store(guild.ID, struct{}{})
		ctx.Shard.Guilds.Store(guild.ID, struct{}{})
		ctx.Shard.Lazy.Store(guild.ID, struct{}{})
	}

	guildCreateEvents := 0

	readyTimeout := time.NewTicker(ReadyTimeout)

ready:
	for {
		select {
		case <-readyTimeout.C:
			ctx.Logger.Info().Int("guilds", guildCreateEvents).Msg("Finished lazy loading guilds")
			break ready
		default:
		}

		msg, err := ctx.readMessage()
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				ctx.Logger.Error().Err(err).Msg("Encountered error during READY")
			}

			break ready
		} else {
			if msg.Type == discord.DiscordEventGuildCreate {
				guildCreateEvents++

				readyTimeout.Reset(ReadyTimeout)
			}

			err = ctx.OnDispatch(ctx.context, msg, trace)
			if err != nil && !errors.Is(err, ErrNoDispatchHandler) {
				ctx.Logger.Error().Err(err).Msg("Failed to dispatch event")
			}
		}
	}

	select {
	case ctx.ready <- void{}:
	default:
	}

	ctx.SetStatus(sandwich_structs.ShardStatusReady)

	ctx.Manager.configurationMu.RLock()
	chunkGuildOnStartup := ctx.Manager.Configuration.Bot.ChunkGuildsOnStartup
	ctx.Manager.configurationMu.RUnlock()

	if chunkGuildOnStartup {
		ctx.Shard.ChunkAllGuilds()
	}

	result.EventDispatchIdentifier = &sandwich_structs.EventDispatchIdentifier{}

	return result, false, nil
}

func OnResumed(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	defer ctx.OnDispatchEvent(msg.Type)

	ctx.Logger.Info().Msg("Received READY payload")

	select {
	case ctx.ready <- void{}:
	default:
	}

	ctx.Shard.IsReady = true

	ctx.SetStatus(sandwich_structs.ShardStatusReady)

	return EventDispatch{
		Data:                    msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{},
	}, true, nil
}

func OnGuildCreate(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var guildCreatePayload discord.GuildCreate

	err = ctx.decodeContent(msg, &guildCreatePayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.OnGuildDispatchEvent(msg.Type, guildCreatePayload.ID)

	ctx.Sandwich.State.SetGuild(ctx, discord.Guild(guildCreatePayload))

	lazy, _ := ctx.Lazy.Load(guildCreatePayload.ID)
	ctx.Lazy.Delete(guildCreatePayload.ID)

	unavailable, _ := ctx.Unavailable.Load(guildCreatePayload.ID)
	ctx.Unavailable.Delete(guildCreatePayload.ID)

	extra, err := makeExtra(map[string]interface{}{
		"lazy":        lazy,
		"unavailable": unavailable,
	})
	if err != nil {
		return result, ok, fmt.Errorf("failed to marshal extras: %w", err)
	}

	return EventDispatch{
		Data:  msg.Data,
		Extra: extra,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: &guildCreatePayload.ID,
		},
	}, true, nil
}

func OnGuildMembersChunk(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	defer ctx.OnDispatchEvent(msg.Type)

	var guildMembersChunkPayload discord.GuildMembersChunk

	err = ctx.decodeContent(msg, &guildMembersChunkPayload)
	if err != nil {
		return result, false, err
	}

	// Force caching of users and members.
	ctx.CacheUsers = true
	ctx.CacheMembers = true

	for _, member := range guildMembersChunkPayload.Members {
		ctx.Sandwich.State.SetGuildMember(ctx, guildMembersChunkPayload.GuildID, member)
	}

	ctx.Logger.Debug().
		Int("memberCount", len(guildMembersChunkPayload.Members)).
		Int32("chunkIndex", guildMembersChunkPayload.ChunkIndex).
		Int32("chunkCount", guildMembersChunkPayload.ChunkCount).
		Int64("guildID", int64(guildMembersChunkPayload.GuildID)).
		Msg("Chunked guild members")

	var guildChunk GuildChunks
	guildChunk, ok = ctx.Sandwich.guildChunks.Load(guildMembersChunkPayload.GuildID)

	if !ok {
		// We don't need to care further
		return EventDispatch{
			Data: msg.Data,
			EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
				GuildID: &guildMembersChunkPayload.GuildID,
			},
		}, true, nil
	}

	// if guildChunk.Complete.Load() {
	// 	ctx.Logger.Warn().
	// 		Int64("guildID", int64(guildMembersChunkPayload.GuildID)).
	// 		Msg("GuildChunks entry is marked as complete, but we received a guild member chunk")
	// }

	if guildChunk.ChunkingChannel != nil {
		select {
		case guildChunk.ChunkingChannel <- &guildMembersChunkPayload:
		default:
		}
	} else {
		// Partial
		totalRecv := guildChunk.ChunkCount.Inc()

		if totalRecv >= guildMembersChunkPayload.ChunkCount {
			ctx.Logger.Debug().
				Int32("chunkIndex", guildMembersChunkPayload.ChunkIndex).
				Int32("chunkCount", guildMembersChunkPayload.ChunkCount).
				Int64("guildID", int64(guildMembersChunkPayload.GuildID)).
				Msg("Finished chunked guild members via partial mode")
			guildChunk.Complete.Store(true)
			guildChunk.CompletedAt.Store(time.Now())
		}
	}

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: &guildMembersChunkPayload.GuildID,
		},
	}, true, nil
}

func OnChannelCreate(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var channelCreatePayload discord.ChannelCreate

	err = ctx.decodeContent(msg, &channelCreatePayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.SafeOnGuildDispatchEvent(msg.Type, channelCreatePayload.GuildID)

	if channelCreatePayload.GuildID != nil && !channelCreatePayload.GuildID.IsNil() {
		ctx.Sandwich.State.SetGuildChannel(ctx, *channelCreatePayload.GuildID, discord.Channel(channelCreatePayload))
	} else if channelCreatePayload.Type == discord.ChannelTypeDM || channelCreatePayload.Type == discord.ChannelTypeGroupDM {
		if len(channelCreatePayload.Recipients) > 0 {
			ctx.Sandwich.State.AddDMChannel(channelCreatePayload.Recipients[0].ID, discord.Channel(channelCreatePayload))
		}
	}

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: channelCreatePayload.GuildID,
		},
	}, true, nil
}

func OnChannelUpdate(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var channelUpdatePayload discord.ChannelUpdate

	err = ctx.decodeContent(msg, &channelUpdatePayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.SafeOnGuildDispatchEvent(msg.Type, channelUpdatePayload.GuildID)

	var beforeChannel *discord.Channel

	if channelUpdatePayload.GuildID != nil && !channelUpdatePayload.GuildID.IsNil() {
		beforeChannelV, _ := ctx.Sandwich.State.GetGuildChannel(*channelUpdatePayload.GuildID, channelUpdatePayload.ID)
		beforeChannel = &beforeChannelV

		ctx.Sandwich.State.SetGuildChannel(ctx, *channelUpdatePayload.GuildID, discord.Channel(channelUpdatePayload))
	} else if channelUpdatePayload.Type == discord.ChannelTypeDM || channelUpdatePayload.Type == discord.ChannelTypeGroupDM {
		beforeChannelV, _ := ctx.Sandwich.State.GetDMChannel(channelUpdatePayload.ID)
		beforeChannel = &beforeChannelV

		ctx.Sandwich.State.UpdateDMChannelByChannelID(channelUpdatePayload.ID, func(channel StateDMChannel) StateDMChannel {
			channel.Channel = discord.Channel(channelUpdatePayload)
			return channel
		})
	}

	extra, err := makeExtra(map[string]interface{}{
		"before": beforeChannel,
	})
	if err != nil {
		return result, ok, fmt.Errorf("failed to marshal extras: %w", err)
	}

	return EventDispatch{
		Data:  msg.Data,
		Extra: extra,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: channelUpdatePayload.GuildID,
		},
	}, true, nil
}

func OnChannelDelete(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var channelDeletePayload discord.ChannelDelete

	err = ctx.decodeContent(msg, &channelDeletePayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.SafeOnGuildDispatchEvent(msg.Type, channelDeletePayload.GuildID)

	var beforeChannel *discord.Channel

	if channelDeletePayload.GuildID != nil && !channelDeletePayload.GuildID.IsNil() {
		beforeChannelV, _ := ctx.Sandwich.State.GetGuildChannel(*channelDeletePayload.GuildID, channelDeletePayload.ID)
		beforeChannel = &beforeChannelV

		ctx.Sandwich.State.RemoveGuildChannel(*channelDeletePayload.GuildID, channelDeletePayload.ID)
	} else if channelDeletePayload.Type == discord.ChannelTypeDM || channelDeletePayload.Type == discord.ChannelTypeGroupDM {
		beforeChannelV, _ := ctx.Sandwich.State.GetDMChannel(channelDeletePayload.ID)
		beforeChannel = &beforeChannelV

		ctx.Sandwich.State.RemoveDMChannelByChannelID(channelDeletePayload.ID)
	}

	extra, err := makeExtra(map[string]interface{}{
		"before": beforeChannel,
	})
	if err != nil {
		return result, ok, fmt.Errorf("failed to marshal extras: %w", err)
	}

	return EventDispatch{
		Data:  msg.Data,
		Extra: extra,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: channelDeletePayload.GuildID,
		},
	}, true, nil
}

func OnChannelPinsUpdate(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var channelPinsUpdatePayload discord.ChannelPinsUpdate

	err = ctx.decodeContent(msg, &channelPinsUpdatePayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.OnGuildDispatchEvent(msg.Type, channelPinsUpdatePayload.GuildID)

	if channelPinsUpdatePayload.GuildID.IsNil() {
		ctx.Sandwich.State.UpdateDMChannelByChannelID(channelPinsUpdatePayload.ChannelID, func(channel StateDMChannel) StateDMChannel {
			channel.Channel.LastPinTimestamp = &channelPinsUpdatePayload.LastPinTimestamp
			return channel
		})
	} else {
		ctx.Sandwich.State.UpdateGuildChannel(channelPinsUpdatePayload.GuildID, channelPinsUpdatePayload.ChannelID, func(channel discord.Channel) discord.Channel {
			channel.LastPinTimestamp = &channelPinsUpdatePayload.LastPinTimestamp
			return channel
		})
	}

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: &channelPinsUpdatePayload.GuildID,
		},
	}, true, nil
}

func OnThreadUpdate(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	defer ctx.OnDispatchEvent(msg.Type)

	var threadUpdatePayload discord.ThreadUpdate

	err = ctx.decodeContent(msg, &threadUpdatePayload)
	if err != nil {
		return result, false, err
	}

	var beforeChannel *discord.Channel

	// Only supports guilds anyways
	if threadUpdatePayload.GuildID != nil && !threadUpdatePayload.GuildID.IsNil() {
		beforeChannelV, _ := ctx.Sandwich.State.GetGuildChannel(*threadUpdatePayload.GuildID, threadUpdatePayload.ID)
		beforeChannel = &beforeChannelV
	}

	extra, err := makeExtra(map[string]interface{}{
		"before": beforeChannel,
	})
	if err != nil {
		return result, ok, fmt.Errorf("failed to marshal extras: %w", err)
	}

	return EventDispatch{
		Data:  msg.Data,
		Extra: extra,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: threadUpdatePayload.GuildID,
		},
	}, true, nil
}

func OnGuildAuditLogEntryCreate(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	defer ctx.OnDispatchEvent(msg.Type)

	var threadMembersUpdatePayload discord.GuildAuditLogEntryCreate

	err = ctx.decodeContent(msg, &threadMembersUpdatePayload)
	if err != nil {
		return result, false, err
	}

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: &threadMembersUpdatePayload.GuildID,
		},
	}, true, nil
}

func OnGuildUpdate(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var guildUpdatePayload discord.GuildUpdate

	err = ctx.decodeContent(msg, &guildUpdatePayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.OnGuildDispatchEvent(msg.Type, guildUpdatePayload.ID)

	beforeGuild, ok := ctx.Sandwich.State.GetGuild(guildUpdatePayload.ID)

	if ok {
		// Preserve values only present in GUILD_CREATE events.
		if guildUpdatePayload.StageInstances == nil {
			guildUpdatePayload.StageInstances = beforeGuild.StageInstances
		}

		if guildUpdatePayload.Channels == nil {
			guildUpdatePayload.Channels = beforeGuild.Channels
		}

		if guildUpdatePayload.Members == nil {
			guildUpdatePayload.Members = beforeGuild.Members
		}

		if guildUpdatePayload.VoiceStates == nil {
			guildUpdatePayload.VoiceStates = beforeGuild.VoiceStates
		}

		if guildUpdatePayload.MemberCount == 0 {
			guildUpdatePayload.MemberCount = beforeGuild.MemberCount
		}

		guildUpdatePayload.Large = beforeGuild.Large
		guildUpdatePayload.JoinedAt = beforeGuild.JoinedAt
	} else {
		ctx.Logger.Warn().
			Int64("guild_id", int64(guildUpdatePayload.ID)).
			Msg("Received " + discord.DiscordEventGuildUpdate + " event, but previous guild not present in state")
	}

	ctx.Sandwich.State.SetGuild(ctx, discord.Guild(guildUpdatePayload))

	extra, err := makeExtra(map[string]interface{}{
		"before": beforeGuild,
	})
	if err != nil {
		return result, ok, fmt.Errorf("failed to marshal extras: %w", err)
	}

	return EventDispatch{
		Data:  msg.Data,
		Extra: extra,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: &guildUpdatePayload.ID,
		},
	}, true, nil
}

func OnGuildDelete(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var guildDeletePayload discord.GuildDelete

	err = ctx.decodeContent(msg, &guildDeletePayload)
	if err != nil {
		ctx.Logger.Error().Err(err).Msg("Failed to decode GUILD_DELETE payload")
		return EventDispatch{
			Data: msg.Data,
			EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
				GuildID: &guildDeletePayload.ID,
			},
		}, true, nil
	}

	defer ctx.OnGuildDispatchEvent(msg.Type, guildDeletePayload.ID)

	beforeGuild, _ := ctx.Sandwich.State.GetGuild(guildDeletePayload.ID)

	if guildDeletePayload.Unavailable {
		ctx.Unavailable.Store(guildDeletePayload.ID, struct{}{})
	} else {
		// We do not remove the actual guild as other managers may be using it.
		// Dereferencing it locally ensures that if other managers are using it,
		// it will stay.
		ctx.ShardGroup.Guilds.Delete(guildDeletePayload.ID)
		ctx.Guilds.Delete(guildDeletePayload.ID)
	}

	extra, err := makeExtra(map[string]interface{}{
		"before": beforeGuild,
	})

	// We still need to dispatch the event to the producers
	if err != nil {
		return EventDispatch{
			Data: msg.Data,
			EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
				GuildID: &guildDeletePayload.ID,
			},
		}, true, nil
	}

	return EventDispatch{
		Data:  msg.Data,
		Extra: extra,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: &guildDeletePayload.ID,
		},
	}, true, nil
}

func OnGuildBanAdd(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var guildBanAddPayload discord.GuildBanAdd

	err = ctx.decodeContent(msg, &guildBanAddPayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.OnGuildDispatchEvent(msg.Type, *guildBanAddPayload.GuildID)

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: guildBanAddPayload.GuildID,
		},
	}, true, nil
}

func OnGuildBanRemove(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var guildBanRemovePayload discord.GuildBanRemove

	err = ctx.decodeContent(msg, &guildBanRemovePayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.OnGuildDispatchEvent(msg.Type, *guildBanRemovePayload.GuildID)

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: guildBanRemovePayload.GuildID,
		},
	}, true, nil
}

func OnGuildEmojisUpdate(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var guildEmojisUpdatePayload discord.GuildEmojisUpdate

	err = ctx.decodeContent(msg, &guildEmojisUpdatePayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.OnGuildDispatchEvent(msg.Type, guildEmojisUpdatePayload.GuildID)

	beforeEmojis, _ := ctx.Sandwich.State.GetAllGuildEmojis(guildEmojisUpdatePayload.GuildID)

	ctx.Sandwich.State.RemoveAllGuildEmojis(guildEmojisUpdatePayload.GuildID)
	ctx.Sandwich.State.SetGuildEmojis(ctx, guildEmojisUpdatePayload.GuildID, guildEmojisUpdatePayload.Emojis)

	extra, err := makeExtra(map[string]interface{}{
		"before": beforeEmojis,
	})
	if err != nil {
		return result, ok, fmt.Errorf("failed to marshal extras: %w", err)
	}

	return EventDispatch{
		Data:  msg.Data,
		Extra: extra,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: &guildEmojisUpdatePayload.GuildID,
		},
	}, true, nil
}

func OnGuildStickersUpdate(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var guildStickersUpdatePayload discord.GuildStickersUpdate

	err = ctx.decodeContent(msg, &guildStickersUpdatePayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.OnGuildDispatchEvent(msg.Type, guildStickersUpdatePayload.GuildID)

	beforeGuild, ok := ctx.Sandwich.State.GetGuild(guildStickersUpdatePayload.GuildID)
	beforeStickers := beforeGuild.Stickers

	if ok {
		beforeGuild.Stickers = discord.StickerList(guildStickersUpdatePayload.Stickers)

		ctx.Sandwich.State.SetGuild(ctx, beforeGuild)
	} else {
		ctx.Logger.Warn().
			Int64("guild_id", int64(guildStickersUpdatePayload.GuildID)).
			Msg("Received " + discord.DiscordEventGuildStickersUpdate + ", however guild is not present in state")
	}

	extra, err := makeExtra(map[string]interface{}{
		"before": beforeStickers,
	})
	if err != nil {
		return result, ok, fmt.Errorf("failed to marshal extras: %w", err)
	}

	return EventDispatch{
		Data:  msg.Data,
		Extra: extra,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: &guildStickersUpdatePayload.GuildID,
		},
	}, true, nil
}

func OnGuildIntegrationsUpdate(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var guildIntegrationsUpdatePayload discord.GuildIntegrationsUpdate

	err = ctx.decodeContent(msg, &guildIntegrationsUpdatePayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.OnGuildDispatchEvent(msg.Type, guildIntegrationsUpdatePayload.GuildID)

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: &guildIntegrationsUpdatePayload.GuildID,
		},
	}, true, nil
}

func OnGuildMemberAdd(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var guildMemberAddPayload discord.GuildMemberAdd

	err = ctx.decodeContent(msg, &guildMemberAddPayload)
	if err != nil {
		return result, false, err
	}

	ddRemoveKey := createDedupeMemberRemoveKey(*guildMemberAddPayload.GuildID, guildMemberAddPayload.User.ID)
	ddAddKey := createDedupeMemberAddKey(*guildMemberAddPayload.GuildID, guildMemberAddPayload.User.ID)

	if !ctx.Sandwich.CheckAndAddDedupe(ddAddKey) {
		ctx.Sandwich.RemoveDedupe(ddRemoveKey)

		ctx.Sandwich.State.Guilds.Update(*guildMemberAddPayload.GuildID, func(guild discord.Guild) discord.Guild {
			guild.MemberCount++
			return guild
		})
	} else {
		ctx.Logger.Info().
			Int64("guild_id", int64(*guildMemberAddPayload.GuildID)).
			Int64("user_id", int64(guildMemberAddPayload.User.ID)).
			Msg("Deduped GUILD_MEMBER_ADD event")

		return result, false, nil
	}

	defer ctx.OnGuildDispatchEvent(msg.Type, *guildMemberAddPayload.GuildID)

	ctx.Sandwich.State.SetGuildMember(ctx, *guildMemberAddPayload.GuildID, discord.GuildMember(guildMemberAddPayload))

	if ctx.StoreMutuals {
		ctx.Sandwich.State.AddUserMutualGuild(ctx, guildMemberAddPayload.User.ID, *guildMemberAddPayload.GuildID)
	}

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: guildMemberAddPayload.GuildID,
		},
	}, true, nil
}

func OnGuildMemberRemove(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var guildMemberRemovePayload discord.GuildMemberRemove

	err = ctx.decodeContent(msg, &guildMemberRemovePayload)
	if err != nil {
		return result, false, err
	}

	ddRemoveKey := createDedupeMemberRemoveKey(guildMemberRemovePayload.GuildID, guildMemberRemovePayload.User.ID)
	ddAddKey := createDedupeMemberAddKey(guildMemberRemovePayload.GuildID, guildMemberRemovePayload.User.ID)

	if !ctx.Sandwich.CheckAndAddDedupe(ddRemoveKey) {
		ctx.Sandwich.RemoveDedupe(ddAddKey)

		guild, ok := ctx.Sandwich.State.Guilds.Load(guildMemberRemovePayload.GuildID)

		if ok {
			guild.MemberCount--
			ctx.Sandwich.State.Guilds.SetIfPresent(guildMemberRemovePayload.GuildID, guild)
		}
	} else {
		ctx.Logger.Info().
			Int64("guild_id", int64(guildMemberRemovePayload.GuildID)).
			Int64("user_id", int64(guildMemberRemovePayload.User.ID)).
			Msg("Deduped GUILD_MEMBER_REMOVE event")

		return result, false, nil
	}

	defer ctx.OnGuildDispatchEvent(msg.Type, guildMemberRemovePayload.GuildID)

	guildMember, _ := ctx.Sandwich.State.GetGuildMember(guildMemberRemovePayload.GuildID, guildMemberRemovePayload.User.ID)

	ctx.Sandwich.State.RemoveGuildMember(guildMemberRemovePayload.GuildID, guildMemberRemovePayload.User.ID)
	ctx.Sandwich.State.RemoveUserMutualGuild(guildMemberRemovePayload.User.ID, guildMemberRemovePayload.GuildID)

	extra, err := makeExtra(map[string]interface{}{
		"before": guildMember,
	})
	if err != nil {
		return result, ok, fmt.Errorf("failed to marshal extras: %w", err)
	}

	return EventDispatch{
		Data:  msg.Data,
		Extra: extra,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: &guildMemberRemovePayload.GuildID,
		},
	}, true, nil
}

func OnGuildMemberUpdate(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var guildMemberUpdatePayload discord.GuildMemberUpdate

	err = ctx.decodeContent(msg, &guildMemberUpdatePayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.OnGuildDispatchEvent(msg.Type, *guildMemberUpdatePayload.GuildID)

	beforeGuildMember, _ := ctx.Sandwich.State.GetGuildMember(
		*guildMemberUpdatePayload.GuildID, guildMemberUpdatePayload.User.ID)

	ctx.Sandwich.State.SetGuildMember(ctx, *guildMemberUpdatePayload.GuildID, discord.GuildMember(guildMemberUpdatePayload))

	extra, err := makeExtra(map[string]interface{}{
		"before": beforeGuildMember,
	})
	if err != nil {
		return result, ok, fmt.Errorf("failed to marshal extras: %w", err)
	}

	return EventDispatch{
		Data:  msg.Data,
		Extra: extra,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: guildMemberUpdatePayload.GuildID,
		},
	}, true, nil
}

func OnGuildRoleCreate(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var guildRoleCreatePayload discord.GuildRoleCreate

	err = ctx.decodeContent(msg, &guildRoleCreatePayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.OnGuildDispatchEvent(msg.Type, *guildRoleCreatePayload.GuildID)

	ctx.Sandwich.State.SetGuildRole(*guildRoleCreatePayload.GuildID, discord.Role(guildRoleCreatePayload))

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: guildRoleCreatePayload.GuildID,
		},
	}, true, nil
}

func OnGuildRoleUpdate(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var guildRoleUpdatePayload discord.GuildRoleUpdate

	err = ctx.decodeContent(msg, &guildRoleUpdatePayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.OnGuildDispatchEvent(msg.Type, guildRoleUpdatePayload.GuildID)

	beforeRole, _ := ctx.Sandwich.State.GetGuildRole(
		guildRoleUpdatePayload.GuildID, guildRoleUpdatePayload.Role.ID)

	ctx.Sandwich.State.SetGuildRole(guildRoleUpdatePayload.GuildID, guildRoleUpdatePayload.Role)

	extra, err := makeExtra(map[string]interface{}{
		"before": beforeRole,
	})
	if err != nil {
		return result, ok, fmt.Errorf("failed to marshal extras: %w", err)
	}

	return EventDispatch{
		Data:  msg.Data,
		Extra: extra,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: &guildRoleUpdatePayload.GuildID,
		},
	}, true, nil
}

func OnGuildRoleDelete(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var guildRoleDeletePayload discord.GuildRoleDelete

	err = ctx.decodeContent(msg, &guildRoleDeletePayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.OnGuildDispatchEvent(msg.Type, guildRoleDeletePayload.GuildID)

	ctx.Sandwich.State.RemoveGuildRole(guildRoleDeletePayload.GuildID, guildRoleDeletePayload.RoleID)

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: &guildRoleDeletePayload.GuildID,
		},
	}, true, nil
}

func OnInviteCreate(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var inviteCreatePayload discord.InviteCreate

	err = ctx.decodeContent(msg, &inviteCreatePayload)
	if err != nil {
		return result, false, err
	}

	if inviteCreatePayload.GuildID != nil {
		defer ctx.SafeOnGuildDispatchEvent(msg.Type, inviteCreatePayload.GuildID)
	}

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: inviteCreatePayload.GuildID,
		},
	}, true, nil
}

func OnInviteDelete(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var inviteDeletePayload discord.InviteDelete

	err = ctx.decodeContent(msg, &inviteDeletePayload)
	if err != nil {
		return result, false, err
	}

	if inviteDeletePayload.GuildID != nil {
		defer ctx.SafeOnGuildDispatchEvent(msg.Type, inviteDeletePayload.GuildID)
	}

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: inviteDeletePayload.GuildID,
		},
	}, true, nil
}

func OnMessageCreate(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var messageCreatePayload discord.MessageCreate

	err = ctx.decodeContent(msg, &messageCreatePayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.SafeOnGuildDispatchEvent(msg.Type, messageCreatePayload.GuildID)

	// If no guild id, we know its a dm event anyways
	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: messageCreatePayload.GuildID,
			UserID:  &messageCreatePayload.Author.ID,
		},
	}, true, nil
}

func OnMessageUpdate(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var messageUpdatePayload discord.MessageUpdate

	err = ctx.decodeContent(msg, &messageUpdatePayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.SafeOnGuildDispatchEvent(msg.Type, messageUpdatePayload.GuildID)

	// If no guild id, we know its a dm event anyways
	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: messageUpdatePayload.GuildID,
			UserID:  &messageUpdatePayload.Author.ID,
		},
	}, true, nil
}

func OnMessageDelete(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var messageDeletePayload discord.MessageDelete

	err = ctx.decodeContent(msg, &messageDeletePayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.SafeOnGuildDispatchEvent(msg.Type, messageDeletePayload.GuildID)

	// If no guild id, we know its a dm event anyways
	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: messageDeletePayload.GuildID,
		},
	}, true, nil
}

func OnMessageDeleteBulk(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var messageDeleteBulkPayload discord.MessageDeleteBulk

	err = ctx.decodeContent(msg, &messageDeleteBulkPayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.SafeOnGuildDispatchEvent(msg.Type, messageDeleteBulkPayload.GuildID)

	// If no guild id, we know its a dm event anyways
	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: messageDeleteBulkPayload.GuildID,
		},
	}, true, nil
}

func OnMessageReactionAdd(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var messageReactionAddPayload discord.MessageReactionAdd

	err = ctx.decodeContent(msg, &messageReactionAddPayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.OnGuildDispatchEvent(msg.Type, messageReactionAddPayload.GuildID)

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: &messageReactionAddPayload.GuildID,
			UserID:  &messageReactionAddPayload.UserID,
		},
	}, true, nil
}

func OnMessageReactionRemove(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var messageReactionRemovePayload discord.MessageReactionRemove

	err = ctx.decodeContent(msg, &messageReactionRemovePayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.SafeOnGuildDispatchEvent(msg.Type, messageReactionRemovePayload.GuildID)

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: messageReactionRemovePayload.GuildID,
			UserID:  &messageReactionRemovePayload.UserID,
		},
	}, true, nil
}

func OnMessageReactionRemoveAll(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var messageReactionRemoveAllPayload discord.MessageReactionRemoveAll

	err = ctx.decodeContent(msg, &messageReactionRemoveAllPayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.OnGuildDispatchEvent(msg.Type, messageReactionRemoveAllPayload.GuildID)

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: &messageReactionRemoveAllPayload.GuildID,
		},
	}, true, nil
}

func OnMessageReactionRemoveEmoji(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var messageReactionRemoveEmojiPayload discord.MessageReactionRemoveEmoji

	err = ctx.decodeContent(msg, &messageReactionRemoveEmojiPayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.SafeOnGuildDispatchEvent(msg.Type, messageReactionRemoveEmojiPayload.GuildID)

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: messageReactionRemoveEmojiPayload.GuildID,
		},
	}, true, nil
}

func OnPresenceUpdate(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var presenceUpdatePayload discord.PresenceUpdate

	err = ctx.decodeContent(msg, &presenceUpdatePayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.OnGuildDispatchEvent(msg.Type, presenceUpdatePayload.GuildID)

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: &presenceUpdatePayload.GuildID,
		},
	}, true, nil
}

func OnTypingStart(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var typingStartPayload discord.TypingStart

	err = ctx.decodeContent(msg, &typingStartPayload)
	if err != nil {
		return result, false, err
	}

	defer ctx.SafeOnGuildDispatchEvent(msg.Type, typingStartPayload.GuildID)

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: typingStartPayload.GuildID,
		},
	}, true, nil
}

func OnUserUpdate(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	defer ctx.OnDispatchEvent(msg.Type)

	var userUpdatePayload discord.UserUpdate

	err = ctx.decodeContent(msg, &userUpdatePayload)
	if err != nil {
		return result, false, err
	}

	beforeUser, _ := ctx.Sandwich.State.GetUser(userUpdatePayload.ID)

	extra, err := makeExtra(map[string]interface{}{
		"before": beforeUser,
	})
	if err != nil {
		return result, ok, fmt.Errorf("failed to marshal extras: %w", err)
	}

	return EventDispatch{
		Data:  msg.Data,
		Extra: extra,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GloballyRouted: true, // No Guild ID is available for routing *user* (not member) updates, send to all shards
		},
	}, true, nil
}

func OnVoiceStateUpdate(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	var voiceStateUpdatePayload discord.VoiceStateUpdate

	err = ctx.decodeContent(msg, &voiceStateUpdatePayload)
	if err != nil {
		return result, false, err
	}

	var guildID discord.GuildID

	if voiceStateUpdatePayload.GuildID != nil {
		guildID = *voiceStateUpdatePayload.GuildID
		defer ctx.OnGuildDispatchEvent(msg.Type, guildID)
	}

	beforeVoiceState, _ := ctx.Sandwich.State.GetVoiceState(guildID, voiceStateUpdatePayload.UserID)

	if guildID.IsNil() {
		ctx.Sandwich.State.RemoveVoiceState(ctx, guildID, voiceStateUpdatePayload.UserID)
	} else {
		ctx.Sandwich.State.UpdateVoiceState(ctx, discord.VoiceState(voiceStateUpdatePayload))
	}

	extra, err := makeExtra(map[string]interface{}{
		"before": beforeVoiceState,
	})
	if err != nil {
		return result, ok, fmt.Errorf("failed to marshal extras: %w", err)
	}

	return EventDispatch{
		Data:  msg.Data,
		Extra: extra,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID: voiceStateUpdatePayload.GuildID,
		},
	}, true, nil
}

func WildcardEvent(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	defer ctx.OnDispatchEvent(msg.Type)

	var guildId struct {
		GuildID *discord.GuildID `json:"guild_id"`
		UserID  *discord.UserID  `json:"user_id"`
	}

	err = ctx.decodeContent(msg, &guildId)

	if err != nil {
		return result, false, err
	}

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID:        guildId.GuildID,
			UserID:         guildId.UserID,
			GloballyRouted: guildId.GuildID == nil,
		},
	}, true, nil
}

func OnEntitlementCreate(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	defer ctx.OnDispatchEvent(msg.Type)

	var entitlementCreatePayload discord.Entitlement

	err = ctx.decodeContent(msg, &entitlementCreatePayload)
	if err != nil {
		return result, false, err
	}

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID:        entitlementCreatePayload.GuildID,
			GloballyRouted: entitlementCreatePayload.GuildID == nil,
		},
	}, true, nil
}

func OnEntitlementUpdate(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	defer ctx.OnDispatchEvent(msg.Type)

	var entitlementUpdatePayload discord.Entitlement

	err = ctx.decodeContent(msg, &entitlementUpdatePayload)
	if err != nil {
		return result, false, err
	}

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID:        entitlementUpdatePayload.GuildID,
			GloballyRouted: entitlementUpdatePayload.GuildID == nil,
		},
	}, true, nil
}

func OnEntitlementDelete(ctx StateCtx, msg discord.GatewayPayload, trace sandwich_structs.SandwichTrace) (result EventDispatch, ok bool, err error) {
	defer ctx.OnDispatchEvent(msg.Type)

	var entitlementUpdatePayload discord.Entitlement

	err = ctx.decodeContent(msg, &entitlementUpdatePayload)
	if err != nil {
		return result, false, err
	}

	return EventDispatch{
		Data: msg.Data,
		EventDispatchIdentifier: &sandwich_structs.EventDispatchIdentifier{
			GuildID:        entitlementUpdatePayload.GuildID,
			GloballyRouted: entitlementUpdatePayload.GuildID == nil,
		},
	}, true, nil
}

func init() {
	registerDispatch(discord.DiscordEventReady, OnReady)
	registerDispatch(discord.DiscordEventResumed, OnResumed)
	registerDispatch(discord.DiscordEventGuildMembersChunk, OnGuildMembersChunk)
	registerDispatch(discord.DiscordEventChannelCreate, OnChannelCreate)
	registerDispatch(discord.DiscordEventChannelUpdate, OnChannelUpdate)
	registerDispatch(discord.DiscordEventChannelDelete, OnChannelDelete)
	registerDispatch(discord.DiscordEventChannelPinsUpdate, OnChannelPinsUpdate)
	registerDispatch(discord.DiscordEventThreadUpdate, OnThreadUpdate)
	registerDispatch(discord.DiscordEventGuildCreate, OnGuildCreate)
	registerDispatch(discord.DiscordEventGuildAuditLogEntryCreate, OnGuildAuditLogEntryCreate)
	registerDispatch(discord.DiscordEventGuildUpdate, OnGuildUpdate)
	registerDispatch(discord.DiscordEventGuildDelete, OnGuildDelete)
	registerDispatch(discord.DiscordEventGuildBanAdd, OnGuildBanAdd)
	registerDispatch(discord.DiscordEventGuildBanRemove, OnGuildBanRemove)
	registerDispatch(discord.DiscordEventGuildEmojisUpdate, OnGuildEmojisUpdate)
	registerDispatch(discord.DiscordEventGuildStickersUpdate, OnGuildStickersUpdate)
	registerDispatch(discord.DiscordEventGuildIntegrationsUpdate, OnGuildIntegrationsUpdate)
	registerDispatch(discord.DiscordEventGuildMemberAdd, OnGuildMemberAdd)
	registerDispatch(discord.DiscordEventGuildMemberRemove, OnGuildMemberRemove)
	registerDispatch(discord.DiscordEventGuildMemberUpdate, OnGuildMemberUpdate)
	registerDispatch(discord.DiscordEventGuildRoleCreate, OnGuildRoleCreate)
	registerDispatch(discord.DiscordEventGuildRoleUpdate, OnGuildRoleUpdate)
	registerDispatch(discord.DiscordEventGuildRoleDelete, OnGuildRoleDelete)
	registerDispatch(discord.DiscordEventInviteCreate, OnInviteCreate)
	registerDispatch(discord.DiscordEventInviteDelete, OnInviteDelete)
	registerDispatch(discord.DiscordEventMessageCreate, OnMessageCreate)
	registerDispatch(discord.DiscordEventMessageUpdate, OnMessageUpdate)
	registerDispatch(discord.DiscordEventMessageDelete, OnMessageDelete)
	registerDispatch(discord.DiscordEventMessageDeleteBulk, OnMessageDeleteBulk)
	registerDispatch(discord.DiscordEventMessageReactionAdd, OnMessageReactionAdd)
	registerDispatch(discord.DiscordEventMessageReactionRemove, OnMessageReactionRemove)
	registerDispatch(discord.DiscordEventMessageReactionRemoveAll, OnMessageReactionRemoveAll)
	registerDispatch(discord.DiscordEventMessageReactionRemoveEmoji, OnMessageReactionRemoveEmoji)
	registerDispatch(discord.DiscordEventPresenceUpdate, OnPresenceUpdate)
	registerDispatch(discord.DiscordEventTypingStart, OnTypingStart)
	registerDispatch(discord.DiscordEventUserUpdate, OnUserUpdate)
	registerDispatch(discord.DiscordEventVoiceStateUpdate, OnVoiceStateUpdate)
	registerDispatch(discord.DiscordEventEntitlementCreate, OnEntitlementCreate)
	registerDispatch(discord.DiscordEventEntitlementUpdate, OnEntitlementUpdate)
	registerDispatch(discord.DiscordEventEntitlementDelete, OnEntitlementDelete)
}
