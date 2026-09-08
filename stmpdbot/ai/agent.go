package ai

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	db "github.com/milindmadhukar/STMPDBot/db/sqlc"
)

// maxToolRounds bounds how many tool round-trips one triggered response can
// spend before it is forced to answer in text. Each round is one completion
// call, so this is also a hard cap on the cost of a single Discord ping.
//
// Raised from 3 once the agent moved into its own process (cmd/agent): a
// deep tool chain here no longer risks starving the bot's gateway dispatch
// or its 60s per-message context, since this now runs in an isolated HTTP
// handler with nothing else waiting on it.
const maxToolRounds = 8

// Respond runs the tool-calling loop and returns the assistant's final text
// reply. history must already open with a system message -- normally
// SystemPrompt() plus LoadMemoryContext's output -- followed by the
// reconstructed conversation and the triggering message. userID is who
// triggered this conversation; it scopes the remember/forget tools.
func (c *Client) Respond(ctx context.Context, queries *db.Queries, mem *Memory, guildID, userID int64, history []Message) (string, error) {
	messages := append([]Message(nil), history...)

	for round := 0; round <= maxToolRounds; round++ {
		tools := Tools()
		if round == maxToolRounds {
			// Force a text-only answer: no tools offered, so there is
			// nothing left for the model to call.
			tools = nil
		}

		reply, err := c.ChatCompletion(ctx, messages, tools)
		if err != nil {
			return "", err
		}

		if len(reply.ToolCalls) == 0 {
			return reply.Content, nil
		}

		messages = append(messages, reply)
		for _, call := range reply.ToolCalls {
			result, err := Dispatch(ctx, queries, mem, guildID, userID, call.Function.Name, call.Function.Arguments)
			if err != nil {
				slog.Warn("ai: tool call failed",
					slog.String("tool", call.Function.Name), slog.Any("err", err))
				result = fmt.Sprintf(`{"error": %q}`, err.Error())
			}
			messages = append(messages, Message{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    result,
			})
		}
	}

	return "", errors.New("ai: model kept calling tools past the round limit")
}
