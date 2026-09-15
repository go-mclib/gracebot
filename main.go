package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
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
	"github.com/go-mclib/data/packets"
	"github.com/sashabaranov/go-openai"
)

type config struct {
	address, username, brand, clientID                                   string
	online, verbose, interactive, gravity, respawn, disconnectOnTransfer bool
	reconnects                                                           int
	aliases                                                              []string
	apiKey, apiBase, model, personalityPath, databasePath, adminsPath    string
	legacyDataPath, legacyHistoryPath, archivePath                       string
	engagementTimeout, debounce                                          time.Duration
	consoleChat, consoleInput                                            bool
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
	aliases := "grace,steedsgate,steeds"
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
	flags.StringVar(&cfg.databasePath, "database", "gracebot.db", "SQLite chat, memory, scores and ranks database")
	flags.StringVar(&cfg.adminsPath, "admins", "admins.json", "administrator usernames JSON file")
	flags.StringVar(&cfg.legacyDataPath, "import-data", "player_data.json", "one-time legacy player JSON import, empty disables")
	flags.StringVar(&cfg.legacyHistoryPath, "import-history", "chat_history.jsonl", "one-time legacy JSONL import, empty disables")
	flags.StringVar(&cfg.archivePath, "archives", "chat_archives", "private daily compressed chat archive directory")
	flags.DurationVar(&cfg.engagementTimeout, "conversation-timeout", defaultEngagementTimeout, "incoming silence timeout (0 requires mentions)")
	flags.DurationVar(&cfg.debounce, "chat-debounce", defaultChatDebounce, "quiet period before processing a speaker's message batch")
	flags.BoolVar(&cfg.consoleChat, "console-chat", false, "loopback testing only: respond to server /say as an UNPRIVILEGED speaker")
	flags.BoolVar(&cfg.consoleInput, "console-input", false, "loopback testing only: operator requests from this bot's stdin")
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
	if cfg.engagementTimeout < 0 || cfg.engagementTimeout > 30*time.Minute {
		return cfg, fmt.Errorf("conversation-timeout must be between 0 and 30m")
	}
	if cfg.debounce < 0 || cfg.debounce > 10*time.Second {
		return cfg, fmt.Errorf("chat-debounce must be between 0 and 10s")
	}
	if cfg.consoleChat || cfg.consoleInput {
		host := cfg.address
		if parsed, _, err := net.SplitHostPort(cfg.address); err == nil {
			host = parsed
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return cfg, fmt.Errorf("console testing requires a literal loopback server address such as 127.0.0.1:25576")
		}
		if cfg.consoleInput && cfg.interactive {
			return cfg, fmt.Errorf("console-input and interactive chat cannot both read stdin")
		}
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
	ctx, shutdown := context.WithCancel(ctx)
	defer shutdown()
	personality, err := os.ReadFile(cfg.personalityPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read personality: %w", err)
	}
	store, err := openPlayerStore(cfg.databasePath, cfg.adminsPath)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, store.close()) }()
	if err := store.importLegacy(cfg.legacyDataPath, cfg.legacyHistoryPath); err != nil {
		return err
	}
	if err := store.maintenance(ctx, time.Now(), cfg.archivePath); err != nil {
		return err
	}
	journal := store.history()
	output := newChatOutput()
	output.journal = journal
	moderation := newChatModeration(store, output.send)
	activities := newActivities(store, output.send)
	defer activities.close()
	web := newWebTools()
	defer web.close()
	observations := newObservations()
	actions := newActionManager(store)
	defer actions.close()
	commands := newServerCommands(store)
	defer commands.close()
	commands.bindJournal(journal)
	commands.BeforeMutation = func() { actions.stopCurrent("server command") }
	apiConfig := openai.DefaultConfig(cfg.apiKey)
	apiConfig.BaseURL = cfg.apiBase
	tools := newToolRegistry(newGameManager(store).tools(), store.tools(), activities.tools(),
		observations.tools(), web.tools(), journal.tools(), commands.tools(), actions.tools(), moderation.tools(), operatorTools(store, shutdown), []toolSpec{weatherTool()})
	tools.blocked = func(actor string) bool { return moderation.blocked(actor, time.Now()) }
	assistant := newAssistant(openai.NewClientWithConfig(apiConfig), cfg.model, string(personality), tools, output.sendMessage)
	assistant.journal = journal
	assistant.moderation = moderation
	moderation.onChange = assistant.moderationChanged
	assistant.engagements.timeout = cfg.engagementTimeout
	assistant.engagements.debounce = cfg.debounce
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); assistant.run(workerCtx) }()
	defer func() { stopWorker(); <-workerDone }()
	moderationDone := make(chan struct{})
	go func() { defer close(moderationDone); moderation.run(workerCtx) }()
	defer func() { stopWorker(); <-moderationDone }()
	router := &chatRouter{assistant: assistant, store: store, aliases: cfg.aliases, journal: journal,
		observations: observations, commands: commands, actions: actions, consoleChat: cfg.consoleChat, moderation: moderation}
	if cfg.consoleInput {
		consoleDone := make(chan struct{})
		go func() { defer close(consoleDone); readConsoleInput(workerCtx, router, output) }()
		defer func() { stopWorker(); <-consoleDone }()
	}
	log.Printf("GraceBot: Minecraft %s (protocol %d), model %s; aliases %q", data.MinecraftVersion, data.ProtocolVersion, cfg.model, cfg.aliases)
	saveTicker := time.NewTicker(time.Minute)
	defer saveTicker.Stop()
	connectionDone := make(chan error, 1)
	connectionCtx, stopConnection := context.WithCancel(ctx)
	defer func() { stopConnection(); <-connectionDone }()
	go func() {
		defer close(connectionDone)
		connectionDone <- connect(connectionCtx, cfg, router, output, activities)
	}()
	for {
		select {
		case err := <-connectionDone:
			if ctx.Err() != nil {
				return nil
			}
			return err
		case <-saveTicker.C:
			if err := store.maintenance(ctx, time.Now(), cfg.archivePath); err != nil {
				log.Printf("maintain chat storage: %v", err)
			}
		}
	}
}

func connect(ctx context.Context, cfg config, router *chatRouter, output *chatOutput, activities *activities) error {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := connectOnce(ctx, cfg, router, output, activities)
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

func connectOnce(ctx context.Context, cfg config, router *chatRouter, output *chatOutput, activities *activities) error {
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer router.disconnect()
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
	chat := &synchronizedChat{inner: chatmodule.New(), ctx: sessionCtx, client: c, gate: make(chan struct{}, 1)}
	chat.inner.SuppressChatLogs = true // Only application-approved input may be retained.
	c.Register(chat)
	voice := newVoiceChat()
	c.Register(voice)
	defer voice.close()
	defer activities.bindVoice(nil)
	players := playerlist.New()
	c.Register(players)
	c.Register(registry.New())
	w := world.New()
	c.Register(w)
	router.observations.register(c, me, players, w)
	if cfg.gravity {
		c.Register(collisions.New())
		c.Register(physics.New())
	}
	router.actions.register(c)
	c.RegisterHandler(router.handler(sessionCtx, players))
	var stopClose func() bool
	c.OnConnect(func() {
		// Capture this TCP client, not a mutable client field, for cancellation.
		tcp := c.TCPClient
		stopClose = context.AfterFunc(sessionCtx, func() { _ = tcp.Close() })
	})
	c.OnPlay(func() {
		chat.gate <- struct{}{}
		chat.ready = true
		<-chat.gate
		conn := c.Conn()
		output.bind(func(ctx context.Context, text string) error { return chat.sendMessage(ctx, text, conn) }, c.Username)
		output.bindWhisper(func(ctx context.Context, actor, text string) error {
			return chat.sendCommand(ctx, "msg "+actor+" "+text, conn)
		})
		router.commands.bind(func(ctx context.Context, command string) error { return chat.sendCommand(ctx, command, conn) })
		router.commands.bindDiscovery(chat.commands)
		router.commands.bindCompletion(func(ctx context.Context, input string) (packets.S2CCommandSuggestions, error) {
			return chat.completeCommand(ctx, input, conn)
		})
		activities.bindVoice(voice)
		log.Printf("Listening for current username %q and aliases %q", c.Username, cfg.aliases)
	})
	c.OnTransfer(func() { activities.bindVoice(nil); output.bind(nil, ""); router.disconnect() })
	c.OnDisconnect(func() { cancel(); activities.bindVoice(nil); output.bind(nil, ""); router.disconnect() })
	err := c.ConnectAndStart(sessionCtx)
	if stopClose != nil {
		stopClose()
	}
	output.bind(nil, "")
	_ = c.TCPClient.Close()
	return err
}

// Unlike a Server /say packet, this input actually belongs to the local process
// operator. Only explicit loopback test mode creates the privileged namespace.
func readConsoleInput(ctx context.Context, router *chatRouter, output *chatOutput) {
	stop := context.AfterFunc(ctx, func() { _ = os.Stdin.Close() })
	defer stop()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 8192)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		text := scanner.Text()
		if strings.TrimSpace(text) == "" {
			continue
		}
		username := output.identity()
		if username == "" {
			log.Print("console input ignored: Minecraft is not ready")
			continue
		}
		at := time.Now()
		if !router.admitChat(ctx, consoleActor, "", text, true, at) {
			continue
		}
		id, err := router.journal.appendOwned("console", consoleActor, consoleActor, text, at)
		if err != nil {
			log.Printf("record console input: %v", err)
			continue
		}
		router.enqueue(ctx, username, consoleActor, text, id, at, true)
	}
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		log.Printf("console input: %v", err)
	}
}
