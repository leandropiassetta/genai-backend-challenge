package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	btypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

// --- fakes dos clients do SDK ---------------------------------------------

type fakeBedrock struct {
	input *bedrockruntime.InvokeModelInput
	body  string
	err   error
}

func (f *fakeBedrock) InvokeModel(_ context.Context, in *bedrockruntime.InvokeModelInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.InvokeModelOutput, error) {
	f.input = in
	if f.err != nil {
		return nil, f.err
	}
	return &bedrockruntime.InvokeModelOutput{Body: []byte(f.body)}, nil
}

type fakeDynamo struct {
	putInput   *dynamodb.PutItemInput
	putErr     error
	queryInput *dynamodb.QueryInput
	queryItems []map[string]dtypes.AttributeValue
}

func (f *fakeDynamo) GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return &dynamodb.GetItemOutput{}, nil
}

func (f *fakeDynamo) Query(_ context.Context, in *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	f.queryInput = in
	return &dynamodb.QueryOutput{Items: f.queryItems}, nil
}

func (f *fakeDynamo) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	f.putInput = in
	return &dynamodb.PutItemOutput{}, f.putErr
}

type fakeSQS struct{ input *sqs.SendMessageInput }

func (f *fakeSQS) SendMessage(_ context.Context, in *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	f.input = in
	return &sqs.SendMessageOutput{}, nil
}

func testSettings() Settings {
	s := DefaultSettings()
	s.MaxTokens = 512
	s.BedrockTimeout = time.Second
	return s
}

var oneUserMsg = []ChatMessage{{Role: "user", Content: "oi"}}

// --- Bedrock ----------------------------------------------------------------

func TestBedrock_RequestBodyAndUsage(t *testing.T) {
	client := &fakeBedrock{body: `{"content":[{"type":"text","text":"Olá!"}],"usage":{"input_tokens":42,"output_tokens":3},"stop_reason":"end_turn"}`}

	got, err := NewBedrockChatModel(client, testSettings()).Complete(context.Background(), "SYS", oneUserMsg)
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if got != (Completion{Text: "Olá!", InputTokens: 42, OutputTokens: 3, StopReason: "end_turn"}) {
		t.Fatalf("Completion = %+v", got)
	}

	var sent map[string]any
	if err := json.Unmarshal(client.input.Body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["anthropic_version"] != "bedrock-2023-05-31" || sent["system"] != "SYS" ||
		sent["max_tokens"] != float64(512) || sent["temperature"] != 0.3 {
		t.Fatalf("corpo enviado = %v", sent)
	}
	if aws.ToString(client.input.ModelId) != DefaultSettings().ModelID {
		t.Fatalf("ModelId = %q", aws.ToString(client.input.ModelId))
	}
}

func TestBedrock_ErrorClassification(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		nonRetriable bool
	}{
		{"validation", &btypes.ValidationException{Message: aws.String("prompt inválido")}, true},
		{"throttling", &btypes.ThrottlingException{}, false},
		{"indisponível", &btypes.ServiceUnavailableException{}, false},
		{"sem permissão", &btypes.AccessDeniedException{}, false},
		{"timeout", context.DeadlineExceeded, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewBedrockChatModel(&fakeBedrock{err: tc.err}, testSettings()).Complete(context.Background(), "SYS", oneUserMsg)
			if err == nil {
				t.Fatal("esperado erro")
			}
			var nre *NonRetriableError
			if errors.As(err, &nre) != tc.nonRetriable {
				t.Fatalf("NonRetriable = %v, esperado %v (err: %v)", !tc.nonRetriable, tc.nonRetriable, err)
			}
			if tc.nonRetriable && nre.Reason != "bedrock_ValidationException" {
				t.Fatalf("Reason = %q", nre.Reason)
			}
		})
	}
}

func TestBedrock_IgnoresNonTextBlocksAndEmptyIsRetriable(t *testing.T) {
	model := NewBedrockChatModel(&fakeBedrock{body: `{"content":[{"type":"tool_use"},{"type":"text","text":"ok"}]}`}, testSettings())
	got, err := model.Complete(context.Background(), "SYS", oneUserMsg)
	if err != nil || got.Text != "ok" {
		t.Fatalf("Text = %q, err = %v", got.Text, err)
	}

	model = NewBedrockChatModel(&fakeBedrock{body: `{"content":[{"type":"text","text":"  "}]}`}, testSettings())
	_, err = model.Complete(context.Background(), "SYS", oneUserMsg)
	var nre *NonRetriableError
	if err == nil || errors.As(err, &nre) {
		t.Fatalf("resposta vazia deve ser erro transitório, veio %v", err)
	}
}

// --- DynamoDB ---------------------------------------------------------------

func TestDynamo_SaveConditional(t *testing.T) {
	client := &fakeDynamo{putErr: &dtypes.ConditionalCheckFailedException{}}
	store := NewDynamoSessionStore(client, "sessions", time.Hour)

	stored, err := store.SaveExchange(context.Background(), "s1", "k1", Exchange{MessageID: "m1", UserText: "u", AssistantText: "a", CreatedAtMs: 1760000000000})
	if stored || err != nil {
		t.Fatalf("stored = %v, err = %v; esperado false, nil", stored, err)
	}
	if aws.ToString(client.putInput.ConditionExpression) != "attribute_not_exists(sk)" {
		t.Fatalf("ConditionExpression = %q", aws.ToString(client.putInput.ConditionExpression))
	}
	if ttl := client.putInput.Item["expires_at"].(*dtypes.AttributeValueMemberN).Value; ttl != "1760003600" {
		t.Fatalf("expires_at = %s, esperado created_at + 1h em segundos", ttl)
	}
}

func dynamoItem(id, user, assistant string) map[string]dtypes.AttributeValue {
	return map[string]dtypes.AttributeValue{
		"message_id":     &dtypes.AttributeValueMemberS{Value: id},
		"user_text":      &dtypes.AttributeValueMemberS{Value: user},
		"assistant_text": &dtypes.AttributeValueMemberS{Value: assistant},
		"created_at_ms":  &dtypes.AttributeValueMemberN{Value: "1"},
	}
}

func TestDynamo_RecentExchangesQueryAndOrder(t *testing.T) {
	client := &fakeDynamo{queryItems: []map[string]dtypes.AttributeValue{
		dynamoItem("m2", "nova", "r2"), // Query reversa: mais nova primeiro
		dynamoItem("m1", "antiga", "r1"),
	}}
	got, err := NewDynamoSessionStore(client, "sessions", time.Hour).RecentExchanges(context.Background(), "s1", "k9", 10)
	if err != nil {
		t.Fatal(err)
	}
	in := client.queryInput
	if aws.ToString(in.KeyConditionExpression) != "session_id = :s AND sk < :k" ||
		aws.ToBool(in.ScanIndexForward) || aws.ToInt32(in.Limit) != 10 {
		t.Fatalf("query = %+v", in)
	}
	if len(got) != 2 || got[0].MessageID != "m1" || got[1].MessageID != "m2" {
		t.Fatalf("esperado ordem cronológica, veio %+v", got)
	}
}

// --- SQS --------------------------------------------------------------------

func TestSQSPublisher_FIFOSetsGroupAndDedup(t *testing.T) {
	msg := InboundMessage{MessageID: "m1", SessionID: "s1", CorrelationID: "c1"}

	client := &fakeSQS{}
	if err := NewSQSPublisher(client, "https://sqs/123/respostas.fifo").Publish(context.Background(), msg, "olá"); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(client.input.MessageGroupId) != "s1" || aws.ToString(client.input.MessageDeduplicationId) != "m1" {
		t.Fatalf("FIFO sem group/dedup: %+v", client.input)
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(aws.ToString(client.input.MessageBody)), &body); err != nil {
		t.Fatal(err)
	}
	if body["session_id"] != "s1" || body["in_reply_to"] != "m1" || body["correlation_id"] != "c1" || body["message"] != "olá" {
		t.Fatalf("body = %v", body)
	}

	client = &fakeSQS{}
	_ = NewSQSPublisher(client, "https://sqs/123/respostas").Publish(context.Background(), msg, "olá")
	if client.input.MessageGroupId != nil || client.input.MessageDeduplicationId != nil {
		t.Fatal("fila padrão não aceita MessageGroupId/MessageDeduplicationId")
	}
}

func TestSQSPoisonSink_SetsReasonAttribute(t *testing.T) {
	client := &fakeSQS{}
	if err := NewSQSPoisonSink(client, "https://sqs/123/dlq").Send(context.Background(), "m1", "{", "invalid_json"); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(client.input.MessageAttributes["failure_reason"].StringValue) != "invalid_json" ||
		aws.ToString(client.input.MessageAttributes["original_message_id"].StringValue) != "m1" ||
		aws.ToString(client.input.MessageBody) != "{" {
		t.Fatalf("input = %+v", client.input)
	}
}

func TestBuildAWSProcessor_MissingEnv(t *testing.T) {
	_, err := BuildAWSProcessor(context.Background(), DefaultSettings())
	if err == nil || !strings.Contains(err.Error(), "SESSIONS_TABLE") {
		t.Fatalf("esperado erro citando SESSIONS_TABLE, veio %v", err)
	}
}
