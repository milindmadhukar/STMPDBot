package ai

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// This file gives the persona eyes. A Discord message is not only text: people
// answer each other with a screenshot, a meme, a GIF reaction, a sticker, a
// voice note. Until now every one of those arrived as an empty string and the
// model replied to nothing, which reads as either evasive or stupid.
//
// The bot forwards attachment metadata only. Fetching and converting happens
// here, in the agent service, for the same reason the API key does: the agent
// is what knows the model, and what the model can be handed is a property of
// the model, not of Discord.

const (
	// maxDownloadBytes caps a single fetch. Discord allows far larger uploads;
	// anything past this is described rather than looked at, because the point
	// of the cap is the request body we then build, not the download.
	maxDownloadBytes = 12 << 20

	// maxImageDimension is the longest side an image is scaled down to before
	// being sent. Vision models bill by tiles, and a 4K phone screenshot costs
	// several times a 1024px one while answering exactly the same question.
	maxImageDimension = 1024

	// maxImages bounds one conversation. A reply chain full of screenshots
	// would otherwise turn a single mention into a very expensive request.
	maxImages = 6

	// jpegQuality is what a downscaled image is re-encoded at.
	jpegQuality = 85
)

// Attachment is one piece of media hanging off a Discord message, as the bot
// describes it. It is deliberately Discord-shaped rather than model-shaped:
// deciding what a model can do with it is this package's job.
type Attachment struct {
	URL          string  `json:"url"`
	ContentType  string  `json:"content_type,omitempty"`
	Filename     string  `json:"filename,omitempty"`
	Size         int     `json:"size,omitempty"`
	Description  string  `json:"description,omitempty"`
	DurationSecs float64 `json:"duration_secs,omitempty"`
	Width        int     `json:"width,omitempty"`
	Height       int     `json:"height,omitempty"`
	// Kind labels media that is not a file upload -- "sticker", "embed" --
	// so the note the model reads can say which it was.
	Kind string `json:"kind,omitempty"`
}

// Fetcher downloads attachments. It is separate from Client's HTTP client
// because the two have nothing in common: this one talks to Discord's CDN,
// follows redirects, and wants a much shorter leash than a model round-trip.
type Fetcher struct {
	httpClient *http.Client
	budget     int
}

func NewFetcher() *Fetcher {
	return &Fetcher{
		httpClient: &http.Client{Timeout: 20 * time.Second},
		budget:     maxImages,
	}
}

// BuildParts turns the text of a message plus its attachments into the parts
// of one multimodal message. It returns nil when there is nothing but text,
// so an ordinary message keeps travelling as an ordinary string.
//
// Failure is never fatal here. An attachment that will not download, will not
// decode, or is not something the model can see becomes a line of text saying
// what it was -- being told "a 14 second voice message" is far closer to what
// a human in the channel perceives than being told nothing at all.
func (f *Fetcher) BuildParts(ctx context.Context, text string, attachments []Attachment) []Part {
	if len(attachments) == 0 {
		return nil
	}

	var (
		images []Part
		notes  []string
	)

	for _, a := range attachments {
		if !isImage(a) {
			notes = append(notes, describe(a))
			continue
		}
		if f.budget <= 0 {
			notes = append(notes, describe(a)+" (not shown: too many images in this conversation)")
			continue
		}

		data, mime, err := f.image(ctx, a)
		if err != nil {
			slog.Warn("ai: could not prepare an image for the model",
				slog.String("filename", a.Filename), slog.String("content_type", a.ContentType),
				slog.Any("err", err))
			notes = append(notes, describe(a)+" (could not be loaded)")
			continue
		}

		f.budget--
		if caption := caption(a); caption != "" {
			notes = append(notes, caption)
		}
		images = append(images, ImagePart(mime, data))
	}

	if len(images) == 0 && len(notes) == 0 {
		return nil
	}

	body := strings.TrimSpace(text)
	if len(notes) > 0 {
		if body != "" {
			body += "\n"
		}
		body += "[" + strings.Join(notes, "; ") + "]"
	}
	if body == "" {
		// An image posted with no words at all. Something has to occupy the
		// text part, and silence is not a prompt.
		body = "(no text, see the attached image)"
	}

	return append([]Part{TextPart(body)}, images...)
}

// image downloads and re-encodes one attachment into something small enough
// to send. An animated GIF collapses to its first frame -- that is all a
// still-image model ever sees of one, and saying so in the note is more
// honest than pretending it watched the animation.
func (f *Fetcher) image(ctx context.Context, a Attachment) ([]byte, string, error) {
	raw, err := f.download(ctx, a.URL)
	if err != nil {
		return nil, "", err
	}

	img, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", fmt.Errorf("decode: %w", err)
	}

	scaled := downscale(img)

	// An unscaled PNG or JPEG can go up untouched: re-encoding it would only
	// cost quality and CPU to produce a file of much the same size.
	if scaled == nil && (format == "jpeg" || format == "png") && len(raw) <= maxDownloadBytes {
		return raw, "image/" + format, nil
	}
	if scaled == nil {
		scaled = img
	}

	var out bytes.Buffer
	if err := jpeg.Encode(&out, scaled, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, "", fmt.Errorf("encode: %w", err)
	}
	return out.Bytes(), "image/jpeg", nil
}

func (f *Fetcher) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := f.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	// LimitReader with one byte of headroom, so a file that is exactly at the
	// cap is told apart from one that ran past it.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDownloadBytes {
		return nil, fmt.Errorf("larger than the %d MB limit", maxDownloadBytes>>20)
	}
	return data, nil
}

// downscale returns nil when the image is already small enough, which is the
// signal to leave the original bytes alone.
func downscale(img image.Image) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxImageDimension && h <= maxImageDimension {
		return nil
	}

	if w > h {
		h = h * maxImageDimension / w
		w = maxImageDimension
	} else {
		w = w * maxImageDimension / h
		h = maxImageDimension
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
	return dst
}

// isImage reports whether the model can actually look at this. The content
// type is Discord's, and it is occasionally absent, so the filename is the
// fallback rather than a trusted source.
func isImage(a Attachment) bool {
	switch strings.ToLower(strings.TrimSpace(a.ContentType)) {
	case "image/png", "image/jpeg", "image/jpg", "image/webp", "image/gif":
		return true
	}
	if a.ContentType != "" {
		return false
	}
	name := strings.ToLower(a.Filename)
	for _, ext := range []string{".png", ".jpg", ".jpeg", ".webp", ".gif"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// caption is what gets said about an image that IS being shown. Alt text is
// worth passing on -- someone who writes it is telling you what matters --
// and a GIF is flagged because only its first frame is going up.
func caption(a Attachment) string {
	var bits []string
	if a.Kind == "sticker" {
		bits = append(bits, "sticker "+strings.TrimSuffix(a.Filename, ".png"))
	}
	if strings.Contains(strings.ToLower(a.ContentType), "gif") || strings.HasSuffix(strings.ToLower(a.Filename), ".gif") {
		bits = append(bits, "an animated GIF, first frame shown")
	}
	if a.Description != "" {
		bits = append(bits, "described by the sender as "+quote(a.Description))
	}
	return strings.Join(bits, ", ")
}

// describe is the fallback for everything the model cannot perceive: audio,
// video, PDFs, archives. It exists so the reply can acknowledge the thing
// instead of ignoring it -- a person who cannot open a .zip still knows one
// was posted.
func describe(a Attachment) string {
	name := a.Filename
	if name == "" {
		name = "a file"
	}

	kind := a.ContentType
	switch {
	case a.DurationSecs > 0 && strings.HasPrefix(kind, "audio/"):
		return fmt.Sprintf("a voice message, %.0f seconds long, which cannot be listened to", a.DurationSecs)
	case strings.HasPrefix(kind, "audio/"):
		return fmt.Sprintf("an audio file %s, which cannot be listened to", quote(name))
	case strings.HasPrefix(kind, "video/"):
		return fmt.Sprintf("a video %s, which cannot be watched", quote(name))
	case kind != "":
		return fmt.Sprintf("a %s file %s", kind, quote(name))
	default:
		return fmt.Sprintf("a file %s", quote(name))
	}
}

func quote(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return `"` + s + `"`
}
