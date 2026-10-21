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
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-lambda-go/events"
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

func main() {}
