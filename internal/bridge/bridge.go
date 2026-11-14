// Package bridge runs one chat turn against an A2A agent. It is channel agnostic: a channel
// adapter (Teams today) turns inbound traffic into a Turn and renders output through a Sink.
package bridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/codentient/teams-a2a/internal/identity"
	"github.com/codentient/teams-a2a/internal/store"
)

// SubmitKey marks card submissions that belong to the gateway.
const SubmitKey = "teamsA2A"

// Turn is one inbound user action.
type Turn struct {
	// SessionKey identifies the conversation scope (chat, thread, or user within a chat).
	SessionKey string
	User       identity.User
	Text       string
	// Submission holds Adaptive Card input values when the user pressed a card button.
	Submission map[string]any
	// Metadata is channel context forwarded to the agent when enabled.
	Metadata map[string]any
}

// Prompt asks the user for a human-in-the-loop decision.
type Prompt struct {
	Kind          string // store.PendingToolApproval or store.PendingAskUser
	Nonce         string
	Hint          string
	Subagent      string
	Tools         []PromptTool
	Questions     []store.Question
	RequesterName string
	// OwnerID is the user whose session holds the request; channels echo it back on
	// submission so a click by someone else resolves to the right session.
	OwnerID string
}

type PromptTool struct {
	Name string
	Args map[string]any
}

// Sink renders a turn into a channel. Progress and Partial must not block for long:
// implementations buffer and pace them.
type Sink interface {
	Progress(text string)
	Partial(fullText string)
	Final(ctx context.Context, r Reply) error
	Notice(ctx context.Context, text string) error
	Prompt(ctx context.Context, p Prompt) (ref string, err error)
	Retire(ctx context.Context, ref, summary string) error
}

// AgentAPI is the subset of *agent.Agent used by the bridge.
type AgentAPI interface {
	Card(ctx context.Context) (*a2a.AgentCard, error)
	HITL(card *a2a.AgentCard) bool
	Stream(ctx context.Context, msg *a2a.Message) iter.Seq2[a2a.Event, error]
	GetTask(ctx context.Context, id string) (*a2a.Task, error)
	Cancel(ctx context.Context, id string) error
}

type Options struct {
	Store            store.Store
	Agent            AgentAPI
	TurnTimeout      time.Duration
	ShowToolActivity bool
	ApprovalPolicy   string // "requester" or "anyone"
	ForwardMetadata  bool
	PollInterval     time.Duration
	Logger           *slog.Logger
	// ObserveTurn, if set, receives the outcome and duration of every turn.
	ObserveTurn func(outcome string, total, firstOutput time.Duration)
}

type Bridge struct{ o Options }

func New(o Options) *Bridge {
	if o.PollInterval == 0 {
		o.PollInterval = 2 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Bridge{o: o}
}

const helpText = "I relay your messages to an AI agent.\n\n" +
	"- **/new**: start a fresh conversation with the agent\n" +
	"- **/cancel**: stop the request that is currently running\n" +
	"- **/help**: show this message"

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func lockKey(sessionKey string) string { return "turn:" + sessionKey }

// Handle runs one turn to completion. Errors are reported to the user through the sink;
// the returned error is for logging.
func (b *Bridge) Handle(ctx context.Context, t Turn, sink Sink) error {
	ctx = identity.With(ctx, t.User)
	log := b.o.Logger.With("session", short(t.SessionKey))

	cmd := strings.ToLower(strings.TrimSpace(t.Text))
	if t.Submission == nil {
		switch cmd {
		case "/help", "help":
			return sink.Notice(ctx, helpText)
		case "/cancel":
			return b.cancel(ctx, t, sink)
		}
	}

	owner := newID()
	ok, err := b.o.Store.Acquire(ctx, lockKey(t.SessionKey), owner, b.o.TurnTimeout+time.Minute)
	if err != nil {
		_ = sink.Notice(ctx, "Sorry, I can't process messages right now. Please try again shortly.")
		return fmt.Errorf("acquire turn lock: %w", err)
	}
	if !ok {
		return sink.Notice(ctx, "I'm still working on your previous message. Send **/cancel** to stop it.")
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = b.o.Store.Release(rctx, lockKey(t.SessionKey), owner)
	}()

	sess, err := b.o.Store.GetSession(ctx, t.SessionKey)
	if err != nil {
		_ = sink.Notice(ctx, "Sorry, I can't process messages right now. Please try again shortly.")
		return fmt.Errorf("load session: %w", err)
	}
	if sess == nil {
		sess = &store.Session{}
	}

	if t.Submission == nil && (cmd == "/new" || cmd == "/reset") {
		if sess.Pending != nil && sess.Pending.CardRef != "" {
			_ = sink.Retire(ctx, sess.Pending.CardRef, "_This request was abandoned when a new conversation started._")
		}
		if err := b.o.Store.DeleteSession(ctx, t.SessionKey); err != nil {
			return err
		}
		return sink.Notice(ctx, "Started a new conversation. The agent won't remember earlier messages.")
	}

	prev := sess.Pending
	msg, onAccepted, err := b.buildMessage(ctx, t, sess, sink)
	if err != nil || msg == nil {
		return err
	}
	if b.o.ForwardMetadata && len(t.Metadata) > 0 {
		if msg.Metadata == nil {
			msg.Metadata = map[string]any{}
		}
		for k, v := range t.Metadata {
			msg.Metadata[k] = v
		}
	}
	return b.run(ctx, log, t, sess, msg, sink, prev, onAccepted)
}

func (b *Bridge) cancel(ctx context.Context, t Turn, sink Sink) error {
	sess, err := b.o.Store.GetSession(ctx, t.SessionKey)
	if err != nil {
		return err
	}
	if sess == nil || sess.RunningTaskID == "" {
		return sink.Notice(ctx, "There's nothing running to cancel.")
	}
	if err := b.o.Agent.Cancel(ctx, sess.RunningTaskID); err != nil {
		_ = sink.Notice(ctx, "I couldn't cancel the running request.")
		return fmt.Errorf("cancel task: %w", err)
	}
	return sink.Notice(ctx, "Cancelling the running request…")
}

func (b *Bridge) mayAnswer(t Turn, p *store.Pending) bool {
	return b.o.ApprovalPolicy == "anyone" || p.RequesterID == "" || p.RequesterID == t.User.ID
}

// buildMessage decides what to send to the agent. A nil message with nil error means the
// turn was fully handled (for example by asking the user to use the card). onAccepted, if
// not nil, must run once the agent has accepted the message.
func (b *Bridge) buildMessage(ctx context.Context, t Turn, sess *store.Session, sink Sink) (msg *a2a.Message, onAccepted func(), err error) {
	p := sess.Pending
	if t.Submission != nil {
		return b.fromSubmission(ctx, t, sess, sink)
	}
	if p == nil {
		m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(t.Text))
		m.ContextID = sess.ContextID
		return m, nil, nil
	}
	if !b.mayAnswer(t, p) {
		return nil, nil, sink.Notice(ctx, "The agent is waiting for an answer from the person who started this request.")
	}
	switch p.Kind {
	case store.PendingToolApproval:
		switch strings.ToLower(strings.Trim(strings.TrimSpace(t.Text), ".!")) {
		case "approve", "approved", "yes", "y", "ok":
			return b.resolveApproval(ctx, t, sess, sink, true, "")
		case "reject", "rejected", "deny", "no", "n":
			return b.resolveApproval(ctx, t, sess, sink, false, "")
		}
		return nil, nil, sink.Notice(ctx, "The agent is waiting for your approval. Use the buttons on the card, or reply **approve** or **reject**.")
	case store.PendingAskUser:
		if len(p.Questions) != 1 || p.Questions[0].Multiple {
			return nil, nil, sink.Notice(ctx, "Please answer the agent's questions on the card above.")
		}
		answer := strings.TrimSpace(t.Text)
		if choices := p.Questions[0].Choices; len(choices) > 0 {
			i := slices.IndexFunc(choices, func(c string) bool { return strings.EqualFold(c, answer) })
			if i < 0 {
				return nil, nil, sink.Notice(ctx, "Please pick one of the choices on the card above.")
			}
			answer = choices[i]
		}
		return b.resolveAnswers(ctx, t, sess, sink, [][]string{{answer}})
	default:
		m := a2a.NewMessageForTask(a2a.MessageRoleUser, a2a.TaskInfo{TaskID: a2a.TaskID(p.TaskID), ContextID: p.ContextID}, a2a.NewTextPart(t.Text))
		sess.Pending = nil
		return m, nil, nil
	}
}

func (b *Bridge) fromSubmission(ctx context.Context, t Turn, sess *store.Session, sink Sink) (*a2a.Message, func(), error) {
	meta, _ := t.Submission[SubmitKey].(map[string]any)
	nonce, _ := meta["nonce"].(string)
	action, _ := meta["action"].(string)
	p := sess.Pending
	if p == nil || nonce == "" || nonce != p.Nonce {
		return nil, nil, sink.Notice(ctx, "That request is no longer waiting for an answer.")
	}
	if !b.mayAnswer(t, p) {
		return nil, nil, sink.Notice(ctx, "Only the person who started this request can answer it.")
	}
	switch {
	case p.Kind == store.PendingToolApproval && (action == "approve" || action == "reject"):
		reason, _ := t.Submission["reason"].(string)
		return b.resolveApproval(ctx, t, sess, sink, action == "approve", strings.TrimSpace(reason))
	case p.Kind == store.PendingAskUser && action == "answer":
		answers := make([][]string, len(p.Questions))
		for i, q := range p.Questions {
			raw, _ := t.Submission[fmt.Sprintf("q%d", i)].(string)
			var vals []string
			if len(q.Choices) > 0 {
				// Cards submit choice indexes; anything else is rejected rather than forwarded.
				for _, v := range strings.Split(raw, ",") {
					v = strings.TrimSpace(v)
					if v == "" {
						continue
					}
					idx, err := strconv.Atoi(v)
					if err != nil || idx < 0 || idx >= len(q.Choices) {
						return nil, nil, sink.Notice(ctx, "That answer isn't one of the offered choices.")
					}
					vals = append(vals, q.Choices[idx])
				}
				if !q.Multiple && len(vals) > 1 {
					vals = vals[:1]
				}
			} else if v := strings.TrimSpace(raw); v != "" {
				vals = []string{v}
			}
			if len(vals) == 0 {
				return nil, nil, sink.Notice(ctx, "Please answer every question before submitting.")
			}
			answers[i] = vals
		}
		return b.resolveAnswers(ctx, t, sess, sink, answers)
	}
	return nil, nil, sink.Notice(ctx, "That action doesn't match what the agent is waiting for.")
}

func (b *Bridge) resolveApproval(ctx context.Context, t Turn, sess *store.Session, sink Sink, approved bool, reason string) (*a2a.Message, func(), error) {
	p := sess.Pending
	text, summary := "Approved.", fmt.Sprintf("✅ **Approved** by %s", nameOr(t.User.Name))
	if !approved {
		text, summary = "Rejected.", fmt.Sprintf("❌ **Rejected** by %s", nameOr(t.User.Name))
		if reason != "" {
			text += " Reason: " + reason
			summary += ": " + reason
		}
	}
	m := toolApprovalMessage(p.TaskID, p.ContextID, p.ToolIDs, approved, reason, text)
	sess.Pending = nil
	return m, retire(ctx, sink, p.CardRef, summary+"\n\n"+toolList(p.Tools)), nil
}

func (b *Bridge) resolveAnswers(ctx context.Context, t Turn, sess *store.Session, sink Sink, answers [][]string) (*a2a.Message, func(), error) {
	p := sess.Pending
	var lines []string
	for i, q := range p.Questions {
		lines = append(lines, fmt.Sprintf("- %s **%s**", q.Text, strings.Join(answers[i], ", ")))
	}
	var flat []string
	for _, a := range answers {
		flat = append(flat, strings.Join(a, ", "))
	}
	m := askUserMessage(p.TaskID, p.ContextID, p.RequestID, answers, strings.Join(flat, "; "))
	sess.Pending = nil
	return m, retire(ctx, sink, p.CardRef, fmt.Sprintf("Answered by %s:\n\n%s", nameOr(t.User.Name), strings.Join(lines, "\n"))), nil
}

func retire(ctx context.Context, sink Sink, ref, summary string) func() {
	return func() {
		if ref != "" {
			_ = sink.Retire(ctx, ref, summary)
		}
	}
}

func (b *Bridge) run(ctx context.Context, log *slog.Logger, t Turn, sess *store.Session, msg *a2a.Message, sink Sink, prev *store.Pending, onAccepted func()) error {
	start := time.Now()
	var firstOutput time.Duration
	ctx, cancel := context.WithTimeout(ctx, b.o.TurnTimeout)
	defer cancel()

	acc := newAccumulator()
	accepted := false
	var streamErr error
	savedRunning := false
	for ev, err := range b.o.Agent.Stream(ctx, msg) {
		if err != nil {
			streamErr = err
			break
		}
		u := acc.apply(ev)
		accepted = true
		if onAccepted != nil {
			onAccepted()
			onAccepted = nil
		}
		if !savedRunning && acc.taskID != "" {
			savedRunning = true
			sess.RunningTaskID = acc.taskID
			if acc.contextID != "" {
				sess.ContextID = acc.contextID
			}
			if err := b.o.Store.PutSession(ctx, t.SessionKey, sess); err != nil {
				log.Warn("save running task", "err", err)
			}
		}
		if b.o.ShowToolActivity {
			for _, p := range u.progress {
				sink.Progress(p)
			}
		}
		if u.textChanged {
			if firstOutput == 0 {
				firstOutput = time.Since(start)
			}
			sink.Partial(acc.text())
		}
	}

	// A dropped stream does not mean a failed task: agents such as kagent persist tasks,
	// so poll for the outcome when we know which task to look at.
	if streamErr != nil && acc.taskID != "" && ctx.Err() == nil && !settled(acc.state) {
		log.Warn("stream interrupted, polling task", "err", streamErr, "task", acc.taskID)
		streamErr = b.poll(ctx, acc)
	}

	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer fcancel()

	outcome := "completed"
	sess.RunningTaskID = ""
	if acc.contextID != "" {
		sess.ContextID = acc.contextID
	}

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		outcome = "timeout"
		if acc.taskID != "" {
			_ = b.o.Agent.Cancel(fctx, acc.taskID)
		}
		_ = sink.Final(fctx, Reply{IsError: true, Text: withPartial(acc.text(),
			"⏱️ The agent took too long to answer, so I stopped waiting. Please try again or ask for something smaller.")})
	case streamErr != nil:
		outcome = "error"
		if !accepted {
			// The agent never saw the message: keep any pending request open for a retry.
			sess.Pending = prev
		}
		ref := newID()[:8]
		log.Error("agent call failed", "err", streamErr, "ref", ref, "task", acc.taskID)
		_ = sink.Final(fctx, Reply{IsError: true, Text: withPartial(acc.text(),
			fmt.Sprintf("⚠️ I couldn't get an answer from the agent. Please try again. (ref `%s`)", ref))})
	case acc.state == a2a.TaskStateInputRequired || acc.state == a2a.TaskStateAuthRequired:
		outcome = "input_required"
		b.pause(fctx, t, sess, acc, sink)
	case acc.state == a2a.TaskStateFailed || acc.state == a2a.TaskStateRejected:
		outcome = "failed"
		detail := truncate(acc.statusText(), 500)
		if detail == "" {
			detail = "no details were given"
		}
		_ = sink.Final(fctx, Reply{IsError: true, Text: withPartial(acc.text(),
			fmt.Sprintf("⚠️ The agent couldn't complete this request (%s): %s", stateWord(acc.state), detail))})
	case acc.state == a2a.TaskStateCanceled:
		outcome = "canceled"
		_ = sink.Final(fctx, Reply{Text: withPartial(acc.text(), "_The request was cancelled._")})
	default:
		r := acc.reply()
		if strings.TrimSpace(r.Text) == "" && len(r.Cards) == 0 && len(r.Images) == 0 {
			r.Text = "_The agent finished without a text answer._"
		}
		if err := sink.Final(fctx, r); err != nil {
			outcome = "delivery_error"
			log.Error("deliver final answer", "err", err)
		}
	}

	if err := b.o.Store.PutSession(fctx, t.SessionKey, sess); err != nil {
		log.Error("save session", "err", err)
	}
	if b.o.ObserveTurn != nil {
		b.o.ObserveTurn(outcome, time.Since(start), firstOutput)
	}
	log.Info("turn finished", "outcome", outcome, "task", acc.taskID, "duration", time.Since(start).Round(time.Millisecond))
	return nil
}

func settled(s a2a.TaskState) bool {
	return s.Terminal() || s == a2a.TaskStateInputRequired || s == a2a.TaskStateAuthRequired
}

func (b *Bridge) poll(ctx context.Context, acc *accumulator) error {
	t := time.NewTicker(b.o.PollInterval)
	defer t.Stop()
	var lastErr error
	for {
		task, err := b.o.Agent.GetTask(ctx, acc.taskID)
		if err == nil {
			acc.applyTask(task)
			if settled(acc.state) {
				return nil
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return lastErr
			}
			return ctx.Err()
		case <-t.C:
		}
	}
}

// pause records a pending request and asks the user for input.
func (b *Bridge) pause(ctx context.Context, t Turn, sess *store.Session, acc *accumulator, sink Sink) {
	p := &store.Pending{
		Kind:        store.PendingInput,
		TaskID:      acc.taskID,
		ContextID:   acc.contextID,
		Nonce:       newID(),
		RequesterID: t.User.ID,
		CreatedAt:   time.Now(),
	}
	req := parseHITL(acc.status)
	if req == nil {
		question := acc.statusText()
		if question == "" {
			question = "The agent needs more information to continue."
		}
		if acc.state == a2a.TaskStateAuthRequired {
			question = "🔐 " + question
		}
		_ = sink.Final(ctx, Reply{Text: withPartial(acc.text(), question)})
		sess.Pending = p
		return
	}

	// Close any open stream first, so the card appears below the partial answer.
	if partial := acc.text(); partial != "" {
		_ = sink.Final(ctx, Reply{Text: partial})
	} else {
		_ = sink.Final(ctx, Reply{})
	}
	prompt := Prompt{Nonce: p.Nonce, Hint: req.Hint, RequesterName: t.User.Name, OwnerID: t.User.ID}
	if req.Nested != nil {
		prompt.Subagent = req.Nested.SubagentName
	}
	switch req.Type {
	case typeToolApprovalRequest:
		p.Kind = store.PendingToolApproval
		prompt.Kind = store.PendingToolApproval
		for _, tool := range req.tools() {
			p.ToolIDs = append(p.ToolIDs, tool.ID)
			p.Tools = append(p.Tools, tool.Name)
			prompt.Tools = append(prompt.Tools, PromptTool{Name: tool.Name, Args: tool.Args})
		}
	case typeAskUserRequest:
		p.Kind = store.PendingAskUser
		prompt.Kind = store.PendingAskUser
		p.RequestID = req.ID
		for _, q := range req.Questions {
			sq := store.Question{Text: q.Question, Choices: q.Choices, Multiple: q.Multiple}
			p.Questions = append(p.Questions, sq)
		}
		prompt.Questions = p.Questions
	}
	ref, err := sink.Prompt(ctx, prompt)
	if err != nil {
		b.o.Logger.Error("send prompt card", "err", err)
	}
	p.CardRef = ref
	sess.Pending = p
}

func withPartial(partial, note string) string {
	if strings.TrimSpace(partial) == "" {
		return note
	}
	return partial + "\n\n" + note
}

func stateWord(s a2a.TaskState) string {
	return strings.ToLower(strings.TrimPrefix(string(s), "TASK_STATE_"))
}

func toolList(tools []string) string {
	if len(tools) == 0 {
		return ""
	}
	return "Tools: `" + strings.Join(tools, "`, `") + "`"
}

func nameOr(n string) string {
	if n == "" {
		return "a user"
	}
	return n
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
