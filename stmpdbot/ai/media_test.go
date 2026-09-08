package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		for y := range h {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestBuildParts(t *testing.T) {
	t.Parallel()

	small := pngBytes(t, 64, 48)
	huge := pngBytes(t, 3000, 1500)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/small.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(small)
		case "/huge.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(huge)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	// t.Cleanup, not defer: the parent returns as soon as the parallel
	// subtests are queued, and defer would close the server out from under
	// every one of them.
	t.Cleanup(srv.Close)

	t.Run("a message with no attachments stays a plain string", func(t *testing.T) {
		t.Parallel()
		if parts := NewFetcher().BuildParts(context.Background(), "hello", nil); parts != nil {
			t.Errorf("BuildParts() = %v, want nil so the message travels as text", parts)
		}
	})

	t.Run("an image becomes a data URL alongside the text", func(t *testing.T) {
		t.Parallel()

		parts := NewFetcher().BuildParts(context.Background(), "what is this?", []Attachment{
			{URL: srv.URL + "/small.png", ContentType: "image/png", Filename: "small.png"},
		})
		if len(parts) != 2 {
			t.Fatalf("got %d parts, want text + image: %+v", len(parts), parts)
		}
		if parts[0].Type != "text" || parts[0].Text != "what is this?" {
			t.Errorf("first part = %+v, want the original text", parts[0])
		}
		if parts[1].Type != "image_url" || parts[1].ImageURL == nil {
			t.Fatalf("second part = %+v, want an image", parts[1])
		}
		if !strings.HasPrefix(parts[1].ImageURL.URL, "data:image/png;base64,") {
			t.Errorf("image URL = %.40q, want an inline data URL -- a remote URL is refused by the router",
				parts[1].ImageURL.URL)
		}
	})

	t.Run("an oversized image is rescaled and re-encoded", func(t *testing.T) {
		t.Parallel()

		parts := NewFetcher().BuildParts(context.Background(), "", []Attachment{
			{URL: srv.URL + "/huge.png", ContentType: "image/png", Filename: "huge.png"},
		})
		if len(parts) != 2 {
			t.Fatalf("got %d parts, want text + image", len(parts))
		}
		if !strings.HasPrefix(parts[1].ImageURL.URL, "data:image/jpeg;base64,") {
			t.Errorf("a rescaled image should come back as JPEG, got %.30q", parts[1].ImageURL.URL)
		}
		// The text part must not be empty: a bare image with no words still
		// has to carry something the model can read.
		if parts[0].Text == "" {
			t.Error("text part is empty; an image posted without words needs a stand-in prompt")
		}
	})

	t.Run("media the model cannot perceive is described instead of dropped", func(t *testing.T) {
		t.Parallel()

		parts := NewFetcher().BuildParts(context.Background(), "listen to this", []Attachment{
			{URL: srv.URL + "/voice.ogg", ContentType: "audio/ogg", Filename: "voice.ogg", DurationSecs: 14},
		})
		if len(parts) != 1 || parts[0].Type != "text" {
			t.Fatalf("got %+v, want a single text part", parts)
		}
		text := parts[0].Text
		if !strings.Contains(text, "listen to this") {
			t.Errorf("the original text was lost: %q", text)
		}
		if !strings.Contains(text, "voice message") || !strings.Contains(text, "14") {
			t.Errorf("text = %q, want it to say a 14 second voice message was posted", text)
		}
	})

	t.Run("a download failure degrades to a note, not an error", func(t *testing.T) {
		t.Parallel()

		parts := NewFetcher().BuildParts(context.Background(), "look", []Attachment{
			{URL: srv.URL + "/gone.png", ContentType: "image/png", Filename: "gone.png"},
		})
		if len(parts) != 1 || !strings.Contains(parts[0].Text, "could not be loaded") {
			t.Errorf("got %+v, want the text to admit the image did not load", parts)
		}
	})

	t.Run("the image budget is finite", func(t *testing.T) {
		t.Parallel()

		var atts []Attachment
		for range maxImages + 3 {
			atts = append(atts, Attachment{URL: srv.URL + "/small.png", ContentType: "image/png", Filename: "s.png"})
		}
		parts := NewFetcher().BuildParts(context.Background(), "", atts)

		var images int
		for _, p := range parts {
			if p.Type == "image_url" {
				images++
			}
		}
		if images != maxImages {
			t.Errorf("sent %d images, want the budget of %d", images, maxImages)
		}
	})
}

// The OpenAI schema types "content" as a string or an array, never both, so
// the two forms have to survive a round trip through the wire shape.
func TestMessageJSON(t *testing.T) {
	t.Parallel()

	t.Run("a text message marshals as a bare string", func(t *testing.T) {
		t.Parallel()

		data, err := json.Marshal(Message{Role: "user", Content: "hi"})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(data), `{"role":"user","content":"hi"}`; got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})

	t.Run("parts replace the string when present", func(t *testing.T) {
		t.Parallel()

		data, err := json.Marshal(Message{
			Role:    "user",
			Content: "ignored",
			Parts:   []Part{TextPart("look"), ImagePart("image/png", []byte{1, 2, 3})},
		})
		if err != nil {
			t.Fatal(err)
		}
		var probe struct {
			Content []map[string]any `json:"content"`
		}
		if err := json.Unmarshal(data, &probe); err != nil {
			t.Fatalf("content did not marshal as an array: %s", data)
		}
		if len(probe.Content) != 2 {
			t.Fatalf("got %d parts on the wire, want 2: %s", len(probe.Content), data)
		}
		if strings.Contains(string(data), "ignored") {
			t.Errorf("Content was sent alongside Parts; the schema allows only one: %s", data)
		}
	})

	t.Run("a reply in either form unmarshals to text", func(t *testing.T) {
		t.Parallel()

		for _, tc := range []struct{ name, raw, want string }{
			{"string", `{"role":"assistant","content":"pong"}`, "pong"},
			{"parts", `{"role":"assistant","content":[{"type":"text","text":"po"},{"type":"text","text":"ng"}]}`, "pong"},
			{"null", `{"role":"assistant","content":null}`, ""},
		} {
			var m Message
			if err := json.Unmarshal([]byte(tc.raw), &m); err != nil {
				t.Errorf("%s: %v", tc.name, err)
				continue
			}
			if m.Content != tc.want {
				t.Errorf("%s: Content = %q, want %q", tc.name, m.Content, tc.want)
			}
		}
	})

	t.Run("tool calls still survive", func(t *testing.T) {
		t.Parallel()

		raw := `{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"search_songs","arguments":"{}"}}]}`
		var m Message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		if len(m.ToolCalls) != 1 || m.ToolCalls[0].Function.Name != "search_songs" {
			t.Errorf("tool calls lost: %+v", m.ToolCalls)
		}
	})
}
