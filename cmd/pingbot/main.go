package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"ping-work/internal/config"
	"ping-work/internal/dedupe"
	"ping-work/internal/discord"
	"ping-work/internal/httpapi"
	"ping-work/internal/queue"
)

const (
	messageTTL     = 10 * time.Minute
	queueCapacity  = 1000
	workerCount    = 4
	serverShutdown = 10 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("aplicação encerrada com erro", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	apiToken := strings.TrimSpace(os.Getenv("API_TOKEN"))
	discordToken := strings.TrimSpace(os.Getenv("DISCORD_BOT_TOKEN"))
	if apiToken == "" {
		return fmt.Errorf("a variável API_TOKEN é obrigatória")
	}
	if discordToken == "" {
		return fmt.Errorf("a variável DISCORD_BOT_TOKEN é obrigatória")
	}

	destinations, err := config.ParseDestinations(os.Getenv("DISCORD_DESTINATIONS"))
	if err != nil {
		return err
	}
	logger.Info("destinos carregados", "count", len(destinations))

	databasePath := envOrDefault("DATABASE_PATH", "./data/ping-work.sqlite")
	dedupeStore, err := dedupe.Open(databasePath)
	if err != nil {
		return err
	}
	defer func() {
		if err := dedupeStore.Close(); err != nil {
			logger.Warn("erro ao fechar banco SQLite", "error", err)
		}
	}()
	logger.Info("banco SQLite de deduplicação iniciado", "path", databasePath, "retention", "30 dias")

	gateway, err := discord.ConnectGateway(discordToken, logger)
	if err != nil {
		return err
	}
	defer func() {
		if err := gateway.Close(); err != nil {
			logger.Warn("erro ao fechar conexão Gateway", "error", err)
		}
	}()

	messageQueue := queue.New(queueCapacity, messageTTL)
	sender := discord.NewClient(discordToken)

	workerCtx, cancelWorkers := context.WithCancel(context.Background())
	defer cancelWorkers()
	var workers sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		workers.Add(1)
		go func(workerID int) {
			defer workers.Done()
			work(workerCtx, workerID, messageQueue, sender, logger)
		}(i + 1)
	}
	go expireMessages(workerCtx, messageQueue)
	workers.Add(1)
	go func() {
		defer workers.Done()
		pruneExpiredWorks(workerCtx, dedupeStore, logger)
	}()

	address := envOrDefault("HTTP_ADDR", ":8080")
	api := httpapi.NewServer(apiToken, destinations, messageQueue, dedupeStore, logger)
	server := &http.Server{
		Addr:              address,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("servidor HTTP iniciado", "address", address)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
		}
	}()

	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case <-signalCtx.Done():
		logger.Info("sinal de encerramento recebido")
	case err := <-serverErrors:
		cancelWorkers()
		workers.Wait()
		return fmt.Errorf("servidor HTTP: %w", err)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), serverShutdown)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("encerramento HTTP excedeu o prazo", "error", err)
		_ = server.Close()
	}
	cancelWorkers()
	workers.Wait()
	return nil
}

func work(ctx context.Context, workerID int, messageQueue *queue.Queue, sender *discord.Client, logger *slog.Logger) {
	for {
		message, err := messageQueue.Next(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				logger.Error("worker não conseguiu obter mensagem", "worker_id", workerID, "error", err)
			}
			return
		}

		failed := false
		delay := retryDelay(message.Attempts)
		for _, channelID := range message.ChannelIDs {
			if time.Until(message.ExpiresAt) <= 0 {
				break
			}
			sendCtx, cancel := context.WithDeadline(ctx, message.ExpiresAt)
			err = sender.SendWork(sendCtx, channelID, message.Title, message.Content, message.Link, message.Date)
			cancel()
			if err == nil {
				messageQueue.MarkDelivered(message.ID, channelID)
				logger.Info("vaga enviada", "message_id", message.ID, "channel_id", channelID)
				continue
			}

			failed = true
			var apiErr *discord.APIError
			if errors.As(err, &apiErr) && apiErr.RetryAfter > delay {
				delay = apiErr.RetryAfter
			}
			logger.Warn("falha ao enviar vaga para destino; nova tentativa agendada",
				"worker_id", workerID,
				"message_id", message.ID,
				"channel_id", channelID,
				"attempt", message.Attempts,
				"error", err,
			)
		}

		if time.Until(message.ExpiresAt) <= 0 {
			messageQueue.Complete(message.ID)
			logger.Warn("vaga expirou antes de ser enviada a todos os destinos", "message_id", message.ID)
			continue
		}
		if failed {
			messageQueue.Retry(message.ID, time.Now().Add(delay))
			logger.Info("nova tentativa da vaga agendada", "message_id", message.ID, "retry_in", delay.String())
		} else {
			messageQueue.Complete(message.ID)
		}
	}
}

func expireMessages(ctx context.Context, messageQueue *queue.Queue) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			messageQueue.Expire()
		}
	}
}

func pruneExpiredWorks(ctx context.Context, dedupeStore *dedupe.Store, logger *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := dedupeStore.PruneExpired(ctx); err != nil {
				logger.Error("não foi possível limpar vagas expiradas do SQLite", "error", err)
			}
		}
	}
}

func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	exponent := attempt - 1
	if exponent > 5 {
		exponent = 5
	}
	delay := time.Second * time.Duration(1<<exponent)
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

func envOrDefault(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}
