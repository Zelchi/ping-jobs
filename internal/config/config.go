package config

import (
	"fmt"
	"strings"
)

type Destinations map[string]string

func ParseDestinations(value string) (Destinations, error) {
	if strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("a variável DISCORD_DESTINATIONS é obrigatória")
	}

	destinations := make(Destinations)
	for i, pair := range strings.Split(value, ",") {
		parts := strings.Split(strings.TrimSpace(pair), ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("destino %d inválido: use guild_id:channel_id", i+1)
		}
		guildID := strings.TrimSpace(parts[0])
		channelID := strings.TrimSpace(parts[1])
		if !isDiscordID(guildID) {
			return nil, fmt.Errorf("destino %d: guild_id inválido", i+1)
		}
		if !isDiscordID(channelID) {
			return nil, fmt.Errorf("destino %d: channel_id inválido", i+1)
		}
		if _, exists := destinations[guildID]; exists {
			return nil, fmt.Errorf("guild_id %q aparece mais de uma vez", guildID)
		}
		destinations[guildID] = channelID
	}

	return destinations, nil
}

func isDiscordID(value string) bool {
	if value == "" || len(value) > 20 || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}
