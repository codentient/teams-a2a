package teams

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/codentient/teams-a2a/internal/bridge"
)

// maxMessageChars keeps each Teams message comfortably under the ~28 KB activity limit.
const maxMessageChars = 20000

// SinkOptions configure one turn's rendering into a Teams conversation.
type SinkOptions struct {
	Connector      *Connector
	ServiceURL     string
	ConversationID string
	// ReplyToID is the inbound activity id; replies thread under it in channels.
	ReplyToID string
	// Stream enables native streaming (Teams supports it in 1:1 chats only).
	Stream        bool
	Interval      time.Duration
	AILabel       bool
	RequesterOnly bool
	Logger        *slog.Logger
}

// Sink implements bridge.Sink for Teams. Progress and Partial calls are coalesced and sent
// from a background loop at most once per Interval, so a fast token stream never trips
// Teams' streaming throttle and never blocks the A2A event stream.
type Sink struct {
	o SinkOptions

	mu          sync.Mutex
	started     bool
	stopped     bool
	wake        chan struct{}
	stop        chan struct{}
	done        chan struct{}
	info        string
	text        string
	sentInfo    string
	sentText    string
	hasContent  bool
	streamID    string
	seq         int
	broken      bool
	lastTyping  time.Time
	bgCtx       context.Context
	cancelBgCtx context.CancelFunc
}

func NewSink(o SinkOptions) *Sink {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Interval < time.Second {
		o.Interval = 1500 * time.Millisecond
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Sink{
		o: o, wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
		bgCtx: ctx, cancelBgCtx: cancel,
	}
}

var _ bridge.Sink = (*Sink)(nil)

func (s *Sink) kick() {
	if !s.started {
		s.started = true
		go s.loop()
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Sink) Progress(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.hasContent {
		return // Teams ignores informative updates once content is streaming
	}
	s.info = text
	s.kick()
}

func (s *Sink) Partial(full string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.text = full
	if strings.TrimSpace(full) != "" {
		s.hasContent = true
	}
	s.kick()
}

func (s *Sink) loop() {
	defer close(s.done)
	for {
		select {
		case <-s.stop:
			return
		case <-s.wake:
		}
		s.flush()
		select {
		case <-s.stop:
			return
		case <-time.After(s.o.Interval):
		}
	}
}

func (s *Sink) flush() {
	s.mu.Lock()
	info, text, broken := s.info, s.text, s.broken
	s.mu.Unlock()
	if broken {
		return
	}
	if !s.o.Stream {
		// No native streaming here (group chat or channel): a typing indicator is the
		// only signal Teams offers. It lasts a few seconds, so refresh it while working.
		if time.Since(s.lastTyping) >= 3*time.Second {
			s.lastTyping = time.Now()
			_, _ = s.o.Connector.SendToConversation(s.bgCtx, s.o.ServiceURL, s.o.ConversationID, &Activity{Type: ActivityTyping})
		}
		return
	}
	streamType, body := "", ""
	switch {
	case strings.TrimSpace(text) != "" && text != s.sentText:
		if utf8.RuneCountInString(text) > maxMessageChars {
			return // too long to stream; the final message carries it in parts
		}
		streamType, body = "streaming", text
	case strings.TrimSpace(text) == "" && info != "" && info != s.sentInfo:
		streamType, body = "informative", info
	default:
		return
	}
	s.seq++
	act := &Activity{
		Type:      ActivityTyping,
		Text:      body,
		ReplyToID: s.o.ReplyToID,
		Entities:  []json.RawMessage{streamInfoEntity(s.streamID, streamType, s.seq)},
	}
	id, err := s.o.Connector.SendToConversation(s.bgCtx, s.o.ServiceURL, s.o.ConversationID, act)
	if err != nil {
		s.o.Logger.Warn("teams streaming update failed; falling back to a single message", "err", err)
		s.mu.Lock()
		s.broken = true
		s.mu.Unlock()
		return
	}
	if s.streamID == "" {
		s.streamID = id
	}
	if streamType == "streaming" {
		s.sentText = body
	} else {
		s.sentInfo = body
	}
}

// halt stops the background loop and waits for any in-flight update to finish, so the
// final message is always the last thing Teams receives for this stream.
func (s *Sink) halt() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	started := s.started
	s.mu.Unlock()
	close(s.stop)
	if started {
		<-s.done
	}
	s.cancelBgCtx()
}

func splitText(text string, max int) []string {
	var out []string
	for utf8.RuneCountInString(text) > max {
		r := []rune(text)
		cut := max
		// Prefer to break at a paragraph or line boundary.
		if i := strings.LastIndex(string(r[:max]), "\n\n"); i > 0 {
			cut = utf8.RuneCountInString(string(r[:max])[:i])
		} else if i := strings.LastIndex(string(r[:max]), "\n"); i > 0 {
			cut = utf8.RuneCountInString(string(r[:max])[:i])
		}
		out = append(out, strings.TrimSpace(string(r[:cut])))
		text = strings.TrimSpace(string(r[cut:]))
	}
	if text != "" || len(out) == 0 {
		out = append(out, text)
	}
	return out
}

// Final closes the stream (if any) and delivers the complete answer.
func (s *Sink) Final(ctx context.Context, r bridge.Reply) error {
	s.halt()
	streamOpen := s.streamID != "" && !s.broken

	var attachments []Attachment
	for _, c := range r.Cards {
		attachments = append(attachments, Attachment{ContentType: ContentTypeAdaptiveCard, Content: c})
	}
	for _, img := range r.Images {
		attachments = append(attachments, Attachment{ContentType: img.MediaType, ContentURL: img.DataURL, Name: img.Name})
	}
	text := r.Text
	if strings.TrimSpace(text) == "" && len(attachments) == 0 {
		if !streamOpen {
			return nil
		}
		text = "_Waiting for your input…_" // a stream must be closed with some text
	}

	chunks := splitText(text, maxMessageChars)
	for i, chunk := range chunks {
		act := MessageActivity(chunk)
		if i == len(chunks)-1 {
			act.Attachments = attachments
		}
		if s.o.AILabel && !r.IsError {
			act.Entities = append(act.Entities, aiGeneratedEntity())
		}
		if i == 0 && streamOpen {
			final := *act
			final.ReplyToID = s.o.ReplyToID
			final.Entities = append(append([]json.RawMessage{}, act.Entities...), streamInfoEntity(s.streamID, "final", 0))
			if _, err := s.o.Connector.SendToConversation(ctx, s.o.ServiceURL, s.o.ConversationID, &final); err == nil {
				continue
			} else {
				s.o.Logger.Warn("closing teams stream failed; sending a plain message", "err", err)
			}
		}
		if _, err := s.o.Connector.ReplyToActivity(ctx, s.o.ServiceURL, s.o.ConversationID, s.o.ReplyToID, act); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sink) Notice(ctx context.Context, text string) error {
	_, err := s.o.Connector.ReplyToActivity(ctx, s.o.ServiceURL, s.o.ConversationID, s.o.ReplyToID, MessageActivity(text))
	return err
}

func (s *Sink) Prompt(ctx context.Context, p bridge.Prompt) (string, bool, error) {
	c, complete := PromptCard(p, s.o.RequesterOnly)
	ref, err := s.o.Connector.ReplyToActivity(ctx, s.o.ServiceURL, s.o.ConversationID, s.o.ReplyToID, AdaptiveCardActivity(c))
	return ref, complete, err
}

func (s *Sink) Retire(ctx context.Context, ref, summary string) error {
	return s.o.Connector.UpdateActivity(ctx, s.o.ServiceURL, s.o.ConversationID, ref, AdaptiveCardActivity(SummaryCard(summary)))
}
