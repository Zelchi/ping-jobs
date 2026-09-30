package discord

import (
	"fmt"
	"log/slog"

	"github.com/bwmarrin/discordgo"
)

type Gateway struct {
	session *discordgo.Session
}

func ConnectGateway(token string, logger *slog.Logger) (*Gateway, error) {
	session, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("criar sessão Gateway: %w", err)
	}
	// O bot não precisa receber eventos de guild, mensagens ou presença.
	session.LogLevel = discordgo.LogWarning
	session.ShouldReconnectOnError = true
	session.Identify.Intents = discordgo.IntentsNone
	session.Identify.Presence = discordgo.GatewayStatusUpdate{
		Status: string(discordgo.StatusOnline),
	}

	if err := session.Open(); err != nil {
		return nil, fmt.Errorf("conectar ao Gateway do Discord: %w", err)
	}
	if err := session.UpdateStatusComplex(discordgo.UpdateStatusData{
		Status:     string(discordgo.StatusOnline),
		Activities: []*discordgo.Activity{},
	}); err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("definir presença online: %w", err)
	}

	logger.Info("conectado ao Gateway do Discord", "presence", "online")
	return &Gateway{session: session}, nil
}

func (g *Gateway) Close() error {
	if g == nil || g.session == nil {
		return nil
	}
	return g.session.Close()
}
