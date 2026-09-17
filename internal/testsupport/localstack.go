//go:build integration

package testsupport

import (
	"context"
	"fmt"
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

// SQSEnv creates a queue of its own for this test and points the process under
// test at it.
func SQSEnv(t *testing.T) (client *awssqs.Client, queueURL string) {
	t.Helper()

	endpoint := SQSEndpoint(t)
	client = SQSClient(t, endpoint)

	name := fmt.Sprintf("events-%d.fifo", time.Now().UnixNano())
	created, err := client.CreateQueue(context.Background(), &awssqs.CreateQueueInput{
		QueueName:  &name,
		Attributes: map[string]string{"FifoQueue": "true"},
	})
	if err != nil {
		t.Fatalf("create queue %s: %v", name, err)
	}

	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ENDPOINT_URL", endpoint)
	t.Setenv("OUTBOX_QUEUE_URL", *created.QueueUrl)

	return client, *created.QueueUrl
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
