package main

import (
	"errors"
	"strings"
	"testing"

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
