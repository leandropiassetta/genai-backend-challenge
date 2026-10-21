package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

func sqsMessage(id, body string, attrs map[string]string) events.SQSMessage {
	return events.SQSMessage{MessageId: id, Body: body, Attributes: attrs}
}

func TestParseRecord_Valid(t *testing.T) {
	r := sqsMessage("m1", `{"session_id":" s1 ","message":" oi ","correlation_id":"c1"}`,
		map[string]string{"SentTimestamp": "1760000000000", "ApproximateReceiveCount": "2"})

	msg, err := ParseRecord(r, 4000)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if msg.SessionID != "s1" || msg.Text != "oi" || msg.CorrelationID != "c1" || msg.ReceiveCount != 2 {
		t.Fatalf("mensagem inesperada: %+v", msg)
	}
	if got := msg.ExchangeKey(); got != "1760000000000#m1" {
		t.Fatalf("ExchangeKey = %q", got)
	}
}

func TestParseRecord_CorrelationDefaultsToMessageID(t *testing.T) {
	msg, err := ParseRecord(sqsMessage("m1", `{"session_id":"s1","message":"oi"}`, nil), 4000)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if msg.CorrelationID != "m1" {
		t.Fatalf("CorrelationID = %q, esperado m1", msg.CorrelationID)
	}
}

func TestParseRecord_MissingSentTimestamp(t *testing.T) {
	msg, err := ParseRecord(sqsMessage("m1", `{"session_id":"s1","message":"oi"}`, nil), 4000)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if got := msg.ExchangeKey(); got != "0000000000000#m1" {
		t.Fatalf("ExchangeKey = %q", got)
	}
}

func TestParseRecord_Invalid(t *testing.T) {
	cases := []struct {
		name, body, reason string
	}{
		{"json quebrado", `não é json`, "invalid_json"},
		{"não é objeto", `[1]`, "body_not_object"},
		{"sem sessão", `{"message":"x"}`, "missing_session_id"},
		{"texto vazio", `{"session_id":"s","message":"  "}`, "missing_message"},
		{"texto longo", `{"session_id":"s","message":"` + strings.Repeat("é", 4001) + `"}`, "message_too_long"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseRecord(sqsMessage("bad", tc.body, nil), 4000)
			var nre *NonRetriableError
			if !errors.As(err, &nre) {
				t.Fatalf("esperado NonRetriableError, veio %v", err)
			}
			if nre.Reason != tc.reason {
				t.Fatalf("Reason = %q, esperado %q", nre.Reason, tc.reason)
			}
		})
	}
}

func TestBuildMessages_OrderAndBudget(t *testing.T) {
	var history []Exchange
	for i := 0; i < 5; i++ {
		history = append(history, Exchange{
			MessageID:     string(rune('a' + i)),
			UserText:      strings.Repeat("u", 10),
			AssistantText: strings.Repeat("a", 10),
		})
	}

	msgs := BuildMessages(history, "nova", 50)

	if len(msgs) != 5 {
		t.Fatalf("len = %d, esperado 5 (2 trocas + nova)", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("papéis fora de ordem: %+v", msgs)
	}
	if last := msgs[len(msgs)-1]; last != (ChatMessage{Role: "user", Content: "nova"}) {
		t.Fatalf("última mensagem = %+v", last)
	}
}

func TestBuildMessages_KeepsMostRecentTurns(t *testing.T) {
	history := []Exchange{
		{UserText: "antiga", AssistantText: "r1"},
		{UserText: "recente", AssistantText: "r2"},
	}
	msgs := BuildMessages(history, "nova", 20)
	if len(msgs) != 3 || msgs[0].Content != "recente" {
		t.Fatalf("esperado manter só a troca mais recente, veio %+v", msgs)
	}
}

func TestBuildMessages_NoHistory(t *testing.T) {
	msgs := BuildMessages(nil, "oi", 100)
	if len(msgs) != 1 || msgs[0] != (ChatMessage{Role: "user", Content: "oi"}) {
		t.Fatalf("veio %+v", msgs)
	}
}

func TestSettingsFromEnv_DefaultsAndOverrides(t *testing.T) {
	s, err := SettingsFromEnv()
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if s != DefaultSettings() {
		t.Fatalf("sem variáveis, esperado os padrões; veio %+v", s)
	}

	t.Setenv("MAX_TOKENS", "512")
	s, err = SettingsFromEnv()
	if err != nil || s.MaxTokens != 512 {
		t.Fatalf("MaxTokens = %d, err = %v", s.MaxTokens, err)
	}

	t.Setenv("MAX_TOKENS", "abc")
	if _, err := SettingsFromEnv(); err == nil {
		t.Fatal("esperado erro para MAX_TOKENS inválido")
	}
}

func TestDefaultSettings_SpecValues(t *testing.T) {
	s := DefaultSettings()
	if s.MaxTokens != 1024 || s.Temperature != 0.3 || s.HistoryMaxTurns != 10 || s.HistoryMaxChars != 12000 ||
		s.MaxInputChars != 4000 || s.MaxWorkers != 5 || s.PromptVersion != "v1" ||
		s.ModelID != "anthropic.claude-3-sonnet-20240229-v1:0" {
		t.Fatalf("padrões divergem da spec: %+v", s)
	}
}

// ---------------------------------------------------------------------------
// Orquestração do lote
// ---------------------------------------------------------------------------

const testNow = int64(1760000000000)

type fakeModel struct {
	mu     sync.Mutex
	calls  [][]ChatMessage
	failOn map[string]error
	delay  time.Duration
}

func (f *fakeModel) Complete(_ context.Context, _ string, msgs []ChatMessage) (Completion, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]ChatMessage(nil), msgs...))
	f.mu.Unlock()
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	last := msgs[len(msgs)-1].Content
	if err, ok := f.failOn[last]; ok {
		return Completion{}, err
	}
	return Completion{Text: "resp:" + last, InputTokens: 10, OutputTokens: 5, StopReason: "end_turn"}, nil
}

func (f *fakeModel) Calls() [][]ChatMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]ChatMessage(nil), f.calls...)
}

type panicModel struct{}

func (panicModel) Complete(context.Context, string, []ChatMessage) (Completion, error) {
	panic("bug no adaptador")
}

type testEnv struct {
	proc  *Processor
	model *fakeModel
	store *InMemorySessionStore
	pub   *InMemoryPublisher
	sink  *InMemoryPoisonSink
	logs  *bytes.Buffer
}

type envOpt func(*Settings, *bool)

func withSettings(f func(*Settings)) envOpt { return func(s *Settings, _ *bool) { f(s) } }
func withoutSink() envOpt                   { return func(_ *Settings, sink *bool) { *sink = false } }

func newTestEnv(t *testing.T, model ChatModel, opts ...envOpt) testEnv {
	t.Helper()
	s := DefaultSettings()
	s.MinRemaining = time.Second
	useSink := true
	for _, o := range opts {
		o(&s, &useSink)
	}
	env := testEnv{
		store: NewInMemorySessionStore(),
		pub:   &InMemoryPublisher{},
		sink:  &InMemoryPoisonSink{},
		logs:  &bytes.Buffer{},
	}
	if fm, ok := model.(*fakeModel); ok {
		env.model = fm
	}
	var poison PoisonSink
	if useSink {
		poison = env.sink
	}
	env.proc = NewProcessor(s, model, env.store, env.pub, poison, DefaultSystemPrompt,
		NewLogger(&syncWriter{w: env.logs}, slog.LevelInfo), io.Discard)
	return env
}

// syncWriter protege o buffer de log contra escrita concorrente das goroutines.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func ctxWithTimeout(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func rec(id, session, text string, offsetMs int64) events.SQSMessage {
	body, _ := json.Marshal(map[string]string{"session_id": session, "message": text})
	return sqsMessage(id, string(body), map[string]string{
		"SentTimestamp":           strconv.FormatInt(testNow+offsetMs, 10),
		"ApproximateReceiveCount": "1",
	})
}

func batch(records ...events.SQSMessage) events.SQSEvent { return events.SQSEvent{Records: records} }

func failedIDs(resp events.SQSEventResponse) []string {
	ids := make([]string, 0, len(resp.BatchItemFailures))
	for _, f := range resp.BatchItemFailures {
		ids = append(ids, f.ItemIdentifier)
	}
	sort.Strings(ids)
	return ids
}

func publishedIDs(pub *InMemoryPublisher) []string {
	var ids []string
	for _, p := range pub.Sent() {
		ids = append(ids, p.MessageID)
	}
	sort.Strings(ids)
	return ids
}

func assertIDs(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if got == nil {
		got = []string{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %v, esperado %v", what, got, want)
	}
}

func TestProcessBatch_HappyPath(t *testing.T) {
	env := newTestEnv(t, &fakeModel{})
	resp, _ := env.proc.ProcessBatch(ctxWithTimeout(t, 2*time.Minute), batch(rec("m1", "s1", "oi", 0), rec("m2", "s2", "olá", 1)))

	assertIDs(t, "falhas", failedIDs(resp))
	assertIDs(t, "publicadas", publishedIDs(env.pub), "m1", "m2")
	if n := len(env.model.Calls()); n != 2 {
		t.Fatalf("chamadas ao modelo = %d, esperado 2", n)
	}
}

func TestProcessBatch_TransientFailureOnlyThatMessage(t *testing.T) {
	env := newTestEnv(t, &fakeModel{failOn: map[string]error{"falha": errors.New("throttling")}})
	resp, _ := env.proc.ProcessBatch(ctxWithTimeout(t, 2*time.Minute), batch(rec("m1", "s1", "ok", 0), rec("m2", "s2", "falha", 1)))

	assertIDs(t, "falhas", failedIDs(resp), "m2")
	assertIDs(t, "publicadas", publishedIDs(env.pub), "m1")
}

func TestProcessBatch_FailureKeepsSessionOrder(t *testing.T) {
	env := newTestEnv(t, &fakeModel{failOn: map[string]error{"primeira": errors.New("timeout")}})
	resp, _ := env.proc.ProcessBatch(ctxWithTimeout(t, 2*time.Minute), batch(
		rec("m1", "s1", "primeira", 0), rec("m2", "s1", "segunda", 1), rec("m3", "s2", "outra", 2)))

	assertIDs(t, "falhas", failedIDs(resp), "m1", "m2")
	for _, call := range env.model.Calls() {
		if call[len(call)-1].Content == "segunda" {
			t.Fatal("m2 não devia chegar ao modelo depois da falha de m1 na mesma sessão")
		}
	}
	assertIDs(t, "publicadas", publishedIDs(env.pub), "m3")
}

func TestProcessBatch_PoisonGoesToDLQ(t *testing.T) {
	for _, body := range []string{`não é json`, `[1]`, `{"message":"sem sessão"}`, `{"session_id":"s","message":"  "}`} {
		t.Run(body, func(t *testing.T) {
			env := newTestEnv(t, &fakeModel{})
			resp, _ := env.proc.ProcessBatch(ctxWithTimeout(t, 2*time.Minute), batch(sqsMessage("bad", body, nil)))

			assertIDs(t, "falhas", failedIDs(resp))
			if got := env.sink.Received(); len(got) != 1 || got[0].MessageID != "bad" {
				t.Fatalf("DLQ recebeu %+v", got)
			}
			if n := len(env.model.Calls()); n != 0 {
				t.Fatalf("modelo chamado %d vezes para mensagem inválida", n)
			}
		})
	}
}

func TestProcessBatch_PoisonWithoutSinkIsRetried(t *testing.T) {
	env := newTestEnv(t, &fakeModel{}, withoutSink())
	resp, _ := env.proc.ProcessBatch(ctxWithTimeout(t, 2*time.Minute), batch(sqsMessage("bad", "{", nil)))
	assertIDs(t, "falhas", failedIDs(resp), "bad")
}

func TestProcessBatch_NonRetriableFromModelGoesToDLQ(t *testing.T) {
	env := newTestEnv(t, &fakeModel{failOn: map[string]error{
		"x": &NonRetriableError{Reason: "bedrock_ValidationException"},
	}})
	resp, _ := env.proc.ProcessBatch(ctxWithTimeout(t, 2*time.Minute), batch(rec("m1", "s1", "x", 0)))

	assertIDs(t, "falhas", failedIDs(resp))
	if got := env.sink.Received(); len(got) != 1 || got[0].Reason != "bedrock_ValidationException" {
		t.Fatalf("DLQ recebeu %+v", got)
	}
}

func TestProcessBatch_PanicIsRetriedNotCrash(t *testing.T) {
	env := newTestEnv(t, panicModel{})
	resp, _ := env.proc.ProcessBatch(ctxWithTimeout(t, 2*time.Minute), batch(rec("m1", "s1", "oi", 0)))
	assertIDs(t, "falhas", failedIDs(resp), "m1")
}

func TestProcessBatch_HistoryInjectedInOrder(t *testing.T) {
	env := newTestEnv(t, &fakeModel{})
	ctx := ctxWithTimeout(t, 2*time.Minute)
	env.proc.ProcessBatch(ctx, batch(rec("m1", "s1", "pergunta 1", 0)))
	env.proc.ProcessBatch(ctx, batch(rec("m2", "s1", "pergunta 2", 10)))

	calls := env.model.Calls()
	want := []ChatMessage{
		{Role: "user", Content: "pergunta 1"},
		{Role: "assistant", Content: "resp:pergunta 1"},
		{Role: "user", Content: "pergunta 2"},
	}
	if got := calls[len(calls)-1]; !reflect.DeepEqual(got, want) {
		t.Fatalf("prompt = %+v", got)
	}
}

func TestProcessBatch_HistoryIsolatedBetweenSessions(t *testing.T) {
	env := newTestEnv(t, &fakeModel{})
	ctx := ctxWithTimeout(t, 2*time.Minute)
	env.proc.ProcessBatch(ctx, batch(rec("m1", "s1", "segredo da s1", 0)))
	env.proc.ProcessBatch(ctx, batch(rec("m2", "s2", "oi", 10)))

	calls := env.model.Calls()
	if got := calls[len(calls)-1]; !reflect.DeepEqual(got, []ChatMessage{{Role: "user", Content: "oi"}}) {
		t.Fatalf("histórico vazou entre sessões: %+v", got)
	}
}

func TestProcessBatch_DuplicateReusesAnswer(t *testing.T) {
	env := newTestEnv(t, &fakeModel{})
	ctx := ctxWithTimeout(t, 2*time.Minute)
	r := rec("m1", "s1", "oi", 0)
	env.proc.ProcessBatch(ctx, batch(r))
	r.Attributes["ApproximateReceiveCount"] = "2"
	resp, _ := env.proc.ProcessBatch(ctx, batch(r))

	assertIDs(t, "falhas", failedIDs(resp))
	if n := len(env.model.Calls()); n != 1 {
		t.Fatalf("modelo chamado %d vezes, esperado 1", n)
	}
	sent := env.pub.Sent()
	if len(sent) != 2 || sent[0].Answer != "resp:oi" || sent[1].Answer != "resp:oi" {
		t.Fatalf("publicadas = %+v", sent)
	}
	if got := env.proc.LastMetrics().Values("DuplicatesSkipped"); !reflect.DeepEqual(got, []float64{1}) {
		t.Fatalf("DuplicatesSkipped = %v", got)
	}
}

func TestProcessBatch_DeadlineNearReturnsMessages(t *testing.T) {
	env := newTestEnv(t, &fakeModel{}, withSettings(func(s *Settings) { s.MinRemaining = 60 * time.Second }))
	resp, _ := env.proc.ProcessBatch(ctxWithTimeout(t, 30*time.Second), batch(rec("m1", "s1", "oi", 0), rec("m2", "s2", "oi", 1)))

	assertIDs(t, "falhas", failedIDs(resp), "m1", "m2")
	if n := len(env.model.Calls()); n != 0 {
		t.Fatalf("modelo chamado %d vezes com deadline estourado", n)
	}
}

func TestProcessBatch_NoDeadlineProcesses(t *testing.T) {
	env := newTestEnv(t, &fakeModel{})
	resp, _ := env.proc.ProcessBatch(context.Background(), batch(rec("m1", "s1", "oi", 0)))
	assertIDs(t, "falhas", failedIDs(resp))
	assertIDs(t, "publicadas", publishedIDs(env.pub), "m1")
}

func TestProcessBatch_SessionsRunInParallel(t *testing.T) {
	env := newTestEnv(t, &fakeModel{delay: 200 * time.Millisecond}, withSettings(func(s *Settings) { s.MaxWorkers = 5 }))
	var records []events.SQSMessage
	for i := 0; i < 5; i++ {
		records = append(records, rec(fmt.Sprintf("m%d", i), fmt.Sprintf("s%d", i), "oi", int64(i)))
	}

	started := time.Now()
	resp, _ := env.proc.ProcessBatch(ctxWithTimeout(t, 2*time.Minute), batch(records...))

	if elapsed := time.Since(started); elapsed > 600*time.Millisecond {
		t.Fatalf("lote levou %v; sequencial levaria ~1s", elapsed)
	}
	assertIDs(t, "falhas", failedIDs(resp))
}

func TestProcessBatch_EmptyBatchReturnsEmptySlice(t *testing.T) {
	env := newTestEnv(t, &fakeModel{})
	resp, _ := env.proc.ProcessBatch(context.Background(), batch())
	out, _ := json.Marshal(resp)
	if !strings.Contains(string(out), `"batchItemFailures":[]`) {
		t.Fatalf("JSON = %s; esperado lista vazia, não null", out)
	}
}

func TestMetrics_FlushEMF(t *testing.T) {
	m := NewMetrics("Chatbot/Consumer", map[string]string{"Service": "chat-consumer"})
	m.Add("X", 1, "Count")
	var buf bytes.Buffer
	if err := m.Flush(&buf); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		AWS struct {
			CloudWatchMetrics []struct {
				Namespace string
				Metrics   []struct{ Name, Unit string }
			}
		} `json:"_aws"`
		X float64
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("EMF inválido: %v (%s)", err, buf.String())
	}
	if doc.AWS.CloudWatchMetrics[0].Namespace != "Chatbot/Consumer" || doc.X != 1 {
		t.Fatalf("EMF = %s", buf.String())
	}
}

func TestLogs_DoNotContainUserText(t *testing.T) {
	env := newTestEnv(t, &fakeModel{})
	env.proc.ProcessBatch(context.Background(), batch(rec("m1", "s1", "segredo123", 0)))
	if strings.Contains(env.logs.String(), "segredo123") {
		t.Fatalf("log contém texto do usuário: %s", env.logs.String())
	}
	if !strings.Contains(env.logs.String(), `"msg":"message_processed"`) {
		t.Fatalf("log message_processed ausente: %s", env.logs.String())
	}
}
