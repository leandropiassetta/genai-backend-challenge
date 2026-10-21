// Consumer Lambda SQS -> Bedrock (versão corrigida).
//
// Este arquivo concentra a lógica que o avaliador compara com o código original:
// validação da mensagem, montagem do prompt com o contexto da sessão e (nas
// próximas partes) a orquestração do lote com partial batch failure.
// Os adaptadores AWS ficam em aws.go e a observabilidade em observability.go.
//
// Suposições sobre o sistema estão documentadas no README.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
)

// DefaultSystemPrompt é o prompt original do desafio. Em produção ele é
// substituído pelo prompt do Exercício 3 via SYSTEM_PROMPT_PATH.
const DefaultSystemPrompt = "Você é um assistente virtual de suporte ao cliente. " +
	"Responda de forma objetiva e cordial. Não forneça informações incorretas."

// ---------------------------------------------------------------------------
// Configuração
// ---------------------------------------------------------------------------

type Settings struct {
	ModelID     string
	MaxTokens   int
	Temperature float64

	HistoryMaxTurns int
	HistoryMaxChars int
	MaxInputChars   int
	MaxWorkers      int

	// MinRemaining é a margem mínima de tempo da Lambda para iniciar uma
	// mensagem. Deve cobrir o timeout do Bedrock + persistência + publicação.
	MinRemaining   time.Duration
	BedrockTimeout time.Duration
	SessionTTL     time.Duration

	SessionsTable    string
	ResponseQueueURL string
	DLQURL           string
	PromptVersion    string
}

func DefaultSettings() Settings {
	return Settings{
		ModelID:     "anthropic.claude-3-sonnet-20240229-v1:0",
		MaxTokens:   1024, // o original usava 2048: mais tokens de saída = mais latência
		Temperature: 0.3,

		HistoryMaxTurns: 10,
		HistoryMaxChars: 12000,
		MaxInputChars:   4000,
		MaxWorkers:      5,

		MinRemaining:   25 * time.Second,
		BedrockTimeout: 20 * time.Second,
		SessionTTL:     7 * 24 * time.Hour,

		PromptVersion: "v1",
	}
}

// SettingsFromEnv parte dos padrões e aplica as variáveis de ambiente presentes.
// Um valor numérico inválido é erro de deploy: falhar cedo é melhor do que
// rodar com um padrão silencioso.
func SettingsFromEnv() (Settings, error) {
	s := DefaultSettings()
	var errs []string

	str := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}
	integer := func(key string, dst *int) {
		if v, ok := os.LookupEnv(key); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, key)
				return
			}
			*dst = n
		}
	}
	duration := func(key string, unit time.Duration, dst *time.Duration) {
		var n int
		if _, ok := os.LookupEnv(key); !ok {
			return
		}
		before := len(errs)
		integer(key, &n)
		if len(errs) == before {
			*dst = time.Duration(n) * unit
		}
	}

	str("MODEL_ID", &s.ModelID)
	integer("MAX_TOKENS", &s.MaxTokens)
	if v, ok := os.LookupEnv("TEMPERATURE"); ok {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			errs = append(errs, "TEMPERATURE")
		} else {
			s.Temperature = f
		}
	}
	integer("HISTORY_MAX_TURNS", &s.HistoryMaxTurns)
	integer("HISTORY_MAX_CHARS", &s.HistoryMaxChars)
	integer("MAX_INPUT_CHARS", &s.MaxInputChars)
	integer("MAX_WORKERS", &s.MaxWorkers)
	duration("MIN_REMAINING_MS", time.Millisecond, &s.MinRemaining)
	duration("BEDROCK_TIMEOUT_MS", time.Millisecond, &s.BedrockTimeout)
	duration("SESSION_TTL_S", time.Second, &s.SessionTTL)
	str("SESSIONS_TABLE", &s.SessionsTable)
	str("RESPONSE_QUEUE_URL", &s.ResponseQueueURL)
	str("DLQ_URL", &s.DLQURL)
	str("PROMPT_VERSION", &s.PromptVersion)

	if len(errs) > 0 {
		return Settings{}, fmt.Errorf("variáveis de ambiente inválidas: %s", strings.Join(errs, ", "))
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// Domínio
// ---------------------------------------------------------------------------

// NonRetriableError marca falhas que nunca vão dar certo com uma nova tentativa
// (mensagem malformada, requisição rejeitada pelo modelo). Essas mensagens vão
// direto para a DLQ em vez de queimar retries e tokens. Qualquer outro erro é
// tratado como transitório e a mensagem volta para a fila.
type NonRetriableError struct {
	Reason string
	Err    error
}

func (e *NonRetriableError) Error() string {
	if e.Err != nil {
		return e.Reason + ": " + e.Err.Error()
	}
	return e.Reason
}

func (e *NonRetriableError) Unwrap() error { return e.Err }

type InboundMessage struct {
	MessageID     string
	SessionID     string
	Text          string
	CorrelationID string
	RawBody       string
	SentAtMs      int64
	ReceiveCount  int
}

// ExchangeKey é determinística entre reentregas (SentTimestamp e messageId não
// mudam), então serve ao mesmo tempo como chave de idempotência e de ordenação
// do histórico.
func (m InboundMessage) ExchangeKey() string {
	return fmt.Sprintf("%013d#%s", m.SentAtMs, m.MessageID)
}

// Exchange é uma troca completa (pergunta do usuário + resposta do assistente).
type Exchange struct {
	MessageID     string
	UserText      string
	AssistantText string
	CreatedAtMs   int64
}

type Completion struct {
	Text         string
	InputTokens  int
	OutputTokens int
	StopReason   string
}

// ChatMessage segue o formato da Messages API da Anthropic.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messageBody struct {
	SessionID     *string `json:"session_id"`
	Message       *string `json:"message"`
	CorrelationID string  `json:"correlation_id"`
}

func ParseRecord(r events.SQSMessage, maxInputChars int) (InboundMessage, error) {
	var raw any
	if err := json.Unmarshal([]byte(r.Body), &raw); err != nil {
		return InboundMessage{}, &NonRetriableError{Reason: "invalid_json", Err: err}
	}
	if _, ok := raw.(map[string]any); !ok {
		return InboundMessage{}, &NonRetriableError{Reason: "body_not_object"}
	}

	var body messageBody
	if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
		// Campo com tipo errado, ex.: "message": 123.
		return InboundMessage{}, &NonRetriableError{Reason: "invalid_field_type", Err: err}
	}
	if body.SessionID == nil || strings.TrimSpace(*body.SessionID) == "" {
		return InboundMessage{}, &NonRetriableError{Reason: "missing_session_id"}
	}
	if body.Message == nil || strings.TrimSpace(*body.Message) == "" {
		return InboundMessage{}, &NonRetriableError{Reason: "missing_message"}
	}
	text := strings.TrimSpace(*body.Message)
	if utf8.RuneCountInString(text) > maxInputChars {
		return InboundMessage{}, &NonRetriableError{Reason: "message_too_long"}
	}

	// Atributos ausentes viram zero: a mensagem ainda é processável.
	sentAt, _ := strconv.ParseInt(r.Attributes["SentTimestamp"], 10, 64)
	receiveCount, err := strconv.Atoi(r.Attributes["ApproximateReceiveCount"])
	if err != nil {
		receiveCount = 1
	}
	correlationID := body.CorrelationID
	if correlationID == "" {
		correlationID = r.MessageId
	}

	return InboundMessage{
		MessageID:     r.MessageId,
		SessionID:     strings.TrimSpace(*body.SessionID),
		Text:          text,
		CorrelationID: correlationID,
		RawBody:       r.Body,
		SentAtMs:      sentAt,
		ReceiveCount:  receiveCount,
	}, nil
}

// BuildMessages monta o histórico em ordem cronológica seguido da mensagem atual.
// Quando o orçamento estoura, descarta as trocas mais antigas primeiro.
// Caracteres são um proxy barato de tokens; prompt maior significa mais latência
// de prefill e mais custo, por isso o limite é conservador.
func BuildMessages(history []Exchange, userText string, maxChars int) []ChatMessage {
	budget := maxChars - utf8.RuneCountInString(userText)
	start := len(history)
	for start > 0 {
		ex := history[start-1]
		cost := utf8.RuneCountInString(ex.UserText) + utf8.RuneCountInString(ex.AssistantText)
		if cost > budget {
			break
		}
		budget -= cost
		start--
	}

	msgs := make([]ChatMessage, 0, 2*(len(history)-start)+1)
	for _, ex := range history[start:] {
		msgs = append(msgs,
			ChatMessage{Role: "user", Content: ex.UserText},
			ChatMessage{Role: "assistant", Content: ex.AssistantText},
		)
	}
	return append(msgs, ChatMessage{Role: "user", Content: userText})
}

// ---------------------------------------------------------------------------
// Portas: uma interface por dependência externa
// ---------------------------------------------------------------------------

type ChatModel interface {
	Complete(ctx context.Context, system string, msgs []ChatMessage) (Completion, error)
}

type SessionStore interface {
	// GetExchange retorna nil, nil quando a troca não existe.
	GetExchange(ctx context.Context, sessionID, key string) (*Exchange, error)
	// RecentExchanges retorna até limit trocas com chave < beforeKey, em ordem cronológica.
	RecentExchanges(ctx context.Context, sessionID, beforeKey string, limit int) ([]Exchange, error)
	// SaveExchange grava só se a chave ainda não existe; stored=false indica que outra entrega já gravou.
	SaveExchange(ctx context.Context, sessionID, key string, ex Exchange) (stored bool, err error)
}

type Publisher interface {
	Publish(ctx context.Context, msg InboundMessage, answer string) error
}

type PoisonSink interface {
	Send(ctx context.Context, messageID, rawBody, reason string) error
}

// ---------------------------------------------------------------------------
// Orquestração
// ---------------------------------------------------------------------------

type Processor struct {
	settings     Settings
	model        ChatModel
	sessions     SessionStore
	publisher    Publisher
	poison       PoisonSink // opcional
	systemPrompt string
	logger       *slog.Logger
	metricsOut   io.Writer

	mu          sync.Mutex
	lastMetrics *Metrics
}

func NewProcessor(s Settings, m ChatModel, st SessionStore, pub Publisher, poison PoisonSink,
	systemPrompt string, logger *slog.Logger, metricsOut io.Writer) *Processor {
	return &Processor{
		settings: s, model: m, sessions: st, publisher: pub, poison: poison,
		systemPrompt: systemPrompt, logger: logger, metricsOut: metricsOut,
	}
}

// LastMetrics expõe as métricas da última invocação (usado nos testes).
func (p *Processor) LastMetrics() *Metrics {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastMetrics
}

// ProcessBatch é o handler da Lambda. Nunca retorna erro: um erro aqui faria o
// SQS reentregar o lote inteiro, que é exatamente o defeito do código original.
// Cada falha é reportada individualmente em BatchItemFailures (exige
// ReportBatchItemFailures no event source mapping).
func (p *Processor) ProcessBatch(ctx context.Context, ev events.SQSEvent) (events.SQSEventResponse, error) {
	started := time.Now()
	metrics := NewMetrics(metricsNamespace, map[string]string{
		"Service":       serviceName,
		"PromptVersion": p.settings.PromptVersion,
	})
	p.mu.Lock()
	p.lastMetrics = metrics
	p.mu.Unlock()

	var failMu sync.Mutex
	failures := make([]events.SQSBatchItemFailure, 0) // [] e não null no JSON
	fail := func(id string) {
		failMu.Lock()
		failures = append(failures, events.SQSBatchItemFailure{ItemIdentifier: id})
		failMu.Unlock()
	}

	// Agrupa por sessão preservando a ordem de chegada.
	var order []string
	groups := map[string][]InboundMessage{}
	for _, r := range ev.Records {
		msg, err := ParseRecord(r, p.settings.MaxInputChars)
		if err != nil {
			if !p.quarantine(ctx, r.MessageId, r.Body, reasonOf(err), metrics) {
				fail(r.MessageId)
			}
			continue
		}
		if _, seen := groups[msg.SessionID]; !seen {
			order = append(order, msg.SessionID)
		}
		groups[msg.SessionID] = append(groups[msg.SessionID], msg)
	}

	// Sessões diferentes em paralelo, mensagens da mesma sessão em sequência.
	sem := make(chan struct{}, max(1, p.settings.MaxWorkers))
	var wg sync.WaitGroup
	for _, sessionID := range order {
		msgs := groups[sessionID]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			for _, id := range p.processSession(ctx, msgs, metrics) {
				fail(id)
			}
		}()
	}
	wg.Wait()

	metrics.Add("BatchSize", float64(len(ev.Records)), "Count")
	metrics.Add("MessagesFailed", float64(len(failures)), "Count")
	if err := metrics.Flush(p.metricsOut); err != nil {
		p.logger.ErrorContext(ctx, "metrics_flush_failed", "error", err.Error())
	}
	p.logger.InfoContext(ctx, "batch_processed",
		"batch_size", len(ev.Records),
		"sessions", len(order),
		"failed", len(failures),
		"duration_ms", time.Since(started).Milliseconds(),
	)
	return events.SQSEventResponse{BatchItemFailures: failures}, nil
}

// processSession processa as mensagens de uma sessão em ordem e retorna os ids
// que devem voltar para a fila.
func (p *Processor) processSession(ctx context.Context, msgs []InboundMessage, metrics *Metrics) []string {
	var failed []string
	for _, msg := range msgs {
		if len(failed) > 0 {
			// Uma mensagem anterior da mesma sessão falhou: as seguintes voltam
			// também, para o usuário não receber a resposta da pergunta 2 antes
			// da pergunta 1.
			p.logger.WarnContext(ctx, "returned_preserving_session_order",
				"message_id", msg.MessageID, "session_id", msg.SessionID, "blocked_by", failed[0])
			metrics.Add("OrderReturns", 1, "Count")
			failed = append(failed, msg.MessageID)
			continue
		}
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < p.settings.MinRemaining {
			// Devolver agora é melhor do que deixar a Lambda estourar o timeout,
			// o que faria o SQS reentregar o lote inteiro.
			p.logger.WarnContext(ctx, "deadline_near_returning_to_queue",
				"message_id", msg.MessageID, "session_id", msg.SessionID,
				"remaining_ms", time.Until(deadline).Milliseconds())
			metrics.Add("DeadlineReturns", 1, "Count")
			failed = append(failed, msg.MessageID)
			continue
		}

		err := p.safeProcessMessage(ctx, msg, metrics)
		if err == nil {
			continue
		}
		var nre *NonRetriableError
		if errors.As(err, &nre) {
			if !p.quarantine(ctx, msg.MessageID, msg.RawBody, nre.Reason, metrics) {
				failed = append(failed, msg.MessageID)
			}
			continue
		}
		p.logger.ErrorContext(ctx, "message_failed_will_retry",
			"message_id", msg.MessageID, "session_id", msg.SessionID,
			"correlation_id", msg.CorrelationID, "receive_count", msg.ReceiveCount,
			"error", err.Error())
		metrics.Add("MessagesRetried", 1, "Count")
		failed = append(failed, msg.MessageID)
	}
	return failed
}

// safeProcessMessage converte um panic em erro transitório: um bug num
// adaptador não pode derrubar a Lambda e, com ela, o lote inteiro.
func (p *Processor) safeProcessMessage(ctx context.Context, msg InboundMessage, metrics *Metrics) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return p.processMessage(ctx, msg, metrics)
}

func (p *Processor) processMessage(ctx context.Context, msg InboundMessage, metrics *Metrics) error {
	key := msg.ExchangeKey()
	existing, err := p.sessions.GetExchange(ctx, msg.SessionID, key)
	if err != nil {
		return fmt.Errorf("get_exchange: %w", err)
	}

	logAttrs := []any{
		"message_id", msg.MessageID, "session_id", msg.SessionID,
		"correlation_id", msg.CorrelationID, "receive_count", msg.ReceiveCount,
		"input_chars", utf8.RuneCountInString(msg.Text),
		"model_id", p.settings.ModelID, "prompt_version", p.settings.PromptVersion,
	}

	var answer string
	if existing != nil {
		// Reentrega de algo já respondido (ex.: a Lambda morreu depois de
		// persistir). Reaproveita a resposta: zero tokens gastos.
		answer = existing.AssistantText
		metrics.Add("DuplicatesSkipped", 1, "Count")
		logAttrs = append(logAttrs, "duplicate", true)
	} else {
		history, err := p.sessions.RecentExchanges(ctx, msg.SessionID, key, p.settings.HistoryMaxTurns)
		if err != nil {
			return fmt.Errorf("recent_exchanges: %w", err)
		}
		msgs := BuildMessages(history, msg.Text, p.settings.HistoryMaxChars)

		t0 := time.Now()
		completion, err := p.model.Complete(ctx, p.systemPrompt, msgs)
		latency := time.Since(t0)
		if err != nil {
			return err
		}
		metrics.Add("ModelLatency", float64(latency.Milliseconds()), "Milliseconds")
		metrics.Add("InputTokens", float64(completion.InputTokens), "Count")
		metrics.Add("OutputTokens", float64(completion.OutputTokens), "Count")
		if completion.StopReason == "max_tokens" {
			metrics.Add("TruncatedResponses", 1, "Count")
		}

		answer = completion.Text
		stored, err := p.sessions.SaveExchange(ctx, msg.SessionID, key, Exchange{
			MessageID: msg.MessageID, UserText: msg.Text, AssistantText: answer,
			CreatedAtMs: time.Now().UnixMilli(),
		})
		if err != nil {
			return fmt.Errorf("save_exchange: %w", err)
		}
		if !stored {
			// Entrega concorrente ganhou a corrida: usamos a resposta dela para
			// que histórico e mensagem publicada sejam a mesma coisa.
			if winner, err := p.sessions.GetExchange(ctx, msg.SessionID, key); err == nil && winner != nil {
				answer = winner.AssistantText
			}
		}
		logAttrs = append(logAttrs,
			"history_turns", (len(msgs)-1)/2,
			"model_latency_ms", latency.Milliseconds(),
			"input_tokens", completion.InputTokens,
			"output_tokens", completion.OutputTokens,
			"stop_reason", completion.StopReason,
		)
	}

	if err := p.publisher.Publish(ctx, msg, answer); err != nil {
		return fmt.Errorf("publish: %w", err)
	}

	if msg.SentAtMs > 0 {
		// Tempo desde que o usuário enviou, incluindo a espera na fila: é aqui
		// que aparece boa parte dos picos de latência, não só no modelo.
		metrics.Add("EndToEndLatency", float64(time.Now().UnixMilli()-msg.SentAtMs), "Milliseconds")
	}
	metrics.Add("MessagesProcessed", 1, "Count")
	logAttrs = append(logAttrs, "answer_chars", utf8.RuneCountInString(answer))
	p.logger.InfoContext(ctx, "message_processed", logAttrs...)
	return nil
}

// quarantine tira da fila uma mensagem irrecuperável. Retorna true se ela foi
// enviada à DLQ com segurança; false significa "devolver à fila".
func (p *Processor) quarantine(ctx context.Context, messageID, rawBody, reason string, metrics *Metrics) bool {
	metrics.Add("MessagesRejected", 1, "Count")
	p.logger.ErrorContext(ctx, "message_rejected",
		"message_id", messageID, "reason", reason, "body_chars", utf8.RuneCountInString(rawBody))
	if p.poison == nil {
		// Sem DLQ_URL: devolvemos à fila e o redrive do SQS leva para a DLQ após
		// maxReceiveCount. Mais lento, mas nunca descarta dado em silêncio.
		return false
	}
	if err := p.poison.Send(ctx, messageID, rawBody, reason); err != nil {
		p.logger.ErrorContext(ctx, "poison_sink_failed", "message_id", messageID, "error", err.Error())
		return false
	}
	return true
}

func reasonOf(err error) string {
	var nre *NonRetriableError
	if errors.As(err, &nre) {
		return nre.Reason
	}
	return err.Error()
}

func main() {
	local := flag.Bool("local", false, "roda uma simulação com implementações em memória, sem AWS")
	flag.Parse()

	if *local {
		if err := runLocalSimulation(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	settings, err := SettingsFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// Fora do handler: roda uma vez por container, na fase INIT da Lambda.
	processor, err := BuildAWSProcessor(context.Background(), settings)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	lambda.Start(processor.ProcessBatch)
}
