package main

import (
	"context"
	"encoding/json"
	"errors"
)

func operatorTools(store *playerStore, disconnect context.CancelFunc) []toolSpec {
	return []toolSpec{
		makeTool("disconnect_bot", "Stop this bot and disconnect from Minecraft without automatically reconnecting. Requires an authenticated admin requester; the Discord bridge cannot authorize shutdown.", nil, nil, func(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
			if err := decodeArgs(raw, &struct{}{}); err != nil {
				return nil, err
			}
			if (actor != consoleActor && !validUsername(actor)) || store == nil || !store.hasRank(actor, "admin") {
				return nil, errors.New("disconnecting the bot requires an authenticated admin requester")
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if disconnect == nil {
				return nil, errors.New("bot shutdown is unavailable")
			}
			disconnect()
			return map[string]any{"status": "shutdown_requested", "reconnect": false}, nil
		}),
	}
}
