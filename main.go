package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-mclib/client/client"
)

var (
	addr                      string
	verbose                   bool
	username                  string
	online                    bool
	hasGravity                bool
	interactive               bool
	treatTransferAsDisconnect bool
	autoRespawn               bool
	brand                     string
	stringAliases             string
)

func main() {
	flag.StringVar(&addr, "s", "localhost:25565", "server address (host:port)")
	flag.BoolVar(&verbose, "v", false, "verbose logging")
	flag.StringVar(&username, "u", "", "offline username (offline or online)")
	flag.BoolVar(&online, "online", true, "assume that the server is in online-mode")
	flag.BoolVar(&hasGravity, "gravity", true, "currently not implemented")
	flag.BoolVar(&interactive, "i", false, "enable interactive mode with chat input")
	flag.BoolVar(&treatTransferAsDisconnect, "d", false, "treat server transfer as disconnect")
	flag.BoolVar(&autoRespawn, "r", true, "automatically respawn on death")
	flag.StringVar(&brand, "b", "gracebot", "brand to send to the server")
	flag.StringVar(&stringAliases, "a", "grace,bot", "aliases for the bot to listen to when it's mentioned")
	flag.Parse()

	mcClient := client.NewClient(addr, username, verbose, online, hasGravity, os.Getenv("AZURE_CLIENT_ID"))
	mcClient.AutoRespawn = autoRespawn
	mcClient.Brand = brand
	mcClient.MaxReconnectAttempts = -1
	mcClient.Interactive = interactive
	mcClient.TreatTransferAsDisconnect = treatTransferAsDisconnect
	mcClient.RegisterDefaultHandlers()

	// Initialize command framework (always available)
	RegisterDefaultCommands()
	InitWordle(mcClient)
	defer ShutdownWordle()
	log.Println("Command mode initialized. Use !help to see available commands.")

	// Initialize LLM manager for conversation mode (optional)
	var manager *LLMManager
	manager, err := NewLLMManager(mcClient)
	if err != nil {
		log.Printf("Conversation mode disabled: %v", err)
		log.Println("Bot will only respond to commands (! prefix)")
	} else {
		log.Println("Conversation mode enabled.")
		defer manager.Shutdown()
	}

	initChatHandlers(manager)
	mcClient.RegisterHandler(graceChatHandler)

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
