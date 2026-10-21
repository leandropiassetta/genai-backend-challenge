package main

// Implementações em memória das portas, usadas nos testes e na simulação local.
// SIMULAÇÃO: nada aqui fala com a AWS.

import (
	"context"
	"io"
	"sort"
	"sync"
)

// runLocalSimulation é implementada na próxima parte (go run . -local).
func runLocalSimulation(_ io.Writer) error { return nil }

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
