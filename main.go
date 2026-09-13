package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-mclib/client"
	chatmodule "github.com/go-mclib/client/modules/chat"
	"github.com/go-mclib/client/modules/collisions"
	"github.com/go-mclib/client/modules/physics"
	"github.com/go-mclib/client/modules/playerlist"
	"github.com/go-mclib/client/modules/protocol"
	"github.com/go-mclib/client/modules/registry"
	"github.com/go-mclib/client/modules/self"
	"github.com/go-mclib/client/modules/world"
	"github.com/go-mclib/data"
	"github.com/sashabaranov/go-openai"
)

type config struct {
	address, username, brand, clientID                                   string
	online, verbose, interactive, gravity, respawn, disconnectOnTransfer bool
	reconnects                                                           int
	aliases                                                              []string
	apiKey, apiBase, model, personalityPath, dataPath, adminsPath        string
}

func readConfig(args []string) (config, error) {
	var cfg config
	cfg.apiKey = os.Getenv("LLM_API_KEY")
	cfg.apiBase, cfg.model = "https://api.openai.com/v1", "gpt-4o-mini"
	if cfg.apiKey == "" {
		cfg.apiKey = os.Getenv("GROQ_API_KEY")
		if cfg.apiKey != "" {
			cfg.apiBase, cfg.model = "https://api.groq.com/openai/v1", "llama-3.3-70b-versatile"
		} else {
			cfg.apiKey = os.Getenv("OPENAI_API_KEY")
		}
	}
	if value := os.Getenv("LLM_BASE_URL"); value != "" {
		cfg.apiBase = value
	}
	if value := os.Getenv("LLM_MODEL"); value != "" {
		cfg.model = value
	}
	cfg.clientID = os.Getenv("AZURE_CLIENT_ID")
	aliases := "grace,bot"
	if value, set := os.LookupEnv("GRACEBOT_ALIASES"); set {
		aliases = value
	}
	flags := flag.NewFlagSet("gracebot", flag.ContinueOnError)
	flags.StringVar(&cfg.address, "s", "localhost:25565", "Minecraft server address")
	flags.StringVar(&cfg.username, "u", "", "offline username (default GraceBot) or cached online profile; mentions use the authenticated name")
	flags.BoolVar(&cfg.online, "online", true, "use Microsoft authentication (AZURE_CLIENT_ID)")
	flags.BoolVar(&cfg.verbose, "v", false, "verbose Minecraft logging")
	flags.BoolVar(&cfg.interactive, "i", false, "interactive terminal chat")
	flags.BoolVar(&cfg.gravity, "gravity", true, "enable world collision and gravity simulation")
	flags.BoolVar(&cfg.respawn, "r", true, "automatically respawn")
	flags.BoolVar(&cfg.disconnectOnTransfer, "d", false, "disconnect on server transfer")
	flags.IntVar(&cfg.reconnects, "reconnects", -1, "reconnect attempts (-1 unlimited, 0 none)")
	flags.StringVar(&cfg.brand, "b", "gracebot", "Minecraft client brand")
	flags.StringVar(&aliases, "a", aliases, "comma-separated case-insensitive aliases; empty means current username only")
	flags.StringVar(&cfg.apiBase, "api-base", cfg.apiBase, "OpenAI-compatible API base URL (LLM_BASE_URL)")
	flags.StringVar(&cfg.model, "model", cfg.model, "tool-capable chat model (LLM_MODEL)")
	flags.StringVar(&cfg.personalityPath, "personality", "personality.txt", "optional personality text file")
	flags.StringVar(&cfg.dataPath, "data", "player_data.json", "player statistics file")
	flags.StringVar(&cfg.adminsPath, "admins", "admins.json", "administrator usernames JSON file")
	if err := flags.Parse(args); err != nil {
		return cfg, err
	}
	if flags.NArg() != 0 {
		return cfg, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if cfg.apiKey == "" {
		return cfg, fmt.Errorf("set LLM_API_KEY, GROQ_API_KEY or OPENAI_API_KEY; GraceBot requires a tool-capable AI provider")
	}
	base, err := url.Parse(cfg.apiBase)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.RawQuery != "" || base.Fragment != "" || base.User != nil {
		return cfg, fmt.Errorf("api-base must be an HTTP(S) base URL without credentials, query or fragment")
	}
	if strings.TrimSpace(cfg.model) == "" {
		return cfg, fmt.Errorf("model must not be empty")
	}
	if cfg.reconnects < -1 {
		return cfg, fmt.Errorf("reconnects must be -1 or nonnegative")
	}
	if !cfg.online && cfg.username == "" {
		cfg.username = "GraceBot"
	}
	if !cfg.online && !validUsername(cfg.username) {
		return cfg, fmt.Errorf("offline username must be 1–16 letters, digits or underscores")
	}
	cfg.apiBase = strings.TrimRight(cfg.apiBase, "/")
	cfg.aliases = parseAliases(aliases)
	return cfg, nil
}

func validUsername(name string) bool {
	if len(name) < 1 || len(name) > 16 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

func main() {
	cfg, err := readConfig(os.Args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		log.Print(err)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, cfg); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config) (result error) {
	personality, err := os.ReadFile(cfg.personalityPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read personality: %w", err)
	}
	store, err := openPlayerStore(cfg.dataPath, cfg.adminsPath)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, store.save()) }()
	output := newChatOutput()
	activities := newActivities(store, output.send)
	defer activities.close()
	tools := newToolRegistry(newGameManager(store).tools(), store.tools(), activities.tools(), []toolSpec{weatherTool()})
	apiConfig := openai.DefaultConfig(cfg.apiKey)
	apiConfig.BaseURL = cfg.apiBase
	assistant := newAssistant(openai.NewClientWithConfig(apiConfig), cfg.model, string(personality), tools, output.send)
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); assistant.run(workerCtx) }()
	defer func() { stopWorker(); <-workerDone }()
	router := &chatRouter{assistant: assistant, store: store, aliases: cfg.aliases}
	log.Printf("GraceBot: Minecraft %s (protocol %d), model %s; aliases %q", data.MinecraftVersion, data.ProtocolVersion, cfg.model, cfg.aliases)
	saveTicker := time.NewTicker(time.Minute)
	defer saveTicker.Stop()
	connectionDone := make(chan error, 1)
	connectionCtx, stopConnection := context.WithCancel(ctx)
	defer stopConnection()
	go func() { connectionDone <- connect(connectionCtx, cfg, router, output) }()
	for {
		select {
		case err := <-connectionDone:
			if ctx.Err() != nil {
				return nil
			}
			return err
		case <-saveTicker.C:
			if err := store.save(); err != nil {
				log.Printf("save player data: %v", err)
			}
		}
	}
}

func connect(ctx context.Context, cfg config, router *chatRouter, output *chatOutput) error {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := connectOnce(ctx, cfg, router, output)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			return nil
		}
		if cfg.reconnects >= 0 && attempt >= cfg.reconnects {
			return err
		}
		log.Printf("Minecraft disconnected: %v; reconnecting in 3s", err)
		timer := time.NewTimer(3 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func connectOnce(ctx context.Context, cfg config, router *chatRouter, output *chatOutput) error {
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	c := client.New(cfg.address, cfg.username, cfg.online)
	c.Verbose, c.Interactive, c.Brand, c.ClientID = cfg.verbose, cfg.interactive, cfg.brand, cfg.clientID
	// The application owns cancellable reconnects; the upstream retry loop ignores context cancellation.
	c.MaxReconnectAttempts = 0
	proto := protocol.New()
	proto.TreatTransferAsDisconnect = cfg.disconnectOnTransfer
	c.Register(proto)
	me := self.New()
	me.SetAutoRespawn(cfg.respawn)
	c.Register(me)
	chat := &synchronizedChat{inner: chatmodule.New(), ctx: sessionCtx}
	c.Register(chat)
	players := playerlist.New()
	c.Register(players)
	c.Register(registry.New())
	if cfg.gravity {
		c.Register(world.New())
		c.Register(collisions.New())
		c.Register(physics.New())
	}
	c.RegisterHandler(router.handler(sessionCtx, players))
	var stopClose func() bool
	c.OnConnect(func() {
		// Capture this TCP client, not a mutable client field, for cancellation.
		tcp := c.TCPClient
		stopClose = context.AfterFunc(sessionCtx, func() { _ = tcp.Close() })
	})
	c.OnPlay(func() {
		chat.mu.Lock()
		chat.ready = true
		chat.mu.Unlock()
		output.bind(chat.SendMessage)
		log.Printf("Listening for current username %q and aliases %q", c.Username, cfg.aliases)
	})
	c.OnTransfer(func() { output.bind(nil) })
	c.OnDisconnect(func() { cancel(); output.bind(nil) })
	err := c.ConnectAndStart(sessionCtx)
	if stopClose != nil {
		stopClose()
	}
	output.bind(nil)
	_ = c.TCPClient.Close()
	return err
}
