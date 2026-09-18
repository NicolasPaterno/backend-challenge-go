-- the inbox. The row is written in the same transaction as the domain change
-- it caused, so a message whose handling committed can never be handled twice,
-- whatever the queue redelivers.
--
-- received_at and completed_at bracket one handling: the consumer's receive and
-- the commit. They differ by however long the work took.
CREATE TABLE inbox_messages (
    consumer_name TEXT        NOT NULL,
    message_id    TEXT        NOT NULL,
    payload_hash  TEXT        NOT NULL,
    received_at   TIMESTAMPTZ NOT NULL,
    completed_at  TIMESTAMPTZ NOT NULL,

    PRIMARY KEY (consumer_name, message_id)
);
