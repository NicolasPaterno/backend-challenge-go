#!/bin/sh
set -e

awslocal sqs create-queue \
    --queue-name wager-events.fifo \
    --attributes FifoQueue=true,ContentBasedDeduplication=false
