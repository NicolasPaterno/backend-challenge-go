//go:build integration

package testsupport

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/localstack"
)

// LocalStackImage matches the one Compose runs.
const LocalStackImage = "localstack/localstack:4"

// One container per test binary, like Keycloak: each test gets its own queue
// instead, which is what has to be isolated.
var stack = sync.OnceValues(startLocalStack)

func startLocalStack() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := localstack.Run(ctx, LocalStackImage,
		testcontainers.WithEnv(map[string]string{"SERVICES": "sqs"}))
	if err != nil {
		return "", err
	}
	endpoint, err := container.PortEndpoint(ctx, "4566/tcp", "http")
	if err != nil {
		return "", err
	}
	return endpoint, nil
}

// SQSEndpoint returns the address of the shared LocalStack container.
func SQSEndpoint(t *testing.T) string {
	t.Helper()

	endpoint, err := stack()
	if err != nil {
		t.Fatalf("start localstack container: %v", err)
	}
	return endpoint
}

func SQSClient(t *testing.T, endpoint string) *awssqs.Client {
	t.Helper()

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion("us-east-1"))
	if err != nil {
		t.Fatalf("load aws configuration: %v", err)
	}
	return awssqs.NewFromConfig(cfg, func(o *awssqs.Options) { o.BaseEndpoint = &endpoint })
}

// SQSEnv gives this test its own pair of queues — outbound, inbound and the
// inbound one's dead-letter queue — and points the process under test at them.
// The inbound url is read back with InboundQueue.
func SQSEnv(t *testing.T) (client *awssqs.Client, queueURL string) {
	t.Helper()

	endpoint := SQSEndpoint(t)
	client = SQSClient(t, endpoint)
	unique := time.Now().UnixNano()

	outbound := createFIFO(t, client, fmt.Sprintf("events-%d.fifo", unique), nil)
	deadLetter := createFIFO(t, client, fmt.Sprintf("wagers-dlq-%d.fifo", unique), nil)

	arns, err := client.GetQueueAttributes(context.Background(), &awssqs.GetQueueAttributesInput{
		QueueUrl: &deadLetter, AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
	})
	if err != nil {
		t.Fatalf("read the dead-letter queue arn: %v", err)
	}

	// The same redrive policy Compose provisions, so a test sees the attempts
	// run out the way production would.
	inbound := createFIFO(t, client, fmt.Sprintf("wagers-%d.fifo", unique), map[string]string{
		"VisibilityTimeout": "2",
		"RedrivePolicy": fmt.Sprintf(`{"deadLetterTargetArn":%q,"maxReceiveCount":"2"}`,
			arns.Attributes[string(types.QueueAttributeNameQueueArn)]),
	})

	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("SQS_ENDPOINT", endpoint)
	t.Setenv("SQS_EVENTS_QUEUE_URL", outbound)
	t.Setenv("SQS_WAGER_TRANSACTIONS_QUEUE_URL", inbound)
	t.Setenv("SQS_WAGER_TRANSACTIONS_DLQ_URL", deadLetter)

	return client, outbound
}

// InboundQueue and InboundDLQ report what SQSEnv provisioned for this test.
func InboundQueue(t *testing.T) string { return envOrFail(t, "SQS_WAGER_TRANSACTIONS_QUEUE_URL") }

func InboundDLQ(t *testing.T) string { return envOrFail(t, "SQS_WAGER_TRANSACTIONS_DLQ_URL") }

func envOrFail(t *testing.T, key string) string {
	t.Helper()

	value := os.Getenv(key)
	if value == "" {
		t.Fatalf("%s is unset; call SQSEnv first", key)
	}
	return value
}

func createFIFO(t *testing.T, client *awssqs.Client, name string, attributes map[string]string) string {
	t.Helper()

	if attributes == nil {
		attributes = map[string]string{}
	}
	attributes["FifoQueue"] = "true"

	created, err := client.CreateQueue(context.Background(), &awssqs.CreateQueueInput{
		QueueName: &name, Attributes: attributes,
	})
	if err != nil {
		t.Fatalf("create queue %s: %v", name, err)
	}
	return *created.QueueUrl
}

// Send puts one message on a FIFO queue, grouped by wallet as the brief specifies.
func Send(t *testing.T, client *awssqs.Client, queueURL, body, group, dedup string) {
	t.Helper()

	if _, err := client.SendMessage(context.Background(), &awssqs.SendMessageInput{
		QueueUrl:               &queueURL,
		MessageBody:            &body,
		MessageGroupId:         &group,
		MessageDeduplicationId: &dedup,
	}); err != nil {
		t.Fatalf("send to %s: %v", queueURL, err)
	}
}

// ReceiveAll drains the queue until it is empty for one long poll, so a test
// asserting on what was published does not race the publisher.
func ReceiveAll(t *testing.T, client *awssqs.Client, queueURL string, want int) []types.Message {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	var got []types.Message
	for len(got) < want && time.Now().Before(deadline) {
		out, err := client.ReceiveMessage(context.Background(), &awssqs.ReceiveMessageInput{
			QueueUrl:            &queueURL,
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     1,
		})
		if err != nil {
			t.Fatalf("receive from %s: %v", queueURL, err)
		}
		got = append(got, out.Messages...)
		for _, m := range out.Messages {
			if _, err := client.DeleteMessage(context.Background(), &awssqs.DeleteMessageInput{
				QueueUrl: &queueURL, ReceiptHandle: m.ReceiptHandle,
			}); err != nil {
				t.Fatalf("delete message: %v", err)
			}
		}
	}
	return got
}
