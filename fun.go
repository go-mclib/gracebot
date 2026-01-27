package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-mclib/client/client"
)

func init() {
	RegisterCommand("say", "Say something", "!say <message>", nil, sayCommand)
	RegisterCommand("8ball", "Ask the magic 8-ball", "!8ball <question>", nil, eightBallCommand)
	RegisterCommand("roll", "Roll dice", "!roll [NdN]", nil, rollCommand)
	RegisterCommand("flip", "Flip a coin", "!flip", nil, flipCommand)
	RegisterCommand("choose", "Random choice", "!choose <opt1> | <opt2> | ...", nil, chooseCommand)
	RegisterCommand("quote", "Random quote", "!quote", nil, quoteCommand)
	RegisterCommand("fact", "Random fun fact", "!fact", nil, factCommand)
}

func sayCommand(c *client.Client, sender string, args []string) {
	if len(args) == 0 {
		c.SendChatMessage("Usage: !say <message>")
		return
	}
	message := strings.Join(args, " ")
	c.SendChatMessage(message)
}

var eightBallResponses = []string{
	// Positive
	"It is certain.",
	"Without a doubt.",
	"Yes, definitely.",
	"You may rely on it.",
	"As I see it, yes.",
	"Most likely.",
	"Outlook good.",
	"Yes.",
	"Signs point to yes.",
	// Neutral
	"Reply hazy, try again.",
	"Ask again later.",
	"Better not tell you now.",
	"Cannot predict now.",
	"Concentrate and ask again.",
	// Negative
	"Don't count on it.",
	"My reply is no.",
	"My sources say no.",
	"Outlook not so good.",
	"Very doubtful.",
}

func eightBallCommand(c *client.Client, sender string, args []string) {
	if len(args) == 0 {
		c.SendChatMessage("Ask a question! Usage: !8ball <question>")
		return
	}
	response := eightBallResponses[rand.Intn(len(eightBallResponses))]
	c.SendChatMessage(fmt.Sprintf("%s: %s", sender, response))
}

var diceRegex = regexp.MustCompile(`^(\d+)?d(\d+)$`)

func rollCommand(c *client.Client, sender string, args []string) {
	numDice := 1
	sides := 6

	if len(args) > 0 {
		input := strings.ToLower(args[0])
		matches := diceRegex.FindStringSubmatch(input)
		if matches == nil {
			c.SendChatMessage("Usage: !roll [NdN] (e.g., !roll 2d6)")
			return
		}
		if matches[1] != "" {
			numDice, _ = strconv.Atoi(matches[1])
		}
		sides, _ = strconv.Atoi(matches[2])
	}

	if numDice < 1 || numDice > 100 {
		c.SendChatMessage("Number of dice must be between 1 and 100.")
		return
	}
	if sides < 2 || sides > 1000 {
		c.SendChatMessage("Dice sides must be between 2 and 1000.")
		return
	}

	var rolls []string
	total := 0
	for range numDice {
		roll := rand.Intn(sides) + 1
		total += roll
		rolls = append(rolls, strconv.Itoa(roll))
	}

	if numDice == 1 {
		c.SendChatMessage(fmt.Sprintf("%s rolled d%d: %d", sender, sides, total))
	} else {
		c.SendChatMessage(fmt.Sprintf("%s rolled %dd%d: %s = %d", sender, numDice, sides, strings.Join(rolls, "+"), total))
	}
}

func flipCommand(c *client.Client, sender string, args []string) {
	result := "Heads"
	if rand.Intn(2) == 1 {
		result = "Tails"
	}
	c.SendChatMessage(fmt.Sprintf("%s flipped: %s", sender, result))
}

func chooseCommand(c *client.Client, sender string, args []string) {
	if len(args) == 0 {
		c.SendChatMessage("Usage: !choose <option1> | <option2> | ...")
		return
	}

	// Join all args and split by |
	input := strings.Join(args, " ")
	options := strings.Split(input, "|")

	// Trim whitespace and filter empty options
	var validOptions []string
	for _, opt := range options {
		opt = strings.TrimSpace(opt)
		if opt != "" {
			validOptions = append(validOptions, opt)
		}
	}

	if len(validOptions) < 2 {
		c.SendChatMessage("Please provide at least 2 options separated by |")
		return
	}

	choice := validOptions[rand.Intn(len(validOptions))]
	c.SendChatMessage(fmt.Sprintf("%s: I choose... %s", sender, choice))
}

type zenQuoteResponse struct {
	Q string `json:"q"` // Quote
	A string `json:"a"` // Author
}

func quoteCommand(c *client.Client, sender string, args []string) {
	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get("https://zenquotes.io/api/random")
	if err != nil {
		c.SendChatMessage("Failed to fetch quote.")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.SendChatMessage("Failed to fetch quote.")
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.SendChatMessage("Failed to read quote.")
		return
	}

	var quotes []zenQuoteResponse
	if err := json.Unmarshal(body, &quotes); err != nil || len(quotes) == 0 {
		c.SendChatMessage("Failed to parse quote.")
		return
	}

	quote := quotes[0]
	// Truncate if needed
	text := quote.Q
	if len(text) > 180 {
		text = text[:177] + "..."
	}
	c.SendChatMessage(fmt.Sprintf("\"%s\" - %s", text, quote.A))
}

type uselessFactResponse struct {
	Text string `json:"text"`
}

func factCommand(c *client.Client, sender string, args []string) {
	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get("https://uselessfacts.jsph.pl/api/v2/facts/random")
	if err != nil {
		c.SendChatMessage("Failed to fetch fact.")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.SendChatMessage("Failed to fetch fact.")
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.SendChatMessage("Failed to read fact.")
		return
	}

	var fact uselessFactResponse
	if err := json.Unmarshal(body, &fact); err != nil {
		c.SendChatMessage("Failed to parse fact.")
		return
	}

	text := fact.Text
	if len(text) > 220 {
		text = text[:217] + "..."
	}
	c.SendChatMessage(fmt.Sprintf("Fun fact: %s", text))
}
