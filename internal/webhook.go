package internal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/WelcomerTeam/Discord/discord"
	jsoniter "github.com/json-iterator/go"
)

// Embed colours for webhooks.
const (
	EmbedColourSandwich = 16701571
	EmbedColourWarning  = 16760839
	EmbedColourDanger   = 14431557

	WebhookRateLimitDuration = 5 * time.Second
	WebhookRateLimitLimit    = 5
)

// PublishSimpleWebhook is a helper function for creating quicker webhook messages.
func (sg *Sandwich) PublishSimpleWebhook(title string, description string, footer string, colour int32) {
	now := discord.Timestamp(time.Now().UTC().String())

	sg.PublishWebhook(discord.WebhookMessageParams{
		Embeds: []*discord.Embed{
			{
				Title:       title,
				Description: description,
				Color:       colour,
				Timestamp:   &now,
				Footer: &discord.EmbedFooter{
					Text: footer,
				},
			},
		},
	})
}

// PublishWebhook sends a webhook message to all added webhooks in the configuration.
func (sg *Sandwich) PublishWebhook(message discord.WebhookMessageParams) {
	for _, webhook := range sg.Configuration.Webhooks {
		_, err := sg.SendWebhook(webhook, message)
		if err != nil && !errors.Is(err, context.Canceled) {
			sg.Logger.Warn().Err(err).Str("url", webhook).Msg("Failed to send webhook")
		}
	}
}

func (sg *Sandwich) SendWebhook(webhookURL string, message discord.WebhookMessageParams) (status int, err error) {
	sg.Logger.Debug().Str("url", webhookURL).Any("message", message).Msg("Sending webhook")

	webhookURL = strings.TrimSpace(webhookURL)

	_, err = url.Parse(webhookURL)
	if err != nil {
		return -1, fmt.Errorf("failed to parse webhook URL: %w", err)
	}

	res, err := jsoniter.Marshal(message)
	if err != nil {
		return -1, fmt.Errorf("failed to marshal webhook message: %w", err)
	}

	_ = sg.webhookBuckets.CreateWaitForBucket(webhookURL, WebhookRateLimitLimit, WebhookRateLimitDuration)

	_, status, err = sg.Client.Fetch(sg.ctx, "POST", webhookURL, bytes.NewBuffer(res), map[string]string{
		"Content-Type": "application/json",
	})

	return status, err
}
