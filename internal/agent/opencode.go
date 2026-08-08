package agent

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// opencodeEventIdleTimeout bounds total event-stream silence during a turn.
// Real opencode activity emits step/delta events continuously, so a stream
// that delivers nothing at all for this long means the provider call wedged
// after accepting the message and session.idle will never arrive; without
// this bound the step hangs forever with no diagnostic (#674).
const opencodeEventIdleTimeout = 10 * time.Minute

// opencodeIdleGracePeriod is how long a completed message response waits for
// the stream's trailing session.idle (and final usage events) before
// proceeding with the response.
const opencodeIdleGracePeriod = 5 * time.Second

// opencodeControlTimeout bounds control-plane HTTP calls (session create).
// The message POST deliberately stays unbounded: it blocks for the whole
// agent turn.
const opencodeControlTimeout = 30 * time.Second

// opencodeAgent starts a persistent HTTP server via `opencode serve`
// and sends requests via REST with SSE streaming.
type opencodeAgent struct {
	bin       string
	extraArgs []string
	// eventIdleTimeout, idleGracePeriod, and controlTimeout override the
	// package defaults when positive; tests use them to keep waits short.
	eventIdleTimeout time.Duration
	idleGracePeriod  time.Duration
	controlTimeout   time.Duration
	mu               sync.Mutex
	server           *managedServer
}

// idleResetReader re-arms the event-stream inactivity watchdog whenever the
// underlying stream delivers bytes, so the watchdog measures true event
// silence rather than total turn duration.
type idleResetReader struct {
	r       io.Reader
	timer   *time.Timer
	timeout time.Duration
}

func (r *idleResetReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.timer.Reset(r.timeout)
	}
	return n, err
}

func (a *opencodeAgent) Name() string { return "opencode" }

func (a *opencodeAgent) ReportsAgentAttempts() bool { return true }

func (a *opencodeAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "opencode", opts, claudeMaxRetries, classifyTransient, a.recoverTransientRetry, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *opencodeAgent) recoverTransientRetry(label string) {
	if label != "connection refused" {
		return
	}
	a.mu.Lock()
	srv := a.server
	a.server = nil
	a.mu.Unlock()
	if srv != nil {
		srv.shutdown()
	}
}

func (a *opencodeAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	// Start server on first invocation (synchronized)
	baseURL, err := a.ensureServer(ctx, opts.CWD)
	if err != nil {
		return nil, err
	}

	// The managed server is the only process behind an opencode attempt, so
	// its PID is the liveness identity surfaced as agent_pid. Without these
	// events opencode steps report no PID and no activity at all (#674).
	pid := a.serverPID()
	emitAgentStarted(opts, "opencode", pid)
	result, err := a.runSession(ctx, opts, baseURL)
	emitAgentExited(opts, "opencode", pid, err)
	return result, err
}

func (a *opencodeAgent) serverPID() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server == nil {
		return 0
	}
	return a.server.pid()
}

func (a *opencodeAgent) runSession(ctx context.Context, opts RunOpts, baseURL string) (*Result, error) {
	// Create session with blanket permissions
	sessionID, err := a.createSession(ctx, baseURL, opts.CWD)
	if err != nil {
		return nil, err
	}
	defer a.deleteSession(baseURL, sessionID)

	// Build prompt with schema instructions if provided
	prompt := opts.Prompt
	if len(opts.JSONSchema) > 0 {
		prompt = buildOpencodePrompt(prompt, opts.JSONSchema)
	}

	// Connect to SSE event stream
	streamCtx, streamCancel := context.WithCancel(ctx)
	defer streamCancel()

	eventBody, err := a.connectEventStream(streamCtx, baseURL)
	if err != nil {
		return nil, err
	}
	defer eventBody.Close()

	// Send message concurrently — blocks until agent completes
	type messageResult struct {
		resp *opencodeMessageResponse
		err  error
	}
	msgCtx, msgCancel := context.WithCancel(ctx)
	defer msgCancel()
	msgCh := make(chan messageResult, 1)
	go func() {
		resp, err := a.sendMessage(msgCtx, baseURL, sessionID, prompt, opts.JSONSchema)
		msgCh <- messageResult{resp: resp, err: err}
	}()

	// Process SSE events until session.idle. Parsing runs in its own
	// goroutine so a failed message POST or total event silence cannot leave
	// the step blocked forever on a stream that will never deliver
	// session.idle (#674). An inactivity watchdog cancels the stream when no
	// bytes arrive for the idle bound. Every exit path below drains sseCh
	// before touching state, so the parse goroutine never races the response
	// merge.
	state := &opencodeStreamState{
		sessionID:  sessionID,
		onChunk:    opts.OnChunk,
		textParts:  make(map[string]*opencodeTextPart),
		usageByMsg: make(map[string]TokenUsage),
	}
	idleTimeout := a.eventIdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = opencodeEventIdleTimeout
	}
	var watchdogFired atomic.Bool
	watchdog := time.AfterFunc(idleTimeout, func() {
		watchdogFired.Store(true)
		streamCancel()
	})
	defer watchdog.Stop()
	sseCh := make(chan error, 1)
	go func() {
		sseCh <- parseOpencodeSSE(&idleResetReader{r: eventBody, timer: watchdog, timeout: idleTimeout}, state)
	}()

	var mr messageResult
	select {
	case sseErr := <-sseCh:
		streamCancel()
		if sseErr != nil {
			// Check if message request failed
			select {
			case mr := <-msgCh:
				if mr.err != nil {
					return nil, fmt.Errorf("opencode message: %w", mr.err)
				}
			default:
			}
			a.abortSession(baseURL, sessionID)
			if watchdogFired.Load() {
				return nil, fmt.Errorf("opencode produced no events for %s", idleTimeout)
			}
			return nil, fmt.Errorf("opencode events: %w", sseErr)
		}

		// Wait for message response
		mr = <-msgCh
		if mr.err != nil {
			return nil, fmt.Errorf("opencode message: %w", mr.err)
		}

	case mr = <-msgCh:
		if mr.err != nil {
			// The message POST failed while the stream stayed open, so
			// session.idle will never arrive for this session; abort and
			// surface the message error immediately instead of waiting on
			// the stream.
			a.abortSession(baseURL, sessionID)
			streamCancel()
			<-sseCh
			return nil, fmt.Errorf("opencode message: %w", mr.err)
		}
		// The response is complete; give the stream a short grace window to
		// deliver session.idle plus trailing usage events, then proceed with
		// the response.
		gracePeriod := a.idleGracePeriod
		if gracePeriod <= 0 {
			gracePeriod = opencodeIdleGracePeriod
		}
		grace := time.NewTimer(gracePeriod)
		defer grace.Stop()
		select {
		case sseErr := <-sseCh:
			if sseErr != nil && !watchdogFired.Load() {
				a.abortSession(baseURL, sessionID)
				return nil, fmt.Errorf("opencode events: %w", sseErr)
			}
		case <-grace.C:
			streamCancel()
			<-sseCh
		}
	}

	// Update usage and text from message response
	responseText := ""
	responseFinalText := ""
	if mr.resp != nil && mr.resp.Info != nil {
		streamedText := state.lastText
		streamedFinalText := state.lastFinalText
		emitResponseChunk := func(chunk string) {
			if opts.OnChunk == nil || chunk == "" {
				return
			}
			state.emitSeparatorIfNeeded()
			opts.OnChunk(chunk)
			state.hasEmittedText = true
		}
		if mr.resp.Info.Role == "assistant" && mr.resp.Info.Tokens != nil {
			state.usageByMsg[mr.resp.Info.ID] = opencodeTokensToUsage(mr.resp.Info.Tokens)
			state.usage = accumulateUsage(state.usageByMsg)
		}
		for _, part := range mr.resp.Parts {
			if part.Type != "text" || strings.TrimSpace(part.Text) == "" {
				continue
			}
			responseText += part.Text
			if part.Metadata != nil && part.Metadata.OpenAI != nil && part.Metadata.OpenAI.Phase == "final_answer" {
				responseFinalText += part.Text
			}
		}
		if responseText != "" {
			state.lastText = responseText
		}
		if responseFinalText != "" {
			state.lastFinalText = responseFinalText
		}
		if responseFinalText != "" {
			responseText = responseFinalText
		}
		if opts.OnChunk != nil && responseText != "" {
			streamedResponseText := streamedText
			if streamedFinalText != "" {
				streamedResponseText = streamedFinalText
			}
			switch {
			case !state.hasEmittedText:
				emitResponseChunk(responseText)
			case streamedResponseText == "":
				emitResponseChunk(responseText)
			case strings.HasPrefix(responseText, streamedResponseText):
				suffix := responseText[len(streamedResponseText):]
				emitResponseChunk(suffix)
			}
		}
	}

	// Prefer structured output from response
	if mr.resp != nil && mr.resp.Info != nil && mr.resp.Info.Structured != nil {
		return &Result{
			Output:                mr.resp.Info.Structured,
			Text:                  state.lastText,
			Usage:                 state.usage,
			UsageReported:         state.usage.Reported,
			CacheCreationReported: state.usage.CacheCreationReported,
		}, nil
	}

	// Surface opencode's StructuredOutputError directly. When the model
	// fails to call the StructuredOutput tool after the configured retries,
	// opencode sets info.error.name = "StructuredOutputError" and the
	// streamed text is just reasoning prose - feeding it to
	// finalizeTextResult produces the misleading "invalid character 'N'
	// looking for beginning of value" error.
	if mr.resp != nil && mr.resp.Info != nil && mr.resp.Info.Error.IsStructuredOutput() {
		retries := 0
		if mr.resp.Info.Error.Retries != nil {
			retries = *mr.resp.Info.Error.Retries
		}
		return nil, fmt.Errorf("opencode structured output failed after %d internal retries: %s",
			retries, mr.resp.Info.Error.Message)
	}

	// Fall back to parsing JSON from text
	outputText := state.lastFinalText
	if outputText == "" {
		outputText = state.lastText
	}
	return finalizeTextResult("opencode", outputText, opts.JSONSchema, state.usage)
}

func (a *opencodeAgent) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server != nil {
		a.server.shutdown()
		a.server = nil
	}
	return nil
}
