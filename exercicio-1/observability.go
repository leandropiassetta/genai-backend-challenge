package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"
)

const (
	serviceName      = "chat-consumer"
	metricsNamespace = "Chatbot/Consumer"
)

// NewLogger cria um logger JSON. Regra do serviço: nunca logar o texto do
// usuário nem a resposta do modelo (dados pessoais, LGPD, e custo de
// CloudWatch Logs), apenas ids, tamanhos e tempos.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})).With("service", serviceName)
}

// Metrics acumula as métricas de uma invocação e as emite como um único
// documento no CloudWatch Embedded Metric Format (EMF). O CloudWatch extrai as
// métricas do log de forma assíncrona, sem chamada de rede no caminho crítico
// (ao contrário de PutMetricData).
type Metrics struct {
	mu        sync.Mutex
	namespace string
	dims      map[string]string
	order     []string
	values    map[string][]float64
	units     map[string]string
}

func NewMetrics(namespace string, dims map[string]string) *Metrics {
	return &Metrics{
		namespace: namespace,
		dims:      dims,
		values:    map[string][]float64{},
		units:     map[string]string{},
	}
}

// Add registra um valor. unit: "Count" ou "Milliseconds".
func (m *Metrics) Add(name string, value float64, unit string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.values[name]; !ok {
		m.order = append(m.order, name)
	}
	m.values[name] = append(m.values[name], value)
	m.units[name] = unit
}

func (m *Metrics) Values(name string) []float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]float64(nil), m.values[name]...)
}

func (m *Metrics) Flush(w io.Writer) error {
	m.mu.Lock()
	if len(m.order) == 0 {
		m.mu.Unlock()
		return nil
	}
	dimKeys := make([]string, 0, len(m.dims))
	for k := range m.dims {
		dimKeys = append(dimKeys, k)
	}
	sort.Strings(dimKeys)

	defs := make([]map[string]string, 0, len(m.order))
	doc := map[string]any{}
	for _, name := range m.order {
		defs = append(defs, map[string]string{"Name": name, "Unit": m.units[name]})
		if v := m.values[name]; len(v) == 1 {
			doc[name] = v[0]
		} else {
			doc[name] = append([]float64(nil), v...)
		}
	}
	for k, v := range m.dims {
		doc[k] = v
	}
	doc["_aws"] = map[string]any{
		"Timestamp": time.Now().UnixMilli(),
		"CloudWatchMetrics": []map[string]any{{
			"Namespace":  m.namespace,
			"Dimensions": [][]string{dimKeys},
			"Metrics":    defs,
		}},
	}
	m.mu.Unlock()

	line, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = w.Write(append(line, '\n'))
	return err
}
