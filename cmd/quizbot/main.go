//go:build !solution

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"gitlab.com/slon/shad-go/Exam-1-QuizBot/quizbot/internal/quiz"
	"gitlab.com/slon/shad-go/Exam-1-QuizBot/quizbot/internal/storage"
	"gitlab.com/slon/shad-go/Exam-1-QuizBot/quizbot/internal/telegram"
)

func main() {
	var (
		token       = flag.String("token", "", "Telegram bot token")
		botUsername = flag.String("bot-username", "", "Telegram bot username (without @)")
		debug       = flag.Bool("debug", false, "Enable debug logs")
	)

	flag.Parse()

	if *token == "" || *botUsername == "" {
		_, _ = fmt.Fprintln(os.Stderr, "Usage: quizbot --token=... --bot-username=... [--debug]")

		os.Exit(2)
	}

	engine := quiz.NewEngine()
	st := storage.NewMemoryStorage()
	client := telegram.NewHTTPClient(*token)

	log.Printf("quizbot: starting (long polling)")
	log.Printf("quizbot: deleteWebhook (timeout ~5s)...")

	if err := client.DeleteWebhook(true); err != nil {
		log.Printf("quizbot: deleteWebhook failed: %v", err)
		log.Printf("quizbot: hint: likely no access to api.telegram.org or Telegram API is down")

		if *debug {
			log.Printf(
				"quizbot: continuing anyway, but getUpdates may not work if webhook stays enabled",
			)
		}
	} else {
		log.Printf("quizbot: deleteWebhook ok")
	}

	if me, err := client.GetMe(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Telegram token invalid or network error:", err)

		os.Exit(1)
	} else if *debug {
		log.Printf("quizbot: authenticated as @%s (id=%d)", me.Username, me.ID)
	}

	bot := telegram.NewBot(client, engine, *botUsername, st, *debug)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := bot.Run(ctx); err != nil && err != context.Canceled {
		_, _ = fmt.Fprintln(os.Stderr, err)

		os.Exit(1)
	}
}
