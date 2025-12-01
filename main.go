package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/conneroisu/groq-go"
	"github.com/go-mclib/client/client"
)

func main() {
	cfg := LoadConfig()
	if cfg.GroqAPIKey == "" {
		log.Fatal("GROQ_API_KEY environment variable is not set")
	}
	if cfg.SystemPrompt == "" {
		log.Fatal("GROQ_SYSTEM_PROMPT environment variable is not set")
	}

	var (
		addr                      string
		verbose                   bool
		username                  string
		online                    bool
		hasGravity                bool
		interactive               bool
		treatTransferAsDisconnect bool
	)

	flag.StringVar(&addr, "s", "localhost:25565", "server address (host:port)")
	flag.BoolVar(&verbose, "v", false, "verbose logging")
	flag.StringVar(&username, "u", "", "offline username (offline or online)")
	flag.BoolVar(&online, "online", true, "assume that the server is in online-mode")
	flag.BoolVar(&hasGravity, "gravity", true, "currently not implemented")
	flag.BoolVar(&interactive, "i", false, "enable interactive mode with chat input")
	flag.BoolVar(&treatTransferAsDisconnect, "d", false, "treat server transfer as disconnect")
	flag.Parse()

	groqClient, err := groq.NewClient(cfg.GroqAPIKey)
	if err != nil {
		log.Fatalf("Failed to initialize Groq client: %v", err)
	}

	mcClient := client.NewClient(addr, username, verbose, online, hasGravity, cfg.AzureClientID)
	mcClient.Interactive = interactive
	mcClient.TreatTransferAsDisconnect = treatTransferAsDisconnect
	mcClient.RegisterDefaultHandlers()

	chatter := NewGraceBot(groqClient, cfg.SystemPrompt, mcClient.Username, cfg.BotAliases)
	registerChatHandler(mcClient, chatter)
	registerTransferHandler(mcClient, chatter)
	chatter.StartWithCancel(mcClient)

	// Log bot aliases if configured
	if len(cfg.BotAliases) > 0 {
		log.Printf("Bot aliases configured: %v", cfg.BotAliases)
	}

	done := make(chan error, 1)
	go func() { done <- mcClient.ConnectAndStart(context.Background()) }()

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)

	select {
	case <-sigc:
		log.Println("Shutting down...")
		mcClient.Disconnect(true)
	case err := <-done:
		if err != nil {
			log.Fatal(err)
		}
	}
}
