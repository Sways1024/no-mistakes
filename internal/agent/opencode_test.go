package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpencodeAgent_CloseWithoutServer(t *testing.T) {
	a := &opencodeAgent{bin: "opencode"}
	if err := a.Close(); err != nil {
		t.Errorf("Close without server should not error: %v", err)
	}
}

// TestOpencodeAgent_FullFlow tests the full session lifecycle using a mock HTTP server.
func TestOpencodeAgent_FullFlow(t *testing.T) {
	calledPaths := make(map[string]bool)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledPaths[r.Method+" "+r.URL.Path] = true
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"test-session-456"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			if r.Header.Get("Accept") != "text/event-stream" {
				t.Error("expected Accept: text/event-stream")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// Send text delta events then usage and idle
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"test-session-456\",\"part\":{\"id\":\"p1\",\"messageID\":\"msg1\",\"type\":\"text\",\"text\":\"{\\\"success\\\":true,\\\"summary\\\":\\\"all good\\\"}\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"test-session-456\",\"info\":{\"id\":\"msg1\",\"role\":\"assistant\",\"tokens\":{\"input\":100,\"output\":50}}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/test-session-456/message" && r.Method == http.MethodPost:
			// Return message response with structured output
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant","structured":{"success":true,"summary":"all good"},"tokens":{"input":100,"output":50}},"parts":[{"type":"text","text":"{\"success\":true,\"summary\":\"all good\"}"}]}`)

		case r.URL.Path == "/session/test-session-456" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	var chunks []string
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "review this code",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object"}`),
		OnChunk:    func(text string) { chunks = append(chunks, text) },
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result")
	}

	// Verify structured output from response
	var output map[string]any
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}
	if output["success"] != true {
		t.Errorf("expected success=true, got %v", output["success"])
	}

	// Verify usage
	if result.Usage.InputTokens != 100 {
		t.Errorf("expected input tokens 100, got %d", result.Usage.InputTokens)
	}
	if result.Usage.OutputTokens != 50 {
		t.Errorf("expected output tokens 50, got %d", result.Usage.OutputTokens)
	}

	// Verify chunks received
	if len(chunks) < 1 {
		t.Error("expected at least 1 chunk")
	}

	// Verify key API calls were made
	if !calledPaths["POST /session"] {
		t.Error("expected POST /session call")
	}
	if !calledPaths["GET /global/event"] {
		t.Error("expected GET /global/event call")
	}
	if !calledPaths["POST /session/test-session-456/message"] {
		t.Error("expected POST /session/{id}/message call")
	}
}

func TestOpencodeAgent_BackfillsAssistantTextWhenStreamCannotClassifyOrphans(t *testing.T) {
	calledPaths := make(map[string]bool)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledPaths[r.Method+" "+r.URL.Path] = true
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"test-session-789"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.delta\",\"properties\":{\"sessionID\":\"test-session-789\",\"field\":\"text\",\"partID\":\"p1\",\"delta\":\"hello \"}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.delta\",\"properties\":{\"sessionID\":\"test-session-789\",\"field\":\"text\",\"partID\":\"p2\",\"delta\":\"world\"}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"test-session-789\",\"info\":{\"id\":\"msg1\",\"role\":\"assistant\",\"tokens\":{\"input\":100,\"output\":50}}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/test-session-789/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant","structured":{"summary":"hello world"},"tokens":{"input":100,"output":50}},"parts":[{"type":"text","text":"hello world"}]}`)

		case r.URL.Path == "/session/test-session-789" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	var chunks []string
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "review this code",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object"}`),
		OnChunk:    func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result")
	}
	if len(chunks) != 1 || chunks[0] != "hello world" {
		t.Fatalf("expected one backfilled chunk, got %v", chunks)
	}
	if result.Text != "hello world" {
		t.Fatalf("expected result text 'hello world', got %q", result.Text)
	}
	if string(result.Output) != `{"summary":"hello world"}` {
		t.Fatalf("expected structured summary 'hello world', got %s", string(result.Output))
	}
	if !calledPaths[http.MethodGet+" /global/event"] {
		t.Fatal("expected event stream to be called")
	}
}

func TestOpencodeAgent_BackfillsAllAssistantResponseParts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"},"parts":[{"type":"text","text":"hello "},{"type":"text","text":"world"}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	var chunks []string
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:  "hello",
		CWD:     t.TempDir(),
		OnChunk: func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "hello world" {
		t.Fatalf("expected combined response text, got %q", result.Text)
	}
	if len(chunks) != 1 || chunks[0] != "hello world" {
		t.Fatalf("expected one combined backfill chunk, got %v", chunks)
	}
}

func TestOpencodeAgent_BackfillsMissingResponseSuffixAfterStreaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"p1\",\"messageID\":\"msg1\",\"type\":\"text\",\"text\":\"hello\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"s1\",\"info\":{\"id\":\"msg1\",\"role\":\"assistant\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"},"parts":[{"type":"text","text":"hello world"}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	var chunks []string
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:  "hello",
		CWD:     t.TempDir(),
		OnChunk: func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "hello world" {
		t.Fatalf("expected completed response text, got %q", result.Text)
	}
	if got := strings.Join(chunks, ""); got != "hello world" {
		t.Fatalf("expected streamed and backfilled text to form full response, got %q from %v", got, chunks)
	}
	if len(chunks) != 2 || chunks[1] != " world" {
		t.Fatalf("expected missing suffix backfill, got %v", chunks)
	}
}

func TestOpencodeAgent_BackfillsMissingResponseSuffixAfterToolStep(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"p1\",\"messageID\":\"msg1\",\"type\":\"text\",\"text\":\"hello\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"s1\",\"info\":{\"id\":\"msg1\",\"role\":\"assistant\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"step1\",\"messageID\":\"msg1\",\"type\":\"step-finish\",\"tokens\":{\"input\":10,\"output\":5}}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"},"parts":[{"type":"text","text":"hello world"}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	var chunks []string
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:  "hello",
		CWD:     t.TempDir(),
		OnChunk: func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "hello world" {
		t.Fatalf("expected completed response text, got %q", result.Text)
	}
	if got := strings.Join(chunks, ""); got != "hello\n\n world" {
		t.Fatalf("expected streamed and backfilled text with separator, got %q from %v", got, chunks)
	}
	if len(chunks) != 3 || chunks[1] != "\n\n" || chunks[2] != " world" {
		t.Fatalf("expected separator before missing suffix backfill, got %v", chunks)
	}
}

func TestOpencodeAgent_DoesNotSeparateBackfillWhenToolStepPrecedesFirstText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"step1\",\"messageID\":\"msg1\",\"type\":\"step-finish\",\"tokens\":{\"input\":10,\"output\":5}}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"p1\",\"messageID\":\"msg1\",\"type\":\"text\",\"text\":\"hello\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"s1\",\"info\":{\"id\":\"msg1\",\"role\":\"assistant\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"},"parts":[{"type":"text","text":"hello world"}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	var chunks []string
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:  "hello",
		CWD:     t.TempDir(),
		OnChunk: func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "hello world" {
		t.Fatalf("expected completed response text, got %q", result.Text)
	}
	if got := strings.Join(chunks, ""); got != "hello world" {
		t.Fatalf("expected streamed and backfilled text without separator, got %q from %v", got, chunks)
	}
	if len(chunks) != 2 || chunks[1] != " world" {
		t.Fatalf("expected suffix backfill without separator, got %v", chunks)
	}
}

// TestOpencodeAgent_NoSchema tests the flow without a JSON schema.
func TestOpencodeAgent_NoSchema(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.delta\",\"properties\":{\"sessionID\":\"s1\",\"field\":\"text\",\"partID\":\"p1\",\"delta\":\"done\"}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"},"parts":[{"type":"text","text":"done"}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	result, err := a.Run(context.Background(), RunOpts{
		Prompt: "hello",
		CWD:    t.TempDir(),
		// No JSONSchema
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result")
	}
	if result.Text != "done" {
		t.Fatalf("expected plain text result, got %q", result.Text)
	}
	if result.Output != nil {
		t.Fatalf("expected nil structured output, got %s", string(result.Output))
	}
}

// TestOpencodeAgent_FinalAnswerPreferred tests that final_answer phase text is preferred.
func TestOpencodeAgent_FinalAnswerPreferred(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			// First text part (regular), then final_answer part
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"p1\",\"type\":\"text\",\"text\":\"thinking...\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"p2\",\"type\":\"text\",\"text\":\"{\\\"answer\\\":42}\",\"metadata\":{\"openai\":{\"phase\":\"final_answer\"}}}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"},"parts":[{"type":"text","text":"thinking..."},{"type":"text","text":"{\"answer\":42}","metadata":{"openai":{"phase":"final_answer"}}}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	result, err := a.Run(context.Background(), RunOpts{
		Prompt: "what is 6*7",
		CWD:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != `{"answer":42}` {
		t.Fatalf("expected final_answer text, got %q", result.Text)
	}
	if result.Output != nil {
		t.Fatalf("expected nil structured output, got %s", string(result.Output))
	}
}

// TestOpencodeAgent_StructuredOutputError asserts that when opencode returns
// an info.error with name=StructuredOutputError, the agent surfaces a
// precise error and does NOT fall through to text-parsing the streamed
// reasoning prose. Regression: run 01KWDTFPNXTC94YEYCN23XFFG1 surfaced
// "invalid character 'N' looking for beginning of value" instead of the
// real cause.
func TestOpencodeAgent_StructuredOutputError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// Stream reasoning prose (no JSON) - this is exactly the
			// shape real opencode emits when the model never calls the
			// StructuredOutput tool.
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"p1\",\"messageID\":\"msg1\",\"type\":\"text\",\"text\":\"Now I need to find the failing test. The only failing test is foo.\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"s1\",\"info\":{\"id\":\"msg1\",\"role\":\"assistant\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			// opencode signals structured-output failure via
			// info.error.name = "StructuredOutputError". The body
			// intentionally omits info.structured.
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant","error":{"name":"StructuredOutputError","message":"Model did not produce structured output","retries":2}},"parts":[{"type":"text","text":"Now I need to find the failing test. The only failing test is foo."}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "fix the failing tests",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`),
	})
	if err == nil {
		t.Fatalf("expected error, got result %+v", result)
	}
	if result != nil {
		t.Fatalf("expected nil result on error, got %+v", result)
	}
	msg := err.Error()
	if !strings.Contains(msg, "structured output failed") {
		t.Errorf("expected error to mention structured output failure, got %q", msg)
	}
	if !strings.Contains(msg, "2 internal retries") {
		t.Errorf("expected error to surface retry count, got %q", msg)
	}
	if strings.Contains(msg, "invalid character") {
		t.Errorf("error must not be the JSON-parse-on-prose symptom, got %q", msg)
	}
	if strings.Contains(msg, "Now I need to find the failing test") {
		t.Errorf("error must not embed the reasoning prose snippet, got %q", msg)
	}
}

// runOpencodeBounded runs a.Run in a goroutine and fails the test if it does
// not return within a generous bound — the #674 regression mode is an
// indefinite silent hang, which must surface as a test failure, not a suite
// timeout.
func runOpencodeBounded(t *testing.T, a *opencodeAgent, opts RunOpts) (*Result, error) {
	t.Helper()
	type runReturn struct {
		result *Result
		err    error
	}
	done := make(chan runReturn, 1)
	go func() {
		result, err := a.Run(context.Background(), opts)
		done <- runReturn{result: result, err: err}
	}()
	select {
	case rr := <-done:
		return rr.result, rr.err
	case <-time.After(10 * time.Second):
		t.Fatal("opencode Run did not return within 10s")
		return nil, nil
	}
}

// TestOpencodeAgent_MessageErrorWhileStreamSilentFailsFast asserts that a
// failed message POST surfaces immediately even while the event stream stays
// open and silent. Before #674 the step blocked in the SSE parse waiting for
// a session.idle that could never arrive.
func TestOpencodeAgent_MessageErrorWhileStreamSilentFailsFast(t *testing.T) {
	aborted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done() // stream stays open, emits nothing

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			http.Error(w, "provider exploded", http.StatusBadGateway)

		case r.URL.Path == "/session/s1/abort" && r.Method == http.MethodPost:
			select {
			case aborted <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	result, err := runOpencodeBounded(t, a, RunOpts{Prompt: "hello", CWD: t.TempDir()})
	if err == nil {
		t.Fatalf("expected error, got result %+v", result)
	}
	if !strings.Contains(err.Error(), "opencode message") {
		t.Errorf("expected message error, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("expected 502 status in error, got %q", err.Error())
	}
	select {
	case <-aborted:
	default:
		t.Error("expected session abort after message failure")
	}
}

// TestOpencodeAgent_EventSilenceWatchdogAbortsWithDiagnostic asserts that a
// stream delivering no events at all for the idle bound fails the attempt
// with a diagnostic instead of hanging forever (#674: provider call wedged
// after accepting the message, so neither the message POST nor session.idle
// ever completes).
func TestOpencodeAgent_EventSilenceWatchdogAbortsWithDiagnostic(t *testing.T) {
	aborted := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done() // stream stays open, emits nothing

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			// Drain the body so the server's background read can observe the
			// client disconnect and cancel the request context.
			io.Copy(io.Discard, r.Body)
			<-r.Context().Done() // provider wedged: the POST never returns

		case r.URL.Path == "/session/s1/abort" && r.Method == http.MethodPost:
			select {
			case aborted <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:              "opencode",
		eventIdleTimeout: 100 * time.Millisecond,
		server:           &managedServer{port: mustParsePort(server.URL)},
	}

	var events []LifecycleEvent
	result, err := runOpencodeBounded(t, a, RunOpts{
		Prompt:      "hello",
		CWD:         t.TempDir(),
		OnLifecycle: func(event LifecycleEvent) { events = append(events, event) },
	})
	if err == nil {
		t.Fatalf("expected error, got result %+v", result)
	}
	if !strings.Contains(err.Error(), "opencode produced no events for 100ms") {
		t.Errorf("expected event-silence diagnostic, got %q", err.Error())
	}
	select {
	case <-aborted:
	default:
		t.Error("expected session abort after watchdog fired")
	}
	if len(events) == 0 || events[len(events)-1].Phase != LifecyclePhaseExit {
		t.Fatalf("expected trailing exit lifecycle event, got %+v", events)
	}
	if !strings.Contains(events[len(events)-1].Message, "produced no events") {
		t.Errorf("expected exit event to carry the diagnostic, got %q", events[len(events)-1].Message)
	}
}

// TestOpencodeAgent_CreateSessionTimesOutInsteadOfHanging asserts the
// control-plane session create is bounded: a server that accepts the request
// and never answers must fail the attempt, not hang the step (#674).
func TestOpencodeAgent_CreateSessionTimesOutInsteadOfHanging(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/session" && r.Method == http.MethodPost {
			// Drain the body so the server's background read can observe the
			// client disconnect and cancel the request context.
			io.Copy(io.Discard, r.Body)
			<-r.Context().Done() // never answers
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:            "opencode",
		controlTimeout: 100 * time.Millisecond,
		server:         &managedServer{port: mustParsePort(server.URL)},
	}

	result, err := runOpencodeBounded(t, a, RunOpts{Prompt: "hello", CWD: t.TempDir()})
	if err == nil {
		t.Fatalf("expected error, got result %+v", result)
	}
	if !strings.Contains(err.Error(), "opencode create session") {
		t.Errorf("expected create-session error, got %q", err.Error())
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected deadline error, got %q", err.Error())
	}
}

// TestOpencodeAgent_ProceedsWithResponseWhenIdleNeverArrives asserts that a
// completed message response is not held hostage by a stream that never
// delivers session.idle: after a short grace window the attempt proceeds
// with the response, preserving the backfill emit path.
func TestOpencodeAgent_ProceedsWithResponseWhenIdleNeverArrives(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done() // stream stays open, never emits session.idle

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant","tokens":{"input":100,"output":50}},"parts":[{"type":"text","text":"hello world"}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:             "opencode",
		idleGracePeriod: 50 * time.Millisecond,
		server:          &managedServer{port: mustParsePort(server.URL)},
	}

	var chunks []string
	result, err := runOpencodeBounded(t, a, RunOpts{
		Prompt:  "hello",
		CWD:     t.TempDir(),
		OnChunk: func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "hello world" {
		t.Fatalf("expected response text, got %q", result.Text)
	}
	if got := strings.Join(chunks, ""); got != "hello world" {
		t.Fatalf("expected backfilled response text, got %q from %v", got, chunks)
	}
	if result.Usage.InputTokens != 100 || result.Usage.OutputTokens != 50 {
		t.Fatalf("expected usage from message response, got %+v", result.Usage)
	}
}

// TestOpencodeAgent_EmitsServerLifecycleEvents asserts each attempt emits
// start/exit lifecycle events carrying the managed-server identity, so the
// pipeline executor populates agent_pid and last_activity for opencode steps
// exactly like subprocess adapters (#674: agent_pid was always empty).
func TestOpencodeAgent_EmitsServerLifecycleEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"},"parts":[{"type":"text","text":"done"}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	var events []LifecycleEvent
	_, err := a.Run(context.Background(), RunOpts{
		Prompt:      "hello",
		CWD:         t.TempDir(),
		OnLifecycle: func(event LifecycleEvent) { events = append(events, event) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected start and exit lifecycle events, got %+v", events)
	}
	if events[0].Phase != LifecyclePhaseStart || events[0].Agent != "opencode" {
		t.Errorf("expected opencode start event first, got %+v", events[0])
	}
	if events[1].Phase != LifecyclePhaseExit || events[1].Agent != "opencode" {
		t.Errorf("expected opencode exit event last, got %+v", events[1])
	}
	if !strings.Contains(events[1].Message, "status=success") {
		t.Errorf("expected success exit message, got %q", events[1].Message)
	}
}
