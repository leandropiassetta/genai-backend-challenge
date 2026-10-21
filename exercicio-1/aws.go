package main

// Adaptadores AWS das portas definidas em codigo_corrigido.go.
// Cada adaptador depende de uma interface mínima com a assinatura do client do
// SDK, o que permite testá-lo com fakes sem acessar a AWS.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	btypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	stypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

type BedrockAPI interface {
	InvokeModel(ctx context.Context, in *bedrockruntime.InvokeModelInput, opts ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelOutput, error)
}

type DynamoAPI interface {
	GetItem(ctx context.Context, in *dynamodb.GetItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	Query(ctx context.Context, in *dynamodb.QueryInput, opts ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, opts ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
}

type SQSAPI interface {
	SendMessage(ctx context.Context, in *sqs.SendMessageInput, opts ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
}

// ---------------------------------------------------------------------------
// Bedrock
// ---------------------------------------------------------------------------

type BedrockChatModel struct {
	client      BedrockAPI
	modelID     string
	maxTokens   int
	temperature float64
	timeout     time.Duration
}

func NewBedrockChatModel(c BedrockAPI, s Settings) *BedrockChatModel {
	return &BedrockChatModel{client: c, modelID: s.ModelID, maxTokens: s.MaxTokens, temperature: s.Temperature, timeout: s.BedrockTimeout}
}

type invokeRequest struct {
	AnthropicVersion string        `json:"anthropic_version"`
	MaxTokens        int           `json:"max_tokens"`
	Temperature      float64       `json:"temperature"`
	System           string        `json:"system"`
	Messages         []ChatMessage `json:"messages"`
}

type invokeResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	StopReason string `json:"stop_reason"`
}

func (m *BedrockChatModel) Complete(ctx context.Context, system string, msgs []ChatMessage) (Completion, error) {
	body, err := json.Marshal(invokeRequest{
		AnthropicVersion: "bedrock-2023-05-31",
		MaxTokens:        m.maxTokens,
		Temperature:      m.temperature,
		System:           system,
		Messages:         msgs,
	})
	if err != nil {
		return Completion{}, &NonRetriableError{Reason: "bedrock_request_encoding", Err: err}
	}

	// O original não tinha timeout: uma chamada lenta segurava a Lambda até o
	// limite e derrubava o lote inteiro.
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	out, err := m.client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(m.modelID),
		Body:        body,
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
	})
	if err != nil {
		// ValidationException é bug nosso (prompt malformado, contexto acima do
		// limite): tentar de novo não resolve, então vai para a DLQ.
		// Throttling, indisponibilidade e timeout são transitórios. AccessDenied
		// também cai aqui de propósito: é erro de deploy, e preferimos segurar as
		// mensagens na fila (com alarme) a descartá-las.
		var ve *btypes.ValidationException
		if errors.As(err, &ve) {
			return Completion{}, &NonRetriableError{Reason: "bedrock_ValidationException", Err: err}
		}
		return Completion{}, fmt.Errorf("bedrock: %w", err)
	}

	var resp invokeResponse
	if err := json.Unmarshal(out.Body, &resp); err != nil {
		return Completion{}, fmt.Errorf("bedrock: resposta ilegível: %w", err)
	}
	var text strings.Builder
	for _, block := range resp.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	answer := strings.TrimSpace(text.String())
	if answer == "" {
		return Completion{}, errors.New("bedrock: resposta vazia")
	}
	return Completion{
		Text:         answer,
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		StopReason:   resp.StopReason,
	}, nil
}

// ---------------------------------------------------------------------------
// DynamoDB: um item por troca (PK session_id, SK "<SentTimestamp>#<messageId>")
// ---------------------------------------------------------------------------

type DynamoSessionStore struct {
	client DynamoAPI
	table  string
	ttl    time.Duration
}

func NewDynamoSessionStore(c DynamoAPI, table string, ttl time.Duration) *DynamoSessionStore {
	return &DynamoSessionStore{client: c, table: table, ttl: ttl}
}

func dynamoKey(sessionID, key string) map[string]dtypes.AttributeValue {
	return map[string]dtypes.AttributeValue{
		"session_id": &dtypes.AttributeValueMemberS{Value: sessionID},
		"sk":         &dtypes.AttributeValueMemberS{Value: key},
	}
}

func (s *DynamoSessionStore) GetExchange(ctx context.Context, sessionID, key string) (*Exchange, error) {
	out, err := s.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(s.table),
		Key:            dynamoKey(sessionID, key),
		ConsistentRead: aws.Bool(true), // idempotência precisa ver a escrita mais recente
	})
	if err != nil {
		return nil, err
	}
	if len(out.Item) == 0 {
		return nil, nil
	}
	ex := toExchange(out.Item)
	return &ex, nil
}

func (s *DynamoSessionStore) RecentExchanges(ctx context.Context, sessionID, beforeKey string, limit int) ([]Exchange, error) {
	// sk < beforeKey: numa reentrega atrasada, o modelo não vê mensagens que o
	// usuário mandou depois desta. Query reversa com Limit lê só o necessário.
	out, err := s.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.table),
		KeyConditionExpression: aws.String("session_id = :s AND sk < :k"),
		ExpressionAttributeValues: map[string]dtypes.AttributeValue{
			":s": &dtypes.AttributeValueMemberS{Value: sessionID},
			":k": &dtypes.AttributeValueMemberS{Value: beforeKey},
		},
		ScanIndexForward: aws.Bool(false),
		Limit:            aws.Int32(int32(limit)),
	})
	if err != nil {
		return nil, err
	}
	exchanges := make([]Exchange, len(out.Items))
	for i, item := range out.Items {
		exchanges[len(out.Items)-1-i] = toExchange(item)
	}
	return exchanges, nil
}

func (s *DynamoSessionStore) SaveExchange(ctx context.Context, sessionID, key string, ex Exchange) (bool, error) {
	item := dynamoKey(sessionID, key)
	item["message_id"] = &dtypes.AttributeValueMemberS{Value: ex.MessageID}
	item["user_text"] = &dtypes.AttributeValueMemberS{Value: ex.UserText}
	item["assistant_text"] = &dtypes.AttributeValueMemberS{Value: ex.AssistantText}
	item["created_at_ms"] = &dtypes.AttributeValueMemberN{Value: strconv.FormatInt(ex.CreatedAtMs, 10)}
	item["expires_at"] = &dtypes.AttributeValueMemberN{Value: strconv.FormatInt(ex.CreatedAtMs/1000+int64(s.ttl.Seconds()), 10)}

	_, err := s.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:           aws.String(s.table),
		Item:                item,
		ConditionExpression: aws.String("attribute_not_exists(sk)"),
	})
	var ccf *dtypes.ConditionalCheckFailedException
	if errors.As(err, &ccf) {
		return false, nil
	}
	return err == nil, err
}

func toExchange(item map[string]dtypes.AttributeValue) Exchange {
	str := func(k string) string {
		if v, ok := item[k].(*dtypes.AttributeValueMemberS); ok {
			return v.Value
		}
		return ""
	}
	createdAt := int64(0)
	if v, ok := item["created_at_ms"].(*dtypes.AttributeValueMemberN); ok {
		createdAt, _ = strconv.ParseInt(v.Value, 10, 64)
	}
	return Exchange{MessageID: str("message_id"), UserText: str("user_text"), AssistantText: str("assistant_text"), CreatedAtMs: createdAt}
}

// ---------------------------------------------------------------------------
// SQS: fila de respostas e DLQ
// ---------------------------------------------------------------------------

type SQSPublisher struct {
	client   SQSAPI
	queueURL string
	fifo     bool
}

func NewSQSPublisher(c SQSAPI, queueURL string) *SQSPublisher {
	return &SQSPublisher{client: c, queueURL: queueURL, fifo: strings.HasSuffix(queueURL, ".fifo")}
}

func (p *SQSPublisher) Publish(ctx context.Context, msg InboundMessage, answer string) error {
	body, err := json.Marshal(map[string]string{
		"session_id":     msg.SessionID,
		"in_reply_to":    msg.MessageID,
		"correlation_id": msg.CorrelationID,
		"message":        answer,
	})
	if err != nil {
		return err
	}
	in := &sqs.SendMessageInput{QueueUrl: aws.String(p.queueURL), MessageBody: aws.String(string(body))}
	if p.fifo {
		// O dedup nativo do FIFO (janela de 5 min) cobre reentregas rápidas.
		// Fora dela, o consumidor da fila deduplica por in_reply_to.
		in.MessageGroupId = aws.String(msg.SessionID)
		in.MessageDeduplicationId = aws.String(msg.MessageID)
	}
	_, err = p.client.SendMessage(ctx, in)
	return err
}

type SQSPoisonSink struct {
	client SQSAPI
	dlqURL string
}

func NewSQSPoisonSink(c SQSAPI, dlqURL string) *SQSPoisonSink {
	return &SQSPoisonSink{client: c, dlqURL: dlqURL}
}

func (s *SQSPoisonSink) Send(ctx context.Context, messageID, rawBody, reason string) error {
	if rawBody == "" {
		rawBody = "<empty>" // SQS rejeita corpo vazio
	}
	_, err := s.client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(s.dlqURL),
		MessageBody: aws.String(rawBody),
		MessageAttributes: map[string]stypes.MessageAttributeValue{
			"failure_reason":      {DataType: aws.String("String"), StringValue: aws.String(reason)},
			"original_message_id": {DataType: aws.String("String"), StringValue: aws.String(messageID)},
		},
	})
	return err
}

// ---------------------------------------------------------------------------
// Montagem com clients reais
// ---------------------------------------------------------------------------

// BuildAWSProcessor cria os clients uma única vez por container (o original os
// recriava a cada invocação, pagando handshake TLS toda vez).
func BuildAWSProcessor(ctx context.Context, s Settings) (*Processor, error) {
	var missing []string
	if s.SessionsTable == "" {
		missing = append(missing, "SESSIONS_TABLE")
	}
	if s.ResponseQueueURL == "" {
		missing = append(missing, "RESPONSE_QUEUE_URL")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("variáveis de ambiente obrigatórias ausentes: %s", strings.Join(missing, ", "))
	}

	systemPrompt := DefaultSystemPrompt
	if path := os.Getenv("SYSTEM_PROMPT_PATH"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("lendo SYSTEM_PROMPT_PATH: %w", err)
		}
		systemPrompt = string(b)
	}

	cfg, err := config.LoadDefaultConfig(ctx) // DynamoDB e SQS: retry padrão (3 tentativas)
	if err != nil {
		return nil, err
	}
	bedrock := bedrockruntime.NewFromConfig(cfg, func(o *bedrockruntime.Options) {
		// Poucas tentativas internas: o retry principal é a reentrega do SQS,
		// que não deixa a Lambda (e o resto do lote) parada esperando backoff.
		o.RetryMaxAttempts = 2
		o.RetryMode = aws.RetryModeAdaptive
		o.HTTPClient = awshttp.NewBuildableClient().WithDialerOptions(func(d *net.Dialer) {
			d.Timeout = 2 * time.Second
		})
	})
	sqsClient := sqs.NewFromConfig(cfg)

	var poison PoisonSink
	if s.DLQURL != "" {
		poison = NewSQSPoisonSink(sqsClient, s.DLQURL)
	}

	return NewProcessor(s,
		NewBedrockChatModel(bedrock, s),
		NewDynamoSessionStore(dynamodb.NewFromConfig(cfg), s.SessionsTable, s.SessionTTL),
		NewSQSPublisher(sqsClient, s.ResponseQueueURL),
		poison,
		systemPrompt,
		NewLogger(os.Stdout, logLevelFromEnv()),
		os.Stdout,
	), nil
}

func logLevelFromEnv() slog.Level {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(os.Getenv("LOG_LEVEL"))); err != nil {
		return slog.LevelInfo
	}
	return lvl
}
