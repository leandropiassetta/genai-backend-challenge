package main

// Implementações em memória das portas, usadas nos testes e na simulação local.
// SIMULAÇÃO: nada aqui fala com a AWS.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

// scriptedModel simula o Bedrock: responde citando quantas trocas anteriores
// recebeu (prova que o histórico foi injetado) e simula throttling quando a
// mensagem contém "boom".
type scriptedModel struct{}

func (scriptedModel) Complete(_ context.Context, _ string, msgs []ChatMessage) (Completion, error) {
	last := msgs[len(msgs)-1].Content
	if strings.Contains(last, "boom") {
		return Completion{}, errors.New("bedrock: ThrottlingException (simulado)")
	}
	return Completion{
		Text:         fmt.Sprintf("[simulado | %d trocas anteriores] %s", (len(msgs)-1)/2, last),
		InputTokens:  120,
		OutputTokens: 30,
		StopReason:   "end_turn",
	}, nil
}

// runLocalSimulation roda o Processor real com dependências em memória:
// um lote com sucesso, throttling, ordem por sessão e mensagem inválida, e
// depois a reentrega de uma mensagem já respondida.
func runLocalSimulation(w io.Writer) error {
	store, pub, sink := NewInMemorySessionStore(), &InMemoryPublisher{}, &InMemoryPoisonSink{}
	settings := DefaultSettings()
	settings.MinRemaining = time.Second
	proc := NewProcessor(settings, scriptedModel{}, store, pub, sink, DefaultSystemPrompt,
		NewLogger(w, slog.LevelInfo), w)

	now := time.Now().UnixMilli()
	record := func(id, session, text string, ageMs int64) events.SQSMessage {
		body, _ := json.Marshal(map[string]string{"session_id": session, "message": text})
		return events.SQSMessage{MessageId: id, Body: string(body), Attributes: map[string]string{
			"SentTimestamp":           strconv.FormatInt(now-ageMs, 10),
			"ApproximateReceiveCount": "1",
		}}
	}
	first := events.SQSEvent{Records: []events.SQSMessage{
		record("m1", "s-1", "Oi, tenho uma dúvida na fatura", 300),
		record("m2", "s-1", "Por que veio mais caro este mês?", 200),
		record("m3", "s-2", "boom", 150),
		record("m4", "s-2", "esta volta junto com a m3 para manter a ordem", 100),
		{MessageId: "m5", Body: "isto não é json"},
	}}
	redelivery := first.Records[1]
	redelivery.Attributes = map[string]string{
		"SentTimestamp":           redelivery.Attributes["SentTimestamp"],
		"ApproximateReceiveCount": "2",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute) // imita o deadline da Lambda
	defer cancel()

	steps := []struct {
		title string
		event events.SQSEvent
	}{
		{"1ª entrega: lote com 5 mensagens", first},
		{"2ª entrega: SQS reentrega a m2 (idempotência, sem chamar o modelo)", events.SQSEvent{Records: []events.SQSMessage{redelivery}}},
	}
	for _, step := range steps {
		fmt.Fprintf(w, "\n=== %s ===\n", step.title)
		resp, err := proc.ProcessBatch(ctx, step.event)
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(resp.BatchItemFailures))
		for _, f := range resp.BatchItemFailures {
			ids = append(ids, f.ItemIdentifier)
		}
		sort.Strings(ids)
		fmt.Fprintf(w, "Falhas devolvidas à fila: %v\n", ids)
	}

	fmt.Fprintln(w, "\n=== Respostas publicadas ===")
	for _, p := range pub.Sent() {
		fmt.Fprintf(w, "  %s %s: %s\n", p.SessionID, p.MessageID, p.Answer)
	}
	fmt.Fprintln(w, "\n=== Enviadas à DLQ ===")
	for _, r := range sink.Received() {
		fmt.Fprintf(w, "  %s (%s)\n", r.MessageID, r.Reason)
	}
	return nil
}

type InMemorySessionStore struct {
	mu   sync.Mutex
	data map[string]map[string]Exchange
}

func NewInMemorySessionStore() *InMemorySessionStore {
	return &InMemorySessionStore{data: map[string]map[string]Exchange{}}
}

func (s *InMemorySessionStore) GetExchange(_ context.Context, sessionID, key string) (*Exchange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ex, ok := s.data[sessionID][key]
	if !ok {
		return nil, nil
	}
	return &ex, nil
}

func (s *InMemorySessionStore) RecentExchanges(_ context.Context, sessionID, beforeKey string, limit int) ([]Exchange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for k := range s.data[sessionID] {
		if k < beforeKey {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) > limit {
		keys = keys[len(keys)-limit:]
	}
	out := make([]Exchange, 0, len(keys))
	for _, k := range keys {
		out = append(out, s.data[sessionID][k])
	}
	return out, nil
}

func (s *InMemorySessionStore) SaveExchange(_ context.Context, sessionID, key string, ex Exchange) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data[sessionID] == nil {
		s.data[sessionID] = map[string]Exchange{}
	}
	if _, exists := s.data[sessionID][key]; exists {
		return false, nil
	}
	s.data[sessionID][key] = ex
	return true, nil
}

type PublishedAnswer struct {
	SessionID, MessageID, Answer string
}

type InMemoryPublisher struct {
	mu   sync.Mutex
	sent []PublishedAnswer
}

func (p *InMemoryPublisher) Publish(_ context.Context, msg InboundMessage, answer string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, PublishedAnswer{SessionID: msg.SessionID, MessageID: msg.MessageID, Answer: answer})
	return nil
}

func (p *InMemoryPublisher) Sent() []PublishedAnswer {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]PublishedAnswer(nil), p.sent...)
}

type RejectedMessage struct {
	MessageID, Reason string
}

type InMemoryPoisonSink struct {
	mu       sync.Mutex
	received []RejectedMessage
}

func (s *InMemoryPoisonSink) Send(_ context.Context, messageID, _, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received = append(s.received, RejectedMessage{MessageID: messageID, Reason: reason})
	return nil
}

func (s *InMemoryPoisonSink) Received() []RejectedMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RejectedMessage(nil), s.received...)
}
