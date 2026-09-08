// Package ai is the AI persona feature's core: a chat-completions client, the
// tools it can call (catalogue, tour dates, message sampling, memory), and
// the agent loop that ties them together. It is imported only by cmd/agent
// -- the standalone service that is the feature's only home for the LLM API
// key and the tool-calling loop, kept out of the bot's own process so a
// prompt/tool/memory change redeploys independently of the bot.
//
// Removing the feature entirely: delete this package, cmd/agent, the
// stmpdbot/listeners/ai.go trigger, LLMConfig/AgentConfig from
// stmpdbot/config.go, the agent Docker/compose service, and the
// b.SetupLLM()/AIListener(b) call sites in main.go.
package ai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to an OpenAI-compatible chat completions endpoint, such as the
// cliproxy.milind.dev proxy this feature was built against.
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
	model      string
	maxTokens  int
}

func NewClient(baseURL, apiKey, model string, maxTokens int) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 60 * time.Second},
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		model:      model,
		maxTokens:  maxTokens,
	}
}

// Message is one turn in a chat-completions conversation. Content is empty on
// an assistant message that only carries ToolCalls; ToolCallID is set only on
// a role "tool" message answering one of those calls.
//
// Parts is the multimodal form of Content: when it is set it replaces Content
// on the wire, because "content" in the OpenAI schema is either a plain string
// or an array of typed parts, never both. Every existing caller that sets
// Content keeps working untouched -- see MarshalJSON.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	Parts      []Part     `json:"-"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Part is one element of a multimodal message: a run of text, or an image.
// Images travel as data: URLs rather than links -- 9router answers a request
// carrying a remote image URL with a 503, so whatever is going to look at a
// picture has to be handed the bytes.
type Part struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
}

func TextPart(text string) Part { return Part{Type: "text", Text: text} }

// ImagePart takes the raw bytes and the MIME type, never a URL.
func ImagePart(mimeType string, data []byte) Part {
	return Part{Type: "image_url", ImageURL: &ImageURL{
		URL: "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data),
	}}
}

// messageWire is Message as the API actually shapes it: "content" typed as
// any, so it can carry either form.
type messageWire struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

func (m Message) MarshalJSON() ([]byte, error) {
	w := messageWire{Role: m.Role, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
	switch {
	case len(m.Parts) > 0:
		w.Content = m.Parts
	case m.Content != "":
		w.Content = m.Content
	}
	return json.Marshal(w)
}

// UnmarshalJSON accepts both forms because a reply is not required to use the
// one the request did. Only text is kept: an assistant turn is fed back into
// the next round as history, and nothing downstream reads image parts off it.
func (m *Message) UnmarshalJSON(data []byte) error {
	var w struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCalls  []ToolCall      `json:"tool_calls"`
		ToolCallID string          `json:"tool_call_id"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	m.Role, m.ToolCalls, m.ToolCallID = w.Role, w.ToolCalls, w.ToolCallID
	m.Content, m.Parts = "", nil

	if len(w.Content) == 0 || string(w.Content) == "null" {
		return nil
	}
	if err := json.Unmarshal(w.Content, &m.Content); err == nil {
		return nil
	}
	var parts []Part
	if err := json.Unmarshal(w.Content, &parts); err != nil {
		return fmt.Errorf("ai: message content is neither a string nor parts: %w", err)
	}
	var text strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			text.WriteString(p.Text)
		}
	}
	m.Content = text.String()
	return nil
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolCallFunc `json:"function"`
}

type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool describes one function the model may call, in the standard
// OpenAI "tools" shape.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatRequest struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	Tools     []Tool    `json:"tools,omitempty"`
	MaxTokens int       `json:"max_tokens,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// ChatCompletion sends one request and returns the assistant's reply message,
// which may carry ToolCalls instead of (or alongside) Content.
func (c *Client) ChatCompletion(ctx context.Context, messages []Message, tools []Tool) (Message, error) {
	body, err := json.Marshal(chatRequest{
		Model:     c.model,
		Messages:  messages,
		Tools:     tools,
		MaxTokens: c.maxTokens,
	})
	if err != nil {
		return Message{}, fmt.Errorf("ai: failed to encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Message{}, fmt.Errorf("ai: failed to build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Message{}, fmt.Errorf("ai: request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return Message{}, fmt.Errorf("ai: failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return Message{}, fmt.Errorf("ai: chat completion returned status %d: %s", resp.StatusCode, string(data))
	}

	var out chatResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return Message{}, fmt.Errorf("ai: failed to decode response: %w", err)
	}
	if out.Error != nil {
		return Message{}, fmt.Errorf("ai: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return Message{}, errors.New("ai: response carried no choices")
	}
	return out.Choices[0].Message, nil
}
