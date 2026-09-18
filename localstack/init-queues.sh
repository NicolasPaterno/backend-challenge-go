#!/bin/sh
set -e

ACCOUNT=000000000000
REGION=us-east-1
BASE="http://localhost:4566/$ACCOUNT"

# The CLI's --attributes shorthand cannot carry a JSON document: it splits on
# "=" and chokes on the quotes. Both the redrive policy and the access policies
# are JSON, so they go through --cli-input-json, where the value is a JSON
# string and has to be escaped as one.
as_json_string() {
    sed 's/"/\\"/g' | tr -d '\n'
}

create_fifo() {
    name=$1
    extra=$2
    printf '{"QueueName":"%s","Attributes":{"FifoQueue":"true","ContentBasedDeduplication":"false"%s}}' \
        "$name" "$extra" > /tmp/queue.json
    awslocal sqs create-queue --cli-input-json file:///tmp/queue.json > /dev/null
}

set_policy() {
    queue=$1
    principal=$2
    actions=$3
    document=$(printf '{"Version":"2012-10-17","Statement":[{"Sid":"%s","Effect":"Allow","Principal":{"AWS":"arn:aws:iam::%s:role/%s"},"Action":%s,"Resource":"arn:aws:sqs:%s:%s:%s"}]}' \
        "$principal" "$ACCOUNT" "$principal" "$actions" "$REGION" "$ACCOUNT" "$queue" | as_json_string)
    printf '{"QueueUrl":"%s/%s","Attributes":{"Policy":"%s"}}' "$BASE" "$queue" "$document" > /tmp/policy.json
    awslocal sqs set-queue-attributes --cli-input-json file:///tmp/policy.json
}

create_fifo wager-events.fifo ''
create_fifo wager-transactions-dlq.fifo ''

DLQ_ARN=$(awslocal sqs get-queue-attributes \
    --queue-url "$BASE/wager-transactions-dlq.fifo" \
    --attribute-names QueueArn --query 'Attributes.QueueArn' --output text)

# maxReceiveCount 3 with a 30s visibility timeout. The consumer dead-letters a
# permanent failure itself; this is the backstop for a transient one that never
# stops failing.
REDRIVE=$(printf '{"deadLetterTargetArn":"%s","maxReceiveCount":"3"}' "$DLQ_ARN" | as_json_string)
create_fifo wager-transactions.fifo ",\"VisibilityTimeout\":\"30\",\"RedrivePolicy\":\"$REDRIVE\""

# access to the messaging layer is controlled by broker credentials and
# policies. Each principal gets the narrowest action it needs. LocalStack's
# community edition stores these documents but does not enforce them, which
# ARCHITECTURE.md records as a limitation.
set_policy wager-transactions.fifo     wagering-producer '["sqs:SendMessage"]'
set_policy wager-transactions-dlq.fifo wagering-api      '["sqs:SendMessage","sqs:ReceiveMessage"]'
set_policy wager-events.fifo           wagering-api      '["sqs:SendMessage"]'
