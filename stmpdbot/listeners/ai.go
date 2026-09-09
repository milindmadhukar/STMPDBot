package listeners

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
	"github.com/milindmadhukar/STMPDBot/stmpdbot"
	"github.com/milindmadhukar/STMPDBot/utils"
)

// AIListener answers a mention or a reply to one of the bot's own messages by
// forwarding the conversation to the standalone AI persona service
// (cmd/agent), grounded on the song catalogue, tour dates and this server's
// own message history via that service's tools.
//
// This is the AI persona feature's only entry point into the rest of the
// bot: deleting this file, LLMConfig, and the b.SetupLLM()/AIListener(b)
// lines in main.go removes the trigger completely. The agent service itself
// (cmd/agent) is a separate deployable unit.
func AIListener(b *stmpdbot.STMPDBot) bot.EventListener {
	cooldowns := newCooldowns()

	return bot.NewListenerFunc(func(e *events.MessageCreate) {
		if b.AgentClient == nil {
			return
		}
		if e.Message.Author.Bot || e.Message.Author.System || e.GuildID == nil {
			return
		}
		if e.Message.MentionEveryone {
			return
		}

		isReply, ok := triggered(b, e.Message)
		if !ok {
			return
		}

		// A reply to the bot's own message is a conversation someone is
		// already in and actively waiting on -- cooldown-blocking it silently
		// dropped a message a user was staring at, which is worse than the
		// spam the cooldown exists to prevent. Only a fresh, unsolicited
		// mention is rate-limited.
		if !isReply {
			cooldown := time.Duration(b.Cfg.LLM.CooldownSeconds) * time.Second
			if cooldown <= 0 {
				cooldown = 15 * time.Second
			}
			if !cooldowns.allow(e.Message.Author.ID, cooldown) {
				slog.Debug("ai: cooldown active, skipping mention",
					slog.String("user_id", e.Message.Author.ID.String()))
				return
			}
		}

		go respond(b, e, isReply)
	})
}

// triggered reports whether message either @mentions the bot or replies to
// one of the bot's own messages, and which of the two it was.
func triggered(b *stmpdbot.STMPDBot, message discord.Message) (isReply, ok bool) {
	selfID := b.Client.ID()

	if message.ReferencedMessage != nil && message.ReferencedMessage.Author.ID == selfID {
		return true, true
	}

	for _, user := range message.Mentions {
		if user.ID == selfID {
			return false, true
		}
	}

	return false, false
}

// respond runs off the gateway goroutine: an LLM round-trip (plus its own
// tool-calling round-trips) can easily take longer than disgo's event
// dispatch should be blocked for.
func respond(b *stmpdbot.STMPDBot, e *events.MessageCreate, isReply bool) {
	start := time.Now()
	// 90s, not 60: the agent now fetches and rescales attachments before it
	// even reaches the model, and 60 was already producing the occasional
	// "context canceled" on a plain text reply.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	stopTyping := keepTyping(ctx, b, e.ChannelID)
	defer stopTyping()

	conversation := buildConversation(ctx, b, e)

	content, err := b.AgentClient.Respond(ctx, int64(*e.GuildID), int64(e.Message.Author.ID), conversation)
	if err != nil {
		slog.Error("ai: failed to generate a response",
			slog.String("user_id", e.Message.Author.ID.String()),
			slog.Bool("is_reply", isReply), slog.Any("err", err))
		return
	}
	if content == "" {
		slog.Warn("ai: model returned an empty reply",
			slog.String("user_id", e.Message.Author.ID.String()), slog.Bool("is_reply", isReply))
		return
	}

	utils.ReplyToMessage(b.Client, e.ChannelID, e.Message, content)
	slog.Info("ai: replied",
		slog.String("user_id", e.Message.Author.ID.String()),
		slog.Bool("is_reply", isReply), slog.Duration("took", time.Since(start)))
}

// keepTyping holds the typing indicator up for as long as the reply takes.
//
// Discord expires it after about ten seconds and expects the bot to re-assert
// it, so the single call this used to make showed "typing..." for a moment and
// then left the channel looking idle -- while a memory search, a tool loop and
// a model round-trip were still running, which together routinely outlast a
// minute. The person asking sees nothing and assumes it broke.
//
// Returns a stop function; call it when the reply is sent, or on any path that
// gives up.
func keepTyping(ctx context.Context, b *stmpdbot.STMPDBot, channelID snowflake.ID) func() {
	send := func() {
		if err := b.Client.Rest.SendTyping(channelID); err != nil {
			slog.Debug("ai: failed to send typing indicator", slog.Any("err", err))
		}
	}
	send()

	done := make(chan struct{})
	go func() {
		// Comfortably inside Discord's ~10s expiry, so the indicator never
		// visibly flickers off between refreshes.
		ticker := time.NewTicker(7 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				send()
			}
		}
	}()

	return sync.OnceFunc(func() { close(done) })
}

// buildConversation walks the Discord reply chain backwards from the
// triggering message, turning it into the conversation sent to the agent
// service -- no system prompt here, the agent service owns its own identity,
// persona and memory and assembles that itself. Nothing about a conversation
// is ever stored on the bot's side -- it is reconstructed from Discord's own
// reply references every time the bot is pinged.
func buildConversation(ctx context.Context, b *stmpdbot.STMPDBot, e *events.MessageCreate) []utils.AgentMessage {
	maxHops := b.Cfg.LLM.MaxContextMessages
	if maxHops <= 0 {
		maxHops = 6
	}
	selfID := b.Client.ID()

	type turn struct {
		role    string
		content string
		media   []utils.AgentAttachment
	}
	var chain []turn

	current := e.Message
	for i := 0; i < maxHops; i++ {
		ref := current.MessageReference
		if ref == nil || ref.MessageID == nil {
			break
		}

		refMsg := current.ReferencedMessage
		if refMsg == nil {
			fetched, err := b.Client.Rest.GetMessage(e.ChannelID, *ref.MessageID, rest.WithCtx(ctx))
			if err != nil {
				break
			}
			refMsg = fetched
		}
		media := mediaOf(*refMsg)
		// A message with no text at all used to end the walk. That silently
		// truncated every chain containing a bare screenshot or reaction GIF
		// -- the most common kind of image message there is. Only a genuinely
		// empty message stops it now.
		if refMsg.Content == "" && len(media) == 0 {
			break
		}

		role := "user"
		if refMsg.Author.ID == selfID {
			role = "assistant"
		}
		content := renderEmoji(resolveMentions(ctx, b, *e.GuildID, refMsg.Content, refMsg.Mentions))
		chain = append(chain, turn{role: role, content: content, media: media})
		current = *refMsg
	}

	messages := make([]utils.AgentMessage, 0, len(chain)+1)
	for i := len(chain) - 1; i >= 0; i-- {
		messages = append(messages, utils.AgentMessage{
			Role:        chain[i].role,
			Content:     chain[i].content,
			Attachments: chain[i].media,
		})
	}
	messages = append(messages, utils.AgentMessage{
		Role:        "user",
		Content:     renderEmoji(resolveMentions(ctx, b, *e.GuildID, e.Message.Content, e.Message.Mentions)),
		Attachments: mediaOf(e.Message),
	})
	return messages
}

var mentionPattern = regexp.MustCompile(`<@!?(\d+)>`)

// emojiPattern matches a custom emoji, animated (<a:name:id>) or not.
var emojiPattern = regexp.MustCompile(`<a?:([A-Za-z0-9_]+):\d+>`)

// renderEmoji turns <:pepeSTMPD:12345> into :pepeSTMPD:. The image itself is
// not worth a vision round-trip -- an emoji is used for its name far more
// than its picture -- but the raw token is unreadable, and a message that is
// nothing but emoji otherwise arrives as a wall of snowflakes.
func renderEmoji(content string) string {
	if !strings.Contains(content, ":") {
		return content
	}
	return emojiPattern.ReplaceAllString(content, ":$1:")
}

// resolveMentions replaces Discord's raw <@id> mention syntax with a readable
// display name, so the model sees "what do you think about Sourav?" instead
// of an opaque snowflake it has no way to identify. Prefers the guild
// nickname over the global display name over the bare username, same
// precedence Discord's own client uses to show a mention.
func resolveMentions(ctx context.Context, b *stmpdbot.STMPDBot, guildID snowflake.ID, content string, mentions []discord.User) string {
	if len(mentions) == 0 || !strings.Contains(content, "<@") {
		return content
	}

	names := make(map[snowflake.ID]string, len(mentions))
	for _, u := range mentions {
		names[u.ID] = displayName(ctx, b, guildID, u)
	}

	return mentionPattern.ReplaceAllStringFunc(content, func(token string) string {
		id, err := snowflake.Parse(mentionPattern.FindStringSubmatch(token)[1])
		if err != nil {
			return token
		}
		if name, ok := names[id]; ok {
			return "@" + name
		}
		return token
	})
}

// displayName tries the member cache first, which only holds members disgo
// has actually seen a gateway event for -- join, update, voice state,
// presence. A message mentioning someone who is otherwise quiet (the exact
// "Sourav (WEIGHTLESS Ambassador)" case) is a routine cache miss, not an
// edge case, so a REST lookup is the fallback rather than an afterthought.
func displayName(ctx context.Context, b *stmpdbot.STMPDBot, guildID snowflake.ID, u discord.User) string {
	if member, ok := b.Client.Caches.Member(guildID, u.ID); ok {
		if member.Nick != nil && *member.Nick != "" {
			return *member.Nick
		}
	} else if member, err := b.Client.Rest.GetMember(guildID, u.ID, rest.WithCtx(ctx)); err == nil {
		if member.Nick != nil && *member.Nick != "" {
			return *member.Nick
		}
	}
	if u.GlobalName != nil && *u.GlobalName != "" {
		return *u.GlobalName
	}
	return u.Username
}

// cooldowns is a simple per-user in-memory rate limit, kept out of the
// database on purpose: this feature is stateless and meant to be removed
// entirely once the experiment is over.
type cooldowns struct {
	mu   sync.Mutex
	last map[snowflake.ID]time.Time
}

func newCooldowns() *cooldowns {
	return &cooldowns{last: make(map[snowflake.ID]time.Time)}
}

func (c *cooldowns) allow(userID snowflake.ID, cooldown time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if last, ok := c.last[userID]; ok && now.Sub(last) < cooldown {
		return false
	}
	c.last[userID] = now
	return true
}
